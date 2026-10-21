package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ting-organization/goting/internal/control"
	"github.com/ting-organization/goting/internal/daemon"
	"github.com/ting-organization/goting/internal/p2p"
	"github.com/ting-organization/goting/internal/types"
)

// The commands below follow the reference node's CLI (wiki: start): a data
// directory, default ~/.ting, chosen with the global --data-dir; results
// printed as JSON; errors as "ting: <error>" with exit status 1.
var daemonCommands = map[string]bool{"init": true, "run": true, "start": true, "stop": true, "status": true,
	"peers": true, "add": true, "remove": true, "mine": true, "miner": true, "votes": true, "template": true,
	"solution": true, "submit": true, "api": true, "invalidate": true, "reconsider": true}

// cliArgs separates flags from positional arguments anywhere on the line.
// valued names the flags that take a value.
type cliArgs struct {
	flags map[string][]string
	pos   []string
}

func parseArgs(args []string, valued ...string) (*cliArgs, error) {
	a := &cliArgs{flags: map[string][]string{}}
	takes := map[string]bool{}
	for _, v := range valued {
		takes[v] = true
	}
	for i := 0; i < len(args); i++ {
		s := args[i]
		if !strings.HasPrefix(s, "-") || s == "-" {
			a.pos = append(a.pos, s)
			continue
		}
		name := strings.TrimLeft(s, "-")
		val, hasVal := "", false
		if k := strings.IndexByte(name, '='); k >= 0 {
			name, val, hasVal = name[:k], name[k+1:], true
		}
		if takes[name] && !hasVal {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag --%s needs a value", name)
			}
			i++
			val = args[i]
		}
		a.flags[name] = append(a.flags[name], val)
	}
	return a, nil
}

func (a *cliArgs) str(name, def string) string {
	if v, ok := a.flags[name]; ok {
		return v[len(v)-1]
	}
	return def
}

func (a *cliArgs) has(name string) bool { _, ok := a.flags[name]; return ok }

func (a *cliArgs) u64(name string) (*uint64, error) {
	v, ok := a.flags[name]
	if !ok {
		return nil, nil
	}
	n, err := strconv.ParseUint(v[len(v)-1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("--%s: expected an integer", name)
	}
	return &n, nil
}

func defaultDataDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("cannot locate your home directory: set HOME to an absolute path or use --data-dir DIRECTORY")
	}
	return filepath.Join(home, ".ting"), nil
}

func rpc(dir, method string, params any) (json.RawMessage, error) {
	r, err := daemon.LoadRunning(dir)
	if err != nil {
		return nil, err
	}
	token, err := daemon.Token(dir)
	if err != nil {
		return nil, err
	}
	return control.Call(context.Background(), r.Control, token, method, params)
}

// edit changes the running node's settings, or config.json while it is not
// running.
func edit(dir, method string, params map[string]any) (any, error) {
	res, liveErr := rpc(dir, method, params)
	if liveErr == nil {
		return res, nil
	}
	lock, err := daemon.Lock(dir)
	if err != nil {
		return nil, liveErr
	}
	defer lock.Close()
	c, err := daemon.LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	switch {
	case method == "api":
		c.APIEnabled = params["enabled"].(bool)
		c.APICORS, _ = params["cors"].(bool)
	case method == "miner" && params["action"] == "producer":
		p, _ := params["producer"].(string)
		if p == "" {
			c.ProducerHex = nil
		} else {
			c.ProducerHex = &p
		}
	case method == "miner" && params["action"] == "votes":
		v := params["votes"].([]uint8)
		c.Votes = [4]uint8{v[0], v[1], v[2], v[3]}
	case method == "miner":
		c.MinerEnabled = params["action"] == "start"
		if t, ok := params["threads"].(*uint64); ok && t != nil {
			c.MinerThreads = uint32(*t)
		}
		if s, ok := params["spacing"].(*uint64); ok && s != nil {
			c.MinerSpacing = *s
		}
	case method == "add_peer" || method == "remove_peer":
		a, err := netip.ParseAddrPort(params["address"].(string))
		if err != nil {
			return nil, errors.New("invalid address")
		}
		kept := []netip.AddrPort{}
		found := false
		for _, p := range c.Peers {
			if p == a {
				found = true
				if method == "remove_peer" {
					continue
				}
			}
			kept = append(kept, p)
		}
		if method == "add_peer" && !found {
			kept = append(kept, a)
		}
		c.Peers = kept
	default:
		return nil, errors.New("unknown configuration operation")
	}
	if err := c.Save(dir); err != nil {
		return nil, err
	}
	return map[string]any{"saved": true, "running": false, "api_enabled": c.APIEnabled, "peers": c.Peers,
		"miner_enabled": c.MinerEnabled}, nil
}

