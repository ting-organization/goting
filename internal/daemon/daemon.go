package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/control"
	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/node"
	"github.com/ting-organization/goting/internal/seed"
	"github.com/ting-organization/goting/internal/types"
)

// Agent is this node's agent string in the peer protocol.
const Agent = "goting/" + node.Version

// InitSeed imports the network's built-in seed into a new data directory,
// as the reference node does at init. It returns the seed's height.
func InitSeed(dir string, g *types.GenesisConfig) (uint64, error) {
	blocks, err := seed.Builtin(g)
	if err != nil || len(blocks) == 0 {
		return 0, err
	}
	database, err := db.Open(filepath.Join(dir, "chain.db"), g.ChainID())
	if err != nil {
		return 0, err
	}
	defer database.Close()
	store, err := chain.OpenStoredBlocks(filepath.Join(dir, "blocks"), g.ChainID(), database)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	ccfg := chain.DefaultConfig()
	ccfg.State = database
	c, err := chain.New(g, nil, store, ccfg)
	if err != nil {
		return 0, err
	}
	rep, err := seed.Replay(c, blocks, nil)
	if err != nil {
		return 0, err
	}
	return rep.Tip.Height, c.Settle()
}

// Daemon is a running node.
type Daemon struct {
	dir   string
	log   *slog.Logger
	token string

	mu      sync.Mutex
	cfg     *Config
	running Running

	chain *chain.Chain
	node  *node.Node
	peers *node.Peers

	api     *http.Server
	apiCORS bool
	stop    context.CancelFunc
}

// Run runs the node of a data directory until it is told to stop, through
// the control channel or a signal.
func Run(dir string, log *slog.Logger) error {
	lock, err := Lock(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}
	token, err := Token(dir)
	if err != nil {
		return err
	}
	g, err := Genesis(dir)
	if err != nil {
		return err
	}
	controlLn, err := net.Listen("tcp", cfg.Control.String())
	if err != nil {
		return fmt.Errorf("control listener: %v", err)
	}
	defer controlLn.Close()
	// The peer listener is opened by the peer layer; checking the address
	// first gives a clear error before anything starts.
	if ln, err := net.Listen("tcp", cfg.P2P.String()); err != nil {
		return fmt.Errorf("peer listener: %v", err)
	} else {
		ln.Close()
	}

	database, err := db.Open(filepath.Join(dir, "chain.db"), g.ChainID())
	if err != nil {
		return err
	}
	defer database.Close()
	store, err := chain.OpenStoredBlocks(filepath.Join(dir, "blocks"), g.ChainID(), database)
	if err != nil {
		return err
	}
	defer store.Close()
	if store.Damaged > 0 {
		log.Warn("block files: damaged frames passed over; their blocks are fetched again", "frames", store.Damaged)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var fatalMu sync.Mutex
	var fatal error
	setFatal := func(err error) {
		fatalMu.Lock()
		if fatal == nil {
			fatal = err
		}
		fatalMu.Unlock()
		stop()
	}
	ccfg := chain.DefaultConfig()
	ccfg.MarksPath = filepath.Join(dir, "marks.json")
	ccfg.State = database
	ccfg.UndoWindow = cfg.UndoWindow
	if cfg.UndoWindow == 0 {
		ccfg.UndoWindow = ^uint64(0) // 0 keeps all undo data
	}
	ccfg.Fatal = func(err error) {
		log.Error("stopping: storage failure", "err", err)
		setFatal(err)
	}
	c, err := chain.New(g, nil, store, ccfg)
	if err != nil {
		return err
	}
	start := time.Now()
	loaded, err := c.Load()
	if err != nil {
		return err
	}
	base := c.Base()
	log.Info("chain loaded", "blocks", loaded, "height", c.Tip().Height, "state_base", base.Height,
		"elapsed", time.Since(start))
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			stop()
		case <-ctx.Done():
		}
	}()

	d := &Daemon{dir: dir, log: log, token: token, cfg: cfg, chain: c, stop: stop}
	n := node.New(c, node.Config{Name: cfg.Name, Mine: cfg.MinerEnabled, Producer: cfg.Producer(), Votes: cfg.Votes,
		Workers: int(cfg.MinerThreads), Interval: time.Duration(cfg.MinerSpacing) * time.Second,
		PoolTransactions: cfg.MempoolTransactions, PoolMiB: int(cfg.MempoolMiB),
		DeferredPath: filepath.Join(dir, "deferred.json")}, log)
	n.IndexCanonical()
	n.Fatal = setFatal
	d.node = n
	var seeds []string
	if cfg.Discovery && g.ChainID() == types.ProductionGenesis().ChainID() {
		seeds = node.ProductionSeeds
	}
	connect := make([]string, len(cfg.Peers))
	for i, a := range cfg.Peers {
		connect[i] = a.String()
	}
	d.peers = node.NewPeers(n, node.PeerConfig{Listen: cfg.P2P.String(), Connect: connect, Seeds: seeds,
		Discovery: cfg.Discovery, MaxPeers: int(cfg.MaxPeers), Agent: Agent,
		BookPath: filepath.Join(dir, "peers.json")})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		if err := d.peers.Run(ctx); err != nil {
			log.Error("peer protocol", "err", err)
			stop()
		}
	}()
	port, _ := d.peers.ListenPort()
	d.running = Running{PID: os.Getpid(), Control: addrOf(controlLn), Started: uint64(time.Now().Unix()),
		P2P: netip.AddrPortFrom(cfg.P2P.Addr(), port)}
	if cfg.APIEnabled {
		if _, err := d.setAPI(true, cfg.APICORS, false); err != nil {
			stop()
			wg.Wait()
			return err
		}
	}
	if err := d.saveRunning(); err != nil {
		stop()
		wg.Wait()
		return err
	}
	srv := &control.Server{Token: token, Handler: d.handle, Log: log, Stop: stop,
		RequestMax: func() int { return int(min(2*n.NextLimits()[types.D]+65_536, 1<<31)) }}
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.Serve(ctx, controlLn)
	}()
	api := "disabled"
	if d.running.API != nil {
		api = "http://" + d.running.API.String()
	}
	fmt.Printf("Ting ready: control=%s peers=%s REST=%s\n", d.running.Control, d.running.P2P, api)
	<-ctx.Done()
	d.mu.Lock()
	if d.api != nil {
		d.api.Close()
	}
	d.mu.Unlock()
	wg.Wait()
	if err := c.Settle(); err != nil {
		log.Error("settling the state", "err", err)
	}
	os.Remove(filepath.Join(dir, "runtime.json"))
	fatalMu.Lock()
	defer fatalMu.Unlock()
	return fatal
}

