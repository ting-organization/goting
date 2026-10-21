// Package conformance runs the reference repository's conformance material
// against this implementation: the consensus vectors
// (conformance/vectors), the mined voting chain (conformance/chains) and the
// production and development seeds (conformance/seed).
package conformance

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/commit"
	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/seed"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/vm"
	"github.com/ting-organization/goting/internal/wasm"
)

// Result is one vector's outcome.
type Result struct {
	Group, Name string
	Pass        bool
	Detail      string
}

type obj = map[string]any

func load(path string) (obj, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.UseNumber()
	var o obj
	if err := d.Decode(&o); err != nil {
		return nil, err
	}
	return o, nil
}

func bigOf(v any) *big.Int {
	switch x := v.(type) {
	case json.Number:
		b, ok := new(big.Int).SetString(x.String(), 10)
		if ok {
			return b
		}
	case string:
		b, ok := new(big.Int).SetString(x, 10)
		if ok {
			return b
		}
	}
	panic(fmt.Sprintf("not an integer: %v", v))
}

func u64(v any) uint64 { return bigOf(v).Uint64() }

func hexOf(v any) []byte {
	b, err := hex.DecodeString(v.(string))
	if err != nil {
		panic(err)
	}
	return b
}

func hashOf(v any) hashing.Hash {
	h, err := hashing.ParseHash(v.(string))
	if err != nil {
		panic(err)
	}
	return h
}

func vec4(v any) types.ResourceVector {
	a := v.([]any)
	var r types.ResourceVector
	for i := range r {
		r[i] = u64(a[i])
	}
	return r
}

func targetOf(v any) types.Target {
	t, err := types.ParseTarget(v.(string))
	if err != nil {
		panic(err)
	}
	return t
}

// Runner holds shared state, such as the base chain the header and block
// vectors build on.
type Runner struct {
	Dir  string // the reference repository's conformance directory
	base [][]byte
}

