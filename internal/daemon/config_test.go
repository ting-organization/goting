package daemon

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ting-organization/goting/internal/types"
)

func TestConfigFiles(t *testing.T) {
	dir := t.TempDir()
	c := DefaultConfig()
	if err := Init(dir, &c, types.DevelopmentGenesis()); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir, &c, types.DevelopmentGenesis()); err == nil || !strings.Contains(err.Error(), "already initialized") {
		t.Fatalf("second init: %v", err)
	}
	st, _ := os.Stat(filepath.Join(dir, "control.token"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v", st.Mode())
	}
	if tok, err := Token(dir); err != nil || len(tok) != 64 {
		t.Fatalf("token: %v", err)
	}
	if g, err := Genesis(dir); err != nil || g.ChainID() != types.DevelopmentGenesis().ChainID() {
		t.Fatalf("genesis: %v", err)
	}
	got, err := LoadConfig(dir)
	if err != nil || got.MaxPeers != 8 || !got.Discovery || got.MempoolTransactions != 1024 {
		t.Fatalf("load: %+v %v", got, err)
	}
	// Unknown fields are refused; left-out fields take their defaults.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"version":1,"name":"x","control":"127.0.0.1:1","p2p":"127.0.0.1:2","api":"127.0.0.1:3","api_enabled":false,"peers":[],"votes":[0,0,0,0],"bogus":1}`), 0o644)
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("unknown field accepted")
	}
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"version":1,"name":"x","control":"127.0.0.1:1","p2p":"127.0.0.1:2","api":"127.0.0.1:3","api_enabled":false,"peers":[],"votes":[0,0,0,0]}`), 0o644)
	if got, err := LoadConfig(dir); err != nil || got.MaxPeers != 8 || got.MinerThreads != 1 {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	for _, bad := range []func(c *Config){
		func(c *Config) { c.Control = netip.MustParseAddrPort("0.0.0.0:1") },
		func(c *Config) { c.MaxPeers = 65 },
		func(c *Config) { c.Votes[2] = 3 },
		func(c *Config) { s := "ABCD"; c.ProducerHex = &s },
		func(c *Config) { c.MempoolTransactions = 0 },
	} {
		c := DefaultConfig()
		bad(&c)
		if c.Validate() == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	// One writer at a time.
	l, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err == nil {
		t.Fatal("second lock taken")
	}
	l.Close()
}
