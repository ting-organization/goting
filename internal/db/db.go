// Package db is the node's database (wiki: node/storage in the reference):
// the state at a base block, as flat records and the branch nodes of its
// tree by position, each flushed block's undo data, and metadata. It is an
// embedded bbolt file, a B+tree like the reference's redb, changed only in
// atomic, durable transactions.
package db

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
)

// Format is the database layout's version, checked when it is opened.
const Format = 1

var (
	bucketMeta     = []byte("meta")
	bucketRecords  = []byte("records")
	bucketBranches = []byte("branches")
	bucketUndo     = []byte("undo")
	bucketBlocks   = []byte("blocks")
	bucketOccur    = []byte("occurrences")
	bucketEvents   = []byte("events")
	bucketTopics   = []byte("event-topics")

	metaFormat    = []byte("format")
	metaChain     = []byte("chain")
	metaBase      = []byte("base")
	metaUndoFloor = []byte("undo-floor")
	metaCursor    = []byte("cursor")
)

// ErrWrongChain refuses a database made for another genesis.
var ErrWrongChain = errors.New("database belongs to another chain")

// ErrExpired refuses a read through a view that a rewind of the base has
// made obsolete.
var ErrExpired = errors.New("state view expired: the stored state was rewound")

// DB is an open database.
type DB struct {
	bolt *bolt.DB
	// epoch counts rewinds: a view taken before one no longer reads.
	epoch atomic.Uint64
}

// Open opens or creates the database file path for chainID. A database
// made for another chain or format is refused, never reset.
func Open(path string, chainID hashing.Hash) (*DB, error) {
	b, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: time.Second, NoFreelistSync: true,
		FreelistType: bolt.FreelistMapType})
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	d := &DB{bolt: b}
	err = b.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketMeta, bucketRecords, bucketBranches, bucketUndo, bucketBlocks,
			bucketOccur, bucketEvents, bucketTopics} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		f := meta.Get(metaFormat)
		if f == nil {
			if err := meta.Put(metaFormat, binary.BigEndian.AppendUint32(nil, Format)); err != nil {
				return err
			}
			return meta.Put(metaChain, chainID[:])
		}
		if len(f) != 4 || binary.BigEndian.Uint32(f) != Format {
			return fmt.Errorf("database format %x, this node reads format %d", f, Format)
		}
		if c := meta.Get(metaChain); len(c) != 32 || hashing.Hash(c) != chainID {
			return ErrWrongChain
		}
		return nil
	})
	if err != nil {
		b.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.bolt.Close() }

// Path returns the database file's path.
func (d *DB) Path() string { return d.bolt.Path() }

func undoKey(height uint64, id hashing.Hash) []byte {
	return append(binary.BigEndian.AppendUint64(nil, height), id[:]...)
}

// Base is the block whose state the database holds.
type Base struct {
	Height uint64
	Block  hashing.Hash
	Count  uint64
	Root   state.Desc
}

func (b Base) encode() []byte {
	out := binary.BigEndian.AppendUint64(nil, b.Height)
	out = append(out, b.Block[:]...)
	out = binary.BigEndian.AppendUint64(out, b.Count)
	return b.Root.Append(out)
}

func decodeBase(v []byte) (Base, error) {
	if len(v) != 8+32+8+state.DescSize {
		return Base{}, errors.New("bad base record")
	}
	b := Base{Height: binary.BigEndian.Uint64(v), Count: binary.BigEndian.Uint64(v[40:48])}
	copy(b.Block[:], v[8:40])
	root, err := state.DecodeDesc(v[48:])
	b.Root = root
	return b, err
}

// Base returns the stored state's block, or false for a database that holds
// none yet: the state of genesis, which is empty.
func (d *DB) Base() (Base, bool, error) {
	var base Base
	var ok bool
	err := d.bolt.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketMeta).Get(metaBase)
		if v == nil {
			return nil
		}
		var err error
		base, err = decodeBase(v)
		ok = err == nil
		return err
	})
	return base, ok, err
}

// BlockUndo is one block's undo data: the records it changed as they were
// before it, absent ones as deletions.
type BlockUndo struct {
	Height uint64
	Block  hashing.Hash
	Prev   []state.Change
}

func encodeUndo(prev []state.Change) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(prev)))
	for _, c := range prev {
		out = append(out, c.Key[:]...)
		if c.Delete {
			out = append(out, 0)
			continue
		}
		out = append(out, 1)
		out = binary.BigEndian.AppendUint32(out, uint32(len(c.Value)))
		out = append(out, c.Value...)
	}
	return out
}

