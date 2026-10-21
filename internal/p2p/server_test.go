package p2p

import (
	"context"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/hashing"
)

type fakeBackend struct {
	mu       sync.Mutex
	chain    hashing.Hash
	imported [][]byte
	ann      chan *Announcement
	punished []netip.Addr
}

func (f *fakeBackend) ChainID() hashing.Hash { return f.chain }
func (f *fakeBackend) State() *State {
	return &State{Version: 1, ChainID: f.chain, Agent: "fake/1", Height: 7, Work: big.NewInt(99),
		Locator: []hashing.Hash{{1}}}
}
func (f *fakeBackend) Headers(loc []hashing.Hash) ([][]byte, error) {
	return nil, errors.New("no common ancestor")
}
func (f *fakeBackend) Block(id hashing.Hash) ([]byte, error) {
	if id == (hashing.Hash{7}) {
		return make([]byte, 3000000), nil
	}
	return nil, ErrNotFound
}
func (f *fakeBackend) CheckHeader(h []byte) (bool, hashing.Hash, uint64, error) {
	if h[0] == 0xbb {
		return false, hashing.Hash{}, 0, &InvalidData{Text: "invalid block: test"}
	}
	return false, hashing.Hash{9}, 4 << 20, nil
}
func (f *fakeBackend) Banned(netip.Addr) bool { return false }
func (f *fakeBackend) Punish(a netip.Addr) {
	f.mu.Lock()
	f.punished = append(f.punished, a)
	f.mu.Unlock()
}
func (f *fakeBackend) Import(raw []byte, from netip.AddrPort) (hashing.Hash, bool, error) {
	if raw[0] == 0xbb {
		return hashing.Hash{}, false, &InvalidData{Text: "invalid block: test"}
	}
	f.mu.Lock()
	f.imported = append(f.imported, raw)
	f.mu.Unlock()
	return hashing.Hash{9}, false, nil
}
func (f *fakeBackend) TxBound() uint64                             { return 100 }
func (f *fakeBackend) Transaction(id hashing.Hash) ([]byte, error) { return nil, ErrNotFound }
func (f *fakeBackend) Submit(raw []byte, nb *uint64, from netip.AddrPort) (hashing.Hash, bool, error) {
	return hashing.Hash{5}, true, nil
}
func (f *fakeBackend) Inventory(d bool, a *hashing.Hash) ([]hashing.Hash, *hashing.Hash) {
	return nil, nil
}
func (f *fakeBackend) Items(d bool, ids []hashing.Hash) []Item                { return nil }
func (f *fakeBackend) Addresses(netip.Addr, *netip.AddrPort) []netip.AddrPort { return nil }
func (f *fakeBackend) Subscribe() (<-chan *Announcement, func())              { return f.ann, func() {} }
func (f *fakeBackend) Learned(netip.AddrPort)                                 {}

func startServer(t *testing.T) (*Server, *fakeBackend, string) {
	t.Helper()
	be := &fakeBackend{chain: hashing.Hash{0xc1}, ann: make(chan *Announcement, 4)}
	srv := &Server{Backend: be, Nonce: 42}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return srv, be, ln.Addr().String()
}

func rawConn(t *testing.T, addr string, frames ...[]byte) net.Conn {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		WriteFrame(nc, f)
	}
	nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	return nc
}

func expectError(t *testing.T, nc net.Conn, text string) {
	t.Helper()
	p, err := ReadFrame(nc, MaxFrame)
	if err != nil {
		t.Fatalf("expected error %q: %v", text, err)
	}
	_, err = DecodeAnswer(p)
	var ea *ErrorAnswer
	if !errors.As(err, &ea) || ea.Text != text {
		t.Fatalf("expected error %q, got %v", text, err)
	}
}

func expectClosed(t *testing.T, nc net.Conn) {
	t.Helper()
	if _, err := ReadFrame(nc, MaxFrame); err == nil {
		t.Fatal("connection still open")
	}
}

