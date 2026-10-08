//go:build wasm

package jit

// A WebAssembly module encoder: just enough of the binary format for one
// kernel. The module imports the Go instance's memory as env.mem and exports
// one function, f(argsPtr i32), so a kernel reads Args and the column blocks
// in place -- a Go pointer is an offset into that memory.
//
// Wasm has only structured control flow, so there are no labels here: the
// emitters nest block/loop and branch by depth.

// Wasm value types.
const (
	wI32  byte = 0x7F
	wI64  byte = 0x7E
	wV128 byte = 0x7B
)

// Wasm opcodes used by the kernels.
const (
	opBlock         byte = 0x02
	opLoop          byte = 0x03
	opEnd           byte = 0x0B
	opBr            byte = 0x0C
	opBrIf          byte = 0x0D
	opSelect        byte = 0x1B
	opLocalGet      byte = 0x20
	opLocalSet      byte = 0x21
	opI64Load       byte = 0x29
	opI64Store      byte = 0x37
	opI64Const      byte = 0x42
	opI32Const      byte = 0x41
	opI32GeU        byte = 0x4F
	opI32GtU        byte = 0x4B
	opI32And        byte = 0x71
	opI32Add        byte = 0x6A
	opI32Shl        byte = 0x74
	opI64Add        byte = 0x7C
	opI32WrapI64    byte = 0xA7
	opI64ExtendI32U byte = 0xAD
	opVoid          byte = 0x40 // empty block type
	opSIMD          byte = 0xFD
)

// SIMD128 sub-opcodes (after the 0xFD prefix, LEB128-encoded).
const (
	simdV128Load     = 0x00
	simdI64x2Splat   = 0x12
	simdI64x2Extract = 0x1D
	simdV128And      = 0x4E
	simdI64x2Add     = 0xCE
	simdI64x2Sub     = 0xD1
	simdI64x2Eq      = 0xD6
	simdI64x2Ne      = 0xD7
	simdI64x2LtS     = 0xD8
	simdI64x2GtS     = 0xD9
	simdI64x2LeS     = 0xDA
	simdI64x2GeS     = 0xDB
)

// Wasm is a function body under construction plus its locals.
type Wasm struct {
	locals []byte // one value type per local after the parameter
	body   []byte
}

func uleb(b []byte, v uint64) []byte {
	for {
		c := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
			continue
		}
		return append(b, c)
	}
}

func sleb(b []byte, v int64) []byte {
	for {
		c := byte(v & 0x7F)
		v >>= 7
		if (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0) {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// Local declares a local of type t and returns its index (0 is argsPtr).
func (w *Wasm) Local(t byte) uint32 {
	w.locals = append(w.locals, t)
	return uint32(len(w.locals))
}

func (w *Wasm) op(b ...byte)      { w.body = append(w.body, b...) }
func (w *Wasm) Get(l uint32)      { w.op(opLocalGet); w.body = uleb(w.body, uint64(l)) }
func (w *Wasm) Set(l uint32)      { w.op(opLocalSet); w.body = uleb(w.body, uint64(l)) }
func (w *Wasm) I32(v int32)       { w.op(opI32Const); w.body = sleb(w.body, int64(v)) }
func (w *Wasm) I64(v int64)       { w.op(opI64Const); w.body = sleb(w.body, v) }
func (w *Wasm) Br(depth uint32)   { w.op(opBr); w.body = uleb(w.body, uint64(depth)) }

// BrTable pops an i32 and branches to depths[it], or to def when it is out of
// range.
func (w *Wasm) BrTable(depths []uint32, def uint32) {
	w.op(0x0E)
	w.body = uleb(w.body, uint64(len(depths)))
	for _, d := range depths {
		w.body = uleb(w.body, uint64(d))
	}
	w.body = uleb(w.body, uint64(def))
}
func (w *Wasm) BrIf(depth uint32) { w.op(opBrIf); w.body = uleb(w.body, uint64(depth)) }
func (w *Wasm) memarg(align, off uint32) {
	w.body = uleb(w.body, uint64(align))
	w.body = uleb(w.body, uint64(off))
}

// LoadI64 replaces the i32 address on the stack with the int64 at addr+off.
func (w *Wasm) LoadI64(off uint32)  { w.op(opI64Load); w.memarg(3, off) }
func (w *Wasm) StoreI64(off uint32) { w.op(opI64Store); w.memarg(3, off) }

// Simd emits one SIMD128 instruction.
func (w *Wasm) Simd(sub uint32) { w.op(opSIMD); w.body = uleb(w.body, uint64(sub)) }

// LoadV128 replaces the i32 address on the stack with the 16 bytes there.
func (w *Wasm) LoadV128() { w.LoadV128At(0) }

// LoadV128At loads the 16 bytes at the stack address plus off.
func (w *Wasm) LoadV128At(off uint32) { w.Simd(simdV128Load); w.memarg(3, off) }

// ExtractLane pushes lane i of the i64x2 on the stack.
func (w *Wasm) ExtractLane(i byte) { w.Simd(simdI64x2Extract); w.op(i) }

// LoadPtr pushes the Go pointer at args+off as an i32 address. Go stores
// pointers as 8 bytes on wasm, but a wasm32 memory never exceeds 4 GiB.
func (w *Wasm) LoadPtr(off uint32) {
	w.Get(0)
	w.LoadI64(off)
	w.op(opI32WrapI64)
}

// Module wraps the body into a complete module: type (i32)->(), import
// env.mem, function 0, export "f".
func (w *Wasm) Module() []byte {
	section := func(m []byte, id byte, payload []byte) []byte {
		m = append(m, id)
		m = uleb(m, uint64(len(payload)))
		return append(m, payload...)
	}
	m := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}
	m = section(m, 1, []byte{1, 0x60, 1, wI32, 0})                                     // (i32) -> ()
	m = section(m, 2, []byte{1, 3, 'e', 'n', 'v', 3, 'm', 'e', 'm', 0x02, 0x00, 0x00}) // memory, min 0
	m = section(m, 3, []byte{1, 0})
	m = section(m, 7, []byte{1, 1, 'f', 0x00, 0})

	// Locals as one (count, type) run per local: simplest valid encoding.
	fn := uleb(nil, uint64(len(w.locals)))
	for _, t := range w.locals {
		fn = append(fn, 1, t)
	}
	fn = append(fn, w.body...)
	fn = append(fn, opEnd)
	code := uleb([]byte{1}, uint64(len(fn)))
	code = append(code, fn...)
	return section(m, 10, code)
}
