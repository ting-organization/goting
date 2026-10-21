package chain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/state"
)

// Deep reorganizations (reference node, wiki: node/storage): a branch that
// forks below the undo window needs undo data the database no longer holds.
// The chain then stays on its tip and, in the background, replays the
// base's ancestors from genesis into a temporary database next to its own,
// every state root checked on the way, keeping the undo data of each block
// above the fork. That undo data is copied into the database a step at a
// time, the undo floor is lowered, and the reorganization goes on as any
// other. There is no depth limit: the deeper the fork, the longer the
// replay. A restart during the replay starts it again.

const (
	// replayStep is how many blocks a replay executes between commits to
	// its temporary database.
	replayStep = 256
	// replayCopy is how many blocks' undo data one commit copies.
	replayCopy = 1024
	// replayRetry is how long after a failed replay another may start.
	replayRetry = time.Minute
)

// errReplaying reports a move that waits for a replay to rebuild undo data.
var errReplaying = errors.New("rebuilding undo data below the undo window")

// replay is a running rebuild of pruned undo data.
type replay struct {
	downTo uint64   // the fork's height: undo data is rebuilt above it
	blocks []*Entry // the base's ancestors from height 1 up
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
}

// unreadable reports a block a replay could not read.
type unreadable struct {
	entry *Entry
	err   error
}

func (u *unreadable) Error() string {
	return fmt.Sprintf("block %s at height %d cannot be read: %v", u.entry.ID, u.entry.Height, u.err)
}

func (c *Chain) replayDir() string {
	return filepath.Join(filepath.Dir(c.cfg.State.Path()), "replay")
}

// Replaying reports whether undo data below the undo window is being
// rebuilt.
func (c *Chain) Replaying() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.replay != nil
}

// Close stops a running replay and starts no more. A node calls it when it
// stops, before Settle; a restart starts the replay again if it is still
// needed.
func (c *Chain) Close() {
	c.processMu.Lock()
	c.closed = true
	c.mu.RLock()
	r := c.replay
	c.mu.RUnlock()
	c.processMu.Unlock()
	if r != nil {
		r.cancel()
		<-r.done
	}
}

// rewindTo moves the database's base down to to, an ancestor of it. When
// that needs undo data the database does not hold, it starts a replay
// instead and reports that the move waits. Called with processMu held.
func (c *Chain) rewindTo(to *Entry) (bool, error) {
	floor, err := c.cfg.State.UndoFloor()
	if err != nil {
		return false, err
	}
	if to.Height+1 < floor {
		c.startReplay(to, floor-1)
		return true, nil
	}
	err = c.rewind(to)
	var m *missingUndo
	if errors.As(err, &m) {
		c.startReplay(to, m.height)
		return true, nil
	}
	return false, err
}

// leaveBase moves the base down to the tip's branch when the tip does not
// descend from it, as an operator's mark leaves it. Called with processMu
// held.
func (c *Chain) leaveBase() error {
	c.mu.RLock()
	tip, base := c.tip, c.base
	c.mu.RUnlock()
	if base == nil || descends(tip, base) {
		return nil
	}
	if _, err := c.rewindTo(commonAncestor(tip, base)); err != nil {
		return c.storageFail(err)
	}
	return nil
}

