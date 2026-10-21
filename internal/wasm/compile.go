package wasm

import "sort"

// Function-body validation (rules §25) and compilation.

const (
	kindFunction byte = iota
	kindBlock
	kindLoop
	kindIfThen
	kindIfElse
)

type ctrlFrame struct {
	kind        byte
	result      byte // 0 none
	height      int
	unreachable bool
	loopPC      uint32 // loop: first instruction of the body
	ifInstr     int    // if-then / if-else: index of the If instruction
	elseJump    int    // if-else: index of the Jump at the end of the then-arm
	patchHead   int    // linked list of branches to patch with this frame's end
}

type patch struct {
	idx   int  // instruction index, or table entry index
	table bool // idx refers to Function.Tables
	next  int
}

// localRun is a run of locals of one type ending before index end.
type localRun struct {
	end uint32
	t   byte
}

type compiler struct {
	m *Module
	r reader
	// locals holds one run per parameter and per local group, so declaring
	// many locals in one group costs no work (rules §18.4).
	locals  []localRun
	vals    []byte
	ctrls   []ctrlFrame
	maxH    int
	slotCap int // most operand slots allowed by the reservation bound
	f       *Function
	patches []patch
}

func compileFunction(m *Module, typeIdx uint32, body []byte) (*Function, error) {
	ft := &m.Types[typeIdx]
	c := &compiler{m: m, r: reader{b: body}}
	f := &Function{
		TypeIndex: typeIdx,
		Canon:     ft.Canon,
		NumParams: uint32(len(ft.Params)),
		Result:    ft.Result,
	}
	c.f = f
	// Locals: groups of (count >= 1, type), neighbors of different types.
	ngroups, err := c.r.u32()
	if err != nil {
		return nil, invalid("local groups: %v", err)
	}
	if uint64(ngroups)*2 > uint64(c.r.remaining()) {
		return nil, invalid("local group count exceeds body")
	}
	total := uint64(len(ft.Params))
	c.locals = make([]localRun, 0, len(ft.Params)+int(ngroups))
	for i, p := range ft.Params {
		c.locals = append(c.locals, localRun{end: uint32(i + 1), t: p})
	}
	var prev byte
	for g := uint32(0); g < ngroups; g++ {
		n, err := c.r.u32()
		if err != nil {
			return nil, invalid("local group %d count: %v", g, err)
		}
		if n == 0 {
			return nil, invalid("local group %d is empty", g)
		}
		t, err := valType(&c.r)
		if err != nil {
			return nil, invalid("local group %d type: %v", g, err)
		}
		if g > 0 && t == prev {
			return nil, invalid("neighboring local groups share a type")
		}
		prev = t
		total += uint64(n)
		if total > MaxLocals {
			return nil, invalid("more than %d parameters and locals", MaxLocals)
		}
		c.locals = append(c.locals, localRun{end: uint32(total), t: t})
	}
	f.NumLocals = uint32(total) - f.NumParams
	c.slotCap = MaxReservation - int(total)
	c.ctrls = append(c.ctrls, ctrlFrame{kind: kindFunction, result: ft.Result, patchHead: -1})
	if err := c.body(); err != nil {
		return nil, err
	}
	if !c.r.eof() {
		return nil, invalid("bytes after the function's final end")
	}
	f.MaxOperand = uint32(c.maxH)
	f.Reservation = total + uint64(c.maxH)
	if f.Reservation > MaxReservation {
		return nil, invalid("reservation %d exceeds %d", f.Reservation, MaxReservation)
	}
	return f, nil
}

// localType returns the type of local idx (parameters first).
func (c *compiler) localType(idx uint32) (byte, bool) {
	n := len(c.locals)
	if n == 0 || idx >= c.locals[n-1].end {
		return 0, false
	}
	i := sort.Search(n, func(i int) bool { return c.locals[i].end > idx })
	return c.locals[i].t, true
}

// Validator stack operations (rules §25.1).

func (c *compiler) push(t byte) error {
	c.vals = append(c.vals, t)
	if len(c.vals) > c.slotCap {
		return invalid("operand height exceeds the reservation bound")
	}
	return nil
}

func (c *compiler) popAny() (byte, error) {
	f := &c.ctrls[len(c.ctrls)-1]
	if len(c.vals) == f.height {
		if f.unreachable {
			return unknown, nil
		}
		return 0, invalid("operand stack underflow")
	}
	t := c.vals[len(c.vals)-1]
	c.vals = c.vals[:len(c.vals)-1]
	return t, nil
}

func (c *compiler) pop(t byte) error {
	a, err := c.popAny()
	if err != nil {
		return err
	}
	if a != unknown && a != t {
		return invalid("type mismatch")
	}
	return nil
}

