// Package state implements Ting's record map and its commitment, the binary
// compact sparse Merkle tree of rules §6.
//
// A Tree is an immutable, persistent value: applying a batch of changes
// returns a new Tree that shares every untouched subtree with the old one.
// Keeping the Tree of each recent block therefore costs only the changed
// paths, and restoring an ancestor's state for a reorganization is O(1).
//
// Internally a subtree that holds two or more records is stored once, at the
// bit where its records first split; the single-sided nodes the hash
// definition places between a parent and that split are computed while
// hashing rather than stored.
//
// A tree can also rest on a stored tree (a Source, a database snapshot): its
// unchanged subtrees are then stubs that carry only their hash and position
// and load from the source when a change or a proof needs them, and records
// below a stub are read from the source's flat record table.
package state

import (
	"bytes"
	"math/bits"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/ting-organization/goting/internal/hashing"
)

// Key is a 32-byte record key.
type Key = hashing.Hash

type node struct {
	hash  hashing.Hash // leaf hash, or the branch's own hash at its split; valid when !dirty
	key   Key          // leaf key, or any key below a branch (its shared prefix)
	left  *node
	right *node
	value []byte
	gen   uint32 // batch that created the node; that batch may mutate it in place
	split uint16 // branch: the bit its records first differ in
	leaf  bool
	dirty bool
	// stub marks a subtree that lies in the tree's source: its hash, key and
	// split are known, its children (or, for a leaf, its value) are not.
	stub bool
}

// Tree is an immutable record map with a cached commitment.
type Tree struct {
	root  *node
	count int
	src   Source // where stubs load from; nil for a tree wholly in memory
}

// Empty is the tree with no records.
var Empty = Tree{}

// Root returns the state root, hash(all records, 0).
func (t Tree) Root() hashing.Hash { return hashAt(t.root, 0) }

// Len returns the number of records.
func (t Tree) Len() int { return t.count }

// bit returns bit i of k, counting from the most significant bit of byte 0.
func bit(k *Key, i int) uint {
	return uint(k[i>>3]>>(7-uint(i&7))) & 1
}

// firstDiff returns the first bit at which a and b differ, or 256.
func firstDiff(a, b *Key) int {
	for i := 0; i < 32; i++ {
		if x := a[i] ^ b[i]; x != 0 {
			return i*8 + bits.LeadingZeros8(x)
		}
	}
	return 256
}

// Get returns the record's value and whether it is present. Below a stub,
// the record comes from the source's record table.
func (t Tree) Get(k Key) ([]byte, bool) {
	n := t.root
	for n != nil {
		if n.leaf {
			if n.key != k {
				return nil, false
			}
			if n.stub {
				return t.src.Record(k)
			}
			return n.value, true
		}
		if n.stub {
			if firstDiff(&n.key, &k) < int(n.split) {
				return nil, false
			}
			return t.src.Record(k)
		}
		if bit(&k, int(n.split)) == 0 {
			n = n.left
		} else {
			n = n.right
		}
	}
	return nil, false
}

var emptyHash = hashing.EmptyStateRoot

func leafHash(k *Key, value []byte) hashing.Hash {
	x := hashing.New(hashing.TagStateLeaf)
	x.Write(k[:])
	x.WriteBytes(value)
	return x.Sum()
}

func nodeHash(depth int, left, right *hashing.Hash) hashing.Hash {
	x := hashing.New(hashing.TagStateNode)
	x.WriteU16(uint16(depth))
	x.Write(left[:])
	x.Write(right[:])
	return x.Sum()
}

// hashAt returns hash(R, d) for the records under n, seen from depth d. A
// branch splitting at s > d is wrapped in s−d single-sided nodes.
func hashAt(n *node, d int) hashing.Hash {
	if n == nil {
		return emptyHash
	}
	if n.leaf {
		return n.hash
	}
	h := n.hash
	for lvl := int(n.split) - 1; lvl >= d; lvl-- {
		if bit(&n.key, lvl) == 0 {
			h = nodeHash(lvl, &h, &emptyHash)
		} else {
			h = nodeHash(lvl, &emptyHash, &h)
		}
	}
	return h
}

