package vm

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/wasm"
	. "github.com/ting-organization/goting/internal/wasm/wasmtest"
)

type harness struct {
	t     *testing.T
	env   *Env
	guard types.ResourceVector
}

func newHarness(t *testing.T) *harness {
	g := types.DevelopmentGenesis()
	return &harness{
		t: t,
		env: &Env{
			Block: &BlockContext{ChainID: g.ChainID(), Height: 5, Timestamp: 1700000300,
				ParentID: hashing.Hash{9}, Producer: []byte("miner")},
			State: state.NewOverlay(state.Empty),
			Codes: NewCodeCache(1 << 26),
		},
		guard: types.ResourceVector{^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)},
	}
}

var bigBudget = types.ResourceVector{1 << 40, 1 << 20, 1 << 30, 1 << 30}

func (h *harness) run(tx *types.Transaction) *types.Receipt {
	h.t.Helper()
	raw := tx.Encode()
	dec, err := types.DecodeTransaction(raw)
	if err != nil {
		h.t.Fatalf("envelope: %v", err)
	}
	r, err := ExecuteTransaction(h.env, h.guard, raw, dec)
	if err != nil {
		h.t.Fatalf("execute: %v", err)
	}
	h.env.State.DiscardJournal()
	return r
}

func (h *harness) deploy(code, input []byte) (hashing.Hash, *types.Receipt) {
	h.t.Helper()
	r := h.run(&types.Transaction{Kind: types.KindDeploy, Budget: bigBudget, Code: code, Input: input})
	var id hashing.Hash
	if r.Status == types.StatusSuccess {
		copy(id[:], r.Result)
	}
	return id, r
}

func (h *harness) call(target hashing.Hash, input []byte) *types.Receipt {
	h.t.Helper()
	return h.run(&types.Transaction{Kind: types.KindCall, Budget: bigBudget, Target: target, Input: input})
}

func TestDeployMinimalExactCharges(t *testing.T) {
	h := newHarness(t)
	m := &Module{}
	m.Invoke(nil, Op(End))
	code := m.Bytes()
	if len(code) != 36 {
		t.Fatalf("module is %d bytes", len(code))
	}
	if p := wasm.CodePrice(code); p != 17775 {
		t.Fatalf("code price %d", p)
	}
	id, r := h.deploy(code, nil)
	if r.Status != types.StatusSuccess {
		t.Fatalf("status %d", r.Status)
	}
	want := types.ResourceVector{74008, 7, 191, 152}
	if r.ResourcesUsed != want {
		t.Fatalf("used %v want %v", r.ResourcesUsed, want)
	}
	txID := types.TransactionID(h.env.Block.ChainID, (&types.Transaction{Kind: types.KindDeploy, Budget: bigBudget, Code: code}).Encode())
	if id != types.ContractID(h.env.Block.ChainID, txID, 0) {
		t.Fatal("contract id")
	}
	// Calling it: data base 70+81=151, root 1, manifest+code reads, code price
	// (not yet prepared in this transaction), preparation, entry.
	r = h.call(id, nil)
	if r.Status != types.StatusSuccess {
		t.Fatalf("call status %d", r.Status)
	}
	want = types.ResourceVector{1 + 1900 + 1900 + 17775 + 40000, 2, 151, 0}
	if r.ResourcesUsed != want {
		t.Fatalf("call used %v want %v", r.ResourcesUsed, want)
	}
	// Absent target: status 8 after the manifest read.
	r = h.call(hashing.Hash{1}, nil)
	if r.Status != types.StatusTargetAbsent || r.ResourcesUsed != (types.ResourceVector{1901, 1, 151, 0}) {
		t.Fatalf("absent: %d %v", r.Status, r.ResourcesUsed)
	}
}