func (c *compiler) popResult(t byte) error {
	if t != 0 {
		return c.pop(t)
	}
	return nil
}

func (c *compiler) pushResult(t byte) error {
	if t != 0 {
		return c.push(t)
	}
	return nil
}

func (c *compiler) makeUnreachable() {
	f := &c.ctrls[len(c.ctrls)-1]
	c.vals = c.vals[:f.height]
	f.unreachable = true
}

func (c *compiler) openFrame(kind, result byte) {
	c.ctrls = append(c.ctrls, ctrlFrame{kind: kind, result: result, height: len(c.vals), patchHead: -1})
}

func (c *compiler) closeFrame() (ctrlFrame, error) {
	f := c.ctrls[len(c.ctrls)-1]
	if err := c.popResult(f.result); err != nil {
		return f, err
	}
	if len(c.vals) != f.height {
		return f, invalid("values remain at the end of a block")
	}
	c.ctrls = c.ctrls[:len(c.ctrls)-1]
	return f, nil
}

func (c *compiler) label(depth uint32) (*ctrlFrame, error) {
	if uint64(depth) >= uint64(len(c.ctrls)) {
		return nil, invalid("branch depth %d out of range", depth)
	}
	return &c.ctrls[len(c.ctrls)-1-int(depth)], nil
}

func labelResult(f *ctrlFrame) byte {
	if f.kind == kindLoop {
		return 0
	}
	return f.result
}

func arity(t byte) uint32 {
	if t != 0 {
		return 1
	}
	return 0
}

func (c *compiler) emit(op Op, a, b uint32, imm uint64) int {
	c.f.Code = append(c.f.Code, Instr{Op: op, Price: uint16(StaticPrice[op]), A: a, B: b, Imm: imm})
	return len(c.f.Code) - 1
}

// target fills a branch's destination: immediately for a loop, or through
// the frame's patch list when its end is reached.
func (c *compiler) branchTo(f *ctrlFrame, instr int) {
	in := &c.f.Code[instr]
	in.B = uint32(f.height)
	in.Imm = uint64(arity(labelResult(f)))
	if f.kind == kindLoop {
		in.A = f.loopPC
		return
	}
	c.patches = append(c.patches, patch{idx: instr, next: f.patchHead})
	f.patchHead = len(c.patches) - 1
}

func (c *compiler) tableTo(f *ctrlFrame) {
	t := BrTarget{Height: uint32(f.height), Arity: arity(labelResult(f))}
	c.f.Tables = append(c.f.Tables, t)
	idx := len(c.f.Tables) - 1
	if f.kind == kindLoop {
		c.f.Tables[idx].PC = f.loopPC
		return
	}
	c.patches = append(c.patches, patch{idx: idx, table: true, next: f.patchHead})
	f.patchHead = len(c.patches) - 1
}

func (c *compiler) resolve(f *ctrlFrame, pc uint32) {
	for p := f.patchHead; p >= 0; p = c.patches[p].next {
		if c.patches[p].table {
			c.f.Tables[c.patches[p].idx].PC = pc
		} else {
			c.f.Code[c.patches[p].idx].A = pc
		}
	}
}

func (c *compiler) blockType() (byte, error) {
	b, err := c.r.byte()
	if err != nil {
		return 0, err
	}
	switch b {
	case blockEmpty:
		return 0, nil
	case I32, I64:
		return b, nil
	}
	return 0, invalid("block type 0x%02x", b)
}

func (c *compiler) memarg(width uint32) (uint32, error) {
	if !c.m.HasMemory {
		return 0, invalid("memory instruction without a memory")
	}
	align, err := c.r.u32()
	if err != nil {
		return 0, invalid("alignment: %v", err)
	}
	maxAlign := uint32(0)
	for w := width; w > 1; w >>= 1 {
		maxAlign++
	}
	if align > maxAlign {
		return 0, invalid("alignment exceeds natural alignment")
	}
	off, err := c.r.u32()
	if err != nil {
		return 0, invalid("offset: %v", err)
	}
	return off, nil
}

func (c *compiler) zeroByte() error {
	b, err := c.r.byte()
	if err != nil {
		return err
	}
	if b != 0 {
		return invalid("reserved byte must be zero")
	}
	return nil
}

func (c *compiler) unop(in, out byte) error {
	if err := c.pop(in); err != nil {
		return err
	}
	return c.push(out)
}

func (c *compiler) binop(in, out byte) error {
	if err := c.pop(in); err != nil {
		return err
	}
	if err := c.pop(in); err != nil {
		return err
	}
	return c.push(out)
}

