// Package wasm decodes, validates and compiles Ting programs: WebAssembly
// modules in the strict profile of rules §16–§18 and §25.
//
// Decode accepts exactly the canonical bytes the profile allows and rejects
// everything else at the first unexpected byte. Every declared count is
// checked against the bytes left in its section before anything is
// allocated, so validation work and memory stay proportional to the module's
// size. A successful decode yields a Module whose functions are compiled to a
// flat instruction array with every branch resolved and every function's
// reservation measured, ready for the interpreter.
package wasm

import (
	"bytes"
	"errors"
	"fmt"
	"unsafe"
)

// Profile bounds (rules §14.5, §18.3).
const (
	MaxModuleSize      = 1 << 30
	MaxTypeParams      = 1000
	MaxLocals          = 16384
	MaxReservation     = 32768
	MaxFunctions       = 1000000
	MaxTypes           = 1000000
	MaxGlobals         = 1000000
	MaxDataSegments    = 100000
	MaxElementSegments = 100000
	MaxMemoryPages     = 65536
	PageSize           = 65536
)

// ErrInvalid wraps every rejection.
var ErrInvalid = errors.New("invalid module")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// FuncType is a function signature.
type FuncType struct {
	Params []byte
	Result byte // 0 for none
	// Canon is equal for structurally equal types.
	Canon uint32
}

// Instr is one compiled instruction. The meaning of A, B and Imm depends on
// the op:
//
//	branches (br, br_if):   A = target pc, B = target operand height, Imm = arity
//	br_table:               A = first entry in Function.Tables, B = entry count (default last)
//	if:                     A = pc of the else arm (or of the code after end)
//	jump (else):            A = target pc
//	local.*:                A = local index
//	global.*:               A = global index
//	call:                   A = defined-function index (relative to the first defined function)
//	call host:              A = HostFunc
//	call_indirect:          A = expected type's Canon
//	loads, stores:          A = offset
//	const:                  Imm = value (i32 zero-extended)
type Instr struct {
	Op Op
	// Price is StaticPrice[Op], stored inline for the dispatch loop.
	Price uint16
	A     uint32
	B     uint32
	Imm   uint64
}

// BrTarget is one br_table entry.
type BrTarget struct {
	PC     uint32
	Height uint32
	Arity  uint32
}

// Function is a validated, compiled defined function.
type Function struct {
	TypeIndex  uint32
	Canon      uint32
	NumParams  uint32
	NumLocals  uint32 // declared locals, excluding parameters
	MaxOperand uint32
	// Reservation is parameters + declared locals + maximum operand height:
	// the slots charged on every entry (rules §21.4).
	Reservation uint64
	Result      byte
	Code        []Instr
	Tables      []BrTarget
}

// Global is an integer global and its constant initializer.
type Global struct {
	Type    byte
	Mutable bool
	Init    uint64
}

// DataSegment is an active data segment.
type DataSegment struct {
	Offset uint32
	Bytes  []byte
}

// TableEntry describes a function reference held in the table.
type TableEntry struct {
	Func  int32  // function index, or -1 for null
	Canon uint32 // its type's Canon
}

// Module is a validated program and its preparation profile (rules §18.4).
type Module struct {
	Types []FuncType
	// Imports maps each imported function index to its host function.
	Imports []HostFunc
	// Funcs are the defined functions; function index = len(Imports) + i.
	Funcs   []*Function
	Globals []Global
	Data    []DataSegment

	HasTable  bool
	TableSize uint32
	// Table is the table's fixed contents after the element segments.
	Table []TableEntry

	HasMemory bool
	MemMin    uint32
	MemMax    uint32

	// DataBytes is the total length of the active data segments and
	// DataSpans the number of them that hold at least one byte.
	DataBytes uint64
	DataSpans uint64

	// Invoke is the defined-function index (relative) of the entry point.
	Invoke uint32

	// funcTypes is the type index of every function in the index space.
	funcTypes []uint32
}

// NumFunctions returns the size of the function index space.
func (m *Module) NumFunctions() int { return len(m.Imports) + len(m.Funcs) }

// FuncCanon returns the Canon of the function at index idx.
func (m *Module) FuncCanon(idx uint32) uint32 { return m.Types[m.funcTypes[idx]].Canon }