// runDaemonCommand runs one of the reference-style commands.
func runDaemonCommand(dir, cmd string, args []string) (any, error) {
	if dir == "" {
		d, err := defaultDataDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	if cmd != "init" {
		if _, err := daemon.LoadConfig(dir); err != nil {
			return nil, err
		}
	}
	a, err := parseArgs(args, "name", "control", "p2p", "api-bind", "genesis", "count", "attempts", "threads",
		"spacing", "tx", "not-before")
	if err != nil {
		return nil, err
	}
	need := func(n int, usage string) error {
		if len(a.pos) != n {
			return errors.New("usage: ting " + usage)
		}
		return nil
	}
	switch cmd {
	case "init":
		return cmdInit(dir, a)
	case "run":
		log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		return nil, daemon.Run(dir, log)
	case "start":
		return startNode(dir)
	case "stop":
		res, err := rpc(dir, "stop", nil)
		if err != nil {
			return nil, err
		}
		for i := 0; i < 150; i++ {
			if l, err := daemon.Lock(dir); err == nil {
				l.Close()
				return res, nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil, errors.New("shutdown is still in progress")
	case "status", "peers", "votes":
		return rpc(dir, cmd, nil)
	case "add", "remove":
		if len(a.pos) != 2 || a.pos[0] != "node" {
			return nil, fmt.Errorf("usage: ting %s node IP:PORT", cmd)
		}
		if _, err := netip.ParseAddrPort(a.pos[1]); err != nil {
			return nil, errors.New("invalid address")
		}
		method := "add_peer"
		if cmd == "remove" {
			method = "remove_peer"
		}
		return edit(dir, method, map[string]any{"address": a.pos[1]})
	case "api":
		if len(a.pos) != 1 || (a.pos[0] != "enable" && a.pos[0] != "disable") {
			return nil, errors.New("usage: ting api enable [--cors] | disable")
		}
		enabled := a.pos[0] == "enable"
		return edit(dir, "api", map[string]any{"enabled": enabled, "cors": enabled && a.has("cors")})
	case "mine":
		count, err := a.u64("count")
		if err != nil {
			return nil, err
		}
		attempts, err := a.u64("attempts")
		if err != nil {
			return nil, err
		}
		p := map[string]any{"count": uint64(1), "attempts": uint64(1_000_000)}
		if count != nil {
			p["count"] = *count
		}
		if attempts != nil {
			p["attempts"] = *attempts
		}
		return rpc(dir, "mine", p)
	case "miner":
		return minerCommand(dir, a)
	case "template":
		p := map[string]any{}
		if ids, ok := a.flags["tx"]; ok {
			p["transactions"] = ids
		}
		return rpc(dir, "template", p)
	case "solution":
		if err := need(2, "solution TEMPLATE NONCE"); err != nil {
			return nil, err
		}
		return rpc(dir, "solution", map[string]any{"template": a.pos[0], "nonce": a.pos[1]})
	case "submit":
		if err := need(1, "submit FILE [--not-before T]"); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(a.pos[0])
		if err != nil {
			return nil, err
		}
		nb, err := a.u64("not-before")
		if err != nil {
			return nil, err
		}
		return rpc(dir, "submit", map[string]any{"hex": hex.EncodeToString(b), "not_before": nb})
	case "invalidate", "reconsider":
		if err := need(1, cmd+" BLOCK"); err != nil {
			return nil, err
		}
		return rpc(dir, cmd, map[string]any{"id": a.pos[0]})
	}
	return nil, fmt.Errorf("unknown command %q", cmd)
}

func cmdInit(dir string, a *cliArgs) (any, error) {
	c := daemon.DefaultConfig()
	c.Name = a.str("name", c.Name)
	var err error
	if c.Control, err = netip.ParseAddrPort(a.str("control", c.Control.String())); err != nil {
		return nil, errors.New("--control: expected IP:PORT")
	}
	if c.API, err = netip.ParseAddrPort(a.str("api-bind", c.API.String())); err != nil {
		return nil, errors.New("--api-bind: expected IP:PORT")
	}
	c.APIEnabled = a.has("api")
	var g *types.GenesisConfig
	public := false
	switch {
	case a.has("genesis") && a.has("dev"):
		return nil, errors.New("--dev and --genesis exclude each other")
	case a.has("genesis"):
		b, err := os.ReadFile(a.str("genesis", ""))
		if err != nil {
			return nil, err
		}
		if g, err = types.DecodeGenesisConfig(b); err != nil {
			return nil, err
		}
	case a.has("dev"):
		g = types.DevelopmentGenesis()
	default:
		g, public = types.ProductionGenesis(), true
	}
	// The peer listener: public on the production network, loopback
	// otherwise.
	p2pDefault := "127.0.0.1:18442"
	if public {
		p2pDefault = "0.0.0.0:18442"
	}
	if c.P2P, err = netip.ParseAddrPort(a.str("p2p", p2pDefault)); err != nil {
		return nil, errors.New("--p2p: expected IP:PORT")
	}
	if err := daemon.Init(dir, &c, g); err != nil {
		return nil, err
	}
	height, err := daemon.InitSeed(dir, g)
	if err != nil {
		return nil, err
	}
	if height > 0 {
		fmt.Fprintf(os.Stderr, "imported the seed: %d blocks\n", height)
	}
	return map[string]any{"initialized": dir, "chain_id": g.ChainID(), "api_enabled": c.APIEnabled}, nil
}

func minerCommand(dir string, a *cliArgs) (any, error) {
	if len(a.pos) == 0 {
		return nil, errors.New("usage: ting miner start [--threads N] [--spacing S] | stop | status | producer [HEX] | votes C S D G")
	}
	switch a.pos[0] {
	case "start":
		threads, err := a.u64("threads")
		if err != nil {
			return nil, err
		}
		spacing, err := a.u64("spacing")
		if err != nil {
			return nil, err
		}
		return edit(dir, "miner", map[string]any{"action": "start", "threads": threads, "spacing": spacing})
	case "stop":
		return edit(dir, "miner", map[string]any{"action": "stop"})
	case "status":
		return rpc(dir, "miner", map[string]any{"action": "status"})
	case "producer":
		p := ""
		if len(a.pos) > 1 {
			p = a.pos[1]
		}
		return edit(dir, "miner", map[string]any{"action": "producer", "producer": p})
	case "votes":
		if len(a.pos) != 5 {
			return nil, errors.New("usage: ting miner votes COMPUTE STATE DATA GROWTH (each hold, increase or decrease)")
		}
		votes := make([]uint8, 4)
		for i, v := range a.pos[1:] {
			switch v {
			case "hold":
				votes[i] = 0
			case "increase":
				votes[i] = 1
			case "decrease":
				votes[i] = 2
			default:
				return nil, fmt.Errorf("invalid vote %q: hold, increase or decrease", v)
			}
		}
		return edit(dir, "miner", map[string]any{"action": "votes", "votes": votes})
	}
	return nil, fmt.Errorf("unknown miner command %q", a.pos[0])
}

// startNode launches the node in the background, its output going to
// node.log, and waits up to 15 seconds for it to answer.
func startNode(dir string) (any, error) {
	if res, err := rpc(dir, "status", nil); err == nil {
		return res, nil
	}
	for i := 0; ; i++ {
		l, err := daemon.Lock(dir)
		if err == nil {
			l.Close()
			break
		}
		if i >= 100 {
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(abs, "node.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "--data-dir", abs, "run")
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for i := 0; i < 150; i++ {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("node exited (%v); see %s", err, filepath.Join(abs, "node.log"))
		default:
		}
		if res, err := rpc(dir, "status", nil); err == nil {
			cmd.Process.Release()
			return res, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("node not ready yet; inspect %s", filepath.Join(abs, "node.log"))
}

// probeHello says hello to a node over the peer protocol as a peer on this
// data directory's network does, and shows its answer.
func probeHello(dir, addr string) (any, error) {
	g, err := daemon.Genesis(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := p2p.Dial(ctx, addr, newHello(g, false))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Peer, nil
}