func (c *compiler) body() error {
	for {
		op, err := c.r.byte()
		if err != nil {
			return invalid("function body ends without end")
		}
		done, err := c.instr(op)
		if err != nil {
			return err
		}
		if len(c.vals) > c.maxH {
			c.maxH = len(c.vals)
		}
		if done {
			return nil
		}
	}
}

// instr validates and compiles one instruction; done reports that the
// function's final end was processed.
func (c *compiler) instr(b byte) (done bool, err error) {
	op := Op(b)
	switch op {
	case OpUnreachable:
		c.emit(op, 0, 0, 0)
		c.makeUnreachable()
	case OpNop:
		c.emit(op, 0, 0, 0)
	case OpBlock, OpLoop:
		bt, err := c.blockType()
		if err != nil {
			return false, err
		}
		c.emit(op, 0, 0, 0)
		if op == OpBlock {
			c.openFrame(kindBlock, bt)
		} else {
			c.openFrame(kindLoop, bt)
			c.ctrls[len(c.ctrls)-1].loopPC = uint32(len(c.f.Code))
		}
	case OpIf:
		bt, err := c.blockType()
		if err != nil {
			return false, err
		}
		if err := c.pop(I32); err != nil {
			return false, err
		}
		idx := c.emit(op, 0, 0, 0)
		c.openFrame(kindIfThen, bt)
		c.ctrls[len(c.ctrls)-1].ifInstr = idx
	case OpElse:
		f, err := c.closeFrame()
		if err != nil {
			return false, err
		}
		if f.kind != kindIfThen {
			return false, invalid("else without if")
		}
		jump := c.emit(OpJump, 0, 0, 0)
		c.f.Code[f.ifInstr].A = uint32(len(c.f.Code))
		nf := ctrlFrame{kind: kindIfElse, result: f.result, height: f.height,
			ifInstr: f.ifInstr, elseJump: jump, patchHead: f.patchHead}
		c.ctrls = append(c.ctrls, nf)
	case OpEnd:
		f, err := c.closeFrame()
		if err != nil {
			return false, err
		}
		switch f.kind {
		case kindIfThen:
			if f.result != 0 {
				return false, invalid("if without else cannot have a result")
			}
			c.f.Code[f.ifInstr].A = uint32(len(c.f.Code))
		case kindIfElse:
			c.f.Code[f.elseJump].A = uint32(len(c.f.Code))
		case kindFunction:
			end := c.emit(OpEndFunc, 0, 0, uint64(arity(f.result)))
			c.resolve(&f, uint32(end))
			if err := c.pushResult(f.result); err != nil {
				return false, err
			}
			return true, nil
		}
		c.resolve(&f, uint32(len(c.f.Code)))
		if err := c.pushResult(f.result); err != nil {
			return false, err
		}
	case OpBr:
		depth, err := c.r.u32()
		if err != nil {
			return false, invalid("br depth: %v", err)
		}
		f, err := c.label(depth)
		if err != nil {
			return false, err
		}
		if err := c.popResult(labelResult(f)); err != nil {
			return false, err
		}
		c.branchTo(f, c.emit(op, 0, 0, 0))
		c.makeUnreachable()
	case OpBrIf:
		depth, err := c.r.u32()
		if err != nil {
			return false, invalid("br_if depth: %v", err)
		}
		f, err := c.label(depth)
		if err != nil {
			return false, err
		}
		if err := c.pop(I32); err != nil {
			return false, err
		}
		lr := labelResult(f)
		if err := c.popResult(lr); err != nil {
			return false, err
		}
		if err := c.pushResult(lr); err != nil {
			return false, err
		}
		c.branchTo(f, c.emit(op, 0, 0, 0))
	case OpBrTable:
		n, err := c.r.u32()
		if err != nil {
			return false, invalid("br_table count: %v", err)
		}
		if uint64(n) >= uint64(c.r.remaining()) {
			return false, invalid("br_table count exceeds body")
		}
		depths := make([]uint32, n+1)
		for i := range depths {
			if depths[i], err = c.r.u32(); err != nil {
				return false, invalid("br_table depth: %v", err)
			}
		}
		if err := c.pop(I32); err != nil {
			return false, err
		}
		def, err := c.label(depths[n])
		if err != nil {
			return false, err
		}
		lr := labelResult(def)
		start := len(c.f.Tables)
		for _, d := range depths {
			f, err := c.label(d)
			if err != nil {
				return false, err
			}
			if labelResult(f) != lr {
				return false, invalid("br_table labels differ in type")
			}
			c.tableTo(f)
		}
		if err := c.popResult(lr); err != nil {
			return false, err
		}
		c.emit(op, uint32(start), n+1, 0)
		c.makeUnreachable()
	case OpReturn:
		if err := c.popResult(c.ctrls[0].result); err != nil {
			return false, err
		}
		c.emit(op, 0, 0, uint64(arity(c.ctrls[0].result)))
		c.makeUnreachable()
	case OpCall:
		fi, err := c.r.u32()
		if err != nil {
			return false, invalid("call index: %v", err)
		}
		if uint64(fi) >= uint64(len(c.m.funcTypes)) {
			return false, invalid("call to function %d out of range", fi)
		}
		ft := &c.m.Types[c.m.funcTypes[fi]]
		for i := len(ft.Params) - 1; i >= 0; i-- {
			if err := c.pop(ft.Params[i]); err != nil {
				return false, err
			}
		}
		if err := c.pushResult(ft.Result); err != nil {
			return false, err
		}
		nimp := uint32(len(c.m.Imports))
		if fi < nimp {
			c.emit(OpCallHost, uint32(c.m.Imports[fi]), 0, 0)
		} else {
			c.emit(OpCall, fi-nimp, 0, 0)
		}
	case OpCallIndirect:
		ti, err := c.r.u32()
		if err != nil {
			return false, invalid("call_indirect type: %v", err)
		}
		if err := c.zeroByte(); err != nil {
			return false, err
		}
		if uint64(ti) >= uint64(len(c.m.Types)) {
			return false, invalid("call_indirect type %d out of range", ti)
		}
		if !c.m.HasTable {
			return false, invalid("call_indirect without a table")
		}
		if err := c.pop(I32); err != nil {
			return false, err
		}
		ft := &c.m.Types[ti]
		for i := len(ft.Params) - 1; i >= 0; i-- {
			if err := c.pop(ft.Params[i]); err != nil {
				return false, err
			}
		}
		if err := c.pushResult(ft.Result); err != nil {
			return false, err
		}
		c.emit(op, ft.Canon, 0, 0)
	case OpDrop:
		if _, err := c.popAny(); err != nil {
			return false, err
		}
		c.emit(op, 0, 0, 0)
	case OpSelect:
		if err := c.pop(I32); err != nil {
			return false, err
		}
		t1, err := c.popAny()
		if err != nil {
			return false, err
		}
		t2, err := c.popAny()
		if err != nil {
			return false, err
		}
		if t1 != unknown && t2 != unknown && t1 != t2 {
			return false, invalid("select operands differ in type")
		}
		t := t1
		if t == unknown {
			t = t2
		}
		if err := c.push(t); err != nil {
			return false, err
		}
		c.emit(op, 0, 0, 0)
	case OpLocalGet, OpLocalSet, OpLocalTee:
		idx, err := c.r.u32()
		if err != nil {
			return false, invalid("local index: %v", err)
		}
		t, ok := c.localType(idx)
		if !ok {
			return false, invalid("local %d out of range", idx)
		}
		switch op {
		case OpLocalGet:
			err = c.push(t)
		case OpLocalSet:
			err = c.pop(t)
		default:
			if err = c.pop(t); err == nil {
				err = c.push(t)
			}
		}
		if err != nil {
			return false, err
		}
		c.emit(op, idx, 0, 0)
	case OpGlobalGet, OpGlobalSet:
		idx, err := c.r.u32()
		if err != nil {
			return false, invalid("global index: %v", err)
		}
		if uint64(idx) >= uint64(len(c.m.Globals)) {
			return false, invalid("global %d out of range", idx)
		}
		g := &c.m.Globals[idx]
		if op == OpGlobalGet {
			err = c.push(g.Type)
		} else {
			if !g.Mutable {
				return false, invalid("global.set of an immutable global")
			}
			err = c.pop(g.Type)
		}
		if err != nil {
			return false, err
		}
		c.emit(op, idx, 0, 0)
	case OpI32Load, OpI32Load8S, OpI32Load8U, OpI32Load16S, OpI32Load16U:
		return false, c.load(op, I32)
	case OpI64Load, OpI64Load8S, OpI64Load8U, OpI64Load16S, OpI64Load16U, OpI64Load32S, OpI64Load32U:
		return false, c.load(op, I64)
	case OpI32Store, OpI32Store8, OpI32Store16:
		return false, c.store(op, I32)
	case OpI64Store, OpI64Store8, OpI64Store16, OpI64Store32:
		return false, c.store(op, I64)
	case OpMemorySize, OpMemoryGrow:
		if err := c.zeroByte(); err != nil {
			return false, err
		}
		if !c.m.HasMemory {
			return false, invalid("memory instruction without a memory")
		}
		if op == OpMemorySize {
			err = c.push(I32)
		} else {
			err = c.unop(I32, I32)
		}
		if err != nil {
			return false, err
		}
		c.emit(op, 0, 0, 0)
	case OpI32Const:
		v, err := c.r.s32()
		if err != nil {
			return false, invalid("i32.const: %v", err)
		}
		if err := c.push(I32); err != nil {
			return false, err
		}
		c.emit(op, 0, 0, uint64(uint32(v)))
	case OpI64Const:
		v, err := c.r.s64()
		if err != nil {
			return false, invalid("i64.const: %v", err)
		}
		if err := c.push(I64); err != nil {
			return false, err
		}
		c.emit(op, 0, 0, uint64(v))
	case OpI32Eqz, OpI32Clz, OpI32Ctz, OpI32Popcnt, OpI32Extend8S, OpI32Extend16S:
		return false, c.simple(op, c.unop(I32, I32))
	case OpI64Eqz:
		return false, c.simple(op, c.unop(I64, I32))
	case OpI64Clz, OpI64Ctz, OpI64Popcnt, OpI64Extend8S, OpI64Extend16S, OpI64Extend32S:
		return false, c.simple(op, c.unop(I64, I64))
	case OpI32WrapI64:
		return false, c.simple(op, c.unop(I64, I32))
	case OpI64ExtendI32S, OpI64ExtendI32U:
		return false, c.simple(op, c.unop(I32, I64))
	case OpPrefixFC:
		return false, c.prefixFC()
	default:
		switch {
		case op >= OpI32Eq && op <= OpI32GeU:
			return false, c.simple(op, c.binop(I32, I32))
		case op >= OpI64Eq && op <= OpI64GeU:
			return false, c.simple(op, c.binop(I64, I32))
		case op >= OpI32Add && op <= OpI32Rotr:
			return false, c.simple(op, c.binop(I32, I32))
		case op >= OpI64Add && op <= OpI64Rotr:
			return false, c.simple(op, c.binop(I64, I64))
		}
		return false, invalid("opcode 0x%02x not accepted", b)
	}
	return false, nil
}