// Vectors runs every vector file.
func (r *Runner) Vectors() ([]Result, error) {
	root := filepath.Join(r.Dir, "vectors")
	b, err := load(filepath.Join(root, "base", "development.json"))
	if err != nil {
		return nil, err
	}
	for _, h := range b["expect"].(obj)["blocks_hex"].([]any) {
		r.base = append(r.base, hexOf(h))
	}
	groups, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, g := range groups {
		if !g.IsDir() || g.Name() == "base" {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, g.Name()))
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			res := Result{Group: g.Name(), Name: strings.TrimSuffix(f.Name(), ".json")}
			v, err := load(filepath.Join(root, g.Name(), f.Name()))
			if err != nil {
				res.Detail = err.Error()
			} else {
				res.Pass, res.Detail = r.run(g.Name(), v)
			}
			out = append(out, res)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// run checks one vector; a panic while reading it is reported as a failure.
func (r *Runner) run(group string, v obj) (pass bool, detail string) {
	defer func() {
		if p := recover(); p != nil {
			pass, detail = false, fmt.Sprintf("panic: %v", p)
		}
	}()
	in, _ := v["input"].(obj)
	ex, _ := v["expect"].(obj)
	switch group {
	case "v1-primitives":
		return r.primitives(in, ex)
	case "v2-envelopes":
		return envelope(in, ex)
	case "v3-headers", "v4-blocks":
		return r.block(in, ex)
	case "v5-difficulty":
		got := consensus.Retarget(consensus.TargetInt(targetOf(in["anchor_target_hex"])),
			consensus.TargetInt(targetOf(in["pow_limit_hex"])), u64(in["interval"]), u64(in["half_life"]),
			bigOf(in["blocks"]), bigOf(in["elapsed"]))
		want := consensus.TargetInt(targetOf(ex["target_hex"]))
		return got.Cmp(want) == 0, fmt.Sprintf("target %x, want %x", got, want)
	case "v6-voting":
		return voting(in, ex)
	case "v7-structural":
		return structural(in, ex)
	case "v8-work":
		return work(in, ex)
	case "v9-tables":
		return tables(in, ex)
	}
	return false, "unknown group"
}

func (r *Runner) primitives(in, ex obj) (bool, string) {
	switch {
	case in["profile"] != nil && ex["config_hex"] != nil:
		g := &types.GenesisConfig{
			ProtocolVersion:  1,
			GenesisTimestamp: u64(in["timestamp"]),
			InitialTarget:    targetOf(in["initial_target_hex"]),
			PowLimit:         targetOf(in["pow_limit_hex"]),
			TargetInterval:   uint32(u64(in["interval"])),
			HalfLife:         uint32(u64(in["half_life"])),
			VoteEpochLength:  uint32(u64(in["vote_epoch"])),
			InitialLimits:    vec4(in["initial"]),
			MinimumLimits:    vec4(in["minimum"]),
		}
		cfg := hex.EncodeToString(g.Encode())
		id := g.ChainID()
		ok := cfg == ex["config_hex"] && id == hashOf(ex["chain_id"]) &&
			types.GenesisBlockID(id) == hashOf(ex["genesis_block_id"])
		return ok, fmt.Sprintf("config %s chain %s", cfg, id)
	case in["tag"] != nil && in["bytes_hex"] != nil:
		got := hashing.H(in["tag"].(string), hexOf(in["bytes_hex"]))
		return got == hashOf(ex["hash"]), got.String()
	case in["items_hex"] != nil:
		var items [][]byte
		for _, it := range in["items_hex"].([]any) {
			items = append(items, hexOf(it))
		}
		got := commit.VectorRoot(in["tag"].(string), items)
		return got == hashOf(ex["root"]), got.String()
	case in["namespace"] != nil:
		lk := [32]byte(hashOf(in["logical_key"]))
		got := types.RecordKey(uint8(u64(in["namespace"])), hashOf(in["contract"]), &lk)
		return got == hashOf(ex["key"]), got.String()
	case in["writes"] != nil:
		o := state.NewOverlay(state.Empty)
		for _, w := range in["writes"].([]any) {
			wo := w.(obj)
			k := hashOf(wo["key"])
			if d, _ := wo["delete"].(bool); d {
				o.Delete(k)
			} else {
				o.Set(k, hexOf(wo["value_hex"]))
			}
		}
		got := o.Commit().Root()
		return got == hashOf(ex["root"]), got.String()
	}
	return false, "unknown primitive"
}

func envelope(in, ex obj) (bool, string) {
	raw := hexOf(in["transaction_hex"])
	_, err := types.DecodeTransaction(raw)
	valid := err == nil
	if valid != ex["valid"].(bool) {
		return false, fmt.Sprintf("valid=%v (%v)", valid, err)
	}
	if id, ok := ex["id_on_development"]; ok {
		got := types.TransactionID(types.DevelopmentGenesis().ChainID(), raw)
		return got == hashOf(id), got.String()
	}
	return true, fmt.Sprint(err)
}

// block processes a vector block on the base chain's first `on` blocks.
func (r *Runner) block(in, ex obj) (bool, string) {
	on := int(u64(in["on"]))
	c, err := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	if err != nil {
		return false, err.Error()
	}
	for i := 0; i < on; i++ {
		if _, err := c.ProcessBlock(r.base[i]); err != nil {
			return false, fmt.Sprintf("base block %d: %v", i+1, err)
		}
	}
	parent, _ := c.AtHeight(uint64(on))
	before, _ := c.StateAt(parent)
	err = c.ValidateNext(parent, hexOf(in["block_hex"]))
	valid := err == nil
	if after, _ := c.StateAt(parent); after.Root() != before.Root() {
		return false, "validation changed the parent's state"
	}
	want := ex["valid"].(bool)
	if !valid && !errors.Is(err, chain.ErrInvalid) {
		return false, fmt.Sprintf("not a verdict: %v", err)
	}
	return valid == want, fmt.Sprintf("valid=%v (%v)", valid, err)
}

func voting(in, ex obj) (bool, string) {
	switch {
	case in["headers"] != nil:
		g, _ := types.NetworkByName(in["profile"].(string))
		var votes [][4]uint8
		var works []*big.Int
		for _, h := range in["headers"].([]any) {
			ho := h.(obj)
			var v [4]uint8
			for i, x := range ho["votes"].([]any) {
				v[i] = uint8(u64(x))
			}
			votes = append(votes, v)
			works = append(works, consensus.Work(targetOf(ho["target_hex"])))
		}
		want := ex["active_limits_after_each_epoch"].([]any)
		E := uint64(g.VoteEpochLength)
		vs := consensus.GenesisVoteState(g)
		for e := 0; e < len(want); e++ {
			h := uint64(e)*E + 1 // first block of epoch e
			// Advance through the heights up to h.
			for x := uint64(1); x <= h; x++ {
				vs = consensus.NextVoteState(g, vs, x, func() *consensus.Tally {
					t := consensus.NewTally()
					closed := consensus.Epoch(x-1, g.VoteEpochLength)
					for b := closed*E + 1; b <= closed*E+E; b++ {
						if int(b-1) < len(votes) {
							t.Add(votes[b-1], works[b-1])
						} else {
							t.Add([4]uint8{}, consensus.Work(g.InitialTarget))
						}
					}
					return t
				})
			}
			if vs.Limits != vec4(want[e]) {
				return false, fmt.Sprintf("epoch %d limits %v, want %v", e, vs.Limits, vec4(want[e]))
			}
			vs = consensus.GenesisVoteState(g)
		}
		return true, ""
	case in["work_and_signal"] != nil:
		t := consensus.NewTally()
		for _, ws := range in["work_and_signal"].([]any) {
			p := ws.([]any)
			t.Add([4]uint8{uint8(u64(p[1]))}, bigOf(p[0]))
		}
		got := t.Decide()[0]
		return uint64(got) == u64(ex["direction"]), fmt.Sprintf("direction %d", got)
	case in["limit"] != nil:
		lim := types.ResourceVector{u64(in["limit"])}
		floor := types.ResourceVector{u64(in["floor"])}
		got := consensus.ApplyDirections(lim, [4]consensus.Direction{consensus.Direction(u64(in["direction"]))}, floor)[0]
		return got == u64(ex["limit"]), fmt.Sprintf("limit %d", got)
	}
	return false, "unknown voting vector"
}

// buildModule assembles a module from its preamble and sections, each
// payload a list of hex parts, some repeated.
func buildModule(m obj) []byte {
	out := hexOf(m["preamble_hex"])
	for _, s := range m["sections"].([]any) {
		so := s.(obj)
		var payload []byte
		for _, part := range so["payload"].([]any) {
			po := part.(obj)
			if h, ok := po["hex"]; ok {
				payload = append(payload, hexOf(h)...)
			} else {
				payload = append(payload, bytes.Repeat(hexOf(po["repeat_hex"]), int(u64(po["times"])))...)
			}
		}
		out = append(out, byte(u64(so["id"])))
		out = wasm.AppendU32(out, uint32(len(payload)))
		out = append(out, payload...)
	}
	return out
}

func structural(in, ex obj) (bool, string) {
	code := buildModule(in["module"].(obj))
	mod, err := wasm.Decode(code)
	accepted := err == nil
	if accepted != ex["accepted"].(bool) {
		return false, fmt.Sprintf("accepted=%v (%v)", accepted, err)
	}
	if !accepted {
		return true, fmt.Sprint(err)
	}
	price := wasm.CodePrice(code)
	slots := mod.Funcs[mod.Invoke].Reservation
	prep := vm.PreparationPrice(mod)
	ok := price == u64(ex["code_price"]) && slots == u64(ex["entry_slots"]) && prep == u64(ex["preparation"])
	return ok, fmt.Sprintf("code price %d, entry slots %d, preparation %d", price, slots, prep)
}

func work(in, ex obj) (bool, string) {
	sum := new(big.Int)
	if t, ok := in["target_hex"]; ok {
		sum = consensus.Work(targetOf(t))
	} else {
		for _, t := range in["targets_hex"].([]any) {
			sum.Add(sum, consensus.Work(targetOf(t)))
		}
	}
	return sum.Cmp(bigOf(ex["work"])) == 0, sum.String()
}

func tables(in, ex obj) (bool, string) {
	switch {
	case in["lengths"] != nil:
		want := ex["compute"].([]any)
		for i, l := range in["lengths"].([]any) {
			if got := hashing.ShaPrice(u64(l)); got != u64(want[i]) {
				return false, fmt.Sprintf("sha(%v) = %d, want %v", l, got, want[i])
			}
		}
		return true, ""
	case in["heights"] != nil:
		want := ex["pages"].([]any)
		for i, h := range in["heights"].([]any) {
			if got := vm.StackPages(u64(h)); got != u64(want[i]) {
				return false, fmt.Sprintf("stack pages(%v) = %d, want %v", h, got, want[i])
			}
		}
		return true, ""
	case in["pages"] != nil:
		for _, run := range ex["runs"].([]any) {
			ro := run.(obj)
			for p := u64(ro["pages_from"]); p <= u64(ro["pages_to"]); p++ {
				got := vm.PriceFor(uint32(p))
				if got.Load != u64(ro["load"]) || got.Store != u64(ro["store"]) || got.Word != u64(ro["word"]) ||
					got.Span != u64(ro["span"]) {
					return false, fmt.Sprintf("pages %d: %+v", p, *got)
				}
			}
		}
		return true, "every size from 0 to 65,536 pages"
	case in["root_budget"] != nil:
		return meterSteps(vec4(in["root_budget"]), ex["steps"].([]any))
	}
	return false, "unknown table"
}

// meterSteps replays a script of charges and frame openings and closings.
func meterSteps(root types.ResourceVector, steps []any) (bool, string) {
	max := types.ResourceVector{^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)}
	m := &vm.Meter{Guard: max}
	type frame struct{ limit, start types.ResourceVector }
	frames := []frame{{limit: root}}
	for i, s := range steps {
		so := s.(obj)
		top := &frames[len(frames)-1]
		switch {
		case so["charge"] != nil:
			st := m.Debit(&top.limit, vec4(so["charge"]))
			got := "ok"
			if st != vm.DebitOK {
				got = string("CSDG"[st-types.StatusExhaustedC])
			}
			if got != so["outcome"] {
				return false, fmt.Sprintf("step %d: charge %s, want %v", i, got, so["outcome"])
			}
		case so["open"] != nil:
			budget := vec4(so["open"])
			got := "opened"
			var limit types.ResourceVector
			for k := 0; k < 4; k++ {
				if budget[k] > top.limit[k]-m.Used[k] {
					got = "refused"
				}
				limit[k] = m.Used[k] + budget[k]
			}
			if got != so["outcome"] {
				return false, fmt.Sprintf("step %d: open %s, want %v", i, got, so["outcome"])
			}
			if got == "opened" {
				frames = append(frames, frame{limit: limit, start: m.Used})
			}
		case so["close"] != nil:
			var used types.ResourceVector
			for k := 0; k < 4; k++ {
				used[k] = m.Used[k] - top.start[k]
			}
			frames = frames[:len(frames)-1]
			if used != vec4(so["outcome"]) {
				return false, fmt.Sprintf("step %d: closed frame used %v, want %v", i, used, so["outcome"])
			}
		}
		if m.Used != vec4(so["root_used_after"]) {
			return false, fmt.Sprintf("step %d: root used %v, want %v", i, m.Used, so["root_used_after"])
		}
	}
	return true, ""
}

