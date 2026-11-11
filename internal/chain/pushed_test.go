package chain_test

import (
	"errors"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/types"
)

// forkOf returns a chain holding c's canonical blocks up to height, to mine
// another branch on.
func forkOf(t *testing.T, c *chain.Chain, height uint64) *chain.Chain {
	t.Helper()
	other := newChain(t, nil)
	for h := uint64(1); h <= height; h++ {
		e, _ := c.AtHeight(h)
		raw, err := c.Block(e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	return other
}

func idOf(raw []byte) [32]byte { return types.HeaderIDFromBytes(raw[:types.HeaderSize]) }

// A sibling of the tip costs its sender one block of work: pushed unasked it
// is held in memory for its connection, not stored or indexed, and the block
// that makes its branch overtake the tip stores and connects the run.
func TestPushedSiblingIsHeldUntilItsBranchOvertakes(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, 3, g.GenesisTimestamp, 60, "main")
	other := forkOf(t, c, 2)
	side := mineEmpty(t, other, 2, g.GenesisTimestamp+200, 61, "side")
	headers, stored, _ := c.Counts()

	if _, err := c.ProcessPushed(side[0], "a"); !errors.Is(err, chain.ErrHeld) {
		t.Fatalf("sibling of the tip: %v", err)
	}
	if _, ok := c.Get(idOf(side[0])); ok {
		t.Fatal("a held block was indexed")
	}
	if h, s, _ := c.Counts(); h != headers || s != stored {
		t.Fatalf("a held block changed the index or the store: %d headers, %d stored", h, s)
	}
	if runs, _, bytes := c.Held(); bytes != len(side[0]) || runs != 1 {
		t.Fatalf("held %d bytes in %d runs", bytes, runs)
	}
	// Pushed again it is known, and costs nothing more.
	if _, err := c.ProcessPushed(side[0], "a"); !errors.Is(err, chain.ErrKnown) {
		t.Fatalf("pushed again: %v", err)
	}
	// Another connection knows nothing of the run.
	if _, err := c.ProcessPushed(side[1], "b"); !errors.Is(err, chain.ErrUnknownParent) {
		t.Fatalf("child from another connection: %v", err)
	}
	// Its child makes the run overtake the tip: both are stored and
	// connected.
	e, err := c.ProcessPushed(side[1], "a")
	if err != nil {
		t.Fatal(err)
	}
	if c.Tip() != e || c.Tip().Height != 4 || !c.IsCanonical(e.Parent) {
		t.Fatalf("tip at height %d", c.Tip().Height)
	}
	if runs, _, bytes := c.Held(); bytes != 0 || runs != 0 {
		t.Fatalf("still holding %d bytes in %d runs", bytes, runs)
	}
}

// A run goes with its connection, and the branch is taken when it is pushed
// again from the block both nodes hold.
func TestHeldRunGoesWithItsConnection(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, 3, g.GenesisTimestamp, 60, "main")
	other := forkOf(t, c, 2)
	side := mineEmpty(t, other, 2, g.GenesisTimestamp+200, 61, "side")
	if _, err := c.ProcessPushed(side[0], "a"); !errors.Is(err, chain.ErrHeld) {
		t.Fatal(err)
	}
	c.ReleasePushed("a")
	if runs, _, bytes := c.Held(); bytes != 0 || runs != 0 {
		t.Fatalf("held %d bytes in %d runs after the connection ended", bytes, runs)
	}
	if _, err := c.ProcessPushed(side[1], "a"); !errors.Is(err, chain.ErrUnknownParent) {
		t.Fatalf("child after the run went: %v", err)
	}
	for i, raw := range side {
		if _, err := c.ProcessPushed(raw, "a2"); err != nil && !(i == 0 && errors.Is(err, chain.ErrHeld)) {
			t.Fatal(i, err)
		}
	}
	if c.Tip().ID != idOf(side[1]) {
		t.Fatal("the branch pushed again was not connected")
	}
}

// A pusher that moves within its run goes on from the block it moved from.
func TestPusherMovesWithinItsRun(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, 5, g.GenesisTimestamp, 60, "main")
	// b3, b4 on block 2; then c4 on b3.
	one := forkOf(t, c, 2)
	// On the main chain's schedule, so that each block carries its work.
	b := mineEmpty(t, one, 2, g.GenesisTimestamp+120, 60, "b")
	two := forkOf(t, c, 2)
	if _, err := two.ProcessBlock(b[0]); err != nil {
		t.Fatal(err)
	}
	cs := mineEmpty(t, two, 3, g.GenesisTimestamp+180, 60, "c")
	for _, raw := range b {
		if _, err := c.ProcessPushed(raw, "a"); !errors.Is(err, chain.ErrHeld) {
			t.Fatal(err)
		}
	}
	// c4 follows b3, which is in the run but not its last block.
	if _, err := c.ProcessPushed(cs[0], "a"); !errors.Is(err, chain.ErrHeld) {
		t.Fatalf("moving within the run: %v", err)
	}
	if _, _, bytes := c.Held(); bytes != len(b[0])+len(cs[0]) {
		t.Fatalf("the run was not cut back: %d bytes", bytes)
	}
	if _, err := c.ProcessPushed(cs[1], "a"); !errors.Is(err, chain.ErrHeld) {
		t.Fatal(err)
	}
	if _, err := c.ProcessPushed(cs[2], "a"); err != nil {
		t.Fatal(err)
	}
	if c.Tip().ID != idOf(cs[2]) {
		t.Fatalf("tip at height %d", c.Tip().Height)
	}
}

