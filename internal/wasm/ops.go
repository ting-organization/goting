package wasm

// Value types (rules §17.3).
const (
	I32 byte = 0x7f
	I64 byte = 0x7e
	// unknown is the validator's internal marker (rules §25.1).
	unknown byte = 0
	// blockEmpty is the empty block type.
	blockEmpty byte = 0x40
)

// Op is an instruction of the compiled form. Opcodes below 0x100 are the
// WebAssembly single-byte opcodes; 0x100+n is the 0xfc-prefixed sub-opcode n;
// the rest are internal forms the compiler emits.
type Op = uint16

// WebAssembly opcodes accepted by the profile (rules §17.4).
const (
	OpUnreachable  Op = 0x00
	OpNop          Op = 0x01
	OpBlock        Op = 0x02
	OpLoop         Op = 0x03
	OpIf           Op = 0x04
	OpElse         Op = 0x05
	OpEnd          Op = 0x0b
	OpBr           Op = 0x0c
	OpBrIf         Op = 0x0d
	OpBrTable      Op = 0x0e
	OpReturn       Op = 0x0f
	OpCall         Op = 0x10
	OpCallIndirect Op = 0x11
	OpDrop         Op = 0x1a
	OpSelect       Op = 0x1b
	OpLocalGet     Op = 0x20
	OpLocalSet     Op = 0x21
	OpLocalTee     Op = 0x22
	OpGlobalGet    Op = 0x23
	OpGlobalSet    Op = 0x24

	OpI32Load    Op = 0x28
	OpI64Load    Op = 0x29
	OpI32Load8S  Op = 0x2c
	OpI32Load8U  Op = 0x2d
	OpI32Load16S Op = 0x2e
	OpI32Load16U Op = 0x2f
	OpI64Load8S  Op = 0x30
	OpI64Load8U  Op = 0x31
	OpI64Load16S Op = 0x32
	OpI64Load16U Op = 0x33
	OpI64Load32S Op = 0x34
	OpI64Load32U Op = 0x35
	OpI32Store   Op = 0x36
	OpI64Store   Op = 0x37
	OpI32Store8  Op = 0x3a
	OpI32Store16 Op = 0x3b
	OpI64Store8  Op = 0x3c
	OpI64Store16 Op = 0x3d
	OpI64Store32 Op = 0x3e
	OpMemorySize Op = 0x3f
	OpMemoryGrow Op = 0x40
	OpI32Const   Op = 0x41
	OpI64Const   Op = 0x42

	OpI32Eqz Op = 0x45
	OpI32Eq  Op = 0x46
	OpI32Ne  Op = 0x47
	OpI32LtS Op = 0x48
	OpI32LtU Op = 0x49
	OpI32GtS Op = 0x4a
	OpI32GtU Op = 0x4b
	OpI32LeS Op = 0x4c
	OpI32LeU Op = 0x4d
	OpI32GeS Op = 0x4e
	OpI32GeU Op = 0x4f
	OpI64Eqz Op = 0x50
	OpI64Eq  Op = 0x51
	OpI64Ne  Op = 0x52
	OpI64LtS Op = 0x53
	OpI64LtU Op = 0x54
	OpI64GtS Op = 0x55
	OpI64GtU Op = 0x56
	OpI64LeS Op = 0x57
	OpI64LeU Op = 0x58
	OpI64GeS Op = 0x59
	OpI64GeU Op = 0x5a

	OpI32Clz    Op = 0x67
	OpI32Ctz    Op = 0x68
	OpI32Popcnt Op = 0x69
	OpI32Add    Op = 0x6a
	OpI32Sub    Op = 0x6b
	OpI32Mul    Op = 0x6c
	OpI32DivS   Op = 0x6d
	OpI32DivU   Op = 0x6e
	OpI32RemS   Op = 0x6f
	OpI32RemU   Op = 0x70
	OpI32And    Op = 0x71
	OpI32Or     Op = 0x72
	OpI32Xor    Op = 0x73
	OpI32Shl    Op = 0x74
	OpI32ShrS   Op = 0x75
	OpI32ShrU   Op = 0x76
	OpI32Rotl   Op = 0x77
	OpI32Rotr   Op = 0x78
	OpI64Clz    Op = 0x79
	OpI64Ctz    Op = 0x7a
	OpI64Popcnt Op = 0x7b
	OpI64Add    Op = 0x7c
	OpI64Sub    Op = 0x7d
	OpI64Mul    Op = 0x7e
	OpI64DivS   Op = 0x7f
	OpI64DivU   Op = 0x80
	OpI64RemS   Op = 0x81
	OpI64RemU   Op = 0x82
	OpI64And    Op = 0x83
	OpI64Or     Op = 0x84
	OpI64Xor    Op = 0x85
	OpI64Shl    Op = 0x86
	OpI64ShrS   Op = 0x87
	OpI64ShrU   Op = 0x88
	OpI64Rotl   Op = 0x89
	OpI64Rotr   Op = 0x8a

	OpI32WrapI64    Op = 0xa7
	OpI64ExtendI32S Op = 0xac
	OpI64ExtendI32U Op = 0xad
	OpI32Extend8S   Op = 0xc0
	OpI32Extend16S  Op = 0xc1
	OpI64Extend8S   Op = 0xc2
	OpI64Extend16S  Op = 0xc3
	OpI64Extend32S  Op = 0xc4
	OpPrefixFC      Op = 0xfc
	OpMemoryCopy    Op = 0x100 + 0x0a
	OpMemoryFill    Op = 0x100 + 0x0b
	OpI64Add128     Op = 0x100 + 0x13
	OpI64Sub128     Op = 0x100 + 0x14
	OpI64MulWideS   Op = 0x100 + 0x15
	OpI64MulWideU   Op = 0x100 + 0x16

	// Internal forms.

	// OpCallHost calls host import A (a HostFunc).
	OpCallHost Op = 0x120
	// OpEndFunc is a function's final end: an implicit, free return.
	OpEndFunc Op = 0x121
	// OpJump is an else reached at the end of a then-arm: a free jump to A.
	OpJump Op = 0x122

	NumOps = 0x123
)

