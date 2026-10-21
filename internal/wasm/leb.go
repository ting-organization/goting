package wasm

import "errors"

var (
	errTruncated   = errors.New("truncated")
	errNonMinimal  = errors.New("non-minimal LEB128")
	errLEBOverflow = errors.New("LEB128 value out of range")
)

// reader decodes the module's canonical variable-width encoding.
type reader struct {
	b   []byte
	pos int
}

func (r *reader) remaining() int { return len(r.b) - r.pos }

func (r *reader) eof() bool { return r.pos >= len(r.b) }

func (r *reader) byte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, errTruncated
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

func (r *reader) bytes(n uint32) ([]byte, error) {
	if uint64(n) > uint64(len(r.b)-r.pos) {
		return nil, errTruncated
	}
	s := r.b[r.pos : r.pos+int(n) : r.pos+int(n)]
	r.pos += int(n)
	return s, nil
}

// u32 reads a minimal unsigned LEB128 of at most 32 bits (rules §17.1, §26.6).
func (r *reader) u32() (uint32, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		if r.pos >= len(r.b) {
			return 0, errTruncated
		}
		c := r.b[r.pos]
		r.pos++
		if i == 4 {
			if c&0x80 != 0 {
				return 0, errLEBOverflow
			}
			if c > 0x0f {
				return 0, errLEBOverflow
			}
		}
		v |= uint32(c&0x7f) << (7 * uint(i))
		if c&0x80 == 0 {
			if i > 0 && c == 0 {
				return 0, errNonMinimal
			}
			return v, nil
		}
	}
	return 0, errLEBOverflow
}

// s32 reads a minimal signed LEB128 of at most 32 bits.
func (r *reader) s32() (int32, error) {
	v, err := r.signed(32)
	return int32(v), err
}

// s64 reads a minimal signed LEB128 of at most 64 bits.
func (r *reader) s64() (int64, error) { return r.signed(64) }

func (r *reader) signed(width uint) (int64, error) {
	start := r.pos
	maxBytes := int((width + 6) / 7)
	var result int64
	var shift uint
	var c byte
	n := 0
	for {
		if r.pos >= len(r.b) {
			return 0, errTruncated
		}
		if n == maxBytes {
			return 0, errLEBOverflow
		}
		c = r.b[r.pos]
		r.pos++
		n++
		if shift < 64 {
			result |= int64(c&0x7f) << shift
		}
		shift += 7
		if c&0x80 == 0 {
			break
		}
	}
	// Sign-extend from the payload width actually read.
	if shift < 64 && c&0x40 != 0 {
		result |= -1 << shift
	}
	// The final byte of a maximal-length encoding carries bits beyond the
	// width; they must all repeat the sign.
	if n == maxBytes {
		used := width - 7*uint(maxBytes-1) // payload bits of the last byte that belong to the value
		// bits [used-1, 6] of c must equal the sign bit (bit used-1).
		sign := (c >> (used - 1)) & 1
		for b := used; b < 7; b++ {
			if (c>>b)&1 != sign {
				return 0, errLEBOverflow
			}
		}
	}
	if width == 32 && (result < -1<<31 || result > 1<<31-1) {
		return 0, errLEBOverflow
	}
	// Minimality: re-encoding must reproduce the bytes exactly.
	if encodedSignedLen(result) != n {
		return 0, errNonMinimal
	}
	_ = start
	return result, nil
}

func encodedSignedLen(v int64) int {
	n := 0
	for {
		payload := byte(v & 0x7f)
		v >>= 7
		n++
		if (v == 0 && payload&0x40 == 0) || (v == -1 && payload&0x40 != 0) {
			return n
		}
	}
}

// AppendU32 appends the minimal unsigned LEB128 of v.
func AppendU32(b []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
		} else {
			return append(b, c)
		}
	}
}

// AppendS64 appends the minimal signed LEB128 of v.
func AppendS64(b []byte, v int64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		done := (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0)
		if done {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// priceU32 is the permissive reader of the code-price scan (rules §26.1):
// at most five bytes, value fits u32, redundant zero groups allowed.
func priceU32(b []byte, at int) (uint32, int, bool) {
	var v uint64
	for i := 0; i < 5; i++ {
		if at >= len(b) {
			return 0, at, false
		}
		c := b[at]
		at++
		v |= uint64(c&0x7f) << (7 * uint(i))
		if c&0x80 == 0 {
			if v > 0xffffffff {
				return 0, at, false
			}
			return uint32(v), at, true
		}
	}
	return 0, at, false
}