// startReplay starts rebuilding the undo data of the base's ancestors above
// to, up to height upto. Only one replay runs at a time, and none starts
// within replayRetry of a failed one. Called with processMu held.
func (c *Chain) startReplay(to *Entry, upto uint64) {
	if c.replay != nil || c.closed || (!c.replayFailed.IsZero() && c.cfg.Clock().Sub(c.replayFailed) < replayRetry) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base.Height < upto || to.Height >= upto {
		return
	}
	blocks := make([]*Entry, upto)
	for n := c.base; n.Height > 0; n = n.Parent {
		if n.Height <= upto {
			blocks[n.Height-1] = n
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &replay{downTo: to.Height, blocks: blocks, dir: c.replayDir(), cancel: cancel, done: make(chan struct{})}
	c.replay = r
	c.cfg.Log.Warn("a branch forks below the undo window: replaying blocks from genesis to rebuild their undo data",
		"fork_height", to.Height, "blocks", upto)
	go func() {
		defer close(r.done)
		defer os.RemoveAll(r.dir)
		err := c.runReplay(ctx, r)
		c.endReplay(ctx, r, err)
	}()
}

// runReplay executes the replay's blocks from genesis into a temporary
// database, keeping the undo data of those above the fork, then copies that
// undo data into the database. The chain goes on meanwhile: the blocks
// replayed stay canonical, as nothing below the undo floor can be
// disconnected until the replay ends, and nothing is pruned.
func (c *Chain) runReplay(ctx context.Context, r *replay) (err error) {
	defer state.Recover(&err)
	if err := os.RemoveAll(r.dir); err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	tmp, err := db.Open(filepath.Join(r.dir, "chain.db"), c.chainID)
	if err != nil {
		return err
	}
	defer tmp.Close()
	stored := state.Empty
	t := stored
	keys := map[state.Key]struct{}{}
	var undo []db.BlockUndo
	for i, b := range r.blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, body, err := c.readBody(b)
		if errors.Is(err, ErrClosed) {
			return errStopped
		}
		if err != nil {
			return &unreadable{entry: b, err: err}
		}
		res, err := c.executeBody(t, b.Header, b.ID, raw, body, b.Vote.Limits)
		if err != nil {
			return fmt.Errorf("replaying block %s at height %d: %w", b.ID, b.Height, err)
		}
		if b.Height > r.downTo {
			undo = append(undo, undoFor(t, b, res.Changes))
		}
		for _, ch := range res.Changes {
			keys[ch.Key] = struct{}{}
		}
		t = res.State
		if (i+1)%replayStep == 0 || i == len(r.blocks)-1 {
			commit := c.commitFor(stored, t, keys, b)
			commit.Undo = undo
			if err := tmp.Commit(commit); err != nil {
				return err
			}
			stored = state.FromRoot(commit.Base.Root, int(commit.Base.Count), tmp.View())
			t, keys, undo = stored, map[state.Key]struct{}{}, nil
		}
	}
	var batch []db.BlockUndo
	for _, b := range r.blocks[r.downTo:] {
		if err := ctx.Err(); err != nil {
			return err
		}
		prev, ok, err := tmp.Undo(b.Height, b.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("the replay kept no undo data for block %s", b.ID)
		}
		batch = append(batch, db.BlockUndo{Height: b.Height, Block: b.ID, Prev: prev})
		if len(batch) == replayCopy {
			if err := c.copyUndo(batch); err != nil {
				return err
			}
			batch = nil
		}
	}
	return c.copyUndo(batch)
}

// errStopped ends a replay whose chain has stopped or failed.
var errStopped = errors.New("the chain has stopped")

// copyUndo stores a step of rebuilt undo data in the database, between the
// chain's other work.
func (c *Chain) copyUndo(batch []db.BlockUndo) error {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	if c.closed || c.broken != nil {
		return errStopped
	}
	if err := c.cfg.State.PutUndo(batch); err != nil {
		c.storageFail(err)
		return errStopped
	}
	return nil
}

// endReplay ends a replay: once its undo data is stored, the undo floor is
// lowered and the chain moves toward its best branch again. A replay that
// failed is tried again at a later move, a block it could not read once
// fetched again.
func (c *Chain) endReplay(ctx context.Context, r *replay, err error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	// Pruning resumes with the next commit, which follows the floor's.
	c.mu.Lock()
	c.replay = nil
	c.mu.Unlock()
	if c.closed || c.broken != nil || ctx.Err() != nil || errors.Is(err, errStopped) {
		return
	}
	if err != nil {
		var u *unreadable
		if errors.As(err, &u) {
			c.forgetBody(u.entry, u.err)
		}
		c.replayFailed = c.cfg.Clock()
		c.cfg.Log.Error("rebuilding undo data failed; it is tried again later", "err", err)
		return
	}
	if err := c.cfg.State.LowerUndoFloor(r.downTo + 1); err != nil {
		c.storageFail(err)
		return
	}
	c.cfg.Log.Info("undo data rebuilt; the reorganization goes on", "above_height", r.downTo)
	if err := c.leaveBase(); err != nil {
		return
	}
	if err := c.activate(); err != nil {
		c.cfg.Log.Warn("moving to the best branch after the replay", "err", err)
	}
}
