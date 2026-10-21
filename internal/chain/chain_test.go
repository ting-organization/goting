package chain_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/commit"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/seed"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/vm"
	. "github.com/ting-organization/goting/internal/wasm/wasmtest"
)

func counterModule() []byte {
	// Increments a u64 counter at storage key 0 and returns it; emits it too.
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	get := m.Host("storage_get")
	set := m.Host("storage_set")
	emit := m.Host("emit")
	ret := m.Host("return")
	m.Invoke(nil,
		// storage_get(key=0, out=64, cap=8) -> ignore length (absent leaves zeros)
		I32Const(0), I32Const(64), I32Const(8), Call(get), Op(Drop),
		I32Const(64), I32Const(64), Mem(0x29, 3, 0), I64Const(1), Op(0x7c), Mem(0x37, 3, 0),
		I32Const(0), I32Const(64), I32Const(8), Call(set),
		I32Const(64), I32Const(8), Call(emit),
		I32Const(64), I32Const(8), Call(ret),
		Op(End))
	return m.Bytes()
}

var budget = types.ResourceVector{50_000_000, 200, 100_000, 100_000}

func newChain(t *testing.T, seedList []hashing.Hash) *chain.Chain {
	t.Helper()
	c, err := chain.New(types.DevelopmentGenesis(), seedListOrNone(seedList), chain.NewMemStore(), chain.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mine(t *testing.T, c *chain.Chain, ts uint64, txs ...[]byte) (*chain.Entry, *miner.Template) {
	t.Helper()
	e, tpl, err := miner.MineOne(context.Background(), c, miner.Options{Timestamp: ts, Producer: []byte("test")}, txs)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	if len(tpl.Skipped) > 0 {
		t.Fatalf("skipped: %v", tpl.Skipped)
	}
	return e, tpl
}

func TestMineAndReplay(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	deploy := (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: counterModule()}).Encode()
	e1, tpl := mine(t, c, g.GenesisTimestamp+60, deploy)
	if tpl.Receipts[0].Status != 0 {
		t.Fatalf("deploy status %d", tpl.Receipts[0].Status)
	}
	var id hashing.Hash
	copy(id[:], tpl.Receipts[0].Result)
	call := (&types.Transaction{Kind: types.KindCall, Budget: budget, Target: id}).Encode()
	// The same bytes twice in one block: two occurrences, both execute.
	_, tpl = mine(t, c, g.GenesisTimestamp+120, call, call)
	// The initializer counted once already.
	if tpl.Receipts[0].Result[0] != 2 || tpl.Receipts[1].Result[0] != 3 {
		t.Fatalf("counter results %v %v", tpl.Receipts[0].Result, tpl.Receipts[1].Result)
	}
	for i := 0; i < 5; i++ {
		mine(t, c, g.GenesisTimestamp+180+uint64(60*i), call)
	}
	mine(t, c, g.GenesisTimestamp+1000) // empty block
	if c.Tip().Height != 8 || e1.Height != 1 {
		t.Fatalf("tip height %d", c.Tip().Height)
	}

	// Replay every block on a fresh node pinned to their ids as its seed.
	var blocks [][]byte
	for h := uint64(1); h <= c.Tip().Height; h++ {
		e, _ := c.AtHeight(h)
		raw, err := c.Block(e.ID)
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, raw)
	}
	list, _ := seed.ListFromBundle(blocks)
	fresh := newChain(t, list)
	rep, err := seed.Replay(fresh, blocks, nil)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if rep.Tip.ID != c.Tip().ID || rep.Transactions != 8 {
		t.Fatalf("replay tip %s txs %d", rep.Tip.ID, rep.Transactions)
	}
	st, _ := fresh.Tip().State()
	st2, _ := c.Tip().State()
	if st.Root() != st2.Root() {
		t.Fatal("state roots differ")
	}

	// A different block at a seed height is refused before anything else.
	other := newChain(t, list[:1])
	if _, err := other.ProcessBlock(blocks[0]); err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), blocks[0]...)
	tampered[types.NonceOffset] ^= 1
	if _, err := newChain(t, list[:1]).ProcessBlock(tampered); !errors.Is(err, chain.ErrInvalid) {
		t.Fatalf("seed mismatch accepted: %v", err)
	}
}

