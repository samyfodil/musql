//go:build wasm

package jit

// The vec1 kernels as WebAssembly SIMD128, the twins of vec1_avx2_amd64.go:
// the same Args and the same answers to the bit. A lane holds a row (f32x4:
// four rows, f64x2: two), a row's sums are accumulated in component order,
// and products and sums are separate operations (SIMD128 has no fused
// multiply-add). The loops are plainer than the native kernels': a host's
// engine schedules them.

// SIMD128 and scalar opcodes the vec1 kernels add.
const (
	simdV128Load32Splat = 0x09
	simdF64x2Splat      = 0x14
	simdF32x4Extract    = 0x1F
	simdF32x4Replace    = 0x20
	simdF32x4Le         = 0x45
	simdF64x2Lt         = 0x49
	simdV128Bitselect   = 0x52
	simdF32x4DemoteZero = 0x5E
	simdF64x2PromoteLow = 0x5F
	simdF32x4Pmin       = 0xEA
	simdF64x2Add        = 0xF0
	simdF64x2Sub        = 0xF1
	simdF64x2Mul        = 0xF2

	opF32Store  byte = 0x38
	opI32Store8 byte = 0x3A
	opF32Add    byte = 0x92
	opF32Mul    byte = 0x94
	opF32Min    byte = 0x96
)

// High two float32 lanes moved low, for f64x2.promote_low.
var shufHi64 = [16]byte{8, 9, 10, 11, 12, 13, 14, 15, 8, 9, 10, 11, 12, 13, 14, 15}

// argI32 sets local l to the int64 Args field at off, as an i32.
func (w *Wasm) argI32(off, l uint32) { w.Get(0); w.LoadI64(off); w.op(opI32WrapI64); w.Set(l) }

// ptrArg sets local l to the pointer Args field at off.
func (w *Wasm) ptrArg(off, l uint32) { w.LoadPtr(off); w.Set(l) }

// v128At pushes the 16 bytes at local addr + off.
func (w *Wasm) v128At(addr, off uint32) { w.Get(addr); w.LoadV128At(off) }

// storeV128At stores local v at local addr + off.
func (w *Wasm) storeV128At(addr, off, v uint32) { w.Get(addr); w.Get(v); w.storeV128(off) }

// splatF32At pushes the float32 at local addr in all four lanes.
func (w *Wasm) splatF32At(addr uint32) { w.Get(addr); w.Simd(simdV128Load32Splat); w.memarg(2, 0) }

// zeroLocal sets v128 local l to zero.
func (w *Wasm) zeroLocal(l uint32) { w.zeroV128(); w.Set(l) }

// count runs body n times, n a local it counts down.
func (w *Wasm) count(n uint32, body func()) {
	w.loopUntil(func() { w.Get(n); w.op(opI32Eqz) })
	body()
	w.bump(n, -1)
	w.closeLoop()
}

// acc4 is  acc = acc + x * y  (f32x4, multiply then add).
func (w *Wasm) acc4(acc uint32, x, y func()) {
	w.Get(acc)
	x()
	y()
	w.Simd(simdF32x4Mul)
	w.Simd(simdF32x4Add)
	w.Set(acc)
}

// sq2 is  acc = acc + e * e  (f64x2) for the f64x2 in local e.
func (w *Wasm) sq2(acc, e uint32) {
	w.Get(acc)
	w.Get(e)
	w.Get(e)
	w.Simd(simdF64x2Mul)
	w.Simd(simdF64x2Add)
	w.Set(acc)
}

// widen sets lo and hi to the two f64x2 halves of f32x4 local v.
func (w *Wasm) widen(v, lo, hi uint32) {
	w.Get(v)
	w.Simd(simdF64x2PromoteLow)
	w.Set(lo)
	w.Get(v)
	w.Get(v)
	w.shuffle(shufHi64)
	w.Simd(simdF64x2PromoteLow)
	w.Set(hi)
}

