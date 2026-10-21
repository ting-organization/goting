package vm

import (
	"math/bits"

	"github.com/ting-organization/goting/internal/types"
)

// Fixed prices (rules §15).
const (
	PagePrice       = 40000 // C per 64 KiB page of memory or stack
	StartPrice      = 40000 // setting up and tearing down an instance
	GlobalPrice     = 40
	TableEntryPrice = 4
	DataSegPrice    = 12
	HostCallPrice   = 94
	RootDispatch    = 1
	StackPageSlots  = 8192
	FrameRecord     = 3 // slots every active function keeps
)

// MemPrice holds the memory-size-dependent prices of rules §15.2.
type MemPrice struct {
	Load, Store, Word, Span uint64
}

var memPrices [65537]MemPrice

func init() {
	for p := 0; p <= 65536; p++ {
		memPrices[p] = computeMemPrice(uint32(p))
	}
}

func ceilDiv(a, b uint64) uint64 { return (a + b - 1) / b }

func computeMemPrice(pages uint32) MemPrice {
	switch {
	case pages <= 1:
		return MemPrice{Load: 18, Store: 1, Word: 4, Span: 0}
	case pages <= 16:
		return MemPrice{Load: 31, Store: 1, Word: 8, Span: 0}
	case pages <= 32:
		return MemPrice{Load: 80, Store: 1, Word: 8, Span: 0}
	}
	b := uint64(31 - bits.LeadingZeros32(pages))
	L := 16*b + (16*uint64(pages))>>b - 16
	d := L - 80 // L >= 80 above 32 pages
	sq := d * d
	store := 16 + ceilDiv(sq, 72)
	return MemPrice{Load: 770 + ceilDiv(sq, 48), Store: store, Word: 12, Span: store}
}

// PriceFor returns the prices at a memory size in pages.
func PriceFor(pages uint32) *MemPrice { return &memPrices[pages] }

// Bytes is bytes(n, s): word × ceil(n / 32) + span × s.
func (p *MemPrice) Bytes(n, spans uint64) uint64 {
	return p.Word*ceilDiv(n, 32) + p.Span*spans
}

// spans counts the nonempty lengths.
func spans(lens ...uint64) uint64 {
	var s uint64
	for _, l := range lens {
		if l > 0 {
			s++
		}
	}
	return s
}

// StackPages returns ⌈h / 8,192⌉ − 1, or 0 for h ≤ 8,192 (rules §15.4).
func StackPages(h uint64) uint64 {
	if h <= StackPageSlots {
		return 0
	}
	// ceil(h / 8192) without overflow for any u64 height.
	pages := h / StackPageSlots
	if h%StackPageSlots != 0 {
		pages++
	}
	return pages - 1
}

// Meter tracks the transaction's consumption. Frames are nested budgets
// that every debit made inside them is also charged to; a child's budget
// never exceeds what its parent has left, and from then on both are charged
// alike, so a frame's remaining allowance never exceeds its ancestors'.
// Each frame is therefore represented by one absolute limit on Used, and a
// debit fits every enclosing frame exactly when it fits the innermost one.
type Meter struct {
	Used  types.ResourceVector // consumed by the transaction's root frame
	Guard types.ResourceVector // block capacity left for this transaction
}

// Debit outcomes besides a receipt status.
const (
	DebitOK        = 0
	DebitBlockFull = 255
)

// Debit charges v against the frame with the given absolute limit. It
// returns DebitOK, the exhausted-resource status (3–6) of the first counter
// in C, S, D, G order that does not fit the frame, or DebitBlockFull when the
// frame can afford it but the block's capacity cannot. Nothing is charged
// unless every counter fits.
func (m *Meter) Debit(limit *types.ResourceVector, v types.ResourceVector) uint8 {
	for i := 0; i < 4; i++ {
		if v[i] > limit[i]-m.Used[i] {
			return types.StatusExhaustedC + uint8(i)
		}
	}
	for i := 0; i < 4; i++ {
		if v[i] > m.Guard[i]-m.Used[i] {
			return DebitBlockFull
		}
	}
	for i := 0; i < 4; i++ {
		m.Used[i] += v[i]
	}
	return DebitOK
}

// DebitC charges compute only.
func (m *Meter) DebitC(limit *types.ResourceVector, c uint64) uint8 {
	if c > limit[0]-m.Used[0] {
		return types.StatusExhaustedC
	}
	if c > m.Guard[0]-m.Used[0] {
		return DebitBlockFull
	}
	m.Used[0] += c
	return DebitOK
}

// Remaining returns the frame's budget minus its consumption.
func (m *Meter) Remaining(limit *types.ResourceVector) types.ResourceVector {
	var r types.ResourceVector
	for i := 0; i < 4; i++ {
		r[i] = limit[i] - m.Used[i]
	}
	return r
}
