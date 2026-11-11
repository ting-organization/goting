package node

import (
	"encoding/json"
	"math/rand/v2"
	"net/netip"
	"os"
	"slices"
	"sort"
	"sync"
)

// Peer discovery policy (reference node, wiki: node/sync). Not consensus.
const (
	ShareMax = 32
	BookMax  = 1024
	// perIP and perGroup are the discovered addresses the book keeps of one
	// public IP address (room for a node and a second one, or a port it
	// moved to) and of one network group: a 32nd of the book, as Bitcoin's
	// tried table keeps one group to 8 of its 256 buckets, so that filling
	// the book takes 32 networks.
	perIP    = 2
	perGroup = BookMax / 32
	// addedPerHour is how many addresses one network group may add in an
	// hour, announcing its own or sharing others: one answer's worth, as
	// many as an honest peer's answer changes by in that time.
	addedPerHour = ShareMax
	hour         = 3600
	shareFresh   = 24 * 3600
	// shareKept: every peer is given the same answer for an hour, so asking
	// again tells nothing new and a peer cannot map the book, or this
	// node's connections, by asking often.
	shareKept  = hour
	retryBase  = 60
	retryMax   = 3600
	BanSeconds = 24 * 3600
	BanMax     = 4096
)

// AddrSource says where the book learned an address.
type AddrSource string

const (
	SourceConfig  AddrSource = "config"
	SourceInbound AddrSource = "inbound"
	SourcePeer    AddrSource = "peer"
	SourceSeed    AddrSource = "seed"
)

// BookEntry is one known address.
type BookEntry struct {
	Address netip.AddrPort `json:"address"`
	Source  AddrSource     `json:"source"`
	// Verified is when a round with it last completed.
	Verified *uint64 `json:"verified,omitempty"`
	// Attempted is when a connection to it was last started.
	Attempted uint64 `json:"attempted"`
	// Failures counts connections failed since a round last completed.
	Failures uint32 `json:"failures"`
}

type reach int

const (
	reachUnusable reach = iota
	reachLoopback
	reachPrivate
	reachPublic
)

func reachOf(a netip.Addr) reach {
	a = a.Unmap()
	switch {
	case !a.IsValid() || a.IsUnspecified() || a.IsMulticast():
		return reachUnusable
	case a.Is4():
		o := a.As4()
		if o == [4]byte{255, 255, 255, 255} || (o[0] == 192 && o[1] == 0 && o[2] == 2) ||
			(o[0] == 198 && o[1] == 51 && o[2] == 100) || (o[0] == 203 && o[1] == 0 && o[2] == 113) {
			return reachUnusable
		}
		if a.IsLoopback() {
			return reachLoopback
		}
		if a.IsPrivate() || a.IsLinkLocalUnicast() || (o[0] == 100 && o[1]&0xc0 == 64) {
			return reachPrivate
		}
		return reachPublic
	default:
		b := a.As16()
		if b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x0d && b[3] == 0xb8 {
			return reachUnusable
		}
		if a.IsLoopback() {
			return reachLoopback
		}
		if b[0]&0xfe == 0xfc || (b[0] == 0xfe && b[1]&0xc0 == 0x80) {
			return reachPrivate
		}
		return reachPublic
	}
}

func usable(a netip.AddrPort) bool {
	return a.Port() != 0 && reachOf(a.Addr()) != reachUnusable
}

// shareable reports whether a may be shared with, or accepted from, a peer
// at peer: public addresses with anyone, private ones only off the public
// internet, loopback ones only on the same machine.
func shareable(a netip.AddrPort, peer netip.Addr) bool {
	switch reachOf(a.Addr()) {
	case reachPublic:
		return true
	case reachPrivate:
		return reachOf(peer) != reachPublic
	case reachLoopback:
		return reachOf(peer) == reachLoopback
	}
	return false
}

// banKey is an address's range: the IPv4 address or the IPv6 /64.
func banKey(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if a.Is4() {
		return a
	}
	b := a.As16()
	for i := 8; i < 16; i++ {
		b[i] = 0
	}
	return netip.AddrFrom16(b)
}

