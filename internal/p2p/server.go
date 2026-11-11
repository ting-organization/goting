package p2p

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"runtime/debug"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ting-organization/goting/internal/hashing"
)

// Backend is what a server needs from its node.
type Backend interface {
	ChainID() hashing.Hash
	// State returns the node's state for a hello or state answer.
	State() *State
	// Headers returns up to 2,000 canonical headers after the highest
	// locator entry on the canonical chain, as raw 294-byte headers.
	Headers(locator []hashing.Hash) ([][]byte, error)
	// Block returns a block the node has validated, or ErrNotFound.
	Block(id hashing.Hash) ([]byte, error)
	// CheckHeader checks the header of a block a peer starts pushing:
	// whether the block is stored already, or the data capacity that bounds
	// it. An *InvalidData error proves the peer broke the rules.
	CheckHeader(header []byte, from netip.AddrPort) (known bool, id hashing.Hash, max uint64, err error)
	// Banned reports whether an address's range is banned; Punish bans it
	// for sending invalid data.
	Banned(addr netip.Addr) bool
	Punish(addr netip.Addr)
	// Import validates a pushed block. A returned *InvalidData proves the
	// peer broke the rules; ErrHeld says the block was checked and is kept
	// in memory until its branch overtakes the tip, which the peer is told
	// as taken.
	Import(raw []byte, from netip.AddrPort) (id hashing.Hash, known bool, err error)
	// TxBound is the data capacity of the next block.
	TxBound() uint64
	Transaction(id hashing.Hash) ([]byte, error)
	Submit(raw []byte, notBefore *uint64, from netip.AddrPort) (id hashing.Hash, known bool, err error)
	Inventory(deferred bool, after *hashing.Hash) ([]hashing.Hash, *hashing.Hash)
	Items(deferred bool, ids []hashing.Hash) []Item
	// Addresses lists addresses to share with a peer at asker, never its
	// own announced listening address.
	Addresses(asker netip.Addr, own *netip.AddrPort) []netip.AddrPort
	// Subscribe returns announcements; cancel stops them.
	Subscribe() (ch <-chan *Announcement, cancel func())
	// Learned reports a peer's listening address from its hello.
	Learned(addr netip.AddrPort)
}

// ErrHeld is returned by Import for a block held, not stored.
var ErrHeld = errors.New("block held")

// connCloser is a Backend that is told when an inbound connection ends, to
// let go what it held for it.
type connCloser interface {
	Closed(from netip.AddrPort)
}

// ErrNotFound answers a request for something the node does not serve.
var ErrNotFound = errors.New("not found")

// InvalidData is an answer proving the asking side sent data that breaks
// the consensus rules; the server then disconnects and bans the address.
type InvalidData struct{ Text string }

func (e *InvalidData) Error() string { return e.Text }

// Server accepts connections from peers and answers them.
type Server struct {
	Backend Backend
	Log     *slog.Logger
	Nonce   uint64
	Agent   string
	// MaxInbound bounds connections; MaxPerRange bounds those from one
	// IPv4 address or IPv6 /64.
	MaxInbound  int
	MaxPerRange int
	// Panicked, when set, is told of a panic recovered in a connection's
	// handling, with its stack.
	Panicked func(task string, v any, stack []byte)

	mu    sync.Mutex
	conns map[*serverConn]struct{}
	// next numbers connections in the order they were taken; deliveries
	// counts the new blocks inbound peers delivered, by which the latest
	// deliveries are known.
	next       uint64
	deliveries uint64
	// took is when each network group last took a connection's place, while
	// that is less than settle ago.
	took map[string]time.Time
}

type serverConn struct {
	nc    net.Conn
	addr  netip.AddrPort
	rng   string
	net   string
	local bool
	key   uint64
	since time.Time
	// block is when the peer last delivered a new block, as the count of
	// deliveries then; 0 if never.
	block uint64
	wmu   sync.Mutex
	// listen is the listening address the peer announced in its hello.
	listen *netip.AddrPort
}

