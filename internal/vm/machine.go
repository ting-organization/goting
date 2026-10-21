package vm

import (
	"encoding/binary"
	"math/bits"

	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/wasm"
)

// runKind says why Machine.run returned.
type runKind uint8

const (
	runContinue  runKind = iota // a host call finished; keep running
	runDone                     // the invocation ended with status and result
	runSuspend                  // a host call or deploy needs a child to run
	runBlockFull                // a debit would exceed the block's capacity
	runFault                    // a node failure, never a guest outcome
)

type runResult struct {
	kind   runKind
	status uint8
	result []byte
	req    *request
	err    error
}

type callFrame struct {
	fn *wasm.Function
	pc int
	fp int
}

// Machine is one invocation's instance and its interpreter state. It is
// resumable: a host call that runs a child saves the state and returns to
// the driver, which later delivers the child's status and resumes it.
type Machine struct {
	x   *TxExec
	act *activation
	mod *wasm.Module

	mem   []byte
	pages uint32
	price *MemPrice

	globals []uint64
	stack   []uint64
	sp      int
	frames  []callFrame
	fn      *wasm.Function
	pc      int
	fp      int

	height, mark uint64

	lastResult []byte
}

// PreparationPrice is the one atomic preparation debit (rules §15.6).
func PreparationPrice(mod *wasm.Module) uint64 { return preparationPrice(mod) }

// preparationPrice is the one atomic preparation debit (rules §15.6).
func preparationPrice(mod *wasm.Module) uint64 {
	init := PriceFor(mod.MemMin)
	return StartPrice +
		PagePrice*uint64(mod.MemMin) +
		GlobalPrice*uint64(len(mod.Globals)) +
		TableEntryPrice*uint64(mod.TableSize) +
		DataSegPrice*uint64(len(mod.Data)) +
		init.Bytes(mod.DataBytes, mod.DataSpans)
}

// newMachine builds a fresh instance: globals set, memory zeroed and
// overlaid with the data segments in order. The table is shared: it never
// changes after instantiation.
func newMachine(x *TxExec, act *activation, mod *wasm.Module) *Machine {
	m := &Machine{x: x, act: act, mod: mod}
	if mod.HasMemory {
		m.mem = getMemory(uint64(mod.MemMin) * wasm.PageSize)
		for _, d := range mod.Data {
			copy(m.mem[d.Offset:], d.Bytes)
		}
	}
	m.pages = mod.MemMin
	m.price = PriceFor(m.pages)
	if len(mod.Globals) > 0 {
		m.globals = make([]uint64, len(mod.Globals))
		for i := range mod.Globals {
			m.globals[i] = mod.Globals[i].Init
		}
	}
	m.stack = make([]uint64, 64)
	return m
}

// effC is the compute limit that applies inside this invocation.
func (m *Machine) effC() uint64 {
	l := m.act.limit[types.C]
	if g := m.x.meter.Guard[types.C]; g < l {
		return g
	}
	return l
}

// ensureStack grows the value stack to hold n slots.
func (m *Machine) ensureStack(n int) {
	if n <= len(m.stack) {
		return
	}
	c := 2 * len(m.stack)
	if c < n {
		c = n
	}
	s := make([]uint64, c)
	copy(s, m.stack[:m.sp])
	m.stack = s
}

// enter debits a defined function's reservation and stack pages and opens
// its frame (rules §21.4). The callee's parameters are the top operands.
func (m *Machine) enter(fn *wasm.Function) uint8 {
	h := m.height + fn.Reservation + FrameRecord
	debit := fn.Reservation
	if h > m.mark {
		debit += PagePrice * (StackPages(h) - StackPages(m.mark))
	}
	if st := m.x.meter.DebitC(&m.act.limit, debit); st != DebitOK {
		return st
	}
	if h > m.mark {
		m.mark = h
	}
	m.height = h
	if m.fn != nil {
		m.frames = append(m.frames, callFrame{fn: m.fn, pc: m.pc, fp: m.fp})
	}
	fp := m.sp - int(fn.NumParams)
	m.ensureStack(fp + int(fn.Reservation))
	nl := int(fn.NumLocals)
	for i := m.sp; i < m.sp+nl; i++ {
		m.stack[i] = 0
	}
	m.sp += nl
	m.fp = fp
	m.fn = fn
	m.pc = 0
	return DebitOK
}

