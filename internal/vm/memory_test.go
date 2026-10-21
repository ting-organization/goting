package vm

import (
	"testing"

	"github.com/ting-organization/goting/internal/types"
	. "github.com/ting-organization/goting/internal/wasm/wasmtest"
)

// Pooled memory must look fresh to every invocation: a program that traps
// when it finds a byte it did not write, then dirties its memory, including
// a page it grows, runs again and again, in calls nested in itself too.
func TestMemoryIsFreshEveryInvocation(t *testing.T) {
	m := &Module{Memory: &Limits{Min: 2, Max: 4}}
	m.Invoke(nil,
		// Address 0, address 70,000 (second page) and the first word of a
		// third page grown now must all read zero.
		I32Const(0), Mem(0x29, 3, 0), Op(0x50), Op(0x45), If(0x40), Op(0x00), Op(End),
		I32Const(70_000), Mem(0x29, 3, 0), Op(0x50), Op(0x45), If(0x40), Op(0x00), Op(End),
		I32Const(1), MemoryGrow(), Op(Drop),
		I32Const(131_072), Mem(0x29, 3, 0), Op(0x50), Op(0x45), If(0x40), Op(0x00), Op(End),
		// Dirty all three places.
		I32Const(0), I64Const(-1), Mem(0x37, 3, 0),
		I32Const(70_000), I64Const(-1), Mem(0x37, 3, 0),
		I32Const(131_072), I64Const(-1), Mem(0x37, 3, 0),
		Op(End))
	h := newHarness(t)
	id, r := h.deploy(m.Bytes(), nil)
	if r.Status != types.StatusSuccess {
		t.Fatalf("deploy status %d", r.Status)
	}
	for i := 0; i < 50; i++ {
		if r := h.call(id, nil); r.Status != types.StatusSuccess {
			t.Fatalf("call %d: status %d: memory was not fresh", i, r.Status)
		}
	}
	// A trap leaves memory behind too; the next invocation still sees zero.
	// (Its initializer, run without input, does not trap.)
	trapper := &Module{Memory: &Limits{Min: 2, Max: 2}}
	input := trapper.Host("input")
	trapper.Invoke(nil, I32Const(0), I64Const(-1), Mem(0x37, 3, 0),
		I32Const(0), I32Const(0), Call(input), If(0x40), Op(0x00), Op(End), Op(End))
	tid, r := h.deploy(trapper.Bytes(), nil)
	if r.Status != types.StatusSuccess {
		t.Fatalf("trapper deploy status %d", r.Status)
	}
	for i := 0; i < 10; i++ {
		if r := h.call(tid, []byte{1}); r.Status != types.StatusTrap {
			t.Fatalf("trapper status %d", r.Status)
		}
		if r := h.call(id, nil); r.Status != types.StatusSuccess {
			t.Fatalf("after a trap: status %d", r.Status)
		}
	}
}

func TestMemoryPoolClasses(t *testing.T) {
	for _, n := range []uint64{1, 65536, 65537, 1 << 20, 3 << 20} {
		b := getMemory(n)
		if uint64(len(b)) != n || cap(b)&(cap(b)-1) != 0 {
			t.Fatalf("getMemory(%d): len %d cap %d", n, len(b), cap(b))
		}
		for i := range b {
			b[i] = 0xaa
		}
		putMemory(b)
		c := getMemory(n)
		for i, x := range c[:cap(c)] {
			if x != 0 {
				t.Fatalf("reused buffer dirty at %d", i)
			}
		}
		putMemory(c)
	}
	// Growing within capacity exposes only zero bytes.
	b := getMemory(65536)
	b[65535] = 1
	b = growMemory(b, 1<<20)
	if len(b) != 1<<20 || b[65535] != 1 || b[65536] != 0 {
		t.Fatal("grow")
	}
	putMemory(b)
	// Buffers too large for the pool are allocated plainly.
	if big := getMemory(100 << 20); len(big) != 100<<20 {
		t.Fatal("large buffer")
	}
}

// Memories beyond the pooled sizes are mapped and unmapped per invocation;
// they too must look fresh, grown pages included.
func TestLargeMemoryIsFresh(t *testing.T) {
	const far = 70_000_000 // beyond 64 MiB
	m := &Module{Memory: &Limits{Min: 1100, Max: 1200}}
	m.Invoke(nil,
		I32Const(0), Mem(0x29, 3, 0), Op(0x50), Op(0x45), If(0x40), Op(0x00), Op(End),
		I32Const(far), Mem(0x29, 3, 0), Op(0x50), Op(0x45), If(0x40), Op(0x00), Op(End),
		I32Const(1), MemoryGrow(), Op(Drop),
		I32Const(1100*65536), Mem(0x29, 3, 0), Op(0x50), Op(0x45), If(0x40), Op(0x00), Op(End),
		I32Const(0), I64Const(-1), Mem(0x37, 3, 0),
		I32Const(far), I64Const(-1), Mem(0x37, 3, 0),
		I32Const(1100*65536), I64Const(-1), Mem(0x37, 3, 0),
		Op(End))
	h := newHarness(t)
	id, r := h.deploy(m.Bytes(), nil)
	if r.Status != types.StatusSuccess {
		t.Fatalf("deploy status %d", r.Status)
	}
	for i := 0; i < 5; i++ {
		if r := h.call(id, nil); r.Status != types.StatusSuccess {
			t.Fatalf("call %d: status %d: memory was not fresh", i, r.Status)
		}
	}
	b := getMemory(100 << 20)
	b[len(b)-1] = 1
	putMemory(b)
	if c := getMemory(100 << 20); c[len(c)-1] != 0 {
		t.Fatal("large buffer reused dirty")
	} else {
		putMemory(c)
	}
}