func decodeUndo(b []byte) ([]state.Change, error) {
	bad := errors.New("bad undo record")
	if len(b) < 4 {
		return nil, bad
	}
	n := binary.BigEndian.Uint32(b)
	b = b[4:]
	out := make([]state.Change, 0, min(n, uint32(len(b)/33)))
	for i := uint32(0); i < n; i++ {
		if len(b) < 33 {
			return nil, bad
		}
		var c state.Change
		copy(c.Key[:], b[:32])
		present := b[32]
		b = b[33:]
		switch present {
		case 0:
			c.Delete = true
		case 1:
			if len(b) < 4 {
				return nil, bad
			}
			l := binary.BigEndian.Uint32(b)
			if uint64(len(b)-4) < uint64(l) {
				return nil, bad
			}
			c.Value = append([]byte(nil), b[4:4+l]...)
			b = b[4+l:]
		default:
			return nil, bad
		}
		out = append(out, c)
	}
	if len(b) != 0 {
		return nil, bad
	}
	return out, nil
}

// Undo returns a block's undo data, if the database holds it.
func (d *DB) Undo(height uint64, id hashing.Hash) ([]state.Change, bool, error) {
	var prev []state.Change
	var ok bool
	err := d.bolt.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketUndo).Get(undoKey(height, id))
		if v == nil {
			return nil
		}
		var err error
		prev, err = decodeUndo(v)
		ok = err == nil
		return err
	})
	return prev, ok, err
}

// UndoFloor is the lowest height whose undo data may still be held.
func (d *DB) UndoFloor() (uint64, error) {
	var floor uint64
	err := d.bolt.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketMeta).Get(metaUndoFloor); len(v) == 8 {
			floor = binary.BigEndian.Uint64(v)
		}
		return nil
	})
	return floor, err
}

// PutUndo stores undo data that a replay rebuilt below the undo floor; the
// floor is lowered once all of it is stored (LowerUndoFloor).
func (d *DB) PutUndo(undo []BlockUndo) error {
	if len(undo) == 0 {
		return nil
	}
	return d.bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUndo)
		for _, u := range undo {
			if err := b.Put(undoKey(u.Height, u.Block), encodeUndo(u.Prev)); err != nil {
				return err
			}
		}
		return nil
	})
}

// LowerUndoFloor records that undo data is held again from floor up, as after
// a replay stored what was pruned. A floor already as low stays.
func (d *DB) LowerUndoFloor(floor uint64) error {
	return d.bolt.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if v := meta.Get(metaUndoFloor); len(v) != 8 || binary.BigEndian.Uint64(v) <= floor {
			return nil
		}
		return meta.Put(metaUndoFloor, binary.BigEndian.AppendUint64(nil, floor))
	})
}

// BlockRow is a stored block as the database lists it, so that a restart
// rebuilds the block index without reading the block files: its header, its
// transaction count and size, and where its frame lies.
type BlockRow struct {
	Height  uint64
	ID      hashing.Hash
	Header  []byte
	TxCount uint32
	Size    uint32
	Segment uint32
	Offset  int64
	Length  int64
}

const headerSize = 294

func (r BlockRow) key() []byte {
	return append(binary.BigEndian.AppendUint64(nil, r.Height), r.ID[:]...)
}

func (r BlockRow) value() []byte {
	v := append(make([]byte, 0, headerSize+28), r.Header...)
	v = binary.BigEndian.AppendUint32(v, r.TxCount)
	v = binary.BigEndian.AppendUint32(v, r.Size)
	v = binary.BigEndian.AppendUint32(v, r.Segment)
	v = binary.BigEndian.AppendUint64(v, uint64(r.Offset))
	return binary.BigEndian.AppendUint64(v, uint64(r.Length))
}

func decodeRow(k, v []byte) (BlockRow, error) {
	if len(k) != 40 || len(v) != headerSize+28 {
		return BlockRow{}, errors.New("bad block row")
	}
	r := BlockRow{Height: binary.BigEndian.Uint64(k), Header: append([]byte(nil), v[:headerSize]...)}
	copy(r.ID[:], k[8:])
	v = v[headerSize:]
	r.TxCount, r.Size, r.Segment = binary.BigEndian.Uint32(v), binary.BigEndian.Uint32(v[4:]), binary.BigEndian.Uint32(v[8:])
	r.Offset, r.Length = int64(binary.BigEndian.Uint64(v[12:])), int64(binary.BigEndian.Uint64(v[20:]))
	return r, nil
}

// Blocks calls fn for every listed block, lowest height first.
func (d *DB) Blocks(fn func(BlockRow) error) error {
	return d.bolt.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketBlocks).ForEach(func(k, v []byte) error {
			r, err := decodeRow(k, v)
			if err != nil {
				return err
			}
			return fn(r)
		})
	})
}

