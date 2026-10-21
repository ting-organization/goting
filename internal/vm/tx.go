package vm

import (
	"errors"
	"fmt"
	"math/bits"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/wasm"
)

// ErrBlockFull reports a debit the transaction could afford but the block's
// capacity cannot. A validator rejects the block; a miner leaves the
// transaction out. It never produces a receipt.
var ErrBlockFull = errors.New("block capacity exceeded during execution")

// BlockFullError is ErrBlockFull together with what the transaction had
// consumed when execution stopped, for a miner's accounting of discarded work.
type BlockFullError struct{ Used types.ResourceVector }

func (e *BlockFullError) Error() string   { return ErrBlockFull.Error() }
func (e *BlockFullError) Is(t error) bool { return t == ErrBlockFull }

// ErrStateCorrupt reports state that violates an invariant, such as a
// manifest without matching valid code. It is a node failure, never a guest
// outcome.
var ErrStateCorrupt = errors.New("state invariant violated")

// BlockContext is the stable block context programs can see (rules §7.3).
type BlockContext struct {
	ChainID   hashing.Hash
	Height    uint64
	Timestamp uint64
	ParentID  hashing.Hash
	Producer  []byte
}

// Env is what transactions of one block execute against.
type Env struct {
	Block *BlockContext
	State *state.Overlay
	Codes *CodeCache
}

// TxExec is one transaction occurrence's execution.
type TxExec struct {
	env      *Env
	txID     hashing.Hash
	meter    Meter
	ordinal  uint64
	prepared map[hashing.Hash]struct{}
	events   []types.Event
}

// activation is a budget frame running a program: a call, or a deployment
// and its initializer.
type activation struct {
	limit     types.ResourceVector // absolute bound on meter.Used
	contract  hashing.Hash
	caller    hashing.Hash
	hasCaller bool
	readOnly  bool
	input     []byte
	stateCP   int
	eventCP   int
	m         *Machine
	deploying bool // a deployment: produce the identifier after the initializer
}

// request is a child a program asked for, after the host's own checks and
// the child's receive debit.
type request struct {
	deploy   bool
	target   hashing.Hash
	code     []byte
	input    []byte
	limit    types.ResourceVector
	readOnly bool
}

type outcome struct {
	status uint8
	result []byte
}

// ExecuteTransaction runs one transaction occurrence (rules §26.2) against
// env.State. guard is the block capacity left for it, as an absolute bound
// on its consumption. On ErrBlockFull the state is left as before the
// transaction.
func ExecuteTransaction(env *Env, guard types.ResourceVector, raw []byte, tx *types.Transaction) (*types.Receipt, error) {
	x := &TxExec{
		env:      env,
		txID:     types.TransactionID(env.Block.ChainID, raw),
		meter:    Meter{Guard: guard},
		prepared: make(map[hashing.Hash]struct{}),
	}
	cp := env.State.Checkpoint()
	root := tx.Budget
	var o outcome
	st := x.meter.Debit(&root, types.ResourceVector{0, 0, types.DataBase(len(raw)), 0})
	if st == DebitOK {
		st = x.meter.Debit(&root, types.ResourceVector{RootDispatch, 0, 0, 0})
	}
	switch st {
	case DebitBlockFull:
		env.State.Revert(cp)
		return nil, &BlockFullError{Used: x.meter.Used}
	case DebitOK:
		act := &activation{limit: root, stateCP: cp}
		req := &request{deploy: tx.Kind == types.KindDeploy, target: tx.Target, code: tx.Code, input: tx.Input}
		var err error
		o, err = x.execute(act, req)
		if err != nil {
			env.State.Revert(cp)
			if errors.Is(err, ErrBlockFull) {
				return nil, &BlockFullError{Used: x.meter.Used}
			}
			return nil, err
		}
	default:
		o = outcome{status: st}
	}
	if o.status != types.StatusSuccess {
		env.State.Revert(cp)
		x.events = nil
	}
	return &types.Receipt{
		TransactionID: x.txID,
		Status:        o.status,
		ResourcesUsed: x.meter.Used,
		Result:        o.result,
		Events:        x.events,
	}, nil
}