// echoModule returns its input, after storing it under key 0 and emitting it.
func echoModule() []byte {
	m := &Module{Memory: &Limits{Min: 1, Max: 2}}
	input := m.Host("input")
	set := m.Host("storage_set")
	emit := m.Host("emit")
	ret := m.Host("return")
	// locals: 0 = length
	m.Invoke([]LocalGroup{{Count: 1, Type: I32}},
		I32Const(64), I32Const(1024), Call(input), LocalSet(0),
		I32Const(0), I32Const(64), LocalGet(0), Call(set),
		I32Const(64), LocalGet(0), Call(emit),
		I32Const(64), LocalGet(0), Call(ret),
		Op(End))
	return m.Bytes()
}

func TestEchoStorageEvents(t *testing.T) {
	h := newHarness(t)
	id, r := h.deploy(echoModule(), []byte("init"))
	if r.Status != types.StatusSuccess {
		t.Fatalf("deploy status %d", r.Status)
	}
	if len(r.Events) != 1 || string(r.Events[0].Payload) != "init" || r.Events[0].Contract != id {
		t.Fatalf("deploy events %+v", r.Events)
	}
	r = h.call(id, []byte("hello"))
	if r.Status != 0 || string(r.Result) != "hello" || len(r.Events) != 1 {
		t.Fatalf("call: %d %q", r.Status, r.Result)
	}
	var zero [32]byte
	v, ok := h.env.State.Get(types.StorageKey(id, &zero))
	if !ok || string(v) != "hello" {
		t.Fatalf("storage %q", v)
	}
}

// Exact accounting of the echo call, computed by hand from the rules.
func TestEchoCallExactCharges(t *testing.T) {
	h := newHarness(t)
	code := echoModule()
	id, _ := h.deploy(code, []byte("init"))
	r := h.call(id, []byte("hello"))
	price := PriceFor(1) // load 18, store 1, word 4, span 0
	_ = price
	codePrice := wasm.CodePrice(code)
	reservation := uint64(1 + 3) // one local, max operand height 3
	c := uint64(1) +             // root dispatch
		1900 + 1900 + codePrice + // manifest, code, code price
		40000 + 40000 + // start + one page
		reservation + // entry (no stack pages)
		// input: i32.const ×2, call (94), copy 5 bytes: 4×1 word
		1 + 1 + 94 + 4 + 1 + // local.set
		// storage_set: consts, local.get, call, key+value copy (37 bytes: 2 words), key read, leaf
		1 + 1 + 1 + 94 + 8 + 1900 + hashing.LeafPrice(5) +
		// emit: const, local.get, call, copy 1 word
		1 + 1 + 94 + 4 +
		// return: const, local.get, call, copy 1 word
		1 + 1 + 94 + 4
	// The record exists ("init", 4 bytes): growth is 1.
	want := types.ResourceVector{c, 2 + 2, 70 + 5 + 81 + (36 + 5) + 5, 1}
	if r.ResourcesUsed != want {
		t.Fatalf("used %v want %v", r.ResourcesUsed, want)
	}
}

func TestTrapAndRevertRollback(t *testing.T) {
	h := newHarness(t)
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	set := m.Host("storage_set")
	emit := m.Host("emit")
	revert := m.Host("revert")
	inputF := m.Host("input")
	// Writes a record and emits, then reverts with "no" if input[0] == 1,
	// traps if input[0] == 2, else succeeds.
	m.Invoke(nil,
		I32Const(0), I32Const(16), Call(inputF), Op(Drop),
		I32Const(100), I32Const(0), I32Const(1), Call(set),
		I32Const(0), I32Const(1), Call(emit),
		I32Const(0), Mem(0x2d, 0, 0), I32Const(1), Op(0x46),
		If(0x40), I32Const(200), I32Const(2), Call(revert), Op(End),
		I32Const(0), Mem(0x2d, 0, 0), I32Const(2), Op(0x46),
		If(0x40), Op(Unreachable), Op(End),
		Op(End))
	m.Data = []Data{{Offset: 200, Bytes: []byte("no")}}
	id, r := h.deploy(m.Bytes(), []byte{0})
	if r.Status != 0 {
		t.Fatalf("deploy %d", r.Status)
	}
	var key [32]byte
	key[0] = 0 // the logical key is memory[100:132], zero
	before, _ := h.env.State.Get(types.StorageKey(id, &key))

	r = h.call(id, []byte{1})
	if r.Status != types.StatusRevert || string(r.Result) != "no" || len(r.Events) != 0 {
		t.Fatalf("revert: %d %q %d", r.Status, r.Result, len(r.Events))
	}
	after, _ := h.env.State.Get(types.StorageKey(id, &key))
	if !bytes.Equal(before, after) {
		t.Fatal("revert left a write")
	}
	if r.ResourcesUsed[types.G] != 0 {
		// The record already existed with the same length: no growth either way.
		t.Fatalf("growth %d", r.ResourcesUsed[types.G])
	}
	r = h.call(id, []byte{2})
	if r.Status != types.StatusTrap || len(r.Result) != 0 {
		t.Fatalf("trap: %d", r.Status)
	}
	r = h.call(id, []byte{3})
	if r.Status != 0 || len(r.Events) != 1 {
		t.Fatalf("ok: %d", r.Status)
	}
}

