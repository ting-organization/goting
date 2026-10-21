package chain

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// Header synchronization (wiki: node/sync): headers from peers are indexed
// when their chain is within the work threshold; a chain below it is checked
// as it streams past by a pre-sync that keeps a salted bit every PERIOD
// headers, then fetched again from its fork and indexed once it reaches the
// threshold.

const (
	presyncPeriod = 600
	presyncBuffer = 14400
)

// walk carries what checking the next header of a branch needs: its
// ancestry, vote state and work.
type walk struct {
	height    uint64
	id        hashing.Hash
	timestamp uint64
	recent    []uint64 // newest first, at most 11, genesis included while fewer
	vote      consensus.VoteState
	tally     *consensus.Tally // of the epoch the last header belongs to
	work      *big.Int
}

// walkFrom starts a walk at an indexed entry. Called with c.mu held.
func (c *Chain) walkFrom(e *Entry) *walk {
	w := &walk{height: e.Height, id: e.ID, timestamp: e.Timestamp, vote: e.Vote,
		work: new(big.Int).Set(e.ChainWork), tally: consensus.NewTally()}
	for n := e; n != nil && len(w.recent) < consensus.MedianTimeSpan; n = n.Parent {
		w.recent = append(w.recent, n.Timestamp)
	}
	if e.Height > 0 {
		ep := consensus.Epoch(e.Height, c.genesis.VoteEpochLength)
		for n := e; n != nil && n.Height > 0 && consensus.Epoch(n.Height, c.genesis.VoteEpochLength) == ep; n = n.Parent {
			w.tally.Add(n.Votes, n.BlockWork)
		}
	}
	return w
}

func (w *walk) clone() *walk {
	c := *w
	c.recent = append([]uint64(nil), w.recent...)
	c.work = new(big.Int).Set(w.work)
	t := consensus.NewTally()
	t.W.Set(w.tally.W)
	for i := 0; i < 4; i++ {
		t.Up[i].Set(w.tally.Up[i])
		t.Down[i].Set(w.tally.Down[i])
	}
	c.tally = t
	return &c
}

// step checks the next header and moves past it.
func (c *Chain) step(w *walk, h *types.Header, id hashing.Hash) (consensus.VoteState, error) {
	closed := w.tally
	vs := consensus.NextVoteState(c.genesis, w.vote, h.Height, func() *consensus.Tally { return closed })
	a := ancestry{height: w.height, id: w.id, timestamp: w.timestamp,
		median: consensus.MedianTime(w.recent), vote: vs}
	if err := c.checkHeaderAt(a, h, id); err != nil {
		return vs, err
	}
	if h.Height == 1 || consensus.Epoch(h.Height, c.genesis.VoteEpochLength) != consensus.Epoch(w.height, c.genesis.VoteEpochLength) {
		w.tally = consensus.NewTally()
	}
	work := consensus.Work(h.Target)
	w.tally.Add(h.Votes, work)
	w.work.Add(w.work, work)
	w.recent = append([]uint64{h.Timestamp}, w.recent...)
	if len(w.recent) > consensus.MedianTimeSpan {
		w.recent = w.recent[:consensus.MedianTimeSpan]
	}
	w.height, w.id, w.timestamp, w.vote = h.Height, id, h.Timestamp, vs
	return vs, nil
}

// presyncState is one peer's header chain under check.
type presyncState struct {
	start      *Entry
	first      *walk
	walk       *walk
	last       hashing.Hash
	count      uint64
	salt       [32]byte
	offset     uint64
	bits       []bool
	most       uint64
	threshold  *big.Int
	refetching bool
	rwalk      *walk
	rcount     uint64
	rlast      hashing.Hash
	held       []*types.Header
}

func presyncBit(salt *[32]byte, id hashing.Hash) bool {
	return hashing.H("presync", salt[:], id[:])[0]&1 == 1
}