// EmitVec1T8 is the twin of the AVX2 EmitVec1T8: the same Args (N groups of
// four blocks, two for Vec1L2D).
func EmitVec1T8(op Vec1Op) ([]byte, error) {
	var w Wasm
	p, blk, n, nb, out, q, c, d := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	qs, lo, hi, e := w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128)
	var acc [4]uint32
	for i := range acc {
		acc[i] = w.Local(wV128)
	}
	t0, t1 := w.Local(wV128), w.Local(wV128)
	w.ptrArg(OffA, p)
	w.argI32(OffXC, blk)
	w.argI32(OffXA, n)
	w.argI32(OffN, nb)
	group := int32(4)
	if op == Vec1L2D {
		group = 2
	}
	w.Get(nb)
	w.I32(group)
	w.op(opI32Mul)
	w.Set(nb)
	w.ptrArg(OffOut, out)
	w.count(nb, func() {
		for i := range acc {
			w.zeroLocal(acc[i])
		}
		w.ptrArg(OffV, q)
		w.Get(p)
		w.Set(c)
		w.Get(n)
		w.Set(d)
		w.count(d, func() {
			w.splatF32At(q)
			w.Set(qs)
			w.v128At(c, 0)
			w.Set(lo)
			w.v128At(c, 16)
			w.Set(hi)
			for i, v := range []uint32{lo, hi} {
				switch op {
				case Vec1L2F:
					w.Get(qs)
					w.Get(v)
					w.Simd(simdF32x4Sub)
					w.Set(e)
					w.acc4(acc[i], func() { w.Get(e) }, func() { w.Get(e) })
				case Vec1DotF:
					w.acc4(acc[i], func() { w.Get(v) }, func() { w.Get(qs) })
				case Vec1L2D:
					w.Get(qs)
					w.Get(v)
					w.Simd(simdF32x4Sub)
					w.Set(e)
					w.widen(e, t0, t1)
					w.sq2(acc[2*i], t0)
					w.sq2(acc[2*i+1], t1)
				}
			}
			w.bump(c, 32)
			w.bump(q, 4)
		})
		k := 2
		if op == Vec1L2D {
			k = 4
		}
		for i := range k {
			w.storeV128At(out, uint32(16*i), acc[i])
		}
		w.bump(out, int32(16*k))
		w.bumpBy(p, blk)
	})
	return w.Module(), nil
}

// EmitVec1Scan16 is the twin of the AVX2 EmitVec1Scan16: each code's table
// entry is loaded into its lane, the sixteen sums added a book at a time.
func EmitVec1Scan16() ([]byte, error) {
	var w Wasm
	p, sz, nblk, nbook, lut, out, c, t, b := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	var acc [4]uint32
	for i := range acc {
		acc[i] = w.Local(wV128)
	}
	v := w.Local(wV128)
	w.ptrArg(OffA, p)
	w.argI32(OffXC, sz)
	w.argI32(OffN, nblk)
	w.argI32(OffXA, nbook)
	w.ptrArg(OffV, lut)
	w.ptrArg(OffOut, out)
	entry := func(j int) { // the table entry of code j, as an f32
		w.Get(t)
		w.Get(c)
		w.op(opI32Load8U)
		w.memarg(0, uint32(j))
		w.I32(2)
		w.op(opI32Shl)
		w.op(opI32Add)
		w.op(opF32Load)
		w.memarg(2, 0)
	}
	w.count(nblk, func() {
		for i := range acc {
			w.zeroLocal(acc[i])
		}
		w.Get(p)
		w.Set(c)
		w.Get(lut)
		w.Set(t)
		w.Get(nbook)
		w.Set(b)
		w.count(b, func() {
			for r := range 4 {
				entry(4 * r)
				w.Simd(simdF32x4Splat)
				for l := 1; l < 4; l++ {
					entry(4*r + l)
					w.Simd(simdF32x4Replace)
					w.op(byte(l))
				}
				w.Set(v)
				w.Get(acc[r])
				w.Get(v)
				w.Simd(simdF32x4Add)
				w.Set(acc[r])
			}
			w.bump(c, 16)
			w.bump(t, 1024)
		})
		for r := range acc {
			w.storeV128At(out, uint32(16*r), acc[r])
		}
		w.bump(out, 64)
		w.bumpBy(p, sz)
	})
	return w.Module(), nil
}