// group is the network group of a public address (an IPv4 /16, an IPv6
// /32), from which a node keeps at most one discovered peer. Local
// addresses have none.
func group(a netip.Addr) (string, bool) {
	a = a.Unmap()
	if reachOf(a) != reachPublic {
		return "", false
	}
	bits := 32
	if a.Is4() {
		bits = 16
	}
	p, _ := a.Prefix(bits)
	return p.String(), true
}

func canon(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}

// discovered reports whether an address came from a peer, rather than being
// configured or a seed's: only discovered ones are limited, and give way.
func (e *BookEntry) discovered() bool {
	return e.Source == SourceInbound || e.Source == SourcePeer
}

// shared is an addresses answer: when it was drawn, and whether it holds
// every address there was to share then.
type shared struct {
	drawn uint64
	list  []netip.AddrPort
	whole bool
}

// added counts the addresses a network group added in its hour.
type added struct {
	began uint64
	count int
}

// AddressBook keeps the addresses this node knows and the ranges it bans.
// Bans are kept for this run only.
type AddressBook struct {
	mu      sync.Mutex
	entries map[netip.AddrPort]*BookEntry
	banned  map[netip.Addr]uint64
	// shared is the addresses answer for peers of each reach; added, by
	// network group, how many addresses it added this hour.
	shared map[reach]*shared
	added  map[string]*added
	path   string
	dirty  bool
}

// NewAddressBook loads peers.json from path, if given; a missing or
// unreadable file starts it empty, since it only holds hints. A book saved
// past the shares of one address and one network keeps what is worth
// keeping: the configured peers and the seeds, then the most recently
// proven, then the least failed.
func NewAddressBook(path string) *AddressBook {
	b := &AddressBook{entries: map[netip.AddrPort]*BookEntry{}, banned: map[netip.Addr]uint64{},
		shared: map[reach]*shared{}, added: map[string]*added{}, path: path}
	if path == "" {
		return b
	}
	// The book holds at most BookMax short entries: a file far larger is
	// not one this node wrote.
	if st, err := os.Stat(path); err != nil || st.Size() > 4<<20 {
		return b
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	var stored struct {
		Addresses []BookEntry `json:"addresses"`
	}
	if json.Unmarshal(data, &stored) != nil {
		return b
	}
	list := stored.Addresses
	sort.SliceStable(list, func(i, j int) bool {
		x, y := &list[i], &list[j]
		if x.discovered() != y.discovered() {
			return !x.discovered()
		}
		xv, yv := uint64(0), uint64(0)
		if x.Verified != nil {
			xv = *x.Verified + 1
		}
		if y.Verified != nil {
			yv = *y.Verified + 1
		}
		if xv != yv {
			return xv > yv
		}
		return x.Failures < y.Failures
	})
	for i := range list {
		if len(b.entries) >= BookMax {
			break
		}
		e := list[i]
		e.Address = canon(e.Address)
		if _, known := b.entries[e.Address]; known || !usable(e.Address) {
			continue
		}
		switch e.Source {
		case SourceConfig, SourceSeed, SourceInbound, SourcePeer:
		default:
			e.Source = SourcePeer
		}
		if !e.discovered() || b.fits(e.Address) {
			b.entries[e.Address] = &e
		}
	}
	return b
}

// Configure marks exactly peers as the configured ones: an address no
// longer configured is forgotten, and configuring one lifts a ban on its
// range.
func (b *AddressBook) Configure(peers []netip.AddrPort) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for a, e := range b.entries {
		if e.Source == SourceConfig && !slices.Contains(peers, a) {
			delete(b.entries, a)
			b.dirty = true
		}
	}
	for _, a := range peers {
		a = canon(a)
		delete(b.banned, banKey(a.Addr()))
		if e, ok := b.entries[a]; ok {
			if e.Source != SourceConfig {
				e.Source = SourceConfig
				b.dirty = true
			}
			continue
		}
		b.insert(a, SourceConfig)
	}
}

