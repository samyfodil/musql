//go:build wasm

package jit

import "fmt"

// The vector-search kernels as WebAssembly SIMD128, the twins of the AVX2
// ones in vector_avx2_amd64.go: same Args, same answers to the bit. A lane
// holds one ROW (f32x4: four rows), each row's sums are accumulated in
// component order, and products and sums are separate f32x4.mul and
// f32x4.add -- SIMD128 has no fused multiply-add (only relaxed SIMD does),
// so every step rounds as libSQL's float32 loop rounds.

// More SIMD128 sub-opcodes.
const (
	simdV128Store      = 0x0B
	simdV128Const      = 0x0C
	simdI8x16Shuffle   = 0x0D
	simdI32x4Splat     = 0x11
	simdF32x4Splat     = 0x13
	simdI32x4Extract   = 0x1B
	simdI8x16NarrowS   = 0x65
	simdF32x4Nearest   = 0x6A
	simdI16x8NarrowS   = 0x85
	simdI16x8ExtLowS   = 0x87
	simdI16x8ExtHighS  = 0x88
	simdI32x4Abs       = 0xA0
	simdI32x4Add       = 0xAE
	simdI32x4MaxS      = 0xB8
	simdI32x4DotI16x8S = 0xBA
	simdF32x4Add       = 0xE4
	simdF32x4Sub       = 0xE5
	simdF32x4Mul       = 0xE6
	simdI32x4TruncSatS = 0xF8
)

// More scalar opcodes.
const (
	opI32Load  byte = 0x28
	opF32Load  byte = 0x2A
	opI32Store byte = 0x36
	opI32Sub   byte = 0x6B
	opI32Mul   byte = 0x6C
	opI32GtS   byte = 0x4A
)

func (w *Wasm) shuffle(lanes [16]byte) { w.Simd(simdI8x16Shuffle); w.op(lanes[:]...) }
func (w *Wasm) zeroV128()              { w.Simd(simdV128Const); w.op(make([]byte, 16)...) }
func (w *Wasm) storeV128(off uint32)   { w.Simd(simdV128Store); w.memarg(4, off) }

// add pushes local a + i32 b (a constant).
func (w *Wasm) addI32(l uint32, d int32) { w.Get(l); w.I32(d); w.op(opI32Add) }

// bump advances local l by d.
func (w *Wasm) bump(l uint32, d int32) { w.addI32(l, d); w.Set(l) }

// bumpBy advances local l by local by.
func (w *Wasm) bumpBy(l, by uint32) { w.Get(l); w.Get(by); w.op(opI32Add); w.Set(l) }

// loopWhile opens block+loop; the body ends with closeLoop. cond leaves an
// i32 that is non-zero to leave the loop, evaluated at the top.
func (w *Wasm) loopUntil(exit func()) {
	w.op(opBlock, opVoid, opLoop, opVoid)
	exit()
	w.BrIf(1)
}
func (w *Wasm) closeLoop() { w.Br(0); w.op(opEnd, opEnd) }

// Transposing four rows' four components: unpack pairs, then combine halves.
var (
	shufLoPairs = [16]byte{0, 1, 2, 3, 16, 17, 18, 19, 4, 5, 6, 7, 20, 21, 22, 23}
	shufHiPairs = [16]byte{8, 9, 10, 11, 24, 25, 26, 27, 12, 13, 14, 15, 28, 29, 30, 31}
	shufLoHalf  = [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 16, 17, 18, 19, 20, 21, 22, 23}
	shufHiHalf  = [16]byte{8, 9, 10, 11, 12, 13, 14, 15, 24, 25, 26, 27, 28, 29, 30, 31}
)

