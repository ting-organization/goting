package wasm_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/wasm"
	. "github.com/ting-organization/goting/internal/wasm/wasmtest"
)

func mustDecode(t *testing.T, b []byte) *wasm.Module {
	t.Helper()
	m, err := wasm.Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func mustReject(t *testing.T, b []byte, why string) {
	t.Helper()
	_, err := wasm.Decode(b)
	if err == nil {
		t.Fatalf("%s: accepted", why)
	}
	if !errors.Is(err, wasm.ErrInvalid) {
		t.Fatalf("%s: error does not wrap ErrInvalid: %v", why, err)
	}
}

func minimal() *Module {
	m := &Module{}
	m.Invoke(nil, Op(End))
	return m
}

func TestMinimalModule(t *testing.T) {
	m := mustDecode(t, minimal().Bytes())
	if len(m.Funcs) != 1 || m.Funcs[0].Reservation != 0 {
		t.Fatalf("reservation %d", m.Funcs[0].Reservation)
	}
}

func TestCodePriceChecks(t *testing.T) {
	if p := wasm.CodePrice(nil); p != 14400 {
		t.Fatalf("empty price %d", p)
	}
	if p := wasm.CodePrice([]byte{0, 0x61, 0x73, 0x6d, 1, 0, 0, 0}); p != 14880 {
		t.Fatalf("preamble price %d", p)
	}
	// Preamble + function section declaring 2 functions: id(1) size(1) content(3: count=2, 0, 0).
	code := []byte{0, 0x61, 0x73, 0x6d, 1, 0, 0, 0, 3, 3, 2, 0, 0}
	want := uint64(14400 + 60*8 + 60*2 + 60*3 + 1215*2)
	if p := wasm.CodePrice(code); p != want {
		t.Fatalf("function section price %d want %d", p, want)
	}
	// Overrunning section: id, size 100, and 1 byte: all remaining at 60.
	code = []byte{0, 0x61, 0x73, 0x6d, 1, 0, 0, 0, 11, 100, 1}
	want = uint64(14400 + 60*8 + 60*3)
	if p := wasm.CodePrice(code); p != want {
		t.Fatalf("overrun price %d want %d", p, want)
	}
	// Non-minimal size is fine for pricing: data section, size 0x81 0x00 = 1, count byte 5.
	code = []byte{0, 0x61, 0x73, 0x6d, 1, 0, 0, 0, 11, 0x81, 0x00, 5}
	want = uint64(14400 + 60*8 + 60*3 + 6*1 + 446*5)
	if p := wasm.CodePrice(code); p != want {
		t.Fatalf("data price %d want %d", p, want)
	}
	// Short preamble.
	if p := wasm.CodePrice([]byte{0, 1, 2}); p != 14400+180 {
		t.Fatalf("short price %d", p)
	}
}

func TestRejectsShape(t *testing.T) {
	// Missing invoke.
	m := &Module{}
	m.AddFunc(nil, nil, nil, Op(End))
	mustReject(t, m.Bytes(), "no invoke")

	// Memory without export.
	m = &Module{Memory: &Limits{1, 1}}
	f := m.AddFunc(nil, nil, nil, Op(End))
	m.Exports = append(m.Exports, Export{Name: "invoke", Index: f})
	mustReject(t, m.Bytes(), "memory not exported")

	// Memory max above 65536.
	m = &Module{Memory: &Limits{1, 65537}}
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "memory max")

	// Memory min above max.
	m = &Module{Memory: &Limits{2, 1}}
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "memory min > max")

	// Table larger than function count.
	m = &Module{Table: &Limits{2, 2}}
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "table too large")

	// Table min != max.
	m = &Module{Table: &Limits{0, 1}}
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "table min != max")

	// Invoke with a result.
	m = &Module{}
	f = m.AddFunc(nil, []byte{I32}, nil, I32Const(1), Op(End))
	m.Exports = append(m.Exports, Export{Name: "invoke", Index: f})
	mustReject(t, m.Bytes(), "invoke with result")

	// Unknown export.
	m = minimal()
	m.Exports = append(m.Exports, Export{Name: "other", Index: 0})
	mustReject(t, m.Bytes(), "extra export")

	// Unknown import.
	m = &Module{}
	m.Types = append(m.Types, Type{})
	m.Imports = append(m.Imports, Import{Name: "clock", Type: 0})
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "unknown import")

	// Import with the wrong signature.
	m = &Module{}
	m.Types = append(m.Types, Type{Params: []byte{I32, I32}})
	m.Imports = append(m.Imports, Import{Name: "input", Type: 0})
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "wrong import signature")

	// Repeated import.
	m = &Module{}
	m.Host("return")
	m.Imports = append(m.Imports, m.Imports[0])
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "repeated import")

	// Data segment outside initial memory.
	m = &Module{Memory: &Limits{1, 2}}
	m.Invoke(nil, Op(End))
	m.Data = []Data{{Offset: 65535, Bytes: []byte{1, 2}}}
	mustReject(t, m.Bytes(), "data out of range")

	// Data at the very end with zero length is fine.
	m = &Module{Memory: &Limits{1, 2}}
	m.Invoke(nil, Op(End))
	m.Data = []Data{{Offset: 65536, Bytes: nil}}
	mod := mustDecode(t, m.Bytes())
	if mod.DataSpans != 0 || mod.DataBytes != 0 {
		t.Fatal("empty data counts")
	}
	// Negative-looking offsets are large unsigned offsets.
	m.Data = []Data{{Offset: 0xffffffff, Bytes: nil}}
	mustReject(t, m.Bytes(), "data offset beyond memory")

	// Custom section rejected.
	b := append(minimal().Bytes(), 0, 1, 0)
	mustReject(t, b, "custom section")

	// Trailing garbage.
	b = append(minimal().Bytes(), 0xff)
	mustReject(t, b, "trailing")

	// Duplicate exports.
	m = minimal()
	m.Exports = append(m.Exports, m.Exports[0])
	mustReject(t, m.Bytes(), "duplicate invoke")
}