func (m *Machine) push(v uint64) {
	m.ensureStack(m.sp + 1)
	m.stack[m.sp] = v
	m.sp++
}

// deliver resumes the machine after a child: the child's result fills the
// last-result buffer and its status is the host call's result.
func (m *Machine) deliver(o outcome) {
	m.lastResult = o.result
	m.push(uint64(o.status))
}

func done(status uint8) runResult { return runResult{kind: runDone, status: status} }

// exhausted turns a failed debit into the run result.
func exhausted(st uint8) runResult {
	if st == DebitBlockFull {
		return runResult{kind: runBlockFull}
	}
	return done(st)
}

// grow implements memory.grow after its dispatch (rules §21.5).
func (m *Machine) grow(delta uint32) (uint32, uint8) {
	old := m.pages
	if uint64(old)+uint64(delta) > uint64(m.mod.MemMax) {
		return 0xffffffff, DebitOK
	}
	if delta == 0 {
		return old, DebitOK
	}
	if st := m.x.meter.DebitC(&m.act.limit, PagePrice*uint64(delta)); st != DebitOK {
		return 0, st
	}
	m.mem = growMemory(m.mem, uint64(old+delta)*wasm.PageSize)
	m.pages = old + delta
	m.price = PriceFor(m.pages)
	return old, DebitOK
}

