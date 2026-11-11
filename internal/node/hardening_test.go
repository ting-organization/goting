package node

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/hashing"
)

// One address announcing many ports, or one network many addresses, takes
// its share of the book and no more, and cannot push out the others.
func TestOneAddressOrNetworkCannotTakeOverTheBook(t *testing.T) {
	b := NewAddressBook("")
	honest := []netip.AddrPort{ap("8.8.8.8:1"), ap("7.7.7.7:1")}
	for _, a := range honest {
		b.Add(a, SourcePeer)
	}
	seed := ap("6.6.6.6:18442")
	b.AddSeed(seed)
	// 2,000 ports of one address, as inbound hellos announce them.
	for port := 1; port <= 2000; port++ {
		b.AddAnnounced(netip.AddrPortFrom(ip("9.9.9.9"), uint16(port)))
	}
	held := 0
	for _, e := range b.Entries() {
		if e.Address.Addr() == ip("9.9.9.9") {
			held++
		}
	}
	if held != perIP {
		t.Fatalf("one address holds %d places", held)
	}
	// A network group: at most its share however many addresses it has,
	// and at most an answer's worth an hour from one source.
	for i := 0; i < 500; i++ {
		b.Add(netip.AddrPortFrom(netip.AddrFrom4([4]byte{20, 20, byte(i >> 8), byte(i)}), 1), SourcePeer)
	}
	inGroup := 0
	for _, e := range b.Entries() {
		if g, _ := group(e.Address.Addr()); g == "20.20.0.0/16" {
			inGroup++
		}
	}
	if inGroup != perGroup {
		t.Fatalf("one network holds %d places", inGroup)
	}
	var shared []netip.AddrPort
	for i := 0; i < 32; i++ {
		shared = append(shared, netip.AddrPortFrom(netip.AddrFrom4([4]byte{byte(30 + i), 1, 1, 1}), 1))
	}
	if n := b.AddShared(shared, ip("40.40.1.1")); n != ShareMax {
		t.Fatalf("learned %d", n)
	}
	more := []netip.AddrPort{ap("99.1.1.1:1")}
	if n := b.AddShared(more, ip("40.40.2.2")); n != 0 {
		t.Fatal("a network added more than its hour's worth")
	}
	if n := b.AddShared(more, ip("41.41.1.1")); n != 1 {
		t.Fatal("another network was held to the first one's count")
	}
	for _, a := range append(honest, seed) {
		if _, ok := b.entries[a]; !ok {
			t.Fatalf("%v was pushed out", a)
		}
	}
}

// Every peer of the same reach is given the same addresses for an hour.
func TestSameAddressesForAnHour(t *testing.T) {
	b := NewAddressBook("")
	for i := 0; i < 200; i++ {
		a := netip.AddrPortFrom(netip.AddrFrom4([4]byte{byte(1 + i), 2, 3, 4}), 1)
		b.Add(a, SourcePeer)
		b.Succeeded(a)
	}
	first := b.Share(ip("5.5.5.5"), nil)
	if len(first) != ShareMax {
		t.Fatalf("shared %d", len(first))
	}
	for i := 0; i < 50; i++ {
		if got := b.Share(netip.AddrFrom4([4]byte{200, byte(i), 1, 1}), nil); !slices.Equal(got, first) {
			t.Fatal("asking again told something new")
		}
	}
	// An hour on, the answer is drawn again.
	b.shared[reachPublic].drawn -= shareKept
	if got := b.Share(ip("5.5.5.5"), nil); slices.Equal(got, first) {
		t.Fatal("the same 32 of 200 were drawn twice")
	}
	// A list that held every address there was is drawn again as soon as
	// there is another.
	c := NewAddressBook("")
	one, two := ap("8.8.8.8:1"), ap("9.9.9.9:1")
	c.Add(one, SourcePeer)
	c.Succeeded(one)
	if got := c.Share(ip("5.5.5.5"), nil); len(got) != 1 {
		t.Fatal(got)
	}
	c.Add(two, SourcePeer)
	c.Succeeded(two)
	if got := c.Share(ip("5.5.5.5"), nil); len(got) != 2 {
		t.Fatal("a new node was held to the few addresses it knew")
	}
}

