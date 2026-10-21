package p2p

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// SeedPin is a network's last seed BlockId. Headers name their parents, so
// it determines the BlockId at every seed height. Built-in networks carry
// their whole list; a pin serves networks that publish only its end.
type SeedPin struct {
	Height uint64
	ID     hashing.Hash
}

// ErrPeerInvalid reports that a peer sent data breaking the consensus rules.
var ErrPeerInvalid = errors.New("peer sent invalid data")

// headerPages is how many pages of headers one round asks for.
const headerPages = 8

// Syncer follows a peer's chain as the reference node does (wiki:
// node/sync): headers past the best common block, page after page, each
// checked as it arrives; then the blocks the chain wants of this peer along
// the best header's branch, which other peers' syncers fetch beside it.
type Syncer struct {
	Chain *chain.Chain
	Log   *slog.Logger
	Pin   *SeedPin
	// Peer names this connection to the chain's download scheduler.
	Peer string
	// OnBlock, when set, sees every block this syncer stores.
	OnBlock func(e *chain.Entry, raw []byte)
	// next is where the next headers request continues.
	next *hashing.Hash
}

func (s *Syncer) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Syncer) peer() string {
	if s.Peer == "" {
		return "peer"
	}
	return s.Peer
}

// Run syncs from c until ctx ends or the connection fails, asking again on
// every announcement and every 30 seconds, and straight away while pages
// remain.
func (s *Syncer) Run(ctx context.Context, c *Conn) error {
	t := time.NewTicker(KeepAlive)
	defer t.Stop()
	for {
		more, _, err := s.round(ctx, c)
		if err != nil {
			return err
		}
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.Done():
			return c.Err()
		case <-c.Announcements:
		case <-t.C:
		}
	}
}

// SyncOnce runs rounds until the peer has nothing new. It returns the number
// of blocks stored.
func (s *Syncer) SyncOnce(ctx context.Context, c *Conn) (int, error) {
	total := 0
	for {
		more, n, err := s.round(ctx, c)
		total += n
		if err != nil {
			return total, err
		}
		if !more && n == 0 {
			return total, nil
		}
	}
}

// round asks for headers, then fetches the blocks the chain wants. It
// reports whether more header pages wait.
func (s *Syncer) round(ctx context.Context, c *Conn) (bool, int, error) {
	more, err := s.headers(ctx, c)
	if err != nil {
		return false, 0, err
	}
	n, err := s.blocks(ctx, c)
	return more, n, err
}

func (s *Syncer) headers(ctx context.Context, c *Conn) (bool, error) {
	for page := 0; page < headerPages; page++ {
		locator := s.Chain.Locator()
		if s.next != nil {
			locator = []hashing.Hash{*s.next}
		}
		m, err := c.Do(ctx, &HeadersRequest{Locator: locator})
		var ea *ErrorAnswer
		if errors.As(err, &ea) && ea.Text == "no common ancestor" && s.next != nil {
			// The peer moved off the header its answer was to follow.
			s.next = nil
			continue
		}
		if err != nil {
			return false, fmt.Errorf("headers: %w", err)
		}
		hs, ok := m.(*Headers)
		if !ok {
			return false, fmt.Errorf("%w: headers answered with another message", ErrNotMessage)
		}
		if len(hs.Headers) == 0 {
			s.next = nil
			return false, nil
		}
		if err := s.pinSeed(hs); err != nil {
			return false, err
		}
		res, err := s.Chain.HeadersFrom(s.peer(), hs.Headers)
		if err != nil {
			var sc *chain.SeedConformanceError
			if errors.As(err, &sc) {
				return false, err
			}
			if errors.Is(err, chain.ErrInvalid) {
				return false, fmt.Errorf("%w: %v", ErrPeerInvalid, err)
			}
			// Behind, ahead of the clock, not following, or a chain fetched
			// again that differs: the rest waits for the peer's next tip.
			s.log().Debug("header round ended", "peer", s.peer(), "err", err)
			s.next = nil
			return false, nil
		}
		full := len(hs.Headers) == MaxHeaders
		if res.Next != nil && (full || res.Refetch) {
			s.next = res.Next
		} else {
			s.next = nil
			return false, nil
		}
	}
	return true, nil
}

