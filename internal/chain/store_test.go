package chain_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// Block files: frames survive a reopen, a torn or zero-filled tail is
// truncated, another chain's files are refused, and blocks stored before
// their parents load all the same.
func TestBlockFiles(t *testing.T) {
	g := types.DevelopmentGenesis()
	dir := filepath.Join(t.TempDir(), "blocks")
	src := newChain(t, nil)
	raws := mineEmpty(t, src, 6, g.GenesisTimestamp+60, 60, "files")

	s, err := chain.OpenFileStore(dir, g.ChainID())
	if err != nil {
		t.Fatal(err)
	}
	// Out of order: children first.
	order := []int{5, 3, 4, 0, 2, 1}
	for _, i := range order {
		if err := s.Put(types.HeaderIDFromBytes(raws[i][:types.HeaderSize]), raws[i]); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	seg := filepath.Join(dir, "00000000.dat")
	// A torn frame and zeros after the last whole one.
	f, _ := os.OpenFile(seg, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte{0x40, 0x01, 0, 0, 0, 0, 0, 0, 1, 2, 3})
	f.Write(make([]byte, 100))
	f.Close()
	before, _ := os.Stat(seg)

	s, err = chain.OpenFileStore(dir, g.ChainID())
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(seg)
	if s.Damaged != 1 || after.Size() != before.Size()-111 {
		t.Fatalf("damaged %d, size %d of %d", s.Damaged, after.Size(), before.Size())
	}
	c, err := chain.New(g, chain.NoSeed, s, chain.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.Load()
	if err != nil || n != 6 || c.Tip().ID != src.Tip().ID {
		t.Fatalf("loaded %d, tip %d: %v", n, c.Tip().Height, err)
	}
	// A checksum that no longer matches: the frame reads as an error.
	s.Close()
	b, _ := os.ReadFile(seg)
	b[len(b)-1] ^= 1
	os.WriteFile(seg, b, 0o644)
	s, _ = chain.OpenFileStore(dir, g.ChainID())
	if s.Damaged != 1 || s.Has(types.HeaderIDFromBytes(raws[1][:types.HeaderSize])) {
		t.Fatal("damaged last frame kept")
	}
	s.Close()

	// Another genesis: refused, not reset.
	other := *g
	other.GenesisTimestamp++
	if _, err := chain.OpenFileStore(dir, other.ChainID()); !errors.Is(err, chain.ErrWrongChain) {
		t.Fatalf("other chain: %v", err)
	}
}

func TestBlockFilesRollOver(t *testing.T) {
	g := types.DevelopmentGenesis()
	dir := filepath.Join(t.TempDir(), "blocks")
	s, err := chain.OpenFileStore(dir, g.ChainID())
	if err != nil {
		t.Fatal(err)
	}
	// Blocks of 48 MiB: two fit a segment; the third starts the next. A
	// block larger than a segment sits alone.
	sizes := []int{48 << 20, 48 << 20, 48 << 20, chain.SegmentSize + 1}
	var ids []hashing.Hash
	for i, n := range sizes {
		raw := make([]byte, n)
		raw[0] = byte(i + 1)
		id := types.HeaderIDFromBytes(raw[:types.HeaderSize])
		ids = append(ids, id)
		if err := s.Put(id, raw); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	for i, want := range []bool{true, true, true, false} {
		_, err := os.Stat(filepath.Join(dir, "0000000"+string(rune('0'+i))+".dat"))
		if (err == nil) != want {
			t.Fatalf("segment %d exists: %v", i, err == nil)
		}
	}
	s, err = chain.OpenFileStore(dir, g.ChainID())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, id := range ids {
		raw, err := s.Get(id)
		if err != nil || len(raw) != sizes[i] || raw[0] != byte(i+1) {
			t.Fatalf("block %d: %v", i, err)
		}
	}
}

// A forgotten block is stored again in a new frame, and a reopen that knows
// the old frame from a database takes the new one.
func TestBlockFilesForget(t *testing.T) {
	g := types.DevelopmentGenesis()
	dir := filepath.Join(t.TempDir(), "blocks")
	src := newChain(t, nil)
	raws := mineEmpty(t, src, 3, g.GenesisTimestamp+60, 60, "forget")
	ids := make([]hashing.Hash, len(raws))
	s, err := chain.OpenFileStore(dir, g.ChainID())
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range raws {
		ids[i] = types.HeaderIDFromBytes(raw[:types.HeaderSize])
		if err := s.Put(ids[i], raw); err != nil {
			t.Fatal(err)
		}
	}
	// A database lists the frames written so far.
	known, cursor := s.FramesAfter(chain.Cursor{}), s.End()
	s.Forget(ids[1])
	if _, err := s.Get(ids[1]); !errors.Is(err, chain.ErrNotFound) || s.Has(ids[1]) {
		t.Fatalf("forgotten block still read: %v", err)
	}
	if err := s.Put(ids[1], raws[1]); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Damage the old frame: only the new one reads.
	f, _ := os.OpenFile(filepath.Join(dir, "00000000.dat"), os.O_RDWR, 0)
	f.WriteAt([]byte{0xff, 0xff}, known[1].Location.Offset+10)
	f.Close()
	s, err = chain.OpenFileStoreFrom(dir, g.ChainID(), cursor, known)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, id := range ids {
		if raw, err := s.Get(id); err != nil || string(raw) != string(raws[i]) {
			t.Fatalf("block %d after reopening: %v", i, err)
		}
	}
	if after := s.FramesAfter(cursor); len(after) != 1 || after[0].ID != ids[1] {
		t.Fatalf("frames after the cursor: %v", after)
	}
}
