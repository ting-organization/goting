package node

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/types"
)

// A node starts at the seeds, leaves them once another peer can serve it,
// so that the seeds keep room for new nodes, and comes back to them when
// that peer goes.
func TestNodeLeavesTheSeedsAndComesBack(t *testing.T) {
	askLooking, manageInterval = 100*time.Millisecond, 50*time.Millisecond
	seedRetry = 100 * time.Millisecond
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		askLooking, manageInterval, seedRetry = 10*time.Second, 2*time.Second, time.Minute
	}()
	start := func(ctx context.Context, cfg PeerConfig) (*Peers, *chain.Chain, string) {
		c, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
		n := New(c, Config{}, log)
		cfg.Listen, cfg.Discovery = "127.0.0.1:0", true
		p := NewPeers(n, cfg)
		wg.Add(2)
		go func() { defer wg.Done(); n.Run(ctx) }()
		go func() { defer wg.Done(); p.Run(ctx) }()
		port, _ := p.ListenPort()
		return p, c, "127.0.0.1:" + itoa(int(port))
	}
	_, seedChain, seedAddr := start(ctx, PeerConfig{Agent: "seed"})
	g := seedChain.Genesis()
	for i := uint64(1); i <= 3; i++ {
		if _, _, err := miner.MineOne(ctx, seedChain, miner.Options{Timestamp: g.GenesisTimestamp + 60*i}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Another node of the network, which the seed comes to know and share.
	otherCtx, stopOther := context.WithCancel(ctx)
	_, otherChain, otherAddr := start(otherCtx, PeerConfig{Agent: "other", Connect: []string{seedAddr}})
	waitFor(t, "the other node to sync", func() bool { return otherChain.Tip().Height == 3 })

	node, nodeChain, _ := start(ctx, PeerConfig{Agent: "new", Seeds: []string{seedAddr}, MaxPeers: 2})
	linked := func(addr string) bool {
		for _, i := range node.Info() {
			if i.Addr == addr && i.Direction == "outbound" {
				return true
			}
		}
		return false
	}
	waitFor(t, "the new node to sync from the seed", func() bool { return nodeChain.Tip().Height == 3 })
	waitFor(t, "the new node to find the other", func() bool { return linked(otherAddr) })
	waitFor(t, "the new node to leave the seed", func() bool { return !linked(seedAddr) && linked(otherAddr) })
	// The seed's place is free for the next new node, and stays so.
	time.Sleep(5 * manageInterval)
	if linked(seedAddr) {
		t.Fatal("the node went back to the seed while another peer served it")
	}
	// Its only other peer goes: the seed is its way back to the network.
	stopOther()
	waitFor(t, "the new node to come back to the seed", func() bool { return linked(seedAddr) })
}

// An address a seed name led to in an earlier run, and leads to no longer,
// is retired once the names have been looked up.
func TestRunRetiresSeedAddressesTheNamesLeft(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	c, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	n := New(c, Config{}, log)
	// Ports of this machine that nothing listens on.
	now, earlier := ap("127.0.0.1:9"), ap("127.0.0.1:1")
	p := NewPeers(n, PeerConfig{Listen: "127.0.0.1:0", Discovery: true, Seeds: []string{now.String()}})
	p.Book().AddSeed(earlier)
	wg.Add(2)
	go func() { defer wg.Done(); n.Run(ctx) }()
	go func() { defer wg.Done(); p.Run(ctx) }()
	waitFor(t, "the earlier address to be retired", func() bool {
		return p.Book().IsSeed(now) && !p.Book().IsSeed(earlier)
	})
}
