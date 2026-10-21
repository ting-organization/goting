package node

import (
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }
func ip(s string) netip.Addr     { return netip.MustParseAddr(s) }

func TestAddressesTravelOnlyWhereReachable(t *testing.T) {
	cases := []struct {
		addr, peer string
		ok         bool
	}{
		{"8.8.8.8:1", "9.9.9.9", true},
		{"10.0.0.1:1", "9.9.9.9", false},
		{"10.0.0.1:1", "192.168.1.1", true},
		{"10.0.0.1:1", "127.0.0.1", true},
		{"127.0.0.1:1", "10.0.0.2", false},
		{"127.0.0.1:1", "127.0.0.1", true},
		{"100.64.0.1:1", "8.8.8.8", false},
		{"[fd00::1]:1", "[2a00::1]", false},
		{"192.0.2.1:1", "127.0.0.1", false},
		{"[2001:db8::1]:1", "[2a00::1]", false},
		{"[::ffff:8.8.8.8]:1", "9.9.9.9", true},
	}
	for _, c := range cases {
		peer := netip.MustParseAddr(trimBrackets(c.peer))
		if got := shareable(canon(ap(c.addr)), peer); got != c.ok {
			t.Errorf("shareable(%s, %s) = %v", c.addr, c.peer, got)
		}
	}
	b := NewAddressBook("")
	if b.Add(ap("8.8.8.8:0"), SourcePeer) || b.Add(ap("0.0.0.0:5"), SourcePeer) || b.Add(ap("224.0.0.1:5"), SourcePeer) {
		t.Fatal("unusable address learned")
	}
}

func trimBrackets(s string) string {
	if len(s) > 1 && s[0] == '[' {
		return s[1 : len(s)-1]
	}
	return s
}

func TestOnlyProvenFreshAddressesShared(t *testing.T) {
	b := NewAddressBook("")
	proven, unproven, failed, owner := ap("8.8.8.8:1"), ap("9.9.9.9:1"), ap("7.7.7.7:1"), ap("6.6.6.6:1")
	for _, a := range []netip.AddrPort{proven, unproven, failed, owner} {
		b.Add(a, SourcePeer)
	}
	b.Succeeded(proven)
	b.Succeeded(failed)
	b.Failed(failed)
	b.Succeeded(owner)
	got := b.Share(ip("5.5.5.5"), &owner)
	if len(got) != 1 || got[0] != proven {
		t.Fatalf("shared %v", got)
	}
	// A proven address older than a day is not shared.
	old := now() - shareFresh - 1
	b.entries[proven].Verified = &old
	if got := b.Share(ip("5.5.5.5"), nil); slices.Contains(got, proven) {
		t.Fatal("stale address shared")
	}
}

func TestSharedListsCappedAndFiltered(t *testing.T) {
	b := NewAddressBook("")
	var shared []netip.AddrPort
	for i := 0; i < 40; i++ {
		shared = append(shared, netip.AddrPortFrom(netip.AddrFrom4([4]byte{8, byte(i), 0, 1}), 1))
	}
	shared[0] = ap("10.0.0.1:1") // private, from a public peer: refused
	if n := b.AddShared(shared, ip("9.9.9.9")); n != ShareMax-1 {
		t.Fatalf("learned %d", n)
	}
}

func TestPicksPreferProvenPeersFromDistinctGroups(t *testing.T) {
	b := NewAddressBook("")
	a1, a2, a3, a4 := ap("8.8.1.1:1"), ap("8.8.2.2:1"), ap("9.9.1.1:1"), ap("7.7.1.1:1")
	for _, a := range []netip.AddrPort{a1, a2, a3, a4} {
		b.Add(a, SourcePeer)
	}
	b.Succeeded(a3)
	got := b.Pick(8, nil)
	if len(got) != 3 || got[0] != a3 {
		t.Fatalf("picked %v", got)
	}
	if slices.Contains(got, a1) && slices.Contains(got, a2) {
		t.Fatal("two peers from one /16")
	}
	// A busy address's group is taken.
	if got := b.Pick(8, map[netip.AddrPort]bool{ap("9.9.200.1:5"): true}); slices.Contains(got, a3) {
		t.Fatal("picked from a busy group")
	}
	// Failures back off: a minute, then two.
	b.Attempted(a4)
	b.Failed(a4)
	if slices.Contains(b.Pick(8, nil), a4) {
		t.Fatal("failed address retried at once")
	}
	b.entries[a4].Attempted = now() - 61
	if !slices.Contains(b.Pick(8, nil), a4) {
		t.Fatal("failed address not retried after a minute")
	}
	b.Failed(a4)
	if slices.Contains(b.Pick(8, nil), a4) {
		t.Fatal("second failure waits two minutes")
	}
	// Local addresses have no group.
	l := NewAddressBook("")
	for _, s := range []string{"10.0.0.1:1", "10.0.0.2:1", "10.0.0.3:1"} {
		l.Add(ap(s), SourcePeer)
	}
	if n := len(l.Pick(8, nil)); n != 3 {
		t.Fatalf("local picks %d", n)
	}
	// Probing tests unproven addresses only.
	if a, ok := b.Probe(nil); !ok || a == a3 {
		t.Fatalf("probe %v %v", a, ok)
	}
}

