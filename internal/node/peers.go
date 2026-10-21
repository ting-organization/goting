package node

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/p2p"
	"github.com/ting-organization/goting/internal/types"
)

// PeerConfig configures the peer protocol.
type PeerConfig struct {
	// Listen is the address for inbound peers; empty means none.
	Listen string
	// Connect lists configured peers: always connected, never banned.
	Connect []string
	// Seeds are a network's seed nodes, looked up at start and learned as
	// discovered addresses when discovery is on.
	Seeds []string
	// Discovery shares and uses learned addresses.
	Discovery bool
	// MaxPeers bounds outbound connections, configured ones included (the
	// reference's max_peers, 8).
	MaxPeers int
	// Agent is sent in hellos.
	Agent string
	// SeedPin pins the network's last seed block, if it has one.
	SeedPin *p2p.SeedPin
	// BookPath persists the address book (peers.json); empty: none.
	BookPath string
}

// Intervals of the connection manager; variables for tests.
var (
	askLooking     = 10 * time.Second
	askInterval    = 600 * time.Second
	manageInterval = 2 * time.Second
)

// ProductionSeeds are the production network's seed nodes: a node with
// discovery on looks them up as it starts and learns their addresses.
var ProductionSeeds = []string{"2.29.97.212:18442"}

// Policy (reference node, wiki: node/sync).
const (
	MaxConfigured    = 32
	dayOfBlocks      = 1440
	announceInterval = 200 * time.Millisecond
	maxImportPush    = 32
	staleTip         = 600 // seconds without a new tip
	probeInterval    = 120 // seconds between probes
	markInterval     = 60  // seconds between book updates of one connection
	seedLookup       = 5 * time.Second
)

type session struct {
	addr       netip.AddrPort
	name       string
	configured bool
	cancel     context.CancelFunc
	connected  uint64
	conn       atomic.Pointer[p2p.Conn]
	lastBlock  atomic.Uint64 // when the peer last delivered a new block
	marked     atomic.Uint64 // when the book last recorded a round
	success    atomic.Uint64 // when a round last completed
	height     atomic.Uint64 // the peer's height, as it last said
}

// PeerStatus is a configured or connected peer as the control channel's
// peers method describes it (the reference's fields).
type PeerStatus struct {
	Address     netip.AddrPort `json:"address"`
	Source      string         `json:"source"`
	Agent       *string        `json:"agent,omitempty"`
	Connected   *uint64        `json:"connected"`
	LastSuccess *uint64        `json:"last_success"`
	Height      *uint64        `json:"height,omitempty"`
	LastBlock   *uint64        `json:"last_block,omitempty"`
	Error       *string        `json:"error"`
	LastAttempt *uint64        `json:"last_attempt,omitempty"`
}

type ended struct {
	s   *session
	err error
}

// Peers runs the node's side of the peer protocol.
type Peers struct {
	n      *Node
	cfg    PeerConfig
	nonce  uint64
	server *p2p.Server
	book   *AddressBook

	configured []netip.AddrPort
	looking    atomic.Bool // fewer connections than MaxPeers

	mu       sync.Mutex
	sessions map[netip.AddrPort]*session
	// ends records how the last connection to an address ended.
	ends     map[netip.AddrPort]PeerStatus
	selves   map[netip.AddrPort]bool // addresses that reached this node itself
	subs     map[chan *p2p.Announcement]struct{}
	sent     map[string]map[hashing.Hash]bool
	listenAt *uint16
	poolSubs map[chan struct{}]struct{}
	ready    chan struct{}
}

// NewPeers prepares the peer protocol for n.
func NewPeers(n *Node, cfg PeerConfig) *Peers {
	var nb [8]byte
	rand.Read(nb[:])
	if cfg.Agent == "" {
		cfg.Agent = "goting/" + Version
	}
	if cfg.MaxPeers <= 0 {
		cfg.MaxPeers = 8
	}
	p := &Peers{n: n, cfg: cfg, nonce: binary.LittleEndian.Uint64(nb[:]),
		book: NewAddressBook(cfg.BookPath), sessions: map[netip.AddrPort]*session{}, ends: map[netip.AddrPort]PeerStatus{},
		selves: map[netip.AddrPort]bool{}, subs: map[chan *p2p.Announcement]struct{}{},
		sent: map[string]map[hashing.Hash]bool{}, poolSubs: map[chan struct{}]struct{}{},
		ready: make(chan struct{})}
	for _, s := range cfg.Connect {
		if len(p.configured) == MaxConfigured {
			n.log.Warn("ignoring configured peers beyond the limit", "limit", MaxConfigured)
			break
		}
		ap, err := resolve(s, 0)
		if err != nil {
			n.log.Warn("configured peer does not resolve", "peer", s, "err", err)
			continue
		}
		p.configured = append(p.configured, ap)
	}
	p.book.Configure(p.configured)
	if cfg.Discovery {
		for _, s := range cfg.Seeds {
			// A seed name that does not resolve in time is passed over.
			if ap, err := resolve(s, seedLookup); err == nil {
				p.book.Add(ap, SourceSeed)
			} else {
				n.log.Debug("seed node does not resolve", "seed", s, "err", err)
			}
		}
	}
	p.looking.Store(true)
	n.tx.changed = p.changed
	n.peers = p
	return p
}