// EmitVec1RowsL2D is the twin of the AVX2 EmitVec1RowsL2D, four rows at a
// time, each lane loaded from its row.
func EmitVec1RowsL2D() ([]byte, error) {
	var w Wasm
	p, stride, nrow, n, out, q, d := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	var r [4]uint32
	for i := range r {
		r[i] = w.Local(wI32)
	}
	v, e, t0, t1, a0, a1 := w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128)
	w.ptrArg(OffA, p)
	w.argI32(OffXC, stride)
	w.argI32(OffN, nrow)
	w.Get(nrow)
	w.I32(2)
	w.op(0x76) // i32.shr_u: tiles of four
	w.Set(nrow)
	w.argI32(OffXA, n)
	w.ptrArg(OffOut, out)
	w.count(nrow, func() {
		for i := range r {
			w.Get(p)
			w.Get(stride)
			w.I32(int32(i))
			w.op(opI32Mul)
			w.op(opI32Add)
			w.Set(r[i])
		}
		w.zeroLocal(a0)
		w.zeroLocal(a1)
		w.ptrArg(OffV, q)
		w.Get(n)
		w.Set(d)
		w.count(d, func() {
			w.Get(r[0])
			w.op(opF32Load)
			w.memarg(2, 0)
			w.Simd(simdF32x4Splat)
			for l := 1; l < 4; l++ {
				w.Get(r[l])
				w.op(opF32Load)
				w.memarg(2, 0)
				w.Simd(simdF32x4Replace)
				w.op(byte(l))
			}
			w.Set(v)
			w.splatF32At(q)
			w.Get(v)
			w.Simd(simdF32x4Sub)
			w.Set(e)
			w.widen(e, t0, t1)
			w.sq2(a0, t0)
			w.sq2(a1, t1)
			for i := range r {
				w.bump(r[i], 4)
			}
			w.bump(q, 4)
		})
		w.storeV128At(out, 0, a0)
		w.storeV128At(out, 16, a1)
		w.bump(out, 32)
		for range 4 {
			w.bumpBy(p, stride)
		}
	})
	return w.Module(), nil
}