// Add learns an address the book does not know, unless it is unusable or
// banned; it reports whether it did. A discovered one takes its place within
// the share of its IP address and network group.
func (b *AddressBook) Add(a netip.AddrPort, src AddrSource) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.learn(a, src)
}

func (b *AddressBook) learn(a netip.AddrPort, src AddrSource) bool {
	a = canon(a)
	if !usable(a) {
		return false
	}
	if _, ok := b.entries[a]; ok || b.isBanned(a.Addr()) {
		return false
	}
	if (src == SourceInbound || src == SourcePeer) && !b.makeRoom(a) {
		return false
	}
	return b.insert(a, src)
}

// AddAnnounced adds the listening address a peer that connected announced,
// at the IP address it came from, unless its network group has added its
// hour's worth; it reports whether it did.
func (b *AddressBook) AddAnnounced(a netip.AddrPort) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := now()
	g, public := group(a.Addr())
	if public && !b.mayAdd(g, t) {
		return false
	}
	if !b.learn(a, SourceInbound) {
		return false
	}
	if public {
		b.countAdded(g, t)
	}
	return true
}

// AddShared adds the addresses one peer shared: at most ShareMax of them,
// only those the peer could know of from where it is, and none once the
// peer's network group has added its hour's worth.
func (b *AddressBook) AddShared(addrs []netip.AddrPort, from netip.Addr) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := now()
	g, public := group(from)
	n := 0
	for i, a := range addrs {
		if i == ShareMax || (public && !b.mayAdd(g, t)) {
			break
		}
		if shareable(canon(a), from) && b.learn(a, SourcePeer) {
			if public {
				b.countAdded(g, t)
			}
			n++
		}
	}
	return n
}

// AddSeed adds a seed node's address, or marks a known one as a seed's
// unless it is configured: a seed is where a node starts, picked after
// every other address. A banned one is kept too, to be connected once the
// ban ends.
func (b *AddressBook) AddSeed(a netip.AddrPort) {
	a = canon(a)
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.entries[a]; ok {
		if e.Source != SourceConfig && e.Source != SourceSeed {
			e.Source = SourceSeed
			b.dirty = true
		}
		return
	}
	if usable(a) {
		b.insert(a, SourceSeed)
	}
}

// IsSeed reports whether an address is a seed node's.
func (b *AddressBook) IsSeed(a netip.AddrPort) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[canon(a)]
	return ok && e.Source == SourceSeed
}

// Seeds lists the seed nodes' addresses.
func (b *AddressBook) Seeds() []netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []netip.AddrPort
	for a, e := range b.entries {
		if e.Source == SourceSeed {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Compare(out[j]) < 0 })
	return out
}

// SeedDue reports whether the node may connect to one of the seed nodes'
// addresses now: one that is not banned, and not waiting after failures.
func (b *AddressBook) SeedDue() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for a, e := range b.entries {
		if e.Source == SourceSeed && b.due(a) {
			return true
		}
	}
	return false
}

// fits reports whether the discovered addresses of a's IP address and
// network group leave room for it. Local addresses have no share: they come
// only from this machine or its own network.
func (b *AddressBook) fits(a netip.AddrPort) bool {
	own, public := group(a.Addr())
	if !public {
		return true
	}
	sameIP, sameGroup := 0, 0
	for _, e := range b.entries {
		if !e.discovered() {
			continue
		}
		if g, ok := group(e.Address.Addr()); ok && g == own {
			sameGroup++
			if e.Address.Addr() == a.Addr() {
				sameIP++
			}
		}
	}
	return sameIP < perIP && sameGroup < perGroup
}

