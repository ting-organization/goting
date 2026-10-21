package chain_test

import (
	"path/filepath"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
)

func TestSnapshotRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := chain.OpenFileStore(filepath.Join(dir, "blocks"), types.DevelopmentGenesis().ChainID())
	if err != nil {
		t.Fatal(err)
	}
	c, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, store, chain.DefaultConfig())
	g := c.Genesis()
	deploy := (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: counterModule()}).Encode()
	_, tpl := mine(t, c, g.GenesisTimestamp+60, deploy)
	var id hashing.Hash
	copy(id[:], tpl.Receipts[0].Result)
	call := (&types.Transaction{Kind: types.KindCall, Budget: budget, Target: id}).Encode()
	for i := 0; i < 10; i++ {
		mine(t, c, g.GenesisTimestamp+120+uint64(60*i), call)
	}
	snapPath := filepath.Join(dir, "state.snap")
	info, ok, err := c.WriteSnapshot(snapPath, 3)
	if err != nil || !ok {
		t.Fatalf("snapshot: %v %v", ok, err)
	}
	if info.Height != 8 {
		t.Fatalf("snapshot height %d", info.Height)
	}
	tipID := c.Tip().ID
	tipRoot := c.Tip().Header.StateRoot
	store.Close()

	// Restart: load the snapshot, then the stored chain.
	store2, err := chain.OpenFileStore(filepath.Join(dir, "blocks"), types.DevelopmentGenesis().ChainID())
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	c2, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, store2, chain.DefaultConfig())
	sinfo, st, err := state.ReadSnapshot(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	n, err := c2.LoadFromSnapshot(sinfo, st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 || c2.Tip().ID != tipID {
		t.Fatalf("loaded %d, tip %s", n, c2.Tip().ID)
	}
	s, _ := c2.StateAt(c2.Tip())
	if s.Root() != tipRoot {
		t.Fatal("tip state differs after restart")
	}
	// A tampered snapshot is refused.
	bad := state.SnapshotInfo{Block: sinfo.Block, Height: sinfo.Height, Root: hashing.Hash{1}}
	c3, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, store2, chain.DefaultConfig())
	if _, err := c3.LoadFromSnapshot(bad, st); err == nil {
		t.Fatal("tampered snapshot accepted")
	}
}
