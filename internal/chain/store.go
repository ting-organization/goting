package chain

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// ErrNotFound reports a block the store does not hold.
var ErrNotFound = errors.New("block not found")

// ErrClosed reports a use of block files after they were closed.
var ErrClosed = errors.New("block files are closed")

// BlockStore keeps raw blocks by BlockId. Storage layout is a node's own
// business (rules §27.4).
type BlockStore interface {
	Put(id hashing.Hash, raw []byte) error
	Get(id hashing.Hash) ([]byte, error)
	Has(id hashing.Hash) bool
	// Each calls fn for every stored block in insertion order.
	Each(fn func(id hashing.Hash, raw []byte) error) error
	// Len returns how many blocks are stored.
	Len() int
	// Forget drops a block, as when its stored copy no longer reads: it is
	// stored again by the next Put.
	Forget(id hashing.Hash)
	Close() error
}

// MemStore is an in-memory BlockStore.
type MemStore struct {
	mu    sync.RWMutex
	m     map[hashing.Hash][]byte
	order []hashing.Hash
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{m: make(map[hashing.Hash][]byte)} }

func (s *MemStore) Put(id hashing.Hash, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		s.m[id] = raw
		s.order = append(s.order, id)
	}
	return nil
}

func (s *MemStore) Get(id hashing.Hash) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

func (s *MemStore) Has(id hashing.Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.m[id]
	return ok
}