func remine(t *testing.T, b *types.Block) []byte {
	t.Helper()
	if _, err := miner.Solve(context.Background(), b.Header, 0); err != nil {
		t.Fatal(err)
	}
	return b.Encode()
}

func TestRejections(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	deploy := (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: counterModule()}).Encode()
	tpl, err := miner.Build(c, c.Tip(), miner.Options{Timestamp: g.GenesisTimestamp + 60}, [][]byte{deploy})
	if err != nil {
		t.Fatal(err)
	}
	good := *tpl.Header
	cases := []struct {
		name   string
		mutate func(b *types.Block)
	}{
		{"state root", func(b *types.Block) { b.Header.StateRoot[0] ^= 1 }},
		{"receipts root", func(b *types.Block) { b.Header.ReceiptsRoot[0] ^= 1 }},
		{"transactions root", func(b *types.Block) { b.Header.TransactionsRoot[0] ^= 1 }},
		{"resources", func(b *types.Block) { b.Header.ResourcesUsed[types.C]++ }},
		{"timestamp", func(b *types.Block) { b.Header.Timestamp = g.GenesisTimestamp }},
		{"target", func(b *types.Block) { b.Header.Target[31] ^= 1 }},
		{"vote", func(b *types.Block) { b.Header.Votes[2] = 3 }},
		{"version", func(b *types.Block) { b.Header.ProtocolVersion = 2 }},
		{"height", func(b *types.Block) { b.Header.Height = 2 }},
		{"producer", func(b *types.Block) { b.Body.ProducerData = []byte("x") }},
		{"receipt", func(b *types.Block) {
			r := append([]byte(nil), b.Body.Receipts[0]...)
			r[33] ^= 1 // resources_used C
			b.Body.Receipts = [][]byte{r}
		}},
		{"missing receipt", func(b *types.Block) {
			b.Body.Transactions = append(b.Body.Transactions, b.Body.Transactions[0])
		}},
		{"capacity", func(b *types.Block) { b.Header.ResourcesUsed[types.S] = 1 << 40 }},
	}
	for _, tc := range cases {
		h := good
		body := *tpl.Body
		b := &types.Block{Header: &h, Body: &body}
		tc.mutate(b)
		raw := remine(t, b)
		_, err := c.ProcessBlock(raw)
		if !errors.Is(err, chain.ErrInvalid) {
			t.Errorf("%s: expected invalid, got %v", tc.name, err)
		}
	}
	// The untouched template is valid.
	h := good
	if _, err := c.ProcessBlock(remine(t, &types.Block{Header: &h, Body: tpl.Body})); err != nil {
		t.Fatalf("good block: %v", err)
	}
	// Bad proof of work: find a nonce whose id exceeds the target.
	h2 := good
	h2.Timestamp++
	for {
		if id := h2.ID(); !(id[0] <= 0x3f) {
			break
		}
		h2.Nonce.Lo++
	}
	if _, err := c.ProcessBlock((&types.Block{Header: &h2, Body: tpl.Body}).Encode()); !errors.Is(err, chain.ErrInvalid) {
		t.Fatalf("bad pow accepted: %v", err)
	}
}

