// Package control is the node's local control channel, compatible with the
// reference node's (wiki: node/security): loopback TCP, frames of a u32
// little-endian length and JSON, and a handshake in which each side proves
// it holds the data directory's token without sending it.
//
//  1. The client sends {"nonce": hex32}.
//  2. The node answers {"nonce": hex32, "proof": HMAC(token, "ting-control-node" ‖
//     client nonce ‖ node nonce)}; the client checks it before sending more.
//  3. The client sends {"request": "<JSON text of {method, params}>", "proof":
//     HMAC(token, "ting-control-client" ‖ node nonce ‖ client nonce ‖ request)}.
//
// HMAC is HMAC-SHA-256 keyed with the token's 64 hex characters. The node
// answers {"ok": true, "result": ...} or {"ok": false, "error": "..."}.
package control

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// Limits and texts (reference node, transport.rs).
const (
	FrameMax     = 16 << 20
	helloMax     = 1024
	structureMax = 65_536
	frameTimeout = 5 * time.Second
	callTimeout  = 35 * time.Second
	maxConns     = 16
	nodeProof    = "ting-control-node"
	clientProof  = "ting-control-client"
	// NotThisNode is what a client says when the listener cannot prove it
	// holds the token.
	NotThisNode = "the control listener did not prove it holds this data directory's token: " +
		"runtime.json may be stale, or the token another node's"
)

// Methods the control channel serves.
var Methods = map[string]bool{"status": true, "peers": true, "add_peer": true, "remove_peer": true,
	"mine": true, "template": true, "solution": true, "miner": true, "votes": true, "submit": true,
	"api": true, "invalidate": true, "reconsider": true, "stop": true}

// Handler answers a request; params is the request's params value.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

func mac(token string, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, []byte(token))
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

func authorized(expected, supplied string) bool {
	return len(expected) == len(supplied) && subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) == 1
}

func nonce() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("no randomness for a nonce: %v", err)
	}
	return b, nil
}

func nonceOf(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, errors.New("expected nonce")
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(s) != 64 || hex.EncodeToString(b) != s {
		return nil, errors.New("expected a 32-byte nonce")
	}
	return b, nil
}

func writeFrame(c net.Conn, v any, max int) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) == 0 || len(b) > max || uint64(len(b)) > 0xffffffff {
		return errors.New("frame size limit")
	}
	c.SetWriteDeadline(time.Now().Add(callTimeout))
	buf := binary.LittleEndian.AppendUint32(make([]byte, 0, 4+len(b)), uint32(len(b)))
	_, err = c.Write(append(buf, b...))
	return err
}

func readFrame(c net.Conn, max int, deadline time.Duration) (map[string]any, error) {
	c.SetReadDeadline(time.Now().Add(deadline))
	var lb [4]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint32(lb[:]))
	if n == 0 || n > max {
		return nil, errors.New("frame size limit")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		return nil, err
	}
	structure := 0
	for _, x := range b {
		if x == '[' || x == '{' || x == ',' || x == ':' {
			structure++
		}
	}
	if structure > structureMax {
		return nil, errors.New("frame structure limit")
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Server serves the control channel.
type Server struct {
	Token   string
	Handler Handler
	// RequestMax bounds a request frame: a request may carry a transaction
	// as large as a block holds, in hex.
	RequestMax func() int
	// Stop is called after the answer to a successful "stop".
	Stop func()
	Log  *slog.Logger
}

// Serve accepts loopback connections until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	slots := make(chan struct{}, maxConns)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			defer c.Close()
			s.serve(ctx, c)
		}()
	}
}

func (s *Server) serve(ctx context.Context, c net.Conn) {
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err != nil || !ap.Addr().Unmap().IsLoopback() {
		return
	}
	hello, err := readFrame(c, helloMax, frameTimeout)
	if err != nil {
		return
	}
	theirs, err := nonceOf(hello["nonce"])
	if err != nil {
		return
	}
	ours, err := nonce()
	if err != nil {
		return
	}
	proof := mac(s.Token, []byte(nodeProof), theirs, ours)
	if writeFrame(c, map[string]string{"nonce": hex.EncodeToString(ours), "proof": hex.EncodeToString(proof)}, FrameMax) != nil {
		return
	}
	most := FrameMax
	if s.RequestMax != nil {
		most = max(most, s.RequestMax())
	}
	frame, err := readFrame(c, most, frameTimeout)
	if err != nil {
		return
	}
	request, _ := frame["request"].(string)
	supplied, _ := frame["proof"].(string)
	expected := hex.EncodeToString(mac(s.Token, []byte(clientProof), ours, theirs, []byte(request)))
	if !authorized(expected, supplied) {
		writeFrame(c, reply(nil, errors.New("unauthorized")), FrameMax)
		return
	}
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(request), &req); err != nil {
		return
	}
	var result any
	if Methods[req.Method] {
		result, err = s.Handler(ctx, req.Method, req.Params)
	} else {
		err = errors.New("unknown control method")
	}
	writeFrame(c, reply(result, err), FrameMax)
	if req.Method == "stop" && err == nil && s.Stop != nil {
		s.Stop()
	}
}

func reply(result any, err error) map[string]any {
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true, "result": result}
}

// Call sends one request to the node at addr, which must be a loopback
// address, and returns its result.
func Call(ctx context.Context, addr netip.AddrPort, token, method string, params any) (json.RawMessage, error) {
	if !addr.Addr().Unmap().IsLoopback() {
		return nil, errors.New("control address must be loopback")
	}
	d := net.Dialer{Timeout: frameTimeout}
	c, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ours, err := nonce()
	if err != nil {
		return nil, err
	}
	if err := writeFrame(c, map[string]string{"nonce": hex.EncodeToString(ours)}, FrameMax); err != nil {
		return nil, err
	}
	hello, err := readFrame(c, helloMax, frameTimeout)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, errors.New("control handshake timed out")
		}
		return nil, err
	}
	theirs, err := nonceOf(hello["nonce"])
	if err != nil {
		msg, _ := hello["error"].(string)
		if msg == "" {
			msg = "no nonce"
		}
		return nil, fmt.Errorf("control handshake failed: %s", msg)
	}
	proof, _ := hello["proof"].(string)
	if !authorized(hex.EncodeToString(mac(token, []byte(nodeProof), ours, theirs)), proof) {
		return nil, errors.New(NotThisNode)
	}
	if params == nil {
		params = map[string]any{}
	}
	rb, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return nil, err
	}
	request := string(rb)
	cp := hex.EncodeToString(mac(token, []byte(clientProof), theirs, ours, rb))
	if err := writeFrame(c, map[string]string{"request": request, "proof": cp}, 0xffffffff); err != nil {
		return nil, err
	}
	c.SetReadDeadline(time.Now().Add(callTimeout))
	var lb [4]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, errors.New("control request timed out")
		}
		return nil, err
	}
	n := int(binary.LittleEndian.Uint32(lb[:]))
	if n == 0 || n > FrameMax {
		return nil, errors.New("control frame size limit")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		return nil, err
	}
	var r struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *string         `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if !r.OK {
		if r.Error != nil {
			return nil, errors.New(*r.Error)
		}
		return nil, errors.New("invalid control reply")
	}
	return r.Result, nil
}
