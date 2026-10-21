package state

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// mapSource is a stored tree in maps, standing in for a database snapshot.
type mapSource struct {
	branches map[Position][]byte
	records  map[Key][]byte
}

func (m *mapSource) Branch(p Position) (Desc, Desc) {
	b, ok := m.branches[p]
	if !ok {
		panic(&StorageError{Err: errBranchHash})
	}
	l, r, err := DecodeBranch(b)
	if err != nil {
		panic(&StorageError{Err: err})
	}
	return l, r
}

func (m *mapSource) Record(k Key) ([]byte, bool) {
	v, ok := m.records[k]
	return v, ok
}

// store writes t, derived from the stored tree base, as a new version of
// src, leaving src itself as it was.
func store(base, t Tree, src *mapSource, keys []Key) (*mapSource, Tree) {
	next := &mapSource{branches: map[Position][]byte{}, records: map[Key][]byte{}}
	for p, b := range src.branches {
		next.branches[p] = b
	}
	for k, v := range src.records {
		next.records[k] = v
	}
	puts, dels := Delta(base, t, keys)
	for _, p := range dels {
		delete(next.branches, p)
	}
	for _, w := range puts {
		next.branches[w.Pos] = w.Record
	}
	for _, k := range keys {
		if v, ok := t.Get(k); ok {
			next.records[k] = v
		} else {
			delete(next.records, k)
		}
	}
	return next, FromRoot(t.RootDesc(), t.Len(), next)
}

func allBranches(t Tree) map[Position][]byte {
	out := map[Position][]byte{}
	puts, _ := Delta(Empty, t, nil)
	for _, w := range puts {
		out[w.Pos] = w.Record
	}
	return out
}

func TestStoredTreeMatchesMemory(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 9))
	var pool []Key
	key := func() Key {
		if len(pool) > 0 && r.IntN(3) == 0 {
			return pool[r.IntN(len(pool))]
		}
		var k Key
		for i := range k {
			k[i] = byte(r.Uint32())
		}
		// Shared prefixes: deep splits and single-sided runs.
		k[0], k[1] = byte(r.IntN(3)), byte(r.IntN(2))
		if r.IntN(4) == 0 {
			copy(k[2:20], make([]byte, 18))
		}
		pool = append(pool, k)
		return k
	}
	changes := func(n int) []Change {
		cs := make([]Change, n)
		for i := range cs {
			cs[i] = Change{Key: key(), Value: []byte{byte(r.Uint32()), byte(i)}, Delete: r.IntN(4) == 0}
		}
		return cs
	}
	mem := Empty
	src := &mapSource{branches: map[Position][]byte{}, records: map[Key][]byte{}}
	base := Empty
	stored := Empty
	var pending []Key
	var olds []struct {
		tree Tree
		mem  Tree
	}
	for round := 0; round < 300; round++ {
		cs := changes(1 + r.IntN(40))
		mem = mem.Apply(cs)
		stored = stored.Apply(cs)
		for _, c := range cs {
			pending = append(pending, c.Key)
		}
		if mem.Root() != stored.Root() || mem.Len() != stored.Len() {
			t.Fatalf("round %d: root or count differs (%d, %d)", round, mem.Len(), stored.Len())
		}
		for i := 0; i < 20; i++ {
			k := key()
			mv, mok := mem.Get(k)
			sv, sok := stored.Get(k)
			if mok != sok || !bytes.Equal(mv, sv) {
				t.Fatalf("round %d: Get differs", round)
			}
			p := stored.Prove(k)
			if !VerifyProof(stored.Root(), k, sv, sok, p) {
				t.Fatalf("round %d: proof over stubs does not check", round)
			}
		}
		if r.IntN(5) == 0 {
			// Flush: the store must then hold exactly the tree's branches.
			src, base = store(base, stored, src, pending)
			pending = nil
			stored = base
			want := allBranches(mem)
			if len(want) != len(src.branches) {
				t.Fatalf("round %d: %d stored branches, tree has %d", round, len(src.branches), len(want))
			}
			for p, b := range want {
				if !bytes.Equal(src.branches[p], b) {
					t.Fatalf("round %d: stored branch differs", round)
				}
			}
			if len(src.records) != mem.Len() {
				t.Fatalf("round %d: %d stored records, tree has %d", round, len(src.records), mem.Len())
			}
			olds = append(olds, struct {
				tree Tree
				mem  Tree
			}{base, mem})
		}
	}
	// Trees on older versions still read their own version.
	for _, o := range olds {
		var mk, sk []Key
		o.mem.Walk(func(k Key, v []byte) bool { mk = append(mk, k); return true })
		o.tree.Walk(func(k Key, v []byte) bool {
			if mv, _ := o.mem.Get(k); !bytes.Equal(mv, v) {
				t.Fatal("old version reads another version's value")
			}
			sk = append(sk, k)
			return true
		})
		if len(mk) != len(sk) {
			t.Fatal("old version walks differently")
		}
	}
}

// A stored branch that does not make up its parent's hash is refused.
func TestStoredBranchChecked(t *testing.T) {
	mem := Empty.Apply([]Change{{Key: Key{1}, Value: []byte{1}}, {Key: Key{2}, Value: []byte{2}}, {Key: Key{3}, Value: []byte{3}}})
	src, base := store(Empty, mem, &mapSource{branches: map[Position][]byte{}, records: map[Key][]byte{}}, []Key{{1}, {2}, {3}})
	for p := range src.branches {
		b := append([]byte(nil), src.branches[p]...)
		b[len(b)-1] ^= 1
		src.branches[p] = b
	}
	var err error
	func() {
		defer Recover(&err)
		base.Apply([]Change{{Key: Key{3}, Value: []byte{9}}})
	}()
	if err == nil {
		t.Fatal("corrupt stored branch accepted")
	}
}