// Cursor is the position in the block files up to which the blocks are
// listed.
func (d *DB) Cursor() (segment uint32, offset int64, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketMeta).Get(metaCursor); len(v) == 12 {
			segment, offset = binary.BigEndian.Uint32(v), int64(binary.BigEndian.Uint64(v[4:]))
		}
		return nil
	})
	return
}

// Occurrence is a transaction included in a block, as the occurrence index
// lists it: by TransactionId, then height, block and place.
type Occurrence struct {
	Tx     hashing.Hash
	Height uint64
	Block  hashing.Hash
	Index  uint32
	Status uint8
}

func (o Occurrence) key() []byte {
	k := append(append([]byte{}, o.Tx[:]...), binary.BigEndian.AppendUint64(nil, o.Height)...)
	k = append(k, o.Block[:]...)
	return binary.BigEndian.AppendUint32(k, o.Index)
}

// EventRow is an event, as the event index lists it: by the program that
// emitted it, then height, block, transaction and place; with its topic, the
// first 32 bytes of its payload, when it has one.
type EventRow struct {
	Contract hashing.Hash
	Height   uint64
	Block    hashing.Hash
	Tx       uint32
	Event    uint32
	Topic    *hashing.Hash
}

func (e EventRow) tail() []byte {
	k := binary.BigEndian.AppendUint64(nil, e.Height)
	k = append(k, e.Block[:]...)
	k = binary.BigEndian.AppendUint32(k, e.Tx)
	return binary.BigEndian.AppendUint32(k, e.Event)
}

func decodeTail(t []byte, e *EventRow) {
	e.Height = binary.BigEndian.Uint64(t)
	copy(e.Block[:], t[8:40])
	e.Tx, e.Event = binary.BigEndian.Uint32(t[40:]), binary.BigEndian.Uint32(t[44:])
}

// Occurrences lists a transaction's indexed occurrences, lowest height
// first, whatever branch their blocks are on.
func (d *DB) Occurrences(tx hashing.Hash) ([]Occurrence, error) {
	var out []Occurrence
	err := d.bolt.View(func(btx *bolt.Tx) error {
		c := btx.Bucket(bucketOccur).Cursor()
		for k, v := c.Seek(tx[:]); k != nil && string(k[:32]) == string(tx[:]); k, v = c.Next() {
			if len(k) != 76 || len(v) != 1 {
				return errors.New("bad occurrence row")
			}
			o := Occurrence{Tx: tx, Height: binary.BigEndian.Uint64(k[32:]), Index: binary.BigEndian.Uint32(k[72:]), Status: v[0]}
			copy(o.Block[:], k[40:72])
			out = append(out, o)
		}
		return nil
	})
	return out, err
}

// Events calls fn for the indexed events of a program, or of one of its
// topics, from height from up, lowest first, until fn returns false.
func (d *DB) Events(contract hashing.Hash, topic *hashing.Hash, from uint64, fn func(EventRow) bool) error {
	return d.bolt.View(func(tx *bolt.Tx) error {
		prefix := append([]byte{}, contract[:]...)
		bucket := bucketEvents
		if topic != nil {
			prefix, bucket = append(prefix, topic[:]...), bucketTopics
		}
		c := tx.Bucket(bucket).Cursor()
		start := binary.BigEndian.AppendUint64(append([]byte{}, prefix...), from)
		for k, _ := c.Seek(start); k != nil && len(k) == len(prefix)+48 && string(k[:len(prefix)]) == string(prefix); k, _ = c.Next() {
			e := EventRow{Contract: contract, Topic: topic}
			decodeTail(k[len(prefix):], &e)
			if !fn(e) {
				return nil
			}
		}
		return nil
	})
}

// Commit is one atomic change: the new base and its state's net changes, and
// undo data added and dropped.
type Commit struct {
	Base    Base
	Records []state.Change
	Puts    []state.BranchWrite
	Dels    []state.Position
	Undo    []BlockUndo
	// DropUndo removes the undo data of these blocks; UndoFloor, when above
	// the current floor, removes all undo data below it.
	DropUndo  []BlockUndo
	UndoFloor uint64
	// Rewind marks a commit whose base does not descend from the previous
	// one: views taken before it expire.
	Rewind bool
	// Blocks lists stored blocks; with them the cursor moves to Segment and
	// Offset, the end of the frames they come from.
	Blocks        []BlockRow
	SetCursor     bool
	CursorSegment uint32
	CursorOffset  int64
	// Index rows of the blocks committed.
	Occurrences []Occurrence
	Events      []EventRow
}