func (c *Chain) newPresync(start *Entry, threshold *big.Int) *presyncState {
	p := &presyncState{start: start, first: c.walkFrom(start), last: start.ID, threshold: threshold}
	p.walk = p.first.clone()
	rand.Read(p.salt[:])
	var ob [8]byte
	rand.Read(ob[:])
	p.offset = binary.LittleEndian.Uint64(ob[:]) % presyncPeriod
	now := c.now()
	if now+ClockSlack > start.Timestamp {
		p.most = (now+ClockSlack-start.Timestamp)*6 + presyncPeriod
	} else {
		p.most = presyncPeriod
	}
	return p
}

func (p *presyncState) follows(h *types.Header) bool {
	if p.refetching {
		return h.ParentID == p.rlast
	}
	return h.ParentID == p.last
}

func (p *presyncState) next() hashing.Hash {
	if p.refetching {
		return p.rlast
	}
	return p.last
}

var errRefetchDiffers = errors.New("the chain fetched again differs")

// HeaderResult says how to continue a header round.
type HeaderResult struct {
	// Next is the header the next request should follow, if any.
	Next *hashing.Hash
	// Refetch asks for the chain again from Next, as a pre-sync requires.
	Refetch bool
	Indexed int
}

// HeadersFrom takes a page of a peer's headers, in order. It indexes those
// within the work threshold and pre-syncs a chain below it. An error
// wrapping ErrInvalid proves the peer sent invalid data; ErrAhead and
// ErrBehind end the round without blame.
func (c *Chain) HeadersFrom(peer string, headers []*types.Header) (HeaderResult, error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	var res HeaderResult
	set := func(id hashing.Hash) { v := id; res.Next = &v }
	for _, h := range headers {
		id := h.ID()
		c.mu.RLock()
		ps := c.presync[peer]
		c.mu.RUnlock()
		if ps != nil && ps.follows(h) {
			if h.Timestamp > c.now()+ClockSlack {
				return res, nil
			}
			out, err := c.presyncAdd(peer, ps, h, id)
			if err != nil {
				c.mu.Lock()
				delete(c.presync, peer)
				c.mu.Unlock()
				return res, err
			}
			switch out {
			case presyncContinue:
				set(ps.next())
			case presyncRefetch:
				set(ps.start.ID)
				res.Refetch = true
				return res, nil
			case presyncIndex, presyncDone:
				for _, hh := range ps.held {
					if _, ok := c.Get(hh.ID()); !ok {
						if _, err := c.acceptHeader(hh, hh.ID(), false, true); err != nil {
							return res, err
						}
						res.Indexed++
					}
				}
				ps.held = nil
				set(ps.next())
				if out == presyncDone {
					c.mu.Lock()
					delete(c.presync, peer)
					c.mu.Unlock()
				}
			}
			continue
		}
		if ps != nil { // the peer moved on to another chain
			c.mu.Lock()
			delete(c.presync, peer)
			c.mu.Unlock()
		}
		if _, ok := c.Get(id); ok {
			set(id)
			continue
		}
		_, err := c.acceptHeader(h, id, false, false)
		switch {
		case err == nil:
			res.Indexed++
			set(id)
		case errors.Is(err, ErrBehind):
			c.mu.RLock()
			start := c.entries[h.ParentID]
			th := c.threshold()
			c.mu.RUnlock()
			c.mu.Lock()
			ps := c.newPresync(start, th)
			c.mu.Unlock()
			out, err := c.presyncAdd(peer, ps, h, id)
			if err != nil {
				return res, err
			}
			c.mu.Lock()
			c.presync[peer] = ps
			c.mu.Unlock()
			if out == presyncRefetch {
				set(ps.start.ID)
				res.Refetch = true
				return res, nil
			}
			set(ps.next())
		case errors.Is(err, ErrAhead), errors.Is(err, ErrUnknownParent):
			return res, nil
		default:
			return res, err
		}
	}
	return res, nil
}

type presyncOutcome int

