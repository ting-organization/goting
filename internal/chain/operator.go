package chain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"

	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/hashing"
)

// ErrMarked refuses a block this node's operator marked invalid, or a block
// above one. It is policy: the peer sending it is not at fault.
var ErrMarked = errors.New("block marked invalid by this node's operator")

// ErrGenesisMark refuses marking the genesis block.
var ErrGenesisMark = errors.New("the genesis block cannot be marked invalid")

type marksFile struct {
	Version int      `json:"version"`
	Marks   []string `json:"marks"`
}

// loadMarks reads the operator's marks. A missing file holds none; one that
// does not parse is an error, since dropping a mark would let the node
// follow a branch its operator refused.
func (c *Chain) loadMarks() error {
	c.marks = map[hashing.Hash]struct{}{}
	if c.cfg.MarksPath == "" {
		return nil
	}
	b, err := os.ReadFile(c.cfg.MarksPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f marksFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 {
		return fmt.Errorf("%s: not a marks file", c.cfg.MarksPath)
	}
	for _, s := range f.Marks {
		id, err := hashing.ParseHash(s)
		if err != nil {
			return fmt.Errorf("%s: %v", c.cfg.MarksPath, err)
		}
		c.marks[id] = struct{}{}
	}
	return nil
}

func (c *Chain) saveMarks() error {
	if c.cfg.MarksPath == "" {
		return nil
	}
	f := marksFile{Version: 1, Marks: []string{}}
	for id := range c.marks {
		f.Marks = append(f.Marks, id.String())
	}
	sort.Strings(f.Marks)
	b, _ := json.MarshalIndent(f, "", "  ")
	tmp := c.cfg.MarksPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if fh, err := os.Open(tmp); err == nil {
		fh.Sync()
		fh.Close()
	}
	if err := os.Rename(tmp, c.cfg.MarksPath); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(c.cfg.MarksPath)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// byOperator reports whether id is marked, or lies above a marked block on
// a branch that is invalid from it on. Called with c.mu held.
func (c *Chain) byOperator(id hashing.Hash) bool {
	if _, ok := c.marks[id]; ok {
		return true
	}
	for e := c.entries[id]; e != nil && e.Status == StatusInvalid; e = e.Parent {
		if _, ok := c.marks[e.ID]; ok {
			return true
		}
	}
	return false
}

// byOperatorParent reports whether a header with this parent lies above a
// marked block. Called with c.mu held.
func (c *Chain) byOperatorParent(parent hashing.Hash) bool {
	p, ok := c.entries[parent]
	return ok && p.Status == StatusInvalid && c.byOperator(parent)
}

// Marks lists the blocks the operator marked invalid.
func (c *Chain) Marks() []hashing.Hash {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]hashing.Hash, 0, len(c.marks))
	for id := range c.marks {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// MarkInvalid marks a block invalid on the operator's word (reference node,
// wiki: node/storage): the node leaves it and every block above it, never
// connects them, and refuses them from peers without holding that against
// the peers. A block not yet seen is refused when it comes. The mark is
// kept across restarts. It reports whether the block was indexed.
func (c *Chain) MarkInvalid(id hashing.Hash) (bool, error) {
	if id == c.genesisID {
		return false, ErrGenesisMark
	}
	c.processMu.Lock()
	defer c.processMu.Unlock()
	c.mu.Lock()
	c.marks[id] = struct{}{}
	if err := c.saveMarks(); err != nil {
		delete(c.marks, id)
		c.mu.Unlock()
		return false, err
	}
	e, known := c.entries[id]
	if known && e.Status != StatusInvalid {
		for _, n := range c.markInvalid(e, ErrMarked) {
			delete(c.delivered, n.ID)
		}
	}
	tip := c.tip
	c.mu.Unlock()
	// Leave the marked branch: back to the highest valid block below it.
	if tip.Status == StatusInvalid {
		back := tip
		for back.Status == StatusInvalid {
			back = back.Parent
		}
		c.publish(c.setTip(back))
		// A tip below the database's base takes the base down with it.
		if c.base != nil && c.base.Height > back.Height {
			if err := c.rewind(back); err != nil {
				return known, c.storageFail(err)
			}
		}
	}
	return known, c.activate()
}

// Reconsider clears the operator's marks on a block and on the blocks
// below it, and every invalid status on its branch: the block, every block
// above it, and the invalid blocks below it. Each clearing mark clears the
// branches above it too. The best branch is then connected again, and a
// block that fails again is invalid again. It reports how many blocks were
// cleared.
func (c *Chain) Reconsider(id hashing.Hash) (int, error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	c.mu.Lock()
	_, known := c.entries[id]
	_, marked := c.marks[id]
	if !known && !marked {
		c.mu.Unlock()
		return 0, ErrNotFound
	}
	cleared := c.clearInvalid(id)
	unmarked := map[hashing.Hash]struct{}{}
	for _, e := range append(cleared, c.entries[id]) {
		if e == nil {
			continue
		}
		if _, ok := c.marks[e.ID]; ok {
			unmarked[e.ID] = struct{}{}
		}
	}
	if marked {
		unmarked[id] = struct{}{}
	}
	for m := range unmarked {
		cleared = append(cleared, c.clearInvalid(m)...)
		delete(c.marks, m)
	}
	err := c.saveMarks()
	seen := map[*Entry]bool{}
	n := 0
	for _, e := range cleared {
		if !seen[e] {
			seen[e] = true
			n++
		}
	}
	c.failedWork = new(big.Int)
	c.mu.Unlock()
	if err != nil {
		return n, err
	}
	// Stored blocks a mark kept out of the index come back.
	if _, err := c.indexStored(); err != nil {
		return n, err
	}
	return n, c.activate()
}

// clearInvalid clears the invalid status of id's invalid ancestors, of id
// and of its descendants, lowest first, and returns them. Called with c.mu
// held.
func (c *Chain) clearInvalid(id hashing.Hash) []*Entry {
	e, ok := c.entries[id]
	if !ok {
		return nil
	}
	var cleared []*Entry
	for p := e.Parent; p != nil && p.Status == StatusInvalid; p = p.Parent {
		cleared = append(cleared, p)
	}
	stack := []*Entry{e}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.Status == StatusInvalid {
			cleared = append(cleared, n)
		}
		stack = append(stack, n.children...)
	}
	sort.Slice(cleared, func(i, j int) bool { return cleared[i].Height < cleared[j].Height })
	for _, n := range cleared {
		delete(c.invalid, n.ID)
		if c.store.Has(n.ID) {
			n.Status = StatusStored
		} else {
			n.Status = StatusHeader
		}
	}
	for _, n := range cleared {
		if n.HasData() {
			c.markComplete(n)
		}
	}
	c.recomputeBest()
	return cleared
}

// Status diagnostics (reference node, wiki: node/storage).
const (
	// DeepReorg is how many replaced blocks make a reorganization deep.
	DeepReorg = 6
	// ForkWarning is how many blocks of work a failed branch must carry
	// beyond the tip to raise a warning.
	ForkWarning = 6
)

// Reorganization describes the last time the tip moved off its branch.
type Reorganization struct {
	Replaced   uint64       `json:"replaced"`
	ForkHeight uint64       `json:"fork_height"`
	Height     uint64       `json:"height"`
	Tip        hashing.Hash `json:"tip"`
	Time       uint64       `json:"time"`
	Deep       bool         `json:"deep"`
}

// LastReorganization returns the last reorganization, if any.
func (c *Chain) LastReorganization() *Reorganization {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.reorg == nil {
		return nil
	}
	r := *c.reorg
	return &r
}

// failedBranch records the work of a branch that failed validation. Called
// with c.mu held.
func (c *Chain) failedBranch(work *big.Int) {
	if work.Cmp(c.failedWork) > 0 {
		c.failedWork = new(big.Int).Set(work)
	}
}

// ForkWarning warns when a branch that failed validation here carries more
// than ForkWarning blocks of work beyond the tip: this node, or much of the
// network, follows other rules.
func (c *Chain) ForkWarning() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	block := consensus.Work(c.tip.Target)
	limit := new(big.Int).Add(c.tip.ChainWork, new(big.Int).Mul(block, big.NewInt(ForkWarning)))
	if c.failedWork.Cmp(limit) <= 0 {
		return ""
	}
	ahead := new(big.Int).Div(new(big.Int).Sub(c.failedWork, c.tip.ChainWork), block)
	return fmt.Sprintf("a branch about %s blocks heavier than this node's chain failed validation here; "+
		"this node or much of the network follows other rules: check for an update", ahead)
}

// Counts returns how many headers the index holds, how many blocks are
// stored, and the best header's height.
func (c *Chain) Counts() (headers, stored int, best uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries), c.store.Len(), c.best.Height
}

// HasBlock reports whether the block's bytes are stored.
func (c *Chain) HasBlock(id hashing.Hash) bool { return c.store.Has(id) }
