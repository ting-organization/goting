// Package samples provides small Ting programs, assembled without an
// external toolchain, for tests, benchmarks and the devnet tool.
package samples

import (
	. "github.com/ting-organization/goting/internal/wasm/wasmtest"
)

// Counter increments a u64 at storage key 0, emits it and returns it.
func Counter() []byte {
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	get := m.Host("storage_get")
	set := m.Host("storage_set")
	emit := m.Host("emit")
	ret := m.Host("return")
	m.Invoke(nil,
		I32Const(0), I32Const(64), I32Const(8), Call(get), Op(Drop),
		I32Const(64), I32Const(64), Mem(0x29, 3, 0), I64Const(1), Op(0x7c), Mem(0x37, 3, 0),
		I32Const(0), I32Const(64), I32Const(8), Call(set),
		I32Const(64), I32Const(8), Call(emit),
		I32Const(64), I32Const(8), Call(ret),
		Op(End))
	return m.Bytes()
}

// Echo returns its input.
func Echo() []byte {
	m := &Module{Memory: &Limits{Min: 1, Max: 16}}
	input := m.Host("input")
	ret := m.Host("return")
	m.Invoke([]LocalGroup{{Count: 1, Type: I32}},
		I32Const(0), I32Const(0), Call(input), LocalSet(0),
		// grow memory to hold the input if needed: pages = len/65536 + 1
		LocalGet(0), I32Const(16), Op(0x76), I32Const(1), Op(0x6a), MemorySize(), Op(0x6b), MemoryGrow(), Op(Drop),
		I32Const(0), LocalGet(0), Call(input), Op(Drop),
		I32Const(0), LocalGet(0), Call(ret),
		Op(End))
	return m.Bytes()
}

// KeyValue stores input[32:] under the 32-byte key input[0:32]; with an
// input of exactly 32 bytes it returns the stored value, and with one byte
// it deletes key 0.
func KeyValue() []byte {
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	input := m.Host("input")
	get := m.Host("storage_get")
	set := m.Host("storage_set")
	del := m.Host("storage_delete")
	ret := m.Host("return")
	m.Invoke([]LocalGroup{{Count: 2, Type: I32}},
		I32Const(0), I32Const(60000), Call(input), LocalSet(0),
		LocalGet(0), I32Const(1), Op(0x46), If(0x40),
		I32Const(1000), Call(del), Op(Return), Op(End),
		LocalGet(0), I32Const(32), Op(0x49), If(0x40), Op(Return), Op(End), // shorter than a key
		LocalGet(0), I32Const(32), Op(0x46), If(0x40),
		I32Const(0), I32Const(100), I32Const(60000), Call(get), LocalSet(1),
		I32Const(100), LocalGet(1), I32Const(-1), Op(0x46), If(I32), I32Const(0), Op(Else), LocalGet(1), Op(End), Call(ret),
		Op(End),
		I32Const(0), I32Const(32), LocalGet(0), I32Const(32), Op(0x6b), Call(set),
		Op(End))
	return m.Bytes()
}

// Proxy calls the ContractId in input[0:32] with input[32:] as its input
// and a fixed budget of 10M C, 64 S, 1 MiB D and 64 KiB G, read-only when
// readOnly is set. It returns the child's status byte followed by its result.
func Proxy(readOnly bool) []byte {
	m := &Module{Memory: &Limits{Min: 2, Max: 2}}
	input := m.Host("input")
	call := m.Host("call")
	callResult := m.Host("call_result")
	ret := m.Host("return")
	flags := int32(0)
	if readOnly {
		flags = 1
	}
	m.Invoke([]LocalGroup{{Count: 2, Type: I32}},
		I32Const(0), I32Const(60000), Call(input), LocalSet(0),
		LocalGet(0), I32Const(32), Op(0x49), If(0x40), Op(Return), Op(End), // no target
		I32Const(60000), I64Const(10_000_000), Mem(0x37, 3, 0),
		I32Const(60008), I64Const(64), Mem(0x37, 3, 0),
		I32Const(60016), I64Const(1<<20), Mem(0x37, 3, 0),
		I32Const(60024), I64Const(1<<16), Mem(0x37, 3, 0),
		I32Const(65536),
		I32Const(0), I32Const(32), LocalGet(0), I32Const(32), Op(0x6b), I32Const(60000), I32Const(flags), Call(call),
		Mem(0x3a, 0, 0),
		I32Const(65537), I32Const(65000), Call(callResult), LocalSet(1),
		I32Const(65536), LocalGet(1), I32Const(1), Op(0x6a), Call(ret),
		Op(End))
	return m.Bytes()
}