// StaticPrice is each compiled instruction's dispatch price in C (rules
// §15.1). Loads and stores are priced by memory size at run time and have 0
// here; calls to defined functions add their reservation at entry.
var StaticPrice [NumOps]uint32

func init() {
	set := func(p uint32, ops ...Op) {
		for _, o := range ops {
			StaticPrice[o] = p
		}
	}
	set(1, OpUnreachable, OpNop, OpBlock, OpBr, OpReturn, OpDrop,
		OpLocalGet, OpLocalSet, OpLocalTee, OpGlobalGet, OpGlobalSet,
		OpMemorySize, OpMemoryGrow, OpI32Const, OpI64Const,
		OpI32Add, OpI32Sub, OpI32And, OpI32Or, OpI32Xor, OpI32Shl, OpI32ShrS, OpI32ShrU,
		OpI64Add, OpI64Sub, OpI64And, OpI64Or, OpI64Xor, OpI64Shl, OpI64ShrS, OpI64ShrU,
		OpI32WrapI64, OpI64ExtendI32S, OpI64ExtendI32U,
		OpI32Extend8S, OpI32Extend16S, OpI64Extend8S, OpI64Extend16S, OpI64Extend32S)
	set(2, OpLoop, OpI32Rotl, OpI32Rotr, OpI64Rotl, OpI64Rotr, OpI64Add128, OpI64Sub128)
	set(3, OpSelect)
	set(4, OpI32Eqz, OpI64Eqz, OpI32Clz, OpI32Ctz, OpI32Popcnt, OpI64Clz, OpI64Ctz, OpI64Popcnt)
	for o := OpI32Eq; o <= OpI32GeU; o++ {
		set(4, o)
	}
	for o := OpI64Eq; o <= OpI64GeU; o++ {
		set(4, o)
	}
	set(5, OpI32Mul, OpI64Mul)
	set(8, OpI64MulWideS, OpI64MulWideU)
	set(9, OpCall)
	set(18, OpIf, OpBrIf)
	set(20, OpMemoryCopy, OpMemoryFill)
	set(33, OpI32DivS, OpI32DivU, OpI32RemS, OpI32RemU, OpI64DivS, OpI64DivU, OpI64RemS, OpI64RemU)
	set(37, OpBrTable)
	set(50, OpCallIndirect)
	set(94, OpCallHost)
	set(0, OpElse, OpEnd, OpEndFunc, OpJump)
}

// IsLoad reports whether op is a load.
func IsLoad(op Op) bool {
	return op == OpI32Load || op == OpI64Load || (op >= OpI32Load8S && op <= OpI64Load32U)
}

// IsStore reports whether op is a store.
func IsStore(op Op) bool {
	return op == OpI32Store || op == OpI64Store || (op >= OpI32Store8 && op <= OpI64Store32)
}

// memWidth is the access width in bytes of a load or store.
func memWidth(op Op) uint32 {
	switch op {
	case OpI32Load8S, OpI32Load8U, OpI64Load8S, OpI64Load8U, OpI32Store8, OpI64Store8:
		return 1
	case OpI32Load16S, OpI32Load16U, OpI64Load16S, OpI64Load16U, OpI32Store16, OpI64Store16:
		return 2
	case OpI32Load, OpI64Load32S, OpI64Load32U, OpI32Store, OpI64Store32:
		return 4
	case OpI64Load, OpI64Store:
		return 8
	}
	return 0
}

// HostFunc identifies one of the twelve host imports (rules §22).
type HostFunc uint8

const (
	HostInput HostFunc = iota
	HostContext
	HostStorageGet
	HostStorageSet
	HostStorageDelete
	HostCodeHash
	HostCall
	HostDeploy
	HostCallResult
	HostEmit
	HostReturn
	HostRevert
	NumHostFuncs
)

type hostSig struct {
	name   string
	params int
	result bool
}

// hostSigs gives each import's name and signature: all-i32 parameters and
// an optional i32 result (rules §22.2).
var hostSigs = [NumHostFuncs]hostSig{
	HostInput:         {"input", 2, true},
	HostContext:       {"context", 3, true},
	HostStorageGet:    {"storage_get", 3, true},
	HostStorageSet:    {"storage_set", 3, false},
	HostStorageDelete: {"storage_delete", 1, false},
	HostCodeHash:      {"code_hash", 3, true},
	HostCall:          {"call", 5, true},
	HostDeploy:        {"deploy", 5, true},
	HostCallResult:    {"call_result", 2, true},
	HostEmit:          {"emit", 2, false},
	HostReturn:        {"return", 2, false},
	HostRevert:        {"revert", 2, false},
}

// HostName returns the import name of f.
func (f HostFunc) String() string {
	if f < NumHostFuncs {
		return hostSigs[f].name
	}
	return "unknown"
}

// HostParams returns the number of i32 parameters of f.
func (f HostFunc) Params() int { return hostSigs[f].params }

// HostResult reports whether f returns an i32.
func (f HostFunc) HasResult() bool { return hostSigs[f].result }

func hostByName(name []byte) (HostFunc, bool) {
	for i := HostFunc(0); i < NumHostFuncs; i++ {
		if hostSigs[i].name == string(name) {
			return i, true
		}
	}
	return 0, false
}