// VotingChain replays conformance/chains/voting.blocks on its own genesis,
// checking each block's active capacity and target against the manifest.
func (r *Runner) VotingChain() (Result, error) {
	res := Result{Group: "chains", Name: "voting"}
	man, err := load(filepath.Join(r.Dir, "chains", "voting.json"))
	if err != nil {
		return res, err
	}
	g, err := types.DecodeGenesisConfigHex(man["genesis"].(string))
	if err != nil {
		return res, err
	}
	raw, err := os.ReadFile(filepath.Join(r.Dir, "chains", "voting.blocks"))
	if err != nil {
		return res, err
	}
	blocks, err := seed.ParseBundle(raw)
	if err != nil {
		return res, err
	}
	c, err := chain.New(g, nil, chain.NewMemStore(), chain.DefaultConfig())
	if err != nil {
		return res, err
	}
	entries := man["blocks"].([]any)
	if len(entries) != len(blocks) {
		res.Detail = fmt.Sprintf("%d blocks, manifest lists %d", len(blocks), len(entries))
		return res, nil
	}
	changes := 0
	var last types.ResourceVector
	for i, b := range blocks {
		e, err := c.ProcessBlock(b)
		if err != nil {
			res.Detail = fmt.Sprintf("block %d: %v", i+1, err)
			return res, nil
		}
		mo := entries[i].(obj)
		if e.ID != hashOf(mo["id"]) {
			res.Detail = fmt.Sprintf("block %d id %s", i+1, e.ID)
			return res, nil
		}
		if e.Vote.Limits != vec4(mo["limits"]) {
			res.Detail = fmt.Sprintf("block %d limits %v, manifest %v", i+1, e.Vote.Limits, vec4(mo["limits"]))
			return res, nil
		}
		if e.Target != targetOf(mo["target"]) {
			res.Detail = fmt.Sprintf("block %d target %s", i+1, e.Target)
			return res, nil
		}
		if i > 0 && e.Vote.Limits != last {
			changes++
		}
		last = e.Vote.Limits
	}
	res.Pass = true
	res.Detail = fmt.Sprintf("%d blocks, epoch length %d, %d capacity changes, final limits %v",
		len(blocks), g.VoteEpochLength, changes, last)
	return res, nil
}

// Seed replays one of the published seeds with its manifest as the list.
func (r *Runner) Seed(name string, g *types.GenesisConfig) (Result, error) {
	res := Result{Group: "seed", Name: name}
	raw, err := os.ReadFile(filepath.Join(r.Dir, "seed", name+".blocks"))
	if err != nil {
		return res, err
	}
	blocks, err := seed.ParseBundle(raw)
	if err != nil {
		return res, err
	}
	lb, err := os.ReadFile(filepath.Join(r.Dir, "seed", name+".json"))
	if err != nil {
		return res, err
	}
	list, err := seed.ParseList(lb)
	if err != nil {
		return res, err
	}
	c, err := chain.New(g, list, chain.NewMemStore(), chain.DefaultConfig())
	if err != nil {
		return res, err
	}
	rep, err := seed.Replay(c, blocks, nil)
	if err != nil {
		res.Detail = err.Error()
		return res, nil
	}
	st, _ := rep.Tip.State()
	res.Pass = true
	res.Detail = fmt.Sprintf("%d blocks, %d transactions, state root %s", rep.Blocks, rep.Transactions, st.Root())
	return res, nil
}
