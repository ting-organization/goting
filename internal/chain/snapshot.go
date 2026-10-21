package chain

import (
	"errors"
	"fmt"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
)

// LoadFromSnapshot rebuilds the block tree from the store using a state
// snapshot this node wrote earlier. Blocks up to and including the snapshot
// block get every header rule and every body commitment checked, but are not
// re-executed: the snapshot's state, verified against the snapshot block's
// state root, stands in for that execution. Every later block is validated in
// full. A snapshot is the node's own record of blocks it validated before;
// it never comes from a peer.
func (c *Chain) LoadFromSnapshot(info state.SnapshotInfo, snap state.Tree) (int, error) {
	if snap.Root() != info.Root {
		return 0, errors.New("snapshot root does not match its records")
	}
	parents := make(map[hashing.Hash]hashing.Hash)
	err := c.store.Each(func(id hashing.Hash, raw []byte) error {
		if len(raw) >= types.HeaderSize {
			var p hashing.Hash
			copy(p[:], raw[42:74])
			parents[id] = p
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	trusted := make(map[hashing.Hash]bool)
	for id := info.Block; id != c.genesisID; {
		p, ok := parents[id]
		if !ok {
			return 0, fmt.Errorf("snapshot block %s or an ancestor is not in the store", id)
		}
		trusted[id] = true
		id = p
	}
	c.processMu.Lock()
	defer c.processMu.Unlock()
	n := 0
	err = c.store.Each(func(id hashing.Hash, raw []byte) error {
		e, _, err := c.acceptBlock(raw, true)
		switch {
		case err == nil:
			n++
		case errors.Is(err, ErrUnknownParent), errors.Is(err, ErrInvalid):
			return nil
		default:
			return fmt.Errorf("loading stored block %s: %w", id, err)
		}
		if !trusted[id] {
			return nil
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		e.Status = StatusValid
		if id == info.Block {
			if snap.Root() != e.Header.StateRoot {
				return fmt.Errorf("snapshot state root %s differs from block %s's %s", snap.Root(), id, e.Header.StateRoot)
			}
			st := snap
			e.state = &st
			c.retained[e] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	return n, c.activate()
}

// WriteSnapshot writes the state of the canonical block depth blocks below
// the tip, deep enough to be unlikely to reorganize. It returns the
// snapshot's description, or false when no suitable state is retained.
func (c *Chain) WriteSnapshot(path string, depth uint64) (state.SnapshotInfo, bool, error) {
	tip := c.Tip()
	if tip.Height <= depth {
		return state.SnapshotInfo{}, false, nil
	}
	e, ok := c.AtHeight(tip.Height - depth)
	if !ok {
		return state.SnapshotInfo{}, false, nil
	}
	st, err := c.StateAt(e)
	if err != nil {
		return state.SnapshotInfo{}, false, err
	}
	info := state.SnapshotInfo{Block: e.ID, Height: e.Height}
	if err := state.WriteSnapshot(path, info, st); err != nil {
		return info, false, err
	}
	info.Root = st.Root()
	info.Count = uint64(st.Len())
	return info, true, nil
}
