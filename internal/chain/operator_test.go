package chain_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/types"
)

func TestOperatorMarks(t *testing.T) {
	g := types.DevelopmentGenesis()
	dir := t.TempDir()
	cfg := chain.DefaultConfig()
	cfg.MarksPath = filepath.Join(dir, "marks.json")
	store, err := chain.OpenFileStore(filepath.Join(dir, "blocks"), g.ChainID())
	if err != nil {
		t.Fatal(err)
	}
	src := newChain(t, nil)
	raws := mineEmpty(t, src, 5, g.GenesisTimestamp+60, 60, "marks")
	id := func(i int) [32]byte { return types.HeaderIDFromBytes(raws[i][:types.HeaderSize]) }

	c, err := chain.New(g, chain.NoSeed, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.MarkInvalid(c.Tip().ID); !errors.Is(err, chain.ErrGenesisMark) {
		t.Fatalf("genesis: %v", err)
	}
	// A block not yet seen is refused when it comes, and blocks above it.
	if known, err := c.MarkInvalid(id(4)); err != nil || known {
		t.Fatalf("unseen mark: %v %v", known, err)
	}
	for i := 0; i < 4; i++ {
		if _, err := c.ProcessBlock(raws[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ProcessBlock(raws[4]); !errors.Is(err, chain.ErrMarked) || errors.Is(err, chain.ErrInvalid) {
		t.Fatalf("marked block: %v", err)
	}
	// Marking a canonical block leaves it: the tip goes back below it.
	if known, err := c.MarkInvalid(id(2)); err != nil || !known {
		t.Fatalf("mark: %v %v", known, err)
	}
	if c.Tip().Height != 2 {
		t.Fatalf("tip %d after marking height 3", c.Tip().Height)
	}
	if _, err := c.ProcessBlock(raws[3]); !errors.Is(err, chain.ErrMarked) {
		t.Fatalf("block above a mark: %v", err)
	}
	// The marks survive a restart.
	store.Close()
	store, _ = chain.OpenFileStore(filepath.Join(dir, "blocks"), g.ChainID())
	defer store.Close()
	c, err = chain.New(g, chain.NoSeed, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Load(); err != nil {
		t.Fatal(err)
	}
	if c.Tip().Height != 2 || len(c.Marks()) != 2 {
		t.Fatalf("after restart: tip %d, %d marks", c.Tip().Height, len(c.Marks()))
	}
	// Reconsidering clears the mark and connects the stored branch again.
	if n, err := c.Reconsider(id(2)); err != nil || n != 0 {
		t.Fatalf("reconsider: %d %v", n, err)
	}
	if c.Tip().Height != 4 {
		t.Fatalf("tip %d after reconsider", c.Tip().Height)
	}
	if _, err := c.Reconsider(id(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProcessBlock(raws[4]); err != nil || c.Tip().Height != 5 {
		t.Fatalf("after clearing the last mark: %v, tip %d", err, c.Tip().Height)
	}
	if _, err := c.Reconsider([32]byte{9}); !errors.Is(err, chain.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}