// Change is one record write (Delete false) or deletion.
type Change struct {
	Key    Key
	Value  []byte
	Delete bool
}

var genCounter atomic.Uint32

// Apply returns a new tree with the changes applied, in order, and every hash
// computed. The receiver is unchanged. Values are retained, not copied: the
// caller must not modify them afterwards.
func (t Tree) Apply(changes []Change) Tree {
	if len(changes) == 0 {
		return t
	}
	gen := genCounter.Add(1)
	if gen == 0 { // skip the zero generation on wraparound
		gen = genCounter.Add(1)
	}
	b := batch{gen: gen, count: t.count, src: t.src}
	root := t.root
	for i := range changes {
		c := &changes[i]
		if c.Delete {
			root = b.remove(root, &c.Key)
		} else {
			root = b.insert(root, &c.Key, c.Value)
		}
	}
	rehash(root, len(changes) >= parallelRehashThreshold, 0)
	return Tree{root: root, count: b.count, src: t.src}
}

type batch struct {
	gen   uint32
	count int
	src   Source
}

// expand loads a stub branch's children from the source, as a node of
// generation gen (0 for a read-only view).
func expand(src Source, n *node, gen uint32) *node {
	l, r := src.Branch(positionOf(n))
	left, right := stubOf(l), stubOf(r)
	// The stored children must make up the stored hash.
	lh, rh := hashAt(left, int(n.split)+1), hashAt(right, int(n.split)+1)
	if nodeHash(int(n.split), &lh, &rh) != n.hash {
		panic(&StorageError{Err: errBranchHash})
	}
	return &node{hash: n.hash, key: n.key, split: n.split, left: left, right: right, gen: gen}
}

func (b *batch) newLeaf(k *Key, value []byte) *node {
	return &node{key: *k, value: value, leaf: true, dirty: true, gen: b.gen}
}

func (b *batch) newBranch(split int, a, c *node) *node {
	// a and c differ first at split; order them by that bit.
	n := &node{split: uint16(split), dirty: true, gen: b.gen}
	if bit(&a.key, split) == 0 {
		n.left, n.right = a, c
	} else {
		n.left, n.right = c, a
	}
	n.key = a.key
	return n
}

// own returns n itself when this batch created it, or a mutable copy.
func (b *batch) own(n *node) *node {
	if n.gen == b.gen {
		n.dirty = true
		return n
	}
	c := *n
	c.gen = b.gen
	c.dirty = true
	return &c
}

func (b *batch) insert(n *node, k *Key, value []byte) *node {
	if n == nil {
		b.count++
		return b.newLeaf(k, value)
	}
	if n.leaf {
		if n.key == *k {
			if n.stub {
				return b.newLeaf(k, value) // replaces the stored leaf
			}
			if bytes.Equal(n.value, value) {
				return n
			}
			m := b.own(n)
			m.value = value
			return m
		}
		b.count++
		return b.newBranch(firstDiff(&n.key, k), n, b.newLeaf(k, value))
	}
	if d := firstDiff(&n.key, k); d < int(n.split) {
		b.count++
		return b.newBranch(d, n, b.newLeaf(k, value))
	}
	e := n
	if n.stub {
		e = expand(b.src, n, b.gen)
	}
	right := bit(k, int(e.split)) == 1
	child := e.left
	if right {
		child = e.right
	}
	nc := b.insert(child, k, value)
	if nc == child {
		// Unchanged, or mutated in place below a node this batch owns.
		return n
	}
	m := b.own(e)
	if right {
		m.right = nc
	} else {
		m.left = nc
	}
	return m
}