func resolve(s string, timeout time.Duration) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return canon(ap), nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return netip.AddrPort{}, err
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if len(ips) == 0 {
		return netip.AddrPort{}, fmt.Errorf("%s: no address", host)
	}
	pn, err := net.LookupPort("tcp", port)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return canon(netip.AddrPortFrom(ips[0], uint16(pn))), nil
}

// Ready is closed once the listener (if any) is open.
func (p *Peers) Ready() <-chan struct{} { return p.ready }

// ListenPort returns the inbound port, once Ready.
func (p *Peers) ListenPort() (uint16, bool) {
	<-p.ready
	if p.listenAt == nil {
		return 0, false
	}
	return *p.listenAt, true
}

// Nonce is this run's nonce.
func (p *Peers) Nonce() uint64 { return p.nonce }

// Book returns the address book.
func (p *Peers) Book() *AddressBook { return p.book }

// changed tells every pool watcher that the pool changed.
func (p *Peers) changed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.poolSubs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// watchPool returns a channel signalled on pool changes.
func (p *Peers) watchPool() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.poolSubs[ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.poolSubs, ch)
		p.mu.Unlock()
	}
}

// Run serves inbound peers and keeps outbound connections until ctx ends.
func (p *Peers) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	if p.cfg.Listen != "" {
		ln, err := net.Listen("tcp", p.cfg.Listen)
		if err != nil {
			close(p.ready)
			return err
		}
		if ap, err := netip.ParseAddrPort(ln.Addr().String()); err == nil {
			port := ap.Port()
			p.listenAt = &port
		}
		p.server = &p2p.Server{Backend: p, Log: p.n.log, Nonce: p.nonce, Agent: p.cfg.Agent}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.server.Serve(ctx, ln); err != nil {
				p.n.log.Error("peer server", "err", err)
			}
		}()
		p.n.log.Info("peer protocol listening", "addr", ln.Addr().String())
	}
	close(p.ready)
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.announceLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		p.manage(ctx)
	}()
	wg.Wait()
	if err := p.book.Save(); err != nil {
		p.n.log.Warn("peer address book", "err", err)
	}
	return nil
}

// own reports whether a is this node's own listening address.
func (p *Peers) own(a netip.AddrPort) bool {
	if p.listenAt == nil || a.Port() != *p.listenAt {
		return false
	}
	ip := a.Addr().Unmap()
	return ip.IsLoopback() || ip.IsUnspecified()
}