func TestReorg(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	a1, _ := mine(t, c, g.GenesisTimestamp+60)
	mine(t, c, g.GenesisTimestamp+120)
	// Build a competing branch from a1 that becomes heavier.
	other := newChain(t, nil)
	raw1, _ := c.Block(a1.ID)
	if _, err := other.ProcessBlock(raw1); err != nil {
		t.Fatal(err)
	}
	var side [][]byte
	for i := 0; i < 3; i++ {
		e, _ := mine(t, other, g.GenesisTimestamp+90+uint64(30*i))
		raw, _ := other.Block(e.ID)
		side = append(side, raw)
	}
	sub, unsub := c.Subscribe()
	defer unsub()
	for _, raw := range side {
		if _, err := c.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	if c.Tip().ID != other.Tip().ID {
		t.Fatalf("did not reorganize: tip height %d", c.Tip().Height)
	}
	var sawRemoval bool
	for len(sub) > 0 {
		ev := <-sub
		if len(ev.Removed) > 0 {
			sawRemoval = true
		}
	}
	if !sawRemoval {
		t.Fatal("no reorg event")
	}
	// Blocks delivered child-first are refused until their parent is known;
	// in order they connect.
	third := newChain(t, nil)
	if _, err := third.ProcessBlock(side[len(side)-1]); !errors.Is(err, chain.ErrUnknownParent) {
		t.Fatalf("child first: %v", err)
	}
	for _, raw := range append([][]byte{raw1}, side...) {
		if _, err := third.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	if third.Tip().ID != other.Tip().ID {
		t.Fatal("in-order blocks not connected")
	}
}

func TestStateRederivation(t *testing.T) {
	cfg := chain.DefaultConfig()
	cfg.RetainDepth = 2
	cfg.CheckpointInterval = 0
	c, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), cfg)
	g := c.Genesis()
	deploy := (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: counterModule()}).Encode()
	_, tpl := mine(t, c, g.GenesisTimestamp+60, deploy)
	var id hashing.Hash
	copy(id[:], tpl.Receipts[0].Result)
	call := (&types.Transaction{Kind: types.KindCall, Budget: budget, Target: id}).Encode()
	for i := 0; i < 6; i++ {
		mine(t, c, g.GenesisTimestamp+120+uint64(60*i), call)
	}
	e2, _ := c.AtHeight(2)
	if _, ok := e2.State(); ok {
		t.Fatal("expected pruned state")
	}
	st, err := c.StateAt(e2)
	if err != nil {
		t.Fatal(err)
	}
	if st.Root() != e2.Header.StateRoot {
		t.Fatal("re-derived root mismatch")
	}
}

// buildUnchecked assembles a block on parent with the given transactions,
// executing them without any capacity limit, and solves it.
func buildUnchecked(t *testing.T, c *chain.Chain, parent *chain.Entry, ts uint64, txs [][]byte) []byte {
	t.Helper()
	height, median, target, _ := c.NextContext(parent)
	if ts <= median {
		ts = median + 1
	}
	ctx := &vm.BlockContext{ChainID: c.ChainID(), Height: height, Timestamp: ts, ParentID: parent.ID}
	maxv := types.ResourceVector{^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)}
	b, err := c.NewBuilder(parent, ctx, maxv)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range txs {
		tx, err := types.DecodeTransaction(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Add(raw, tx); err != nil {
			t.Fatal(err)
		}
	}
	res := b.Finish()
	h := &types.Header{
		ProtocolVersion: 1, ChainID: c.ChainID(), Height: height, ParentID: parent.ID, Timestamp: ts,
		Target: target, TransactionsRoot: commit.TransactionsRoot(res.Transactions),
		ReceiptsRoot: commit.ReceiptsRoot(res.ReceiptBytes), StateRoot: res.State.Root(),
		ResourcesUsed: res.Used, ProducerHash: types.ProducerHash(nil),
	}
	blk := &types.Block{Header: h, Body: &types.Body{Transactions: res.Transactions, Receipts: res.ReceiptBytes}}
	return remine(t, blk)
}

