// Package wasmtest builds WebAssembly modules byte by byte for tests and
// tools, without an external toolchain.
package wasmtest

import (
	"github.com/ting-organization/goting/internal/wasm"
)

// Value types.
const (
	I32 = wasm.I32
	I64 = wasm.I64
)

// Type is a function signature.
type Type struct {
	Params  []byte
	Results []byte
}

// Import is a host function import.
type Import struct {
	Module string
	Name   string
	Type   uint32
}

// LocalGroup declares Count locals of one type.
type LocalGroup struct {
	Count uint32
	Type  byte
}

// Func is a defined function.
type Func struct {
	Type   uint32
	Locals []LocalGroup
	Body   []byte // instructions including the final end
}

// Global is an integer global.
type Global struct {
	Type    byte
	Mutable bool
	Init    int64
}

// Export names a function (kind 0) or memory (kind 2).
type Export struct {
	Name  string
	Kind  byte
	Index uint32
}

// Elem is an active element segment.
type Elem struct {
	Offset uint32
	Funcs  []uint32
}

// Data is an active data segment.
type Data struct {
	Offset uint32
	Bytes  []byte
}

// Limits of a table or memory.
type Limits struct {
	Min, Max uint32
}

// Module collects sections. Empty sections are omitted when encoded.
type Module struct {
	Types   []Type
	Imports []Import
	Funcs   []Func
	Table   *Limits
	Memory  *Limits
	Globals []Global
	Exports []Export
	Elems   []Elem
	Data    []Data
}

// AddType appends a type and returns its index; equal types are reused.
func (m *Module) AddType(params, results []byte) uint32 {
	for i, t := range m.Types {
		if string(t.Params) == string(params) && string(t.Results) == string(results) {
			return uint32(i)
		}
	}
	m.Types = append(m.Types, Type{Params: params, Results: results})
	return uint32(len(m.Types) - 1)
}

var hostSigs = map[string]struct {
	params int
	result bool
}{
	"input": {2, true}, "context": {3, true}, "storage_get": {3, true},
	"storage_set": {3, false}, "storage_delete": {1, false}, "code_hash": {3, true},
	"call": {5, true}, "deploy": {5, true}, "call_result": {2, true},
	"emit": {2, false}, "return": {2, false}, "revert": {2, false},
}

// Host imports a host function by name with its exact signature and returns
// its function index. Imports must be added before defined functions.
func (m *Module) Host(name string) uint32 {
	for i, im := range m.Imports {
		if im.Name == name {
			return uint32(i)
		}
	}
	sig, ok := hostSigs[name]
	if !ok {
		panic("unknown host function " + name)
	}
	params := make([]byte, sig.params)
	for i := range params {
		params[i] = I32
	}
	var results []byte
	if sig.result {
		results = []byte{I32}
	}
	t := m.AddType(params, results)
	m.Imports = append(m.Imports, Import{Module: "ting", Name: name, Type: t})
	return uint32(len(m.Imports) - 1)
}

// AddFunc appends a defined function and returns its function index.
func (m *Module) AddFunc(params, results []byte, locals []LocalGroup, body ...[]byte) uint32 {
	t := m.AddType(params, results)
	m.Funcs = append(m.Funcs, Func{Type: t, Locals: locals, Body: Code(body...)})
	return uint32(len(m.Imports) + len(m.Funcs) - 1)
}

// Invoke adds the invoke entry point with the given body and exports it,
// along with the memory when one is declared.
func (m *Module) Invoke(locals []LocalGroup, body ...[]byte) uint32 {
	f := m.AddFunc(nil, nil, locals, body...)
	m.Exports = append(m.Exports, Export{Name: "invoke", Kind: 0, Index: f})
	if m.Memory != nil {
		m.Exports = append(m.Exports, Export{Name: "memory", Kind: 2, Index: 0})
	}
	return f
}