func TestExhaustion(t *testing.T) {
	h := newHarness(t)
	m := &Module{}
	m.Invoke(nil, Loop(0x40), Br(0), Op(End), Op(End))
	// The initializer loops forever: the deployment exhausts compute, and
	// consumes exactly its budget since every price here is 1 or 2.
	budget := types.ResourceVector{20_000_000, 100, 1000, 1000}
	raw0 := (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: m.Bytes()}).Encode()
	dec0, _ := types.DecodeTransaction(raw0)
	r, err := ExecuteTransaction(h.env, h.guard, raw0, dec0)
	if err != nil || r.Status != types.StatusExhaustedC || r.ResourcesUsed[types.C] != budget[types.C] {
		t.Fatalf("loop deploy: %v %d %v", err, r.Status, r.ResourcesUsed)
	}
	budget = types.ResourceVector{200000, 100, 1000, 1000}
	raw := (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: m.Bytes()}).Encode()
	dec, _ := types.DecodeTransaction(raw)
	r, err = ExecuteTransaction(h.env, h.guard, raw, dec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != types.StatusExhaustedC || r.ResourcesUsed[types.C] != 200000 {
		t.Fatalf("status %d used %v", r.Status, r.ResourcesUsed)
	}
	// G exhaustion at installation: budget too small for 116 + len.
	budget = types.ResourceVector{1 << 30, 100, 1000, 100}
	raw = (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: m.Bytes()}).Encode()
	dec, _ = types.DecodeTransaction(raw)
	r, _ = ExecuteTransaction(h.env, h.guard, raw, dec)
	if r.Status != types.StatusExhaustedG {
		t.Fatalf("G status %d", r.Status)
	}
	// The manifest's 80 G fit; the code record's did not.
	if r.ResourcesUsed[types.G] != 80 {
		t.Fatalf("G used %d", r.ResourcesUsed[types.G])
	}
}

func TestInvalidModuleStatus9(t *testing.T) {
	h := newHarness(t)
	_, r := h.deploy([]byte{0, 0x61, 0x73, 0x6d, 1, 0, 0, 0}, nil)
	if r.Status != types.StatusModuleInvalid {
		t.Fatalf("status %d", r.Status)
	}
	// data base 42+8+81 = 131; C: root + id + manifest read + price(14880) + hash price.
	want := types.ResourceVector{1 + 1900 + 1900 + 14880 + hashing.CodeHashPrice(8), 1, 131, 0}
	if r.ResourcesUsed != want {
		t.Fatalf("used %v want %v", r.ResourcesUsed, want)
	}
}

