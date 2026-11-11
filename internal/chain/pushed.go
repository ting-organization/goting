package chain

import (
	"errors"
	"math/big"

	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// Blocks a peer pushes unasked (reference node, wiki: node/sync).
//
// Every child of one block has the same target, so a sibling of the tip
// costs its sender one block of work, and its body need be valid only as far
// as its roots: stored as it came, with nothing executing it unless its
// branch overtook the tip, such blocks would fill the disk and the index of
// any node a miner can connect to, with no ban to follow.
//
// So a pushed block is stored only when the node stores its whole branch
// and, with it, the branch overtakes the tip by the fork-choice margin: it
// is then executed at once, and connected or found invalid and held against
// its sender. Any other pushed block whose parent the node stores with its
// whole branch is checked, header and body, and held in memory, in one run
// per connection of blocks that each follow the one before; the pusher is
// answered as if it were taken and goes on. The block that makes the run
// overtake the tip stores the run and connects it. An honest pusher pushes
// only when its tip has more work than this node's, from the block the two
// share, in order: exactly such a run. What is not kept comes back by its
// headers from the peers the node follows.
const (
	// heldPerPeer bounds one connection's run, heldMax all of them; the
	// largest other run gives way.
	heldPerPeer = 16 << 20
	heldMax     = 64 << 20
)

// ErrHeld answers a pushed block that was checked and is held in memory
// until its branch overtakes the tip.
var ErrHeld = errors.New("block held until its branch overtakes the tip")

// ErrLetGo answers the child of a run that was let go to make room for
// others. It is no policy refusal, on purpose: the pushing node then ends
// its connection and, connected again, pushes the branch from the block
// both nodes hold.
var ErrLetGo = errors.New("the blocks this connection pushed were let go to make room; push them again from the block both nodes hold")

type heldBlock struct {
	id  hashing.Hash
	h   *types.Header
	raw []byte
}

// heldRun is the blocks one connection pushed that wait for their branch to
// overtake the tip: each the child of the one before, the first a child of
// start, a block the node stores with its whole branch.
type heldRun struct {
	start  *Entry
	blocks []heldBlock
	bytes  int
	walk   *walk // past the last block
	// gone is the last block of the run this connection last lost, and
	// goneErr what its child is answered.
	gone    hashing.Hash
	goneErr error
}

func (r *heldRun) find(id hashing.Hash) int {
	for i := range r.blocks {
		if r.blocks[i].id == id {
			return i
		}
	}
	return -1
}

// ProcessPushed takes a block a peer pushed unasked. It returns ErrHeld for
// a block held in the connection's run, and otherwise what ProcessBlockFrom
// returns.
func (c *Chain) ProcessPushed(raw []byte, peer string) (*Entry, error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	if c.broken != nil {
		return nil, c.broken
	}
	if len(raw) < types.HeaderSize {
		return nil, &InvalidError{Reason: "truncated header"}
	}
	id := types.HeaderIDFromBytes(raw[:types.HeaderSize])
	h, _ := types.DecodeHeader(raw[:types.HeaderSize])
	c.mu.RLock()
	_, indexed := c.entries[id]
	parent, parentIndexed := c.entries[h.ParentID]
	whole := parentIndexed && parent.complete && parent.Status != StatusInvalid
	tip := c.tip
	run := c.runs[peer]
	c.mu.RUnlock()
	// A block whose header the node keeps is one it would ask for: stored
	// as a download is.
	if indexed {
		return c.process(raw, peer)
	}
	if run != nil && run.find(id) >= 0 {
		return nil, ErrKnown
	}
	if whole {
		work := consensus.Work(h.Target)
		chainWork := new(big.Int).Add(parent.ChainWork, work)
		if isAncestor(tip, parent) || exceeds(chainWork, work, tip.ChainWork) {
			// Executed at once.
			return c.process(raw, peer)
		}
		c.mu.RLock()
		w := c.walkFrom(parent)
		c.mu.RUnlock()
		return c.hold(peer, &heldRun{start: parent, walk: w}, h, id, raw)
	}
	if run != nil {
		if at := run.find(h.ParentID); at >= 0 {
			// The pusher moved within its run, or goes on from its end.
			if at != len(run.blocks)-1 {
				cut, err := c.cutRun(run, at+1)
				if err != nil {
					c.dropRun(peer, ErrBehind)
					return nil, ErrBehind
				}
				run = cut
			}
			return c.hold(peer, run, h, id, raw)
		}
		if run.goneErr != nil && run.gone == h.ParentID {
			return nil, run.goneErr
		}
	}
	if parentIndexed {
		// Held only as a header, or without its whole branch: the branch
		// comes by its headers.
		return nil, ErrBehind
	}
	return nil, ErrUnknownParent
}

// cutRun returns run cut back to its first n blocks, with a walk made
// anew. Called with processMu held.
func (c *Chain) cutRun(run *heldRun, n int) (*heldRun, error) {
	c.mu.RLock()
	w := c.walkFrom(run.start)
	c.mu.RUnlock()
	cut := &heldRun{start: run.start, walk: w, gone: run.gone, goneErr: run.goneErr}
	for _, b := range run.blocks[:n] {
		if _, err := c.step(cut.walk, b.h, b.id); err != nil {
			return nil, err
		}
		cut.blocks = append(cut.blocks, b)
		cut.bytes += len(b.raw)
	}
	return cut, nil
}

// checkPushed checks a pushed block against the run it would follow, as far
// as holding it needs: every header rule, along the run, and the body
// against the header. It moves w past the block. Called with processMu held.
func (c *Chain) checkPushed(w *walk, h *types.Header, id hashing.Hash, raw []byte) error {
	c.mu.RLock()
	reason, bad := c.invalid[id]
	_, marked := c.marks[id]
	tip := c.tip
	c.mu.RUnlock()
	if marked {
		return ErrMarked
	}
	if bad && !c.listedAs(h, id) {
		return &InvalidError{Height: h.Height, ID: id, Reason: "previously rejected: " + reason, Header: true}
	}
	vs, err := c.step(w, h, id)
	if err != nil {
		return c.fail(h, id, err)
	}
	if h.Timestamp > c.now()+ClockSlack {
		return ErrAhead
	}
	// Within a day's work of the tip, as every block the node takes whole.
	day := new(big.Int).Mul(consensus.Work(tip.Target), big.NewInt(ThresholdBlocks))
	if new(big.Int).Add(w.work, day).Cmp(tip.ChainWork) < 0 {
		return ErrBehind
	}
	if raw == nil {
		return nil
	}
	if _, err := checkBody(h, id, raw, vs.Limits[types.D]); err != nil {
		return err
	}
	return nil
}

// hold checks a pushed block as the next of run and keeps it there for the
// connection; when the run then overtakes the tip, the run is stored and
// connected. Called with processMu held.
func (c *Chain) hold(peer string, run *heldRun, h *types.Header, id hashing.Hash, raw []byte) (*Entry, error) {
	w := run.walk.clone()
	if err := c.checkPushed(w, h, id, raw); err != nil {
		return nil, err
	}
	if run.bytes+len(raw) > heldPerPeer {
		// Pushed whole once more the branch would be dropped again: it
		// comes by its headers. This block, pushed again, is answered so
		// too.
		c.setRun(peer, run)
		c.dropRun(peer, ErrBehind)
		return nil, ErrBehind
	}
	// The run may be the one the connection already has, which Held and
	// the count of all runs read under c.mu.
	kept := heldBlock{id: id, h: h, raw: append([]byte(nil), raw...)}
	c.mu.Lock()
	run.blocks = append(run.blocks, kept)
	run.bytes += len(raw)
	run.walk = w
	c.keepRun(peer, run)
	tipWork := c.tip.ChainWork
	c.mu.Unlock()
	if !exceeds(w.work, consensus.Work(h.Target), tipWork) {
		return nil, ErrHeld
	}
	return c.storeRun(peer, run)
}

// setRun is keepRun for a caller that holds processMu only.
func (c *Chain) setRun(peer string, run *heldRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keepRun(peer, run)
}

// keepRun makes run the connection's, and makes room among all runs: the
// largest other one gives way. A run is changed only with both locks held,
// so either lets it be read. Called with processMu and c.mu held.
func (c *Chain) keepRun(peer string, run *heldRun) {
	if old := c.runs[peer]; old != nil && run.goneErr == nil {
		run.gone, run.goneErr = old.gone, old.goneErr
	}
	c.runs[peer] = run
	c.countRuns()
	for c.runBytes > heldMax {
		var largest string
		most := -1
		for name, r := range c.runs {
			if name != peer && (r.bytes > most || (r.bytes == most && name < largest)) {
				largest, most = name, r.bytes
			}
		}
		if most <= 0 {
			return
		}
		c.letGo(largest, ErrLetGo)
	}
}

// countRuns sums what the runs hold. There are no more runs than inbound
// connections. Called with c.mu held.
func (c *Chain) countRuns() {
	c.runBytes = 0
	for _, r := range c.runs {
		c.runBytes += r.bytes
	}
}

// letGo empties a connection's run, remembering its last block and what
// its child is answered. Called with c.mu held.
func (c *Chain) letGo(peer string, answer error) {
	r := c.runs[peer]
	if r == nil {
		return
	}
	kept := &heldRun{start: r.start, goneErr: answer}
	if n := len(r.blocks); n > 0 {
		kept.gone = r.blocks[n-1].id
	} else {
		kept.gone, kept.goneErr = r.gone, r.goneErr
	}
	c.runs[peer] = kept
	c.countRuns()
}

func (c *Chain) dropRun(peer string, answer error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.letGo(peer, answer)
}

// ReleasePushed lets go what a connection that ended had pushed.
func (c *Chain) ReleasePushed(peer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.runs, peer)
	c.countRuns()
}

