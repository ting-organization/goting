// Package miner assembles block templates from pending transactions and
// searches for a nonce. Which transactions to include and in what order is
// local policy; the resulting block must satisfy every rule.
package miner

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/commit"
	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/exec"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/vm"
)

// Template is a block ready for the nonce search.
type Template struct {
	Header   *types.Header
	Body     *types.Body
	Receipts []*types.Receipt
	// Skipped are the indices of offered transactions left out, with why.
	Skipped map[int]error
}

// Options shape a template.
type Options struct {
	Timestamp uint64 // raised to median+1 when not above the median
	Producer  []byte
	Votes     [4]uint8
}

// Build executes the offered raw transactions on top of parent, keeping
// those that are valid envelopes and fit the block, in order.
func Build(c *chain.Chain, parent *chain.Entry, opts Options, txs [][]byte) (*Template, error) {
	height, median, target, limits := c.NextContext(parent)
	ts := opts.Timestamp
	if ts <= median {
		ts = median + 1
	}
	ctx := &vm.BlockContext{
		ChainID:   c.ChainID(),
		Height:    height,
		Timestamp: ts,
		ParentID:  parent.ID,
		Producer:  opts.Producer,
	}
	b, err := c.NewBuilder(parent, ctx, limits)
	if err != nil {
		return nil, err
	}
	t := &Template{Skipped: make(map[int]error)}
	for i, raw := range txs {
		tx, err := types.DecodeTransaction(raw)
		if err != nil {
			t.Skipped[i] = err
			continue
		}
		// Room for at least the transaction's own data base.
		if b.Used()[types.D]+types.DataBase(len(raw)) > limits[types.D] {
			t.Skipped[i] = exec.ErrCapacity
			continue
		}
		r, err := b.Add(raw, tx)
		if err != nil {
			if errors.Is(err, exec.ErrCapacity) {
				t.Skipped[i] = err
				continue
			}
			return nil, err
		}
		t.Receipts = append(t.Receipts, r)
	}
	a, err := Assemble(c.ChainID(), ctx, target, opts.Votes, b)
	if err != nil {
		return nil, err
	}
	a.Skipped = t.Skipped
	return a, nil
}

// Assemble finishes a template from a builder whose transactions are final.
func Assemble(chainID hashing.Hash, ctx *vm.BlockContext, target types.Target, votes [4]uint8, b *exec.Builder) (*Template, error) {
	res, err := b.Finish()
	if err != nil {
		return nil, err
	}
	t := &Template{Skipped: map[int]error{}}
	t.Receipts = res.Receipts
	t.Body = &types.Body{ProducerData: ctx.Producer, Transactions: res.Transactions, Receipts: res.ReceiptBytes}
	t.Header = &types.Header{
		ProtocolVersion:  types.ProtocolVersion,
		ChainID:          chainID,
		Height:           ctx.Height,
		ParentID:         ctx.ParentID,
		Timestamp:        ctx.Timestamp,
		Target:           target,
		TransactionsRoot: commit.TransactionsRoot(res.Transactions),
		ReceiptsRoot:     commit.ReceiptsRoot(res.ReceiptBytes),
		StateRoot:        res.State.Root(),
		ResourcesUsed:    res.Used,
		Votes:            votes,
		ProducerHash:     types.ProducerHash(ctx.Producer),
	}
	return t, nil
}

// Block returns the template's block.
func (t *Template) Block() *types.Block { return &types.Block{Header: t.Header, Body: t.Body} }

// ErrStopped reports a cancelled search.
var ErrStopped = errors.New("nonce search stopped")

// ErrExhausted reports a bounded search that found nothing.
var ErrExhausted = errors.New("nonce attempts exhausted")

// Solve searches nonces on all cores until the BlockId meets the target. It
// sets the header's nonce and returns the BlockId.
func Solve(ctx context.Context, h *types.Header, workers int) (hashing.Hash, error) {
	return SolveN(ctx, h, workers, 0)
}

// SolveN is Solve bounded to about attempts nonces in all (0: unbounded),
// starting from the header's nonce, so a miner can take a fresh template.
func SolveN(ctx context.Context, h *types.Header, workers int, attempts uint64) (hashing.Hash, error) {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	enc := h.Encode()
	// The hashed message is the domain header ("Ting/", u16 length, "block")
	// then the 294 header bytes; the nonce is in the last 16. Everything
	// before the last partial 64-byte block is hashed once per worker.
	prefix := []byte("Ting/\x05\x00block")
	msg := append(prefix, enc...)
	nonceAt := len(prefix) + types.NonceOffset
	split := (nonceAt / 64) * 64
	base := sha256.New()
	base.Write(msg[:split])
	mid, err := base.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return hashing.Hash{}, err
	}
	target := h.Target
	var found atomic.Bool
	var result struct {
		sync.Mutex
		nonce types.Nonce
		id    hashing.Hash
	}
	var wg sync.WaitGroup
	start := h.Nonce
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			hs := sha256.New()
			tail := append([]byte(nil), msg[split:]...)
			off := nonceAt - split
			lo, hi := start.Lo+uint64(w), start.Hi
			var sum [32]byte
			perWorker := attempts / uint64(workers)
			if attempts > 0 && perWorker == 0 {
				perWorker = 1
			}
			for i := uint64(0); ; i++ {
				if i&0xfff == 0 && (found.Load() || ctx.Err() != nil) {
					return
				}
				if attempts > 0 && i >= perWorker {
					return
				}
				binary.LittleEndian.PutUint64(tail[off:], lo)
				binary.LittleEndian.PutUint64(tail[off+8:], hi)
				_ = hs.(encoding.BinaryUnmarshaler).UnmarshalBinary(mid)
				hs.Write(tail)
				hs.Sum(sum[:0])
				if consensus.ValidPoW(sum, target) {
					if found.CompareAndSwap(false, true) {
						result.Lock()
						result.nonce = types.Nonce{Lo: lo, Hi: hi}
						result.id = sum
						result.Unlock()
					}
					return
				}
				next := lo + uint64(workers)
				if next < lo {
					hi++
				}
				lo = next
			}
		}(w)
	}
	wg.Wait()
	if !found.Load() {
		if ctx.Err() == nil && attempts > 0 {
			return hashing.Hash{}, ErrExhausted
		}
		return hashing.Hash{}, ErrStopped
	}
	h.Nonce = result.nonce
	if id := h.ID(); id != result.id {
		return hashing.Hash{}, fmt.Errorf("miner: midstate hash mismatch")
	}
	return result.id, nil
}

// MineOne builds, solves and submits one block on the current tip.
func MineOne(ctx context.Context, c *chain.Chain, opts Options, txs [][]byte) (*chain.Entry, *Template, error) {
	t, err := Build(c, c.Tip(), opts, txs)
	if err != nil {
		return nil, nil, err
	}
	if _, err := Solve(ctx, t.Header, 0); err != nil {
		return nil, t, err
	}
	e, err := c.ProcessBlock(t.Block().Encode())
	return e, t, err
}
