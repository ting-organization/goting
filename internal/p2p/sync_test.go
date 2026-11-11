package p2p

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// A peer that shows no headers is judged by the tip it claimed before it
// was asked: one it announces while its answer is on the way may be a block
// it took since, which says nothing of the answer.
func TestNoHeadersIsJudgedByTheTipClaimedBefore(t *testing.T) {
	_, be, addr := startServer(t)
	c, err := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	be.chain = c.ChainID()
	before, since := hashing.Hash{0xaa}, hashing.Hash{0xbb}
	var tip atomic.Pointer[hashing.Hash]
	tip.Store(&before)
	// The peer's newer tip reaches this node, from another task of the
	// connection, before the empty answer does.
	be.headers = func([]hashing.Hash) ([][]byte, error) {
		tip.Store(&since)
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, addr, &Hello{Version: 1, ChainID: be.chain, Agent: "client", Nonce: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var judged []hashing.Hash
	sy := &Syncer{Chain: c,
		Claimed:     func() *hashing.Hash { return tip.Load() },
		OnNoHeaders: func(_ []hashing.Hash, claimed *hashing.Hash) { judged = append(judged, *claimed) }}
	if _, err := sy.SyncOnce(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if len(judged) != 1 || judged[0] != before {
		t.Fatalf("judged by %x", judged)
	}
}
