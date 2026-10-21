package state

import (
	"math/rand/v2"
	"testing"
)

func TestProofs(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	key := func() Key {
		var k Key
		for i := range k {
			k[i] = byte(r.Uint32())
		}
		// Low entropy in the first bytes makes long shared prefixes.
		k[0], k[1] = byte(r.IntN(4)), 0
		return k
	}
	if p := Empty.Prove(key()); p.End != EndEmpty || len(p.Siblings) != 0 ||
		!VerifyProof(Empty.Root(), key(), nil, false, p) {
		t.Fatal("empty tree")
	}
	tr := Empty
	var keys []Key
	for round := 0; round < 40; round++ {
		var ch []Change
		for i := 0; i < 1+r.IntN(20); i++ {
			if len(keys) > 0 && r.IntN(4) == 0 {
				k := keys[r.IntN(len(keys))]
				ch = append(ch, Change{Key: k, Delete: true})
				continue
			}
			k := key()
			keys = append(keys, k)
			ch = append(ch, Change{Key: k, Value: []byte{byte(i), byte(round)}})
		}
		tr = tr.Apply(ch)
		root := tr.Root()
		for _, k := range append(keys[max(0, len(keys)-30):], key(), key()) {
			v, ok := tr.Get(k)
			p := tr.Prove(k)
			if !VerifyProof(root, k, v, ok, p) {
				t.Fatalf("round %d: proof of %x (present %v, end %v) does not check", round, k[:4], ok, p.End)
			}
			if ok && VerifyProof(root, k, append(v, 0), true, p) {
				t.Fatal("proof checks a wrong value")
			}
			if VerifyProof(root, k, v, !ok, p) {
				t.Fatal("proof checks the opposite presence")
			}
		}
	}
}
