// Package p2p speaks the reference node's peer protocol
// (docs/peer-protocol.md): length-prefixed frames carrying requests,
// answers, error answers and announcements over TCP. The protocol is not
// consensus; it carries blocks, headers and transactions in their consensus
// encodings.
package p2p

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/netip"
	"unicode/utf8"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// Frame kinds.
const (
	KindRequest      = 1
	KindAnswer       = 2
	KindError        = 3
	KindAnnouncement = 4
)

// Limits (peer protocol "Limits").
const (
	MaxFrame      = 16 << 20
	MaxHelloFrame = 4 << 10
	MaxSlice      = 1 << 20
	MaxAgent      = 64
	MaxFeatures   = 16
	MaxFeature    = 32
	MaxLocator    = 32
	MaxPoolIDs    = 128
	MaxHeaders    = 2000
	MaxInventory  = 1024
	MaxItems      = 32
	MaxAddresses  = 32
	MaxErrorText  = 1024
	MaxWorkBytes  = 64
)

// ErrNotMessage reports a frame that breaks the framing or field rules.
var ErrNotMessage = errors.New("frame is no message")

// ReadFrame reads one frame. The buffer grows as bytes arrive, so a hostile
// length reserves nothing in advance.
func ReadFrame(r io.Reader, max int) ([]byte, error) {
	var lb [4]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return nil, err
	}
	return readBody(r, int(binary.LittleEndian.Uint32(lb[:])), max)
}

// readBody reads a frame's n bytes after its length.
func readBody(r io.Reader, n int, max int) ([]byte, error) {
	if n < 1 || n > max {
		return nil, fmt.Errorf("%w: frame length %d", ErrNotMessage, n)
	}
	const chunk = 64 << 10
	buf := make([]byte, 0, min(n, chunk))
	for len(buf) < n {
		k := min(n-len(buf), chunk)
		start := len(buf)
		buf = append(buf, make([]byte, k)...)
		if _, err := io.ReadFull(r, buf[start:]); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) < 1 || len(payload) > MaxFrame {
		return fmt.Errorf("frame of %d bytes out of range", len(payload))
	}
	buf := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)
	_, err := w.Write(buf)
	return err
}

// enc builds a frame's payload.
type enc struct{ b []byte }

func (e *enc) u8(v uint8)   { e.b = append(e.b, v) }
func (e *enc) u16(v uint16) { e.b = binary.LittleEndian.AppendUint16(e.b, v) }
func (e *enc) u32(v uint32) { e.b = binary.LittleEndian.AppendUint32(e.b, v) }
func (e *enc) u64(v uint64) { e.b = binary.LittleEndian.AppendUint64(e.b, v) }
func (e *enc) flag(v bool) {
	if v {
		e.u8(1)
	} else {
		e.u8(0)
	}
}
func (e *enc) id(h hashing.Hash) { e.b = append(e.b, h[:]...) }
func (e *enc) bytes(b []byte) {
	e.u32(uint32(len(b)))
	e.b = append(e.b, b...)
}
func (e *enc) name(s string) {
	e.u16(uint16(len(s)))
	e.b = append(e.b, s...)
}
func (e *enc) text(s string) { e.name(s) }
func (e *enc) ids(l []hashing.Hash) {
	e.u16(uint16(len(l)))
	for _, h := range l {
		e.id(h)
	}
}
func (e *enc) optU64(v *uint64) {
	e.flag(v != nil)
	if v != nil {
		e.u64(*v)
	}
}
func (e *enc) optID(v *hashing.Hash) {
	e.flag(v != nil)
	if v != nil {
		e.id(*v)
	}
}
func (e *enc) work(w *big.Int) {
	if w == nil || w.Sign() == 0 {
		e.u8(0)
		return
	}
	b := w.Bytes()
	e.u8(uint8(len(b)))
	e.b = append(e.b, b...)
}
func (e *enc) address(a netip.AddrPort) {
	ip := a.Addr()
	if ip.Is4() || ip.Is4In6() {
		e.u8(4)
		v := ip.Unmap().As4()
		e.b = append(e.b, v[:]...)
	} else {
		e.u8(6)
		v := ip.As16()
		e.b = append(e.b, v[:]...)
	}
	e.u16(a.Port())
}

// dec reads fields strictly; the first error sticks.
type dec struct {
	b   []byte
	off int
	err error
}

