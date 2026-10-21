package types

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/ting-organization/goting/internal/commit"
	"github.com/ting-organization/goting/internal/hashing"
)

func TestSHA256Vectors(t *testing.T) {
	// §29: raw SHA-256 test digests.
	e := sha256.Sum256(nil)
	if hex.EncodeToString(e[:]) != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal("empty sha")
	}
	a := sha256.Sum256([]byte("abc"))
	if hex.EncodeToString(a[:]) != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("abc sha")
	}
}

func TestGenesisCheckValues(t *testing.T) {
	cases := []struct {
		name    string
		g       *GenesisConfig
		chain   string
		genesis string
	}{
		{"development", DevelopmentGenesis(),
			"8578787146be1ef0aacc797754001290272915e89cd9e5784fe1368e4c2131bc",
			"ae99e1c9c68f6fe3e527ab9952a1a5ee3876a996ddb7b1149682b320b80ec0bb"},
		{"experimental", ExperimentalGenesis(),
			"15b2b480e8b3bb449479c1aef1dbf1e5a27c907d49fc876842d4b83332facd53",
			"2a25002d2674c05cc1e310c81fa3be635c66e6164923041418d47c2ffc84018f"},
	}
	for _, c := range cases {
		if got := len(c.g.Encode()); got != GenesisConfigSize {
			t.Fatalf("%s: encoded %d bytes", c.name, got)
		}
		id := c.g.ChainID()
		if id.String() != c.chain {
			t.Errorf("%s: ChainId %s", c.name, id)
		}
		if g := GenesisBlockID(id); g.String() != c.genesis {
			t.Errorf("%s: GenesisBlockId %s", c.name, g)
		}
	}
	dev := DevelopmentGenesis()
	if dev.GenesisTimestamp != 1700000000 || dev.TargetInterval != 60 || dev.HalfLife != 172800 ||
		dev.VoteEpochLength != 20160 {
		t.Fatalf("dev fields: %+v", dev)
	}
	if dev.InitialLimits != (ResourceVector{100000000, 1024, 1048576, 524288}) ||
		dev.MinimumLimits != (ResourceVector{100000, 16, 1024, 4096}) {
		t.Fatalf("dev limits: %v %v", dev.InitialLimits, dev.MinimumLimits)
	}
	p := ProductionGenesis()
	if id := p.ChainID(); id.String() != "15d6549cfacd816f835c632b4c67308d1c63e642f4cbf4049368430670705107" ||
		GenesisBlockID(id).String() != "24b76a52f3825b921373b6c6a41ced2a74de7dbdb34015669c10594996887459" {
		t.Fatalf("production ids %s", id)
	}
	if p.GenesisTimestamp != 1792540800 || p.HalfLife != 43200 || p.InitialLimits != (ResourceVector{450000000, 2048, 262144, 131072}) ||
		p.MinimumLimits != (ResourceVector{40000000, 64, 98304, 98304}) || p.VoteEpochLength != 20160 {
		t.Fatalf("production fields: %+v", p)
	}
}

func TestPrimitiveCommitments(t *testing.T) {
	check := func(name string, got hashing.Hash, want string) {
		t.Helper()
		if got.String() != want {
			t.Errorf("%s: got %s want %s", name, got, want)
		}
	}
	check("empty state root", hashing.EmptyStateRoot, "1afb449a10b29cdbe8a0b60a98e26b413a52f8bc35af49a1fc2d4fff669f5889")
	check("empty producer", ProducerHash(nil), "51976778d26056c36803109ea49beaec0c618c49fc39c63fb1f10e51bf56592c")
	check("empty transactions root", commit.TransactionsRoot(nil), "8c2beadecc1824b6e7f88cd39fff6d8ffdc9e8a41e93254d7f7109ba7d218f49")
	check("empty receipts root", commit.ReceiptsRoot(nil), "de3aa3b4c7df30035eb3ddc2d357d2a54521c27a4742438a9ab4561cb516ee6b")
}

func TestArithmeticChecks(t *testing.T) {
	call := &Transaction{Kind: KindCall}
	if n := len(call.Encode()); n != 70 || DataBase(n) != 151 {
		t.Fatalf("call envelope %d", n)
	}
	dep := &Transaction{Kind: KindDeploy}
	if n := len(dep.Encode()); n != 42 || DataBase(n) != 123 {
		t.Fatalf("deploy envelope %d", n)
	}
	r := &Receipt{}
	if n := len(r.Encode()); n != 73 {
		t.Fatalf("receipt %d", n)
	}
	b := &Block{Header: &Header{}, Body: &Body{}}
	if n := len(b.Encode()); n != 302 || BlockOverhead != 302 {
		t.Fatalf("empty block %d", n)
	}
	if hashing.CodeHashPrice(0) != 1000 {
		t.Fatal("codehash price")
	}
	if hashing.RecordKeyPrice != 1900 || hashing.ContractIDPrice != 1900 {
		t.Fatal("key price")
	}
	if hashing.LeafPrice(2) != 1000 || hashing.LeafPrice(3) != 1900 {
		t.Fatalf("leaf price %d %d", hashing.LeafPrice(2), hashing.LeafPrice(3))
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	tx := &Transaction{Kind: KindCall, Budget: ResourceVector{1, 2, 200, 4}, Input: []byte("hi")}
	tx.Target[0] = 9
	enc := tx.Encode()
	got, err := DecodeTransaction(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != tx.Target || string(got.Input) != "hi" || got.Budget != tx.Budget {
		t.Fatal("round trip")
	}
	// D below floor.
	tx.Budget[D] = DataBase(len(enc)) - 1
	if _, err := DecodeTransaction(tx.Encode()); err == nil {
		t.Fatal("expected floor violation")
	}
	tx.Budget[D] = 1 << 32
	if _, err := DecodeTransaction(tx.Encode()); err == nil {
		t.Fatal("expected ceiling violation")
	}
	// Trailing byte.
	tx.Budget[D] = 1000
	if _, err := DecodeTransaction(append(tx.Encode(), 0)); err == nil {
		t.Fatal("expected trailing rejection")
	}
	// Bad kind.
	bad := tx.Encode()
	bad[1] = 2
	if _, err := DecodeTransaction(bad); err == nil {
		t.Fatal("expected kind rejection")
	}
}

func TestBodyFraming(t *testing.T) {
	tx := (&Transaction{Kind: KindDeploy, Budget: ResourceVector{0, 0, 200, 0}}).Encode()
	rc := (&Receipt{}).Encode()
	body := &Body{ProducerData: []byte("p"), Transactions: [][]byte{tx}, Receipts: [][]byte{rc}}
	b := &Block{Header: &Header{ProtocolVersion: 1}, Body: body}
	enc := b.Encode()
	got, err := DecodeBlock(enc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body.ProducerData) != "p" || len(got.Body.Transactions) != 1 {
		t.Fatal("decode")
	}
	if _, err := DecodeBlock(append(enc, 0)); err == nil {
		t.Fatal("expected trailing rejection")
	}
}