// manage keeps the outbound connections, checking every two seconds: every
// configured peer, and discovered ones up to MaxPeers in all (one more
// while the tip is stale), each from its own network group.
func (p *Peers) manage(ctx context.Context) {
	var wg sync.WaitGroup
	done := make(chan ended, 64)
	probes := make(chan ended, 1)
	probing := false
	var lastProbe uint64
	tip, tipSince := p.n.Chain.Tip().ID, now()
	t := time.NewTicker(manageInterval)
	defer t.Stop()
	for {
		// Connections that ended.
	drain:
		for {
			select {
			case d := <-done:
				p.ended(d.s, d.err)
			case d := <-probes:
				probing = false
				switch {
				case d.err == nil:
					p.book.Succeeded(d.s.addr)
				case errors.Is(d.err, p2p.ErrSelf):
					p.mu.Lock()
					p.selves[d.s.addr] = true
					p.mu.Unlock()
					p.book.Forget(d.s.addr)
				default:
					p.book.Failed(d.s.addr)
				}
			default:
				break drain
			}
		}
		// Peers whose blocks failed once stored sent invalid data too.
		for _, name := range p.n.Chain.Offenders() {
			if ap, err := netip.ParseAddrPort(name); err == nil {
				p.n.log.Warn("peer delivered an invalid block", "peer", name)
				p.Punish(ap.Addr())
				p.mu.Lock()
				if s, ok := p.sessions[canon(ap)]; ok {
					s.cancel()
				}
				p.mu.Unlock()
			}
		}
		t0 := now()
		if id := p.n.Chain.Tip().ID; id != tip {
			tip, tipSince = id, t0
		}
		stale := t0 >= tipSince+staleTip
		p.mu.Lock()
		var configured []netip.AddrPort
		isConfigured := map[netip.AddrPort]bool{}
		for _, a := range p.configured {
			if !p.own(a) && !p.selves[a] {
				configured = append(configured, a)
				isConfigured[a] = true
			}
		}
		want := 0
		if p.cfg.Discovery {
			want = max(p.cfg.MaxPeers-len(configured), 0)
			if stale {
				want++
			}
		}
		// Connections that should end: to a banned range, or discovered
		// peers beyond those wanted, the ones whose last new block came
		// longest ago first.
		var discovered []*session
		for _, s := range p.sessions {
			if (!s.configured && p.book.Banned(s.addr.Addr())) || s.configured != isConfigured[s.addr] {
				s.cancel()
				continue
			}
			if !s.configured {
				discovered = append(discovered, s)
			}
		}
		if len(discovered) > want {
			sort.Slice(discovered, func(i, j int) bool {
				bi, bj := discovered[i].lastBlock.Load(), discovered[j].lastBlock.Load()
				if bi != bj {
					return bi < bj
				}
				return discovered[i].connected > discovered[j].connected
			})
			for _, s := range discovered[:len(discovered)-want] {
				s.cancel()
			}
			discovered = discovered[len(discovered)-want:]
		}
		// New connections: configured peers not connected, and discovered
		// ones up to the number wanted, each when due after a failure.
		for _, a := range configured {
			if _, on := p.sessions[a]; !on && p.book.Due(a) {
				p.startLocked(ctx, &wg, done, a, true)
			}
		}
		if p.cfg.Discovery {
			busy := map[netip.AddrPort]bool{}
			for a := range p.sessions {
				busy[a] = true
			}
			for _, a := range configured {
				busy[a] = true
			}
			for a := range p.selves {
				busy[a] = true
			}
			missing := max(want-len(discovered), 0)
			for _, a := range p.book.Pick(missing, busy) {
				if !p.own(a) {
					p.startLocked(ctx, &wg, done, a, false)
				}
			}
			if missing == 0 && !probing && t0 >= lastProbe+probeInterval {
				if a, ok := p.book.Probe(busy); ok && !p.own(a) {
					probing, lastProbe = true, t0
					p.book.Attempted(a)
					wg.Add(1)
					go func() {
						defer wg.Done()
						probes <- ended{s: &session{addr: a}, err: p.probe(ctx, a)}
					}()
				}
			}
		}
		p.looking.Store(len(p.sessions) < p.cfg.MaxPeers)
		p.mu.Unlock()
		if err := p.book.Save(); err != nil {
			p.n.log.Warn("peer address book", "err", err)
		}
		select {
		case <-ctx.Done():
			p.mu.Lock()
			for _, s := range p.sessions {
				s.cancel()
			}
			p.mu.Unlock()
			stopped := make(chan struct{})
			go func() {
				wg.Wait()
				close(stopped)
			}()
			for {
				select {
				case d := <-done:
					p.ended(d.s, d.err)
				case <-probes:
				case <-stopped:
					return
				}
			}
		case <-t.C:
		case d := <-done:
			p.ended(d.s, d.err)
		}
	}
}

// startLocked starts a connection. Called with p.mu held.
func (p *Peers) startLocked(ctx context.Context, wg *sync.WaitGroup, done chan<- ended, a netip.AddrPort, configured bool) {
	sctx, cancel := context.WithCancel(ctx)
	s := &session{addr: a, name: a.String(), configured: configured, cancel: cancel, connected: now()}
	p.sessions[a] = s
	p.book.Attempted(a)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := p.run(sctx, s)
		cancel()
		if ctx.Err() != nil {
			err = nil
		}
		done <- ended{s: s, err: err}
	}()
}

// ended records how a connection ended.
func (p *Peers) ended(s *session, err error) {
	p.mu.Lock()
	if p.sessions[s.addr] == s {
		delete(p.sessions, s.addr)
	}
	delete(p.sent, s.name)
	p.mu.Unlock()
	p.n.Chain.Release(s.name)
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	p.n.log.Debug("peer connection ended", "peer", s.name, "err", err)
	if !errors.Is(err, p2p.ErrSelf) {
		st := p.statusOf(s)
		msg, t := err.Error(), now()
		st.Error, st.LastAttempt, st.Connected = &msg, &t, nil
		p.mu.Lock()
		p.ends[s.addr] = st
		p.mu.Unlock()
	}
	var sc *chain.SeedConformanceError
	if errors.As(err, &sc) {
		p.n.log.Error("seed conformance fault", "err", sc)
		if p.n.Fatal != nil {
			p.n.Fatal(sc)
		}
	}
	if errors.Is(err, p2p.ErrPeerInvalid) {
		p.n.log.Warn("peer sent invalid data", "peer", s.name, "err", err)
		p.Punish(s.addr.Addr())
	}
	if errors.Is(err, p2p.ErrSelf) {
		// Under some other address it reached itself: not again this run.
		p.mu.Lock()
		p.selves[s.addr] = true
		p.mu.Unlock()
		p.book.Forget(s.addr)
		return
	}
	p.book.Failed(s.addr)
}

