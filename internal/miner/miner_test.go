package miner

import (
	"context"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/types"
)

func TestSolveMidstate(t *testing.T) {
	// A target needing a few hundred tries on average: the midstate search
	// must find a nonce whose full header hash meets it.
	var target types.Target
	target[0] = 0x00
	target[1] = 0x7f
	for i := 2; i < 32; i++ {
		target[i] = 0xff
	}
	h := &types.Header{ProtocolVersion: 1, Height: 7, Target: target, Timestamp: 42}
	h.Nonce = types.Nonce{Lo: ^uint64(0) - 3, Hi: 5} // exercise the carry into Hi
	id, err := Solve(context.Background(), h, 3)
	if err != nil {
		t.Fatal(err)
	}
	if h.ID() != id || !consensus.ValidPoW(id, target) {
		t.Fatal("solution does not meet the target")
	}
}

func TestSolveCancel(t *testing.T) {
	var target types.Target // zero: practically impossible
	target[31] = 1
	h := &types.Header{Target: target}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Solve(ctx, h, 2); err != ErrStopped {
		t.Fatalf("expected ErrStopped, got %v", err)
	}
}
