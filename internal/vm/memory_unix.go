//go:build unix

package vm

import (
	"sync"
	"syscall"
	"unsafe"
)

// mapped records the buffers bigMemory mapped, by their first byte, with
// their mapped length.
var mapped sync.Map

// bigMemory returns a zeroed buffer of n bytes mapped outside the Go heap,
// or from the heap if mapping fails.
func bigMemory(n uint64) []byte {
	page := uint64(syscall.Getpagesize())
	size := (n + page - 1) / page * page
	b, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return make([]byte, n)
	}
	mapped.Store(unsafe.Pointer(&b[0]), b)
	return b[:n]
}

// freeBigMemory unmaps a buffer bigMemory mapped; one it allocated from the
// heap is left to the collector.
func freeBigMemory(b []byte) {
	if cap(b) == 0 {
		return
	}
	if m, ok := mapped.LoadAndDelete(unsafe.Pointer(&b[:1][0])); ok {
		syscall.Munmap(m.([]byte))
	}
}
