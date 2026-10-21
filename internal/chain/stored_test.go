package chain_test

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/types"
)

// branch mines n blocks on c's tip, each with transactions that write,
// overwrite and delete records of a key-value program deployed in the
// first; tag makes branches differ.
func branch(t *testing.T, c *chain.Chain, n int, start uint64, tag byte, r *rand.Rand) [][]byte {
	t.Helper()
	var out [][]byte
	ts := start
	small := types.ResourceVector{400_000, 4, 400, 200}
	var kv hashing.Hash
	for i := 0; i < n; i++ {
		ts += 60
		var txs [][]byte
		if i == 0 {
			txs = append(txs, (&types.Transaction{Kind: types.KindDeploy, Input: []byte{tag},
				Budget: types.ResourceVector{10_000_000, 16, 20_000, 20_000}, Code: samples.KeyValue()}).Encode())
		} else {
			for j := 0; j < 1+r.IntN(12); j++ {
				in := make([]byte, 40)
				binary.LittleEndian.PutUint64(in, uint64(r.IntN(40))) // few keys: overwrites
				in[8] = tag
				binary.LittleEndian.PutUint64(in[32:], r.Uint64())
				if r.IntN(5) == 0 {
					in = []byte{1} // deletes key 0
				}
				txs = append(txs, (&types.Transaction{Kind: types.KindCall, Budget: small, Target: kv, Input: in}).Encode())
			}
		}
		e, tpl, err := miner.MineOne(context.Background(), c, miner.Options{Timestamp: ts, Producer: []byte{tag}}, txs)
		if err != nil {
			t.Fatalf("mine %d: %v", i, err)
		}
		if i == 0 {
			copy(kv[:], tpl.Receipts[0].Result)
		}
		raw, _ := c.Block(e.ID)
		out = append(out, raw)
	}
	return out
}