// EmitVecDistStrided is the strided distance kernel (see the AVX2 twin): rows
// back to back at stride XC, eight rows per iteration as two tiles of four,
// each tile's 4x4 component blocks transposed in registers so a lane is a row.
// Args as on amd64; XA a multiple of 8, N a multiple of 8.
func EmitVecDistStrided(l2 bool) ([]byte, error) {
	var w Wasm
	p, stride, n, blocks, out, out2, qbase := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	off, qp, j := w.Local(wI32), w.Local(wI32), w.Local(wI32)
	var rowp [8]uint32
	for i := range rowp {
		rowp[i] = w.Local(wI32)
	}
	var r [4]uint32
	for i := range r {
		r[i] = w.Local(wV128)
	}
	ta, tb, tc, td := w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128)
	var col [4]uint32
	for i := range col {
		col[i] = w.Local(wV128)
	}
	qv := w.Local(wV128)
	sum := [2]uint32{w.Local(wV128), w.Local(wV128)}
	nrm := [2]uint32{w.Local(wV128), w.Local(wV128)}

	w.LoadPtr(OffA)
	w.Set(p)
	w.Get(0)
	w.LoadI64(OffXC)
	w.op(opI32WrapI64)
	w.Set(stride)
	w.Get(0)
	w.LoadI64(OffN)
	w.op(opI32WrapI64)
	w.Set(n)
	w.Get(0)
	w.LoadI64(OffXA)
	w.op(opI32WrapI64)
	w.I32(2)
	w.op(0x76) // i32.shr_u: blocks of four components
	w.Set(blocks)
	w.LoadPtr(OffOut)
	w.Set(out)
	w.LoadPtr(OffOut2)
	w.Set(out2)
	w.LoadPtr(OffV)
	w.Set(qbase)

	w.loopUntil(func() { w.Get(n); w.op(opI32Eqz) })
	// The eight rows' addresses.
	for i := range rowp {
		w.Get(p)
		w.Get(stride)
		w.I32(int32(i))
		w.op(opI32Mul)
		w.op(opI32Add)
		w.Set(rowp[i])
	}
	for t := range 2 {
		w.zeroV128()
		w.Set(sum[t])
		w.zeroV128()
		w.Set(nrm[t])
	}
	w.I32(0)
	w.Set(off)
	w.Get(qbase)
	w.Set(qp)
	w.Get(blocks)
	w.Set(j)
	w.loopUntil(func() { w.Get(j); w.op(opI32Eqz) })
	for t := range 2 {
		for i := range 4 {
			w.Get(rowp[4*t+i])
			w.Get(off)
			w.op(opI32Add)
			w.LoadV128()
			w.Set(r[i])
		}
		pair := func(x, y uint32, lanes [16]byte, into uint32) {
			w.Get(x)
			w.Get(y)
			w.shuffle(lanes)
			w.Set(into)
		}
		pair(r[0], r[1], shufLoPairs, ta) // r0c0 r1c0 r0c1 r1c1
		pair(r[0], r[1], shufHiPairs, tb) // r0c2 r1c2 r0c3 r1c3
		pair(r[2], r[3], shufLoPairs, tc)
		pair(r[2], r[3], shufHiPairs, td)
		pair(ta, tc, shufLoHalf, col[0])
		pair(ta, tc, shufHiHalf, col[1])
		pair(tb, td, shufLoHalf, col[2])
		pair(tb, td, shufHiHalf, col[3])
		for k := range 4 {
			w.Get(qp)
			w.op(opF32Load)
			w.memarg(2, uint32(4*k))
			w.Simd(simdF32x4Splat)
			w.Set(qv)
			if l2 {
				w.Get(sum[t])
				w.Get(col[k])
				w.Get(qv)
				w.Simd(simdF32x4Sub)
				w.Set(ta) // reuse as the difference
				w.Get(ta)
				w.Get(ta)
				w.Simd(simdF32x4Mul)
				w.Simd(simdF32x4Add)
				w.Set(sum[t])
			} else {
				w.Get(sum[t])
				w.Get(col[k])
				w.Get(qv)
				w.Simd(simdF32x4Mul)
				w.Simd(simdF32x4Add)
				w.Set(sum[t])
				w.Get(nrm[t])
				w.Get(col[k])
				w.Get(col[k])
				w.Simd(simdF32x4Mul)
				w.Simd(simdF32x4Add)
				w.Set(nrm[t])
			}
		}
	}
	w.bump(off, 16)
	w.bump(qp, 16)
	w.bump(j, -1)
	w.closeLoop()
	for t := range 2 {
		w.Get(out)
		w.Get(sum[t])
		w.storeV128(uint32(16 * t))
		if !l2 {
			w.Get(out2)
			w.Get(nrm[t])
			w.storeV128(uint32(16 * t))
		}
	}
	w.bump(out, 32)
	w.bump(out2, 32)
	for range 8 {
		w.bumpBy(p, stride)
	}
	w.bump(n, -8)
	w.closeLoop()
	return w.Module(), nil
}

