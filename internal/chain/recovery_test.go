package chain_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// recovering is a chain with a database whose storage failures are
// recorded rather than fatal to the test.
type recovering struct {
	*chain.Chain
	db    *db.DB
	store *chain.FileStore
	fatal error
}

func openRecovering(t *testing.T, dir string, depth, undo uint64) *recovering {
	t.Helper()
	g := types.DevelopmentGenesis()
	d, err := db.Open(filepath.Join(dir, "chain.db"), g.ChainID())
	if err != nil {
		t.Fatal(err)
	}
	store, err := chain.OpenStoredBlocks(filepath.Join(dir, "blocks"), g.ChainID(), d)
	if err != nil {
		t.Fatal(err)
	}
	r := &recovering{db: d, store: store}
	cfg := chain.DefaultConfig()
	cfg.State, cfg.RetainDepth, cfg.UndoWindow = d, depth, undo
	cfg.MarksPath = filepath.Join(dir, "marks.json")
	cfg.Fatal = func(err error) { r.fatal = err }
	if r.Chain, err = chain.New(g, chain.NoSeed, store, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return r
}

func (r *recovering) close() {
	r.Close()
	r.db.Close()
	r.store.Close()
}

func (r *recovering) process(t *testing.T, raws [][]byte) {
	t.Helper()
	for i, raw := range raws {
		if _, err := r.ProcessBlock(raw); err != nil && !errors.Is(err, chain.ErrKnown) {
			t.Fatalf("block %d: %v", i, err)
		}
	}
	if r.fatal != nil {
		t.Fatalf("storage failure: %v", r.fatal)
	}
}

// checkTip checks the tip against want's, and its state against its header.
func (r *recovering) checkTip(t *testing.T, want *chain.Entry) {
	t.Helper()
	if r.Tip().ID != want.ID {
		t.Fatalf("tip at height %d, want %d", r.Tip().Height, want.Height)
	}
	st, err := r.StateAt(r.Tip())
	if err != nil || st.Root() != want.Header.StateRoot {
		t.Fatalf("state at the tip: %v", err)
	}
	if r.fatal != nil {
		t.Fatalf("storage failure: %v", r.fatal)
	}
}

// damage flips a byte in the middle of a stored block's frame.
func damage(t *testing.T, dir string, loc chain.Location) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "blocks", fmt.Sprintf("%08d.dat", loc.Segment)), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 1)
	at := loc.Offset + loc.Length/2
	if _, err := f.ReadAt(b, at); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b, at); err != nil {
		t.Fatal(err)
	}
}

// locate finds a stored block's frame: listed in the database, or written
// after the frames it lists.
func (r *recovering) locate(t *testing.T, id hashing.Hash) chain.Location {
	t.Helper()
	var loc chain.Location
	r.db.Blocks(func(row db.BlockRow) error {
		if row.ID == id {
			loc = chain.Location{Segment: row.Segment, Offset: row.Offset, Length: row.Length}
		}
		return nil
	})
	for _, f := range r.store.FramesAfter(chain.Cursor{}) {
		if f.ID == id {
			loc = f.Location
		}
	}
	if loc.Length == 0 {
		t.Fatalf("block %s has no frame", id)
	}
	return loc
}

func wanted(c *chain.Chain, id hashing.Hash) bool {
	for _, w := range c.BlocksWanted("p") {
		if w.ID == id {
			return true
		}
	}
	return false
}

// A block above the base whose frame no longer reads after a restart is
// fetched again instead of stopping the node: it loads at the base, and the
// block brought again moves it back to its tip.
func TestStoredRefetchesUnreadableBlock(t *testing.T) {
	r := rand.New(rand.NewPCG(15, 16))
	src := newChain(t, nil)
	raws := branch(t, src, 40, src.Genesis().GenesisTimestamp, 1, r)
	dir := t.TempDir()
	c := openRecovering(t, dir, 3, chain.DefaultUndoWindow)
	c.process(t, raws)
	base := c.Base()
	victim, _ := c.AtHeight(base.Height + 1)
	loc := c.locate(t, victim.ID)
	c.close()
	damage(t, dir, loc)

	c = openRecovering(t, dir, 3, chain.DefaultUndoWindow)
	if c.Tip().ID != base.ID || c.fatal != nil {
		t.Fatalf("loaded at height %d (base %d): %v", c.Tip().Height, base.Height, c.fatal)
	}
	if !wanted(c.Chain, victim.ID) {
		t.Fatal("the unreadable block is not fetched again")
	}
	c.process(t, raws[victim.Height-1:victim.Height])
	c.checkTip(t, src.Tip())
	c.close()

	// The block is stored again in a later frame, which a restart reads in
	// place of the one the database still lists.
	c = openRecovering(t, dir, 3, chain.DefaultUndoWindow)
	defer c.close()
	c.checkTip(t, src.Tip())
}