// EmitVec1BestD is the twin of the AVX2 EmitVec1BestD.
func EmitVec1BestD(round bool) ([]byte, error) {
	var w Wasm
	p, blk, nb, n, tab, q, c, d := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	qs, e, t0, t1, mask, step := w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128)
	var acc, best, rows, cur [4]uint32
	for i := range 4 {
		acc[i], best[i], rows[i], cur[i] = w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128)
	}
	w.ptrArg(OffA, p)
	w.argI32(OffXC, blk)
	w.argI32(OffN, nb)
	w.Get(nb)
	w.I32(1)
	w.op(opI32Shl) // pairs to blocks
	w.Set(nb)
	w.argI32(OffXA, n)
	w.ptrArg(OffC, tab)
	for i := range 4 {
		w.v128At(tab, uint32(16*i))
		w.Set(cur[i])
		w.v128At(tab, 96)
		w.Set(best[i])
		w.zeroLocal(rows[i])
	}
	w.v128At(tab, 64)
	w.Set(step)
	w.count(nb, func() {
		for i := range acc {
			w.zeroLocal(acc[i])
		}
		w.ptrArg(OffV, q)
		w.Get(p)
		w.Set(c)
		w.Get(n)
		w.Set(d)
		w.count(d, func() {
			w.splatF32At(q)
			w.Set(qs)
			for h := range 2 {
				w.Get(qs)
				w.v128At(c, uint32(16*h))
				w.Simd(simdF32x4Sub)
				w.Set(e)
				w.widen(e, t0, t1)
				w.sq2(acc[2*h], t0)
				w.sq2(acc[2*h+1], t1)
			}
			w.bump(c, 32)
			w.bump(q, 4)
		})
		for i := range 4 {
			if round {
				w.Get(acc[i])
				w.Simd(simdF32x4DemoteZero)
				w.Simd(simdF64x2PromoteLow)
				w.Set(acc[i])
			}
			w.Get(acc[i])
			w.Get(best[i])
			w.Simd(simdF64x2Lt)
			w.Set(mask)
			w.Get(acc[i])
			w.Get(best[i])
			w.Get(mask)
			w.Simd(simdV128Bitselect)
			w.Set(best[i])
			w.Get(cur[i])
			w.Get(rows[i])
			w.Get(mask)
			w.Simd(simdV128Bitselect)
			w.Set(rows[i])
			w.Get(cur[i])
			w.Get(step)
			w.Simd(simdF64x2Add)
			w.Set(cur[i])
		}
		w.bumpBy(p, blk)
	})
	w.ptrArg(OffOut, c)
	w.ptrArg(OffOut2, d)
	for i := range 4 {
		w.storeV128At(c, uint32(16*i), best[i])
		w.storeV128At(d, uint32(16*i), rows[i])
	}
	return w.Module(), nil
}

