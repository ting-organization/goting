package vm

import (
	"encoding/binary"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/wasm"
)

// The twelve host functions (rules §22). Each runs after the dispatch price
// of the call that reached it, checks and charges in the order the rules
// give, and either continues the program, ends the invocation, or suspends
// it to run a child.

const absent = 0xffffffff

var continueRun = runResult{kind: runContinue}

// span reports whether [ptr, ptr+n) lies in the exported memory (rules §22.3).
func (m *Machine) span(ptr, n uint32) bool {
	return m.mod.HasMemory && uint64(ptr)+uint64(n) <= uint64(len(m.mem))
}

// charge debits v to the executing frame; a refusal ends the invocation.
func (m *Machine) charge(v types.ResourceVector) (runResult, bool) {
	st := m.x.meter.Debit(&m.act.limit, v)
	if st == DebitOK {
		return runResult{}, true
	}
	return exhausted(st), false
}

func (m *Machine) chargeC(c uint64) (runResult, bool) {
	st := m.x.meter.DebitC(&m.act.limit, c)
	if st == DebitOK {
		return runResult{}, true
	}
	return exhausted(st), false
}

// args pops n i32 arguments.
func (m *Machine) args(n int) (a [5]uint32) {
	m.sp -= n
	for i := 0; i < n; i++ {
		a[i] = uint32(m.stack[m.sp+i])
	}
	return a
}

// getterOut finishes a getter (rules §22.4): copy value when it fits the
// already validated output span, then return its full length.
func (m *Machine) getterOut(out, capacity uint32, value []byte) runResult {
	n := uint64(len(value))
	if n <= uint64(capacity) {
		if r, ok := m.chargeC(m.price.Bytes(n, spans(n))); !ok {
			return r
		}
		copy(m.mem[out:], value)
	}
	m.push(n)
	return continueRun
}

func (m *Machine) readKey(ptr uint32) (k [32]byte) {
	// In 64-bit arithmetic: with a full 4 GiB memory the end can be 2^32.
	copy(k[:], m.mem[uint64(ptr):uint64(ptr)+32])
	return
}