// Held is what the node holds of pushed blocks in memory: the connections
// with a run, the blocks in them, and their size.
func (c *Chain) Held() (runs, blocks, bytes int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, r := range c.runs {
		blocks += len(r.blocks)
	}
	return len(c.runs), blocks, c.runBytes
}

// storeRun stores a run that overtakes the tip, parents first, each block
// by the path every block whose parent is indexed takes, and connects it.
// What that path makes of a block decides; the check along the run only
// decided what to hold. Should the two differ, the fault is this node's:
// the run is dropped and refused as policy, and no peer is blamed. Called
// with processMu held.
func (c *Chain) storeRun(peer string, run *heldRun) (*Entry, error) {
	c.mu.Lock()
	delete(c.runs, peer)
	c.countRuns()
	c.mu.Unlock()
	var last *Entry
	known := false
	for _, b := range run.blocks {
		e, k, err := c.acceptBlock(b.raw, false)
		if err != nil {
			c.cfg.Log.Warn("a held run of pushed blocks was refused when stored; the fault is this node's",
				"peer", peer, "id", b.id.String(), "err", err)
			return nil, ErrBehind
		}
		if !k {
			c.mu.Lock()
			if e.Status == StatusStored && len(c.delivered) < maxDelivered {
				c.delivered[e.ID] = peer
			}
			c.mu.Unlock()
		}
		last, known = e, k
	}
	return c.settle(last, known)
}