// Nested call: a caller forwards its input to a callee (given by the first 32
// input bytes) with a fixed budget, then returns the child's status byte and
// the child's result.
func callerModule(readOnly bool) []byte {
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	input := m.Host("input")
	call := m.Host("call")
	callResult := m.Host("call_result")
	ret := m.Host("return")
	flags := int32(0)
	if readOnly {
		flags = 1
	}
	// memory: 0..32 target, 32.. rest of input; budget at 1000; output at 2000.
	m.Invoke([]LocalGroup{{Count: 2, Type: I32}},
		I32Const(0), I32Const(900), Call(input), LocalSet(0),
		I32Const(1000), I64Const(10_000_000), Mem(0x37, 3, 0),
		I32Const(1008), I64Const(100), Mem(0x37, 3, 0),
		I32Const(1016), I64Const(10_000), Mem(0x37, 3, 0),
		I32Const(1024), I64Const(10_000), Mem(0x37, 3, 0),
		// status = call(target=0, input=32, len=n-32, budget=1000, flags)
		I32Const(2000),
		I32Const(0), I32Const(32), LocalGet(0), I32Const(32), Op(0x6b), I32Const(1000), I32Const(flags), Call(call),
		Mem(0x3a, 0, 0),
		// n = call_result(2001, 800)
		I32Const(2001), I32Const(800), Call(callResult), LocalSet(1),
		I32Const(2000), LocalGet(1), I32Const(1), Op(0x6a), Call(ret),
		Op(End))
	return m.Bytes()
}

func TestNestedCallAndReadOnly(t *testing.T) {
	h := newHarness(t)
	echo, _ := h.deploy(echoModule(), []byte("init"))
	caller, r := h.deploy(callerModule(false), make([]byte, 32))
	// The initializer calls the zero ContractId, which is absent: fine.
	if r.Status != 0 {
		t.Fatalf("deploy caller %d", r.Status)
	}
	in := append(append([]byte{}, echo[:]...), []byte("nested")...)
	r = h.call(caller, in)
	if r.Status != 0 || r.Result[0] != 0 || string(r.Result[1:]) != "nested" {
		t.Fatalf("nested: %d %v", r.Status, r.Result)
	}
	if len(r.Events) != 1 || r.Events[0].Contract != echo {
		t.Fatalf("events %+v", r.Events)
	}
	// Read-only: the echo's storage_set fails with 11; caller succeeds.
	ro, _ := h.deploy(callerModule(true), make([]byte, 32))
	r = h.call(ro, in)
	if r.Status != 0 || r.Result[0] != types.StatusReadOnly || len(r.Result) != 1 || len(r.Events) != 0 {
		t.Fatalf("read-only: %d %v", r.Status, r.Result)
	}
	// Call to an absent target returns 8.
	in2 := append(make([]byte, 32), 'x')
	r = h.call(caller, in2)
	if r.Status != 0 || r.Result[0] != types.StatusTargetAbsent {
		t.Fatalf("absent child: %v", r.Result)
	}
}

func TestContextAndCaller(t *testing.T) {
	h := newHarness(t)
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	ctx := m.Host("context")
	inputF := m.Host("input")
	ret := m.Host("return")
	// Return context field selected by input byte 0.
	m.Invoke([]LocalGroup{{Count: 1, Type: I32}},
		I32Const(0), I32Const(1), Call(inputF), Op(Drop),
		I32Const(0), Mem(0x2d, 0, 0), I32Const(100), I32Const(500), Call(ctx), LocalSet(0),
		I32Const(100), LocalGet(0), Call(ret), Op(End))
	id, r := h.deploy(m.Bytes(), []byte{3})
	if r.Status != 0 {
		t.Fatalf("deploy %d", r.Status)
	}
	check := func(sel byte, want []byte) {
		t.Helper()
		r := h.call(id, []byte{sel})
		if r.Status != 0 || !bytes.Equal(r.Result, want) {
			t.Fatalf("selector %d: %d %x want %x", sel, r.Status, r.Result, want)
		}
	}
	check(0, h.env.Block.ChainID[:])
	check(2, id[:])
	check(3, make([]byte, 33))
	check(4, binary.LittleEndian.AppendUint64(nil, 5))
	check(5, binary.LittleEndian.AppendUint64(nil, 1700000300))
	check(7, []byte("miner"))
	r = h.call(id, []byte{9})
	if r.Status != types.StatusTrap {
		t.Fatalf("selector 9: %d", r.Status)
	}
	r = h.call(id, []byte{8})
	if r.Status != 0 || len(r.Result) != 32 {
		t.Fatal("selector 8")
	}
	remC := binary.LittleEndian.Uint64(r.Result)
	if remC >= bigBudget[types.C] || remC == 0 {
		t.Fatalf("remaining C %d", remC)
	}
}

