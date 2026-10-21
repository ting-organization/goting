package vm

import (
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
	. "github.com/ting-organization/goting/internal/wasm/wasmtest"
)

// FuzzExecute deploys arbitrary bytes and, when they deploy, calls the
// contract. Anything the validator accepts must run to a status without the
// interpreter failing.
func FuzzExecute(f *testing.F) {
	for _, code := range [][]byte{samples.Counter(), samples.Echo(), samples.KeyValue(),
		samples.Proxy(false), samples.Failer(), samples.Spin(), samples.Factory(samples.Echo())} {
		f.Add(code, []byte{1, 2, 3})
	}
	m := &Module{Table: &Limits{Min: 2, Max: 2}, Globals: []Global{{Type: I64, Mutable: true, Init: 5}}}
	g := m.AddFunc([]byte{I32}, []byte{I32}, nil, LocalGet(0), I32Const(1), Op(0x6a), Op(End))
	m.Invoke(nil, Block(I32), I32Const(3), Call(g), BrIf(0), Op(Drop), I32Const(1), Op(End),
		I32Const(0), CallIndirect(m.AddType([]byte{I32}, []byte{I32})), Op(Drop),
		GlobalGet(0), I64Const(2), Op(0x7e), GlobalSet(0), Op(End))
	m.Elems = []Elem{{Offset: 0, Funcs: []uint32{g}}}
	f.Add(m.Bytes(), []byte{0})
	chainID := types.DevelopmentGenesis().ChainID()
	f.Fuzz(func(t *testing.T, code, input []byte) {
		env := &Env{
			Block: &BlockContext{ChainID: chainID, Height: 1, Timestamp: 1, Producer: []byte("p")},
			State: state.NewOverlay(state.Empty),
			Codes: NewCodeCache(1 << 24),
		}
		budget := types.ResourceVector{3_000_000, 64, 1 << 20, 1 << 20}
		guard := types.ResourceVector{^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)}
		dep := &types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: code, Input: input}
		raw := dep.Encode()
		r, err := ExecuteTransaction(env, guard, raw, dep)
		if err != nil {
			t.Fatalf("deploy: %v", err)
		}
		if !r.ResourcesUsed.LessEq(budget) {
			t.Fatalf("over budget: %v", r.ResourcesUsed)
		}
		if r.Status != types.StatusSuccess {
			return
		}
		var id hashing.Hash
		copy(id[:], r.Result)
		env.State.DiscardJournal()
		call := &types.Transaction{Kind: types.KindCall, Budget: budget, Target: id, Input: input}
		r, err = ExecuteTransaction(env, guard, call.Encode(), call)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !r.ResourcesUsed.LessEq(budget) {
			t.Fatalf("over budget: %v", r.ResourcesUsed)
		}
	})
}
