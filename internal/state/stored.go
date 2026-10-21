package state

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ting-organization/goting/internal/hashing"
)

// Source is a stored tree: its branch nodes by position and its records by
// key, as one consistent version (a database snapshot). Its methods panic
// with a *StorageError when the storage fails; Recover turns that into an
// error where a caller can handle it.
type Source interface {
	// Branch returns the children of the stored branch node at pos.
	Branch(pos Position) (left, right Desc)
	// Record returns a stored record's value.
	Record(k Key) ([]byte, bool)
}

// StorageError is a failure to read a stored tree. It is never a property
// of a block: a node that meets one stops rather than judge anything.
type StorageError struct{ Err error }

func (e *StorageError) Error() string { return "storage: " + e.Err.Error() }
func (e *StorageError) Unwrap() error { return e.Err }

var (
	errBranchHash = errors.New("stored branch does not match its hash")
	errLeafRecord = errors.New("stored leaf without its record")
)

// Recover, deferred, turns a storage failure raised below into *err.
func Recover(err *error) {
	if r := recover(); r != nil {
		se, ok := r.(*StorageError)
		if !ok {
			panic(r)
		}
		*err = se
	}
}

// Position names a branch node: the bit its records first split at, and the
// key bits above it.
type Position struct {
	Split  uint16
	Prefix Key // bits at and after Split are zero
}

func positionOf(n *node) Position {
	return PositionOf(n.split, &n.key)
}

// PositionOf is the position of a branch splitting at split with key below it.
func PositionOf(split uint16, k *Key) Position {
	p := Position{Split: split}
	full := int(split) / 8
	copy(p.Prefix[:full], k[:full])
	if r := split % 8; r != 0 {
		p.Prefix[full] = k[full] & byte(0xff<<(8-r))
	}
	return p
}

// Bytes encodes a position: the split as u16 big-endian, then the prefix's
// bytes that hold its bits.
func (p Position) Bytes() []byte {
	n := (int(p.Split) + 7) / 8
	b := make([]byte, 2+n)
	binary.BigEndian.PutUint16(b, p.Split)
	copy(b[2:], p.Prefix[:n])
	return b
}

// Desc describes a subtree for its parent: none, a leaf (its key and leaf
// hash) or a branch (its split, a key below it, and its hash at the split).
type Desc struct {
	Kind  uint8
	Split uint16
	Key   Key
	Hash  hashing.Hash
}

// Subtree kinds.
const (
	DescEmpty  = 0
	DescLeaf   = 1
	DescBranch = 2
)

// DescSize is a Desc's encoded size.
const DescSize = 1 + 2 + 32 + 32

// Append appends the descriptor's encoding.
func (d Desc) Append(b []byte) []byte {
	b = append(b, d.Kind)
	b = binary.BigEndian.AppendUint16(b, d.Split)
	b = append(b, d.Key[:]...)
	return append(b, d.Hash[:]...)
}

// DecodeDesc decodes one descriptor.
func DecodeDesc(b []byte) (Desc, error) {
	if len(b) != DescSize || b[0] > DescBranch {
		return Desc{}, fmt.Errorf("bad subtree descriptor")
	}
	d := Desc{Kind: b[0], Split: binary.BigEndian.Uint16(b[1:3])}
	copy(d.Key[:], b[3:35])
	copy(d.Hash[:], b[35:67])
	return d, nil
}

// EncodeBranch is a stored branch node: its children's descriptors.
func EncodeBranch(left, right Desc) []byte {
	return right.Append(left.Append(make([]byte, 0, 2*DescSize)))
}

// DecodeBranch decodes a stored branch node.
func DecodeBranch(b []byte) (left, right Desc, err error) {
	if len(b) != 2*DescSize {
		return Desc{}, Desc{}, fmt.Errorf("bad stored branch of %d bytes", len(b))
	}
	if left, err = DecodeDesc(b[:DescSize]); err != nil {
		return
	}
	right, err = DecodeDesc(b[DescSize:])
	return
}

func descOf(n *node) Desc {
	switch {
	case n == nil:
		return Desc{}
	case n.leaf:
		return Desc{Kind: DescLeaf, Key: n.key, Hash: n.hash}
	}
	return Desc{Kind: DescBranch, Split: n.split, Key: n.key, Hash: n.hash}
}

func stubOf(d Desc) *node {
	switch d.Kind {
	case DescLeaf:
		return &node{leaf: true, stub: true, key: d.Key, hash: d.Hash}
	case DescBranch:
		return &node{stub: true, split: d.Split, key: d.Key, hash: d.Hash}
	}
	return nil
}

// FromRoot is the stored tree whose root is root and which holds count
// records, read from src.
func FromRoot(root Desc, count int, src Source) Tree {
	return Tree{root: stubOf(root), count: count, src: src}
}

// RootDesc describes the tree's root.
func (t Tree) RootDesc() Desc { return descOf(t.root) }

// Stored reports whether the tree rests on a source.
func (t Tree) Stored() bool { return t.src != nil }

// BranchWrite is a branch node to store at a position.
type BranchWrite struct {
	Pos    Position
	Record []byte
}

// Delta is what replacing the stored tree old with t, a tree derived from
// old, writes to the branch nodes: every branch t holds in memory, all of
// which are new since old, and the positions of old's branches on the paths
// of keys — every key whose record may differ — that t no longer has.
func Delta(old, t Tree, keys []Key) (puts []BranchWrite, dels []Position) {
	var collect func(n *node)
	collect = func(n *node) {
		if n == nil || n.leaf || n.stub {
			return
		}
		puts = append(puts, BranchWrite{Pos: positionOf(n), Record: EncodeBranch(descOf(n.left), descOf(n.right))})
		collect(n.left)
		collect(n.right)
	}
	collect(t.root)
	seen := map[Position]bool{}
	for i := range keys {
		k := &keys[i]
		for n := old.root; n != nil && !n.leaf; {
			if firstDiff(&n.key, k) < int(n.split) {
				break
			}
			if n.stub {
				n = expand(old.src, n, 0)
			}
			p := positionOf(n)
			if !seen[p] {
				seen[p] = true
				if !t.hasBranch(p) {
					dels = append(dels, p)
				}
			}
			if bit(k, int(n.split)) == 0 {
				n = n.left
			} else {
				n = n.right
			}
		}
	}
	return puts, dels
}

// hasBranch reports whether t has a branch at p, which the tree t derives
// from has. Below a stub t is unchanged, so it has every branch there.
func (t Tree) hasBranch(p Position) bool {
	for n := t.root; n != nil && !n.leaf; {
		if int(n.split) > int(p.Split) || firstDiff(&n.key, &p.Prefix) < int(n.split) {
			return false
		}
		if n.split == p.Split {
			return true
		}
		if n.stub {
			return true
		}
		if bit(&p.Prefix, int(n.split)) == 0 {
			n = n.left
		} else {
			n = n.right
		}
	}
	return false
}