func (s *MemStore) Each(fn func(id hashing.Hash, raw []byte) error) error {
	s.mu.RLock()
	order := append([]hashing.Hash(nil), s.order...)
	s.mu.RUnlock()
	for _, id := range order {
		b, _ := s.Get(id)
		if err := fn(id, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

func (s *MemStore) Forget(id hashing.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return
	}
	delete(s.m, id)
	for i, o := range s.order {
		if o == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

func (s *MemStore) Close() error { return nil }

// Block files (reference node, wiki: node/storage): append-only segments
// blocks/00000000.dat, 00000001.dat, ..., each beginning with the 8 bytes
// "TINGBLK\0" and the ChainId, then one frame per block:
//
//	u64 little-endian length ‖ canonical block bytes ‖ H("node-block", block)
//
// A frame never spans two files: a new file begins when the next frame
// would not fit in SegmentSize, and a larger frame sits alone in a file of
// its own.
const (
	SegmentSize  = 128 << 20
	segmentMagic = "TINGBLK\x00"
	segmentHead  = 8 + 32
	frameExtra   = 8 + 32
	minBlock     = types.HeaderSize + 8 // an empty block with no producer data
)

// FileStore keeps blocks in segment files. An in-memory index of frame
// locations is rebuilt by scanning on open. Frames are forced to disk by
// Sync, which the chain calls before every database commit that moves its
// base past them, and otherwise at most syncInterval after they are written:
// a frame lost in a crash before then is only fetched again.
type FileStore struct {
	mu       sync.Mutex
	dir      string
	chainID  hashing.Hash
	files    []*os.File // by segment number
	size     int64      // of the last segment
	index    map[hashing.Hash]location
	order    []hashing.Hash
	unsynced map[*os.File]struct{}
	synced   time.Time
	// Damaged counts frames passed over on open: torn or zero-filled tails
	// of the last segment are truncated, damage earlier is skipped.
	Damaged int
}

type location struct {
	seg    int
	offset int64 // of the block bytes
	length int64
}

// Location is where a block lies in the block files: its segment, the
// offset of its bytes and their length.
type Location struct {
	Segment uint32
	Offset  int64
	Length  int64
}

// Cursor is a position in the block files: the end of the frames before
// it, which a database records with their locations.
type Cursor struct {
	Segment uint32
	Offset  int64
}

// covers reports whether the frame at l lies before the cursor.
func (c Cursor) covers(l location) bool {
	return uint32(l.seg) < c.Segment || (uint32(l.seg) == c.Segment && l.offset < c.Offset)
}

// Frame is a stored block and its location.
type Frame struct {
	ID       hashing.Hash
	Location Location
}

// ErrWrongChain reports block files made for another genesis.
var ErrWrongChain = errors.New("block files belong to another chain")

// OpenFileStore opens or creates the block files in dir for chainID. Files
// made for another chain are refused, never reset.
func OpenFileStore(dir string, chainID hashing.Hash) (*FileStore, error) {
	return OpenFileStoreFrom(dir, chainID, Cursor{}, nil)
}

// OpenFileStoreFrom opens the block files knowing the frames before from:
// known gives their locations, as a database recorded them, and only the
// frames after from are scanned. Frames before it are checked when read.
func OpenFileStoreFrom(dir string, chainID hashing.Hash, from Cursor, known []Frame) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &FileStore{dir: dir, chainID: chainID, index: make(map[hashing.Hash]location, len(known)),
		unsynced: map[*os.File]struct{}{}, synced: time.Now()}
	for _, f := range known {
		s.index[f.ID] = location{seg: int(f.Location.Segment), offset: f.Location.Offset, length: f.Location.Length}
	}
	for n := 0; ; n++ {
		f, err := os.OpenFile(s.segPath(n), os.O_RDWR, 0o644)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			s.Close()
			return nil, err
		}
		s.files = append(s.files, f)
		start := int64(segmentHead)
		switch {
		case uint32(n) < from.Segment:
			start = -1 // wholly before the cursor
		case uint32(n) == from.Segment && from.Offset > segmentHead:
			start = from.Offset
		}
		size, err := s.scan(n, f, start, from)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.size = size
	}
	if len(s.files) < int(from.Segment)+1 && (from.Segment > 0 || from.Offset > segmentHead) {
		s.Close()
		return nil, fmt.Errorf("block files end before the position the database records")
	}
	if len(s.files) == 0 {
		if err := s.newSegment(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *FileStore) segPath(n int) string {
	return filepath.Join(s.dir, fmt.Sprintf("%08d.dat", n))
}

// scan indexes one segment's frames and returns where its valid frames end.
// A damaged frame ends the scan; in the last segment it is truncated with
// everything after it.
func (s *FileStore) scan(n int, f *os.File, start int64, from Cursor) (int64, error) {
	head := make([]byte, segmentHead)
	if _, err := io.ReadFull(f, head); err != nil || string(head[:8]) != segmentMagic {
		return 0, fmt.Errorf("%s: not a block file", s.segPath(n))
	}
	if hashing.Hash(head[8:]) != s.chainID {
		return 0, fmt.Errorf("%s: %w", s.segPath(n), ErrWrongChain)
	}
	if start < 0 {
		st, err := f.Stat()
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if start > st.Size() {
		return 0, fmt.Errorf("%s ends before the position the database records", s.segPath(n))
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, start, st.Size()-start), 1<<20)
	off := start
	var lenBuf [8]byte
	var sum hashing.Hash
	for {
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			if err != io.EOF {
				s.Damaged++
			}
			break
		}
		n64 := binary.LittleEndian.Uint64(lenBuf[:])
		if n64 < minBlock || n64 > 1<<40 {
			s.Damaged++
			break
		}
		raw := make([]byte, n64)
		if _, err := io.ReadFull(r, raw); err != nil {
			s.Damaged++
			break
		}
		if _, err := io.ReadFull(r, sum[:]); err != nil || sum != hashing.H("node-block", raw) {
			s.Damaged++
			break
		}
		id := types.HeaderIDFromBytes(raw[:types.HeaderSize])
		// A block is stored twice only when its first frame was forgotten
		// for no longer reading: the later frame stands for it.
		old, dup := s.index[id]
		s.index[id] = location{seg: n, offset: off + 8, length: int64(n64)}
		if !dup || from.covers(old) {
			s.order = append(s.order, id)
		}
		off += 8 + int64(n64) + 32
	}
	// Only the last segment is appended to; damage there is a torn write.
	if _, err := os.Stat(s.segPath(n + 1)); errors.Is(err, os.ErrNotExist) {
		if err := f.Truncate(off); err != nil {
			return 0, err
		}
		if err := f.Sync(); err != nil {
			return 0, err
		}
	}
	return off, nil
}

func (s *FileStore) newSegment() error {
	n := len(s.files)
	f, err := os.OpenFile(s.segPath(n), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	head := append([]byte(segmentMagic), s.chainID[:]...)
	if _, err := f.Write(head); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	syncDir(s.dir)
	s.files = append(s.files, f)
	s.size = segmentHead
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

func (s *FileStore) Put(id hashing.Hash, raw []byte) error {
	if len(raw) < minBlock {
		return fmt.Errorf("block of %d bytes too short to store", len(raw))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		return ErrClosed
	}
	if _, ok := s.index[id]; ok {
		return nil
	}
	frame := int64(len(raw)) + frameExtra
	if s.size > segmentHead && s.size+frame > SegmentSize {
		if err := s.syncLocked(); err != nil {
			return err
		}
		if err := s.newSegment(); err != nil {
			return err
		}
	}
	buf := make([]byte, 0, frame)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(len(raw)))
	buf = append(buf, raw...)
	sum := hashing.H("node-block", raw)
	buf = append(buf, sum[:]...)
	n := len(s.files) - 1
	f := s.files[n]
	if _, err := f.WriteAt(buf, s.size); err != nil {
		f.Truncate(s.size)
		return err
	}
	s.unsynced[f] = struct{}{}
	s.index[id] = location{seg: n, offset: s.size + 8, length: int64(len(raw))}
	s.order = append(s.order, id)
	s.size += frame
	if time.Since(s.synced) >= syncInterval {
		return s.syncLocked()
	}
	return nil
}

// syncInterval bounds how long a written frame may wait for Sync.
const syncInterval = 2 * time.Second

// Sync forces every frame written so far to disk.
func (s *FileStore) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncLocked()
}

func (s *FileStore) syncLocked() error {
	for f := range s.unsynced {
		if err := f.Sync(); err != nil {
			return err
		}
		delete(s.unsynced, f)
	}
	s.synced = time.Now()
	return nil
}

func (s *FileStore) Get(id hashing.Hash) ([]byte, error) {
	s.mu.Lock()
	loc, ok := s.index[id]
	var f *os.File
	if ok && loc.seg < len(s.files) {
		f = s.files[loc.seg]
	}
	closed := s.files == nil
	s.mu.Unlock()
	switch {
	case closed:
		return nil, ErrClosed
	case !ok:
		return nil, ErrNotFound
	case f == nil:
		return nil, fmt.Errorf("stored block %s: segment %d is missing", id, loc.seg)
	}
	b := make([]byte, loc.length+32)
	if _, err := f.ReadAt(b, loc.offset); err != nil {
		return nil, err
	}
	raw := b[:loc.length]
	if hashing.Hash(b[loc.length:]) != hashing.H("node-block", raw) {
		return nil, fmt.Errorf("stored block %s: checksum mismatch", id)
	}
	return raw, nil
}

// Forget drops a block whose frame no longer reads: it is no longer served,
// and storing it again writes a new frame. The old frame stays in its file,
// unreferenced.
func (s *FileStore) Forget(id hashing.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[id]; !ok {
		return
	}
	delete(s.index, id)
	for i, o := range s.order {
		if o == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

func (s *FileStore) Has(id hashing.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.index[id]
	return ok
}

// Each calls fn for every stored block in the order stored. A block whose
// frame no longer reads back is passed over.
func (s *FileStore) Each(fn func(id hashing.Hash, raw []byte) error) error {
	s.mu.Lock()
	order := append([]hashing.Hash(nil), s.order...)
	s.mu.Unlock()
	for _, id := range order {
		b, err := s.Get(id)
		if err != nil {
			s.mu.Lock()
			s.Damaged++
			s.mu.Unlock()
			continue
		}
		if err := fn(id, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.index)
}

// Close forces the block files to disk and closes them.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	first := s.syncLocked()
	for _, f := range s.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.files = nil
	return first
}

// SeedFromStore derives the seed list ending at pinID (at height n) from the
// parent links of stored blocks. It returns nil when the store does not hold
// the pinned block and all its ancestors.
func SeedFromStore(store BlockStore, genesisID, pinID hashing.Hash, n uint64) []hashing.Hash {
	parents := make(map[hashing.Hash]hashing.Hash)
	store.Each(func(id hashing.Hash, raw []byte) error {
		if len(raw) >= types.HeaderSize {
			var p hashing.Hash
			copy(p[:], raw[42:74])
			parents[id] = p
		}
		return nil
	})
	list := make([]hashing.Hash, n)
	id := pinID
	for h := n; h >= 1; h-- {
		p, ok := parents[id]
		if !ok {
			return nil
		}
		list[h-1] = id
		id = p
	}
	if id != genesisID {
		return nil
	}
	return list
}

// End is the cursor at the end of the frames written so far.
func (s *FileStore) End() Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Cursor{Segment: uint32(len(s.files) - 1), Offset: s.size}
}

// FramesAfter lists the blocks stored at or after c, in the order stored.
func (s *FileStore) FramesAfter(c Cursor) []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Frame
	for _, id := range s.order {
		l := s.index[id]
		if !c.covers(l) {
			out = append(out, Frame{ID: id, Location: Location{Segment: uint32(l.seg), Offset: l.offset, Length: l.length}})
		}
	}
	return out
}

// Trim forgets which frames before c were stored this run: a database now
// lists them.
func (s *FileStore) Trim(c Cursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.order[:0]
	for _, id := range s.order {
		if !c.covers(s.index[id]) {
			kept = append(kept, id)
		}
	}
	s.order = kept
}