// pinSeed derives a seed list from headers that reach the pinned last seed
// block, for a network whose list is not built in.
func (s *Syncer) pinSeed(hs *Headers) error {
	if s.Pin == nil || len(s.Chain.Seed()) > 0 || len(hs.Headers) == 0 {
		return nil
	}
	first := hs.Headers[0].Height
	last := hs.Headers[len(hs.Headers)-1].Height
	if s.Pin.Height < first || s.Pin.Height > last || first != 1 {
		return nil
	}
	list := make([]hashing.Hash, s.Pin.Height)
	for i := range list {
		list[i] = types.HeaderIDFromBytes(hs.Raw[i])
		if i > 0 && hs.Headers[i].ParentID != list[i-1] {
			return fmt.Errorf("%w: headers do not link", ErrPeerInvalid)
		}
	}
	if list[len(list)-1] != s.Pin.ID {
		return fmt.Errorf("%w: block at seed height %d is %s, not the pinned %s",
			ErrPeerInvalid, s.Pin.Height, list[len(list)-1], s.Pin.ID)
	}
	if err := s.Chain.SetSeed(list); err != nil {
		return err
	}
	s.log().Info("seed list pinned", "length", len(list), "last", s.Pin.ID.String())
	return nil
}

type download struct {
	id    hashing.Hash
	max   uint64
	ans   <-chan Result
	start time.Time
}

// blocks fetches what the chain wants of this peer, as long as it wants
// more, with up to MaxPipeline slices asked at once.
func (s *Syncer) blocks(ctx context.Context, c *Conn) (int, error) {
	total := 0
	for {
		wanted := s.Chain.BlocksWanted(s.peer())
		if len(wanted) == 0 {
			return total, nil
		}
		var queue []download
		next := 0
		for next < len(wanted) || len(queue) > 0 {
			for next < len(wanted) && len(queue) < MaxPipeline-1 {
				w, err := c.Send(&RawRequest{ID: wanted[next].ID, Length: MaxSlice})
				if err != nil {
					return total, err
				}
				queue = append(queue, download{id: wanted[next].ID, max: wanted[next].Max, ans: w, start: time.Now()})
				next++
			}
			d := queue[0]
			queue = queue[1:]
			raw, err := s.collect(ctx, c, d)
			var ea *ErrorAnswer
			if errors.As(err, &ea) && ea.Text == "block not found" {
				// The peer does not have it: the others are asked, and this
				// one again after its next announcement.
				for _, q := range queue {
					c.Wait(ctx, q.ans)
				}
				s.Chain.Release(s.peer())
				return total, nil
			}
			if err != nil {
				return total, err
			}
			e, err := s.Chain.ProcessBlockFrom(raw, s.Peer)
			if err != nil && !errors.Is(err, chain.ErrKnown) {
				var sc *chain.SeedConformanceError
				if errors.As(err, &sc) {
					return total, err
				}
				if errors.Is(err, chain.ErrInvalid) {
					return total, fmt.Errorf("%w: %v", ErrPeerInvalid, err)
				}
				s.log().Debug("block not taken", "peer", s.peer(), "id", d.id.String(), "err", err)
				continue
			}
			total++
			if s.OnBlock != nil && e != nil {
				s.OnBlock(e, raw)
			}
		}
	}
}

// collect gathers a block's slices: the first answer, then further raw
// requests while bytes remain, within the block's transfer time.
func (s *Syncer) collect(ctx context.Context, c *Conn, d download) ([]byte, error) {
	m, err := c.Wait(ctx, d.ans)
	if err != nil {
		return nil, err
	}
	sl, ok := m.(*Slice)
	if !ok || sl.Offset != 0 {
		return nil, fmt.Errorf("%w: raw answered with another message", ErrNotMessage)
	}
	if sl.Total > d.max {
		return nil, fmt.Errorf("%w: block %s of %d bytes exceeds its capacity", ErrPeerInvalid, d.id, sl.Total)
	}
	if sl.Total < types.HeaderSize || uint64(len(sl.Bytes)) > sl.Total || len(sl.Bytes) < types.HeaderSize ||
		types.HeaderIDFromBytes(sl.Bytes[:types.HeaderSize]) != d.id {
		return nil, fmt.Errorf("%w: slice is not block %s", ErrPeerInvalid, d.id)
	}
	deadline := d.start.Add(chain.TransferTime(d.max))
	raw := append(make([]byte, 0, sl.Total), sl.Bytes...)
	for uint64(len(raw)) < sl.Total {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("block %s: transfer time exceeded", d.id)
		}
		rest := sl.Total - uint64(len(raw))
		m, err := c.Do(ctx, &RawRequest{ID: d.id, Offset: uint64(len(raw)), Length: uint32(min(rest, MaxSlice))})
		if err != nil {
			return nil, err
		}
		more, ok := m.(*Slice)
		if !ok || more.Offset != uint64(len(raw)) || more.Total != sl.Total || len(more.Bytes) == 0 ||
			uint64(len(more.Bytes)) > rest {
			return nil, fmt.Errorf("%w: bad continuation slice of %s", ErrPeerInvalid, d.id)
		}
		raw = append(raw, more.Bytes...)
	}
	return raw, nil
}