// A ban keeps a seed's address, to be connected once it ends; seeds are
// picked after every other address and never probed or given way.
func TestSeedsStayInTheBook(t *testing.T) {
	b := NewAddressBook("")
	seed, other := ap("6.6.6.6:18442"), ap("8.8.8.8:1")
	b.AddSeed(seed)
	b.Add(other, SourcePeer)
	if got := b.Pick(2, nil); len(got) != 2 || got[0] != other {
		t.Fatalf("picked %v", got)
	}
	if a, ok := b.Probe(nil); !ok || a == seed {
		t.Fatal("a seed was probed")
	}
	if !b.Ban(seed.Addr()) || !b.IsSeed(seed) {
		t.Fatal("a ban dropped the seed's address")
	}
	if b.SeedDue() || b.Due(seed) || slices.Contains(b.Pick(2, nil), seed) {
		t.Fatal("a banned seed is connected")
	}
	b.banned[banKey(seed.Addr())] = now() - 1
	if !b.SeedDue() || !slices.Contains(b.Pick(2, nil), seed) {
		t.Fatal("the seed is not connected once the ban ended")
	}
	for i := 0; i < 3*BookMax; i++ {
		b.Add(netip.AddrPortFrom(netip.AddrFrom4([4]byte{byte(11 + i%200), byte(i >> 8), byte(i), 1}), 1), SourcePeer)
	}
	if !b.IsSeed(seed) {
		t.Fatal("a full book dropped a seed")
	}
}

// The miner waits for its node only while something moves.
func TestMinerWaitsOnlyWhileTheNodeMoves(t *testing.T) {
	var w behindWait
	t0 := time.Now()
	patience := 70 * time.Second
	tip := hashing.Hash{1}
	if w.waits(tip, 10, false, false, patience, t0) {
		t.Fatal("waits though not behind")
	}
	if !w.waits(tip, 10, true, false, patience, t0) {
		t.Fatal("does not wait for a node that just fell behind")
	}
	// Headers nobody serves: nothing moves, and the miner mines.
	if w.waits(tip, 10, true, false, patience, t0.Add(71*time.Second)) {
		t.Fatal("waits for good on headers without blocks")
	}
	// A download stores blocks before it moves the tip: that is progress.
	var d behindWait
	d.waits(tip, 10, true, false, patience, t0)
	if !d.waits(tip, 11, true, false, patience, t0.Add(60*time.Second)) ||
		!d.waits(tip, 11, true, false, patience, t0.Add(120*time.Second)) {
		t.Fatal("a stored block did not count as progress")
	}
	// A side branch forgotten lowers the count: no progress.
	if d.waits(tip, 5, true, false, patience, t0.Add(131*time.Second)) {
		t.Fatal("a falling count counted as progress")
	}
	// A replay is waited for.
	if !d.waits(tip, 5, true, true, patience, t0.Add(500*time.Second)) {
		t.Fatal("does not wait for a replay")
	}
}

// REST holds each client to its share of connections, and gives a place
// back when a connection closes.
func TestRESTConnectionLimits(t *testing.T) {
	n := newTestNode(t, Config{})
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := LimitListener(inner, 4).(*limitListener)
	srv := NewAPIServer(n.API(APIOptions{}))
	go srv.Serve(ln)
	defer srv.Close()
	// Four idle connections take every place; a fifth waits in the queue
	// and is served once one closes.
	var idle []net.Conn
	for i := 0; i < 4; i++ {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, c)
	}
	waitFor(t, "four places taken", func() bool { return len(ln.slots) == 4 })
	fifth, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer fifth.Close()
	io.WriteString(fifth, "GET /health HTTP/1.1\r\nHost: x\r\n\r\n")
	fifth.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := fifth.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection beyond the total was served")
	}
	idle[0].Close()
	fifth.SetReadDeadline(time.Now().Add(5 * time.Second))
	line := make([]byte, 12)
	if _, err := io.ReadFull(fifth, line); err != nil || string(line) != "HTTP/1.1 200" {
		t.Fatalf("after a place was given back: %q %v", line, err)
	}
	for _, c := range idle[1:] {
		c.Close()
	}
	// Per client: a remote address or /64 holds at most restPerClient.
	l := LimitListener(&fakeListener{from: "9.9.9.9:1", n: restPerClient + 5}, 512).(*limitListener)
	taken := 0
	for {
		c, err := l.Accept()
		if err != nil {
			break
		}
		_ = c
		taken++
	}
	if taken != restPerClient {
		t.Fatalf("one client took %d connections", taken)
	}
}