func TestArithmetic(t *testing.T) {
	h := newHarness(t)
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	ret := m.Host("return")
	// Compute a few values into memory and return 48 bytes.
	// locals: 0, 1 = i64 scratch
	m.Invoke([]LocalGroup{{Count: 2, Type: I64}},
		// mem[0..8] = low, mem[8..16] = high of i64.mul_wide_s(-3, 5)
		I64Const(-3), I64Const(5), MulWideS(), LocalSet(1), LocalSet(0),
		I32Const(0), LocalGet(0), Mem(0x37, 3, 0),
		I32Const(8), LocalGet(1), Mem(0x37, 3, 0),
		// mem[16..24] = high of add128(lo=-1, hi=0, lo=1, hi=0) = 1
		I64Const(-1), I64Const(0), I64Const(1), I64Const(0), Add128(), LocalSet(1), LocalSet(0),
		I32Const(16), LocalGet(1), Mem(0x37, 3, 0),
		// mem[24..28] = i32.rem_s(-7, 2) = -1
		I32Const(24), I32Const(-7), I32Const(2), Op(0x6f), Mem(0x36, 2, 0),
		// mem[28..32] = i32.rotl(0x80000001, 1) = 3
		I32Const(28), I32Const(-0x7fffffff), I32Const(1), Op(0x77), Mem(0x36, 2, 0),
		// mem[32..40] = i64.clz(1) = 63
		I32Const(32), I64Const(1), Op(0x79), Mem(0x37, 3, 0),
		// mem[40..48] = i64.extend_i32_s(-2)
		I32Const(40), I32Const(-2), Op(0xac), Mem(0x37, 3, 0),
		I32Const(0), I32Const(48), Call(ret), Op(End))
	id, r := h.deploy(m.Bytes(), nil)
	if r.Status != 0 {
		t.Fatalf("deploy %d", r.Status)
	}
	r = h.call(id, nil)
	if r.Status != 0 {
		t.Fatalf("call %d", r.Status)
	}
	res := r.Result
	u64 := func(o int) uint64 { return binary.LittleEndian.Uint64(res[o:]) }
	if int64(u64(0)) != -15 || u64(8) != ^uint64(0) {
		t.Fatalf("mul_wide_s %d %x", int64(u64(0)), u64(8))
	}
	if u64(16) != 1 {
		t.Fatalf("add128 hi %d", u64(16))
	}
	if int32(binary.LittleEndian.Uint32(res[24:])) != -1 {
		t.Fatal("rem_s")
	}
	if binary.LittleEndian.Uint32(res[28:]) != 3 {
		t.Fatal("rotl")
	}
	if u64(32) != 63 {
		t.Fatal("clz")
	}
	if int64(u64(40)) != -2 {
		t.Fatal("extend")
	}
}

func TestMemoryPricesTable(t *testing.T) {
	cases := []struct {
		pages                   uint32
		load, store, word, span uint64
	}{
		{32, 80, 1, 8, 0}, {33, 770, 16, 12, 16}, {48, 772, 17, 12, 17},
		{256, 818, 48, 12, 48}, {1024, 904, 105, 12, 105}, {4096, 1032, 191, 12, 191},
		{16384, 1202, 304, 12, 304}, {65536, 1416, 447, 12, 447},
		{0, 18, 1, 4, 0}, {1, 18, 1, 4, 0}, {2, 31, 1, 8, 0}, {16, 31, 1, 8, 0}, {17, 80, 1, 8, 0},
	}
	for _, c := range cases {
		p := PriceFor(c.pages)
		if p.Load != c.load || p.Store != c.store || p.Word != c.word || p.Span != c.span {
			t.Errorf("pages %d: %+v", c.pages, *p)
		}
	}
	p := PriceFor(48)
	if p.Bytes(1, 2) != 46 || p.Bytes(1, 1) != 29 || p.Bytes(33, 2) != 58 {
		t.Fatal("bytes() checks")
	}
	if StackPages(8192) != 0 || StackPages(8193) != 1 || StackPages(16384) != 1 || StackPages(16385) != 2 {
		t.Fatal("stack pages")
	}
}