// Inbound policy (reference node, wiki: node/sync).
const (
	// protectedDeliverers is how many of the latest peers to deliver a new
	// block keep their place in a full node whatever else.
	protectedDeliverers = 4
	// settle is how long a new connection in a full node keeps its place,
	// so that a new node has the time to say hello and ask for addresses;
	// and how long the network group of one that took another's place takes
	// no other's, so that one address or a few networks cannot keep the
	// newest place changing hands.
	settle = 10 * time.Second
)

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// Serve accepts connections until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// Out of files or memory for a moment: wait rather than end
			// the listener, and with it every new peer, for good.
			if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOBUFS) ||
				errors.Is(err, syscall.ENOMEM) || errors.Is(err, syscall.ECONNABORTED) {
				s.log().Warn("accepting a peer connection", "err", err)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
				continue
			}
			return err
		}
		ap, _ := netip.ParseAddrPort(nc.RemoteAddr().String())
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		sc := newServerConn(nc, ap, time.Now())
		if !s.admit(sc) {
			nc.Close()
			continue
		}
		go func() {
			defer s.drop(sc)
			// A fault in one connection's handling ends that connection,
			// not the node.
			defer func() {
				if v := recover(); v != nil {
					s.panicked("inbound peer "+sc.addr.String(), v)
				}
			}()
			s.handle(ctx, sc)
		}()
	}
}

func (s *Server) panicked(task string, v any) {
	if s.Panicked != nil {
		s.Panicked(task, v, debug.Stack())
		return
	}
	s.log().Error("a peer task panicked", "task", task, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
}

func (s *Server) init() {
	if s.MaxInbound == 0 {
		s.MaxInbound = 125
	}
	if s.MaxPerRange == 0 {
		s.MaxPerRange = 8
	}
	if s.conns == nil {
		s.conns = make(map[*serverConn]struct{})
		s.took = make(map[string]time.Time)
	}
}

func newServerConn(nc net.Conn, ap netip.AddrPort, now time.Time) *serverConn {
	return &serverConn{nc: nc, addr: ap, rng: rangeOf(ap.Addr()), net: networkOf(ap.Addr()),
		local: local(ap.Addr()), since: now}
}

// rangeOf names an IPv4 address or IPv6 /64.
func rangeOf(a netip.Addr) string {
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}

// networkOf names what inbound connections give way by: a public address's
// network group (an IPv4 /16, an IPv6 /32), and a local one's range.
func networkOf(a netip.Addr) string {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || !a.IsGlobalUnicast() {
		return rangeOf(a)
	}
	bits := 32
	if a.Is4() {
		if o := a.As4(); o[0] == 100 && o[1]&0xc0 == 64 { // carrier-grade NAT
			return rangeOf(a)
		}
		bits = 16
	}
	p, _ := a.Prefix(bits)
	return p.String()
}

func local(a netip.Addr) bool { return a.IsLoopback() }

// settled reports whether another connection may take c's place at now.
// This machine's own connections are its operator's, and need no time.
func (c *serverConn) settled(now time.Time) bool {
	return c.local || now.Sub(c.since) >= settle
}

// admit applies bans and connection limits: a connection is taken under its
// range's share, and with every slot taken, in place of a settled one,
// unless its network group took another's place less than settle ago. That
// is the newest connection of the network holding the most, if that network
// holds at least two more than the newcomer's, so that many addresses of one
// network cannot hold every slot; otherwise the one least worth keeping. A
// connection from this machine is held to neither the share nor the wait.
func (s *Server) admit(sc *serverConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if s.Backend.Banned(sc.addr.Addr()) {
		return false
	}
	ranged := 0
	for c := range s.conns {
		if c.rng == sc.rng {
			ranged++
		}
	}
	if ranged >= s.MaxPerRange && !sc.local {
		return false
	}
	if len(s.conns) >= s.MaxInbound {
		now := sc.since
		for n, at := range s.took {
			if now.Sub(at) >= settle {
				delete(s.took, n)
			}
		}
		if _, ok := s.took[sc.net]; ok && !sc.local {
			return false
		}
		leaving := s.crowded(sc, now)
		if leaving == nil {
			leaving = s.leastUseful(sc.local, now)
		}
		if leaving == nil {
			return false
		}
		leaving.nc.Close()
		delete(s.conns, leaving)
		if !sc.local {
			s.took[sc.net] = now
		}
	}
	s.next++
	sc.key = s.next
	s.conns[sc] = struct{}{}
	return true
}

// crowded is the newest settled connection of the network holding the most,
// if that network holds at least two more than the newcomer's. Called with
// s.mu held.
func (s *Server) crowded(sc *serverConn, now time.Time) *serverConn {
	held := map[string]int{}
	for c := range s.conns {
		held[c.net]++
	}
	var largest string
	most := 0
	for n, k := range held {
		if k > most || (k == most && n > largest) {
			largest, most = n, k
		}
	}
	if most <= held[sc.net]+1 {
		return nil
	}
	var newest *serverConn
	for c := range s.conns {
		if c.net == largest && c.settled(now) && (newest == nil || c.key > newest.key) {
			newest = c
		}
	}
	return newest
}

// leastUseful is the connection least worth keeping, after Bitcoin's
// eviction: this machine's own (unless the newcomer is local too), the
// latest to deliver new blocks, and of the rest the longest connected half
// are kept; of the settled others the newest of the network holding the most
// gives way, and of networks holding as many, the one whose newest is
// newest. None while every connection is kept. Called with s.mu held.
func (s *Server) leastUseful(newcomerLocal bool, now time.Time) *serverConn {
	var rest []*serverConn
	for c := range s.conns {
		if newcomerLocal || !c.local {
			rest = append(rest, c)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].key < rest[j].key }) // oldest first
	latest := make([]*serverConn, 0, len(rest))
	for _, c := range rest {
		if c.block > 0 {
			latest = append(latest, c)
		}
	}
	sort.Slice(latest, func(i, j int) bool { return latest[i].block > latest[j].block })
	if len(latest) > protectedDeliverers {
		latest = latest[:protectedDeliverers]
	}
	kept := map[*serverConn]bool{}
	for _, c := range latest {
		kept[c] = true
	}
	others := rest[:0:0]
	for _, c := range rest {
		if !kept[c] {
			others = append(others, c)
		}
	}
	type group struct {
		count  int
		newest *serverConn
	}
	held := map[string]*group{}
	for _, c := range others[len(others)/2:] {
		if c.settled(now) {
			g := held[c.net]
			if g == nil {
				g = &group{}
				held[c.net] = g
			}
			g.count++
			g.newest = c // others is oldest first
		}
	}
	var worst *group
	for _, g := range held {
		if worst == nil || g.count > worst.count || (g.count == worst.count && g.newest.key > worst.newest.key) {
			worst = g
		}
	}
	if worst == nil {
		return nil
	}
	return worst.newest
}

