package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/p2p"
	"github.com/ting-organization/goting/internal/seed"
	"github.com/ting-organization/goting/internal/types"
)

// seedPin returns a built-in network's pinned last seed block.
func seedPin(g *types.GenesisConfig) *p2p.SeedPin {
	if g.ChainID() == types.ProductionGenesis().ChainID() {
		return &p2p.SeedPin{Height: types.ProductionSeedLength, ID: hashing.MustParseHash(types.ProductionLastSeedIDHex)}
	}
	return nil
}

// cmdSync validates a peer's chain block by block.
func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	gf := addGenesisFlags(fs)
	addr := fs.String("peer", "2.29.97.212:18442", "peer to sync from")
	data := fs.String("data", "", "data directory for the block log (empty: memory)")
	bundleOut := fs.String("bundle-out", "", "write the seed blocks as a bundle (and FILE.list)")
	follow := fs.Bool("follow", false, "keep following the peer's announcements")
	verbose := fs.Bool("v", false, "log every block with transactions")
	fs.Parse(args)
	g, err := gf.load()
	if err != nil {
		return err
	}
	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	var store chain.BlockStore = chain.NewMemStore()
	if *data != "" {
		if err := os.MkdirAll(*data, 0o755); err != nil {
			return err
		}
		fsStore, err := chain.OpenFileStore(filepath.Join(*data, "blocks"), g.ChainID())
		if err != nil {
			return err
		}
		defer fsStore.Close()
		store = fsStore
	}
	pin := seedPin(g)
	var ids []hashing.Hash
	if pin != nil {
		ids = chain.SeedFromStore(store, types.GenesisBlockID(g.ChainID()), pin.ID, pin.Height)
	}
	c, err := chain.New(g, ids, store, chain.DefaultConfig())
	if err != nil {
		return err
	}
	if n, err := c.Load(); err != nil {
		return err
	} else if n > 0 {
		log.Info("loaded stored blocks", "count", n, "tip", c.Tip().Height)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	conn, err := p2p.Dial(ctx, *addr, newHello(g, *follow))
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Info("connected", "peer", *addr, "agent", conn.Peer.Agent, "height", conn.Peer.Height,
		"tip", conn.Peer.Tip.String())
	start := time.Now()
	txs := 0
	var seedBlocks [][]byte
	sy := &p2p.Syncer{Chain: c, Log: log, Pin: pin, OnBlock: func(e *chain.Entry, raw []byte) {
		txs += e.TxCount
		if pin != nil && e.Height <= pin.Height && *bundleOut != "" {
			seedBlocks = append(seedBlocks, raw)
		}
		if e.Height%50 == 0 {
			log.Info("progress", "height", e.Height, "transactions", txs, "elapsed", time.Since(start).Round(time.Millisecond))
		}
	}}
	n, err := sy.SyncOnce(ctx, conn)
	tip := c.Tip()
	st, _ := c.StateAt(tip)
	fmt.Printf("connected %d blocks with %d transactions in %s\ntip %s height %d\nstate root %s (%d records)\n",
		n, txs, time.Since(start).Round(time.Millisecond), tip.ID, tip.Height, st.Root(), st.Len())
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
	if pin != nil {
		if e, ok := c.AtHeight(pin.Height); ok && e.ID == pin.ID {
			fmt.Printf("seed verified: all %d seed blocks validated and executed; height %d is %s\n", pin.Height, pin.Height, pin.ID)
		} else {
			fmt.Printf("seed not reached: tip height %d\n", tip.Height)
		}
	}
	if *bundleOut != "" && len(seedBlocks) > 0 {
		if err := os.WriteFile(*bundleOut, seed.EncodeBundle(seedBlocks), 0o644); err != nil {
			return err
		}
		list, _ := seed.ListFromBundle(seedBlocks)
		f, err := os.Create(*bundleOut + ".list")
		if err != nil {
			return err
		}
		for _, id := range list {
			fmt.Fprintln(f, id)
		}
		f.Close()
		fmt.Printf("wrote %d seed blocks to %s and %s.list\n", len(seedBlocks), *bundleOut, *bundleOut)
	}
	if *follow {
		log.Info("following the peer")
		err := sy.Run(ctx, conn)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