// execute drives the top-level request and every nested invocation with an
// explicit stack, so nesting never uses the native stack (rules §20.4, §24.4).
func (x *TxExec) execute(top *activation, req *request) (outcome, error) {
	var stack []*activation
	o, act, err := x.start(top, req)
	for {
		if err != nil {
			return outcome{}, err
		}
		if act != nil {
			stack = append(stack, act)
		} else if len(stack) == 0 {
			return o, nil
		} else {
			stack[len(stack)-1].m.deliver(o)
		}
		cur := stack[len(stack)-1]
		r := cur.m.run()
		switch r.kind {
		case runDone:
			stack = stack[:len(stack)-1]
			cur.m = nil // the instance disappears with the invocation
			o, err = x.finish(cur, r.status, r.result)
			act = nil
		case runSuspend:
			child := &activation{
				limit:     r.req.limit,
				caller:    cur.contract,
				hasCaller: true,
				readOnly:  !r.req.deploy && (r.req.readOnly || cur.readOnly),
				stateCP:   x.env.State.Checkpoint(),
				eventCP:   len(x.events),
			}
			o, act, err = x.start(child, r.req)
		case runBlockFull:
			return outcome{}, ErrBlockFull
		case runFault:
			return outcome{}, r.err
		default:
			return outcome{}, fmt.Errorf("vm: unexpected run result %d", r.kind)
		}
	}
}

func (x *TxExec) rollback(act *activation) {
	x.env.State.Revert(act.stateCP)
	x.events = x.events[:act.eventCP]
}

// debit charges v to act's frame. It returns the failure status (3–6), 0 on
// success, and ErrBlockFull when the block cannot afford it.
func (x *TxExec) debit(act *activation, v types.ResourceVector) (uint8, error) {
	st := x.meter.Debit(&act.limit, v)
	if st == DebitBlockFull {
		return 0, ErrBlockFull
	}
	return st, nil
}

// start begins a call or deployment. It either finishes at once, returning
// its outcome with a nil activation, or returns the activation to run.
func (x *TxExec) start(act *activation, req *request) (outcome, *activation, error) {
	if req.deploy {
		return x.startDeploy(act, req.code, req.input)
	}
	act.contract = req.target
	act.input = req.input
	return x.startInvoke(act)
}

// startInvoke performs steps 1–3 of an invocation (rules §23.2): the code
// reads and price, the instance preparation and the entry reservation.
func (x *TxExec) startInvoke(act *activation) (outcome, *activation, error) {
	if st, err := x.debit(act, types.ResourceVector{hashing.RecordKeyPrice, 1, 0, 0}); st != 0 || err != nil {
		return outcome{status: st}, nil, err
	}
	mb, ok := x.env.State.Get(types.ManifestKey(act.contract))
	if !ok {
		return outcome{status: types.StatusTargetAbsent}, nil, nil
	}
	man, err := types.DecodeManifest(mb)
	if err != nil {
		return outcome{}, nil, fmt.Errorf("%w: manifest of %s: %v", ErrStateCorrupt, act.contract, err)
	}
	if st, err := x.debit(act, types.ResourceVector{hashing.RecordKeyPrice, 1, 0, 0}); st != 0 || err != nil {
		return outcome{status: st}, nil, err
	}
	// Unless this transaction has prepared the code, its price, which the
	// manifest records, is paid before any of its bytes are read (§23.2);
	// the bytes are read only when the node has no decoded copy.
	if _, ok := x.prepared[man.CodeHash]; !ok {
		if st, err := x.debit(act, types.ResourceVector{man.CodePrice, 0, 0, 0}); st != 0 || err != nil {
			return outcome{status: st}, nil, err
		}
		x.prepared[man.CodeHash] = struct{}{}
	}
	mod := x.env.Codes.Get(man.CodeHash)
	if mod == nil {
		code, ok := x.env.State.Get(types.CodeKey(act.contract))
		if !ok || uint64(len(code)) != uint64(man.CodeLength) || wasm.CodePrice(code) != man.CodePrice ||
			types.CodeHash(code) != man.CodeHash {
			return outcome{}, nil, fmt.Errorf("%w: code record of %s", ErrStateCorrupt, act.contract)
		}
		mod, err = x.env.Codes.Validate(man.CodeHash, code)
		if mod == nil {
			return outcome{}, nil, fmt.Errorf("%w: installed code of %s is invalid: %v", ErrStateCorrupt, act.contract, err)
		}
	}
	if st, err := x.debit(act, types.ResourceVector{preparationPrice(mod), 0, 0, 0}); st != 0 || err != nil {
		return outcome{status: st}, nil, err
	}
	m := newMachine(x, act, mod)
	act.m = m
	if st := m.enter(mod.Funcs[mod.Invoke]); st != DebitOK {
		act.m = nil
		if st == DebitBlockFull {
			return outcome{}, nil, ErrBlockFull
		}
		return outcome{status: st}, nil, nil
	}
	return outcome{}, act, nil
}

func satAdd(a, b uint64) uint64 {
	s, c := bits.Add64(a, b, 0)
	if c != 0 {
		return ^uint64(0)
	}
	return s
}

