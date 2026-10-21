package commit

import (
	"encoding/binary"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
)

// naiveRoot follows rules §2.5 literally.
func naiveRoot(tag string, items [][]byte) hashing.Hash {
	if len(items) == 0 {
		return hashing.H(tag+"-root", []byte{0, 0, 0, 0})
	}
	width := 1
	for width < len(items) {
		width *= 2
	}
	level := make([]hashing.Hash, width)
	for i := 0; i < width; i++ {
		idx := binary.LittleEndian.AppendUint64(nil, uint64(i))
		if i < len(items) {
			ln := binary.LittleEndian.AppendUint32(nil, uint32(len(items[i])))
			level[i] = hashing.H(tag+"-leaf", idx, ln, items[i])
		} else {
			level[i] = hashing.H(tag+"-pad", idx)
		}
	}
	for len(level) > 1 {
		next := make([]hashing.Hash, len(level)/2)
		for k := range next {
			next[k] = hashing.H(tag+"-node", level[2*k][:], level[2*k+1][:])
		}
		level = next
	}
	cnt := binary.LittleEndian.AppendUint32(nil, uint32(len(items)))
	return hashing.H(tag+"-root", cnt, level[0][:])
}

func TestVectorRoot(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 31, 255, 256, 257, 1000, 2048} {
		items := make([][]byte, n)
		for i := range items {
			items[i] = []byte{byte(i), byte(i >> 8), 7}
		}
		if got, want := VectorRoot("transactions", items), naiveRoot("transactions", items); got != want {
			t.Fatalf("n=%d: mismatch", n)
		}
	}
	// Position matters: swapping two items changes the root.
	a := [][]byte{{1}, {2}}
	b := [][]byte{{2}, {1}}
	if VectorRoot("receipts", a) == VectorRoot("receipts", b) {
		t.Fatal("order not committed")
	}
}