func (d *dec) fail(format string, args ...any) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: %s", ErrNotMessage, fmt.Sprintf(format, args...))
	}
}

func (d *dec) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || n > len(d.b)-d.off {
		d.fail("truncated")
		return nil
	}
	s := d.b[d.off : d.off+n : d.off+n]
	d.off += n
	return s
}

func (d *dec) u8() uint8 {
	if s := d.take(1); s != nil {
		return s[0]
	}
	return 0
}
func (d *dec) u16() uint16 {
	if s := d.take(2); s != nil {
		return binary.LittleEndian.Uint16(s)
	}
	return 0
}
func (d *dec) u32() uint32 {
	if s := d.take(4); s != nil {
		return binary.LittleEndian.Uint32(s)
	}
	return 0
}
func (d *dec) u64() uint64 {
	if s := d.take(8); s != nil {
		return binary.LittleEndian.Uint64(s)
	}
	return 0
}
func (d *dec) flag() bool {
	v := d.u8()
	if v > 1 {
		d.fail("flag %d", v)
	}
	return v == 1
}
func (d *dec) id() (h hashing.Hash) {
	if s := d.take(32); s != nil {
		copy(h[:], s)
	}
	return
}
func (d *dec) bytes(max int) []byte {
	n := d.u32()
	if d.err == nil && int64(n) > int64(max) {
		d.fail("bytes of %d exceed %d", n, max)
		return nil
	}
	return d.take(int(n))
}
func (d *dec) name(max int) string {
	n := int(d.u16())
	if d.err == nil && n > max {
		d.fail("name of %d bytes exceeds %d", n, max)
		return ""
	}
	s := d.take(n)
	for _, c := range s {
		if c < 0x20 || c > 0x7e {
			d.fail("name byte 0x%02x not printable", c)
			return ""
		}
	}
	return string(s)
}
func (d *dec) text(max int) string {
	n := int(d.u16())
	if d.err == nil && n > max {
		d.fail("text of %d bytes exceeds %d", n, max)
		return ""
	}
	s := d.take(n)
	if d.err == nil && !utf8.Valid(s) {
		d.fail("text is not UTF-8")
	}
	return string(s)
}
func (d *dec) count(min, max int) int {
	n := int(d.u16())
	if d.err == nil && (n < min || n > max) {
		d.fail("list of %d outside %d..%d", n, min, max)
		return 0
	}
	return n
}
func (d *dec) ids(min, max int) []hashing.Hash {
	n := d.count(min, max)
	if d.err != nil || n*32 > len(d.b)-d.off {
		d.fail("id list truncated")
		return nil
	}
	out := make([]hashing.Hash, n)
	for i := range out {
		out[i] = d.id()
	}
	return out
}
func (d *dec) optU64() *uint64 {
	if !d.flag() {
		return nil
	}
	v := d.u64()
	return &v
}
func (d *dec) optID() *hashing.Hash {
	if !d.flag() {
		return nil
	}
	v := d.id()
	return &v
}
func (d *dec) work() *big.Int {
	n := int(d.u8())
	if d.err == nil && n > MaxWorkBytes {
		d.fail("work of %d bytes", n)
		return nil
	}
	s := d.take(n)
	if d.err == nil && n > 0 && s[0] == 0 {
		d.fail("work has a leading zero byte")
	}
	return new(big.Int).SetBytes(s)
}
func (d *dec) address() netip.AddrPort {
	switch d.u8() {
	case 4:
		var a [4]byte
		copy(a[:], d.take(4))
		return netip.AddrPortFrom(netip.AddrFrom4(a), d.u16())
	case 6:
		var a [16]byte
		copy(a[:], d.take(16))
		ip := netip.AddrFrom16(a)
		if ip.Is4In6() {
			d.fail("IPv4 address mapped into IPv6")
		}
		return netip.AddrPortFrom(ip, d.u16())
	default:
		d.fail("address family")
		return netip.AddrPort{}
	}
}
func (d *dec) header() *types.Header {
	s := d.take(types.HeaderSize)
	if s == nil {
		return nil
	}
	h, _ := types.DecodeHeader(s)
	return h
}
func (d *dec) end() {
	if d.err == nil && d.off != len(d.b) {
		d.fail("%d bytes left over", len(d.b)-d.off)
	}
}
