package node

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/types"
	. "github.com/ting-organization/goting/internal/wasm/wasmtest"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func pooled(src string, n int) []*Pooled {
	var out []*Pooled
	for i := 0; i < n; i++ {
		var a netip.Addr
		if src != "" {
			a = addr(src)
		}
		out = append(out, &Pooled{ID: hashing.Hash{byte(len(out)), byte(i)}, Source: a})
	}
	return out
}

func TestFairShare(t *testing.T) {
	pool := append(pooled("10.0.0.1", 3), pooled("10.0.0.2", 1)...)
	// A newcomer from a third address: the newest of the largest gives way.
	if at := fairShare(pool, addr("10.0.0.3")); at != 2 {
		t.Fatalf("gave way at %d", at)
	}
	// From the largest itself: nobody gives way.
	if at := fairShare(pool, addr("10.0.0.1")); at != -1 {
		t.Fatalf("largest evicted itself: %d", at)
	}
	// The node's own transactions never give way, but its submissions may
	// evict a relayed one.
	own := pooled("", 4)
	if at := fairShare(own, addr("10.0.0.3")); at != -1 {
		t.Fatal("own transactions gave way")
	}
	if at := fairShare(pool, netip.Addr{}); at != 2 {
		t.Fatalf("own submission: %d", at)
	}
}

func TestTurns(t *testing.T) {
	a, b := addr("10.0.0.1"), addr("10.0.0.2")
	pool := []*Pooled{
		{ID: hashing.Hash{1}, Source: a}, {ID: hashing.Hash{2}, Source: a},
		{ID: hashing.Hash{3}, Source: b}, {ID: hashing.Hash{4}, Source: a, Tried: 5},
	}
	// Untried first; the first of each address before the second of any.
	got := turns(pool, map[netip.Addr]uint64{})
	want := []int{0, 2, 1, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v want %v", got, want)
		}
	}
	// An address tried more recently waits behind the others.
	got = turns(pool, map[netip.Addr]uint64{a: 7})
	if got[0] != 2 {
		t.Fatalf("order %v", got)
	}
}

func newTestNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	c, err := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return New(c, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func callTx(target hashing.Hash, input []byte, budget types.ResourceVector) []byte {
	return (&types.Transaction{Kind: types.KindCall, Budget: budget, Target: target, Input: input}).Encode()
}

func TestPoolBounds(t *testing.T) {
	n := newTestNode(t, Config{PoolTransactions: 3})
	budget := types.ResourceVector{1000, 10, 1000, 0}
	for i := 0; i < 3; i++ {
		if _, err := n.Submit(callTx(hashing.Hash{byte(i)}, nil, budget), nil, addr("10.0.0.1"), false); err != nil {
			t.Fatal(err)
		}
	}
	// Full: the same address is refused; another address evicts one.
	if _, err := n.Submit(callTx(hashing.Hash{9}, nil, budget), nil, addr("10.0.0.1"), false); err != ErrPoolFull {
		t.Fatalf("expected full, got %v", err)
	}
	if _, err := n.Submit(callTx(hashing.Hash{10}, nil, budget), nil, addr("10.0.0.2"), false); err != nil {
		t.Fatal(err)
	}
	if n.PoolLen() != 3 {
		t.Fatalf("pool %d", n.PoolLen())
	}
	// Larger than the next block's data capacity.
	big := callTx(hashing.Hash{11}, make([]byte, 1<<20), types.ResourceVector{1000, 10, 1 << 21, 0})
	if _, err := n.Submit(big, nil, netip.Addr{}, false); err != ErrTooLarge {
		t.Fatalf("expected too large, got %v", err)
	}
	// Known again.
	r, err := n.Submit(callTx(hashing.Hash{10}, nil, budget), nil, addr("10.0.0.2"), false)
	if err != nil || !r.Known {
		t.Fatalf("resubmission: %+v %v", r, err)
	}
}

// failOnInput reverts when its input is "no" and succeeds otherwise.
func failOnInput() []byte {
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	input := m.Host("input")
	revert := m.Host("revert")
	m.Invoke(nil,
		I32Const(0), I32Const(2), Call(input), I32Const(2), Op(0x46),
		If(0x40), I32Const(0), Mem(0x2f, 1, 0), I32Const(0x6f6e), Op(0x46),
		If(0x40), I32Const(0), I32Const(0), Call(revert), Op(End), Op(End),
		Op(End))
	return m.Bytes()
}

func TestDeferredLifecycle(t *testing.T) {
	n := newTestNode(t, Config{})
	g := n.Chain.Genesis()
	_ = g
	// Deploy a counter and the reverting program in a mined block.
	budget := types.ResourceVector{50_000_000, 64, 20_000, 20_000}
	for _, code := range [][]byte{samples.Counter(), failOnInput()} {
		if _, err := n.SubmitTransaction((&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: code}).Encode()); err != nil {
			t.Fatal(err)
		}
	}
	tpl, err := n.Template(nil)
	if err != nil || len(tpl.Receipts) != 2 {
		t.Fatalf("template: %v", err)
	}
	mineTemplate(t, n, tpl)
	var counter, failer hashing.Hash
	copy(counter[:], tpl.Receipts[0].Result)
	copy(failer[:], tpl.Receipts[1].Result)

	// A deferred call that is due now: it stays deferred, is tried on the
	// tip, and is admitted to the pool on success.
	past := now() - 10
	okTx := callTx(counter, nil, budget)
	r, err := n.Submit(okTx, &past, netip.Addr{}, false)
	if err != nil || r.Deferred == nil || !*r.Deferred {
		t.Fatalf("deferred submit: %+v %v", r, err)
	}
	// One that fails on the tip stays deferred and out of the pool.
	badTx := callTx(failer, []byte("no"), budget)
	if _, err := n.Submit(badTx, &past, netip.Addr{}, false); err != nil {
		t.Fatal(err)
	}
	if err := n.tick(); err != nil {
		t.Fatal(err)
	}
	okID := types.TransactionID(n.Chain.ChainID(), okTx)
	badID := types.TransactionID(n.Chain.ChainID(), badTx)
	_, d, pooled := n.GetTx(okID)
	if d == nil || !pooled || !d.Admitted {
		t.Fatalf("successful deferred entry: deferred=%v pooled=%v", d != nil, pooled)
	}
	_, d, pooled = n.GetTx(badID)
	if d == nil || pooled || d.LastResult != "Revert" {
		t.Fatalf("failing deferred entry: %+v pooled=%v", d, pooled)
	}
	// Admitted entries are relayed as deferred only.
	if ids, _ := n.TxIDs(false, nil, 10); len(ids) != 0 {
		t.Fatalf("pending inventory lists deferred entries: %v", ids)
	}
	// Mining includes it successfully: it leaves the deferred set.
	tpl, err = n.Template(nil)
	if err != nil {
		t.Fatal(err)
	}
	mineTemplate(t, n, tpl)
	if _, d, _ := n.GetTx(okID); d != nil {
		t.Fatal("included deferred entry stayed")
	}
	if _, d, _ := n.GetTx(badID); d == nil {
		t.Fatal("failing deferred entry left")
	}
	// Relayed resubmission of what a block just included is refused; the
	// node's own is taken.
	if _, err := n.Submit(okTx, nil, addr("10.0.0.9"), true); err != ErrIncluded {
		t.Fatalf("relayed: %v", err)
	}
	if _, err := n.Submit(okTx, nil, netip.Addr{}, false); err != nil {
		t.Fatalf("own: %v", err)
	}
	// Horizon.
	far := now() + DeferredHorizon + 10
	if _, err := n.Submit(callTx(counter, []byte{1}, budget), &far, netip.Addr{}, false); err != ErrHorizon {
		t.Fatalf("horizon: %v", err)
	}
}

func mineTemplate(t *testing.T, n *Node, tpl interface{}) {
	t.Helper()
	tp := tpl.(*minerTemplate)
	if _, err := solveTemplate(tp); err != nil {
		t.Fatal(err)
	}
	if _, err := n.SubmitBlock(tp.Block().Encode()); err != nil {
		t.Fatal(err)
	}
}

func TestNeverFittingTransactionLeaves(t *testing.T) {
	n := newTestNode(t, Config{})
	// An infinite loop with a budget above the block's compute: it cannot
	// fit even an empty block, so the template drops it.
	m := &Module{}
	m.Invoke(nil, Loop(0x40), Br(0), Op(End), Op(End))
	tx := (&types.Transaction{Kind: types.KindDeploy, Budget: types.ResourceVector{1 << 40, 64, 10000, 10000},
		Code: m.Bytes()}).Encode()
	if _, err := n.SubmitTransaction(tx); err != nil {
		t.Fatal(err)
	}
	tpl, err := n.Template(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tpl.Body.Transactions) != 0 || n.PoolLen() != 0 {
		t.Fatalf("kept: %d in block, %d pooled", len(tpl.Body.Transactions), n.PoolLen())
	}
}

type minerTemplate = miner.Template

func solveTemplate(t *miner.Template) (hashing.Hash, error) {
	return miner.Solve(context.Background(), t.Header, 1)
}
