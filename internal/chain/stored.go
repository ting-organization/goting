package chain

import (
	"errors"
	"fmt"
	"sort"

	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/exec"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
)

// With a database (Config.State), the chain keeps the state of one canonical
// block, the base, on disk, and in memory only the states of the blocks
// above it, each as a tree resting on the stored one. When the tip is twice
// RetainDepth above the base, the base moves up to RetainDepth below the
// tip: the blocks in between are written in one atomic commit with their
// undo data, and the trees left in memory are rebuilt on the new base from
// each block's change list, every root checked against its header. A
// reorganization that forks below the base first rewinds it with the undo
// data, then executes the new branch as any other.

// ErrBelowBase reports a state the database no longer holds: that of a
// block below its base, or of a branch that forks below it.
var ErrBelowBase = errors.New("state below the database's base")

// DefaultUndoWindow is how many blocks below the base keep undo data: the
// deepest reorganization a node with a database follows (the reference's
// undo_window).
const DefaultUndoWindow = 43_200

func stateRootOf(e *Entry) hashing.Hash {
	if e.Header == nil {
		return hashing.EmptyStateRoot
	}
	return e.Header.StateRoot
}

// storageFail records a failure of the node's own storage. The chain processes
// nothing more: the node must stop and be restarted.
func (c *Chain) storageFail(err error) error {
	if c.broken == nil {
		c.broken = fmt.Errorf("storage failure; restart required: %w", err)
		if c.cfg.Fatal != nil {
			c.cfg.Fatal(c.broken)
		}
	}
	return c.broken
}