// EmitVecDist has no wasm twin: SIMD128 has no gather. The engine's Go tiles
// take rows that are not stored back to back.
func EmitVecDist(l2 bool) ([]byte, error) {
	return nil, fmt.Errorf("jit: no gathering vector kernel in wasm")
}

// rowLoop emits the shared frame of the per-row kernels: p the row, stride,
// n the rows, blocks the per-row block count (XA >> shift); body runs once
// per row, and the frame advances p by stride.
func (w *Wasm) rowLoop(shift int32, body func(p, blocks uint32)) {
	p, stride, n, blocks := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	w.LoadPtr(OffA)
	w.Set(p)
	w.Get(0)
	w.LoadI64(OffXC)
	w.op(opI32WrapI64)
	w.Set(stride)
	w.Get(0)
	w.LoadI64(OffN)
	w.op(opI32WrapI64)
	w.Set(n)
	w.Get(0)
	w.LoadI64(OffXA)
	w.op(opI32WrapI64)
	w.I32(shift)
	w.op(0x76) // i32.shr_u
	w.Set(blocks)
	w.loopUntil(func() { w.Get(n); w.op(opI32Eqz) })
	body(p, blocks)
	w.bumpBy(p, stride)
	w.bump(n, -1)
	w.closeLoop()
}

// sumLanes pushes the four i32 lanes of v added together.
func (w *Wasm) sumLanes(v uint32) {
	for i := range 4 {
		w.Get(v)
		w.Simd(simdI32x4Extract)
		w.op(byte(i))
		if i > 0 {
			w.op(opI32Add)
		}
	}
}

// EmitI8Dot is the int8 dot-product kernel: for each row of codes, the exact
// int32 sum of code[i]*query[i], sixteen codes per step widened to int16 and
// multiplied and pair-summed by i32x4.dot_i16x8_s. Args as on amd64, with XC
// unused (rows are XA bytes apart); XA a multiple of 16.
func EmitI8Dot() ([]byte, error) {
	var w Wasm
	q, qp, o, j := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	cur := w.Local(wI32)
	v, acc := w.Local(wV128), w.Local(wV128)
	w.LoadPtr(OffC)
	w.Set(q)
	w.LoadPtr(OffOut)
	w.Set(o)
	// Rows are back to back: stride = XA, which rowLoop reads from XC, so
	// keep XC out of it by walking cur ourselves.
	w.LoadPtr(OffA)
	w.Set(cur)
	n, blocks := w.Local(wI32), w.Local(wI32)
	w.Get(0)
	w.LoadI64(OffN)
	w.op(opI32WrapI64)
	w.Set(n)
	w.Get(0)
	w.LoadI64(OffXA)
	w.op(opI32WrapI64)
	w.I32(4)
	w.op(0x76)
	w.Set(blocks)
	w.loopUntil(func() { w.Get(n); w.op(opI32Eqz) })
	w.zeroV128()
	w.Set(acc)
	w.Get(q)
	w.Set(qp)
	w.Get(blocks)
	w.Set(j)
	w.loopUntil(func() { w.Get(j); w.op(opI32Eqz) })
	w.Get(cur)
	w.LoadV128()
	w.Set(v)
	for half, ext := range []uint32{simdI16x8ExtLowS, simdI16x8ExtHighS} {
		w.Get(acc)
		w.Get(v)
		w.Simd(ext)
		w.Get(qp)
		w.LoadV128At(uint32(16 * half))
		w.Simd(simdI32x4DotI16x8S)
		w.Simd(simdI32x4Add)
		w.Set(acc)
	}
	w.bump(cur, 16)
	w.bump(qp, 32)
	w.bump(j, -1)
	w.closeLoop()
	w.Get(o)
	w.sumLanes(acc)
	w.op(opI32Store)
	w.memarg(2, 0)
	w.bump(o, 4)
	w.bump(n, -1)
	w.closeLoop()
	return w.Module(), nil
}