func TestRejectsNonMinimalLEB(t *testing.T) {
	// Body with i32.const 64 encoded minimally: 41 c0 00.
	m := &Module{}
	m.Invoke(nil, []byte{0x41, 0xc0, 0x00, 0x1a, 0x0b})
	mustDecode(t, m.Bytes())
	// Non-minimal: 41 c0 80 00 (extra group) is rejected.
	m = &Module{}
	m.Invoke(nil, []byte{0x41, 0xc0, 0x80, 0x00, 0x1a, 0x0b})
	mustReject(t, m.Bytes(), "non-minimal const")
	// i32.const -64 is 41 40.
	m = &Module{}
	m.Invoke(nil, []byte{0x41, 0x40, 0x1a, 0x0b})
	mustDecode(t, m.Bytes())
	// -64 as 41 c0 7f is non-minimal.
	m = &Module{}
	m.Invoke(nil, []byte{0x41, 0xc0, 0x7f, 0x1a, 0x0b})
	mustReject(t, m.Bytes(), "non-minimal negative")
	// Local index with padding: 20 80 00.
	m = &Module{}
	m.Invoke([]LocalGroup{{1, I32}}, []byte{0x20, 0x80, 0x00, 0x1a, 0x0b})
	mustReject(t, m.Bytes(), "non-minimal index")
	// i32 out of range in 5 bytes: ff ff ff ff 0f is 2^32-1.
	m = &Module{}
	m.Invoke(nil, []byte{0x41, 0xff, 0xff, 0xff, 0xff, 0x0f, 0x1a, 0x0b})
	mustReject(t, m.Bytes(), "i32 overflow")
	// INT32_MIN: 80 80 80 80 78.
	m = &Module{}
	m.Invoke(nil, []byte{0x41, 0x80, 0x80, 0x80, 0x80, 0x78, 0x1a, 0x0b})
	mustDecode(t, m.Bytes())
	// INT64_MIN: 80 x9 7f.
	m = &Module{}
	m.Invoke(nil, []byte{0x42, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x7f, 0x1a, 0x0b})
	mustDecode(t, m.Bytes())
	// i64 with bad high bits in the 10th byte.
	m = &Module{}
	m.Invoke(nil, []byte{0x42, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02, 0x1a, 0x0b})
	mustReject(t, m.Bytes(), "i64 high bits")
}

