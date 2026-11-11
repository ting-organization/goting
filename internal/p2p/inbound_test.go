package p2p

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/hashing"
)

// admitFrom offers the server a connection from ip at a time.
func admitFrom(s *Server, ip string, at time.Time) (*serverConn, bool) {
	a, b := net.Pipe()
	b.Close()
	sc := newServerConn(a, netip.AddrPortFrom(netip.MustParseAddr(ip), 4000), at)
	return sc, s.admit(sc)
}

func newInbound(max int) *Server {
	return &Server{Backend: &fakeBackend{chain: hashing.Hash{1}}, MaxInbound: max}
}

func open(s *Server, sc *serverConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.conns[sc]
	return ok
}

func ip4(a, b, c, d int) string {
	return netip.AddrFrom4([4]byte{byte(a), byte(b), byte(c), byte(d)}).String()
}

// Many addresses of one network cannot hold every inbound slot: a newcomer
// from elsewhere takes the place of the newest of the network holding the
// most.
func TestManyAddressesOfOneNetworkCannotHoldEverySlot(t *testing.T) {
	s := newInbound(16)
	t0 := time.Now()
	var crowd []*serverConn
	for i := 0; i < 16; i++ {
		sc, ok := admitFrom(s, ip4(9, 9, i, 1), t0)
		if !ok {
			t.Fatal("slot refused while there was room")
		}
		crowd = append(crowd, sc)
	}
	later := t0.Add(time.Minute)
	if _, ok := admitFrom(s, "8.8.8.8", later); !ok {
		t.Fatal("a newcomer from another network was refused by a crowded node")
	}
	if open(s, crowd[15]) || !open(s, crowd[0]) {
		t.Fatal("the newest of the crowding network did not give way")
	}
	// One more of the crowding network takes nobody's place of another's.
	if _, ok := admitFrom(s, ip4(9, 9, 200, 1), later); ok && s.Inbound() != 16 {
		t.Fatalf("%d connections", s.Inbound())
	}
}

// With every slot taken and no network crowding, the latest peers to
// deliver a block and the longest connected half keep their places.
func TestFullNodeTakesNewcomersInPlaceOfItsLeastUseful(t *testing.T) {
	s := newInbound(8)
	t0 := time.Now()
	var conns []*serverConn
	for i := 0; i < 8; i++ {
		sc, ok := admitFrom(s, ip4(10+i, 1, 1, 1), t0.Add(time.Duration(i)*time.Second))
		if !ok {
			t.Fatal("refused")
		}
		conns = append(conns, sc)
	}
	// The two newest delivered blocks.
	s.delivered(conns[7])
	s.delivered(conns[6])
	later := t0.Add(time.Minute)
	if _, ok := admitFrom(s, "50.1.1.1", later); !ok {
		t.Fatal("a full node refused a newcomer")
	}
	// Of the six others the newest half is 3, 4, 5: the newest gives way.
	if open(s, conns[5]) {
		t.Fatal("the newest connection that delivered nothing kept its place")
	}
	for _, i := range []int{0, 1, 2, 6, 7} {
		if !open(s, conns[i]) {
			t.Fatalf("connection %d gave way", i)
		}
	}
	// The network that just took a place takes no other's for a while, and
	// a connection just taken is not displaced.
	if _, ok := admitFrom(s, "50.1.2.2", later.Add(time.Second)); ok {
		t.Fatal("one network took two places in a row")
	}
	s.mu.Lock()
	clear(s.took)
	for c := range s.conns {
		c.since = later // all just taken
	}
	s.mu.Unlock()
	if _, ok := admitFrom(s, "60.1.1.1", later.Add(time.Second)); ok {
		t.Fatal("a connection that had not settled gave way")
	}
}

// This machine's own connections give way only to each other, and one
// address range holds at most its share.
func TestLocalConnectionsAndRangeShare(t *testing.T) {
	s := newInbound(4)
	t0 := time.Now()
	var local []*serverConn
	for i := 0; i < 4; i++ {
		sc, _ := admitFrom(s, "127.0.0.1", t0)
		local = append(local, sc)
	}
	if _, ok := admitFrom(s, "8.8.8.8", t0.Add(time.Minute)); ok {
		for _, c := range local {
			if !open(s, c) {
				return // crowded by this machine: allowed
			}
		}
		t.Fatal("more connections than slots")
	}
	r := newInbound(64)
	for i := 0; i < 8; i++ {
		if _, ok := admitFrom(r, "8.8.8.8", t0); !ok {
			t.Fatal("refused within the range's share")
		}
	}
	if _, ok := admitFrom(r, "8.8.8.8", t0); ok {
		t.Fatal("a ninth connection from one address")
	}
}

// A request is charged for the bytes its answer carries: a pending answer
// is not two units whatever it holds.
func TestAnswersAreChargedByTheirBytes(t *testing.T) {
	big := Encode(&Items{Items: []Item{{ID: hashing.Hash{1}, Tx: make([]byte, 300<<10)}}})
	if c := sliceCost(&PendingRequest{}, big); c < 5 {
		t.Fatalf("a 300 KiB pending answer cost %v units", c)
	}
	if c := sliceCost(&StateRequest{}, big); c != 0 {
		t.Fatalf("state: %v", c)
	}
	if maxAnswer(&RawRequest{Length: 100000}) > 101000 || maxAnswer(&StateRequest{}) > 64<<10 ||
		maxAnswer(&AddressesRequest{}) > 8<<10 {
		t.Fatal("answer bounds")
	}
	// An error answer fits every bound, also that of a block's last few
	// bytes.
	longest := len(EncodeError(strings.Repeat("x", 2*MaxErrorText)))
	for _, req := range []Message{&RawRequest{Length: 1}, &TransactionRawRequest{Length: 1}, &StateRequest{},
		&HeadersRequest{}, &InventoryRequest{}, &AddressesRequest{}, &Import{}, &Submit{}, &PendingRequest{},
		&DeferredRequest{}} {
		if most := maxAnswer(req); most < longest {
			t.Fatalf("%T: an error answer of %d bytes is over the bound of %d", req, longest, most)
		}
	}
}

// Inbound connections give way by the network group the address book
// counts its shares by, and addresses with no group by their range.
func TestInboundAndBookAgreeOnNetworks(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "20.20.1.1", "2a01:4f8:1:2::3", "::ffff:8.8.4.4"} {
		a := netip.MustParseAddr(s)
		g, ok := Group(a)
		if !ok || networkOf(a) != g {
			t.Fatalf("%s: group %q (%v), inbound network %q", s, g, ok, networkOf(a))
		}
	}
	if g, _ := Group(netip.MustParseAddr("20.20.1.1")); g != "20.20.0.0/16" {
		t.Fatal(g)
	}
	if g, _ := Group(netip.MustParseAddr("2a01:4f8:1:2::3")); g != "2a01:4f8::/32" {
		t.Fatal(g)
	}
	// Private, carrier-grade NAT, loopback and documentation addresses
	// have no group: each range stands for itself.
	for _, s := range []string{"10.1.2.3", "100.64.1.1", "127.0.0.1", "192.0.2.7", "fd00::1", "2001:db8::1"} {
		a := netip.MustParseAddr(s)
		if _, ok := Group(a); ok || networkOf(a) != rangeOf(a) {
			t.Fatalf("%s: inbound network %q", s, networkOf(a))
		}
	}
}
