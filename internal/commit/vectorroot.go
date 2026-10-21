// Package commit implements the ordered-list commitment VectorRoot (rules §2.5).
package commit

import (
	"runtime"
	"sync"

	"github.com/ting-organization/goting/internal/hashing"
)

// parallelThreshold is the item count above which leaves are hashed on
// several goroutines.
const parallelThreshold = 256

// VectorRoot commits to the order, number and exact bytes of items:
//
//	count == 0: H(tag-root, u32(0))
//	otherwise:  leaves padded to a power of two, paired with tag-node,
//	            root = H(tag-root, u32(count) || tree_root)
func VectorRoot(tag string, items [][]byte) hashing.Hash {
	count := len(items)
	if count == 0 {
		x := hashing.New(tag + "-root")
		x.WriteU32(0)
		return x.Sum()
	}
	width := 1
	for width < count {
		width <<= 1
	}
	level := make([]hashing.Hash, width)
	leafTag := tag + "-leaf"
	padTag := tag + "-pad"
	hashLeaves := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if i < count {
				x := hashing.New(leafTag)
				x.WriteU64(uint64(i))
				x.WriteBytes(items[i])
				level[i] = x.Sum()
			} else {
				x := hashing.New(padTag)
				x.WriteU64(uint64(i))
				level[i] = x.Sum()
			}
		}
	}
	if width >= parallelThreshold {
		parallelRange(width, hashLeaves)
	} else {
		hashLeaves(0, width)
	}
	nodeTag := tag + "-node"
	for len(level) > 1 {
		next := level[:len(level)/2]
		pair := func(lo, hi int) {
			for k := lo; k < hi; k++ {
				x := hashing.New(nodeTag)
				x.Write(level[2*k][:])
				x.Write(level[2*k+1][:])
				next[k] = x.Sum()
			}
		}
		// Writing next[k] only overwrites level[k] for k < 2k, which has
		// already been read by every pair that needs it, except when
		// running in parallel; use a fresh slice in that case.
		if len(next) >= parallelThreshold {
			fresh := make([]hashing.Hash, len(next))
			src := level
			parallelRange(len(fresh), func(lo, hi int) {
				for k := lo; k < hi; k++ {
					x := hashing.New(nodeTag)
					x.Write(src[2*k][:])
					x.Write(src[2*k+1][:])
					fresh[k] = x.Sum()
				}
			})
			level = fresh
			continue
		}
		pair(0, len(next))
		level = next
	}
	x := hashing.New(tag + "-root")
	x.WriteU32(uint32(count))
	x.Write(level[0][:])
	return x.Sum()
}

// parallelRange splits [0, n) across GOMAXPROCS goroutines.
func parallelRange(n int, fn func(lo, hi int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	if workers <= 1 {
		fn(0, n)
		return
	}
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for lo := 0; lo < n; lo += chunk {
		hi := lo + chunk
		if hi > n {
			hi = n
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			fn(lo, hi)
		}(lo, hi)
	}
	wg.Wait()
}

// TransactionsRoot is VectorRoot("transactions", transactions).
func TransactionsRoot(txs [][]byte) hashing.Hash { return VectorRoot(hashing.TagTransactionsRoot, txs) }

// ReceiptsRoot is VectorRoot("receipts", receipts).
func ReceiptsRoot(receipts [][]byte) hashing.Hash {
	return VectorRoot(hashing.TagReceiptsRoot, receipts)
}