// An executed block that no longer reads when the base would move past it
// is fetched again first; the base waits for it instead of failing.
func TestStoredRefetchesLostExecutedBlock(t *testing.T) {
	r := rand.New(rand.NewPCG(21, 22))
	src := newChain(t, nil)
	raws := branch(t, src, 30, src.Genesis().GenesisTimestamp, 1, r)
	dir := t.TempDir()
	c := openRecovering(t, dir, 4, chain.DefaultUndoWindow)
	defer c.close()
	c.process(t, raws[:20])
	base := c.Base()
	// The next move of the base executes nothing again but reads every
	// block it passes; damage the first of them.
	victim, _ := c.AtHeight(base.Height + 1)
	damage(t, dir, c.locate(t, victim.ID))
	c.process(t, raws[20:])
	if c.Base() != base {
		t.Fatalf("the base moved from %d to %d past an unreadable block", base.Height, c.Base().Height)
	}
	if w := c.BlocksWanted("p"); len(w) == 0 || w[0].ID != victim.ID {
		t.Fatal("the executed block is not fetched first")
	}
	c.process(t, raws[victim.Height-1:victim.Height])
	more := branch(t, src, 1, src.Tip().Timestamp, 1, r)
	c.process(t, more)
	if c.Base().Height <= victim.Height {
		t.Fatalf("the base stayed at %d once the block was stored again", c.Base().Height)
	}
	c.checkTip(t, src.Tip())
}

// twoBranches mines a common start of 5 blocks, then branch A of na blocks
// and a heavier branch B of nb blocks from it.
func twoBranches(t *testing.T, seed uint64, na, nb int) (common, a, b [][]byte, srcA, srcB *chain.Chain) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, seed+1))
	srcA = newChain(t, nil)
	common = branch(t, srcA, 5, srcA.Genesis().GenesisTimestamp, 1, r)
	srcB = newChain(t, nil)
	for _, raw := range common {
		if _, err := srcB.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	forkTime := srcA.Tip().Timestamp
	a = branch(t, srcA, na, forkTime, 2, r)
	b = branch(t, srcB, nb, forkTime+1, 3, r)
	return
}

// A heavier branch that forks below the undo window: the node stays on its
// tip while it replays the chain from genesis to rebuild the undo data,
// then reorganizes.
func TestStoredReplayBelowUndoWindow(t *testing.T) {
	common, a, b, srcA, srcB := twoBranches(t, 11, 40, 50)
	c := openRecovering(t, t.TempDir(), 2, 10)
	defer c.close()
	c.process(t, append(common, a...))
	if floor, _ := c.db.UndoFloor(); floor <= 6 {
		t.Fatalf("undo floor %d: the fork is not below the window", floor)
	}
	c.process(t, b)
	// The node stays on its branch while the replay runs.
	if tip := c.Tip(); tip.ID != srcA.Tip().ID && tip.ID != srcB.Tip().ID {
		t.Fatalf("tip at height %d during the replay", tip.Height)
	}
	c.WaitReplay()
	c.checkTip(t, srcB.Tip())
	if c.Replaying() {
		t.Fatal("still replaying")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(c.db.Path()), "replay")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the replay's database was left behind: %v", err)
	}
}

// A replay that did not happen before a stop happens after the restart.
func TestStoredReplayAfterRestart(t *testing.T) {
	common, a, b, srcA, srcB := twoBranches(t, 23, 40, 50)
	dir := t.TempDir()
	c := openRecovering(t, dir, 2, 10)
	c.process(t, append(common, a...))
	c.Close() // no replay starts after this
	c.process(t, b)
	c.checkTip(t, srcA.Tip())
	c.db.Close()
	c.store.Close()

	c = openRecovering(t, dir, 2, 10)
	defer c.close()
	// While the replay runs, the node is on the branch it can connect.
	if tip := c.Tip(); tip.ID != srcA.Tip().ID && tip.ID != srcB.Tip().ID {
		t.Fatalf("loaded at height %d during the replay", tip.Height)
	}
	c.WaitReplay()
	c.checkTip(t, srcB.Tip())
}

// The operator's mark on a block below the undo window: the tip goes back
// below it once a replay rebuilds the undo data, also when the node stops
// before the replay is done.
func TestStoredMarkBelowUndoWindow(t *testing.T) {
	for _, restart := range []bool{false, true} {
		common, a, _, _, _ := twoBranches(t, 25, 40, 1)
		dir := t.TempDir()
		c := openRecovering(t, dir, 2, 10)
		c.process(t, append(common, a...))
		want, _ := c.AtHeight(4)
		marked, _ := c.AtHeight(5)
		if restart {
			c.Close() // no replay starts after this
		}
		if _, err := c.MarkInvalid(marked.ID); err != nil {
			t.Fatal(err)
		}
		if restart {
			if c.Base().Height <= 4 {
				t.Fatal("the base moved without a replay")
			}
			c.db.Close()
			c.store.Close()
			c = openRecovering(t, dir, 2, 10)
		}
		c.WaitReplay()
		c.checkTip(t, want)
		if c.Base().Height > 4 {
			t.Fatalf("restart %v: base at %d above the tip", restart, c.Base().Height)
		}
		c.close()
	}
}

// Blocks the database lists above a mark come back when the mark is
// reconsidered after a restart, without being fetched again.
func TestStoredReconsiderAfterRestart(t *testing.T) {
	r := rand.New(rand.NewPCG(27, 28))
	src := newChain(t, nil)
	raws := branch(t, src, 30, src.Genesis().GenesisTimestamp, 1, r)
	dir := t.TempDir()
	c := openRecovering(t, dir, 2, chain.DefaultUndoWindow)
	c.process(t, raws)
	marked, _ := c.AtHeight(20)
	if _, err := c.MarkInvalid(marked.ID); err != nil {
		t.Fatal(err)
	}
	c.close()
	c = openRecovering(t, dir, 2, chain.DefaultUndoWindow)
	defer c.close()
	if c.Tip().Height != 19 {
		t.Fatalf("tip %d after the restart", c.Tip().Height)
	}
	if _, err := c.Reconsider(marked.ID); err != nil {
		t.Fatal(err)
	}
	c.checkTip(t, src.Tip())
}