func openStored(t *testing.T, dir string, depth uint64) (*chain.Chain, *db.DB, *chain.FileStore) {
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
	cfg.State, cfg.RetainDepth = d, depth
	c, err := chain.New(g, chain.NoSeed, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Load(); err != nil {
		t.Fatal(err)
	}
	return c, d, store
}

// With a database, every block's state equals the in-memory chain's while
// the base moves up every few blocks; a restart resumes from the base.
func TestStoredStateMatchesMemory(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	src := newChain(t, nil)
	g := src.Genesis()
	raws := branch(t, src, 40, g.GenesisTimestamp, 1, r)

	dir := t.TempDir()
	c, d, store := openStored(t, dir, 3)
	for i, raw := range raws {
		e, err := c.ProcessBlock(raw)
		if err != nil {
			t.Fatalf("block %d: %v", i+1, err)
		}
		st, err := c.StateAt(c.Tip())
		if err != nil || st.Root() != e.Header.StateRoot {
			t.Fatalf("block %d: state %v", i+1, err)
		}
	}
	base := c.Base()
	if base == nil || base.Height < 30 || base.Height > 37 {
		t.Fatalf("base at %v", base)
	}
	stored, ok, err := d.Base()
	if err != nil || !ok || stored.Block != base.ID {
		t.Fatal("database base differs from the chain's")
	}
	// Every record of the tip reads the same as in memory.
	want, _ := src.StateAt(src.Tip())
	got, _ := c.StateAt(c.Tip())
	n := 0
	want.Walk(func(k hashing.Hash, v []byte) bool {
		gv, ok := got.Get(k)
		if !ok || string(gv) != string(v) {
			t.Fatalf("record %x differs", k[:4])
		}
		n++
		return true
	})
	if n != got.Len() {
		t.Fatalf("%d records, stored state counts %d", n, got.Len())
	}
	tip := c.Tip().ID
	d.Close()
	store.Close()

	// Restart: the base comes from the database, the blocks above it are
	// executed again.
	c2, d2, store2 := openStored(t, dir, 3)
	defer d2.Close()
	defer store2.Close()
	// Only the frames the database does not list were read.
	if scanned := len(store2.FramesAfter(chain.Cursor{})); scanned == 0 || scanned > 10 {
		t.Fatalf("restart scanned %d frames", scanned)
	}
	if c2.Tip().ID != tip || c2.Base().ID != base.ID {
		t.Fatalf("restart: tip %d, base %d", c2.Tip().Height, c2.Base().Height)
	}
	st, err := c2.StateAt(c2.Tip())
	if err != nil || st.Root() != c2.Tip().Header.StateRoot {
		t.Fatalf("state after restart: %v", err)
	}
	// And it goes on.
	more := branch(t, src, 10, src.Tip().Timestamp, 1, r)
	for _, raw := range more {
		if _, err := c2.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	if c2.Tip().ID != src.Tip().ID || c2.Base().Height <= base.Height {
		t.Fatal("did not follow after restart")
	}
	// Settling moves the base to the tip: nothing is executed again.
	if err := c2.Settle(); err != nil || c2.Base() != c2.Tip() {
		t.Fatalf("settle: %v", err)
	}
	st, err = c2.StateAt(c2.Tip())
	if err != nil || st.Root() != c2.Tip().Header.StateRoot {
		t.Fatalf("state after settling: %v", err)
	}
}

// A heavier branch that forks below the base: the base is rewound with the
// undo data, the branch executed, and back again when the first branch
// grows heavier still.
func TestStoredDeepReorganization(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	srcA := newChain(t, nil)
	g := srcA.Genesis()
	common := branch(t, srcA, 5, g.GenesisTimestamp, 1, r)
	srcB := newChain(t, nil)
	for _, raw := range common {
		if _, err := srcB.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	forkTime := srcA.Tip().Timestamp
	a := branch(t, srcA, 20, forkTime, 2, r)
	b := branch(t, srcB, 24, forkTime+1, 3, r)

	c, d, store := openStored(t, t.TempDir(), 2)
	defer d.Close()
	defer store.Close()
	for _, raw := range append(append([][]byte{}, common...), a...) {
		if _, err := c.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	if c.Base().Height <= 5 {
		t.Fatalf("base at %d did not pass the fork", c.Base().Height)
	}
	for _, raw := range b {
		if _, err := c.ProcessBlock(raw); err != nil && !errors.Is(err, chain.ErrKnown) {
			t.Fatal(err)
		}
	}
	if c.Tip().ID != srcB.Tip().ID {
		t.Fatalf("tip %d did not move to the heavier branch", c.Tip().Height)
	}
	st, err := c.StateAt(c.Tip())
	if err != nil || st.Root() != c.Tip().Header.StateRoot {
		t.Fatalf("state after the reorganization: %v", err)
	}
	// Branch A grows heavier again: back across the fork.
	moreA := branch(t, srcA, 8, srcA.Tip().Timestamp, 2, r)
	for _, raw := range moreA {
		if _, err := c.ProcessBlock(raw); err != nil && !errors.Is(err, chain.ErrKnown) {
			t.Fatal(err)
		}
	}
	if c.Tip().ID != srcA.Tip().ID {
		t.Fatalf("tip %d did not move back", c.Tip().Height)
	}
	want, _ := srcA.StateAt(srcA.Tip())
	got, err := c.StateAt(c.Tip())
	if err != nil || got.Root() != want.Root() {
		t.Fatalf("state after moving back: %v", err)
	}
}

// A database that no longer reads stops the chain; no block is judged
// invalid for it.
func TestStoredStateFailureStops(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 9))
	src := newChain(t, nil)
	raws := branch(t, src, 12, src.Genesis().GenesisTimestamp, 1, r)
	dir := t.TempDir()
	c, d, store := openStored(t, dir, 2)
	defer store.Close()
	for _, raw := range raws[:8] {
		if _, err := c.ProcessBlock(raw); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	// Damage every stored branch.
	bdb, err := bolt.Open(filepath.Join(dir, "chain.db"), 0o644, nil)
	if err != nil {
		t.Fatal(err)
	}
	bdb.Update(func(tx *bolt.Tx) error {
		bk := tx.Bucket([]byte("branches"))
		var keys [][]byte
		bk.ForEach(func(k, v []byte) error { keys = append(keys, append([]byte(nil), k...)); return nil })
		for _, k := range keys {
			bk.Put(k, make([]byte, 134))
		}
		return nil
	})
	bdb.Close()
	d2, err := db.Open(filepath.Join(dir, "chain.db"), src.ChainID())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	cfg := chain.DefaultConfig()
	var fatal error
	cfg.State, cfg.RetainDepth, cfg.Fatal = d2, 2, func(err error) { fatal = err }
	c2, _ := chain.New(src.Genesis(), chain.NoSeed, store, cfg)
	_, err = c2.Load()
	if err == nil && fatal == nil {
		// Loading may defer the read; the next blocks must fail it.
		for _, raw := range raws[8:] {
			_, err = c2.ProcessBlock(raw)
		}
	}
	if err == nil && fatal == nil {
		t.Fatal("a damaged database went unnoticed")
	}
	if errors.Is(err, chain.ErrInvalid) {
		t.Fatalf("storage failure judged a block invalid: %v", err)
	}
	_ = c
}