// Bytes encodes the module.
func (m *Module) Bytes() []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	section := func(id byte, content []byte) {
		out = append(out, id)
		out = wasm.AppendU32(out, uint32(len(content)))
		out = append(out, content...)
	}
	if len(m.Types) > 0 {
		c := u32(nil, len(m.Types))
		for _, t := range m.Types {
			c = append(c, 0x60)
			c = u32(c, len(t.Params))
			c = append(c, t.Params...)
			c = u32(c, len(t.Results))
			c = append(c, t.Results...)
		}
		section(1, c)
	}
	if len(m.Imports) > 0 {
		c := u32(nil, len(m.Imports))
		for _, im := range m.Imports {
			mod := im.Module
			if mod == "" {
				mod = "ting"
			}
			c = name(c, mod)
			c = name(c, im.Name)
			c = append(c, 0x00)
			c = wasm.AppendU32(c, im.Type)
		}
		section(2, c)
	}
	if len(m.Funcs) > 0 {
		c := u32(nil, len(m.Funcs))
		for _, f := range m.Funcs {
			c = wasm.AppendU32(c, f.Type)
		}
		section(3, c)
	}
	if m.Table != nil {
		c := []byte{1, 0x70, 0x01}
		c = wasm.AppendU32(c, m.Table.Min)
		c = wasm.AppendU32(c, m.Table.Max)
		section(4, c)
	}
	if m.Memory != nil {
		c := []byte{1, 0x01}
		c = wasm.AppendU32(c, m.Memory.Min)
		c = wasm.AppendU32(c, m.Memory.Max)
		section(5, c)
	}
	if len(m.Globals) > 0 {
		c := u32(nil, len(m.Globals))
		for _, g := range m.Globals {
			c = append(c, g.Type)
			if g.Mutable {
				c = append(c, 1)
			} else {
				c = append(c, 0)
			}
			if g.Type == I32 {
				c = append(c, I32Const(int32(g.Init))...)
			} else {
				c = append(c, I64Const(g.Init)...)
			}
			c = append(c, 0x0b)
		}
		section(6, c)
	}
	if len(m.Exports) > 0 {
		c := u32(nil, len(m.Exports))
		for _, e := range m.Exports {
			c = name(c, e.Name)
			c = append(c, e.Kind)
			c = wasm.AppendU32(c, e.Index)
		}
		section(7, c)
	}
	if len(m.Elems) > 0 {
		c := u32(nil, len(m.Elems))
		for _, e := range m.Elems {
			c = append(c, 0x00)
			c = append(c, I32Const(int32(e.Offset))...)
			c = append(c, 0x0b)
			c = u32(c, len(e.Funcs))
			for _, f := range e.Funcs {
				c = wasm.AppendU32(c, f)
			}
		}
		section(9, c)
	}
	if len(m.Funcs) > 0 {
		c := u32(nil, len(m.Funcs))
		for _, f := range m.Funcs {
			b := u32(nil, len(f.Locals))
			for _, g := range f.Locals {
				b = wasm.AppendU32(b, g.Count)
				b = append(b, g.Type)
			}
			b = append(b, f.Body...)
			c = u32(c, len(b))
			c = append(c, b...)
		}
		section(10, c)
	}
	if len(m.Data) > 0 {
		c := u32(nil, len(m.Data))
		for _, d := range m.Data {
			c = append(c, 0x00)
			c = append(c, I32Const(int32(d.Offset))...)
			c = append(c, 0x0b)
			c = u32(c, len(d.Bytes))
			c = append(c, d.Bytes...)
		}
		section(11, c)
	}
	return out
}

func u32(b []byte, n int) []byte { return wasm.AppendU32(b, uint32(n)) }

func name(b []byte, s string) []byte {
	b = u32(b, len(s))
	return append(b, s...)
}

// Code concatenates instruction byte strings.
func Code(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Op encodes immediate-free single-byte instructions.
func Op(ops ...byte) []byte { return ops }

// Common opcodes.
const (
	Unreachable = 0x00
	Nop         = 0x01
	Else        = 0x05
	End         = 0x0b
	Return      = 0x0f
	Drop        = 0x1a
	Select      = 0x1b
	BlockVoid   = 0x40
)

// I32Const encodes i32.const v.
func I32Const(v int32) []byte { return wasm.AppendS64([]byte{0x41}, int64(v)) }

// I64Const encodes i64.const v.
func I64Const(v int64) []byte { return wasm.AppendS64([]byte{0x42}, v) }

func withU32(op byte, v uint32) []byte { return wasm.AppendU32([]byte{op}, v) }

func LocalGet(i uint32) []byte  { return withU32(0x20, i) }
func LocalSet(i uint32) []byte  { return withU32(0x21, i) }
func LocalTee(i uint32) []byte  { return withU32(0x22, i) }
func GlobalGet(i uint32) []byte { return withU32(0x23, i) }
func GlobalSet(i uint32) []byte { return withU32(0x24, i) }
func Call(f uint32) []byte      { return withU32(0x10, f) }
func Br(d uint32) []byte        { return withU32(0x0c, d) }
func BrIf(d uint32) []byte      { return withU32(0x0d, d) }

// CallIndirect encodes call_indirect type 0.
func CallIndirect(t uint32) []byte { return append(withU32(0x11, t), 0x00) }

// BrTable encodes br_table with targets and a default.
func BrTable(targets []uint32, def uint32) []byte {
	b := u32([]byte{0x0e}, len(targets))
	for _, t := range targets {
		b = wasm.AppendU32(b, t)
	}
	return wasm.AppendU32(b, def)
}

// Block opens a block with block type bt (0x40, I32 or I64).
func Block(bt byte) []byte { return []byte{0x02, bt} }

// Loop opens a loop.
func Loop(bt byte) []byte { return []byte{0x03, bt} }

// If opens an if.
func If(bt byte) []byte { return []byte{0x04, bt} }

// Mem encodes a load or store with alignment and offset.
func Mem(op byte, align, offset uint32) []byte {
	b := wasm.AppendU32([]byte{op}, align)
	return wasm.AppendU32(b, offset)
}

// MemorySize and MemoryGrow.
func MemorySize() []byte { return []byte{0x3f, 0x00} }
func MemoryGrow() []byte { return []byte{0x40, 0x00} }

// MemoryCopy and MemoryFill.
func MemoryCopy() []byte { return []byte{0xfc, 0x0a, 0x00, 0x00} }
func MemoryFill() []byte { return []byte{0xfc, 0x0b, 0x00} }

// Wide arithmetic.
func Add128() []byte   { return []byte{0xfc, 0x13} }
func Sub128() []byte   { return []byte{0xfc, 0x14} }
func MulWideS() []byte { return []byte{0xfc, 0x15} }
func MulWideU() []byte { return []byte{0xfc, 0x16} }

// ReturnBytes emits code that returns the len bytes at ptr via ting.return.
func ReturnBytes(retFunc uint32, ptr, n int32) []byte {
	return Code(I32Const(ptr), I32Const(n), Call(retFunc))
}