func addrOf(ln net.Listener) netip.AddrPort {
	ap, _ := netip.ParseAddrPort(ln.Addr().String())
	return ap
}

func (d *Daemon) saveRunning() error {
	d.mu.Lock()
	r := d.running
	d.mu.Unlock()
	return r.Save(d.dir)
}

// setAPI starts or stops the REST API, binding before it changes the
// configuration (when persist), and answers {enabled, address}.
func (d *Daemon) setAPI(enabled, cors, persist bool) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Enabling with the other CORS setting restarts the service.
	if enabled && d.api != nil && d.apiCORS != cors {
		d.api.Close()
		d.api = nil
	}
	switch {
	case enabled && d.api == nil:
		ln, err := net.Listen("tcp", d.cfg.API.String())
		if err != nil {
			return nil, fmt.Errorf("cannot bind REST: %v", err)
		}
		if persist {
			c := *d.cfg
			c.APIEnabled, c.APICORS = true, cors
			if err := c.Save(d.dir); err != nil {
				ln.Close()
				return nil, err
			}
			d.cfg = &c
		}
		srv := &http.Server{Handler: d.node.API(node.APIOptions{CORS: cors}), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)
		d.api, d.apiCORS = srv, cors
		a := addrOf(ln)
		d.running.API = &a
	case !enabled && d.api != nil:
		if persist {
			c := *d.cfg
			c.APIEnabled, c.APICORS = false, false
			if err := c.Save(d.dir); err != nil {
				return nil, err
			}
			d.cfg = &c
		}
		d.api.Close()
		d.api = nil
		d.running.API = nil
	}
	if persist {
		if err := d.running.Save(d.dir); err != nil {
			return nil, err
		}
	}
	return map[string]any{"enabled": d.api != nil, "address": d.running.API}, nil
}

// editConfig changes the configuration and saves it.
func (d *Daemon) editConfig(fn func(c *Config) error) (*Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := *d.cfg
	c.Peers = append([]netip.AddrPort(nil), d.cfg.Peers...)
	if err := fn(&c); err != nil {
		return nil, err
	}
	if err := c.Save(d.dir); err != nil {
		return nil, err
	}
	d.cfg = &c
	return &c, nil
}

