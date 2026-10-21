// Command ting is a Ting node and toolbox.
//
//	ting info     [-network dev | -genesis FILE]
//	ting replay   [-network dev] -bundle FILE [-list FILE | -trust-bundle]
//	ting node     [-network dev] [-data DIR] [-http ADDR] [-mine] [-seed-bundle FILE -seed-list FILE]
//	ting tx       deploy|call [-network dev] ...
//	ting decode   -block FILE | -tx HEX | -receipt HEX
//	ting devnet   [-blocks N] -out FILE
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/node"
	"github.com/ting-organization/goting/internal/seed"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
)

func main() {
	args := os.Args[1:]
	// The global --data-dir names the node the reference-style commands use.
	dataDir := ""
	for len(args) > 0 && (args[0] == "--data-dir" || args[0] == "-data-dir" ||
		strings.HasPrefix(args[0], "--data-dir=") || strings.HasPrefix(args[0], "-data-dir=")) {
		if k := strings.IndexByte(args[0], '='); k >= 0 {
			dataDir, args = args[0][k+1:], args[1:]
			continue
		}
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "ting: --data-dir needs a directory")
			os.Exit(2)
		}
		dataDir, args = args[1], args[2:]
	}
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	if daemonCommands[args[0]] || (args[0] == "probe" && len(args) == 2 && !strings.HasPrefix(args[1], "-")) {
		var res any
		var err error
		if args[0] == "probe" {
			dir := dataDir
			if dir == "" {
				dir, err = defaultDataDir()
			}
			if err == nil {
				res, err = probeHello(dir, args[1])
			}
		} else {
			res, err = runDaemonCommand(dataDir, args[0], args[1:])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "ting:", err)
			os.Exit(1)
		}
		if res != nil {
			var v any = res
			if raw, ok := res.(json.RawMessage); ok {
				var x any
				if json.Unmarshal(raw, &x) == nil {
					v = x
				}
			}
			b, _ := json.MarshalIndent(v, "", "  ")
			fmt.Println(string(b))
		}
		return
	}
	os.Args = append([]string{os.Args[0]}, args...)
	var err error
	switch os.Args[1] {
	case "info":
		err = cmdInfo(os.Args[2:])
	case "replay":
		err = cmdReplay(os.Args[2:])
	case "node":
		err = cmdNode(os.Args[2:])
	case "tx":
		err = cmdTx(os.Args[2:])
	case "decode":
		err = cmdDecode(os.Args[2:])
	case "devnet":
		err = cmdDevnet(os.Args[2:])
	case "snapshot":
		err = cmdSnapshot(os.Args[2:])
	case "peer":
		err = cmdPeer(os.Args[2:])
	case "sync":
		err = cmdSync(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "probe":
		err = cmdProbe(os.Args[2:])
	case "conformance":
		err = cmdConformance(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ting: a Ting protocol node in Go

node commands (as the reference node's; global --data-dir DIR, default ~/.ting):
  init [--dev | --genesis FILE] [--name N] [--control A] [--p2p A] [--api-bind A] [--api]
  run | start | stop | status | peers | votes
  probe IP:PORT                 say hello over the peer protocol and show the answer
  add node IP:PORT | remove node IP:PORT
  api enable [--cors] | api disable
  mine [--count N] [--attempts N]
  miner start [--threads N] [--spacing S] | stop | status | producer [HEX] | votes C S D G
  template [--tx ID]... | solution TEMPLATE NONCE
  submit FILE [--not-before T]
  invalidate BLOCK | reconsider BLOCK

tools:
  info     print a network's genesis configuration and identifiers
  replay   validate a seed bundle block by block (rules §27.2)
  node     run a node from flags rather than a data directory's config.json
  tx       build a transaction envelope (deploy or call)
  decode   decode a block, transaction or receipt
  devnet   mine a sample development chain into a bundle
  snapshot print a state snapshot's records as JSON lines
  peer     ask a peer for its state, headers, a block or addresses
  sync     validate a peer's chain block by block (and its seed)
  stats    summarize what a bundle's blocks exercise
  conformance  run the reference repository's vectors, voting chain and seeds

run "ting <command> -h" for flags
`)
}

// genesisFlags adds -network and -genesis.
type genesisFlags struct {
	network *string
	file    *string
}

func addGenesisFlags(fs *flag.FlagSet) genesisFlags {
	return genesisFlags{
		network: fs.String("network", "development", "built-in network: development or experimental"),
		file:    fs.String("genesis", "", "file holding the 150-byte genesis configuration (raw or hex)"),
	}
}

func (g genesisFlags) load() (*types.GenesisConfig, error) {
	if *g.file != "" {
		b, err := os.ReadFile(*g.file)
		if err != nil {
			return nil, err
		}
		if len(b) == types.GenesisConfigSize {
			return types.DecodeGenesisConfig(b)
		}
		return types.DecodeGenesisConfigHex(string(b))
	}
	cfg, ok := types.NetworkByName(*g.network)
	if !ok {
		return nil, fmt.Errorf("unknown network %q", *g.network)
	}
	return cfg, nil
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	gf := addGenesisFlags(fs)
	fs.Parse(args)
	g, err := gf.load()
	if err != nil {
		return err
	}
	id := g.ChainID()
	out := map[string]any{
		"protocol_version":  g.ProtocolVersion,
		"genesis_timestamp": g.GenesisTimestamp,
		"initial_target":    g.InitialTarget.String(),
		"pow_limit":         g.PowLimit.String(),
		"target_interval":   g.TargetInterval,
		"half_life":         g.HalfLife,
		"vote_epoch_length": g.VoteEpochLength,
		"initial_limits":    g.InitialLimits,
		"minimum_limits":    g.MinimumLimits,
		"config_hex":        hex.EncodeToString(g.Encode()),
		"chain_id":          id,
		"genesis_block_id":  types.GenesisBlockID(id),
	}
	return printJSON(out)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func loadSeed(bundlePath, listPath string, trust bool) ([][]byte, []hashing.Hash, error) {
	b, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, nil, err
	}
	blocks, err := seed.ParseBundle(b)
	if err != nil {
		return nil, nil, err
	}
	var list []hashing.Hash
	switch {
	case listPath != "":
		lb, err := os.ReadFile(listPath)
		if err != nil {
			return nil, nil, err
		}
		if list, err = seed.ParseList(lb); err != nil {
			return nil, nil, err
		}
	case trust:
		if list, err = seed.ListFromBundle(blocks); err != nil {
			return nil, nil, err
		}
	}
	return blocks, list, nil
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	gf := addGenesisFlags(fs)
	bundle := fs.String("bundle", "", "seed bundle: u32 length + block bytes, repeated")
	list := fs.String("list", "", "seed list: BlockIds at heights 1..N (text or JSON)")
	trust := fs.Bool("trust-bundle", false, "pin the bundle's own BlockIds as the seed list (testing only)")
	verbose := fs.Bool("v", false, "print every block")
	snapOut := fs.String("snapshot", "", "write the final state as a snapshot file")
	fs.Parse(args)
	if *bundle == "" {
		return errors.New("-bundle is required")
	}
	g, err := gf.load()
	if err != nil {
		return err
	}
	blocks, ids, err := loadSeed(*bundle, *list, *trust)
	if err != nil {
		return err
	}
	if len(ids) == 0 && !*trust {
		if pin := seedPin(g); pin != nil {
			// The pinned last seed BlockId authenticates the whole bundle:
			// every header names its parent.
			derived, err := seed.ListFromBundle(blocks)
			if err != nil {
				return err
			}
			if uint64(len(derived)) != pin.Height || derived[len(derived)-1] != pin.ID {
				return fmt.Errorf("bundle does not end at the pinned seed block %s at height %d", pin.ID, pin.Height)
			}
			ids = derived
			fmt.Printf("seed pinned by its last BlockId %s\n", pin.ID)
		}
	}
	if len(ids) > 0 && len(ids) != len(blocks) {
		return fmt.Errorf("bundle has %d blocks, list has %d", len(blocks), len(ids))
	}
	c, err := chain.New(g, ids, chain.NewMemStore(), chain.DefaultConfig())
	if err != nil {
		return err
	}
	fmt.Printf("network chain id %s, genesis %s, %d blocks\n", c.ChainID(), c.GenesisID(), len(blocks))
	rep, err := seed.Replay(c, blocks, func(h uint64, e *chain.Entry) {
		if *verbose {
			fmt.Printf("height %d  %s  txs %d  used %v\n", h, e.ID, e.TxCount, e.Header.ResourcesUsed)
		}
	})
	if err != nil {
		var rm *chain.ReceiptMismatch
		var sc *chain.SeedConformanceError
		if errors.As(err, &sc) {
			errors.As(sc.Err, &rm)
		}
		if rm != nil || errors.As(err, &rm) {
			fmt.Printf("FIRST DIFFERENCE: height %d transaction %d field %s\n  derived: %s\n  block:   %s\n",
				rm.Height, rm.Index, rm.Field, rm.Got, rm.Want)
		}
		return err
	}
	st, _ := rep.Tip.State()
	fmt.Printf("replayed %d blocks, %d transactions in %s\ntip %s height %d\nstate root %s (%d records)\n",
		rep.Blocks, rep.Transactions, rep.Elapsed.Round(time.Millisecond), rep.Tip.ID, rep.Tip.Height,
		st.Root(), st.Len())
	if *snapOut != "" {
		info := state.SnapshotInfo{Block: rep.Tip.ID, Height: rep.Tip.Height}
		if err := state.WriteSnapshot(*snapOut, info, st); err != nil {
			return err
		}
		fmt.Printf("wrote state snapshot %s\n", *snapOut)
	}
	return nil
}

func cmdNode(args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	gf := addGenesisFlags(fs)
	data := fs.String("data", "", "data directory (empty: in memory)")
	addr := fs.String("api", node.DefaultAPIAddr, "REST API listen address (empty: none)")
	cors := fs.Bool("api-cors", false, "let browser applications from any origin use the REST API")
	name := fs.String("name", "", "the node's name, shown in its status")
	mine := fs.Bool("mine", false, "mine blocks")
	producer := fs.String("producer", "", "producer data for mined blocks (the reference default: none)")
	votes := fs.String("votes", "0,0,0,0", "capacity votes C,S,D,G (0 hold, 1 increase, 2 decrease)")
	workers := fs.Int("workers", 0, "mining goroutines (0: all cores)")
	seedBundle := fs.String("seed-bundle", "", "seed bundle to connect at start")
	seedList := fs.String("seed-list", "", "seed list pinned for the seed heights")
	trust := fs.Bool("trust-bundle", false, "pin the seed bundle's own BlockIds (testing only)")
	interval := fs.Duration("interval", 0, "least time between blocks this node mines (0: as fast as the target allows)")
	peersFlag := fs.String("peers", "", "comma-separated configured peers, always kept connected")
	listen := fs.String("listen", "", "peer protocol listen address, such as :18442 (empty: no inbound peers)")
	discovery := fs.Bool("discovery", true, "find peers through peers and the network's seed nodes, and share addresses")
	maxPeers := fs.Int("max-peers", 8, "outbound connections kept, configured ones included (1 to 64)")
	logLevel := fs.String("log", "info", "log level: debug, info, warn, error")
	fs.Parse(args)
	g, err := gf.load()
	if err != nil {
		return err
	}
	var blocks [][]byte
	var ids []hashing.Hash
	if *seedBundle != "" {
		if blocks, ids, err = loadSeed(*seedBundle, *seedList, *trust); err != nil {
			return err
		}
	} else if *seedList != "" {
		lb, err := os.ReadFile(*seedList)
		if err != nil {
			return err
		}
		if ids, err = seed.ParseList(lb); err != nil {
			return err
		}
	}
	var store chain.BlockStore = chain.NewMemStore()
	var database *db.DB
	if *data != "" {
		if err := os.MkdirAll(*data, 0o755); err != nil {
			return err
		}
		if database, err = db.Open(filepath.Join(*data, "chain.db"), g.ChainID()); err != nil {
			return err
		}
		defer database.Close()
		fsStore, err := chain.OpenStoredBlocks(filepath.Join(*data, "blocks"), g.ChainID(), database)
		if err != nil {
			return err
		}
		defer fsStore.Close()
		store = fsStore
	}
	if pin := seedPin(g); pin != nil && len(ids) == 0 {
		ids = chain.SeedFromStore(store, types.GenesisBlockID(g.ChainID()), pin.ID, pin.Height)
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var fatal atomic.Pointer[error]
	setFatal := func(err error) {
		fatal.CompareAndSwap(nil, &err)
		stop()
	}
	ccfg := chain.DefaultConfig()
	ccfg.Log = log
	if *data != "" {
		ccfg.MarksPath = filepath.Join(*data, "marks.json")
		ccfg.State = database
		ccfg.Fatal = setFatal
	}
	c, err := chain.New(g, ids, store, ccfg)
	if err != nil {
		return err
	}
	start := time.Now()
	n, err := c.Load()
	if err != nil {
		return err
	}
	if n > 0 {
		log.Info("loaded stored blocks", "count", n, "tip", c.Tip().Height, "elapsed", time.Since(start))
	}
	if len(blocks) > 0 {
		rep, err := seed.Replay(c, blocks, nil)
		if err != nil {
			return err
		}
		log.Info("seed connected", "blocks", rep.Blocks, "tip", rep.Tip.Height)
	}
	v, err := parseVotes(*votes)
	if err != nil {
		return err
	}
	deferredPath := ""
	if *data != "" {
		deferredPath = filepath.Join(*data, "deferred.json")
	}
	nd := node.New(c, node.Config{Name: *name, Mine: *mine, Producer: []byte(*producer), Votes: v, Workers: *workers,
		Interval: *interval, DeferredPath: deferredPath}, log)
	nd.IndexCanonical()
	nd.Fatal = setFatal
	if *addr != "" {
		srv := &http.Server{Addr: *addr, Handler: nd.API(node.APIOptions{CORS: *cors}), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("http api", "addr", *addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http", "err", err)
				stop()
			}
		}()
		defer srv.Shutdown(context.Background())
	}
	if c.SeedLen() == 0 && seedPin(g) == nil {
		log.Warn("no seed list pinned: seed heights are not checked against the network's BlockIds")
	}
	var connect []string
	for _, a := range strings.Split(*peersFlag, ",") {
		if a = strings.TrimSpace(a); a != "" {
			connect = append(connect, a)
		}
	}
	if len(connect) > node.MaxConfigured {
		return fmt.Errorf("at most %d configured peers", node.MaxConfigured)
	}
	if *maxPeers < 1 || *maxPeers > 64 {
		return errors.New("-max-peers must be 1 to 64")
	}
	var seeds []string
	if seedPin(g) != nil && g.ChainID() == types.ProductionGenesis().ChainID() {
		seeds = node.ProductionSeeds
	}
	bookPath := ""
	if *data != "" {
		bookPath = filepath.Join(*data, "peers.json")
	}
	if len(connect) > 0 || *listen != "" || *discovery {
		peers := node.NewPeers(nd, node.PeerConfig{Listen: *listen, Connect: connect, Seeds: seeds,
			Discovery: *discovery, MaxPeers: *maxPeers, Agent: Agent, SeedPin: seedPin(g), BookPath: bookPath})
		go func() {
			if err := peers.Run(ctx); err != nil {
				log.Error("peer protocol", "err", err)
				stop()
			}
		}()
	}
	log.Info("node running", "chain_id", c.ChainID().String(), "height", c.Tip().Height, "mine", *mine,
		"peers", connect, "listen", *listen)
	nd.Run(ctx)
	c.Close()
	if err := c.Settle(); err != nil {
		log.Error("settling the state", "err", err)
	}
	if p := fatal.Load(); p != nil {
		return *p
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func parseVotes(s string) ([4]uint8, error) {
	var v [4]uint8
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return v, errors.New("votes must be four comma-separated values")
	}
	for i, p := range parts {
		x, err := strconv.ParseUint(strings.TrimSpace(p), 10, 8)
		if err != nil || x > 2 {
			return v, fmt.Errorf("vote %q must be 0, 1 or 2", p)
		}
		v[i] = uint8(x)
	}
	return v, nil
}

func parseBudget(s string) (types.ResourceVector, error) {
	var v types.ResourceVector
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return v, errors.New("budget must be C,S,D,G")
	}
	for i, p := range parts {
		x, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return v, err
		}
		v[i] = x
	}
	return v, nil
}

func readHexOrFile(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "@") {
		return os.ReadFile(s[1:])
	}
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

func cmdTx(args []string) error {
	if len(args) < 1 || (args[0] != "deploy" && args[0] != "call") {
		return errors.New(`usage: ting tx deploy|call [flags]`)
	}
	kind := args[0]
	fs := flag.NewFlagSet("tx "+kind, flag.ExitOnError)
	gf := addGenesisFlags(fs)
	code := fs.String("code", "", "module: hex, or @file")
	target := fs.String("target", "", "target ContractId (hex)")
	input := fs.String("input", "", "input: hex, or @file")
	budget := fs.String("budget", "100000000,1024,0,524288", "budget C,S,D,G (D 0: the data base plus 4096)")
	out := fs.String("out", "", "write raw bytes to this file instead of printing hex")
	fs.Parse(args[1:])
	g, err := gf.load()
	if err != nil {
		return err
	}
	b, err := parseBudget(*budget)
	if err != nil {
		return err
	}
	in, err := readHexOrFile(*input)
	if err != nil {
		return err
	}
	tx := &types.Transaction{Budget: b, Input: in}
	if kind == "deploy" {
		tx.Kind = types.KindDeploy
		if tx.Code, err = readHexOrFile(*code); err != nil {
			return err
		}
	} else {
		tx.Kind = types.KindCall
		if tx.Target, err = hashing.ParseHash(strings.TrimPrefix(*target, "0x")); err != nil {
			return fmt.Errorf("target: %w", err)
		}
	}
	if tx.Budget[types.D] == 0 {
		tx.Budget[types.D] = types.DataBase(len(tx.Encode())) + 4096
	}
	raw := tx.Encode()
	if _, err := types.DecodeTransaction(raw); err != nil {
		return err
	}
	id := types.TransactionID(g.ChainID(), raw)
	if *out != "" {
		if err := os.WriteFile(*out, raw, 0o644); err != nil {
			return err
		}
	}
	res := map[string]any{"transaction_id": id, "data_base": types.DataBase(len(raw)), "size": len(raw)}
	if *out == "" {
		res["raw"] = hex.EncodeToString(raw)
	}
	if kind == "deploy" {
		res["contract_id_ordinal_0"] = types.ContractID(g.ChainID(), id, 0)
	}
	return printJSON(res)
}

func cmdDecode(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	block := fs.String("block", "", "block: hex, or @file")
	tx := fs.String("tx", "", "transaction: hex, or @file")
	receipt := fs.String("receipt", "", "receipt: hex, or @file")
	fs.Parse(args)
	switch {
	case *block != "":
		raw, err := readHexOrFile(*block)
		if err != nil {
			return err
		}
		b, err := types.DecodeBlock(raw)
		if err != nil {
			return err
		}
		h := b.Header
		out := map[string]any{"id": h.ID(), "version": h.ProtocolVersion, "chain_id": h.ChainID,
			"height": h.Height, "parent": h.ParentID, "timestamp": h.Timestamp, "target": h.Target.String(),
			"transactions_root": h.TransactionsRoot, "receipts_root": h.ReceiptsRoot, "state_root": h.StateRoot,
			"resources_used": h.ResourcesUsed, "votes": h.Votes, "producer_hash": h.ProducerHash,
			"nonce": []uint64{h.Nonce.Lo, h.Nonce.Hi}, "producer": hex.EncodeToString(b.Body.ProducerData),
			"transactions": len(b.Body.Transactions), "size": len(raw)}
		var rs []any
		for _, r := range b.Body.Receipts {
			rc, err := types.DecodeReceipt(r)
			if err != nil {
				rs = append(rs, err.Error())
				continue
			}
			rs = append(rs, map[string]any{"tx": rc.TransactionID, "status": rc.Status,
				"used": rc.ResourcesUsed, "result": hex.EncodeToString(rc.Result), "events": len(rc.Events)})
		}
		out["receipts"] = rs
		return printJSON(out)
	case *tx != "":
		raw, err := readHexOrFile(*tx)
		if err != nil {
			return err
		}
		t, err := types.DecodeTransaction(raw)
		if err != nil {
			return err
		}
		out := map[string]any{"kind": t.Kind, "budget": t.Budget, "input": hex.EncodeToString(t.Input),
			"data_base": types.DataBase(len(raw))}
		if t.Kind == types.KindCall {
			out["target"] = t.Target
		} else {
			out["code_length"] = len(t.Code)
			out["code_hash"] = types.CodeHash(t.Code)
		}
		return printJSON(out)
	case *receipt != "":
		raw, err := readHexOrFile(*receipt)
		if err != nil {
			return err
		}
		r, err := types.DecodeReceipt(raw)
		if err != nil {
			return err
		}
		return printJSON(r)
	}
	return errors.New("one of -block, -tx or -receipt is required")
}

func cmdSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	in := fs.String("in", "", "snapshot file")
	fs.Parse(args)
	if *in == "" {
		return errors.New("-in is required")
	}
	info, t, err := state.ReadSnapshot(*in)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(map[string]any{"block": info.Block, "height": info.Height, "root": info.Root, "records": info.Count})
	t.Walk(func(k hashing.Hash, v []byte) bool {
		enc.Encode(map[string]string{"key": k.String(), "value": hex.EncodeToString(v)})
		return true
	})
	return nil
}