// Commit applies c atomically and durably.
func (d *DB) Commit(c *Commit) error {
	if c.Rewind {
		// Before the commit, so that no read starting after it can pass.
		d.epoch.Add(1)
	}
	return d.bolt.Update(func(tx *bolt.Tx) error {
		branches, records, undo, meta := tx.Bucket(bucketBranches), tx.Bucket(bucketRecords),
			tx.Bucket(bucketUndo), tx.Bucket(bucketMeta)
		for _, p := range c.Dels {
			if err := branches.Delete(p.Bytes()); err != nil {
				return err
			}
		}
		for _, w := range c.Puts {
			if err := branches.Put(w.Pos.Bytes(), w.Record); err != nil {
				return err
			}
		}
		for _, r := range c.Records {
			var err error
			if r.Delete {
				err = records.Delete(r.Key[:])
			} else {
				err = records.Put(append([]byte(nil), r.Key[:]...), r.Value)
			}
			if err != nil {
				return err
			}
		}
		for _, u := range c.DropUndo {
			if err := undo.Delete(undoKey(u.Height, u.Block)); err != nil {
				return err
			}
		}
		for _, u := range c.Undo {
			if err := undo.Put(undoKey(u.Height, u.Block), encodeUndo(u.Prev)); err != nil {
				return err
			}
		}
		if c.UndoFloor > 0 {
			var floor uint64
			if v := meta.Get(metaUndoFloor); len(v) == 8 {
				floor = binary.BigEndian.Uint64(v)
			}
			if c.UndoFloor > floor {
				cur := undo.Cursor()
				for k, _ := cur.First(); k != nil && binary.BigEndian.Uint64(k) < c.UndoFloor; k, _ = cur.First() {
					if err := cur.Delete(); err != nil {
						return err
					}
				}
				if err := meta.Put(metaUndoFloor, binary.BigEndian.AppendUint64(nil, c.UndoFloor)); err != nil {
					return err
				}
			}
		}
		blocks := tx.Bucket(bucketBlocks)
		for _, r := range c.Blocks {
			if err := blocks.Put(r.key(), r.value()); err != nil {
				return err
			}
		}
		occ, events, topics := tx.Bucket(bucketOccur), tx.Bucket(bucketEvents), tx.Bucket(bucketTopics)
		for _, o := range c.Occurrences {
			if err := occ.Put(o.key(), []byte{o.Status}); err != nil {
				return err
			}
		}
		for _, e := range c.Events {
			if err := events.Put(append(append([]byte{}, e.Contract[:]...), e.tail()...), []byte{}); err != nil {
				return err
			}
			if e.Topic != nil {
				k := append(append(append([]byte{}, e.Contract[:]...), e.Topic[:]...), e.tail()...)
				if err := topics.Put(k, []byte{}); err != nil {
					return err
				}
			}
		}
		if c.SetCursor {
			v := binary.BigEndian.AppendUint32(nil, c.CursorSegment)
			if err := meta.Put(metaCursor, binary.BigEndian.AppendUint64(v, uint64(c.CursorOffset))); err != nil {
				return err
			}
		}
		return meta.Put(metaBase, c.Base.encode())
	})
}

// Source is a view of the stored state, a state.Source. It reads the
// database as it is, which serves every tree whose block descends from the
// base: moving the base forward along such a tree's branch never changes
// what its stubs stand for. A rewind expires it.
type Source struct {
	d     *DB
	epoch uint64
}

// View returns a view of the stored state.
func (d *DB) View() *Source { return &Source{d: d, epoch: d.epoch.Load()} }

func (s *Source) get(bucket, key []byte) ([]byte, bool) {
	var out []byte
	var ok bool
	err := s.d.bolt.View(func(tx *bolt.Tx) error {
		if s.d.epoch.Load() != s.epoch {
			return ErrExpired
		}
		if v := tx.Bucket(bucket).Get(key); v != nil {
			out, ok = append([]byte{}, v...), true
		}
		return nil
	})
	if err != nil {
		panic(&state.StorageError{Err: err})
	}
	return out, ok
}

// Branch implements state.Source.
func (s *Source) Branch(p state.Position) (state.Desc, state.Desc) {
	v, ok := s.get(bucketBranches, p.Bytes())
	if !ok {
		panic(&state.StorageError{Err: fmt.Errorf("stored branch at depth %d missing", p.Split)})
	}
	l, r, err := state.DecodeBranch(v)
	if err != nil {
		panic(&state.StorageError{Err: err})
	}
	return l, r
}

// Record implements state.Source.
func (s *Source) Record(k state.Key) ([]byte, bool) {
	return s.get(bucketRecords, k[:])
}
