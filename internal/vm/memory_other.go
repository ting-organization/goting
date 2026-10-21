//go:build !unix

package vm

func bigMemory(n uint64) []byte { return make([]byte, n) }

func freeBigMemory([]byte) {}