const (
	presyncContinue presyncOutcome = iota
	presyncRefetch
	presyncIndex
	presyncDone
)

func (c *Chain) presyncAdd(peer string, p *presyncState, h *types.Header, id hashing.Hash) (presyncOutcome, error) {
	if !p.refetching {
		if _, err := c.step(p.walk, h, id); err != nil {
			return 0, err
		}
		if (p.count+p.offset)%presyncPeriod == 0 {
			p.bits = append(p.bits, presyncBit(&p.salt, id))
		}
		p.count++
		p.last = id
		if p.count > p.most {
			return 0, fmt.Errorf("more headers than the clock allows")
		}
		if p.walk.work.Cmp(p.threshold) >= 0 {
			p.refetching = true
			p.rwalk = p.first.clone()
			p.rcount = 0
			p.rlast = p.start.ID
			return presyncRefetch, nil
		}
		return presyncContinue, nil
	}
	if _, err := c.step(p.rwalk, h, id); err != nil {
		return 0, err
	}
	if (p.rcount+p.offset)%presyncPeriod == 0 {
		index := int((p.rcount + p.offset) / presyncPeriod)
		first := 0
		if p.offset != 0 {
			first = 1
		}
		if i := index - first; i >= 0 && i < len(p.bits) && p.bits[i] != presyncBit(&p.salt, id) {
			return 0, errRefetchDiffers
		}
	}
	p.rcount++
	p.rlast = id
	p.held = append(p.held, h)
	done := p.rcount >= p.count
	if done && p.rlast != p.last {
		return 0, errRefetchDiffers
	}
	if done {
		return presyncDone, nil
	}
	if len(p.held) >= presyncBuffer {
		return presyncIndex, nil
	}
	return presyncContinue, nil
}

// Block download scheduling (wiki: node/sync).
const (
	downloadWindow = 1024
	perPeer        = 64
	lapsedWait     = 120 * time.Second
)

// TransferTime is how long a block or transaction of at most capacity bytes
// may take: a minute, and ten seconds more per MiB.
func TransferTime(capacity uint64) time.Duration {
	return time.Minute + time.Duration((capacity+(1<<20)-1)>>20)*10*time.Second
}

// Wanted is a block for a peer to fetch, with its most bytes.
type Wanted struct {
	ID  hashing.Hash
	Max uint64
}

// BlocksWanted returns blocks along the best header's branch for peer to
// fetch: from the lowest one not yet stored, within the window, at most 64
// asked of this peer at once, none asked of two peers at once until its
// transfer time passes.
func (c *Chain) BlocksWanted(peer string) []Wanted {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Clock()
	for id, f := range c.inFlight {
		e := c.entries[id]
		if e == nil || now.Sub(f.since) > TransferTime(e.Vote.Limits[types.D]) {
			delete(c.inFlight, id)
			c.expired[id] = flight{peer: f.peer, since: now}
		}
	}
	for id, f := range c.expired {
		if now.Sub(f.since) > lapsedWait {
			delete(c.expired, id)
		}
	}
	c.advanceFrontier()
	var out []Wanted
	asked := 0
	for i := c.frontier; i < len(c.bestPath) && i < c.frontier+downloadWindow && asked < perPeer; i++ {
		e := c.bestPath[i]
		if e.HasData() || e.Status == StatusInvalid {
			continue
		}
		if f, ok := c.expired[e.ID]; ok && f.peer == peer {
			continue
		}
		if f, ok := c.inFlight[e.ID]; ok && f.peer != peer {
			continue
		}
		c.inFlight[e.ID] = flight{peer: peer, since: now}
		out = append(out, Wanted{ID: e.ID, Max: e.Vote.Limits[types.D]})
		asked++
	}
	return out
}

// Release returns a peer's blocks in flight to the others, as when it
// answers that it does not have one.
func (c *Chain) Release(peer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, f := range c.inFlight {
		if f.peer == peer {
			delete(c.inFlight, id)
		}
	}
	delete(c.presync, peer)
}