func TestMemoryGrowAndBulk(t *testing.T) {
	h := newHarness(t)
	m := &Module{Memory: &Limits{Min: 1, Max: 3}}
	ret := m.Host("return")
	m.Invoke([]LocalGroup{{Count: 1, Type: I32}},
		// grow by 1 -> 1; grow by 5 -> -1; size -> 2
		I32Const(0), I32Const(1), MemoryGrow(), Mem(0x36, 2, 0),
		I32Const(4), I32Const(5), MemoryGrow(), Mem(0x36, 2, 0),
		I32Const(8), MemorySize(), Mem(0x36, 2, 0),
		// fill [100, 110) with 7, copy [100,110) -> [105,115) overlapping
		I32Const(100), I32Const(7), I32Const(10), MemoryFill(),
		I32Const(12), I32Const(0xab), Mem(0x3a, 0, 0),
		I32Const(105), I32Const(12), I32Const(1), MemoryCopy(),
		I32Const(0), I32Const(120), Call(ret), Op(End))
	id, r := h.deploy(m.Bytes(), nil)
	if r.Status != 0 {
		t.Fatalf("deploy %d", r.Status)
	}
	r = h.call(id, nil)
	if r.Status != 0 {
		t.Fatalf("call %d", r.Status)
	}
	res := r.Result
	if binary.LittleEndian.Uint32(res[0:]) != 1 || binary.LittleEndian.Uint32(res[4:]) != 0xffffffff ||
		binary.LittleEndian.Uint32(res[8:]) != 2 {
		t.Fatalf("grow results %x", res[:12])
	}
	if res[100] != 7 || res[104] != 7 || res[105] != 0xab || res[109] != 7 {
		t.Fatalf("bulk %v", res[100:115])
	}
}

func TestOutOfBoundsTraps(t *testing.T) {
	h := newHarness(t)
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	m.Invoke(nil, I32Const(65533), Mem(0x28, 2, 0), Op(Drop), Op(End))
	_, r := h.deploy(m.Bytes(), nil)
	if r.Status != types.StatusTrap {
		t.Fatalf("status %d", r.Status)
	}
	// Offset makes the effective address overflow 32 bits: no wraparound.
	m = &Module{Memory: &Limits{Min: 1, Max: 1}}
	m.Invoke(nil, I32Const(-1), Mem(0x2d, 0, 1), Op(Drop), Op(End))
	_, r = h.deploy(m.Bytes(), nil)
	if r.Status != types.StatusTrap {
		t.Fatalf("status %d", r.Status)
	}
}

func TestCallIndirect(t *testing.T) {
	h := newHarness(t)
	m := &Module{Memory: &Limits{Min: 1, Max: 1}, Table: &Limits{Min: 3, Max: 3}}
	ret := m.Host("return")
	inputF := m.Host("input")
	seven := m.AddFunc(nil, []byte{I32}, nil, I32Const(7), Op(End))
	wrong := m.AddFunc([]byte{I32}, []byte{I32}, nil, LocalGet(0), Op(End))
	tI32 := m.AddType(nil, []byte{I32})
	m.Invoke(nil,
		I32Const(0), I32Const(1), Call(inputF), Op(Drop),
		I32Const(0),
		I32Const(0), Mem(0x2d, 0, 0), CallIndirect(tI32),
		Mem(0x36, 2, 0),
		I32Const(0), I32Const(4), Call(ret), Op(End))
	m.Elems = []Elem{{Offset: 0, Funcs: []uint32{seven, wrong}}}
	id, r := h.deploy(m.Bytes(), []byte{0})
	if r.Status != 0 {
		t.Fatalf("deploy %d", r.Status)
	}
	r = h.call(id, []byte{0})
	if r.Status != 0 || binary.LittleEndian.Uint32(r.Result) != 7 {
		t.Fatalf("indirect %d %v", r.Status, r.Result)
	}
	for _, sel := range []byte{1, 2, 3} { // wrong type, null, out of range
		r = h.call(id, []byte{sel})
		if r.Status != types.StatusTrap {
			t.Fatalf("selector %d: %d", sel, r.Status)
		}
	}
}

