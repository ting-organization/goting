package node

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/types"
)

func storedNode(t *testing.T, dir string, mine bool) (*Node, func()) {
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
	cfg := chain.DefaultConfig()
	cfg.State, cfg.RetainDepth = d, 2
	c, err := chain.New(g, chain.NoSeed, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Load(); err != nil {
		t.Fatal(err)
	}
	n := New(c, Config{Mine: mine, Workers: 1, Interval: 20 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.IndexCanonical()
	return n, func() { store.Close(); d.Close() }
}

// With a database, transactions and events of blocks below its base are
// found through it, before and after a restart.
func TestIndexesThroughDatabase(t *testing.T) {
	dir := t.TempDir()
	n, closeAll := storedNode(t, dir, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	deploy := (&types.Transaction{Kind: types.KindDeploy, Budget: types.ResourceVector{50_000_000, 64, 10_000, 10_000},
		Code: samples.Counter()}).Encode()
	id, err := n.SubmitTransaction(deploy)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the deployment", func() bool { return len(n.Occurrences(id)) == 1 })
	occ := n.Occurrences(id)[0]
	waitFor(t, "the base to pass the deployment", func() bool {
		b := n.Chain.Base()
		return b != nil && b.Height > occ.Height+2
	})
	cancel()
	<-done
	n.mu.RLock()
	inMemory := len(n.occ[id])
	n.mu.RUnlock()
	if inMemory != 0 {
		t.Fatal("a flushed block stayed in the memory index")
	}
	got := n.Occurrences(id)
	if len(got) != 1 || got[0] != occ {
		t.Fatalf("occurrence through the database: %+v", got)
	}
	var contract hashing.Hash
	raw, _ := n.Chain.Block(occ.Block)
	body, _ := types.DecodeBody(raw[types.HeaderSize:])
	r, _ := types.DecodeReceipt(body.Receipts[occ.Index])
	copy(contract[:], r.Result)
	groups, more := n.EventsOf(contract, nil, 1, 10)
	if len(groups) != 1 || more || groups[0][0].Block != occ.Block {
		t.Fatalf("events through the database: %v %v", groups, more)
	}
	closeAll()

	n2, close2 := storedNode(t, dir, false)
	defer close2()
	if got := n2.Occurrences(id); len(got) != 1 || got[0] != occ {
		t.Fatalf("occurrence after a restart: %+v", got)
	}
	if groups, _ := n2.EventsOf(contract, nil, 1, 10); len(groups) != 1 {
		t.Fatal("events after a restart")
	}
}