func (c *compiler) simple(op Op, err error) error {
	if err != nil {
		return err
	}
	c.emit(op, 0, 0, 0)
	return nil
}

func (c *compiler) load(op Op, t byte) error {
	off, err := c.memarg(memWidth(op))
	if err != nil {
		return err
	}
	if err := c.unop(I32, t); err != nil {
		return err
	}
	c.emit(op, off, 0, 0)
	return nil
}

func (c *compiler) store(op Op, t byte) error {
	off, err := c.memarg(memWidth(op))
	if err != nil {
		return err
	}
	if err := c.pop(t); err != nil {
		return err
	}
	if err := c.pop(I32); err != nil {
		return err
	}
	c.emit(op, off, 0, 0)
	return nil
}

func (c *compiler) prefixFC() error {
	sub, err := c.r.u32()
	if err != nil {
		return invalid("0xfc sub-opcode: %v", err)
	}
	switch sub {
	case 0x0a: // memory.copy 00 00
		if err := c.zeroByte(); err != nil {
			return err
		}
		if err := c.zeroByte(); err != nil {
			return err
		}
		return c.bulk(OpMemoryCopy)
	case 0x0b: // memory.fill 00
		if err := c.zeroByte(); err != nil {
			return err
		}
		return c.bulk(OpMemoryFill)
	case 0x13, 0x14: // i64.add128, i64.sub128
		for i := 0; i < 4; i++ {
			if err := c.pop(I64); err != nil {
				return err
			}
		}
		if err := c.push(I64); err != nil {
			return err
		}
		if err := c.push(I64); err != nil {
			return err
		}
		c.emit(Op(0x100+sub), 0, 0, 0)
		return nil
	case 0x15, 0x16: // i64.mul_wide_s, i64.mul_wide_u
		for i := 0; i < 2; i++ {
			if err := c.pop(I64); err != nil {
				return err
			}
		}
		if err := c.push(I64); err != nil {
			return err
		}
		if err := c.push(I64); err != nil {
			return err
		}
		c.emit(Op(0x100+sub), 0, 0, 0)
		return nil
	}
	return invalid("0xfc sub-opcode %d not accepted", sub)
}

func (c *compiler) bulk(op Op) error {
	if !c.m.HasMemory {
		return invalid("memory instruction without a memory")
	}
	for i := 0; i < 3; i++ {
		if err := c.pop(I32); err != nil {
			return err
		}
	}
	c.emit(op, 0, 0, 0)
	return nil
}
