// Package daemon runs a node from a data directory laid out as the
// reference node's (wiki: node): config.json, genesis.bin, control.token,
// runtime.json, node.lock, peers.json, deferred.json and the block files,
// with the local control channel and the REST API.
package daemon

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ting-organization/goting/internal/types"
)

// Config is config.json, with the reference node's fields and defaults.
// Unknown fields are refused. undo_window and db_cache_mib are accepted for
// compatibility; this node keeps state in memory and has no undo data.
type Config struct {
	Version    uint32           `json:"version"`
	Name       string           `json:"name"`
	Control    netip.AddrPort   `json:"control"`
	P2P        netip.AddrPort   `json:"p2p"`
	API        netip.AddrPort   `json:"api"`
	APIEnabled bool             `json:"api_enabled"`
	APICORS    bool             `json:"api_cors"`
	Peers      []netip.AddrPort `json:"peers"`
	Discovery  bool             `json:"discovery"`
	MaxPeers   uint32           `json:"max_peers"`
	Votes      [4]uint8         `json:"votes"`
	// The built-in CPU miner: whether it runs from start, with how many
	// threads, and the least seconds between its own blocks.
	MinerEnabled bool    `json:"miner_enabled"`
	MinerThreads uint32  `json:"miner_threads"`
	MinerSpacing uint64  `json:"miner_spacing"`
	ProducerHex  *string `json:"producer_hex"`
	UndoWindow   uint64  `json:"undo_window"`
	DBCacheMiB   uint64  `json:"db_cache_mib"`
	// Pool bounds (wiki: node/mining).
	MempoolTransactions int    `json:"mempool_transactions"`
	MempoolMiB          uint64 `json:"mempool_mib"`
}

// ProducerMax bounds producer_hex.
const ProducerMax = 4096

// DefaultConfig is the reference node's default configuration.
func DefaultConfig() Config {
	return Config{Version: 1, Name: "Ting node",
		Control: netip.MustParseAddrPort("127.0.0.1:18440"), P2P: netip.MustParseAddrPort("127.0.0.1:18442"),
		API: netip.MustParseAddrPort("127.0.0.1:18441"), Peers: []netip.AddrPort{}, Discovery: true,
		MaxPeers: 8, MinerThreads: 1, UndoWindow: 43_200, DBCacheMiB: 256,
		MempoolTransactions: 1024, MempoolMiB: 16}
}

// Validate applies the reference's bounds.
func (c *Config) Validate() error {
	if c.Version != 1 || !c.Control.Addr().Unmap().IsLoopback() || len(c.Name) > 128 || len(c.Peers) > 32 ||
		c.MaxPeers < 1 || c.MaxPeers > 64 || c.Votes[0] > 2 || c.Votes[1] > 2 || c.Votes[2] > 2 || c.Votes[3] > 2 ||
		c.MinerThreads < 1 || c.MinerThreads > 64 || c.DBCacheMiB == 0 || c.MempoolTransactions == 0 || c.MempoolMiB == 0 {
		return errors.New("invalid configuration (control must be loopback, maximum 32 peers, max_peers 1..64, " +
			"votes 0..2, miner threads 1..64, db_cache_mib, mempool_transactions and mempool_mib at least 1)")
	}
	if c.ProducerHex != nil {
		if _, err := unhex(*c.ProducerHex, ProducerMax); err != nil {
			return errors.New("invalid configuration (producer_hex must be lowercase hex, at most 4096 bytes)")
		}
	}
	return nil
}

// Producer returns the producer data of the blocks this node builds.
func (c *Config) Producer() []byte {
	if c.ProducerHex == nil || *c.ProducerHex == "" {
		return nil
	}
	b, _ := unhex(*c.ProducerHex, ProducerMax)
	return b
}

func unhex(s string, max int) ([]byte, error) {
	if len(s)%2 != 0 || len(s)/2 > max {
		return nil, errors.New("invalid hex length")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, errors.New("hex must be lowercase")
		}
	}
	return hex.DecodeString(s)
}

// LoadConfig reads and validates config.json, with defaults for fields it
// leaves out.
func LoadConfig(dir string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("node is not initialized at %s. Run: ting --data-dir \"%s\" init", dir, dir)
	}
	if err != nil {
		return nil, err
	}
	c := DefaultConfig()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid configuration at %s: %v", dir, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save validates and writes config.json atomically.
func (c *Config) Save(dir string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Peers == nil {
		c.Peers = []netip.AddrPort{}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(dir, "config.json"), b)
}

// AtomicWrite writes a file through a synced temporary file and a rename.
func AtomicWrite(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Lock takes the data directory's single-writer lock, node.lock.
func Lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "node.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("data directory is locked: %v", err)
	}
	return f, nil
}

// Init creates a data directory for the network of g: config.json,
// genesis.bin and control.token, readable by its owner only.
func Init(dir string, c *Config, g *types.GenesisConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := g.Validate(); err != nil {
		return err
	}
	lock, err := Lock(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	for _, f := range []string{"config.json", "genesis.bin"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return errors.New("directory is already initialized")
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	tf, err := os.OpenFile(filepath.Join(dir, "control.token"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := tf.WriteString(hex.EncodeToString(secret)); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return err
	}
	tf.Close()
	if err := os.Chmod(filepath.Join(dir, "control.token"), 0o600); err != nil {
		return err
	}
	if err := AtomicWrite(filepath.Join(dir, "genesis.bin"), g.Encode()); err != nil {
		return err
	}
	return c.Save(dir)
}

// Genesis reads genesis.bin.
func Genesis(dir string) (*types.GenesisConfig, error) {
	b, err := os.ReadFile(filepath.Join(dir, "genesis.bin"))
	if err != nil {
		return nil, err
	}
	return types.DecodeGenesisConfig(b)
}

// Token reads control.token: 64 lowercase hex characters.
func Token(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "control.token"))
	if err != nil {
		return "", err
	}
	t := string(b)
	if len(t) != 64 {
		return "", errors.New("invalid control token")
	}
	if _, err := unhex(t, 32); err != nil {
		return "", errors.New("invalid control token")
	}
	return t, nil
}

// Running is runtime.json: where a running node listens.
type Running struct {
	PID     int             `json:"pid"`
	Control netip.AddrPort  `json:"control"`
	P2P     netip.AddrPort  `json:"p2p"`
	API     *netip.AddrPort `json:"api"`
	Started uint64          `json:"started"`
}

// LoadRunning reads runtime.json.
func LoadRunning(dir string) (*Running, error) {
	b, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("node is not running at %s. Run: ting --data-dir \"%s\" start", dir, dir)
	}
	if err != nil {
		return nil, err
	}
	var r Running
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Save writes runtime.json.
func (r *Running) Save(dir string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(dir, "runtime.json"), b)
}
