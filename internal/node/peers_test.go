package node

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/types"
)

// Two nodes speak the peer protocol to each other: B follows A's chain,
// relays a transaction to A, A mines it, and B receives the block.
func TestTwoNodesOverPeerProtocol(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ca, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	a := New(ca, Config{Mine: true, Workers: 1, Interval: 100 * time.Millisecond}, log)
	pa := NewPeers(a, PeerConfig{Listen: "127.0.0.1:0", Agent: "node-a"})
	go a.Run(ctx)
	go pa.Run(ctx)
	port, ok := pa.ListenPort()
	if !ok {
		t.Fatal("A did not listen")
	}
	addr := "127.0.0.1:" + itoa(int(port))

	cb, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	b := New(cb, Config{}, log)
	pb := NewPeers(b, PeerConfig{Connect: []string{addr}, Agent: "node-b"})
	go b.Run(ctx)
	go pb.Run(ctx)

	// B catches up with A.
	waitFor(t, "B to follow A", func() bool { return cb.Tip().Height >= 3 && cb.Tip().Height+2 >= ca.Tip().Height })

	// A transaction submitted to B reaches A's pool and is mined.
	tx := (&types.Transaction{Kind: types.KindDeploy, Budget: types.ResourceVector{50_000_000, 64, 10_000, 10_000},
		Code: samples.Counter()}).Encode()
	id, err := b.SubmitTransaction(tx)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the transaction to be mined and reach B", func() bool { return len(b.Occurrences(id)) == 1 })
	if len(a.Occurrences(id)) != 1 {
		t.Fatal("A did not index its own block")
	}
	if b.PoolLen() != 0 {
		t.Fatal("B kept the mined transaction pooled")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Discovery: C announces its listening port to A, A proves it with a
// connection of its own and shares it with B, which then connects to C
// without having it configured.
func TestDiscoveryThroughAPeer(t *testing.T) {
	askLooking, manageInterval = 200*time.Millisecond, 100*time.Millisecond
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		askLooking, manageInterval = 10*time.Second, 2*time.Second
	}()
	start := func(name string, connect ...string) (*Peers, string) {
		c, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
		n := New(c, Config{}, log)
		p := NewPeers(n, PeerConfig{Listen: "127.0.0.1:0", Connect: connect, Discovery: true, Agent: name})
		wg.Add(2)
		go func() { defer wg.Done(); n.Run(ctx) }()
		go func() { defer wg.Done(); p.Run(ctx) }()
		port, _ := p.ListenPort()
		return p, "127.0.0.1:" + itoa(int(port))
	}
	_, addrA := start("a")
	pc, addrC := start("c", addrA)
	pb, _ := start("b", addrA)
	connected := func(p *Peers, addr string) bool {
		for _, i := range p.Info() {
			if i.Addr == addr && i.Direction == "outbound" && !i.Configured {
				return true
			}
		}
		return false
	}
	waitFor(t, "B to connect to C, learned from A", func() bool { return connected(pb, addrC) })
	_ = pc
}
