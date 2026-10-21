package control

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

const token = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func serve(t *testing.T, stopped chan struct{}) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{Token: token, Stop: func() { close(stopped) },
		Handler: func(ctx context.Context, method string, p json.RawMessage) (any, error) {
			if method == "status" {
				return map[string]any{"params": json.RawMessage(p)}, nil
			}
			if method == "submit" {
				return nil, errors.New("mempool is full")
			}
			return map[string]any{"stopping": true}, nil
		}}
	go s.Serve(ctx, ln)
	return netip.MustParseAddrPort(ln.Addr().String())
}

func TestControlChannel(t *testing.T) {
	stopped := make(chan struct{})
	addr := serve(t, stopped)
	ctx := context.Background()
	res, err := Call(ctx, addr, token, "status", map[string]any{"x": 1})
	if err != nil || !strings.Contains(string(res), `"x":1`) {
		t.Fatalf("status: %s %v", res, err)
	}
	if _, err := Call(ctx, addr, token, "submit", nil); err == nil || err.Error() != "mempool is full" {
		t.Fatalf("error answer: %v", err)
	}
	if _, err := Call(ctx, addr, token, "config", nil); err == nil || err.Error() != "unknown control method" {
		t.Fatalf("unlisted method: %v", err)
	}
	// A client with another token learns the node cannot prove it holds
	// this one, and sends nothing more.
	other := strings.Repeat("ab", 32)
	if _, err := Call(ctx, addr, other, "status", nil); err == nil || err.Error() != NotThisNode {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := Call(ctx, netip.MustParseAddrPort("192.0.2.1:1"), token, "status", nil); err == nil {
		t.Fatal("non-loopback control address accepted")
	}
	if _, err := Call(ctx, addr, token, "stop", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop not called")
	}
}

// A request whose proof does not match is answered "unauthorized".
func TestControlUnauthorized(t *testing.T) {
	addr := serve(t, make(chan struct{}))
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ours := make([]byte, 32)
	writeFrame(c, map[string]string{"nonce": hex.EncodeToString(ours)}, FrameMax)
	hello, err := readFrame(c, helloMax, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ := hex.DecodeString(hello["nonce"].(string))
	if hello["proof"] != hex.EncodeToString(mac(token, []byte(nodeProof), ours, theirs)) {
		t.Fatal("node proof")
	}
	writeFrame(c, map[string]string{"request": `{"method":"stop","params":{}}`, "proof": strings.Repeat("00", 32)}, FrameMax)
	r, err := readFrame(c, FrameMax, 5*time.Second)
	if err != nil || r["ok"] != false || r["error"] != "unauthorized" {
		t.Fatalf("reply %v %v", r, err)
	}
	// Frames past the hello bound are refused.
	c2, _ := net.Dial("tcp", addr.String())
	defer c2.Close()
	var lb [4]byte
	binary.LittleEndian.PutUint32(lb[:], helloMax+1)
	c2.Write(lb[:])
	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, _ := c2.Read(make([]byte, 1)); n != 0 {
		t.Fatal("oversized hello answered")
	}
}
