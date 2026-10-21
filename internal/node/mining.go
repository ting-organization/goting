package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/types"
)

// Mining (reference node, wiki: node/mining): templates and solutions for
// external miners, a bounded development `mine`, and a built-in CPU miner
// that can be started and stopped while the node runs.
const (
	// ProducerMax bounds producer data: a block may hold more, but this
	// keeps it sensible.
	ProducerMax = 4096
	// FutureLimit is how far ahead of the local clock a block's timestamp
	// may be before nodes defer it (rules §9.1, admission policy).
	FutureLimit = 7200 * time.Second
	// nonceSlice is the nonces searched per thread before taking a fresh
	// template, so the timestamp moves on and new transactions get in.
	nonceSlice = 1 << 24
)

// Producer returns the producer data for blocks this node builds.
func (n *Node) Producer() []byte {
	n.minerMu.Lock()
	defer n.minerMu.Unlock()
	return n.producer
}

// Votes returns this node's capacity votes.
func (n *Node) Votes() [4]uint8 {
	n.minerMu.Lock()
	defer n.minerMu.Unlock()
	return n.votes
}

// SetProducer changes the producer data; templates built before are
// dropped.
func (n *Node) SetProducer(p []byte) error {
	if len(p) > ProducerMax {
		return fmt.Errorf("producer data is at most %d bytes", ProducerMax)
	}
	n.minerMu.Lock()
	n.producer = append([]byte(nil), p...)
	n.minerMu.Unlock()
	n.clearTemplates()
	return nil
}

// ErrVotes refuses votes other than four of 0 (hold), 1 (increase) or 2
// (decrease).
var ErrVotes = errors.New("votes must be four of 0 (hold), 1 (increase) or 2 (decrease)")

// SetVotes changes the capacity votes; templates built before are dropped.
func (n *Node) SetVotes(v [4]uint8) error {
	for _, x := range v {
		if x > 2 {
			return ErrVotes
		}
	}
	n.minerMu.Lock()
	n.votes = v
	n.minerMu.Unlock()
	n.clearTemplates()
	return nil
}

func (n *Node) clearTemplates() {
	n.tx.mu.Lock()
	n.templates = nil
	n.tx.mu.Unlock()
}

// TemplateJSON describes a template for an external miner: the header to
// search, where its nonce lies, and the target it must meet.
func (n *Node) TemplateJSON(t *miner.Template) map[string]any {
	txs := make([]hashing.Hash, len(t.Body.Transactions))
	for i, raw := range t.Body.Transactions {
		txs[i] = types.TransactionID(n.Chain.ChainID(), raw)
	}
	return map[string]any{"id": TemplateID(t), "height": t.Header.Height, "parent": t.Header.ParentID,
		"timestamp": t.Header.Timestamp, "header_hex": hex.EncodeToString(t.Header.Encode()),
		"bytes": len(t.Block().Encode()), "used": t.Header.ResourcesUsed, "transactions": txs,
		"mining": map[string]any{"nonce_offset": types.NonceOffset, "nonce_bytes": 16, "nonce_endian": "little",
			"target_hex": targetHex(t.Header.Target)}}
}

// ParseNonce reads a u128 nonce in decimal.
func ParseNonce(s string) (types.Nonce, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() < 0 || v.BitLen() > 128 {
		return types.Nonce{}, errors.New("expected nonce")
	}
	lo := new(big.Int).And(v, new(big.Int).SetUint64(^uint64(0)))
	return types.Nonce{Lo: lo.Uint64(), Hi: new(big.Int).Rsh(v, 64).Uint64()}, nil
}

// SolutionJSON imports a solved template's block and describes it.
func (n *Node) SolutionJSON(id hashing.Hash, nonce types.Nonce) (map[string]any, error) {
	e, err := n.Solution(id, nonce)
	known := errors.Is(err, chain.ErrKnown)
	if err != nil && !known {
		return nil, err
	}
	return map[string]any{"id": e.ID, "known": known, "canonical": n.Chain.IsCanonical(e),
		"height": n.Chain.Tip().Height}, nil
}

// Mine is development mining in one call: up to count blocks, each from a
// fresh template, searching at most attempts nonces for each.
func (n *Node) Mine(ctx context.Context, count, attempts uint64) (map[string]any, error) {
	if count < 1 || count > 100 || attempts < 1 || attempts > 10_000_000 {
		return nil, errors.New("count must be 1..100 and attempts 1..10000000")
	}
	blocks := []any{}
	stale := 0
	for uint64(len(blocks)) < count {
		tpl, err := n.Template(nil)
		if err != nil {
			return nil, err
		}
		h := *tpl.Header
		if _, err := miner.SolveN(ctx, &h, 0, attempts); err != nil {
			if ctx.Err() != nil {
				return nil, errors.New("node is stopping")
			}
			return map[string]any{"blocks": blocks, "exhausted": true}, nil
		}
		out, err := n.SolutionJSON(TemplateID(tpl), h.Nonce)
		if errors.Is(err, ErrExpiredTemplate) && stale < 10 {
			stale++
			continue
		}
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, out)
	}
	return map[string]any{"blocks": blocks, "exhausted": false}, nil
}

