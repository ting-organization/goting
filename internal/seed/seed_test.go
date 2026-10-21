package seed

import (
	"bytes"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

func TestBundleRoundTrip(t *testing.T) {
	blocks := [][]byte{bytes.Repeat([]byte{1}, 300), bytes.Repeat([]byte{2}, 310)}
	b := EncodeBundle(blocks)
	got, err := ParseBundle(b)
	if err != nil || len(got) != 2 || !bytes.Equal(got[1], blocks[1]) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParseBundle(b[:len(b)-1]); err == nil {
		t.Fatal("truncated bundle accepted")
	}
	if _, err := ParseBundle(append(b, 0, 0)); err == nil {
		t.Fatal("trailing bytes accepted")
	}
}

func TestParseList(t *testing.T) {
	a := hashing.Hash{1}
	b := hashing.Hash{2}
	want := []hashing.Hash{a, b}
	inputs := []string{
		a.String() + "\n" + b.String() + "\n",
		`["` + a.String() + `", "0x` + b.String() + `"]`,
		`{"blocks": [{"id": "` + a.String() + `"}, {"block_id": "` + b.String() + `"}]}`,
	}
	for _, in := range inputs {
		got, err := ParseList([]byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%q: %v", in, got)
		}
	}
	if _, err := ParseList([]byte("zz")); err == nil {
		t.Fatal("bad hex accepted")
	}
}

func TestBuiltinSeeds(t *testing.T) {
	for _, g := range []*types.GenesisConfig{types.DevelopmentGenesis(), types.ProductionGenesis()} {
		blocks, err := Builtin(g)
		if err != nil || len(blocks) == 0 {
			t.Fatalf("%x: %d blocks: %v", g.ChainID(), len(blocks), err)
		}
	}
	if b, err := Builtin(types.ExperimentalGenesis()); b != nil || err != nil {
		t.Fatal("experimental network has no seed")
	}
}