// makeRoom makes room for a within the shares of its IP address and network
// group if either is full: the least worth keeping of that address or group
// gives way, never a proven one. It reports whether there is room.
func (b *AddressBook) makeRoom(a netip.AddrPort) bool {
	own, public := group(a.Addr())
	if !public {
		return true
	}
	sameIP := func(e *BookEntry) bool { return e.Address.Addr() == a.Addr() }
	sameGroup := func(e *BookEntry) bool {
		g, ok := group(e.Address.Addr())
		return ok && g == own
	}
	if b.held(sameIP) >= perIP && !b.giveWay(sameIP, false) {
		return false
	}
	if b.held(sameGroup) >= perGroup && !b.giveWay(sameGroup, false) {
		return false
	}
	return b.fits(a)
}

func (b *AddressBook) held(same func(*BookEntry) bool) int {
	n := 0
	for _, e := range b.entries {
		if e.discovered() && same(e) {
			n++
		}
	}
	return n
}

// mayAdd reports whether network group g may add another address at t: it
// has not added addedPerHour in the hour since its first.
func (b *AddressBook) mayAdd(g string, t uint64) bool {
	a, ok := b.added[g]
	return !ok || t < a.began || t-a.began >= hour || a.count < addedPerHour
}

// countAdded counts an address network group g added at t. The groups
// counted are at most as many as the book holds: past that, the one whose
// hour began first gives way.
func (b *AddressBook) countAdded(g string, t uint64) {
	if _, ok := b.added[g]; !ok && len(b.added) >= BookMax {
		var first string
		began := ^uint64(0)
		for k, a := range b.added {
			if t < a.began || t-a.began >= hour {
				delete(b.added, k)
			} else if a.began < began {
				first, began = k, a.began
			}
		}
		if len(b.added) >= BookMax {
			delete(b.added, first)
		}
	}
	a := b.added[g]
	if a == nil || t < a.began || t-a.began >= hour {
		a = &added{began: t}
		b.added[g] = a
	}
	a.count++
}

func (b *AddressBook) insert(a netip.AddrPort, src AddrSource) bool {
	if len(b.entries) >= BookMax && !b.giveWay(func(*BookEntry) bool { return true }, true) {
		return false
	}
	b.entries[a] = &BookEntry{Address: a, Source: src}
	b.dirty = true
	return true
}

// giveWay drops the least useful of the discovered addresses among picks,
// and only an unproven one unless proven: never a configured one or a
// seed's, an unproven one before a proven one, of unproven ones the most
// failed first, of proven ones the one proven longest ago, and of those
// alike one at random, so that no address can be chosen to stay. It reports
// whether one was dropped.
func (b *AddressBook) giveWay(among func(*BookEntry) bool, proven bool) bool {
	type rank struct {
		verified bool
		at       uint64
		failures uint32
	}
	of := func(e *BookEntry) rank {
		if e.Verified == nil {
			return rank{false, 0, ^e.Failures}
		}
		return rank{true, *e.Verified, ^e.Failures}
	}
	less := func(x, y rank) bool {
		if x.verified != y.verified {
			return !x.verified
		}
		if x.at != y.at {
			return x.at < y.at
		}
		return x.failures < y.failures
	}
	var alike []netip.AddrPort
	var worst rank
	for _, e := range b.entries {
		if !e.discovered() || !among(e) || (!proven && e.Verified != nil) {
			continue
		}
		r := of(e)
		switch {
		case len(alike) == 0 || less(r, worst):
			worst, alike = r, append(alike[:0], e.Address)
		case r == worst:
			alike = append(alike, e.Address)
		}
	}
	if len(alike) == 0 {
		return false
	}
	delete(b.entries, alike[rand.IntN(len(alike))])
	b.dirty = true
	return true
}

// Forget drops an address, as one that reached this node itself.
func (b *AddressBook) Forget(a netip.AddrPort) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.entries[canon(a)]; ok {
		delete(b.entries, canon(a))
		b.dirty = true
	}
}

// Attempted records a connection attempt; Succeeded a completed round;
// Failed a failed connection.
func (b *AddressBook) Attempted(a netip.AddrPort) {
	b.update(a, func(e *BookEntry) { e.Attempted = now() })
}