// builtinMiner is the loop between templates and the nonce search.
type builtinMiner struct {
	cancel  context.CancelFunc
	done    chan struct{}
	threads int
	spacing time.Duration

	mu        sync.Mutex
	since     uint64
	templates uint64
	attempts  atomic.Uint64
	blocks    uint64
	height    uint64
	lastError *string
}

// StartMiner starts the built-in miner with threads search goroutines (0:
// all cores) and at least spacing between its own blocks, replacing one
// that runs.
func (n *Node) StartMiner(threads int, spacing time.Duration) {
	n.StopMiner()
	ctxp := n.runCtx.Load()
	if ctxp == nil {
		return
	}
	ctx, cancel := context.WithCancel(*ctxp)
	m := &builtinMiner{cancel: cancel, done: make(chan struct{}), threads: threads, spacing: spacing, since: now()}
	n.minerMu.Lock()
	n.running = m
	n.minerMu.Unlock()
	go func() {
		defer close(m.done)
		n.mineLoop(ctx, m)
	}()
}

// StopMiner stops the built-in miner and waits for it.
func (n *Node) StopMiner() {
	n.minerMu.Lock()
	m := n.running
	n.running = nil
	n.minerMu.Unlock()
	if m != nil {
		m.cancel()
		<-m.done
	}
}

// MinerStatus describes the built-in miner and the mining settings.
func (n *Node) MinerStatus() map[string]any {
	n.minerMu.Lock()
	m := n.running
	producer, votes := n.producer, n.votes
	n.minerMu.Unlock()
	var out map[string]any
	if m == nil {
		out = map[string]any{"running": false, "threads": n.cfg.Workers, "spacing": uint64(n.cfg.Interval.Seconds())}
	} else {
		m.mu.Lock()
		out = map[string]any{"running": true, "search": fmt.Sprintf("cpu, %d threads", max(m.threads, 1)),
			"spacing": uint64(m.spacing.Seconds()), "since": m.since, "templates": m.templates,
			"attempts": m.attempts.Load(), "blocks": m.blocks, "height": m.height, "last_error": m.lastError}
		m.mu.Unlock()
	}
	out["producer_hex"] = hex.EncodeToString(producer)
	out["votes"] = votes
	return out
}

func (m *builtinMiner) fail(err string) {
	m.mu.Lock()
	m.lastError = &err
	m.mu.Unlock()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// mineLoop fetches a template, searches a slice of nonces on it and hands a
// solution back, over and over: waiting out the spacing since its own last
// canonical block, while the tip is more than a block behind the best
// header, and while the next timestamp would be too far ahead for peers.
func (n *Node) mineLoop(ctx context.Context, m *builtinMiner) {
	var lastBlock time.Time
	var start uint64
	threads := m.threads
	for ctx.Err() == nil {
		if m.spacing > 0 && !lastBlock.IsZero() && time.Since(lastBlock) < m.spacing {
			if !sleepCtx(ctx, min(200*time.Millisecond, m.spacing-time.Since(lastBlock))) {
				return
			}
			continue
		}
		tip := n.Chain.Tip()
		if _, _, best := n.Chain.Counts(); best > tip.Height+1 {
			m.fail(fmt.Sprintf("waiting: the node is %d blocks behind its best header", best-tip.Height))
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		_, median, _, _ := n.Chain.NextContext(tip)
		if ahead := time.Unix(int64(median)+1, 0).Sub(time.Now().Add(FutureLimit)); ahead > 0 {
			m.fail("waiting: the next timestamp would be too far ahead of the clock")
			if !sleepCtx(ctx, min(ahead, time.Second)) {
				return
			}
			continue
		}
		tpl, err := n.Template(nil)
		if err != nil {
			m.fail(err.Error())
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		m.mu.Lock()
		m.templates++
		m.height = tpl.Header.Height
		m.mu.Unlock()
		// The slice ends early when the tip moves.
		sctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-n.tipChanged:
				cancel()
			case <-sctx.Done():
			}
		}()
		// Each slice starts where the last left off, in case the template
		// came out the same.
		h := *tpl.Header
		h.Nonce = types.Nonce{Lo: start}
		slice := uint64(max(threads, 1)) * nonceSlice
		start += slice
		_, err = miner.SolveN(sctx, &h, threads, slice)
		cancel()
		if err != nil {
			m.attempts.Add(slice)
			m.mu.Lock()
			m.lastError = nil
			m.mu.Unlock()
			continue
		}
		out, err := n.SolutionJSON(TemplateID(tpl), h.Nonce)
		if err != nil {
			m.fail(err.Error())
			if !sleepCtx(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		m.mu.Lock()
		if out["canonical"] == true {
			m.blocks++
			lastBlock = time.Now()
		}
		m.lastError = nil
		m.mu.Unlock()
	}
}