func TestTyping(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		ok   bool
		maxH uint32
	}{
		{"add", Code(I32Const(1), I32Const(2), Op(0x6a), Op(Drop), Op(End)), true, 2},
		{"mismatch", Code(I32Const(1), I64Const(2), Op(0x6a), Op(Drop), Op(End)), false, 0},
		{"leftover", Code(I32Const(1), Op(End)), false, 0},
		{"underflow", Code(Op(Drop), Op(End)), false, 0},
		{"unreachable then anything", Code(Op(Unreachable), Op(0x6a), Op(Drop), Op(End)), true, 1},
		{"unreachable keeps known types", Code(Op(Unreachable), I64Const(1), Op(0x6a), Op(Drop), Op(End)), false, 0},
		{"select unknowns", Code(Op(Unreachable), Op(Select), Op(Drop), Op(End)), true, 1},
		{"select mixed", Code(I32Const(1), I64Const(1), I32Const(0), Op(Select), Op(Drop), Op(End)), false, 0},
		{"if without else with result", Code(I32Const(1), If(I32), I32Const(1), Op(End), Op(Drop), Op(End)), false, 0},
		{"if else result", Code(I32Const(1), If(I32), I32Const(1), Op(Else), I32Const(2), Op(End), Op(Drop), Op(End)), true, 1},
		{"br to block result", Code(Block(I32), I32Const(5), Br(0), Op(End), Op(Drop), Op(End)), true, 1},
		{"br loop label carries nothing", Code(Loop(I32), I32Const(5), Br(0), Op(End), Op(Drop), Op(End)), true, 1},
		{"br_table mismatched labels", Code(Block(I32), Block(0x40), I32Const(0), BrTable([]uint32{0}, 1), Op(End), I32Const(1), Op(End), Op(Drop), Op(End)), false, 0},
		{"br_table unreachable mismatch still rejected", Code(Block(I32), Block(0x40), Op(Unreachable), BrTable([]uint32{0}, 1), Op(End), I32Const(1), Op(End), Op(Drop), Op(End)), false, 0},
		{"nested frame starts reachable", Code(Op(Unreachable), Block(0x40), Op(Drop), Op(End), Op(End)), false, 0},
		{"else without if", Code(Op(Else), Op(End)), false, 0},
		{"wide add", Code(I64Const(1), I64Const(2), I64Const(3), I64Const(4), Add128(), Op(Drop), Op(Drop), Op(End)), true, 4},
		{"mul wide", Code(I64Const(1), I64Const(2), MulWideU(), Op(Drop), Op(Drop), Op(End)), true, 2},
		{"float op", Code(Op(0x43, 0, 0, 0, 0), Op(Drop), Op(End)), false, 0},
		{"memory op without memory", Code(I32Const(0), Mem(0x28, 2, 0), Op(Drop), Op(End)), false, 0},
		{"return in unreachable", Code(Op(Return), Op(Return), Op(End)), true, 0},
	}
	for _, c := range cases {
		m := &Module{}
		m.Invoke(nil, c.body)
		mod, err := wasm.Decode(m.Bytes())
		if c.ok != (err == nil) {
			t.Errorf("%s: ok=%v err=%v", c.name, c.ok, err)
			continue
		}
		if c.ok && mod.Funcs[0].MaxOperand != c.maxH {
			t.Errorf("%s: max operand %d want %d", c.name, mod.Funcs[0].MaxOperand, c.maxH)
		}
	}
}

func TestReservationMeasure(t *testing.T) {
	// A function returning i64 whose body ends unreachable still has max >= 1.
	m := &Module{}
	f := m.AddFunc([]byte{I32}, []byte{I64}, []LocalGroup{{2, I64}}, Op(Unreachable), Op(End))
	m.Invoke(nil, Op(End))
	_ = f
	mod := mustDecode(t, m.Bytes())
	fn := mod.Funcs[0]
	if fn.MaxOperand != 1 || fn.Reservation != 1+2+1 {
		t.Fatalf("max %d reservation %d", fn.MaxOperand, fn.Reservation)
	}
	// The first local group may share the last parameter's type.
	m = &Module{}
	m.AddFunc([]byte{I32}, nil, []LocalGroup{{1, I32}, {1, I64}}, Op(End))
	m.Invoke(nil, Op(End))
	mustDecode(t, m.Bytes())
	// Neighboring groups of one type are rejected.
	m = &Module{}
	m.AddFunc(nil, nil, []LocalGroup{{1, I32}, {1, I32}}, Op(End))
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "neighbor groups")
	// Too many locals.
	m = &Module{}
	m.AddFunc(nil, nil, []LocalGroup{{16385, I32}}, Op(End))
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "locals")
	// Exactly 16384 is fine, but then any operand pushes the reservation
	// past 32768? No: 16384 + operands <= 32768.
	m = &Module{}
	m.AddFunc(nil, nil, []LocalGroup{{16384, I32}}, Op(End))
	m.Invoke(nil, Op(End))
	mustDecode(t, m.Bytes())
}

