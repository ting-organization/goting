// Package codec implements Ting's canonical fixed-width byte encoding
// (rules §2.1–2.2): little-endian integers of fixed width, big-endian u256
// targets, opaque 32-byte strings and u32-length-prefixed byte strings.
//
// The Reader never allocates based on an untrusted length: every length is
// checked against the remaining input first, and byte strings are returned as
// sub-slices of the input.
package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrTruncated is returned when the input ends before a field does.
var ErrTruncated = errors.New("codec: truncated input")

// ErrTrailing is returned when bytes remain after a complete value.
var ErrTrailing = errors.New("codec: trailing bytes")

// Writer appends canonical encodings to a buffer.
type Writer struct {
	Buf []byte
}

// NewWriter returns a writer with the given capacity hint.
func NewWriter(capacity int) *Writer { return &Writer{Buf: make([]byte, 0, capacity)} }

func (w *Writer) U8(v uint8) { w.Buf = append(w.Buf, v) }

func (w *Writer) U16(v uint16) { w.Buf = binary.LittleEndian.AppendUint16(w.Buf, v) }

func (w *Writer) U32(v uint32) { w.Buf = binary.LittleEndian.AppendUint32(w.Buf, v) }

func (w *Writer) U64(v uint64) { w.Buf = binary.LittleEndian.AppendUint64(w.Buf, v) }

// Raw appends bytes without a prefix (bytes32, u256be, u128 as stored).
func (w *Writer) Raw(b []byte) { w.Buf = append(w.Buf, b...) }

// Bytes appends Bytes(b) = u32(len(b)) || b.
func (w *Writer) Bytes(b []byte) {
	w.U32(uint32(len(b)))
	w.Buf = append(w.Buf, b...)
}

// Len returns the number of bytes written.
func (w *Writer) Len() int { return len(w.Buf) }

// Reader consumes canonical encodings from a byte slice. The first error is
// sticky: once set, every further read returns zero values.
type Reader struct {
	b   []byte
	off int
	err error
}

// NewReader reads from b.
func NewReader(b []byte) *Reader { return &Reader{b: b} }

// Err returns the first error encountered.
func (r *Reader) Err() error { return r.err }

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int { return len(r.b) - r.off }

// Offset returns the number of bytes consumed.
func (r *Reader) Offset() int { return r.off }

// Fail records err if no error is recorded yet.
func (r *Reader) Fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// Failf records a formatted error.
func (r *Reader) Failf(format string, args ...any) { r.Fail(fmt.Errorf(format, args...)) }

func (r *Reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b)-r.off {
		r.err = ErrTruncated
		return nil
	}
	s := r.b[r.off : r.off+n : r.off+n]
	r.off += n
	return s
}

func (r *Reader) U8() uint8 {
	s := r.take(1)
	if s == nil {
		return 0
	}
	return s[0]
}

func (r *Reader) U16() uint16 {
	s := r.take(2)
	if s == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(s)
}

func (r *Reader) U32() uint32 {
	s := r.take(4)
	if s == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(s)
}

func (r *Reader) U64() uint64 {
	s := r.take(8)
	if s == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(s)
}

// Fixed reads exactly n raw bytes (a sub-slice of the input).
func (r *Reader) Fixed(n int) []byte { return r.take(n) }

// Into copies exactly len(dst) raw bytes into dst.
func (r *Reader) Into(dst []byte) {
	s := r.take(len(dst))
	if s != nil {
		copy(dst, s)
	}
}

// Bytes reads Bytes(x): a u32 length then that many bytes. The length is
// checked against the remaining input before anything is returned.
func (r *Reader) Bytes() []byte {
	n := r.U32()
	if r.err != nil {
		return nil
	}
	if uint64(n) > uint64(len(r.b)-r.off) {
		r.err = ErrTruncated
		return nil
	}
	return r.take(int(n))
}

// End records ErrTrailing when unread bytes remain.
func (r *Reader) End() {
	if r.err == nil && r.off != len(r.b) {
		r.err = ErrTrailing
	}
}
