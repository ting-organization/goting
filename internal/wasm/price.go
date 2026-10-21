package wasm

import "math/bits"

// CodePrice computes the code price of a module's bytes from its section
// framing alone (rules §15.7, §26.1). It never validates and accepts invalid
// code. Exact prices beyond the u64 range saturate at the u64 maximum: every
// deployment pays something before its code price, so such a price is always
// refused.
func CodePrice(code []byte) uint64 {
	var p sat
	p.add(14400)
	at := len(code)
	if at > 8 {
		at = 8
	}
	p.addMul(60, uint64(at))
	for at < len(code) {
		start := at
		id := code[at]
		at++
		size, next, ok := priceU32(code, at)
		if !ok || uint64(next)+uint64(size) > uint64(len(code)) {
			p.addMul(60, uint64(len(code)-start))
			break
		}
		at = next
		payload := code[at : at+int(size)]
		rate := uint64(60)
		switch id {
		case 9:
			rate = 175
		case 6:
			rate = 100
		case 11:
			rate = 6
		}
		p.addMul(60, uint64(at-start))
		p.addMul(rate, uint64(size))
		if id == 3 || id == 11 {
			cnt, _, ok := priceU32(payload, 0)
			if !ok {
				cnt = 0
			}
			if id == 3 {
				p.addMul(1215, uint64(cnt))
			} else {
				p.addMul(446, uint64(cnt))
			}
		}
		at += int(size)
	}
	return p.v
}

// sat is a saturating u64 accumulator.
type sat struct{ v uint64 }

func (s *sat) add(x uint64) {
	r, c := bits.Add64(s.v, x, 0)
	if c != 0 {
		r = ^uint64(0)
	}
	s.v = r
}

func (s *sat) addMul(a, b uint64) {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		s.v = ^uint64(0)
		return
	}
	s.add(lo)
}
