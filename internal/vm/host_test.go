package vm

import (
	"encoding/binary"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/wasm"
	. "github.com/ting-organization/goting/internal/wasm/wasmtest"
)

// callCost is what a top-level call of code costs before its first
// instruction: root dispatch, manifest and code reads, the code price (not
// prepared in a new transaction), preparation and the entry reservation.
func callCost(code []byte, pages uint64) uint64 {
	mod, err := wasm.Decode(code)
	if err != nil {
		panic(err)
	}
	return 1 + 1900 + 1900 + wasm.CodePrice(code) + preparationPrice(mod) + mod.Funcs[mod.Invoke].Reservation
}

func deployAndCall(t *testing.T, m *Module, input []byte) (*types.Receipt, []byte) {
	t.Helper()
	h := newHarness(t)
	code := m.Bytes()
	id, r := h.deploy(code, []byte{0xff}) // the initializer sees input 0xff
	if r.Status != 0 {
		t.Fatalf("deploy status %d", r.Status)
	}
	return h.call(id, input), code
}

func TestGetterAbsentRecord(t *testing.T) {
	// storage_get of an absent record with capacity 0: the output span and
	// key are checked, the key copied and the record read; nothing is copied
	// out and the result is 2^32 − 1.
	m2 := &Module{Memory: &Limits{Min: 1, Max: 1}}
	get2 := m2.Host("storage_get")
	m2.Invoke(nil, I32Const(0), I32Const(100), I32Const(0), Call(get2), Op(Drop), Op(End))
	r, code := deployAndCall(t, m2, nil)
	if r.Status != 0 {
		t.Fatalf("status %d", r.Status)
	}
	// const ×3, call 94, key copy bytes(32,1) = 4, read 1900, drop 1.
	want := callCost(code, 1) + 3 + 94 + 4 + 1900 + 1
	if r.ResourcesUsed[types.C] != want || r.ResourcesUsed[types.S] != 3 {
		t.Fatalf("absent get: used %v want C=%d S=3", r.ResourcesUsed, want)
	}
}

func TestGetterCopyCharges(t *testing.T) {
	// The initializer stores 40 bytes under key 0; the call reads it with
	// capacity 39 (no copy) and then 40 (copy of 2 words at 1 page: 8 C).
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	inputF := m.Host("input")
	get := m.Host("storage_get")
	set := m.Host("storage_set")
	m.Invoke(nil,
		I32Const(0), I32Const(0), Call(inputF), If(0x40), // initializer has input 0xff: length 1
		I32Const(0), I32Const(200), I32Const(40), Call(set), Op(Return), Op(End),
		I32Const(0), I32Const(300), I32Const(39), Call(get), Op(Drop),
		I32Const(0), I32Const(300), I32Const(40), Call(get), Op(Drop),
		Op(End))
	r, code := deployAndCall(t, m, nil)
	if r.Status != 0 {
		t.Fatalf("status %d", r.Status)
	}
	prefix := uint64(2 + 94 + 18) // two consts, input call, if (input length 0 -> skip)
	get1 := uint64(3+94+4+1900) + 1
	get2 := uint64(3+94+4+1900) + 8 + 1
	want := callCost(code, 1) + prefix + get1 + get2
	if r.ResourcesUsed[types.C] != want {
		t.Fatalf("C %d want %d", r.ResourcesUsed[types.C], want)
	}
}