func (b *batch) remove(n *node, k *Key) *node {
	if n == nil {
		return nil
	}
	if n.leaf {
		if n.key == *k {
			b.count--
			return nil
		}
		return n
	}
	if firstDiff(&n.key, k) < int(n.split) {
		return n // not below this branch
	}
	e := n
	if n.stub {
		e = expand(b.src, n, b.gen)
	}
	var child, other *node
	right := bit(k, int(e.split)) == 1
	if right {
		child, other = e.right, e.left
	} else {
		child, other = e.left, e.right
	}
	nc := b.remove(child, k)
	if nc == child {
		return n
	}
	if nc == nil {
		// The other side moves up; as a leaf it keeps moving up for as long
		// as it is alone, which happens naturally as callers collapse too.
		return other
	}
	m := b.own(e)
	if right {
		m.right = nc
	} else {
		m.left = nc
	}
	return m
}

// parallelRehashThreshold is the batch size above which the top levels of the
// tree are rehashed on several goroutines.
const parallelRehashThreshold = 2048

// parallelDepth bounds how many levels fork goroutines (2^depth tasks).
const parallelDepth = 4

func rehash(n *node, parallel bool, level int) {
	if n == nil || !n.dirty || n.stub {
		return
	}
	if n.leaf {
		n.hash = leafHash(&n.key, n.value)
		n.dirty = false
		return
	}
	if parallel && level < parallelDepth && n.left != nil && n.left.dirty && n.right != nil && n.right.dirty {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			rehash(n.left, true, level+1)
		}()
		rehash(n.right, true, level+1)
		wg.Wait()
	} else {
		rehash(n.left, parallel, level+1)
		rehash(n.right, parallel, level+1)
	}
	s := int(n.split)
	l := hashAt(n.left, s+1)
	r := hashAt(n.right, s+1)
	n.hash = nodeHash(s, &l, &r)
	n.dirty = false
}

// Walk calls fn for every record in key order until fn returns false. It
// loads every stub on the way.
func (t Tree) Walk(fn func(k Key, v []byte) bool) {
	t.walk(t.root, fn)
}

func (t Tree) walk(n *node, fn func(k Key, v []byte) bool) bool {
	if n == nil {
		return true
	}
	if n.leaf {
		if n.stub {
			v, ok := t.src.Record(n.key)
			if !ok {
				panic(&StorageError{Err: errLeafRecord})
			}
			return fn(n.key, v)
		}
		return fn(n.key, n.value)
	}
	if n.stub {
		n = expand(t.src, n, 0)
	}
	return t.walk(n.left, fn) && t.walk(n.right, fn)
}

// Build constructs a tree from records. It sorts recs by key; keys must be
// distinct.
func Build(recs []Change) Tree {
	sort.Slice(recs, func(i, j int) bool { return bytes.Compare(recs[i].Key[:], recs[j].Key[:]) < 0 })
	gen := genCounter.Add(1)
	b := batch{gen: gen}
	root := b.build(recs, 0)
	b.count = len(recs)
	rehash(root, len(recs) >= parallelRehashThreshold, 0)
	return Tree{root: root, count: len(recs)}
}

// build creates the subtree of sorted records whose keys agree below depth.
func (b *batch) build(recs []Change, depth int) *node {
	switch len(recs) {
	case 0:
		return nil
	case 1:
		return b.newLeaf(&recs[0].Key, recs[0].Value)
	}
	first, last := &recs[0].Key, &recs[len(recs)-1].Key
	s := firstDiff(first, last) // sorted: the first and last differ first where any do
	// Records with bit s == 0 come first.
	i := sort.Search(len(recs), func(i int) bool { return bit(&recs[i].Key, s) == 1 })
	n := &node{split: uint16(s), key: *first, dirty: true, gen: b.gen}
	n.left = b.build(recs[:i], s+1)
	n.right = b.build(recs[i:], s+1)
	return n
}