// EmitMaxAbsBits is each row's largest |component| as float32 bits, an
// integer max over the bits with the sign masked (see the AVX2 twin). Args as
// on amd64; XA a multiple of 4.
func EmitMaxAbsBits() ([]byte, error) {
	var w Wasm
	o, j, cur := w.Local(wI32), w.Local(wI32), w.Local(wI32)
	m, mask, best := w.Local(wV128), w.Local(wV128), w.Local(wI32)
	lane := w.Local(wI32)
	w.LoadPtr(OffOut)
	w.Set(o)
	w.I32(0x7fffffff)
	w.Simd(simdI32x4Splat)
	w.Set(mask)
	w.rowLoop(2, func(p, blocks uint32) {
		w.zeroV128()
		w.Set(m)
		w.Get(p)
		w.Set(cur)
		w.Get(blocks)
		w.Set(j)
		w.loopUntil(func() { w.Get(j); w.op(opI32Eqz) })
		w.Get(m)
		w.Get(cur)
		w.LoadV128()
		w.Get(mask)
		w.Simd(simdV128And)
		w.Simd(simdI32x4MaxS)
		w.Set(m)
		w.bump(cur, 16)
		w.bump(j, -1)
		w.closeLoop()
		w.I32(0)
		w.Set(best)
		for i := range 4 { // best = max(best, lane i), signed: the bits are non-negative
			w.Get(m)
			w.Simd(simdI32x4Extract)
			w.op(byte(i))
			w.Set(lane)
			w.Get(lane)
			w.Get(best)
			w.Get(lane)
			w.Get(best)
			w.op(opI32GtS)
			w.op(opSelect)
			w.Set(best)
		}
		w.Get(o)
		w.Get(best)
		w.op(opI32Store)
		w.memarg(2, 0)
		w.bump(o, 4)
	})
	return w.Module(), nil
}

// EmitQuantize is each row's codes, round-to-nearest(x * 1/scale) packed to
// int8, and its sum of |code| (see the AVX2 twin). Args as on amd64; XA a
// multiple of 8.
func EmitQuantize() ([]byte, error) {
	var w Wasm
	o, o2, inv, j, cur := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	s, a, b, c1 := w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128)
	w.LoadPtr(OffOut)
	w.Set(o)
	w.LoadPtr(OffOut2)
	w.Set(o2)
	w.LoadPtr(OffV)
	w.Set(inv)
	w.rowLoop(3, func(p, blocks uint32) {
		w.Get(inv)
		w.op(opF32Load)
		w.memarg(2, 0)
		w.Simd(simdF32x4Splat)
		w.Set(s)
		w.zeroV128()
		w.Set(c1)
		w.Get(p)
		w.Set(cur)
		w.Get(blocks)
		w.Set(j)
		w.loopUntil(func() { w.Get(j); w.op(opI32Eqz) })
		for i, into := range []uint32{a, b} {
			w.Get(cur)
			w.LoadV128At(uint32(16 * i))
			w.Get(s)
			w.Simd(simdF32x4Mul)
			w.Simd(simdF32x4Nearest)
			w.Simd(simdI32x4TruncSatS)
			w.Set(into)
			w.Get(c1)
			w.Get(into)
			w.Simd(simdI32x4Abs)
			w.Simd(simdI32x4Add)
			w.Set(c1)
		}
		// Eight codes: the low 8 bytes of the twice-narrowed pair.
		w.Get(o)
		w.Get(a)
		w.Get(b)
		w.Simd(simdI16x8NarrowS)
		w.Set(a)
		w.Get(a)
		w.Get(a)
		w.Simd(simdI8x16NarrowS)
		w.ExtractLane(0)
		w.StoreI64Align(0, 0)
		w.bump(o, 8)
		w.bump(cur, 32)
		w.bump(j, -1)
		w.closeLoop()
		w.Get(o2)
		w.sumLanes(c1)
		w.op(opI32Store)
		w.memarg(2, 0)
		w.bump(o2, 4)
		w.bump(inv, 4)
	})
	return w.Module(), nil
}

// StoreI64Align stores the i64 on the stack at addr+off with alignment hint
// 2^align: the codes' rows are only byte-aligned.
func (w *Wasm) StoreI64Align(align, off uint32) { w.op(opI64Store); w.memarg(align, off) }