// run executes until the invocation ends, needs a child, or the block is full.
//
// The hot registers live in locals; every exit or call-out writes them back
// first ("sync") and reloads them afterwards.
func (m *Machine) run() runResult {
	meter := &m.x.meter
	effC := m.effC()
	gas := effC - meter.Used[types.C]
	code := m.fn.Code
	stack := m.stack
	sp := m.sp
	pc := m.pc
	fp := m.fp
	opBase := fp + int(m.fn.NumParams+m.fn.NumLocals)
	mem := m.mem
	price := m.price
	var need uint64 // the price that did not fit, for oog
	var r runResult
	var st uint8

	for {
		in := &code[pc]
		pc++
		if p := uint64(in.Price); p > 0 {
			if p > gas {
				need = p
				goto oog
			}
			gas -= p
		}
		switch in.Op {
		case wasm.OpUnreachable:
			goto trap
		case wasm.OpNop, wasm.OpBlock, wasm.OpLoop:
		case wasm.OpIf:
			sp--
			if uint32(stack[sp]) == 0 {
				pc = int(in.A)
			}
		case wasm.OpJump:
			pc = int(in.A)
		case wasm.OpBr:
			dst := opBase + int(in.B)
			if in.Imm != 0 {
				stack[dst] = stack[sp-1]
			}
			sp = dst + int(in.Imm)
			pc = int(in.A)
		case wasm.OpBrIf:
			sp--
			if uint32(stack[sp]) != 0 {
				dst := opBase + int(in.B)
				if in.Imm != 0 {
					stack[dst] = stack[sp-1]
				}
				sp = dst + int(in.Imm)
				pc = int(in.A)
			}
		case wasm.OpBrTable:
			sp--
			idx := uint32(stack[sp])
			if idx >= in.B-1 {
				idx = in.B - 1
			}
			t := &m.fn.Tables[in.A+idx]
			dst := opBase + int(t.Height)
			if t.Arity != 0 {
				stack[dst] = stack[sp-1]
			}
			sp = dst + int(t.Arity)
			pc = int(t.PC)
		case wasm.OpReturn, wasm.OpEndFunc:
			if in.Imm != 0 {
				stack[fp] = stack[sp-1]
			}
			sp = fp + int(in.Imm)
			m.height -= m.fn.Reservation + FrameRecord
			if len(m.frames) == 0 {
				meter.Used[types.C] = effC - gas
				m.sp, m.pc, m.fp = sp, pc, fp
				return done(types.StatusSuccess)
			}
			f := m.frames[len(m.frames)-1]
			m.frames = m.frames[:len(m.frames)-1]
			m.fn = f.fn
			code = f.fn.Code
			pc = f.pc
			fp = f.fp
			opBase = fp + int(m.fn.NumParams+m.fn.NumLocals)
		case wasm.OpCall:
			meter.Used[types.C] = effC - gas
			m.sp, m.pc, m.fp = sp, pc, fp
			if st = m.enter(m.mod.Funcs[in.A]); st != DebitOK {
				return exhausted(st)
			}
			gas = effC - meter.Used[types.C]
			stack = m.stack
			sp, pc, fp = m.sp, m.pc, m.fp
			code = m.fn.Code
			opBase = fp + int(m.fn.NumParams+m.fn.NumLocals)
			mem, price = m.mem, m.price

		case wasm.OpCallIndirect:
			sp--
			idx := uint32(stack[sp])
			if uint64(idx) >= uint64(len(m.mod.Table)) {
				goto trap
			}
			e := &m.mod.Table[idx]
			if e.Func < 0 || e.Canon != in.A {
				goto trap
			}
			nimp := int32(len(m.mod.Imports))
			if e.Func < nimp {
				if HostCallPrice > gas {
					need = HostCallPrice
					goto oog
				}
				gas -= HostCallPrice
				meter.Used[types.C] = effC - gas
				m.sp, m.pc, m.fp = sp, pc, fp
				if r = m.host(m.mod.Imports[e.Func]); r.kind != runContinue {
					return r
				}
				gas = effC - meter.Used[types.C]
				stack = m.stack
				sp, pc, fp = m.sp, m.pc, m.fp
				code = m.fn.Code
				opBase = fp + int(m.fn.NumParams+m.fn.NumLocals)
				mem, price = m.mem, m.price
				continue
			}
			meter.Used[types.C] = effC - gas
			m.sp, m.pc, m.fp = sp, pc, fp
			if st = m.enter(m.mod.Funcs[e.Func-nimp]); st != DebitOK {
				return exhausted(st)
			}
			gas = effC - meter.Used[types.C]
			stack = m.stack
			sp, pc, fp = m.sp, m.pc, m.fp
			code = m.fn.Code
			opBase = fp + int(m.fn.NumParams+m.fn.NumLocals)
			mem, price = m.mem, m.price

		case wasm.OpCallHost:
			meter.Used[types.C] = effC - gas
			m.sp, m.pc, m.fp = sp, pc, fp
			if r = m.host(wasm.HostFunc(in.A)); r.kind != runContinue {
				return r
			}
			gas = effC - meter.Used[types.C]
			stack = m.stack
			sp, pc, fp = m.sp, m.pc, m.fp
			code = m.fn.Code
			opBase = fp + int(m.fn.NumParams+m.fn.NumLocals)
			mem, price = m.mem, m.price

		case wasm.OpDrop:
			sp--
		case wasm.OpSelect:
			sp -= 2
			if uint32(stack[sp+1]) == 0 {
				stack[sp-1] = stack[sp]
			}
		case wasm.OpLocalGet:
			stack[sp] = stack[fp+int(in.A)]
			sp++
		case wasm.OpLocalSet:
			sp--
			stack[fp+int(in.A)] = stack[sp]
		case wasm.OpLocalTee:
			stack[fp+int(in.A)] = stack[sp-1]
		case wasm.OpGlobalGet:
			stack[sp] = m.globals[in.A]
			sp++
		case wasm.OpGlobalSet:
			sp--
			m.globals[in.A] = stack[sp]

		case wasm.OpI32Load, wasm.OpI64Load, wasm.OpI32Load8S, wasm.OpI32Load8U,
			wasm.OpI32Load16S, wasm.OpI32Load16U, wasm.OpI64Load8S, wasm.OpI64Load8U,
			wasm.OpI64Load16S, wasm.OpI64Load16U, wasm.OpI64Load32S, wasm.OpI64Load32U:
			if price.Load > gas {
				need = price.Load
				goto oog
			}
			gas -= price.Load
			ea := uint64(uint32(stack[sp-1])) + uint64(in.A)
			var v uint64
			switch in.Op {
			case wasm.OpI32Load:
				if ea+4 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(binary.LittleEndian.Uint32(mem[ea:]))
			case wasm.OpI64Load:
				if ea+8 > uint64(len(mem)) {
					goto trap
				}
				v = binary.LittleEndian.Uint64(mem[ea:])
			case wasm.OpI32Load8S:
				if ea+1 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(uint32(int32(int8(mem[ea]))))
			case wasm.OpI32Load8U, wasm.OpI64Load8U:
				if ea+1 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(mem[ea])
			case wasm.OpI32Load16S:
				if ea+2 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(uint32(int32(int16(binary.LittleEndian.Uint16(mem[ea:])))))
			case wasm.OpI32Load16U, wasm.OpI64Load16U:
				if ea+2 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(binary.LittleEndian.Uint16(mem[ea:]))
			case wasm.OpI64Load8S:
				if ea+1 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(int64(int8(mem[ea])))
			case wasm.OpI64Load16S:
				if ea+2 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(int64(int16(binary.LittleEndian.Uint16(mem[ea:]))))
			case wasm.OpI64Load32S:
				if ea+4 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(int64(int32(binary.LittleEndian.Uint32(mem[ea:]))))
			case wasm.OpI64Load32U:
				if ea+4 > uint64(len(mem)) {
					goto trap
				}
				v = uint64(binary.LittleEndian.Uint32(mem[ea:]))
			}
			stack[sp-1] = v

		case wasm.OpI32Store, wasm.OpI64Store, wasm.OpI32Store8, wasm.OpI32Store16,
			wasm.OpI64Store8, wasm.OpI64Store16, wasm.OpI64Store32:
			if price.Store > gas {
				need = price.Store
				goto oog
			}
			gas -= price.Store
			sp -= 2
			ea := uint64(uint32(stack[sp])) + uint64(in.A)
			v := stack[sp+1]
			switch in.Op {
			case wasm.OpI32Store, wasm.OpI64Store32:
				if ea+4 > uint64(len(mem)) {
					goto trap
				}
				binary.LittleEndian.PutUint32(mem[ea:], uint32(v))
			case wasm.OpI64Store:
				if ea+8 > uint64(len(mem)) {
					goto trap
				}
				binary.LittleEndian.PutUint64(mem[ea:], v)
			case wasm.OpI32Store8, wasm.OpI64Store8:
				if ea+1 > uint64(len(mem)) {
					goto trap
				}
				mem[ea] = byte(v)
			case wasm.OpI32Store16, wasm.OpI64Store16:
				if ea+2 > uint64(len(mem)) {
					goto trap
				}
				binary.LittleEndian.PutUint16(mem[ea:], uint16(v))
			}

		case wasm.OpMemorySize:
			stack[sp] = uint64(m.pages)
			sp++
		case wasm.OpMemoryGrow:
			meter.Used[types.C] = effC - gas
			res, st := m.grow(uint32(stack[sp-1]))
			if st != DebitOK {
				m.sp, m.pc, m.fp = sp, pc, fp
				return exhausted(st)
			}
			gas = effC - meter.Used[types.C]
			stack[sp-1] = uint64(res)
			mem, price = m.mem, m.price
		case wasm.OpMemoryCopy:
			sp -= 3
			dst, src, n := uint64(uint32(stack[sp])), uint64(uint32(stack[sp+1])), uint64(uint32(stack[sp+2]))
			var c uint64
			if n != 0 {
				c = price.Bytes(n, 2)
			}
			if c > gas {
				need = c
				goto oog
			}
			gas -= c
			if src+n > uint64(len(mem)) || dst+n > uint64(len(mem)) {
				goto trap
			}
			copy(mem[dst:dst+n], mem[src:src+n])
		case wasm.OpMemoryFill:
			sp -= 3
			dst, val, n := uint64(uint32(stack[sp])), byte(stack[sp+1]), uint64(uint32(stack[sp+2]))
			var c uint64
			if n != 0 {
				c = price.Bytes(n, 1)
			}
			if c > gas {
				need = c
				goto oog
			}
			gas -= c
			if dst+n > uint64(len(mem)) {
				goto trap
			}
			fill(mem[dst:dst+n], val)

		case wasm.OpI32Const, wasm.OpI64Const:
			stack[sp] = in.Imm
			sp++

		case wasm.OpI32Eqz:
			stack[sp-1] = b2u(uint32(stack[sp-1]) == 0)
		case wasm.OpI64Eqz:
			stack[sp-1] = b2u(stack[sp-1] == 0)
		case wasm.OpI32Eq, wasm.OpI32Ne, wasm.OpI32LtS, wasm.OpI32LtU, wasm.OpI32GtS, wasm.OpI32GtU,
			wasm.OpI32LeS, wasm.OpI32LeU, wasm.OpI32GeS, wasm.OpI32GeU:
			sp--
			a, b := uint32(stack[sp-1]), uint32(stack[sp])
			var r bool
			switch in.Op {
			case wasm.OpI32Eq:
				r = a == b
			case wasm.OpI32Ne:
				r = a != b
			case wasm.OpI32LtS:
				r = int32(a) < int32(b)
			case wasm.OpI32LtU:
				r = a < b
			case wasm.OpI32GtS:
				r = int32(a) > int32(b)
			case wasm.OpI32GtU:
				r = a > b
			case wasm.OpI32LeS:
				r = int32(a) <= int32(b)
			case wasm.OpI32LeU:
				r = a <= b
			case wasm.OpI32GeS:
				r = int32(a) >= int32(b)
			case wasm.OpI32GeU:
				r = a >= b
			}
			stack[sp-1] = b2u(r)
		case wasm.OpI64Eq, wasm.OpI64Ne, wasm.OpI64LtS, wasm.OpI64LtU, wasm.OpI64GtS, wasm.OpI64GtU,
			wasm.OpI64LeS, wasm.OpI64LeU, wasm.OpI64GeS, wasm.OpI64GeU:
			sp--
			a, b := stack[sp-1], stack[sp]
			var r bool
			switch in.Op {
			case wasm.OpI64Eq:
				r = a == b
			case wasm.OpI64Ne:
				r = a != b
			case wasm.OpI64LtS:
				r = int64(a) < int64(b)
			case wasm.OpI64LtU:
				r = a < b
			case wasm.OpI64GtS:
				r = int64(a) > int64(b)
			case wasm.OpI64GtU:
				r = a > b
			case wasm.OpI64LeS:
				r = int64(a) <= int64(b)
			case wasm.OpI64LeU:
				r = a <= b
			case wasm.OpI64GeS:
				r = int64(a) >= int64(b)
			case wasm.OpI64GeU:
				r = a >= b
			}
			stack[sp-1] = b2u(r)

		case wasm.OpI32Clz:
			stack[sp-1] = uint64(bits.LeadingZeros32(uint32(stack[sp-1])))
		case wasm.OpI32Ctz:
			stack[sp-1] = uint64(bits.TrailingZeros32(uint32(stack[sp-1])))
		case wasm.OpI32Popcnt:
			stack[sp-1] = uint64(bits.OnesCount32(uint32(stack[sp-1])))
		case wasm.OpI32Add:
			sp--
			stack[sp-1] = uint64(uint32(stack[sp-1]) + uint32(stack[sp]))
		case wasm.OpI32Sub:
			sp--
			stack[sp-1] = uint64(uint32(stack[sp-1]) - uint32(stack[sp]))
		case wasm.OpI32Mul:
			sp--
			stack[sp-1] = uint64(uint32(stack[sp-1]) * uint32(stack[sp]))
		case wasm.OpI32DivS:
			sp--
			a, b := int32(stack[sp-1]), int32(stack[sp])
			if b == 0 || (a == -1<<31 && b == -1) {
				goto trap
			}
			stack[sp-1] = uint64(uint32(a / b))
		case wasm.OpI32DivU:
			sp--
			a, b := uint32(stack[sp-1]), uint32(stack[sp])
			if b == 0 {
				goto trap
			}
			stack[sp-1] = uint64(a / b)
		case wasm.OpI32RemS:
			sp--
			a, b := int32(stack[sp-1]), int32(stack[sp])
			if b == 0 {
				goto trap
			}
			if b == -1 {
				stack[sp-1] = 0
			} else {
				stack[sp-1] = uint64(uint32(a % b))
			}
		case wasm.OpI32RemU:
			sp--
			a, b := uint32(stack[sp-1]), uint32(stack[sp])
			if b == 0 {
				goto trap
			}
			stack[sp-1] = uint64(a % b)
		case wasm.OpI32And:
			sp--
			stack[sp-1] = uint64(uint32(stack[sp-1]) & uint32(stack[sp]))
		case wasm.OpI32Or:
			sp--
			stack[sp-1] = uint64(uint32(stack[sp-1]) | uint32(stack[sp]))
		case wasm.OpI32Xor:
			sp--
			stack[sp-1] = uint64(uint32(stack[sp-1]) ^ uint32(stack[sp]))
		case wasm.OpI32Shl:
			sp--
			stack[sp-1] = uint64(uint32(stack[sp-1]) << (uint32(stack[sp]) & 31))
		case wasm.OpI32ShrS:
			sp--
			stack[sp-1] = uint64(uint32(int32(stack[sp-1]) >> (uint32(stack[sp]) & 31)))
		case wasm.OpI32ShrU:
			sp--
			stack[sp-1] = uint64(uint32(stack[sp-1]) >> (uint32(stack[sp]) & 31))
		case wasm.OpI32Rotl:
			sp--
			stack[sp-1] = uint64(bits.RotateLeft32(uint32(stack[sp-1]), int(uint32(stack[sp])&31)))
		case wasm.OpI32Rotr:
			sp--
			stack[sp-1] = uint64(bits.RotateLeft32(uint32(stack[sp-1]), -int(uint32(stack[sp])&31)))

		case wasm.OpI64Clz:
			stack[sp-1] = uint64(bits.LeadingZeros64(stack[sp-1]))
		case wasm.OpI64Ctz:
			stack[sp-1] = uint64(bits.TrailingZeros64(stack[sp-1]))
		case wasm.OpI64Popcnt:
			stack[sp-1] = uint64(bits.OnesCount64(stack[sp-1]))
		case wasm.OpI64Add:
			sp--
			stack[sp-1] += stack[sp]
		case wasm.OpI64Sub:
			sp--
			stack[sp-1] -= stack[sp]
		case wasm.OpI64Mul:
			sp--
			stack[sp-1] *= stack[sp]
		case wasm.OpI64DivS:
			sp--
			a, b := int64(stack[sp-1]), int64(stack[sp])
			if b == 0 || (a == -1<<63 && b == -1) {
				goto trap
			}
			stack[sp-1] = uint64(a / b)
		case wasm.OpI64DivU:
			sp--
			if stack[sp] == 0 {
				goto trap
			}
			stack[sp-1] /= stack[sp]
		case wasm.OpI64RemS:
			sp--
			a, b := int64(stack[sp-1]), int64(stack[sp])
			if b == 0 {
				goto trap
			}
			if b == -1 {
				stack[sp-1] = 0
			} else {
				stack[sp-1] = uint64(a % b)
			}
		case wasm.OpI64RemU:
			sp--
			if stack[sp] == 0 {
				goto trap
			}
			stack[sp-1] %= stack[sp]
		case wasm.OpI64And:
			sp--
			stack[sp-1] &= stack[sp]
		case wasm.OpI64Or:
			sp--
			stack[sp-1] |= stack[sp]
		case wasm.OpI64Xor:
			sp--
			stack[sp-1] ^= stack[sp]
		case wasm.OpI64Shl:
			sp--
			stack[sp-1] <<= stack[sp] & 63
		case wasm.OpI64ShrS:
			sp--
			stack[sp-1] = uint64(int64(stack[sp-1]) >> (stack[sp] & 63))
		case wasm.OpI64ShrU:
			sp--
			stack[sp-1] >>= stack[sp] & 63
		case wasm.OpI64Rotl:
			sp--
			stack[sp-1] = bits.RotateLeft64(stack[sp-1], int(stack[sp]&63))
		case wasm.OpI64Rotr:
			sp--
			stack[sp-1] = bits.RotateLeft64(stack[sp-1], -int(stack[sp]&63))

		case wasm.OpI32WrapI64:
			stack[sp-1] = uint64(uint32(stack[sp-1]))
		case wasm.OpI64ExtendI32S:
			stack[sp-1] = uint64(int64(int32(stack[sp-1])))
		case wasm.OpI64ExtendI32U:
			stack[sp-1] = uint64(uint32(stack[sp-1]))
		case wasm.OpI32Extend8S:
			stack[sp-1] = uint64(uint32(int32(int8(stack[sp-1]))))
		case wasm.OpI32Extend16S:
			stack[sp-1] = uint64(uint32(int32(int16(stack[sp-1]))))
		case wasm.OpI64Extend8S:
			stack[sp-1] = uint64(int64(int8(stack[sp-1])))
		case wasm.OpI64Extend16S:
			stack[sp-1] = uint64(int64(int16(stack[sp-1])))
		case wasm.OpI64Extend32S:
			stack[sp-1] = uint64(int64(int32(stack[sp-1])))

		case wasm.OpI64Add128:
			sp -= 2
			ll, lh, rl, rh := stack[sp-2], stack[sp-1], stack[sp], stack[sp+1]
			lo, carry := bits.Add64(ll, rl, 0)
			hi, _ := bits.Add64(lh, rh, carry)
			stack[sp-2], stack[sp-1] = lo, hi
		case wasm.OpI64Sub128:
			sp -= 2
			ll, lh, rl, rh := stack[sp-2], stack[sp-1], stack[sp], stack[sp+1]
			lo, borrow := bits.Sub64(ll, rl, 0)
			hi, _ := bits.Sub64(lh, rh, borrow)
			stack[sp-2], stack[sp-1] = lo, hi
		case wasm.OpI64MulWideU:
			a, b := stack[sp-2], stack[sp-1]
			hi, lo := bits.Mul64(a, b)
			stack[sp-2], stack[sp-1] = lo, hi
		case wasm.OpI64MulWideS:
			a, b := stack[sp-2], stack[sp-1]
			hi, lo := bits.Mul64(a, b)
			if int64(a) < 0 {
				hi -= b
			}
			if int64(b) < 0 {
				hi -= a
			}
			stack[sp-2], stack[sp-1] = lo, hi
		default:
			panic("vm: unknown compiled op")
		}
	}

trap:
	meter.Used[types.C] = effC - gas
	m.sp, m.pc, m.fp = sp, pc, fp
	return done(types.StatusTrap)

oog:
	// A price did not fit: it exhausts the frame, or else the block.
	meter.Used[types.C] = effC - gas
	m.sp, m.pc, m.fp = sp, pc, fp
	if need > m.act.limit[types.C]-meter.Used[types.C] {
		return done(types.StatusExhaustedC)
	}
	return runResult{kind: runBlockFull}
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func fill(b []byte, v byte) {
	if len(b) == 0 {
		return
	}
	if v == 0 {
		clear(b)
		return
	}
	b[0] = v
	for i := 1; i < len(b); i *= 2 {
		copy(b[i:], b[:i])
	}
}
