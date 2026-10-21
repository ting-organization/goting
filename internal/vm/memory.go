package vm

import (
	"math/bits"
	"sync"
)

// Linear memory buffers are pooled. An invocation takes its memory from a
// pool and gives it back, zeroed, when it ends: a module's declared minimum
// memory, commonly a megabyte, is otherwise allocated afresh for every call,
// and a busy block's calls keep the collector running far more than they
// keep the interpreter. Charges never depend on this.
//
// Memories above the largest pooled size, which only programs declaring
// hundreds of MiB have, are mapped from the operating system outside the Go
// heap: its zero pages cost nothing until touched, and they go back the
// moment the invocation ends.
//
// Invariant: a pooled buffer is zero throughout its capacity. A buffer only
// ever exposes bytes below its length, extends its length only over bytes
// that are still zero, and is cleared below its length when it returns.
const (
	minMemClass = 16 // one 64 KiB page
	maxMemClass = 26 // buffers up to 64 MiB are pooled; larger ones are not
)

var memPools [maxMemClass + 1]sync.Pool

// memClass is the smallest k with 1<<k >= n.
func memClass(n uint64) int {
	k := bits.Len64(n - 1)
	return max(k, minMemClass)
}

// getMemory returns a zeroed buffer of length n.
func getMemory(n uint64) []byte {
	if n == 0 {
		return nil
	}
	k := memClass(n)
	if k > maxMemClass {
		return bigMemory(n)
	}
	if b, ok := memPools[k].Get().(*[]byte); ok {
		return (*b)[:n]
	}
	return make([]byte, n, uint64(1)<<k)
}

// putMemory clears a buffer's used part and pools it for reuse. The caller
// must hold no slice of it afterwards.
func putMemory(b []byte) {
	c := uint64(cap(b))
	if c > 1<<maxMemClass {
		freeBigMemory(b)
		return
	}
	if c == 0 || c&(c-1) != 0 {
		return
	}
	k := bits.TrailingZeros64(c)
	if k < minMemClass || k > maxMemClass {
		return
	}
	clear(b)
	b = b[:0]
	memPools[k].Put(&b)
}

// growMemory extends a buffer to length n, zero beyond its old length,
// moving to a larger buffer when its capacity does not suffice.
func growMemory(b []byte, n uint64) []byte {
	if n <= uint64(cap(b)) {
		return b[:n]
	}
	nb := getMemory(n)
	copy(nb, b)
	putMemory(b)
	return nb
}

// release gives the machine's memory back. The machine must not run again.
func (m *Machine) release() {
	if m == nil {
		return
	}
	putMemory(m.mem)
	m.mem = nil
}