func (m *Machine) host(f wasm.HostFunc) runResult {
	x := m.x
	switch f {
	case wasm.HostInput:
		a := m.args(2)
		out, capacity := a[0], a[1]
		if !m.span(out, capacity) {
			return done(types.StatusTrap)
		}
		return m.getterOut(out, capacity, m.act.input)

	case wasm.HostContext:
		a := m.args(3)
		sel, out, capacity := a[0], a[1], a[2]
		if sel > 8 {
			return done(types.StatusTrap)
		}
		if !m.span(out, capacity) {
			return done(types.StatusTrap)
		}
		return m.getterOut(out, capacity, m.contextField(sel))

	case wasm.HostStorageGet:
		a := m.args(3)
		keyPtr, out, capacity := a[0], a[1], a[2]
		if !m.span(out, capacity) || !m.span(keyPtr, 32) {
			return done(types.StatusTrap)
		}
		if r, ok := m.chargeC(m.price.Bytes(32, 1)); !ok {
			return r
		}
		logical := m.readKey(keyPtr)
		if r, ok := m.charge(types.ResourceVector{hashing.RecordKeyPrice, 1, 0, 0}); !ok {
			return r
		}
		v, present := x.env.State.Get(types.StorageKey(m.act.contract, &logical))
		if !present {
			m.push(absent)
			return continueRun
		}
		return m.getterOut(out, capacity, v)

	case wasm.HostStorageSet:
		a := m.args(3)
		keyPtr, valPtr, valLen := a[0], a[1], a[2]
		if m.act.readOnly {
			return done(types.StatusReadOnly)
		}
		if !m.span(keyPtr, 32) || !m.span(valPtr, valLen) || valLen == 0xffffffff {
			return done(types.StatusTrap)
		}
		n := uint64(valLen)
		if r, ok := m.chargeC(m.price.Bytes(32+n, 1+spans(n))); !ok {
			return r
		}
		logical := m.readKey(keyPtr)
		value := make([]byte, n)
		copy(value, m.mem[valPtr:uint64(valPtr)+n])
		st, err := x.writeRecord(m.act, types.StorageKey(m.act.contract, &logical), value)
		if err != nil {
			return runResult{kind: runBlockFull}
		}
		if st != 0 {
			return done(st)
		}
		return continueRun

	case wasm.HostStorageDelete:
		a := m.args(1)
		keyPtr := a[0]
		if m.act.readOnly {
			return done(types.StatusReadOnly)
		}
		if !m.span(keyPtr, 32) {
			return done(types.StatusTrap)
		}
		if r, ok := m.chargeC(m.price.Bytes(32, 1)); !ok {
			return r
		}
		logical := m.readKey(keyPtr)
		st, err := x.deleteRecord(m.act, types.StorageKey(m.act.contract, &logical))
		if err != nil {
			return runResult{kind: runBlockFull}
		}
		if st != 0 {
			return done(st)
		}
		return continueRun

	case wasm.HostCodeHash:
		a := m.args(3)
		targetPtr, out, capacity := a[0], a[1], a[2]
		if !m.span(out, capacity) || !m.span(targetPtr, 32) {
			return done(types.StatusTrap)
		}
		if r, ok := m.chargeC(m.price.Bytes(32, 1)); !ok {
			return r
		}
		var target hashing.Hash = m.readKey(targetPtr)
		if r, ok := m.charge(types.ResourceVector{hashing.RecordKeyPrice, 1, 0, 0}); !ok {
			return r
		}
		mb, present := x.env.State.Get(types.ManifestKey(target))
		if !present {
			m.push(absent)
			return continueRun
		}
		if len(mb) != types.ManifestSize {
			return runResult{kind: runFault, err: ErrStateCorrupt}
		}
		return m.getterOut(out, capacity, mb[:32])

	case wasm.HostCall:
		a := m.args(5)
		targetPtr, inPtr, inLen, budgetPtr, flags := a[0], a[1], a[2], a[3], a[4]
		if flags > 1 {
			return done(types.StatusTrap)
		}
		if !m.span(targetPtr, 32) || !m.span(inPtr, inLen) || !m.span(budgetPtr, 32) {
			return done(types.StatusTrap)
		}
		if r, ok := m.chargeC(m.price.Bytes(64, 2)); !ok {
			return r
		}
		var target hashing.Hash = m.readKey(targetPtr)
		budget := m.readBudget(budgetPtr)
		m.lastResult = nil
		limit, fits := m.childLimit(budget)
		if !fits {
			m.push(types.CallError)
			return continueRun
		}
		// The child pays for receiving its input, at the caller's prices.
		n := uint64(inLen)
		switch st := x.meter.DebitC(&limit, m.price.Bytes(n, spans(n))); st {
		case DebitOK:
		case DebitBlockFull:
			return runResult{kind: runBlockFull}
		default:
			m.push(uint64(types.StatusExhaustedC))
			return continueRun
		}
		input := make([]byte, n)
		copy(input, m.mem[inPtr:uint64(inPtr)+n])
		return runResult{kind: runSuspend, req: &request{
			target: target, input: input, limit: limit, readOnly: flags == 1,
		}}

	case wasm.HostDeploy:
		a := m.args(5)
		codePtr, codeLen, inPtr, inLen, budgetPtr := a[0], a[1], a[2], a[3], a[4]
		if m.act.readOnly {
			return done(types.StatusReadOnly)
		}
		if !m.span(codePtr, codeLen) || !m.span(inPtr, inLen) || !m.span(budgetPtr, 32) {
			return done(types.StatusTrap)
		}
		if r, ok := m.chargeC(m.price.Bytes(32, 1)); !ok {
			return r
		}
		budget := m.readBudget(budgetPtr)
		m.lastResult = nil
		limit, fits := m.childLimit(budget)
		if !fits {
			m.push(types.CallError)
			return continueRun
		}
		cn, in := uint64(codeLen), uint64(inLen)
		switch st := x.meter.DebitC(&limit, m.price.Bytes(cn+in, spans(cn, in))); st {
		case DebitOK:
		case DebitBlockFull:
			return runResult{kind: runBlockFull}
		default:
			m.push(uint64(types.StatusExhaustedC))
			return continueRun
		}
		code := make([]byte, cn)
		copy(code, m.mem[codePtr:uint64(codePtr)+cn])
		input := make([]byte, in)
		copy(input, m.mem[inPtr:uint64(inPtr)+in])
		return runResult{kind: runSuspend, req: &request{
			deploy: true, code: code, input: input, limit: limit,
		}}

	case wasm.HostCallResult:
		a := m.args(2)
		out, capacity := a[0], a[1]
		if !m.span(out, capacity) {
			return done(types.StatusTrap)
		}
		return m.getterOut(out, capacity, m.lastResult)

	case wasm.HostEmit:
		a := m.args(2)
		ptr, n := a[0], uint64(a[1])
		if m.act.readOnly {
			return done(types.StatusReadOnly)
		}
		if !m.span(ptr, a[1]) {
			return done(types.StatusTrap)
		}
		if r, ok := m.charge(types.ResourceVector{m.price.Bytes(n, spans(n)), 0, types.EventFixedSize + n, 0}); !ok {
			return r
		}
		payload := make([]byte, n)
		copy(payload, m.mem[ptr:uint64(ptr)+n])
		x.events = append(x.events, types.Event{Contract: m.act.contract, Payload: payload})
		return continueRun

	case wasm.HostReturn, wasm.HostRevert:
		a := m.args(2)
		ptr, n := a[0], uint64(a[1])
		if !m.span(ptr, a[1]) {
			return done(types.StatusTrap)
		}
		if r, ok := m.charge(types.ResourceVector{m.price.Bytes(n, spans(n)), 0, n, 0}); !ok {
			return r
		}
		result := make([]byte, n)
		copy(result, m.mem[ptr:uint64(ptr)+n])
		st := uint8(types.StatusSuccess)
		if f == wasm.HostRevert {
			st = types.StatusRevert
		}
		return runResult{kind: runDone, status: st, result: result}
	}
	panic("vm: unknown host function")
}