func (b *AddressBook) Succeeded(a netip.AddrPort) {
	b.update(a, func(e *BookEntry) { t := now(); e.Verified = &t; e.Failures = 0 })
}

func (b *AddressBook) Failed(a netip.AddrPort) {
	b.update(a, func(e *BookEntry) {
		if e.Failures < ^uint32(0) {
			e.Failures++
		}
	})
}

func (b *AddressBook) update(a netip.AddrPort, fn func(*BookEntry)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.entries[canon(a)]; ok {
		fn(e)
		b.dirty = true
	}
}

// retry reports whether a discovered address may be tried again: a minute
// after a failure, doubling with each further one up to an hour.
func (e *BookEntry) retry(t uint64) bool {
	if e.Failures == 0 {
		return true
	}
	wait := min(uint64(retryBase)<<min(e.Failures-1, 6), retryMax)
	return t >= e.Attempted+wait
}

// Due reports whether a connection to an address may be tried: at once, or
// after a failure once the wait is over, and not while it is banned. A
// configured peer waits a minute.
func (b *AddressBook) Due(a netip.AddrPort) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.due(canon(a))
}

func (b *AddressBook) due(a netip.AddrPort) bool {
	if b.isBanned(a.Addr()) {
		return false
	}
	e, ok := b.entries[a]
	if !ok {
		return true
	}
	t := now()
	if e.Source == SourceConfig {
		return e.Failures == 0 || t >= e.Attempted+retryBase
	}
	return e.retry(t)
}

// Pick returns up to want addresses to connect to, besides busy: proven
// ones first, most recently proven first, then unproven ones in random
// order, and seeds after every other; each from a network group none of the
// others is in.
func (b *AddressBook) Pick(want int, busy map[netip.AddrPort]bool) []netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	groups := map[string]bool{}
	for a := range busy {
		if g, ok := group(a.Addr()); ok {
			groups[g] = true
		}
	}
	t := now()
	var cands []*BookEntry
	for _, e := range b.entries {
		if e.Source != SourceConfig && !busy[e.Address] && !b.isBanned(e.Address.Addr()) && e.retry(t) {
			cands = append(cands, e)
		}
	}
	rand.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	sort.SliceStable(cands, func(i, j int) bool {
		if si, sj := cands[i].Source == SourceSeed, cands[j].Source == SourceSeed; si != sj {
			return sj
		}
		vi, vj := cands[i].Verified, cands[j].Verified
		switch {
		case vi == nil && vj == nil:
			return false
		case vi == nil:
			return false
		case vj == nil:
			return true
		}
		return *vi > *vj
	})
	var out []netip.AddrPort
	for _, e := range cands {
		if len(out) == want {
			break
		}
		if g, ok := group(e.Address.Addr()); ok {
			if groups[g] {
				continue
			}
			groups[g] = true
		}
		out = append(out, e.Address)
	}
	return out
}

// Probe returns one unproven address, not a seed's, to test with a short
// connection when every connection slot is taken, so new nodes still become
// shareable.
func (b *AddressBook) Probe(busy map[netip.AddrPort]bool) (netip.AddrPort, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := now()
	var cands []netip.AddrPort
	for _, e := range b.entries {
		if e.Verified == nil && e.Source != SourceSeed && !busy[e.Address] && !b.isBanned(e.Address.Addr()) && e.retry(t) {
			cands = append(cands, e.Address)
		}
	}
	if len(cands) == 0 {
		return netip.AddrPort{}, false
	}
	return cands[rand.IntN(len(cands))], true
}