func TestFullBookEvictsUnprovenNeverConfigured(t *testing.T) {
	b := NewAddressBook("")
	cfg := ap("8.8.8.8:1")
	b.Configure([]netip.AddrPort{cfg})
	proven := ap("8.8.8.9:1")
	b.Add(proven, SourcePeer)
	b.Succeeded(proven)
	for i := 0; len(b.entries) < BookMax; i++ {
		b.Add(netip.AddrPortFrom(netip.AddrFrom4([4]byte{9, byte(i >> 8), byte(i), 1}), 1), SourcePeer)
	}
	for i := 0; i < 100; i++ {
		if !b.Add(netip.AddrPortFrom(netip.AddrFrom4([4]byte{11, 0, byte(i), 1}), 1), SourcePeer) {
			t.Fatal("full book refused an address")
		}
	}
	if b.Len() != BookMax || b.entries[cfg] == nil || b.entries[proven] == nil {
		t.Fatal("evicted a configured or proven address")
	}
	// Configuring anew forgets a peer no longer configured.
	b.Configure(nil)
	if b.entries[cfg] != nil {
		t.Fatal("unconfigured peer kept")
	}
}

func TestBans(t *testing.T) {
	b := NewAddressBook("")
	bad := ap("8.8.8.8:1")
	b.Add(bad, SourcePeer)
	if !b.Ban(bad.Addr()) || b.Len() != 0 || !b.Banned(ip("8.8.8.8")) || b.Add(bad, SourcePeer) {
		t.Fatal("ban")
	}
	b.banned[banKey(bad.Addr())] = now() - 1
	if b.Banned(bad.Addr()) || !b.Add(bad, SourcePeer) {
		t.Fatal("ban did not end")
	}
	// An IPv6 ban covers the /64.
	if !b.Ban(ip("2a00:1:2:3:4::1")) || !b.Banned(ip("2a00:1:2:3:ffff::9")) || b.Banned(ip("2a00:1:2:4::1")) {
		t.Fatal("ipv6 range")
	}
	// Loopback and configured ranges are never banned; configuring lifts one.
	b.Configure([]netip.AddrPort{ap("9.9.9.9:1")})
	if b.Ban(ip("127.0.0.1")) || b.Ban(ip("9.9.9.9")) {
		t.Fatal("exempt range banned")
	}
	b.Ban(ip("7.7.7.7"))
	b.Configure([]netip.AddrPort{ap("7.7.7.7:3")})
	if b.Banned(ip("7.7.7.7")) {
		t.Fatal("configuring did not lift the ban")
	}
	// The kept bans are capped; the soonest ending gives way.
	c := NewAddressBook("")
	for i := 0; i < BanMax+10; i++ {
		c.Ban(netip.AddrFrom4([4]byte{20, byte(i >> 16), byte(i >> 8), byte(i)}))
	}
	if len(c.banned) != BanMax {
		t.Fatalf("%d bans", len(c.banned))
	}
}

func TestBookSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	b := NewAddressBook(path)
	b.Add(ap("8.8.8.8:1"), SourceSeed)
	b.Succeeded(ap("8.8.8.8:1"))
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	r := NewAddressBook(path)
	e := r.Entries()
	if len(e) != 1 || e[0].Source != SourceSeed || e[0].Verified == nil {
		t.Fatalf("reloaded %+v", e)
	}
}