// Factory deploys the module embedded as its data segment, passing its own
// input as the child's initial input, and returns the deploy status byte
// followed by call_result (the child's ContractId on success).
func Factory(child []byte) []byte {
	m := &Module{Memory: &Limits{Min: 2, Max: 2}}
	input := m.Host("input")
	deploy := m.Host("deploy")
	callResult := m.Host("call_result")
	ret := m.Host("return")
	m.Data = []Data{{Offset: 65536, Bytes: child}}
	m.Invoke([]LocalGroup{{Count: 2, Type: I32}},
		I32Const(1024), I32Const(30000), Call(input), LocalSet(0),
		I32Const(200), I64Const(40_000_000), Mem(0x37, 3, 0),
		I32Const(208), I64Const(100), Mem(0x37, 3, 0),
		I32Const(216), I64Const(1<<20), Mem(0x37, 3, 0),
		I32Const(224), I64Const(1<<20), Mem(0x37, 3, 0),
		I32Const(300),
		I32Const(65536), I32Const(int32(len(child))), I32Const(1024), LocalGet(0), I32Const(200), Call(deploy),
		Mem(0x3a, 0, 0),
		I32Const(301), I32Const(600), Call(callResult), LocalSet(1),
		I32Const(300), LocalGet(1), I32Const(1), Op(0x6a), Call(ret),
		Op(End))
	return m.Bytes()
}

// Failer reverts with its input when input[0] is 1, traps when it is 2,
// loops until compute runs out when it is 3, and otherwise succeeds.
func Failer() []byte {
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	input := m.Host("input")
	revert := m.Host("revert")
	emit := m.Host("emit")
	m.Invoke([]LocalGroup{{Count: 1, Type: I32}},
		I32Const(0), I32Const(1000), Call(input), LocalSet(0),
		I32Const(0), LocalGet(0), Call(emit),
		I32Const(0), Mem(0x2d, 0, 0), I32Const(1), Op(0x46), If(0x40),
		I32Const(0), LocalGet(0), Call(revert), Op(End),
		I32Const(0), Mem(0x2d, 0, 0), I32Const(2), Op(0x46), If(0x40),
		Op(Unreachable), Op(End),
		I32Const(0), Mem(0x2d, 0, 0), I32Const(3), Op(0x46), If(0x40),
		Loop(0x40), Br(0), Op(End), Op(End),
		Op(End))
	return m.Bytes()
}

// Spin runs a loop of n iterations (n = u32 at input[0:4]) doing an i64
// multiply-add, for benchmarks.
func Spin() []byte {
	m := &Module{Memory: &Limits{Min: 1, Max: 1}}
	input := m.Host("input")
	ret := m.Host("return")
	m.Invoke([]LocalGroup{{Count: 1, Type: I32}, {Count: 1, Type: I64}},
		I32Const(0), I32Const(4), Call(input), Op(Drop),
		I32Const(0), Mem(0x28, 2, 0), LocalSet(0),
		Block(0x40), Loop(0x40),
		LocalGet(0), Op(0x45), BrIf(1),
		LocalGet(1), I64Const(6364136223846793005), Op(0x7e), I64Const(1442695040888963407), Op(0x7c), LocalSet(1),
		LocalGet(0), I32Const(1), Op(0x6b), LocalSet(0),
		Br(0), Op(End), Op(End),
		I32Const(8), LocalGet(1), Mem(0x37, 3, 0),
		I32Const(8), I32Const(8), Call(ret),
		Op(End))
	return m.Bytes()
}