func (p *Peers) hello(announce bool) *p2p.Hello {
	h := &p2p.Hello{Version: p2p.ProtocolVersion, ChainID: p.n.Chain.ChainID(), Agent: p.cfg.Agent,
		Nonce: p.nonce, Announce: announce}
	if p.cfg.Discovery {
		h.Listen = p.listenAt
	}
	return h
}

// probe tests an address with a connection that only exchanges hellos.
func (p *Peers) probe(ctx context.Context, a netip.AddrPort) error {
	conn, err := p2p.Dial(ctx, a.String(), p.hello(false))
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// round records a completed round with a peer, at most once a minute.
func (p *Peers) round(s *session) {
	t := now()
	s.success.Store(t)
	if t >= s.marked.Load()+markInterval {
		s.marked.Store(t)
		p.book.Succeeded(s.addr)
	}
}

// run is one outbound connection: follow the peer's chain, push ours when
// it is ahead, exchange transactions, and ask for addresses.
func (p *Peers) run(ctx context.Context, s *session) error {
	conn, err := p2p.Dial(ctx, s.name, p.hello(true))
	if err != nil {
		return err
	}
	defer conn.Close()
	p.n.log.Info("peer connected", "peer", s.name, "agent", conn.Peer.Agent, "height", conn.Peer.Height)
	s.height.Store(conn.Peer.Height)
	p.mu.Lock()
	delete(p.ends, s.addr)
	s.conn.Store(conn)
	p.sent[s.name] = map[hashing.Hash]bool{}
	p.mu.Unlock()
	// Every task of the connection ends with it.
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		conn.Close()
		wg.Wait()
	}()
	errc := make(chan error, 4)
	spawn := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errc <- f()
		}()
	}
	sy := &p2p.Syncer{Chain: p.n.Chain, Log: p.n.log, Pin: p.cfg.SeedPin, Peer: s.name,
		OnBlock: func(*chain.Entry, []byte) { s.lastBlock.Store(now()) }}
	spawn(func() error {
		if _, err := sy.SyncOnce(ctx, conn); err != nil {
			return err
		}
		p.round(s)
		return sy.Run(ctx, conn)
	})
	spawn(func() error { return p.pushLoop(ctx, s, conn) })
	spawn(func() error { return p.poolLoop(ctx, s.name, conn) })
	if p.cfg.Discovery {
		spawn(func() error { return p.addressLoop(ctx, s, conn) })
	}
	select {
	case err := <-errc:
		if err == nil {
			err = errors.New("connection ended")
		}
		return err
	case <-conn.Done():
		return conn.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// addressLoop asks the peer for addresses: every ten seconds while this
// node has fewer connections than it wants, otherwise every ten minutes.
func (p *Peers) addressLoop(ctx context.Context, s *session, conn *p2p.Conn) error {
	for {
		m, err := conn.Do(ctx, &p2p.AddressesRequest{})
		var ea *p2p.ErrorAnswer
		switch {
		case errors.As(err, &ea):
		case err != nil:
			return err
		default:
			if a, ok := m.(*p2p.Addresses); ok {
				p.book.AddShared(a.Addrs, s.addr.Addr())
			}
		}
		wait := askInterval
		if p.looking.Load() {
			wait = askLooking
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// pushLoop sends the peer our blocks when our tip has more work than its:
// up to 32 past its locator, the last block it took first, straight away
// again while more remain.
func (p *Peers) pushLoop(ctx context.Context, s *session, conn *p2p.Conn) error {
	name := s.name
	tips, unsubscribe := p.n.Chain.Subscribe()
	defer unsubscribe()
	t := time.NewTicker(p2p.KeepAlive)
	defer t.Stop()
	var pushed *hashing.Hash
	for {
		more := false
		st, err := conn.State(ctx)
		if err != nil {
			return err
		}
		s.height.Store(st.Height)
		p.round(s)
		tip := p.n.Chain.Tip()
		if tip.ChainWork.Cmp(st.Work) > 0 {
			locator := st.Locator
			if pushed != nil {
				locator = append([]hashing.Hash{*pushed}, locator...)
				if len(locator) > p2p.MaxLocator {
					locator = locator[:p2p.MaxLocator]
				}
			}
			hs, err := p.n.Chain.HeadersAfter(locator, maxImportPush)
			if err == nil {
				refused := false
				for _, h := range hs {
					id := types.HeaderIDFromBytes(h)
					raw, err := p.n.Chain.Block(id)
					if err != nil {
						refused = true
						break
					}
					err = pushItem(ctx, conn, raw, func(off, total uint64, b []byte) p2p.Message {
						return &p2p.Import{Offset: off, Total: total, Bytes: b}
					})
					var ea *p2p.ErrorAnswer
					if errors.As(err, &ea) {
						p.n.log.Debug("peer refused our block", "peer", name, "id", id.String(), "err", ea.Text)
						refused = true
						break
					}
					if err != nil {
						return err
					}
					v := id
					pushed = &v
				}
				more = !refused && len(hs) == maxImportPush
			}
		}
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tips:
		case <-t.C:
		}
	}
}

// pushItem sends an item whole or in 1 MiB slices.
func pushItem(ctx context.Context, conn *p2p.Conn, raw []byte, mk func(off, total uint64, b []byte) p2p.Message) error {
	total := uint64(len(raw))
	for off := uint64(0); off < total || total == 0; {
		end := min(off+p2p.MaxSlice, total)
		m, err := conn.Do(ctx, mk(off, total, raw[off:end]))
		if err != nil {
			return err
		}
		switch a := m.(type) {
		case *p2p.Received:
			if a.Bytes != end {
				return fmt.Errorf("peer received %d of %d", a.Bytes, end)
			}
		case *p2p.Taken:
			return nil
		default:
			return fmt.Errorf("%w: unexpected push answer", p2p.ErrNotMessage)
		}
		off = end
	}
	return nil
}

// poolLoop compares pools with the peer, pending and deferred: it fetches
// what the peer holds that this node lacks and offers what it lacks.
func (p *Peers) poolLoop(ctx context.Context, name string, conn *p2p.Conn) error {
	t := time.NewTicker(p2p.KeepAlive)
	defer t.Stop()
	pool, stop := p.watchPool()
	defer stop()
	var source netip.Addr
	if ap, err := netip.ParseAddrPort(conn.RemoteAddr().String()); err == nil {
		source = ap.Addr().Unmap()
	}
	for {
		for _, deferred := range []bool{false, true} {
			if err := p.exchange(ctx, name, conn, deferred, source); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-pool:
		}
	}
}

// errInventoryTooLarge ends a connection whose peer lists more than this
// node's pool or deferred set can hold.
var errInventoryTooLarge = errors.New("inventory beyond the set's size")

func (p *Peers) exchange(ctx context.Context, name string, conn *p2p.Conn, deferred bool, source netip.Addr) error {
	limit := p.n.tx.maxCount
	if deferred {
		limit = DeferredMax
	}
	theirs := map[hashing.Hash]bool{}
	var after *hashing.Hash
	for {
		m, err := conn.Do(ctx, &p2p.InventoryRequest{Deferred: deferred, After: after})
		if err != nil {
			return err
		}
		inv, ok := m.(*p2p.Inventory)
		if !ok {
			return fmt.Errorf("%w: inventory", p2p.ErrNotMessage)
		}
		for _, id := range inv.IDs {
			theirs[id] = true
		}
		if len(theirs) > limit {
			return errInventoryTooLarge
		}
		if inv.Next == nil {
			break
		}
		after = inv.Next
	}
	var want []hashing.Hash
	for id := range theirs {
		if !p.n.HasTx(id) {
			want = append(want, id)
		}
	}
	for len(want) > 0 {
		k := min(len(want), p2p.MaxItems)
		asked := want[:k]
		want = want[k:]
		if err := p.fetchItems(ctx, conn, deferred, asked, source); err != nil {
			return err
		}
	}
	ours, _ := p.n.TxIDs(deferred, nil, 1<<30)
	for _, id := range ours {
		p.mu.Lock()
		done := p.sent[name][id]
		p.mu.Unlock()
		if done || theirs[id] {
			continue
		}
		raw, d, _ := p.n.GetTx(id)
		if raw == nil {
			continue
		}
		var notBefore *uint64
		if deferred && d != nil {
			nb := d.NotBefore
			notBefore = &nb
		}
		err := pushItem(ctx, conn, raw, func(off, total uint64, b []byte) p2p.Message {
			return &p2p.Submit{Offset: off, Total: total, Bytes: b, NotBefore: notBefore}
		})
		var ea *p2p.ErrorAnswer
		if err != nil && !errors.As(err, &ea) {
			return err
		}
		p.mu.Lock()
		if p.sent[name] != nil {
			p.sent[name][id] = true
		}
		p.mu.Unlock()
	}
	return nil
}

// errNotAsked ends a connection whose peer sends a transaction other than
// the one asked for.
var errNotAsked = errors.New("not the transaction asked for")

func (p *Peers) fetchItems(ctx context.Context, conn *p2p.Conn, deferred bool, asked []hashing.Hash, source netip.Addr) error {
	var req p2p.Message = &p2p.PendingRequest{IDs: asked}
	if deferred {
		req = &p2p.DeferredRequest{IDs: asked}
	}
	m, err := conn.Do(ctx, req)
	if err != nil {
		return err
	}
	items, ok := m.(*p2p.Items)
	if !ok {
		return fmt.Errorf("%w: items", p2p.ErrNotMessage)
	}
	wanted := map[hashing.Hash]bool{}
	for _, id := range asked {
		wanted[id] = true
	}
	if len(items.Items) > len(asked) {
		return fmt.Errorf("%w: more items than asked", p2p.ErrNotMessage)
	}
	bound := p.TxBound()
	for _, it := range items.Items {
		if !wanted[it.ID] || (it.NotBefore != nil) != deferred {
			return fmt.Errorf("%w: items not as asked", p2p.ErrNotMessage)
		}
		delete(wanted, it.ID)
		raw := it.Tx
		if it.BySize {
			if it.Size > bound {
				continue // larger than a block can hold
			}
			if raw, err = fetchTx(ctx, conn, it.ID, it.Size); err != nil {
				return err
			}
		} else if uint64(len(raw)) > bound {
			continue
		}
		if types.TransactionID(p.n.Chain.ChainID(), raw) != it.ID {
			return errNotAsked
		}
		p.n.Submit(raw, it.NotBefore, source, true)
	}
	return nil
}

func fetchTx(ctx context.Context, conn *p2p.Conn, id hashing.Hash, size uint64) ([]byte, error) {
	deadline := time.Now().Add(chain.TransferTime(size))
	var raw []byte
	for uint64(len(raw)) < size {
		if time.Now().After(deadline) {
			return nil, errors.New("transaction transfer time exceeded")
		}
		m, err := conn.Do(ctx, &p2p.TransactionRawRequest{ID: id, Offset: uint64(len(raw)),
			Length: uint32(min(size-uint64(len(raw)), p2p.MaxSlice))})
		if err != nil {
			return nil, err
		}
		s, ok := m.(*p2p.Slice)
		if !ok || s.Offset != uint64(len(raw)) || s.Total != size || len(s.Bytes) == 0 {
			return nil, fmt.Errorf("%w: transaction slice", p2p.ErrNotMessage)
		}
		raw = append(raw, s.Bytes...)
	}
	return raw, nil
}

// announceLoop sends announcements to subscribed inbound peers when the tip
// or the pool sizes change, at most every 0.2 seconds.
func (p *Peers) announceLoop(ctx context.Context) {
	tips, unsubscribe := p.n.Chain.Subscribe()
	defer unsubscribe()
	pool, stop := p.watchPool()
	defer stop()
	t := time.NewTicker(announceInterval)
	defer t.Stop()
	var last p2p.Announcement
	dirty := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tips:
			dirty = true
		case <-pool:
			dirty = true
		case <-t.C:
			if !dirty {
				continue
			}
			dirty = false
			tip := p.n.Chain.Tip()
			pending, _ := p.n.TxIDs(false, nil, 1<<30)
			a := p2p.Announcement{Tip: tip.ID, Height: tip.Height, Work: tip.ChainWork,
				Pending: uint64(len(pending)), Deferred: uint64(p.n.DeferredLen())}
			if a.Tip == last.Tip && a.Pending == last.Pending && a.Deferred == last.Deferred {
				continue
			}
			last = a
			p.mu.Lock()
			for ch := range p.subs {
				select {
				case ch <- &a:
				default:
				}
			}
			p.mu.Unlock()
		}
	}
}

// Backend implementation.

func (p *Peers) ChainID() hashing.Hash { return p.n.Chain.ChainID() }

func (p *Peers) State() *p2p.State {
	c := p.n.Chain
	tip := c.Tip()
	pending, _ := p.n.TxIDs(false, nil, p2p.MaxPoolIDs)
	deferred, _ := p.n.TxIDs(true, nil, p2p.MaxPoolIDs)
	return &p2p.State{Version: p2p.ProtocolVersion, ChainID: c.ChainID(), Agent: p.cfg.Agent,
		Nonce: p.nonce, Height: tip.Height, Tip: tip.ID, Work: new(big.Int).Set(tip.ChainWork),
		Locator: c.Locator(), Pending: pending, Deferred: deferred}
}

func (p *Peers) Headers(locator []hashing.Hash) ([][]byte, error) {
	return p.n.Chain.HeadersAfter(locator, p2p.MaxHeaders)
}

func (p *Peers) Block(id hashing.Hash) ([]byte, error) {
	e, ok := p.n.Chain.Get(id)
	if !ok || e.Status != chain.StatusValid {
		return nil, p2p.ErrNotFound
	}
	return p.n.Chain.Block(id)
}

// classify turns a chain error into a peer-protocol answer.
func classify(err error) error {
	var sc *chain.SeedConformanceError
	if errors.As(err, &sc) {
		return errors.New("this node rejects a listed seed block: " + sc.Err.Error())
	}
	var ie *chain.InvalidError
	if errors.As(err, &ie) && ie.Header {
		return &p2p.InvalidData{Text: "invalid header: " + ie.Reason}
	}
	if errors.Is(err, chain.ErrInvalid) {
		return &p2p.InvalidData{Text: "invalid block: " + err.Error()}
	}
	return err
}

func (p *Peers) CheckHeader(header []byte) (bool, hashing.Hash, uint64, error) {
	h, err := types.DecodeHeader(header)
	if err != nil {
		return false, hashing.Hash{}, 0, &p2p.InvalidData{Text: "invalid block: " + err.Error()}
	}
	known, max, err := p.n.Chain.CheckHeader(h)
	if err != nil {
		return false, hashing.Hash{}, 0, classify(err)
	}
	return known, h.ID(), max, nil
}

func (p *Peers) Import(raw []byte, from netip.AddrPort) (hashing.Hash, bool, error) {
	h, err := types.DecodeHeader(raw[:types.HeaderSize])
	if err != nil {
		return hashing.Hash{}, false, &p2p.InvalidData{Text: "invalid header: " + err.Error()}
	}
	id := types.HeaderIDFromBytes(raw[:types.HeaderSize])
	tip := p.n.Chain.Tip()
	if parent, ok := p.n.Chain.Get(h.ParentID); ok && !p.n.Chain.IsCanonical(parent) {
		day := new(big.Int).Mul(consensus.Work(tip.Target), big.NewInt(dayOfBlocks))
		work := new(big.Int).Add(parent.ChainWork, consensus.Work(h.Target))
		if new(big.Int).Add(work, day).Cmp(tip.ChainWork) < 0 {
			if _, indexed := p.n.Chain.Get(id); !indexed {
				return id, false, chain.ErrBehind
			}
		}
	}
	_, err = p.n.SubmitBlockFrom(raw, from.String())
	switch {
	case err == nil:
		return id, false, nil
	case errors.Is(err, chain.ErrKnown):
		return id, true, nil
	}
	return id, false, classify(err)
}

func (p *Peers) TxBound() uint64 { return p.n.nextLimits()[types.D] }

func (p *Peers) Transaction(id hashing.Hash) ([]byte, error) {
	raw, _, _ := p.n.GetTx(id)
	if raw == nil {
		return nil, p2p.ErrNotFound
	}
	return raw, nil
}

func (p *Peers) Submit(raw []byte, notBefore *uint64, from netip.AddrPort) (hashing.Hash, bool, error) {
	r, err := p.n.Submit(raw, notBefore, from.Addr().Unmap(), true)
	if err != nil {
		return hashing.Hash{}, false, err
	}
	return r.ID, r.Known, nil
}

func (p *Peers) Inventory(deferred bool, after *hashing.Hash) ([]hashing.Hash, *hashing.Hash) {
	ids, more := p.n.TxIDs(deferred, after, p2p.MaxInventory)
	if !more || len(ids) == 0 {
		return ids, nil
	}
	next := ids[len(ids)-1]
	return ids, &next
}

// relayInline is the most bytes of transactions an Items answer carries
// whole: half a frame.
const relayInline = p2p.MaxFrame / 2

func (p *Peers) Items(deferred bool, ids []hashing.Hash) []p2p.Item {
	var out []p2p.Item
	whole := 0
	for _, id := range ids {
		raw, d, pooled := p.n.GetTx(id)
		if raw == nil || (deferred && d == nil) || (!deferred && (!pooled || d != nil)) {
			continue
		}
		it := p2p.Item{ID: id}
		if deferred {
			nb := d.NotBefore
			it.NotBefore = &nb
		}
		if len(raw) <= p2p.MaxSlice && whole+len(raw) <= relayInline {
			it.Tx = raw
			whole += len(raw)
		} else {
			it.BySize = true
			it.Size = uint64(len(raw))
		}
		out = append(out, it)
	}
	return out
}

func (p *Peers) Addresses(asker netip.Addr, own *netip.AddrPort) []netip.AddrPort {
	if !p.cfg.Discovery {
		return nil
	}
	return p.book.Share(asker, own)
}

func (p *Peers) Subscribe() (<-chan *p2p.Announcement, func()) {
	ch := make(chan *p2p.Announcement, 8)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

func (p *Peers) Learned(addr netip.AddrPort) {
	if p.cfg.Discovery {
		p.book.Add(addr, SourceInbound)
	}
}

func (p *Peers) Banned(a netip.Addr) bool { return p.book.Banned(a) }

// Punish bans a peer's range for a day and ends its connections, unless
// the range holds a configured peer or is this machine.
func (p *Peers) Punish(a netip.Addr) {
	if !p.book.Ban(a) {
		return
	}
	if p.server != nil {
		p.server.CloseRange(a)
	}
	k := banKey(a)
	p.mu.Lock()
	for _, s := range p.sessions {
		if banKey(s.addr.Addr()) == k {
			s.cancel()
		}
	}
	p.mu.Unlock()
}

// PeerInfo describes a connection for the API.
type PeerInfo struct {
	Addr       string `json:"address"`
	Direction  string `json:"direction"`
	Configured bool   `json:"configured,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Height     uint64 `json:"height,omitempty"`
}

// Info lists connections.
func (p *Peers) Info() []PeerInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []PeerInfo
	for _, s := range p.sessions {
		pi := PeerInfo{Addr: s.name, Direction: "outbound", Configured: s.configured}
		if c := s.conn.Load(); c != nil {
			pi.Agent, pi.Height = c.Peer.Agent, c.Peer.Height
		}
		out = append(out, pi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	if p.server != nil {
		for i := 0; i < p.server.Inbound(); i++ {
			out = append(out, PeerInfo{Direction: "inbound"})
		}
	}
	return out
}

// bestPeerHeight is the greatest height a connected peer reported.
func (p *Peers) bestPeerHeight() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best uint64
	for _, s := range p.sessions {
		if c := s.conn.Load(); c != nil && c.Peer.Height > best {
			best = c.Peer.Height
		}
	}
	return best
}

func (p *Peers) statusOf(s *session) PeerStatus {
	st := PeerStatus{Address: s.addr, Source: "discovered"}
	if s.configured {
		st.Source = "config"
	}
	if c := s.conn.Load(); c != nil {
		agent, connected, h := c.Peer.Agent, s.connected, s.height.Load()
		st.Agent, st.Connected, st.Height = &agent, &connected, &h
	}
	if t := s.success.Load(); t > 0 {
		st.LastSuccess = &t
	}
	if t := s.lastBlock.Load(); t > 0 {
		st.LastBlock = &t
	}
	return st
}

// Statuses lists the configured peers, connected or not, then the
// discovered peers connected.
func (p *Peers) Statuses() []PeerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []PeerStatus
	for _, a := range p.configured {
		switch s, ok := p.sessions[a]; {
		case ok:
			out = append(out, p.statusOf(s))
		default:
			if st, ok := p.ends[a]; ok {
				st.Source = "config"
				out = append(out, st)
			} else {
				out = append(out, PeerStatus{Address: a, Source: "config"})
			}
		}
	}
	var disc []PeerStatus
	for _, s := range p.sessions {
		if !s.configured {
			disc = append(disc, p.statusOf(s))
		}
	}
	sort.Slice(disc, func(i, j int) bool { return disc[i].Address.String() < disc[j].Address.String() })
	return append(out, disc...)
}

// Configured returns the configured peers.
func (p *Peers) Configured() []netip.AddrPort {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]netip.AddrPort(nil), p.configured...)
}

// SetConfigured replaces the configured peers: new ones are connected, and
// a connection to one no longer configured ends; it is forgotten unless a
// peer shares it later.
func (p *Peers) SetConfigured(peers []netip.AddrPort) error {
	if len(peers) > MaxConfigured {
		return fmt.Errorf("at most %d configured peers", MaxConfigured)
	}
	list := make([]netip.AddrPort, len(peers))
	for i, a := range peers {
		list[i] = canon(a)
	}
	p.mu.Lock()
	p.configured = list
	for a := range p.ends {
		keep := false
		for _, c := range list {
			keep = keep || c == a
		}
		if !keep {
			delete(p.ends, a)
		}
	}
	p.mu.Unlock()
	p.book.Configure(list)
	return nil
}