type fakeListener struct {
	from string
	n    int
}

type fakeConn struct {
	net.Conn
	remote net.Addr
}

func (c *fakeConn) RemoteAddr() net.Addr { return c.remote }
func (c *fakeConn) Close() error         { return nil }

func (l *fakeListener) Accept() (net.Conn, error) {
	if l.n == 0 {
		return nil, net.ErrClosed
	}
	l.n--
	return &fakeConn{remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(l.from))}, nil
}
func (l *fakeListener) Close() error   { return nil }
func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

// A batch storage read's body is bounded before it is parsed, and a panic
// in a handler ends the request, not the node.
func TestRESTBodyLimitAndPanics(t *testing.T) {
	n := newTestNode(t, Config{})
	api := n.API(APIOptions{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewAPIServer(api)
	go srv.Serve(LimitListener(ln, 0))
	defer srv.Close()
	url := "http://" + ln.Addr().String()
	body := `{"reads":[` + strings.Repeat("{},", 400_000) + `{}]}`
	r, err := http.Post(url+"/v1/storage", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized batch: %d", r.StatusCode)
	}
	// The largest honest batch still passes the size check.
	ok := `{"reads":[` + strings.TrimSuffix(strings.Repeat(`{"contract":"`+strings.Repeat("ab", 32)+`","key":"`+strings.Repeat("cd", 32)+`"},`, batchMax), ",") + `]}`
	r, err = http.Post(url+"/v1/storage", "application/json", strings.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("the largest batch: %d", r.StatusCode)
	}

	// A panic recovered leaves the locks free, so the node goes on.
	exited := make(chan int, 1)
	exit = func(code int) { exited <- code }
	livenessWait = 200 * time.Millisecond
	defer func() { exit, livenessWait = osExit, 60*time.Second }()
	n.panicked("test", "boom", nil)
	select {
	case <-exited:
		t.Fatal("the node gave up though its locks were free")
	case <-time.After(400 * time.Millisecond):
	}
	// One that left a lock held ends the process, to be started again.
	n.tx.mu.Lock()
	n.panicked("test", "boom", nil)
	select {
	case code := <-exited:
		if code != 2 {
			t.Fatal(code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a node with a lock held for good did not give up")
	}
	n.tx.mu.Unlock()
}

// Each relayed transaction is fetched and sent once a connection, and one
// refused for want of room is tried again once the tip has moved.
func TestRelayOnceAConnection(t *testing.T) {
	r := newRelay()
	a, b := hashing.Hash{1}, hashing.Hash{2}
	r.fetched[a], r.fetched[b] = false, true
	r.sent[a], r.sent[b] = true, false
	r.retry(hashing.Hash{9})
	if _, ok := r.fetched[a]; !ok {
		t.Fatal("a fetched transaction was forgotten")
	}
	if _, ok := r.fetched[b]; ok {
		t.Fatal("one refused for want of room is not tried again at a new tip")
	}
	if _, ok := r.sent[a]; ok {
		t.Fatal("one the peer had no room for is not sent again at a new tip")
	}
	r.fetched[b] = true
	r.retry(hashing.Hash{9})
	if _, ok := r.fetched[b]; !ok {
		t.Fatal("retried again at the same tip")
	}
	if !wantsRoom(ErrPoolFull) || !wantsRoom(ErrDeferredFull) || wantsRoom(ErrIncluded) || wantsRoom(nil) {
		t.Fatal("refusals for want of room")
	}
	// The sorted listings are reused for a moment, then made anew.
	n := newTestNode(t, Config{})
	ids1, _ := n.TxIDs(false, nil, 10)
	if _, err := n.SubmitTransaction(callTx(hashing.Hash{7}, nil, [4]uint64{1000, 10, 1000, 0})); err != nil {
		t.Fatal(err)
	}
	n.tx.mu.Lock()
	n.tx.listed[0].made = time.Now().Add(-time.Second)
	n.tx.mu.Unlock()
	ids2, _ := n.TxIDs(false, nil, 10)
	if len(ids1) != 0 || len(ids2) != 1 || !bytes.Equal(ids2[0][:], ids2[0][:]) {
		t.Fatalf("listings %d then %d", len(ids1), len(ids2))
	}
}
