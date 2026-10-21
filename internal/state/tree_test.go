package state

import (
	"bytes"
	"math/rand"
	"sort"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
)

// refRoot computes hash(R, d) directly from the definition in rules §6.2.
func refRoot(recs map[Key][]byte) hashing.Hash {
	keys := make([]Key, 0, len(recs))
	for k := range recs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	return refHash(keys, recs, 0)
}

func refHash(keys []Key, recs map[Key][]byte, d int) hashing.Hash {
	switch len(keys) {
	case 0:
		return hashing.EmptyStateRoot
	case 1:
		return leafHash(&keys[0], recs[keys[0]])
	}
	var r0, r1 []Key
	for _, k := range keys {
		if bit(&k, d) == 0 {
			r0 = append(r0, k)
		} else {
			r1 = append(r1, k)
		}
	}
	l := refHash(r0, recs, d+1)
	r := refHash(r1, recs, d+1)
	return nodeHash(d, &l, &r)
}

func randKey(rng *rand.Rand) Key {
	var k Key
	rng.Read(k[:])
	return k
}

func TestEmptyAndSingle(t *testing.T) {
	if Empty.Root() != hashing.EmptyStateRoot {
		t.Fatal("empty root")
	}
	k := Key{1}
	tr := Empty.Apply([]Change{{Key: k, Value: []byte("v")}})
	if tr.Root() != leafHash(&k, []byte("v")) {
		t.Fatal("single record root must be its leaf")
	}
	tr = tr.Apply([]Change{{Key: k, Delete: true}})
	if tr.Root() != hashing.EmptyStateRoot || tr.Len() != 0 {
		t.Fatal("delete to empty")
	}
}

func TestDeepSplit(t *testing.T) {
	// Two keys differing only in their last bit put leaves at depth 256.
	var a, b Key
	a[31] = 0
	b[31] = 1
	recs := map[Key][]byte{a: {1}, b: {2}}
	tr := Empty.Apply([]Change{{Key: a, Value: []byte{1}}, {Key: b, Value: []byte{2}}})
	if tr.Root() != refRoot(recs) {
		t.Fatal("deep split root")
	}
	// Add a key that differs at bit 0 and one in the middle.
	var c, d Key
	c[0] = 0x80
	d[15] = 0x10
	recs[c] = []byte{}
	recs[d] = []byte{4, 4}
	tr = tr.Apply([]Change{{Key: c, Value: []byte{}}, {Key: d, Value: []byte{4, 4}}})
	if tr.Root() != refRoot(recs) {
		t.Fatal("mixed root")
	}
	delete(recs, a)
	tr = tr.Apply([]Change{{Key: a, Delete: true}})
	if tr.Root() != refRoot(recs) {
		t.Fatal("after delete")
	}
}

func TestRandomAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	recs := map[Key][]byte{}
	tr := Empty
	var keys []Key
	for round := 0; round < 60; round++ {
		var changes []Change
		n := rng.Intn(40) + 1
		if round == 30 {
			n = 3000 // exercise parallel rehash
		}
		for i := 0; i < n; i++ {
			switch {
			case len(keys) > 0 && rng.Intn(4) == 0:
				k := keys[rng.Intn(len(keys))]
				changes = append(changes, Change{Key: k, Delete: true})
				delete(recs, k)
			case len(keys) > 0 && rng.Intn(3) == 0:
				k := keys[rng.Intn(len(keys))]
				v := make([]byte, rng.Intn(5))
				rng.Read(v)
				changes = append(changes, Change{Key: k, Value: v})
				recs[k] = v
			default:
				k := randKey(rng)
				// Occasionally share a long prefix with an existing key.
				if len(keys) > 0 && rng.Intn(5) == 0 {
					k = keys[rng.Intn(len(keys))]
					k[31] ^= byte(1 << uint(rng.Intn(8)))
				}
				v := make([]byte, rng.Intn(5))
				rng.Read(v)
				changes = append(changes, Change{Key: k, Value: v})
				recs[k] = v
				keys = append(keys, k)
			}
		}
		prev := tr
		prevRoot := prev.Root()
		tr = tr.Apply(changes)
		if got, want := tr.Root(), refRoot(recs); got != want {
			t.Fatalf("round %d: root mismatch", round)
		}
		if tr.Len() != len(recs) {
			t.Fatalf("round %d: len %d want %d", round, tr.Len(), len(recs))
		}
		if prev.Root() != prevRoot {
			t.Fatalf("round %d: previous version changed", round)
		}
		for k, v := range recs {
			got, ok := tr.Get(k)
			if !ok || !bytes.Equal(got, v) {
				t.Fatalf("round %d: get mismatch", round)
			}
		}
	}
	// Bulk build agrees.
	var all []Change
	for k, v := range recs {
		all = append(all, Change{Key: k, Value: v})
	}
	if Build(all).Root() != tr.Root() {
		t.Fatal("build root mismatch")
	}
}

func TestOverlayCheckpoints(t *testing.T) {
	base := Empty.Apply([]Change{{Key: Key{1}, Value: []byte("a")}})
	o := NewOverlay(base)
	cp0 := o.Checkpoint()
	o.Set(Key{2}, []byte("b"))
	cp1 := o.Checkpoint()
	o.Delete(Key{1})
	o.Set(Key{2}, []byte("c"))
	if _, ok := o.Get(Key{1}); ok {
		t.Fatal("deleted")
	}
	o.Revert(cp1)
	if v, _ := o.Get(Key{2}); string(v) != "b" {
		t.Fatal("revert to cp1")
	}
	if v, ok := o.Get(Key{1}); !ok || string(v) != "a" {
		t.Fatal("restored delete")
	}
	o.Revert(cp0)
	if _, ok := o.Get(Key{2}); ok {
		t.Fatal("revert to cp0")
	}
	o.Set(Key{3}, nil)
	if v, ok := o.Get(Key{3}); !ok || len(v) != 0 {
		t.Fatal("empty present")
	}
	tr := o.Commit()
	if tr.Len() != 2 {
		t.Fatalf("commit len %d", tr.Len())
	}
}

func BenchmarkApply1000(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	var recs []Change
	for i := 0; i < 100000; i++ {
		recs = append(recs, Change{Key: randKey(rng), Value: []byte("value")})
	}
	tr := Build(recs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch := make([]Change, 1000)
		for j := range ch {
			ch[j] = Change{Key: randKey(rng), Value: []byte("x")}
		}
		tr.Apply(ch)
	}
}