// candSet emits one set's estimates and bit sets for one group of four
// vectors: set the rows (B blocks), vec the first vector (stride locals),
// a the vectors' A (32 bytes each), out the record, est scratch for B*128
// bytes, n the components, wts and minusTwo the scratch's constants, f F.
func (w *Wasm) candSet(set, vec, stride, a, out, est, n, B uint32, wts [2]uint32, minusTwo, f uint32) {
	blkp, vp, e, d, k, cnt, o := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	lo, hi, s, bound, m0, m1 := w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128)
	var mlo, mhi [4]uint32
	for vv := range 4 {
		mlo[vv], mhi[vv] = w.Local(wV128), w.Local(wV128)
		for _, m := range []uint32{mlo[vv], mhi[vv]} {
			w.I32(0x7F800000)
			w.Simd(simdI32x4Splat)
			w.Set(m)
		}
	}
	w.Get(set)
	w.Set(blkp)
	w.Get(est)
	w.Set(e)
	w.Get(B)
	w.Set(cnt)
	w.count(cnt, func() {
		for vv := range 4 {
			w.zeroLocal(lo)
			w.zeroLocal(hi)
			w.Get(vec)
			w.Get(stride)
			w.I32(int32(vv))
			w.op(opI32Mul)
			w.op(opI32Add)
			w.Set(vp)
			w.Get(blkp)
			w.Set(k)
			w.Get(n)
			w.Set(d)
			w.count(d, func() {
				w.splatF32At(vp)
				w.Set(s)
				w.acc4(lo, func() { w.v128At(k, 0) }, func() { w.Get(s) })
				w.acc4(hi, func() { w.v128At(k, 16) }, func() { w.Get(s) })
				w.bump(k, 32)
				w.bump(vp, 4)
			})
			// k is at the block's norms: est = sum * -2 + norm.
			for h, x := range []uint32{lo, hi} {
				w.Get(x)
				w.Get(minusTwo)
				w.Simd(simdF32x4Mul)
				w.v128At(k, uint32(16*h))
				w.Simd(simdF32x4Add)
				w.Set(x)
				w.storeV128At(e, uint32(32*vv+16*h), x)
			}
			// pmin(m, x) is x < m ? x : m: a NaN estimate leaves the minimum be.
			for _, pr := range [][2]uint32{{mlo[vv], lo}, {mhi[vv], hi}} {
				w.Get(pr[0])
				w.Get(pr[1])
				w.Simd(simdF32x4Pmin)
				w.Set(pr[0])
			}
		}
		w.Get(k)
		w.I32(32)
		w.op(opI32Add)
		w.Set(blkp) // past the norms: the next block
		w.bump(e, 128)
	})
	w.Get(blkp)
	w.Set(set) // the caller's next set
	w.Get(out)
	w.Set(o)
	for vv := range 4 {
		w.Get(mlo[vv])
		w.Get(mhi[vv])
		w.Simd(simdF32x4Pmin)
		w.Set(s)
		w.Get(o) // lo, stored
		for l := range 4 {
			w.Get(s)
			w.Simd(simdF32x4Extract)
			w.op(byte(l))
			if l > 0 {
				w.op(opF32Min)
			}
		}
		w.op(opF32Store)
		w.memarg(2, uint32(4*vv))
		w.Get(o)
		w.op(opF32Load)
		w.memarg(2, uint32(4*vv))
		w.Get(a)
		w.op(opF32Load)
		w.memarg(2, uint32(32*vv))
		w.op(opF32Add)
		w.Get(f)
		w.op(opF32Mul)
		w.Simd(simdF32x4Splat)
		w.Set(bound)
		w.Get(est)
		w.Set(e)
		w.Get(B)
		w.Set(cnt)
		w.count(cnt, func() {
			for h, m := range []uint32{m0, m1} {
				w.v128At(e, uint32(32*vv+16*h))
				w.Get(bound)
				w.Simd(simdF32x4Le)
				w.Get(wts[h])
				w.Simd(simdV128And)
				w.Set(m)
			}
			w.Get(o)
			w.Get(cnt)
			w.op(opI32Sub) // o - cnt + B + 16 + vv*B: this block's byte
			w.Get(B)
			w.op(opI32Add)
			w.I32(16)
			w.op(opI32Add)
			w.Get(B)
			w.I32(int32(vv))
			w.op(opI32Mul)
			w.op(opI32Add)
			w.Get(m0)
			w.Get(m1)
			w.Simd(simdI32x4Add)
			w.Set(m0)
			w.sumLanes(m0)
			w.op(opI32Store8)
			w.memarg(0, 0)
			w.bump(e, 128)
		})
	}
	w.Get(o)
	w.Get(B)
	w.I32(4)
	w.op(opI32Mul)
	w.op(opI32Add)
	w.I32(16)
	w.op(opI32Add)
	w.Set(out) // the next record
}

// candConsts loads the scratch's constants: B, F, -2 and the lane weights.
func (w *Wasm) candConsts(scratch uint32) (B, f, minusTwo uint32, wts [2]uint32) {
	B, f, minusTwo = w.Local(wI32), w.Local(wI32), w.Local(wV128)
	wts = [2]uint32{w.Local(wV128), w.Local(wV128)}
	w.Get(scratch)
	w.op(opI32Load)
	w.memarg(2, 0)
	w.Set(B)
	w.Get(scratch)
	w.I32(32)
	w.op(opI32Add)
	w.Set(f) // F's address: candSet loads it as an f32
	w.v128At(scratch, 64)
	w.Set(minusTwo)
	w.v128At(scratch, 96)
	w.Set(wts[0])
	w.v128At(scratch, 112)
	w.Set(wts[1])
	return
}