func TestServerHelloRules(t *testing.T) {
	_, be, addr := startServer(t)
	// Another chain.
	nc := rawConn(t, addr, Encode(&Hello{Version: 1, ChainID: hashing.Hash{0xc2}, Agent: "x", Nonce: 1}))
	expectError(t, nc, "peer version or genesis mismatch")
	expectClosed(t, nc)
	// Version 0.
	nc = rawConn(t, addr, Encode(&Hello{Version: 0, ChainID: be.chain, Agent: "x", Nonce: 1}))
	expectError(t, nc, "peer version or genesis mismatch")
	// A first frame that is not a hello.
	nc = rawConn(t, addr, Encode(&StateRequest{}))
	expectError(t, nc, "peer version or genesis mismatch")
	// Its own nonce.
	nc = rawConn(t, addr, Encode(&Hello{Version: 1, ChainID: be.chain, Agent: "x", Nonce: 42}))
	expectError(t, nc, "connected to itself")
	// A second hello.
	hello := Encode(&Hello{Version: 1, ChainID: be.chain, Agent: "x", Nonce: 1})
	nc = rawConn(t, addr, hello, hello)
	if p, err := ReadFrame(nc, MaxFrame); err != nil || p[0] != KindAnswer {
		t.Fatal("no state answer")
	}
	expectError(t, nc, "unknown peer method")
	expectClosed(t, nc)
	// An unknown tag ends the connection without an answer.
	nc = rawConn(t, addr, hello, []byte{KindRequest, 99})
	ReadFrame(nc, MaxFrame) // state
	expectClosed(t, nc)
	// A later version is accepted.
	nc = rawConn(t, addr, Encode(&Hello{Version: 2, ChainID: be.chain, Agent: "x", Nonce: 1}))
	if p, err := ReadFrame(nc, MaxFrame); err != nil || p[0] != KindAnswer {
		t.Fatal("version 2 refused")
	}
}

func TestServerRequests(t *testing.T) {
	_, be, addr := startServer(t)
	ctx := context.Background()
	c, err := Dial(ctx, addr, &Hello{Version: 1, ChainID: be.chain, Agent: "client", Nonce: 1, Announce: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Peer.Height != 7 || c.Peer.Agent != "fake/1" {
		t.Fatalf("state %+v", c.Peer)
	}
	if _, err := c.Do(ctx, &HeadersRequest{Locator: []hashing.Hash{{1}}}); err == nil {
		t.Fatal("expected no common ancestor")
	}
	// A large block in slices.
	m, err := c.Do(ctx, &RawRequest{ID: hashing.Hash{7}, Offset: 2 << 20, Length: MaxSlice})
	if err != nil {
		t.Fatal(err)
	}
	if s := m.(*Slice); s.Total != 3000000 || len(s.Bytes) != 3000000-(2<<20) {
		t.Fatalf("slice %d of %d", len(s.Bytes), s.Total)
	}
	// A sliced import: Received, then Taken.
	blk := make([]byte, MaxSlice+500)
	m, err = c.Do(ctx, &Import{Offset: 0, Total: uint64(len(blk)), Bytes: blk[:MaxSlice]})
	if err != nil || m.(*Received).Bytes != MaxSlice {
		t.Fatalf("first slice: %v %v", m, err)
	}
	m, err = c.Do(ctx, &Import{Offset: MaxSlice, Total: uint64(len(blk)), Bytes: blk[MaxSlice:]})
	if err != nil || m.(*Taken).ID != (hashing.Hash{9}) {
		t.Fatalf("last slice: %v %v", m, err)
	}
	// A slice with nothing being pushed, out of order and empty slices.
	if _, err := c.Do(ctx, &Import{Offset: 5, Total: 10, Bytes: []byte{1}}); err == nil ||
		err.(*ErrorAnswer).Text != "nothing being pushed" {
		t.Fatalf("nothing pushed: %v", err)
	}
	if _, err := c.Do(ctx, &Submit{Offset: 0, Total: 10, Bytes: []byte{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, &Submit{Offset: 5, Total: 10, Bytes: []byte{1}}); err == nil ||
		err.(*ErrorAnswer).Text != "slice out of order" {
		t.Fatalf("out of order: %v", err)
	}
	if _, err := c.Do(ctx, &Submit{Offset: 0, Total: 10}); err == nil || err.(*ErrorAnswer).Text != "empty slice" {
		t.Fatalf("empty: %v", err)
	}
	if _, err := c.Do(ctx, &Submit{Offset: 0, Total: 1000, Bytes: []byte{1}}); err == nil ||
		err.(*ErrorAnswer).Text != "transaction larger than the data capacity" {
		t.Fatalf("too large: %v", err)
	}
	// Announcements arrive between answers.
	be.ann <- &Announcement{Tip: hashing.Hash{3}, Height: 8, Work: big.NewInt(100)}
	select {
	case a := <-c.Announcements:
		if a.Height != 8 {
			t.Fatal("announcement")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no announcement")
	}
	// Invalid data: an error answer, then the connection closes and the
	// address would be banned (loopback is exempt).
	bad := append([]byte{0xbb}, make([]byte, 400)...)
	if _, err := c.Do(ctx, &Import{Offset: 0, Total: uint64(len(bad)), Bytes: bad}); err == nil ||
		err.(*ErrorAnswer).Text != "invalid block: test" {
		t.Fatalf("invalid: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connection stayed open after invalid data")
	}
	be.mu.Lock()
	punished := len(be.punished)
	be.mu.Unlock()
	if punished != 1 {
		t.Fatalf("punished %d times", punished)
	}
}

func TestBucketWaits(t *testing.T) {
	b := &bucket{tokens: 0, last: time.Now()}
	start := time.Now()
	if err := b.take(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("20 units at 200/s took %v", d)
	}
}