func TestTableAndElements(t *testing.T) {
	m := &Module{Table: &Limits{2, 2}}
	ret := m.Host("return")
	f := m.AddFunc(nil, nil, nil, Op(End))
	m.Invoke(nil, Code(I32Const(0), CallIndirect(m.AddType(nil, nil)), Op(End)))
	m.Elems = []Elem{{Offset: 0, Funcs: []uint32{f, ret}}}
	mod := mustDecode(t, m.Bytes())
	if mod.Table[0].Func != int32(f) || mod.Table[1].Func != int32(ret) {
		t.Fatalf("table %+v", mod.Table)
	}
	m.Elems = []Elem{{Offset: 1, Funcs: []uint32{f, f}}}
	mustReject(t, m.Bytes(), "element overflow")
}

func FuzzDecode(f *testing.F) {
	f.Add(minimal().Bytes())
	m := &Module{Memory: &Limits{1, 2}}
	m.Host("return")
	m.Invoke(nil, Code(I32Const(0), I32Const(4), Call(0), Op(End)))
	m.Data = []Data{{Offset: 0, Bytes: []byte("abcd")}}
	f.Add(m.Bytes())
	f.Fuzz(func(t *testing.T, b []byte) {
		wasm.CodePrice(b)
		wasm.Decode(b)
	})
}

func TestLocalTypeRuns(t *testing.T) {
	// params (i32, i64), locals 2×i32 then 3×i64: indices 0..6.
	want := []byte{I32, I64, I32, I32, I64, I64, I64}
	for idx, typ := range want {
		other := I64
		if typ == I64 {
			other = I32
		}
		ok := &Module{}
		ok.AddFunc([]byte{I32, I64}, nil, []LocalGroup{{Count: 2, Type: I32}, {Count: 3, Type: I64}},
			LocalGet(uint32(idx)), LocalSet(uint32(idx)), Op(End))
		ok.Invoke(nil, Op(End))
		mustDecode(t, ok.Bytes())
		// Setting the local from a value of the other type is rejected.
		bad := &Module{}
		konst := I32Const(0)
		if other == I64 {
			konst = I64Const(0)
		}
		bad.AddFunc([]byte{I32, I64}, nil, []LocalGroup{{Count: 2, Type: I32}, {Count: 3, Type: I64}},
			konst, LocalSet(uint32(idx)), Op(End))
		bad.Invoke(nil, Op(End))
		mustReject(t, bad.Bytes(), "local type")
	}
	m := &Module{}
	m.AddFunc([]byte{I32, I64}, nil, []LocalGroup{{Count: 2, Type: I32}, {Count: 3, Type: I64}},
		LocalGet(7), Op(Drop), Op(End))
	m.Invoke(nil, Op(End))
	mustReject(t, m.Bytes(), "local out of range")
}

func TestManyDeclaredLocalsAreCheap(t *testing.T) {
	// 20,000 functions each declaring 16,384 locals in one group: the
	// validator must not do work per declared local (rules §18.4).
	m := &Module{}
	for i := 0; i < 20000; i++ {
		m.AddFunc(nil, nil, []LocalGroup{{Count: 16384, Type: I64}}, LocalGet(16383), Op(Drop), Op(End))
	}
	m.Invoke(nil, Op(End))
	start := time.Now()
	mustDecode(t, m.Bytes())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("decode took %v", d)
	}
}

func TestTruncationWrapsErrInvalid(t *testing.T) {
	for _, body := range [][]byte{{0x02}, {0x3f}, {0xfc, 0x0b}, {0x41}} {
		m := &Module{Memory: &Limits{Min: 1, Max: 1}}
		m.Invoke(nil, body)
		mustReject(t, m.Bytes(), "truncated body")
	}
}
