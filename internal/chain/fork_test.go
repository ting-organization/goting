package chain_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/types"
)

// mineEmpty mines n empty blocks on c's tip with timestamps spaced by gap
// seconds from start, returning their bytes.
func mineEmpty(t *testing.T, c *chain.Chain, n int, start, gap uint64, producer string) [][]byte {
	t.Helper()
	var out [][]byte
	ts := start
	for i := 0; i < n; i++ {
		ts += gap
		e, _, err := miner.MineOne(context.Background(), c, miner.Options{Timestamp: ts, Producer: []byte(producer)}, nil)
		if err != nil {
			t.Fatalf("mine %d: %v", i, err)
		}
		raw, _ := c.Block(e.ID)
		out = append(out, raw)
	}
	return out
}

func TestSideBranchStoredNotExecuted(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, 3, g.GenesisTimestamp, 60, "main")
	// A one-block side branch at height 1: less work, stored only.
	other := newChain(t, nil)
	side := mineEmpty(t, other, 1, g.GenesisTimestamp+1, 61, "side")
	e, err := c.ProcessBlock(side[0])
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != chain.StatusStored || c.IsCanonical(e) {
		t.Fatalf("side block status %v canonical %v", e.Status, c.IsCanonical(e))
	}
}

func TestLowWorkForkRefused(t *testing.T) {
	// Experimental: a far-behind fork is cheap, so the threshold must hold.
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, chain.ThresholdBlocks+5, g.GenesisTimestamp, 60, "main")
	other := newChain(t, nil)
	fork := mineEmpty(t, other, 1, g.GenesisTimestamp, 61, "fork")
	_, err := c.ProcessBlock(fork[0])
	if !errors.Is(err, chain.ErrBehind) {
		t.Fatalf("low-work fork: %v", err)
	}
}

func TestHeadersFromPresyncAdoptsHeavierDeepFork(t *testing.T) {
	// A chain that forks at genesis and stays below the work threshold for
	// its first pages, then carries more work: indexed only after the
	// pre-sync's refetch.
	c := newChain(t, nil)
	g := c.Genesis()
	mineEmpty(t, c, chain.ThresholdBlocks+10, g.GenesisTimestamp, 60, "main")
	other := newChain(t, nil)
	mineEmpty(t, other, chain.ThresholdBlocks+30, g.GenesisTimestamp, 60, "fork")
	var headers []*types.Header
	for h := uint64(1); h <= other.Tip().Height; h++ {
		e, _ := other.AtHeight(h)
		headers = append(headers, e.Header)
	}
	res, err := c.HeadersFrom("p", headers[:2])
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 0 || res.Next == nil {
		t.Fatalf("expected a pre-sync to start: %+v", res)
	}
	// Stream the rest: the pre-sync reaches the threshold and asks again.
	res, err = c.HeadersFrom("p", headers[2:])
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refetch || *res.Next != c.GenesisID() {
		t.Fatalf("expected a refetch from genesis: %+v", res)
	}
	// The refetch indexes every header; the best header is the fork's tip.
	res, err = c.HeadersFrom("p", headers)
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != len(headers) || c.BestHeader().ID != other.Tip().ID {
		t.Fatalf("indexed %d, best %s", res.Indexed, c.BestHeader().ID)
	}
	// Bodies are then wanted along the fork, and connecting them reorganizes.
	if w := c.BlocksWanted("p"); len(w) != 64 || w[0].ID != headers[0].ID() {
		t.Fatalf("wanted %d blocks", len(w))
	}
	for h := uint64(1); h <= other.Tip().Height; h++ {
		e, _ := other.AtHeight(h)
		raw, _ := other.Block(e.ID)
		if _, err := c.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	if c.Tip().ID != other.Tip().ID {
		t.Fatal("did not reorganize to the heavier fork")
	}
}

// A block that fails only once its branch is activated, after another
// peer's block made the branch heaviest, makes its deliverer an offender.
func TestOffenders(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	tpl1, err := miner.Build(c, c.Tip(), miner.Options{Timestamp: g.GenesisTimestamp + 60}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := *tpl1.Header
	good := remine(t, &types.Block{Header: &h, Body: tpl1.Body})
	if _, err := c.ProcessBlockFrom(good, "9.9.9.1:1"); err != nil {
		t.Fatal(err)
	}
	bh := *tpl1.Header
	bh.StateRoot[0] ^= 1
	bad := remine(t, &types.Block{Header: &bh, Body: tpl1.Body})
	if _, err := c.ProcessBlockFrom(bad, "9.9.9.2:1"); err != nil {
		t.Fatalf("an equal-work side block is stored: %v", err)
	}
	if o := c.Offenders(); len(o) != 0 {
		t.Fatalf("offenders before activation: %v", o)
	}
	tpl2, err := miner.Build(c, c.Tip(), miner.Options{Timestamp: g.GenesisTimestamp + 120}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch := *tpl2.Header
	ch.ParentID = types.HeaderIDFromBytes(bad[:types.HeaderSize])
	child := remine(t, &types.Block{Header: &ch, Body: tpl2.Body})
	c.ProcessBlockFrom(child, "9.9.9.3:1")
	// As the reference node does, whoever delivered a block the failure
	// invalidates is an offender: the child's deliverer passed on a block
	// above an invalid one. The honest tip's deliverer is not.
	o := c.Offenders()
	sort.Strings(o)
	if len(o) != 2 || o[0] != "9.9.9.2:1" || o[1] != "9.9.9.3:1" {
		t.Fatalf("offenders %v", o)
	}
	if c.Tip().ID != types.HeaderIDFromBytes(good[:types.HeaderSize]) {
		t.Fatal("tip moved to an invalid branch")
	}
}