// EmitVec1Cand is the twin of the AVX2 EmitVec1Cand. Its scratch also holds,
// at byte 96, the uint32 lane weights a bit set is made from.
func EmitVec1Cand() ([]byte, error) {
	var w Wasm
	set, vec, stride, nsets, n, out, scratch, a, est := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	w.ptrArg(OffA, set)
	w.ptrArg(OffV, vec)
	w.argI32(OffXC, stride)
	w.argI32(OffN, nsets)
	w.argI32(OffXA, n)
	w.ptrArg(OffOut, out)
	w.ptrArg(OffC, scratch)
	B, f, minusTwo, wts := w.candConsts(scratch)
	fl := w.Local(0x7D) // f32
	w.Get(f)
	w.op(opF32Load)
	w.memarg(2, 0)
	w.Set(fl)
	w.Get(scratch)
	w.I32(128)
	w.op(opI32Add)
	w.Set(a)
	w.Get(scratch)
	w.I32(256)
	w.op(opI32Add)
	w.Set(est)
	w.count(nsets, func() {
		w.candSet(set, vec, stride, a, out, est, n, B, wts, minusTwo, fl)
		w.Get(vec)
		w.Get(n)
		w.I32(2)
		w.op(opI32Shl)
		w.op(opI32Add)
		w.Set(vec) // the next components
	})
	return w.Module(), nil
}

// EmitVec1CandMany is the twin of the AVX2 EmitVec1CandMany: the groups one
// after another over the one set.
func EmitVec1CandMany() ([]byte, error) {
	var w Wasm
	set0, set, vec, stride, groups, n, out, scratch, a, est := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	w.ptrArg(OffA, set0)
	w.ptrArg(OffV, vec)
	w.argI32(OffXC, stride)
	w.argI32(OffN, groups)
	w.argI32(OffXA, n)
	w.ptrArg(OffOut, out)
	w.ptrArg(OffC, scratch)
	B, f, minusTwo, wts := w.candConsts(scratch)
	fl := w.Local(0x7D)
	w.Get(f)
	w.op(opF32Load)
	w.memarg(2, 0)
	w.Set(fl)
	// The A follow the groups' minima (128 bytes each, unused here); the
	// estimates follow the A.
	w.Get(scratch)
	w.I32(128)
	w.op(opI32Add)
	w.Get(groups)
	w.I32(7)
	w.op(opI32Shl)
	w.op(opI32Add)
	w.Set(a)
	w.Get(a)
	w.Get(groups)
	w.I32(7)
	w.op(opI32Shl)
	w.op(opI32Add)
	w.Set(est)
	w.count(groups, func() {
		w.Get(set0)
		w.Set(set)
		w.candSet(set, vec, stride, a, out, est, n, B, wts, minusTwo, fl)
		w.Get(vec)
		w.Get(stride)
		w.I32(2)
		w.op(opI32Shl)
		w.op(opI32Add)
		w.Set(vec) // the next group's vectors
		w.bump(a, 128)
	})
	return w.Module(), nil
}