func (m *Machine) readBudget(ptr uint32) types.ResourceVector {
	var b types.ResourceVector
	for i := 0; i < 4; i++ {
		b[i] = binary.LittleEndian.Uint64(m.mem[uint64(ptr)+uint64(8*i):])
	}
	return b
}

// childLimit converts a requested sub-budget into the child's absolute limit,
// refusing one larger, in any counter, than the caller has left.
func (m *Machine) childLimit(budget types.ResourceVector) (types.ResourceVector, bool) {
	var limit types.ResourceVector
	used := &m.x.meter.Used
	for i := 0; i < 4; i++ {
		if budget[i] > m.act.limit[i]-used[i] {
			return limit, false
		}
		limit[i] = used[i] + budget[i]
	}
	return limit, true
}

// contextField encodes a context selector's value (rules §22.8).
func (m *Machine) contextField(sel uint32) []byte {
	b := m.x.env.Block
	switch sel {
	case 0:
		return b.ChainID[:]
	case 1:
		id := m.x.txID
		return id[:]
	case 2:
		id := m.act.contract
		return id[:]
	case 3:
		out := make([]byte, 33)
		if m.act.hasCaller {
			out[0] = 1
			copy(out[1:], m.act.caller[:])
		}
		return out
	case 4:
		return binary.LittleEndian.AppendUint64(nil, b.Height)
	case 5:
		return binary.LittleEndian.AppendUint64(nil, b.Timestamp)
	case 6:
		return b.ParentID[:]
	case 7:
		return b.Producer
	case 8:
		r := m.x.meter.Remaining(&m.act.limit)
		out := make([]byte, 32)
		for i := 0; i < 4; i++ {
			binary.LittleEndian.PutUint64(out[8*i:], r[i])
		}
		return out
	}
	return nil
}