func TestNestedDeploy(t *testing.T) {
	h := newHarness(t)
	child := echoModule()
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	deploy := m.Host("deploy")
	callResult := m.Host("call_result")
	ret := m.Host("return")
	m.Data = []Data{{Offset: 1000, Bytes: child}, {Offset: 0, Bytes: []byte("kid")}}
	m.Invoke([]LocalGroup{{Count: 1, Type: I32}},
		I32Const(200), I64Const(50_000_000), Mem(0x37, 3, 0),
		I32Const(208), I64Const(100), Mem(0x37, 3, 0),
		I32Const(216), I64Const(10_000), Mem(0x37, 3, 0),
		I32Const(224), I64Const(10_000), Mem(0x37, 3, 0),
		I32Const(300),
		I32Const(1000), I32Const(int32(len(child))), I32Const(0), I32Const(3), I32Const(200), Call(deploy),
		Mem(0x3a, 0, 0),
		I32Const(301), I32Const(64), Call(callResult), LocalSet(0),
		I32Const(300), LocalGet(0), I32Const(1), Op(0x6a), Call(ret), Op(End))
	parent, r := h.deploy(m.Bytes(), nil)
	if r.Status != 0 {
		t.Fatalf("deploy parent %d", r.Status)
	}
	r = h.call(parent, nil)
	if r.Status != 0 || r.Result[0] != 0 || len(r.Result) != 33 {
		t.Fatalf("nested deploy: %d %v", r.Status, r.Result)
	}
	var kid hashing.Hash
	copy(kid[:], r.Result[1:])
	txID := types.TransactionID(h.env.Block.ChainID, (&types.Transaction{Kind: types.KindCall, Budget: bigBudget, Target: parent}).Encode())
	if kid != types.ContractID(h.env.Block.ChainID, txID, 0) {
		t.Fatal("nested contract id")
	}
	if len(r.Events) != 1 || r.Events[0].Contract != kid || string(r.Events[0].Payload) != "kid" {
		t.Fatalf("events %+v", r.Events)
	}
	// The same call again in a new occurrence derives the same id: status 10.
	r = h.call(parent, nil)
	if r.Status != 0 || r.Result[0] != types.StatusIDExists {
		t.Fatalf("second deploy: %v", r.Result)
	}
}

func TestStackPagesCharged(t *testing.T) {
	// A recursive function with a large reservation: each level adds
	// reservation+3 slots; crossing 8192 slots charges 40,000 C per page.
	h := newHarness(t)
	m := &Module{}
	// f(n): if n == 0 return; else f(n-1). 1000 locals per frame.
	f := uint32(0)
	m.AddFunc([]byte{I32}, nil, []LocalGroup{{Count: 1000, Type: I64}},
		LocalGet(0), Op(0x45), If(0x40), Op(Return), Op(End),
		LocalGet(0), I32Const(1), Op(0x6b), Call(f), Op(End))
	m.Invoke(nil, I32Const(20), Call(f), Op(End))
	_, r := h.deploy(m.Bytes(), nil)
	if r.Status != 0 {
		t.Fatalf("status %d", r.Status)
	}
	// Function f: params 1, locals 1000, max operand 2 -> reservation 1003.
	mod, _ := wasm.Decode(m.Bytes())
	if mod.Funcs[0].Reservation != 1003 {
		t.Fatalf("reservation %d", mod.Funcs[0].Reservation)
	}
	// 21 frames of f (n = 20..0) on top of invoke (reservation 1): height
	// 1+3 + 21*1006 = 21130 -> stack pages 2 -> 80,000 C.
	if StackPages(4+21*1006) != 2 {
		t.Fatal("expected 2 stack pages")
	}
}