func TestCallRefusalAndReceiveFailure(t *testing.T) {
	// A call whose budget exceeds what is left returns 128; a call whose
	// budget cannot pay for receiving its input returns 3; neither runs a child.
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	call := m.Host("call")
	ret := m.Host("return")
	big := uint64(1) << 62
	m.Invoke(nil,
		// budget A at 1000: C = 2^62 (too much)
		I32Const(1000), I64Const(int64(big)), Mem(0x37, 3, 0),
		// budget B at 1100: C = 1 (cannot receive 64 bytes: 2 words = 8 C)
		I32Const(1100), I64Const(1), Mem(0x37, 3, 0),
		I32Const(0),
		I32Const(0), I32Const(0), I32Const(64), I32Const(1000), I32Const(0), Call(call),
		Mem(0x3a, 0, 0),
		I32Const(1),
		I32Const(0), I32Const(0), I32Const(64), I32Const(1100), I32Const(0), Call(call),
		Mem(0x3a, 0, 0),
		I32Const(0), I32Const(2), Call(ret),
		Op(End))
	r, code := deployAndCall(t, m, nil)
	if r.Status != 0 || r.Result[0] != types.CallError || r.Result[1] != types.StatusExhaustedC {
		t.Fatalf("status %d result %v", r.Status, r.Result)
	}
	stores := uint64(2 * (1 + 1 + 1)) // two i64 stores with their consts
	call1 := uint64(1+5) + 94 + 8     // consts, call, bytes(64,2) = 2 words × 4
	call2 := uint64(1+5) + 94 + 8     // the failed receive consumed nothing
	byteStores := uint64(2 * 1)
	retCost := uint64(2) + 94 + 4
	want := callCost(code, 1) + stores + call1 + call2 + byteStores + retCost
	if r.ResourcesUsed[types.C] != want || r.ResourcesUsed[types.S] != 2 {
		t.Fatalf("used %v want C=%d S=2", r.ResourcesUsed, want)
	}
}

func TestTrapOrdering(t *testing.T) {
	// context with an unknown selector traps even when its output span is
	// also invalid; storage_set in read-only mode returns 11 before span
	// checks; call with flags 2 traps before its spans.
	cases := []struct {
		name string
		body func(m *Module) []byte
		want uint8
	}{
		{"selector before span", func(m *Module) []byte {
			ctx := m.Host("context")
			return Code(I32Const(9), I32Const(-1), I32Const(100), Call(ctx), Op(Drop), Op(End))
		}, types.StatusTrap},
		{"flags before spans", func(m *Module) []byte {
			call := m.Host("call")
			return Code(I32Const(-1), I32Const(0), I32Const(0), I32Const(0), I32Const(2), Call(call), Op(Drop), Op(End))
		}, types.StatusTrap},
		{"value of 2^32-1 bytes", func(m *Module) []byte {
			set := m.Host("storage_set")
			return Code(I32Const(0), I32Const(0), I32Const(-1), Call(set), Op(End))
		}, types.StatusTrap},
		{"zero-length span at end is valid", func(m *Module) []byte {
			emit := m.Host("emit")
			return Code(I32Const(65536), I32Const(0), Call(emit), Op(End))
		}, types.StatusSuccess},
		{"zero-length span past end traps", func(m *Module) []byte {
			emit := m.Host("emit")
			return Code(I32Const(65537), I32Const(0), Call(emit), Op(End))
		}, types.StatusTrap},
	}
	for _, c := range cases {
		m := &Module{Memory: &Limits{Min: 1, Max: 1}}
		inputF := m.Host("input")
		body := c.body(m)
		// Skip the body in the initializer (input 0xff has length 1).
		m.Invoke(nil, Code(I32Const(0), I32Const(0), Call(inputF), If(0x40), Op(Return), Op(End)), body)
		r, _ := deployAndCall(t, m, nil)
		if r.Status != c.want {
			t.Errorf("%s: status %d want %d", c.name, r.Status, c.want)
		}
	}
}

func TestReadOnlyChildCannotWrite(t *testing.T) {
	// A read-only call into a contract that emits returns 11 to the caller;
	// the caller's own emit is kept.
	h := newHarness(t)
	em := &Module{Memory: &Limits{Min: 1, Max: 1}}
	emit := em.Host("emit")
	inputF := em.Host("input")
	// The initializer (input of 2 bytes) returns at once; calls emit.
	em.Invoke(nil, I32Const(0), I32Const(0), Call(inputF), I32Const(2), Op(0x46), If(0x40), Op(Return), Op(End),
		I32Const(0), I32Const(1), Call(emit), Op(End))
	emitter, r := h.deploy(em.Bytes(), []byte{1, 2})
	if r.Status != 0 {
		t.Fatal("deploy emitter")
	}
	in := append(append([]byte{}, emitter[:]...), 'x')
	proxy, r := h.deploy(proxyModule(true), make([]byte, 32))
	if r.Status != 0 {
		t.Fatalf("deploy proxy %d", r.Status)
	}
	r = h.call(proxy, in)
	if r.Status != 0 || r.Result[0] != types.StatusReadOnly {
		t.Fatalf("result %v", r.Result)
	}
	_ = hashing.Hash{}
	_ = binary.LittleEndian
}

func proxyModule(readOnly bool) []byte { return callerModule(readOnly) }
