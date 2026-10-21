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
	ShareMax   = 32
	BookMax    = 1024
	shareFresh = 24 * 3600
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

// AddressBook keeps the addresses this node knows and the ranges it bans.
// Bans are kept for this run only.
type AddressBook struct {
	mu      sync.Mutex
	entries map[netip.AddrPort]*BookEntry
	banned  map[netip.Addr]uint64
	path    string
	dirty   bool
}

// NewAddressBook loads peers.json from path, if given; a missing or
// unreadable file starts it empty, since it only holds hints.
func NewAddressBook(path string) *AddressBook {
	b := &AddressBook{entries: map[netip.AddrPort]*BookEntry{}, banned: map[netip.Addr]uint64{}, path: path}
	if path == "" {
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
	for i := range stored.Addresses {
		e := stored.Addresses[i]
		e.Address = canon(e.Address)
		if usable(e.Address) && len(b.entries) < BookMax {
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
// banned; it reports whether it did.
func (b *AddressBook) Add(a netip.AddrPort, src AddrSource) bool {
	a = canon(a)
	if !usable(a) {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.entries[a]; ok || b.isBanned(a.Addr()) {
		return false
	}
	return b.insert(a, src)
}

// AddShared adds the addresses one peer shared: at most ShareMax of them,
// and only those the peer could know of from where it is.
func (b *AddressBook) AddShared(addrs []netip.AddrPort, from netip.Addr) int {
	n := 0
	for i, a := range addrs {
		if i == ShareMax {
			break
		}
		if shareable(canon(a), from) && b.Add(a, SourcePeer) {
			n++
		}
	}
	return n
}

func (b *AddressBook) insert(a netip.AddrPort, src AddrSource) bool {
	if len(b.entries) >= BookMax && !b.evict() {
		return false
	}
	b.entries[a] = &BookEntry{Address: a, Source: src}
	b.dirty = true
	return true
}

// evict drops the least useful address: never a configured one, an
// unproven one before a proven one, the longest unproven and most failed
// first.
func (b *AddressBook) evict() bool {
	var worst *BookEntry
	key := func(e *BookEntry) (bool, uint64, uint32) {
		if e.Verified == nil {
			return false, 0, ^e.Failures
		}
		return true, *e.Verified, ^e.Failures
	}
	less := func(x, y *BookEntry) bool {
		xv, xt, xf := key(x)
		yv, yt, yf := key(y)
		if xv != yv {
			return !xv
		}
		if xt != yt {
			return xt < yt
		}
		if xf != yf {
			return xf < yf
		}
		return x.Address.String() < y.Address.String()
	}
	for _, e := range b.entries {
		if e.Source != SourceConfig && (worst == nil || less(e, worst)) {
			worst = e
		}
	}
	if worst == nil {
		return false
	}
	delete(b.entries, worst.Address)
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
// after a failure once the wait is over. A configured peer waits a minute.
func (b *AddressBook) Due(a netip.AddrPort) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[canon(a)]
	if !ok {
		return true
	}
	t := now()
	if e.Source == SourceConfig {
		return e.Failures == 0 || t >= e.Attempted+retryBase
	}
	return e.retry(t)
}

// Pick returns up to want discovered addresses to connect to, besides
// busy: proven ones first, most recently proven first, then unproven ones in
// random order; each from a network group none of the others is in.
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
		vi, vj := cands[i].Verified, cands[j].Verified
		switch {
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

// Probe returns one unproven address to test with a short connection when
// every connection slot is taken, so new nodes still become shareable.
func (b *AddressBook) Probe(busy map[netip.AddrPort]bool) (netip.AddrPort, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := now()
	var cands []netip.AddrPort
	for _, e := range b.entries {
		if e.Verified == nil && !busy[e.Address] && !b.isBanned(e.Address.Addr()) && e.retry(t) {
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
// that the asker can reach, except the asker's own announced address.
func (b *AddressBook) Share(asker netip.Addr, own *netip.AddrPort) []netip.AddrPort {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := now()
	var out []netip.AddrPort
	for _, e := range b.entries {
		if e.Verified == nil || t > *e.Verified+shareFresh || e.Failures != 0 {
			continue
		}
		if (own != nil && canon(*own) == e.Address) || !shareable(e.Address, asker) {
			continue
		}
		out = append(out, e.Address)
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	if len(out) > ShareMax {
		out = out[:ShareMax]
	}
	return out
}

// Ban bans an address's range for a day, dropping every address in it; it
// reports whether it did. A range holding a configured peer is never
// banned, nor is this machine: banning it would ban every node on it.
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
	for addr := range b.entries {
		if banKey(addr.Addr()) == k {
			delete(b.entries, addr)
			b.dirty = true
		}
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