// startDeploy performs a deployment's steps up to its initializer (rules
// §23.1), then starts the initializer in the deployment's own frame.
func (x *TxExec) startDeploy(act *activation, code, input []byte) (outcome, *activation, error) {
	if x.ordinal == ^uint64(0) {
		return outcome{status: types.StatusOrdinalOverflow}, nil, nil
	}
	ord := x.ordinal
	x.ordinal++
	if st, err := x.debit(act, types.ResourceVector{hashing.ContractIDPrice, 0, 0, 0}); st != 0 || err != nil {
		return outcome{status: st}, nil, err
	}
	id := types.ContractID(x.env.Block.ChainID, x.txID, ord)
	if st, err := x.debit(act, types.ResourceVector{hashing.RecordKeyPrice, 1, 0, 0}); st != 0 || err != nil {
		return outcome{status: st}, nil, err
	}
	if _, exists := x.env.State.Get(types.ManifestKey(id)); exists {
		return outcome{status: types.StatusIDExists}, nil, nil
	}
	codePrice := wasm.CodePrice(code)
	if st, err := x.debit(act, types.ResourceVector{satAdd(codePrice, hashing.CodeHashPrice(uint64(len(code)))), 0, 0, 0}); st != 0 || err != nil {
		return outcome{status: st}, nil, err
	}
	codeHash := types.CodeHash(code)
	mod, _ := x.env.Codes.Validate(codeHash, code)
	if mod == nil {
		return outcome{status: types.StatusModuleInvalid}, nil, nil
	}
	x.prepared[codeHash] = struct{}{}

	act.stateCP = x.env.State.Checkpoint()
	act.eventCP = len(x.events)
	man := types.Manifest{CodeHash: codeHash, CodeLength: uint32(len(code)), CodePrice: codePrice}
	if st, err := x.writeRecord(act, types.ManifestKey(id), man.Encode()); st != 0 || err != nil {
		x.rollback(act)
		return outcome{status: st}, nil, err
	}
	if st, err := x.writeRecord(act, types.CodeKey(id), code); st != 0 || err != nil {
		x.rollback(act)
		return outcome{status: st}, nil, err
	}
	act.contract = id
	act.input = input
	act.deploying = true
	o, a, err := x.startInvoke(act)
	if a == nil {
		x.rollback(act)
		o.result = nil
	}
	return o, a, err
}

// writeRecord follows the storage-write rule (rules §22.5 steps 3–5): the
// key and the read of the old record, then one atomic debit of the leaf
// hash, the write's access and its growth, then the write.
func (x *TxExec) writeRecord(act *activation, key hashing.Hash, value []byte) (uint8, error) {
	if st, err := x.debit(act, types.ResourceVector{hashing.RecordKeyPrice, 1, 0, 0}); st != 0 || err != nil {
		return st, err
	}
	old, present := x.env.State.Get(key)
	n := uint64(len(value))
	var growth uint64
	if !present {
		growth = 36 + n
	} else if n > uint64(len(old)) {
		growth = n - uint64(len(old))
	}
	if st, err := x.debit(act, types.ResourceVector{hashing.LeafPrice(n), 1, 0, growth}); st != 0 || err != nil {
		return st, err
	}
	x.env.State.Set(key, value)
	return 0, nil
}

// deleteRecord is storage_delete's steps 3–5.
func (x *TxExec) deleteRecord(act *activation, key hashing.Hash) (uint8, error) {
	if st, err := x.debit(act, types.ResourceVector{hashing.RecordKeyPrice, 1, 0, 0}); st != 0 || err != nil {
		return st, err
	}
	x.env.State.Get(key) // only the old record's length is needed
	if st, err := x.debit(act, types.ResourceVector{0, 1, 0, 0}); st != 0 || err != nil {
		return st, err
	}
	x.env.State.Delete(key)
	return 0, nil
}

// finish ends an activation whose program stopped with status and result.
func (x *TxExec) finish(act *activation, status uint8, result []byte) (outcome, error) {
	if act.deploying {
		if status == types.StatusSuccess {
			st, err := x.debit(act, types.ResourceVector{32, 0, 32, 0})
			if err != nil {
				return outcome{}, err
			}
			if st != 0 {
				x.rollback(act)
				return outcome{status: st}, nil
			}
			id := act.contract
			return outcome{status: types.StatusSuccess, result: id[:]}, nil
		}
		x.rollback(act)
		return outcome{status: status, result: result}, nil
	}
	if status != types.StatusSuccess {
		x.rollback(act)
	}
	return outcome{status: status, result: result}, nil
}