type params map[string]json.RawMessage

func (p params) str(key string) (string, error) {
	var s string
	if err := json.Unmarshal(p[key], &s); err != nil {
		return "", fmt.Errorf("expected string %s", key)
	}
	return s, nil
}

func (p params) u64(key string) (*uint64, error) {
	raw, ok := p[key]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var v uint64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("expected integer %s", key)
	}
	return &v, nil
}

func (p params) has(key string) bool {
	raw, ok := p[key]
	return ok && string(raw) != "null"
}

// named resolves a block named by an id or the height of a connected block.
func (d *Daemon) named(text string) (hashing.Hash, error) {
	if len(text) != 64 && text != "" {
		if h, err := strconv.ParseUint(text, 10, 64); err == nil {
			e, ok := d.chain.AtHeight(h)
			if !ok {
				return hashing.Hash{}, errors.New("block not found")
			}
			return e.ID, nil
		}
	}
	return hashing.ParseHash(text)
}

// handle answers a control request.
func (d *Daemon) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	p := params{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, errors.New("expected params object")
		}
	}
	n := d.node
	switch method {
	case "stop":
		return map[string]any{"stopping": true}, nil
	case "status":
		s := n.Status()
		d.mu.Lock()
		s["listeners"] = d.running
		d.mu.Unlock()
		return s, nil
	case "peers":
		d.mu.Lock()
		discovery, maxPeers := d.cfg.Discovery, d.cfg.MaxPeers
		d.mu.Unlock()
		return map[string]any{"items": d.peers.Statuses(), "discovery": discovery, "max_peers": maxPeers,
			"known": d.peers.Book().Len()}, nil
	case "add_peer", "remove_peer":
		text, err := p.str("address")
		if err != nil {
			return nil, err
		}
		a, err := netip.ParseAddrPort(text)
		if err != nil {
			return nil, errors.New("expected IP:port")
		}
		c, err := d.editConfig(func(c *Config) error {
			kept := c.Peers[:0]
			found := false
			for _, x := range c.Peers {
				if x == a {
					found = true
					if method == "remove_peer" {
						continue
					}
				}
				kept = append(kept, x)
			}
			if method == "add_peer" && !found {
				kept = append(kept, a)
			}
			c.Peers = kept
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := d.peers.SetConfigured(c.Peers); err != nil {
			return nil, err
		}
		return map[string]any{"peers": c.Peers}, nil
	case "api":
		var enabled bool
		if err := json.Unmarshal(p["enabled"], &enabled); err != nil {
			return nil, errors.New("expected enabled boolean")
		}
		var cors bool
		if p.has("cors") {
			json.Unmarshal(p["cors"], &cors)
		}
		return d.setAPI(enabled, cors, true)
	case "votes":
		v := n.Voting()
		v["votes"] = n.Votes()
		return v, nil
	case "template":
		var explicit []hashing.Hash
		if p.has("transactions") {
			var ids []string
			if err := json.Unmarshal(p["transactions"], &ids); err != nil {
				return nil, errors.New("expected transaction id")
			}
			explicit = []hashing.Hash{}
			for _, s := range ids {
				id, err := hashing.ParseHash(s)
				if err != nil {
					return nil, err
				}
				explicit = append(explicit, id)
			}
		}
		tpl, err := n.Template(explicit)
		if err != nil {
			return nil, err
		}
		return n.TemplateJSON(tpl), nil
	case "solution":
		ts, err := p.str("template")
		if err != nil {
			return nil, err
		}
		id, err := hashing.ParseHash(ts)
		if err != nil {
			return nil, err
		}
		var nonceText string
		if err := json.Unmarshal(p["nonce"], &nonceText); err != nil {
			var v uint64
			if json.Unmarshal(p["nonce"], &v) != nil {
				return nil, errors.New("expected nonce")
			}
			nonceText = strconv.FormatUint(v, 10)
		}
		nonce, err := node.ParseNonce(nonceText)
		if err != nil {
			return nil, err
		}
		return n.SolutionJSON(id, nonce)
	case "mine":
		count, attempts := uint64(1), uint64(1_000_000)
		if v, err := p.u64("count"); err != nil {
			return nil, err
		} else if v != nil {
			count = *v
		}
		if v, err := p.u64("attempts"); err != nil {
			return nil, err
		} else if v != nil {
			attempts = *v
		}
		return n.Mine(ctx, count, attempts)
	case "miner":
		return d.miner(p)
	case "submit":
		text, err := p.str("hex")
		if err != nil {
			return nil, err
		}
		raw, err := unhex(text, int(min(n.NextLimits()[types.D], 1<<31)))
		if err != nil {
			return nil, err
		}
		nb, err := p.u64("not_before")
		if err != nil {
			return nil, err
		}
		return n.Submit(raw, nb, netip.Addr{}, false)
	case "invalidate", "reconsider":
		text, err := p.str("id")
		if err != nil {
			return nil, err
		}
		id, err := d.named(text)
		if err != nil {
			return nil, err
		}
		tip := func() (hashing.Hash, uint64) { t := d.chain.Tip(); return t.ID, t.Height }
		if method == "invalidate" {
			known, err := d.chain.MarkInvalid(id)
			if err != nil {
				return nil, err
			}
			d.log.Warn("block marked invalid by the operator", "id", id.String())
			t, h := tip()
			return map[string]any{"id": id, "known": known, "tip": t, "height": h}, nil
		}
		cleared, err := d.chain.Reconsider(id)
		if err != nil {
			return nil, err
		}
		d.log.Warn("block reconsidered by the operator", "id", id.String(), "cleared", cleared)
		t, h := tip()
		return map[string]any{"id": id, "cleared": cleared, "tip": t, "height": h}, nil
	}
	return nil, errors.New("unknown control method")
}

// miner starts, stops or describes the built-in miner, and sets what the
// blocks this node builds carry; changes are remembered in config.json.
func (d *Daemon) miner(p params) (any, error) {
	n := d.node
	action := "status"
	if p.has("action") {
		s, err := p.str("action")
		if err != nil {
			return nil, err
		}
		action = s
	}
	switch action {
	case "start":
		d.mu.Lock()
		threads, spacing := uint64(d.cfg.MinerThreads), d.cfg.MinerSpacing
		d.mu.Unlock()
		if v, err := p.u64("threads"); err != nil || (v != nil && (*v < 1 || *v > 64)) {
			return nil, errors.New("threads must be 1..64")
		} else if v != nil {
			threads = *v
		}
		if v, err := p.u64("spacing"); err != nil {
			return nil, err
		} else if v != nil {
			spacing = *v
		}
		if _, err := d.editConfig(func(c *Config) error {
			c.MinerEnabled, c.MinerThreads, c.MinerSpacing = true, uint32(threads), spacing
			return nil
		}); err != nil {
			return nil, err
		}
		n.StartMiner(int(threads), time.Duration(spacing)*time.Second)
		return n.MinerStatus(), nil
	case "stop":
		if _, err := d.editConfig(func(c *Config) error { c.MinerEnabled = false; return nil }); err != nil {
			return nil, err
		}
		n.StopMiner()
		return map[string]any{"running": false}, nil
	case "producer":
		text := ""
		if p.has("producer") {
			s, err := p.str("producer")
			if err != nil {
				return nil, errors.New("expected producer hex")
			}
			text = s
		}
		b, err := unhex(text, ProducerMax)
		if err != nil {
			return nil, errors.New("invalid configuration (producer_hex must be lowercase hex, at most 4096 bytes)")
		}
		c, err := d.editConfig(func(c *Config) error {
			if text == "" {
				c.ProducerHex = nil
			} else {
				c.ProducerHex = &text
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := n.SetProducer(b); err != nil {
			return nil, err
		}
		return d.minerConfig(c), nil
	case "votes":
		var v []uint8
		if err := json.Unmarshal(p["votes"], &v); err != nil || len(v) != 4 {
			return nil, node.ErrVotes
		}
		votes := [4]uint8{v[0], v[1], v[2], v[3]}
		if err := n.SetVotes(votes); err != nil {
			return nil, err
		}
		c, err := d.editConfig(func(c *Config) error { c.Votes = votes; return nil })
		if err != nil {
			return nil, err
		}
		return d.minerConfig(c), nil
	}
	return n.MinerStatus(), nil
}

func (d *Daemon) minerConfig(c *Config) map[string]any {
	return map[string]any{"enabled": c.MinerEnabled, "threads": c.MinerThreads, "spacing": c.MinerSpacing,
		"producer_hex": fmt.Sprintf("%x", c.Producer()), "votes": c.Votes, "selection": "first in, by turns of source address"}
}