// OpenStoredBlocks opens the block files in dir, knowing from the database
// every frame it lists and the position up to which it lists them: only the
// frames after that are scanned.
func OpenStoredBlocks(dir string, chainID hashing.Hash, d *db.DB) (*FileStore, error) {
	seg, off, err := d.Cursor()
	if err != nil {
		return nil, err
	}
	var known []Frame
	err = d.Blocks(func(r db.BlockRow) error {
		known = append(known, Frame{ID: r.ID, Location: Location{Segment: r.Segment, Offset: r.Offset, Length: r.Length}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return OpenFileStoreFrom(dir, chainID, Cursor{Segment: seg, Offset: off}, known)
}

// restoreRows indexes the blocks the database lists, without reading them:
// their headers are checked again, their bodies were checked when stored.
// Called with processMu held.
func (c *Chain) restoreRows() error {
	seg, off, err := c.cfg.State.Cursor()
	if err != nil {
		return err
	}
	c.cursor = Cursor{Segment: seg, Offset: off}
	return c.cfg.State.Blocks(func(r db.BlockRow) error {
		h, err := types.DecodeHeader(r.Header)
		if err != nil || types.HeaderIDFromBytes(r.Header) != r.ID {
			return fmt.Errorf("the database lists block %s with a bad header", r.ID)
		}
		c.mu.RLock()
		_, have := c.entries[r.ID]
		c.mu.RUnlock()
		if have {
			return nil
		}
		e, err := c.acceptHeader(h, r.ID, true, true)
		switch {
		case err == nil:
		case errors.Is(err, ErrUnknownParent), errors.Is(err, ErrInvalid), errors.Is(err, ErrMarked):
			return nil // not part of any branch; fetched again if needed
		default:
			return err
		}
		c.mu.Lock()
		if e.Status == StatusHeader {
			e.Status = StatusStored
		}
		e.TxCount, e.Size = int(r.TxCount), int(r.Size)
		c.markComplete(e)
		c.advanceFrontier()
		c.mu.Unlock()
		return nil
	})
}

// listFrames adds to a commit the stored blocks whose frames lie after the
// listed ones, and moves the cursor past them. It returns the new cursor.
func (c *Chain) listFrames(commit *db.Commit) (Cursor, bool) {
	fs, ok := c.store.(*FileStore)
	if !ok {
		return Cursor{}, false
	}
	end := fs.End()
	c.mu.RLock()
	for _, f := range fs.FramesAfter(c.cursor) {
		e, ok := c.entries[f.ID]
		if !ok || !e.HasData() || e.Header == nil {
			continue
		}
		commit.Blocks = append(commit.Blocks, db.BlockRow{Height: e.Height, ID: e.ID, Header: e.Header.Encode(),
			TxCount: uint32(e.TxCount), Size: uint32(e.Size), Segment: f.Location.Segment,
			Offset: f.Location.Offset, Length: f.Location.Length})
	}
	c.mu.RUnlock()
	commit.SetCursor, commit.CursorSegment, commit.CursorOffset = true, end.Segment, end.Offset
	return end, true
}

// loadBase attaches the database's base after the stored blocks are
// indexed: the base block and its ancestors were validated before the base
// moved past them, and the stored state stands for their execution. Called
// with processMu held.
func (c *Chain) loadBase() error {
	b, ok, err := c.cfg.State.Base()
	if err != nil {
		return err
	}
	if !ok {
		c.base = c.gen
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[b.Block]
	if !ok || e.Height != b.Height || !e.HasData() {
		return fmt.Errorf("the database's base block %s at height %d is not stored", b.Block, b.Height)
	}
	t := state.FromRoot(b.Root, int(b.Count), c.cfg.State.View())
	if t.Root() != stateRootOf(e) {
		return fmt.Errorf("the database's state root %s differs from block %s's", t.Root(), e.ID)
	}
	for n := e; n != c.gen; n = n.Parent {
		n.Status = StatusValid
	}
	e.state = &t
	c.retained[e] = struct{}{}
	if e != c.gen {
		c.gen.state = nil
		delete(c.retained, c.gen)
	}
	c.base = e
	return nil
}

// Base returns the block whose state the database holds, or nil without a
// database.
func (c *Chain) Base() *Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.base
}

// maybeFlush moves the base up when the tip is far enough above it. Called
// with processMu held.
func (c *Chain) maybeFlush() error {
	if c.cfg.State == nil || c.broken != nil {
		return c.broken
	}
	w := max(c.cfg.RetainDepth, 1)
	c.mu.RLock()
	tip, base := c.tip, c.base
	var target *Entry
	if tip.Height >= base.Height+2*w {
		target = c.canonical[tip.Height-w]
	}
	c.mu.RUnlock()
	if target == nil {
		return nil
	}
	if err := c.flush(target); err != nil {
		if errors.Is(err, errBodyLost) {
			return nil // the base moves once the block is stored again
		}
		return c.storageFail(err)
	}
	return nil
}

// Settle moves the database's base up to the tip, as a node does when it
// stops cleanly or finishes importing its seed: a restart then executes
// nothing again.
func (c *Chain) Settle() error {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	if c.cfg.State == nil || c.broken != nil {
		return c.broken
	}
	c.mu.RLock()
	tip, base := c.tip, c.base
	c.mu.RUnlock()
	if tip == base || !descends(tip, base) {
		return nil
	}
	if err := c.flush(tip); err != nil {
		if errors.Is(err, errBodyLost) {
			return nil // a restart executes the blocks above the base again
		}
		return c.storageFail(err)
	}
	return nil
}

// flush writes the canonical blocks above the base up to target and makes
// target the base. Called with processMu held.
func (c *Chain) flush(target *Entry) (err error) {
	defer state.Recover(&err)
	c.mu.RLock()
	base := c.base
	baseTree := *base.state
	var blocks []*Entry
	for n := target; n != base; n = n.Parent {
		if n.Height <= base.Height {
			c.mu.RUnlock()
			return fmt.Errorf("block %s to flush does not descend from the base", target.ID)
		}
		blocks = append(blocks, n)
	}
	c.mu.RUnlock()
	t := baseTree
	keys := map[state.Key]struct{}{}
	undo := make([]db.BlockUndo, 0, len(blocks))
	var occurrences []db.Occurrence
	var eventRows []db.EventRow
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		prev := t
		c.mu.RLock()
		st, changes := b.state, b.changes
		c.mu.RUnlock()
		if st != nil && changes != nil {
			t = *st
		} else {
			res, err := c.reexecute(t, b)
			if err != nil {
				return err
			}
			t, changes = res.State, res.Changes
		}
		for _, ch := range changes {
			keys[ch.Key] = struct{}{}
		}
		undo = append(undo, undoFor(prev, b, changes))
		occ, events, err := c.indexRows(b)
		if err != nil {
			return err
		}
		occurrences = append(occurrences, occ...)
		eventRows = append(eventRows, events...)
	}
	if t.Root() != target.Header.StateRoot {
		return fmt.Errorf("state to flush at height %d has root %s, its header %s", target.Height, t.Root(), target.Header.StateRoot)
	}
	commit := c.commitFor(baseTree, t, keys, target)
	commit.Undo = undo
	commit.Occurrences, commit.Events = occurrences, eventRows
	// Nothing is pruned while a replay restores what was.
	if c.replay == nil && target.Height > c.cfg.UndoWindow {
		commit.UndoFloor = target.Height - c.cfg.UndoWindow
	}
	end, listed := c.listFrames(commit)
	if err := c.syncBlocks(); err != nil {
		return err
	}
	if err := c.cfg.State.Commit(commit); err != nil {
		return err
	}
	if listed {
		c.cursor = end
		c.store.(*FileStore).Trim(end)
	}
	return c.rebase(target, commit.Base)
}

// undoFor is a block's undo data: the value before it, in prev, of every
// record it changed.
func undoFor(prev state.Tree, b *Entry, changes []state.Change) db.BlockUndo {
	u := db.BlockUndo{Height: b.Height, Block: b.ID, Prev: make([]state.Change, len(changes))}
	for j, ch := range changes {
		v, ok := prev.Get(ch.Key)
		u.Prev[j] = state.Change{Key: ch.Key, Value: v, Delete: !ok}
	}
	return u
}

// commitFor is the commit that stores t, derived from the stored tree old,
// as the state of block at.
func (c *Chain) commitFor(old, t state.Tree, keys map[state.Key]struct{}, at *Entry) *db.Commit {
	list := make([]state.Key, 0, len(keys))
	for k := range keys {
		list = append(list, k)
	}
	sort.Slice(list, func(i, j int) bool { return string(list[i][:]) < string(list[j][:]) })
	puts, dels := state.Delta(old, t, list)
	records := make([]state.Change, len(list))
	for i, k := range list {
		v, ok := t.Get(k)
		records[i] = state.Change{Key: k, Value: v, Delete: !ok}
	}
	return &db.Commit{Base: db.Base{Height: at.Height, Block: at.ID, Count: uint64(t.Len()), Root: t.RootDesc()},
		Records: records, Puts: puts, Dels: dels}
}

// reexecute runs a stored block again on its parent's state. Called with
// processMu held.
func (c *Chain) reexecute(parent state.Tree, b *Entry) (*exec.Result, error) {
	raw, body, err := c.readBody(b)
	if err != nil {
		c.forgetBody(b, err)
		return nil, fmt.Errorf("block at height %d: %w", b.Height, errBodyLost)
	}
	res, err := c.executeBody(parent, b.Header, b.ID, raw, body, b.Vote.Limits)
	if err != nil {
		return nil, fmt.Errorf("re-executing stored block %s: %w", b.ID, err)
	}
	return res, nil
}

// rebase makes newBase the base: its state the stored tree, and every state
// kept above it rebuilt on that tree from the blocks' change lists. States
// at or below the base, and those of branches forking below it, are
// dropped. Called with processMu held.
func (c *Chain) rebase(newBase *Entry, b db.Base) error {
	bt := state.FromRoot(b.Root, int(b.Count), c.cfg.State.View())
	c.mu.RLock()
	var above []*Entry
	for e := range c.retained {
		if e.Height > newBase.Height && e.changes != nil && descends(e, newBase) {
			above = append(above, e)
		}
	}
	c.mu.RUnlock()
	sort.Slice(above, func(i, j int) bool { return above[i].Height < above[j].Height })
	rebuilt := map[*Entry]*state.Tree{newBase: &bt}
	for _, e := range above {
		parent, ok := rebuilt[e.Parent]
		if !ok {
			continue // its parent's state is not kept: it is derived again when needed
		}
		t := parent.Apply(e.changes)
		if t.Root() != stateRootOf(e) {
			return fmt.Errorf("rebuilt state of block %s has root %s, its header %s", e.ID, t.Root(), stateRootOf(e))
		}
		rebuilt[e] = &t
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := range c.retained {
		e.state = nil
		delete(c.retained, e)
	}
	for e, t := range rebuilt {
		e.state = t
		c.retained[e] = struct{}{}
	}
	for _, e := range c.entries {
		if e.changes != nil && rebuilt[e] == nil {
			e.changes = nil
		}
	}
	newBase.changes = nil
	c.base = newBase
	return nil
}

// descends reports whether e is a or a descendant of a.
func descends(e, a *Entry) bool {
	for e != nil && e.Height > a.Height {
		e = e.Parent
	}
	return e == a
}

// missingUndo reports a block whose undo data a rewind needs and the
// database does not hold.
type missingUndo struct{ height uint64 }

func (m *missingUndo) Error() string {
	return fmt.Sprintf("the database holds no undo data for the block at height %d", m.height)
}

// rewind moves the base down to to, an ancestor of it, with the stored undo
// data; nothing changes when some is missing (*missingUndo). Called with
// processMu held.
func (c *Chain) rewind(to *Entry) (err error) {
	defer state.Recover(&err)
	c.mu.RLock()
	base := c.base
	c.mu.RUnlock()
	// The stored state, which the base's own may no longer be kept as: an
	// operator's mark below the base drops the states above the mark.
	baseTree, err := c.storedTree()
	if err != nil {
		return err
	}
	t := baseTree
	keys := map[state.Key]struct{}{}
	var drop []db.BlockUndo
	for b := base; b != to; b = b.Parent {
		prev, ok, err := c.cfg.State.Undo(b.Height, b.ID)
		if err != nil {
			return err
		}
		if !ok {
			return &missingUndo{height: b.Height}
		}
		t = t.Apply(prev)
		if t.Root() != stateRootOf(b.Parent) {
			return fmt.Errorf("undoing block %s gives state root %s, its parent's is %s", b.ID, t.Root(), stateRootOf(b.Parent))
		}
		for _, ch := range prev {
			keys[ch.Key] = struct{}{}
		}
		drop = append(drop, db.BlockUndo{Height: b.Height, Block: b.ID})
	}
	commit := c.commitFor(baseTree, t, keys, to)
	commit.DropUndo = drop
	commit.Rewind = true
	c.mu.Lock()
	c.rewinds++
	c.mu.Unlock()
	if err := c.cfg.State.Commit(commit); err != nil {
		return err
	}
	return c.rebase(to, commit.Base)
}

// storedTree is the state the database holds: the base's.
func (c *Chain) storedTree() (state.Tree, error) {
	b, ok, err := c.cfg.State.Base()
	if err != nil || !ok {
		return state.Empty, err
	}
	return state.FromRoot(b.Root, int(b.Count), c.cfg.State.View()), nil
}

// commonAncestor is the latest block both a and b descend from.
func commonAncestor(a, b *Entry) *Entry {
	for a.Height > b.Height {
		a = a.Parent
	}
	for b.Height > a.Height {
		b = b.Parent
	}
	for a != b {
		a, b = a.Parent, b.Parent
	}
	return a
}

// syncBlocks forces the stored blocks to disk, before a commit moves the
// base past them.
func (c *Chain) syncBlocks() error {
	if s, ok := c.store.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
}

// indexRows are the occurrence and event index rows of a stored block.
// Called with processMu held.
func (c *Chain) indexRows(b *Entry) ([]db.Occurrence, []db.EventRow, error) {
	_, body, err := c.readBody(b)
	if err != nil {
		c.forgetBody(b, err)
		return nil, nil, fmt.Errorf("block at height %d: %w", b.Height, errBodyLost)
	}
	occ := make([]db.Occurrence, len(body.Transactions))
	var events []db.EventRow
	for i, tx := range body.Transactions {
		r, err := types.DecodeReceipt(body.Receipts[i])
		if err != nil {
			return nil, nil, fmt.Errorf("stored block %s: %w", b.ID, err)
		}
		occ[i] = db.Occurrence{Tx: types.TransactionID(c.chainID, tx), Height: b.Height, Block: b.ID,
			Index: uint32(i), Status: r.Status}
		for j, ev := range r.Events {
			row := db.EventRow{Contract: ev.Contract, Height: b.Height, Block: b.ID, Tx: uint32(i), Event: uint32(j)}
			if len(ev.Payload) >= 32 {
				topic := hashing.Hash(ev.Payload[:32])
				row.Topic = &topic
			}
			events = append(events, row)
		}
	}
	return occ, events, nil
}

// Database returns the chain's database, or nil without one.
func (c *Chain) Database() *db.DB { return c.cfg.State }
