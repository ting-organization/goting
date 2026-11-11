package state

import (
	"bytes"
	"testing"
)

// A checked tree reads every stored record by its path and against its
// leaf's hash: a record table that differs from the tree is found out, where
// the plain read trusts it.
func TestCheckedReadsFindADamagedRecordTable(t *testing.T) {
	keys := []Key{{1}, {2}, {3}, {0x80, 1}}
	var changes []Change
	for i, k := range keys {
		changes = append(changes, Change{Key: k, Value: []byte{byte(i + 1), 7}})
	}
	mem := Empty.Apply(changes)
	src, base := store(Empty, mem, &mapSource{branches: map[Position][]byte{}, records: map[Key][]byte{}}, keys)
	checked := base.Checked()
	for i, k := range keys {
		v, ok := checked.Get(k)
		if !ok || !bytes.Equal(v, []byte{byte(i + 1), 7}) {
			t.Fatalf("checked read of %x: %x %v", k[:2], v, ok)
		}
	}
	if _, ok := checked.Get(Key{9}); ok {
		t.Fatal("an absent record was read")
	}
	// The table says another value for one record.
	src.records[keys[1]] = []byte{0xff}
	if v, ok := base.Get(keys[1]); !ok || v[0] != 0xff {
		t.Fatal("the plain read does not come from the record table")
	}
	var err error
	func() {
		defer Recover(&err)
		checked.Get(keys[1])
	}()
	if err == nil {
		t.Fatal("a record that differs from its leaf was read")
	}
	// The others still read.
	if v, ok := checked.Get(keys[0]); !ok || v[0] != 1 {
		t.Fatal("an undamaged record no longer reads")
	}
}
