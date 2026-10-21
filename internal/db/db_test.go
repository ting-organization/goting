package db

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
)

func TestStoreAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	chainID := hashing.Hash{1}
	d, err := Open(path, chainID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.Base(); ok || err != nil {
		t.Fatal("fresh database has a base")
	}
	r := rand.New(rand.NewPCG(1, 1))
	var cs []state.Change
	var keys []state.Key
	for i := 0; i < 2000; i++ {
		var k state.Key
		for j := range k {
			k[j] = byte(r.Uint32())
		}
		keys = append(keys, k)
		cs = append(cs, state.Change{Key: k, Value: []byte{byte(i), byte(i >> 8)}})
	}
	mem := state.Empty.Apply(cs)
	commit := func(base, tree state.Tree, keys []state.Key, height uint64, undo []BlockUndo, rewind bool) {
		t.Helper()
		puts, dels := state.Delta(base, tree, keys)
		var recs []state.Change
		for _, k := range keys {
			v, ok := tree.Get(k)
			recs = append(recs, state.Change{Key: k, Value: v, Delete: !ok})
		}
		err := d.Commit(&Commit{Base: Base{Height: height, Block: hashing.Hash{byte(height)}, Count: uint64(tree.Len()),
			Root: tree.RootDesc()}, Records: recs, Puts: puts, Dels: dels, Undo: undo, Rewind: rewind})
		if err != nil {
			t.Fatal(err)
		}
	}
	commit(state.Empty, mem, keys, 1, []BlockUndo{{Height: 1, Block: hashing.Hash{1}, Prev: []state.Change{{Key: keys[0], Delete: true}}}}, false)
	d.Close()

	if _, err := Open(path, hashing.Hash{2}); !errors.Is(err, ErrWrongChain) {
		t.Fatalf("other chain: %v", err)
	}
	d, err = Open(path, chainID)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	base, ok, err := d.Base()
	if err != nil || !ok || base.Height != 1 || base.Count != 2000 {
		t.Fatalf("base %+v %v %v", base, ok, err)
	}
	stored := state.FromRoot(base.Root, int(base.Count), d.View())
	if stored.Root() != mem.Root() {
		t.Fatal("stored root differs")
	}
	for _, k := range keys[:200] {
		mv, _ := mem.Get(k)
		if sv, ok := stored.Get(k); !ok || !bytes.Equal(mv, sv) {
			t.Fatal("stored record differs")
		}
	}
	// Moving forward keeps views valid: a change applied over the stored
	// tree, then stored, and the old view reads on.
	ch := []state.Change{{Key: keys[5], Delete: true}, {Key: keys[6], Value: []byte("x")}, {Key: state.Key{9}, Value: []byte("y")}}
	next := stored.Apply(ch)
	commit(stored, next, []state.Key{keys[5], keys[6], {9}}, 2, nil, false)
	again := state.FromRoot(next.RootDesc(), next.Len(), d.View())
	if again.Root() != mem.Apply(ch).Root() {
		t.Fatal("second version differs")
	}
	if v, ok := next.Get(keys[7]); !ok || len(v) != 2 {
		t.Fatal("view after a forward commit")
	}
	// A rewind expires older views.
	commit(again, again, nil, 2, nil, true)
	var rerr error
	func() {
		defer state.Recover(&rerr)
		next.Get(keys[7])
	}()
	if !errors.Is(rerr, ErrExpired) {
		t.Fatalf("read across a rewind: %v", rerr)
	}
	prev, ok, err := d.Undo(1, hashing.Hash{1})
	if err != nil || !ok || len(prev) != 1 || !prev[0].Delete || prev[0].Key != keys[0] {
		t.Fatalf("undo %v %v %v", prev, ok, err)
	}
	// Raising the undo floor drops older undo data.
	if err := d.Commit(&Commit{Base: base, UndoFloor: 2}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.Undo(1, hashing.Hash{1}); ok {
		t.Fatal("undo below the floor kept")
	}
}

func TestUndoEncoding(t *testing.T) {
	prev := []state.Change{{Key: state.Key{1}, Value: []byte{}}, {Key: state.Key{2}, Delete: true}, {Key: state.Key{3}, Value: bytes.Repeat([]byte{7}, 1000)}}
	got, err := decodeUndo(encodeUndo(prev))
	if err != nil || len(got) != 3 || got[0].Delete || len(got[0].Value) != 0 || !got[1].Delete || len(got[2].Value) != 1000 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := decodeUndo(append(encodeUndo(prev), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
}