// EmitVec1WHT is the twin of the AVX2 EmitVec1WHT: strides 1 and 2 within a
// register by shuffles, the rest between registers.
func EmitVec1WHT() ([]byte, error) {
	var w Wasm
	p, end, x, y, stride, blk, i, half := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	v, r, s, dv := w.Local(wV128), w.Local(wV128), w.Local(wV128), w.Local(wV128)
	w.ptrArg(OffA, p)
	w.argI32(OffN, end)
	w.Get(end)
	w.I32(2)
	w.op(opI32Shl)
	w.Get(p)
	w.op(opI32Add)
	w.Set(end)
	w.Get(p)
	w.Set(x)
	lane := func(l ...byte) (out [16]byte) {
		for k, b := range l {
			for j := range 4 {
				out[4*k+j] = 4*b + byte(j)
			}
		}
		return
	}
	w.loopUntil(func() { w.Get(x); w.Get(end); w.op(opI32GeU) })
	w.v128At(x, 0)
	w.Set(v)
	for _, st := range []struct{ partner, keep [16]byte }{
		{lane(1, 0, 3, 2), lane(0, 4, 2, 6)}, // stride 1: s0 d0 s2 d2
		{lane(2, 3, 0, 1), lane(0, 1, 4, 5)}, // stride 2: s0 s1 d0 d1
	} {
		w.Get(v)
		w.Get(v)
		w.shuffle(st.partner)
		w.Set(r)
		w.Get(v)
		w.Get(r)
		w.Simd(simdF32x4Add)
		w.Set(s)
		w.Get(v)
		w.Get(r)
		w.Simd(simdF32x4Sub)
		w.Set(dv)
		w.Get(s)
		w.Get(dv)
		w.shuffle(st.keep)
		w.Set(v)
	}
	w.storeV128At(x, 0, v)
	w.bump(x, 16)
	w.closeLoop()

	w.I32(16)
	w.Set(stride)
	w.loopUntil(func() {
		w.Get(p)
		w.Get(stride)
		w.op(opI32Add)
		w.Get(end)
		w.op(opI32GeU)
	})
	w.Get(p)
	w.Set(blk)
	w.loopUntil(func() { w.Get(blk); w.Get(end); w.op(opI32GeU) })
	w.Get(blk)
	w.Get(stride)
	w.op(opI32Add)
	w.Set(half)
	w.Get(blk)
	w.Set(i)
	w.loopUntil(func() { w.Get(i); w.Get(half); w.op(opI32GeU) })
	w.Get(i)
	w.Get(stride)
	w.op(opI32Add)
	w.Set(y)
	w.v128At(i, 0)
	w.Set(v)
	w.v128At(y, 0)
	w.Set(r)
	w.Get(i)
	w.Get(v)
	w.Get(r)
	w.Simd(simdF32x4Add)
	w.storeV128(0)
	w.Get(y)
	w.Get(v)
	w.Get(r)
	w.Simd(simdF32x4Sub)
	w.storeV128(0)
	w.bump(i, 16)
	w.closeLoop()
	w.Get(half)
	w.Get(stride)
	w.op(opI32Add)
	w.Set(blk)
	w.closeLoop()
	w.Get(stride)
	w.I32(1)
	w.op(opI32Shl)
	w.Set(stride)
	w.closeLoop()
	return w.Module(), nil
}

// EmitVec1Rot4 is the twin of the AVX2 EmitVec1Rot4: block by block, each
// vector's two sums in component order, multiply then add.
func EmitVec1Rot4() ([]byte, error) {
	var w Wasm
	p, nb, n, stride, out, rbytes, vv0, vp, k, d, o := w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32), w.Local(wI32)
	lo, hi, s := w.Local(wV128), w.Local(wV128), w.Local(wV128)
	w.ptrArg(OffA, p)
	w.argI32(OffN, nb)
	w.Get(nb)
	w.I32(6)
	w.op(opI32Shl)
	w.Set(rbytes) // a result's bytes: 64 * N
	w.Get(nb)
	w.I32(1)
	w.op(opI32Shl)
	w.Set(nb) // blocks
	w.argI32(OffXA, n)
	w.argI32(OffXC, stride)
	w.ptrArg(OffOut, out)
	w.ptrArg(OffV, vv0)
	w.count(nb, func() {
		for vv := range 4 {
			w.zeroLocal(lo)
			w.zeroLocal(hi)
			w.Get(vv0)
			w.Get(stride)
			w.I32(int32(vv))
			w.op(opI32Mul)
			w.op(opI32Add)
			w.Set(vp)
			w.Get(p)
			w.Set(k)
			w.Get(n)
			w.Set(d)
			w.count(d, func() {
				w.splatF32At(vp)
				w.Set(s)
				w.acc4(lo, func() { w.v128At(k, 0) }, func() { w.Get(s) })
				w.acc4(hi, func() { w.v128At(k, 16) }, func() { w.Get(s) })
				w.bump(k, 32)
				w.bump(vp, 4)
			})
			w.Get(out)
			w.Get(rbytes)
			w.I32(int32(vv))
			w.op(opI32Mul)
			w.op(opI32Add)
			w.Set(o)
			w.storeV128At(o, 0, lo)
			w.storeV128At(o, 16, hi)
		}
		w.Get(k)
		w.Set(p) // past the block
		w.bump(out, 32)
	})
	return w.Module(), nil
}