// Section ids in their required order (rules §17.2).
var sectionOrder = [...]byte{1, 2, 3, 4, 5, 6, 7, 9, 10, 11}

func sectionRank(id byte) int {
	for i, s := range sectionOrder {
		if s == id {
			return i
		}
	}
	return -1
}

var magic = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

// Decode validates a module and compiles it. Any error wraps ErrInvalid.
func Decode(code []byte) (*Module, error) {
	m, err := decode(code)
	if err != nil && !errors.Is(err, ErrInvalid) {
		err = fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return m, err
}

func decode(code []byte) (*Module, error) {
	if len(code) > MaxModuleSize {
		return nil, invalid("module of %d bytes exceeds 2^30", len(code))
	}
	if len(code) < 8 || !bytes.Equal(code[:8], magic) {
		return nil, invalid("bad preamble")
	}
	d := &decoder{m: &Module{}}
	r := reader{b: code, pos: 8}
	lastRank := -1
	var funcDecls []uint32 // type index of each defined function, from section 3
	haveCode := false
	for !r.eof() {
		id, _ := r.byte()
		rank := sectionRank(id)
		if rank < 0 {
			return nil, invalid("section id %d not accepted", id)
		}
		if rank <= lastRank {
			return nil, invalid("section %d out of order or repeated", id)
		}
		lastRank = rank
		size, err := r.u32()
		if err != nil {
			return nil, invalid("section %d size: %v", id, err)
		}
		content, err := r.bytes(size)
		if err != nil {
			return nil, invalid("section %d runs past the end", id)
		}
		sr := &reader{b: content}
		switch id {
		case 1:
			err = d.typeSection(sr)
		case 2:
			err = d.importSection(sr)
		case 3:
			funcDecls, err = d.functionSection(sr)
		case 4:
			err = d.tableSection(sr)
		case 5:
			err = d.memorySection(sr)
		case 6:
			err = d.globalSection(sr)
		case 7:
			err = d.exportSection(sr)
		case 9:
			err = d.elementSection(sr)
		case 10:
			haveCode = true
			err = d.codeSection(sr, funcDecls)
		case 11:
			err = d.dataSection(sr)
		}
		if err != nil {
			return nil, err
		}
		if !sr.eof() {
			return nil, invalid("section %d has %d unparsed bytes", id, sr.remaining())
		}
	}
	if len(funcDecls) > 0 && !haveCode {
		return nil, invalid("function section without code section")
	}
	if !d.haveInvoke {
		return nil, invalid("no invoke export")
	}
	if d.m.HasMemory != d.haveMemExport {
		return nil, invalid("memory export must be present exactly when a memory is declared")
	}
	return d.m, nil
}

type decoder struct {
	m             *Module
	haveInvoke    bool
	haveMemExport bool
	numDefined    uint32
}

// count reads a section's leading vector count: nonzero and no more than the
// bytes left (rules §17.2, §26.6).
func count(r *reader, what string, minEntry int) (uint32, error) {
	n, err := r.u32()
	if err != nil {
		return 0, invalid("%s count: %v", what, err)
	}
	if n == 0 {
		return 0, invalid("empty %s section", what)
	}
	if uint64(n)*uint64(minEntry) > uint64(r.remaining()) {
		return 0, invalid("%s count %d exceeds section size", what, n)
	}
	return n, nil
}

func valType(r *reader) (byte, error) {
	b, err := r.byte()
	if err != nil {
		return 0, err
	}
	if b != I32 && b != I64 {
		return 0, invalid("value type 0x%02x", b)
	}
	return b, nil
}

func (d *decoder) typeSection(r *reader) error {
	n, err := count(r, "type", 3)
	if err != nil {
		return err
	}
	if n > MaxTypes {
		return invalid("%d types", n)
	}
	d.m.Types = make([]FuncType, n)
	canon := make(map[string]uint32)
	for i := range d.m.Types {
		form, err := r.byte()
		if err != nil || form != 0x60 {
			return invalid("type %d: expected 0x60", i)
		}
		np, err := r.u32()
		if err != nil {
			return invalid("type %d params: %v", i, err)
		}
		if np > MaxTypeParams {
			return invalid("type %d has %d parameters", i, np)
		}
		if uint64(np) > uint64(r.remaining()) {
			return invalid("type %d parameter count exceeds section", i)
		}
		ft := &d.m.Types[i]
		ft.Params = make([]byte, np)
		for j := range ft.Params {
			if ft.Params[j], err = valType(r); err != nil {
				return invalid("type %d param %d: %v", i, j, err)
			}
		}
		nr, err := r.u32()
		if err != nil {
			return invalid("type %d results: %v", i, err)
		}
		if nr > 1 {
			return invalid("type %d has %d results", i, nr)
		}
		if nr == 1 {
			if ft.Result, err = valType(r); err != nil {
				return invalid("type %d result: %v", i, err)
			}
		}
		key := string(ft.Params) + "|" + string([]byte{ft.Result})
		c, ok := canon[key]
		if !ok {
			c = uint32(len(canon))
			canon[key] = c
		}
		ft.Canon = c
	}
	return nil
}

func name(r *reader) ([]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	return r.bytes(n)
}

func (d *decoder) importSection(r *reader) error {
	n, err := count(r, "import", 4)
	if err != nil {
		return err
	}
	if n > uint32(NumHostFuncs) {
		return invalid("%d imports", n)
	}
	var seen [NumHostFuncs]bool
	for i := uint32(0); i < n; i++ {
		mod, err := name(r)
		if err != nil {
			return invalid("import %d module name: %v", i, err)
		}
		if string(mod) != "ting" {
			return invalid("import %d from module %q", i, mod)
		}
		field, err := name(r)
		if err != nil {
			return invalid("import %d name: %v", i, err)
		}
		hf, ok := hostByName(field)
		if !ok {
			return invalid("import %d: unknown host function %q", i, field)
		}
		if seen[hf] {
			return invalid("import %q repeated", field)
		}
		seen[hf] = true
		kind, err := r.byte()
		if err != nil || kind != 0x00 {
			return invalid("import %d: kind must be function", i)
		}
		ti, err := r.u32()
		if err != nil {
			return invalid("import %d type: %v", i, err)
		}
		if ti >= uint32(len(d.m.Types)) {
			return invalid("import %d: type index %d out of range", i, ti)
		}
		ft := &d.m.Types[ti]
		sig := hostSigs[hf]
		if len(ft.Params) != sig.params {
			return invalid("import %q: wrong signature", field)
		}
		for _, p := range ft.Params {
			if p != I32 {
				return invalid("import %q: wrong signature", field)
			}
		}
		if sig.result != (ft.Result == I32) || (ft.Result != 0 && ft.Result != I32) {
			return invalid("import %q: wrong signature", field)
		}
		d.m.Imports = append(d.m.Imports, hf)
		d.m.funcTypes = append(d.m.funcTypes, ti)
	}
	return nil
}

func (d *decoder) functionSection(r *reader) ([]uint32, error) {
	n, err := count(r, "function", 1)
	if err != nil {
		return nil, err
	}
	if uint64(n)+uint64(len(d.m.Imports)) > MaxFunctions {
		return nil, invalid("%d functions", uint64(n)+uint64(len(d.m.Imports)))
	}
	decls := make([]uint32, n)
	for i := range decls {
		ti, err := r.u32()
		if err != nil {
			return nil, invalid("function %d type: %v", i, err)
		}
		if ti >= uint32(len(d.m.Types)) {
			return nil, invalid("function %d: type index %d out of range", i, ti)
		}
		decls[i] = ti
	}
	d.numDefined = n
	d.m.funcTypes = append(d.m.funcTypes, decls...)
	return decls, nil
}

func (d *decoder) totalFuncs() uint32 { return uint32(len(d.m.Imports)) + d.numDefined }

func (d *decoder) tableSection(r *reader) error {
	n, err := count(r, "table", 4)
	if err != nil {
		return err
	}
	if n != 1 {
		return invalid("%d tables", n)
	}
	et, err := r.byte()
	if err != nil || et != 0x70 {
		return invalid("table element type must be funcref")
	}
	flag, err := r.byte()
	if err != nil || flag != 0x01 {
		return invalid("table limits flag must be 0x01")
	}
	min, err := r.u32()
	if err != nil {
		return invalid("table minimum: %v", err)
	}
	max, err := r.u32()
	if err != nil {
		return invalid("table maximum: %v", err)
	}
	if min != max {
		return invalid("table maximum must equal its minimum")
	}
	if min > d.totalFuncs() {
		return invalid("table size %d exceeds function count %d", min, d.totalFuncs())
	}
	d.m.HasTable = true
	d.m.TableSize = min
	d.m.Table = make([]TableEntry, min)
	for i := range d.m.Table {
		d.m.Table[i].Func = -1
	}
	return nil
}

func (d *decoder) memorySection(r *reader) error {
	n, err := count(r, "memory", 3)
	if err != nil {
		return err
	}
	if n != 1 {
		return invalid("%d memories", n)
	}
	flag, err := r.byte()
	if err != nil || flag != 0x01 {
		return invalid("memory limits flag must be 0x01")
	}
	min, err := r.u32()
	if err != nil {
		return invalid("memory minimum: %v", err)
	}
	max, err := r.u32()
	if err != nil {
		return invalid("memory maximum: %v", err)
	}
	if max > MaxMemoryPages || min > max {
		return invalid("memory limits %d..%d", min, max)
	}
	d.m.HasMemory = true
	d.m.MemMin = min
	d.m.MemMax = max
	return nil
}

// constExpr reads `i32.const n end` (want I32) or `i64.const n end`.
func constExpr(r *reader, want byte) (uint64, error) {
	op, err := r.byte()
	if err != nil {
		return 0, err
	}
	var v uint64
	switch {
	case want == I32 && op == byte(OpI32Const):
		x, err := r.s32()
		if err != nil {
			return 0, err
		}
		v = uint64(uint32(x))
	case want == I64 && op == byte(OpI64Const):
		x, err := r.s64()
		if err != nil {
			return 0, err
		}
		v = uint64(x)
	default:
		return 0, invalid("initializer must be a single constant of the declared type")
	}
	end, err := r.byte()
	if err != nil || end != byte(OpEnd) {
		return 0, invalid("initializer must end after its constant")
	}
	return v, nil
}

func (d *decoder) globalSection(r *reader) error {
	n, err := count(r, "global", 5)
	if err != nil {
		return err
	}
	if n > MaxGlobals {
		return invalid("%d globals", n)
	}
	d.m.Globals = make([]Global, n)
	for i := range d.m.Globals {
		g := &d.m.Globals[i]
		if g.Type, err = valType(r); err != nil {
			return invalid("global %d type: %v", i, err)
		}
		mut, err := r.byte()
		if err != nil || mut > 1 {
			return invalid("global %d mutability", i)
		}
		g.Mutable = mut == 1
		if g.Init, err = constExpr(r, g.Type); err != nil {
			return invalid("global %d initializer: %v", i, err)
		}
	}
	return nil
}

func (d *decoder) exportSection(r *reader) error {
	n, err := count(r, "export", 3)
	if err != nil {
		return err
	}
	if n > 2 {
		return invalid("%d exports", n)
	}
	for i := uint32(0); i < n; i++ {
		nm, err := name(r)
		if err != nil {
			return invalid("export %d name: %v", i, err)
		}
		kind, err := r.byte()
		if err != nil {
			return invalid("export %d kind", i)
		}
		idx, err := r.u32()
		if err != nil {
			return invalid("export %d index: %v", i, err)
		}
		switch string(nm) {
		case "invoke":
			if d.haveInvoke {
				return invalid("invoke exported twice")
			}
			if kind != 0x00 {
				return invalid("invoke must be a function")
			}
			nimp := uint32(len(d.m.Imports))
			if idx < nimp || idx >= d.totalFuncs() {
				return invalid("invoke must name a defined function")
			}
			ft := &d.m.Types[d.m.funcTypes[idx]]
			if len(ft.Params) != 0 || ft.Result != 0 {
				return invalid("invoke must take no parameters and return nothing")
			}
			d.haveInvoke = true
			d.m.Invoke = idx - nimp
		case "memory":
			if d.haveMemExport {
				return invalid("memory exported twice")
			}
			if kind != 0x02 || idx != 0 || !d.m.HasMemory {
				return invalid("memory export must be memory 0 of a declared memory")
			}
			d.haveMemExport = true
		default:
			return invalid("export %q not accepted", nm)
		}
	}
	return nil
}

func (d *decoder) elementSection(r *reader) error {
	n, err := count(r, "element", 4)
	if err != nil {
		return err
	}
	if !d.m.HasTable {
		return invalid("element segments without a table")
	}
	if n > MaxElementSegments {
		return invalid("%d element segments", n)
	}
	total := d.totalFuncs()
	for i := uint32(0); i < n; i++ {
		flag, err := r.byte()
		if err != nil || flag != 0x00 {
			return invalid("element segment %d flag", i)
		}
		off, err := constExpr(r, I32)
		if err != nil {
			return invalid("element segment %d offset: %v", i, err)
		}
		cnt, err := r.u32()
		if err != nil {
			return invalid("element segment %d count: %v", i, err)
		}
		if uint64(cnt) > uint64(r.remaining()) {
			return invalid("element segment %d count exceeds section", i)
		}
		if off+uint64(cnt) > uint64(d.m.TableSize) {
			return invalid("element segment %d does not fit the table", i)
		}
		for j := uint32(0); j < cnt; j++ {
			fi, err := r.u32()
			if err != nil {
				return invalid("element segment %d entry %d: %v", i, j, err)
			}
			if fi >= total {
				return invalid("element segment %d: function %d out of range", i, fi)
			}
			d.m.Table[off+uint64(j)] = TableEntry{Func: int32(fi), Canon: d.m.Types[d.m.funcTypes[fi]].Canon}
		}
	}
	return nil
}

func (d *decoder) codeSection(r *reader, decls []uint32) error {
	n, err := count(r, "code", 2)
	if err != nil {
		return err
	}
	if n != uint32(len(decls)) {
		return invalid("code count %d does not match function count %d", n, len(decls))
	}
	d.m.Funcs = make([]*Function, n)
	for i := range d.m.Funcs {
		size, err := r.u32()
		if err != nil {
			return invalid("body %d size: %v", i, err)
		}
		body, err := r.bytes(size)
		if err != nil {
			return invalid("body %d runs past its section", i)
		}
		f, err := compileFunction(d.m, decls[i], body)
		if err != nil {
			return fmt.Errorf("%w (function %d)", err, i)
		}
		d.m.Funcs[i] = f
	}
	return nil
}

func (d *decoder) dataSection(r *reader) error {
	n, err := count(r, "data", 4)
	if err != nil {
		return err
	}
	if !d.m.HasMemory {
		return invalid("data segments without a memory")
	}
	if n > MaxDataSegments {
		return invalid("%d data segments", n)
	}
	limit := uint64(d.m.MemMin) * PageSize
	d.m.Data = make([]DataSegment, n)
	for i := range d.m.Data {
		flag, err := r.byte()
		if err != nil || flag != 0x00 {
			return invalid("data segment %d flag", i)
		}
		off, err := constExpr(r, I32)
		if err != nil {
			return invalid("data segment %d offset: %v", i, err)
		}
		ln, err := r.u32()
		if err != nil {
			return invalid("data segment %d length: %v", i, err)
		}
		b, err := r.bytes(ln)
		if err != nil {
			return invalid("data segment %d runs past its section", i)
		}
		if off+uint64(ln) > limit {
			return invalid("data segment %d does not fit the initial memory", i)
		}
		d.m.Data[i] = DataSegment{Offset: uint32(off), Bytes: b}
		d.m.DataBytes += uint64(ln)
		if ln > 0 {
			d.m.DataSpans++
		}
	}
	return nil
}

// Weight is roughly the memory a decoded module holds, in bytes: what a
// code cache counts it as (the reference's Module::weight).
func (m *Module) Weight() int64 {
	n := int64(unsafe.Sizeof(*m))
	for _, f := range m.Funcs {
		n += int64(unsafe.Sizeof(*f)) + int64(len(f.Code))*int64(unsafe.Sizeof(Instr{})) +
			int64(len(f.Tables))*int64(unsafe.Sizeof(BrTarget{}))
	}
	for _, t := range m.Types {
		n += int64(unsafe.Sizeof(t)) + int64(len(t.Params))
	}
	for _, d := range m.Data {
		n += int64(unsafe.Sizeof(d)) + int64(len(d.Bytes))
	}
	n += int64(len(m.Imports))*int64(unsafe.Sizeof(HostFunc(0))) +
		int64(len(m.Globals))*int64(unsafe.Sizeof(Global{})) +
		int64(len(m.Table))*int64(unsafe.Sizeof(TableEntry{})) + int64(len(m.funcTypes))*4
	return n
}