// delivered records that sc delivered a block this node did not have. A
// transaction counts for nothing: one is pooled without being executed, so
// delivering one costs nothing.
func (s *Server) delivered(sc *serverConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries++
	sc.block = s.deliveries
}

func (s *Server) drop(sc *serverConn) {
	sc.nc.Close()
	if b, ok := s.Backend.(connCloser); ok {
		b.Closed(sc.addr)
	}
	s.mu.Lock()
	delete(s.conns, sc)
	s.mu.Unlock()
}

// ban bans the peer's range and ends its open connections; an exempt peer
// (configured or local) loses only this connection.
func (s *Server) ban(sc *serverConn) {
	s.Backend.Punish(sc.addr.Addr())
	if s.Backend.Banned(sc.addr.Addr()) {
		s.CloseRange(sc.addr.Addr())
	}
	sc.nc.Close()
}

// CloseRange ends every inbound connection from a's range.
func (s *Server) CloseRange(a netip.Addr) {
	r := rangeOf(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		if c.rng == r {
			c.nc.Close()
		}
	}
}

// Inbound returns the number of inbound connections.
func (s *Server) Inbound() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (sc *serverConn) write(p []byte) error {
	sc.wmu.Lock()
	defer sc.wmu.Unlock()
	sc.nc.SetWriteDeadline(time.Now().Add(FrameTimeout))
	return WriteFrame(sc.nc, p)
}

// maxRequest bounds a request's frame: the largest carries one slice.
const maxRequest = MaxSlice + 4<<10

// readRequest waits up to the idle limit for a frame's length, then five
// seconds for its bytes.
func readRequest(nc net.Conn, max int, idle time.Duration) ([]byte, error) {
	nc.SetReadDeadline(time.Now().Add(idle))
	var lb [4]byte
	if _, err := io.ReadFull(nc, lb[:]); err != nil {
		return nil, err
	}
	nc.SetReadDeadline(time.Now().Add(FrameTimeout))
	return readBody(nc, int(binary.LittleEndian.Uint32(lb[:])), max)
}

// bucket is a token bucket of request units: 200 a second after a burst
// of 2,000. Requests beyond it wait rather than fail.
type bucket struct {
	tokens float64
	last   time.Time
}

const (
	unitsPerSecond = 200
	unitsBurst     = 2000
)

