package types

import (
	"fmt"
	"math/bits"

	"github.com/ting-organization/goting/internal/codec"
)

// Resource indices in ResourceVector order (rules §2.2, §12).
const (
	C = 0 // compute
	S = 1 // state access
	D = 2 // data
	G = 3 // growth
)

// ResourceVector is four u64 counters in the order C, S, D, G.
type ResourceVector [4]uint64

func (v ResourceVector) String() string {
	return fmt.Sprintf("[C=%d S=%d D=%d G=%d]", v[0], v[1], v[2], v[3])
}

// Encode appends the 32-byte canonical encoding.
func (v ResourceVector) Encode(w *codec.Writer) {
	for _, x := range v {
		w.U64(x)
	}
}

// DecodeResourceVector reads 32 bytes.
func DecodeResourceVector(r *codec.Reader) ResourceVector {
	var v ResourceVector
	for i := range v {
		v[i] = r.U64()
	}
	return v
}

// AddChecked returns v + o and false when any component overflows u64.
func (v ResourceVector) AddChecked(o ResourceVector) (ResourceVector, bool) {
	var out ResourceVector
	for i := range v {
		s, carry := bits.Add64(v[i], o[i], 0)
		if carry != 0 {
			return out, false
		}
		out[i] = s
	}
	return out, true
}

// LessEq reports whether every component of v is at most the one in o.
func (v ResourceVector) LessEq(o ResourceVector) bool {
	for i := range v {
		if v[i] > o[i] {
			return false
		}
	}
	return true
}