func TestExecutionExceedsCapacity(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	deploy := (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: counterModule()}).Encode()
	_, tpl := mine(t, c, g.GenesisTimestamp+60, deploy)
	var id hashing.Hash
	copy(id[:], tpl.Receipts[0].Result)
	// Each counter call takes S = 2 (code) + 1 (get) + 2 (set) = 5; the
	// development S capacity is 1024, so 205 calls exceed it.
	call := (&types.Transaction{Kind: types.KindCall, Budget: budget, Target: id}).Encode()
	txs := make([][]byte, 205)
	for i := range txs {
		txs[i] = call
	}
	raw := buildUnchecked(t, c, c.Tip(), g.GenesisTimestamp+120, txs)
	_, err := c.ProcessBlock(raw)
	if !errors.Is(err, chain.ErrInvalid) {
		t.Fatalf("over-capacity block accepted: %v", err)
	}
	// 204 calls fit exactly: S = 1020.
	raw = buildUnchecked(t, c, c.Tip(), g.GenesisTimestamp+121, txs[:204])
	if _, err := c.ProcessBlock(raw); err != nil {
		t.Fatalf("204 calls: %v", err)
	}
}

func TestEnvelopeAndIdentityRejections(t *testing.T) {
	c := newChain(t, nil)
	g := c.Genesis()
	tpl, err := miner.Build(c, c.Tip(), miner.Options{Timestamp: g.GenesisTimestamp + 60}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A transaction whose D budget is below its data base is an invalid
	// envelope: the block is invalid, not the transaction failed.
	bad := (&types.Transaction{Kind: types.KindCall, Budget: types.ResourceVector{1000, 10, 151, 0}}).Encode()
	bad[2+16] = 150 // D budget, little-endian low byte: 151 -> 150
	h := *tpl.Header
	h.TransactionsRoot = commit.TransactionsRoot([][]byte{bad})
	rc := (&types.Receipt{}).Encode()
	h.ReceiptsRoot = commit.ReceiptsRoot([][]byte{rc})
	blk := &types.Block{Header: &h, Body: &types.Body{Transactions: [][]byte{bad}, Receipts: [][]byte{rc}}}
	if _, err := c.ProcessBlock(remine(t, blk)); !errors.Is(err, chain.ErrInvalid) {
		t.Fatalf("invalid envelope accepted: %v", err)
	}
	// Wrong chain id and wrong parent.
	for _, mutate := range []func(h *types.Header){
		func(h *types.Header) { h.ChainID[0] ^= 1 },
		func(h *types.Header) { h.ParentID = c.ChainID() },
	} {
		h := *tpl.Header
		mutate(&h)
		_, err := c.ProcessBlock(remine(t, &types.Block{Header: &h, Body: tpl.Body}))
		if err == nil {
			t.Fatal("accepted")
		}
	}
}

func TestSeedConformanceFault(t *testing.T) {
	// A block the seed list pins but this node rejects is reported as a
	// conformance fault of the node, not remembered as invalid (rules §27.1).
	c := newChain(t, nil)
	g := c.Genesis()
	tpl, err := miner.Build(c, c.Tip(), miner.Options{Timestamp: g.GenesisTimestamp + 60}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := *tpl.Header
	h.StateRoot[0] ^= 1 // this node will derive a different root
	raw := remine(t, &types.Block{Header: &h, Body: tpl.Body})
	id := types.HeaderIDFromBytes(raw[:types.HeaderSize])
	pinned := newChain(t, []hashing.Hash{id})
	_, err = pinned.ProcessBlock(raw)
	var sc *chain.SeedConformanceError
	if !errors.As(err, &sc) || errors.Is(err, chain.ErrInvalid) {
		t.Fatalf("expected a conformance fault, got %v", err)
	}
	if _, bad := pinned.InvalidReason(id); bad {
		t.Fatal("listed block remembered as invalid")
	}
}

// seedListOrNone pins the given list, or no seed at all.
func seedListOrNone(list []hashing.Hash) []hashing.Hash {
	if list == nil {
		return chain.NoSeed
	}
	return list
}