// A block whose body does not match its header leaves no header behind: two
// such, above the tip, would hold the best header there for as long as the
// tip did not move, with the miner waiting for blocks nobody has.
func TestBadBodyLeavesNoHeader(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, 2, g.GenesisTimestamp, 60, "main")
	other := forkOf(t, c, 2)
	next := mineEmpty(t, other, 1, g.GenesisTimestamp+200, 61, "next")[0]
	bad := append([]byte(nil), next...)
	bad = append(bad[:len(bad)-1], bad[len(bad)-1]^1, 0)
	for _, push := range []func([]byte) (*chain.Entry, error){
		func(raw []byte) (*chain.Entry, error) { return c.ProcessPushed(raw, "a") },
		c.ProcessBlock,
	} {
		if _, err := push(bad); !errors.Is(err, chain.ErrInvalid) {
			t.Fatalf("bad body: %v", err)
		}
		if _, ok := c.Get(idOf(bad)); ok || c.BestHeader() != c.Tip() {
			t.Fatal("a header was indexed for a body that does not match it")
		}
	}
	// The same header with its own body is still taken.
	if _, err := c.ProcessPushed(next, "a"); err != nil {
		t.Fatal(err)
	}
	if c.Tip().ID != idOf(next) {
		t.Fatal("the real block was not connected")
	}
}

func headersOf(t *testing.T, raws ...[]byte) []*types.Header {
	t.Helper()
	var out []*types.Header
	for _, raw := range raws {
		h, err := types.DecodeHeader(raw[:types.HeaderSize])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

// Headers a peer shows are kept only once their chain overtakes the best
// header: siblings, one block of work each, leave the index as it was.
func TestSideHeadersAreKeptOnlyOnceTheyOvertake(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, 5, g.GenesisTimestamp, 60, "main")
	before, _, _ := c.Counts()
	// Forty siblings of the tip, each from its own miner.
	var siblings [][]byte
	for i := 0; i < 40; i++ {
		other := forkOf(t, c, 4)
		siblings = append(siblings, mineEmpty(t, other, 1, g.GenesisTimestamp+300+uint64(i), 61, "s")[0])
	}
	res, err := c.HeadersFrom("p", headersOf(t, siblings...))
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 0 || res.Next != nil {
		t.Fatalf("a page of siblings: %+v", res)
	}
	if n, _, _ := c.Counts(); n != before {
		t.Fatalf("siblings were indexed: %d headers, %d before", n, before)
	}
	// A page of copies of one header begins one pre-sync and ends there.
	copies := make([][]byte, 500)
	for i := range copies {
		copies[i] = siblings[0]
	}
	if res, err := c.HeadersFrom("q", headersOf(t, copies...)); err != nil || res.Indexed != 0 || res.Next != nil {
		t.Fatalf("a page of copies: %+v %v", res, err)
	}
	// A sibling with its child overtakes: the branch is fetched again from
	// where it forked, indexed, and is the best header.
	other := forkOf(t, c, 4)
	branch := mineEmpty(t, other, 2, g.GenesisTimestamp+900, 61, "branch")
	hs := headersOf(t, branch...)
	if res, err := c.HeadersFrom("r", hs[:1]); err != nil || res.Indexed != 0 {
		t.Fatalf("the sibling alone: %+v %v", res, err)
	}
	res, err = c.HeadersFrom("r", hs)
	if err != nil || !res.Refetch || res.Next == nil || *res.Next != hs[0].ParentID {
		t.Fatalf("expected the branch to be asked for again from its fork: %+v %v", res, err)
	}
	res, err = c.HeadersFrom("r", hs)
	if err != nil || res.Indexed != 2 {
		t.Fatalf("the branch fetched again: %+v %v", res, err)
	}
	if c.BestHeader().ID != idOf(branch[1]) {
		t.Fatal("the overtaking branch is not the best header")
	}
	// Its blocks are then wanted, and connect.
	for _, raw := range branch {
		if _, err := c.ProcessBlockFrom(raw, "r"); err != nil {
			t.Fatal(err)
		}
	}
	if c.Tip().ID != idOf(branch[1]) {
		t.Fatal("the branch was not connected")
	}
}

// A branch tied with the best header, fetched again after the best header
// moved on, is not indexed.
func TestPresyncIndexesOnlyWhatStillOvertakes(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, 5, g.GenesisTimestamp, 60, "main")
	other := forkOf(t, c, 4)
	branch := mineEmpty(t, other, 2, g.GenesisTimestamp+900, 61, "branch")
	hs := headersOf(t, branch...)
	if res, err := c.HeadersFrom("p", hs); err != nil || !res.Refetch {
		t.Fatalf("%+v %v", res, err)
	}
	// The honest chain moves two blocks before the branch comes again.
	mineEmpty(t, c, 2, g.GenesisTimestamp+2000, 60, "main")
	before, _, _ := c.Counts()
	res, _ := c.HeadersFrom("p", hs)
	if n, _, _ := c.Counts(); res.Indexed != 0 || n != before {
		t.Fatalf("a branch the best header passed was indexed: %+v", res)
	}
}

// A run grows where it is kept, while the status page counts what is held
// and other connections end: run under the race detector.
func TestHeldIsReadWhileARunGrows(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, 6, g.GenesisTimestamp, 60, "main")
	other := forkOf(t, c, 2)
	side := mineEmpty(t, other, 3, g.GenesisTimestamp+120, 60, "side")
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				c.Held()
				c.ReleasePushed("another")
			}
		}
	}()
	for _, raw := range side {
		if _, err := c.ProcessPushed(raw, "a"); !errors.Is(err, chain.ErrHeld) {
			t.Fatal(err)
		}
	}
	close(stop)
	<-done
	if runs, blocks, _ := c.Held(); runs != 1 || blocks != len(side) {
		t.Fatalf("held %d blocks in %d runs", blocks, runs)
	}
}