func (b *bucket) take(ctx context.Context, n float64) error {
	for {
		now := time.Now()
		b.tokens = math.Min(unitsBurst, b.tokens+now.Sub(b.last).Seconds()*unitsPerSecond)
		b.last = now
		if b.tokens >= n {
			b.tokens -= n
			return nil
		}
		wait := time.Duration((n - b.tokens) / unitsPerSecond * float64(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// push accumulates a sliced import or submit.
type push struct {
	tag      byte
	total    uint64
	buf      []byte
	deadline time.Time
	notBefor *uint64
}

func (s *Server) handle(ctx context.Context, sc *serverConn) {
	// Hello: the first frame, whole within five seconds, at most 4 KiB.
	sc.nc.SetReadDeadline(time.Now().Add(FrameTimeout))
	p, err := ReadFrame(sc.nc, MaxHelloFrame)
	if err != nil {
		return
	}
	m, err := DecodeRequest(p)
	if err != nil {
		return // no message: the connection ends without an answer
	}
	hello, ok := m.(*Hello)
	if !ok || hello.Version < 1 || hello.ChainID != s.Backend.ChainID() {
		sc.write(EncodeError("peer version or genesis mismatch"))
		return
	}
	if hello.Nonce == s.Nonce {
		sc.write(EncodeError("connected to itself"))
		return
	}
	if hello.Listen != nil {
		l := netip.AddrPortFrom(sc.addr.Addr(), *hello.Listen)
		sc.listen = &l
		s.Backend.Learned(l)
	}
	if err := sc.write(Encode(s.Backend.State())); err != nil {
		return
	}
	s.log().Debug("peer connected", "addr", sc.addr.String(), "agent", hello.Agent)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if hello.Announce {
		ch, stop := s.Backend.Subscribe()
		defer stop()
		go func() {
			defer func() {
				if v := recover(); v != nil {
					s.panicked("announcements to "+sc.addr.String(), v)
					cancel()
				}
			}()
			for {
				select {
				case <-cctx.Done():
					return
				case a, ok := <-ch:
					if !ok {
						return
					}
					if sc.write(EncodeAnnouncement(a)) != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	b := &bucket{tokens: unitsBurst, last: time.Now()}
	var pending *push
	for cctx.Err() == nil {
		p, err := readRequest(sc.nc, maxRequest, IdleLimit)
		if err != nil {
			return
		}
		m, err := DecodeRequest(p)
		if err != nil {
			return // no message: the connection ends without an answer
		}
		if err := b.take(cctx, cost(m)); err != nil {
			return
		}
		ans, closeAfter, ban := s.answer(sc, m, &pending)
		if err := sc.write(ans); err != nil {
			return
		}
		if extra := sliceCost(m, ans); extra > 0 {
			if err := b.take(cctx, extra); err != nil {
				return
			}
		}
		if ban {
			s.ban(sc)
		}
		if closeAfter {
			return
		}
	}
}

// cost is a request's units before it is answered.
func cost(m Message) float64 {
	switch r := m.(type) {
	case *HeadersRequest:
		return 8
	case *StateRequest, *InventoryRequest, *PendingRequest, *DeferredRequest:
		return 2
	case *Import:
		return 1 + carried(len(r.Bytes))
	case *Submit:
		return 1 + carried(len(r.Bytes))
	}
	return 1
}

// carried is what carrying n bytes of a block or transaction adds.
func carried(n int) float64 { return float64((n + (64 << 10) - 1) / (64 << 10)) }

// sliceCost is what an answer adds once it is written: the bytes of a raw
// or transaction_raw slice, and of the transactions a pending or deferred
// answer carries, which count as a block's slices do. Without it a peer
// could draw megabytes for the two units of the request.
func sliceCost(m Message, ans []byte) float64 {
	switch m.(type) {
	case *RawRequest, *TransactionRawRequest:
		if a, err := DecodeAnswer(ans); err == nil {
			if sl, ok := a.(*Slice); ok {
				return carried(len(sl.Bytes))
			}
		}
	case *PendingRequest, *DeferredRequest:
		return carried(len(ans))
	}
	return 0
}

func errAnswer(format string, args ...any) []byte { return EncodeError(fmt.Sprintf(format, args...)) }

// answer handles one request. It returns the answer frame, whether to close
// the connection after it, and whether to ban the peer.
func (s *Server) answer(sc *serverConn, m Message, pending **push) ([]byte, bool, bool) {
	be := s.Backend
	switch r := m.(type) {
	case *Hello:
		return EncodeError("unknown peer method"), true, false
	case *StateRequest:
		return Encode(be.State()), false, false
	case *HeadersRequest:
		hs, err := be.Headers(r.Locator)
		if err != nil {
			return EncodeError(err.Error()), false, false
		}
		out := &enc{b: []byte{KindAnswer, AnsHeaders}}
		out.u16(uint16(len(hs)))
		for _, h := range hs {
			out.b = append(out.b, h...)
		}
		return out.b, false, false
	case *RawRequest:
		raw, err := be.Block(r.ID)
		if err != nil {
			return EncodeError("block not found"), false, false
		}
		return sliceAnswer(raw, r.Offset, r.Length), false, false
	case *TransactionRawRequest:
		raw, err := be.Transaction(r.ID)
		if err != nil {
			return EncodeError("transaction not found"), false, false
		}
		return sliceAnswer(raw, r.Offset, r.Length), false, false
	case *Import:
		return s.pushSlice(sc, TagImport, r.Offset, r.Total, r.Bytes, nil, pending)
	case *Submit:
		return s.pushSlice(sc, TagSubmit, r.Offset, r.Total, r.Bytes, r.NotBefore, pending)
	case *InventoryRequest:
		ids, next := be.Inventory(r.Deferred, r.After)
		return Encode(&Inventory{IDs: ids, Next: next}), false, false
	case *PendingRequest:
		return Encode(&Items{Items: be.Items(false, r.IDs)}), false, false
	case *DeferredRequest:
		return Encode(&Items{Items: be.Items(true, r.IDs)}), false, false
	case *AddressesRequest:
		addrs := be.Addresses(sc.addr.Addr(), sc.listen)
		if len(addrs) > MaxAddresses {
			addrs = addrs[:MaxAddresses]
		}
		return Encode(&Addresses{Addrs: addrs}), false, false
	}
	return EncodeError("unknown peer method"), true, false
}

func sliceAnswer(raw []byte, offset uint64, length uint32) []byte {
	total := uint64(len(raw))
	if offset > total {
		return EncodeError("slice offset out of range")
	}
	end := offset + uint64(length)
	if end > total {
		end = total
	}
	return Encode(&Slice{Offset: offset, Total: total, Bytes: raw[offset:end]})
}

// pushSlice takes an import or submit as the reference node does: an item
// whole at offset 0 is taken at once; otherwise the first slice sets the
// size (a block's header, which it holds, bounds it by the data capacity at
// its height; a transaction is bounded by the next block's), and the rest
// follow in order within the item's transfer time.
func (s *Server) pushSlice(sc *serverConn, tag byte, offset, total uint64, b []byte, notBefore *uint64, pending **push) ([]byte, bool, bool) {
	be := s.Backend
	answer := func(raw []byte) ([]byte, bool, bool) {
		var id hashing.Hash
		var known bool
		var err error
		if tag == TagImport {
			if len(raw) < 294 {
				return EncodeError("invalid block: truncated header"), true, true
			}
			id, known, err = be.Import(raw, sc.addr)
			switch {
			case errors.Is(err, ErrHeld):
				// Held blocks do not count as delivered: holding one
				// executes nothing.
				err = nil
			case err == nil && !known:
				s.delivered(sc)
			}
		} else {
			id, known, err = be.Submit(raw, notBefore, sc.addr)
		}
		if err != nil {
			var inv *InvalidData
			if errors.As(err, &inv) {
				return EncodeError(inv.Text), true, true
			}
			return EncodeError(err.Error()), false, false
		}
		return Encode(&Taken{ID: id, Known: known}), false, false
	}
	if offset == 0 && uint64(len(b)) == total {
		*pending = nil
		return answer(b)
	}
	if offset == 0 {
		*pending = nil
		if tag == TagImport {
			if len(b) < 294 {
				return EncodeError("invalid block: truncated header"), true, true
			}
			known, id, max, err := be.CheckHeader(b[:294], sc.addr)
			if err != nil {
				var inv *InvalidData
				if errors.As(err, &inv) {
					return EncodeError(inv.Text), true, true
				}
				return EncodeError(err.Error()), false, false
			}
			if known {
				return Encode(&Taken{ID: id, Known: true}), false, false
			}
			if total > max {
				return EncodeError("invalid block: larger than its capacity"), true, true
			}
		} else if total > be.TxBound() {
			return EncodeError("transaction larger than the data capacity"), false, false
		}
		*pending = &push{tag: tag, total: total, buf: make([]byte, 0, min(total, 1<<20)), notBefor: notBefore,
			deadline: time.Now().Add(transferTime(total))}
	}
	p := *pending
	if p == nil {
		return EncodeError("nothing being pushed"), false, false
	}
	end := offset + uint64(len(b))
	if p.tag != tag || offset != uint64(len(p.buf)) || total != p.total || end > total || time.Now().After(p.deadline) {
		*pending = nil
		return EncodeError("slice out of order"), false, false
	}
	if end < total && len(b) == 0 {
		*pending = nil
		return EncodeError("empty slice"), false, false
	}
	p.buf = append(p.buf, b...)
	if end < total {
		return Encode(&Received{Bytes: end}), false, false
	}
	*pending = nil
	return answer(p.buf)
}

// transferTime is a minute, and ten seconds more per MiB.
func transferTime(n uint64) time.Duration {
	return time.Minute + time.Duration((n+(1<<20)-1)>>20)*10*time.Second
}