// checkHeld is CheckHeaderFrom for a header that belongs with the run held
// for a connection: known if the run holds it; the data capacity that
// bounds it if its parent is a block of the run. ok is false when the
// header has nothing to do with the run. Called with processMu held.
func (c *Chain) checkHeld(h *types.Header, id hashing.Hash, peer string) (known bool, max uint64, ok bool, err error) {
	if peer == "" {
		return false, 0, false, nil
	}
	c.mu.RLock()
	run := c.runs[peer]
	_, indexed := c.entries[h.ParentID]
	c.mu.RUnlock()
	if run == nil || indexed {
		return false, 0, false, nil
	}
	if run.find(id) >= 0 {
		return true, 0, true, nil
	}
	at := run.find(h.ParentID)
	if at < 0 {
		if run.goneErr != nil && run.gone == h.ParentID {
			return false, 0, true, run.goneErr
		}
		return false, 0, false, nil
	}
	w := run.walk
	if at != len(run.blocks)-1 {
		cut, err := c.cutRun(run, at+1)
		if err != nil {
			return false, 0, true, ErrBehind
		}
		w = cut.walk
	}
	w = w.clone()
	if err := c.checkPushed(w, h, id, nil); err != nil {
		return false, 0, true, err
	}
	return false, w.vote.Limits[types.D], true, nil
}