// Share returns up to ShareMax addresses, in random order, for a peer at
// asker: those with a completed round in the last day and no failure since
// that the asker can reach, except the asker's own announced address. Every
// peer that can reach the same is given the same, drawn at most an hour
// ago; a list that held every address there was to share is drawn again as
// soon as there is another, so a node that has just started is not held to
// the few it knew for an hour.
func (b *AddressBook) Share(asker netip.Addr, own *netip.AddrPort) []netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := now()
	var fresh []netip.AddrPort
	for _, e := range b.entries {
		if e.Verified == nil || t > *e.Verified+shareFresh || e.Failures != 0 {
			continue
		}
		if !shareable(e.Address, asker) || b.isBanned(e.Address.Addr()) {
			continue
		}
		fresh = append(fresh, e.Address)
	}
	r := reachOf(asker)
	kept := b.shared[r]
	if kept != nil && (t < kept.drawn || t-kept.drawn >= shareKept) {
		kept = nil
	}
	if kept != nil && kept.whole {
		for _, a := range fresh {
			if !slices.Contains(kept.list, a) {
				kept = nil
				break
			}
		}
	}
	if kept == nil {
		whole := len(fresh) <= ShareMax
		rand.Shuffle(len(fresh), func(i, j int) { fresh[i], fresh[j] = fresh[j], fresh[i] })
		if len(fresh) > ShareMax {
			fresh = fresh[:ShareMax]
		}
		kept = &shared{drawn: t, list: fresh, whole: whole}
		b.shared[r] = kept
	}
	out := make([]netip.AddrPort, 0, len(kept.list))
	for _, a := range kept.list {
		if own == nil || canon(*own) != a {
			out = append(out, a)
		}
	}
	return out
}

// Ban bans an address's range for a day, dropping every address in it but a
// seed's, which is not connected until the ban ends; it reports whether it
// did. A range holding a configured peer is never banned, nor is this
// machine: banning it would ban every node on it.
func (b *AddressBook) Ban(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	k := banKey(a)
	for addr, e := range b.entries {
		if e.Source == SourceConfig && banKey(addr.Addr()) == k {
			return false
		}
	}
	t := now()
	for r, until := range b.banned {
		if until <= t {
			delete(b.banned, r)
		}
	}
	until := t + BanSeconds
	if old, ok := b.banned[k]; ok {
		until = max(old, until)
	} else if len(b.banned) >= BanMax {
		// The ban that ends soonest gives way.
		var soonest netip.Addr
		end := ^uint64(0)
		for r, u := range b.banned {
			if u < end || (u == end && r.Less(soonest)) {
				soonest, end = r, u
			}
		}
		delete(b.banned, soonest)
	}
	b.banned[k] = until
	for addr, e := range b.entries {
		if e.Source != SourceSeed && banKey(addr.Addr()) == k {
			delete(b.entries, addr)
			b.dirty = true
		}
	}
	for _, sh := range b.shared {
		sh.list = slices.DeleteFunc(sh.list, func(a netip.AddrPort) bool { return banKey(a.Addr()) == k })
	}
	return true
}

// Banned reports whether an address's range is banned.
func (b *AddressBook) Banned(a netip.Addr) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.isBanned(a)
}

func (b *AddressBook) isBanned(a netip.Addr) bool {
	until, ok := b.banned[banKey(a)]
	return ok && until > now()
}

// Len returns the number of known addresses.
func (b *AddressBook) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// Entries returns a copy of the book.
func (b *AddressBook) Entries() []BookEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BookEntry, 0, len(b.entries))
	for _, e := range b.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address.String() < out[j].Address.String() })
	return out
}

// Save writes peers.json when the book changed.
func (b *AddressBook) Save() error {
	b.mu.Lock()
	if b.path == "" || !b.dirty {
		b.mu.Unlock()
		return nil
	}
	stored := struct {
		Addresses []BookEntry `json:"addresses"`
	}{Addresses: make([]BookEntry, 0, len(b.entries))}
	for _, e := range b.entries {
		stored.Addresses = append(stored.Addresses, *e)
	}
	sort.Slice(stored.Addresses, func(i, j int) bool {
		return stored.Addresses[i].Address.String() < stored.Addresses[j].Address.String()
	})
	path := b.path
	b.mu.Unlock()
	data, _ := json.MarshalIndent(stored, "", "  ")
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	b.mu.Lock()
	b.dirty = false
	b.mu.Unlock()
	return nil
}
