package state

import (
	"slices"

	"github.com/ting-organization/goting/internal/hashing"
)

// ProofEnd says where a key's path through the tree ends.
type ProofEnd int

const (
	// EndPresent: at the record's own leaf.
	EndPresent ProofEnd = iota
	// EndEmpty: at an empty subtree.
	EndEmpty
	// EndOther: at the leaf of another record whose key shares the path.
	EndOther
)

func (e ProofEnd) String() string {
	switch e {
	case EndPresent:
		return "present"
	case EndEmpty:
		return "empty"
	}
	return "other"
}

// Proof is a record's Merkle proof against a state root, in the reference
// node's format (wiki: data/state-tree#proofs): where the key's path ends,
// and the other child's hash at each level of the path, from its end up to
// the root. The protocol defines the hashes, not a proof format.
type Proof struct {
	End ProofEnd
	// OtherKey and OtherValue are the record the path ends at, for EndOther.
	OtherKey   Key
	OtherValue []byte
	Siblings   []hashing.Hash
}

// Prove returns the proof of the record at k, present or absent.
func (t Tree) Prove(k Key) Proof {
	var p Proof
	n, d := t.root, 0
walk:
	for {
		switch {
		case n == nil:
			p.End = EndEmpty
			break walk
		case n.leaf:
			if n.key == k {
				p.End = EndPresent
			} else {
				p.End, p.OtherKey, p.OtherValue = EndOther, n.key, n.value
				if n.stub {
					v, ok := t.src.Record(n.key)
					if !ok {
						panic(&StorageError{Err: errLeafRecord})
					}
					p.OtherValue = v
				}
			}
			break walk
		}
		// A branch splitting at s, seen from depth d: single-sided levels
		// down to s, where its records divide.
		s := int(n.split)
		for lvl := d; lvl < s; lvl++ {
			if bit(&k, lvl) != bit(&n.key, lvl) {
				// The key's side is empty from here; the other side holds
				// every record of the branch.
				p.Siblings = append(p.Siblings, hashAt(n, lvl+1))
				p.End = EndEmpty
				break walk
			}
			p.Siblings = append(p.Siblings, emptyHash)
		}
		if n.stub {
			n = expand(t.src, n, 0)
		}
		if bit(&k, s) == 0 {
			p.Siblings = append(p.Siblings, hashAt(n.right, s+1))
			n = n.left
		} else {
			p.Siblings = append(p.Siblings, hashAt(n.left, s+1))
			n = n.right
		}
		d = s + 1
	}
	slices.Reverse(p.Siblings)
	return p
}

// VerifyProof reports whether p shows the record at k to hold value, or
// with a nil value to be absent, in the state whose root is root.
func VerifyProof(root hashing.Hash, k Key, value []byte, present bool, p Proof) bool {
	depth := len(p.Siblings)
	if depth > 256 {
		return false
	}
	var h hashing.Hash
	switch {
	case p.End == EndPresent && present:
		h = leafHash(&k, value)
	case p.End == EndEmpty && !present:
		h = emptyHash
	case p.End == EndOther && !present && p.OtherKey != k && firstDiff(&p.OtherKey, &k) >= depth:
		h = leafHash(&p.OtherKey, p.OtherValue)
	default:
		return false
	}
	for i, s := range p.Siblings {
		lvl := depth - 1 - i
		if bit(&k, lvl) == 0 {
			h = nodeHash(lvl, &h, &s)
		} else {
			h = nodeHash(lvl, &s, &h)
		}
	}
	return h == root
}
