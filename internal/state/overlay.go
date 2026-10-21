package state

import (
	"bytes"
	"sort"
)

type entry struct {
	value   []byte
	present bool
}

type undo struct {
	key  Key
	prev entry
	had  bool // whether the overlay held an entry for key before
}

// Overlay layers uncommitted writes over an immutable base Tree. It keeps an
// undo journal so execution can take checkpoints and roll back to them.
type Overlay struct {
	base    Tree
	writes  map[Key]entry
	journal []undo
}

// NewOverlay starts an empty overlay over base.
func NewOverlay(base Tree) *Overlay {
	return &Overlay{base: base, writes: make(map[Key]entry)}
}

// Base returns the tree the overlay reads through to.
func (o *Overlay) Base() Tree { return o.base }

// Get returns the current value of a record and whether it is present.
func (o *Overlay) Get(k Key) ([]byte, bool) {
	if e, ok := o.writes[k]; ok {
		return e.value, e.present
	}
	return o.base.Get(k)
}

func (o *Overlay) record(k Key) {
	prev, had := o.writes[k]
	o.journal = append(o.journal, undo{key: k, prev: prev, had: had})
}

// Set writes a record. The value is retained; the caller must not modify it.
func (o *Overlay) Set(k Key, value []byte) {
	o.record(k)
	if value == nil {
		value = []byte{}
	}
	o.writes[k] = entry{value: value, present: true}
}

// Delete makes a record absent.
func (o *Overlay) Delete(k Key) {
	o.record(k)
	o.writes[k] = entry{}
}

// Checkpoint returns a marker for Revert.
func (o *Overlay) Checkpoint() int { return len(o.journal) }

// Revert undoes every write made since the checkpoint.
func (o *Overlay) Revert(cp int) {
	for i := len(o.journal) - 1; i >= cp; i-- {
		u := o.journal[i]
		if u.had {
			o.writes[u.key] = u.prev
		} else {
			delete(o.writes, u.key)
		}
	}
	o.journal = o.journal[:cp]
}

// DiscardJournal forgets the undo history; later reverts cannot cross it.
func (o *Overlay) DiscardJournal() { o.journal = o.journal[:0] }

// Changes returns the overlay's net writes sorted by key.
func (o *Overlay) Changes() []Change {
	out := make([]Change, 0, len(o.writes))
	for k, e := range o.writes {
		out = append(out, Change{Key: k, Value: e.value, Delete: !e.present})
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].Key[:], out[j].Key[:]) < 0 })
	return out
}

// Commit applies the overlay's writes to its base and returns the new tree.
func (o *Overlay) Commit() Tree { return o.base.Apply(o.Changes()) }
