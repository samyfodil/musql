package engine

// rowidBits is a set of rowids kept as one bit each over the span they
// cover, beside a map that holds the same set: a lookup that misses the map
// costs a hash and, in a map of tens of thousands of rowids, a cache miss,
// where a bit test over rowids that sit close together costs neither.
//
// It answers only "certainly not in the set" (has false); true means "maybe",
// and the map decides. Once the rowids span more than rowidBitsMax it gives up
// (full) and answers maybe for everything. A nil *rowidBits answers maybe.
type rowidBits struct {
	lo   int64 // the first rowid w covers, a multiple of 64
	w    []uint64
	full bool
}

// rowidBitsMax is the widest span of rowids a rowidBits covers: 4 MB of bits.
const rowidBitsMax = 1 << 25

func (b *rowidBits) add(r int64) {
	if b.full {
		return
	}
	at := r &^ 63 // rounds down, negative rowids included
	switch {
	case len(b.w) == 0:
		b.lo, b.w = at, make([]uint64, 1)
	case at < b.lo:
		grow := int((b.lo - at) / 64)
		if (len(b.w)+grow)*64 > rowidBitsMax {
			b.full, b.w = true, nil
			return
		}
		b.w = append(make([]uint64, grow, grow+len(b.w)), b.w...)
		b.lo = at
	case at >= b.lo+int64(len(b.w))*64:
		need := int((at-b.lo)/64) + 1
		if need*64 > rowidBitsMax {
			b.full, b.w = true, nil
			return
		}
		b.w = append(b.w, make([]uint64, need-len(b.w))...)
	}
	i := r - b.lo
	b.w[i/64] |= 1 << uint(i%64)
}

func (b *rowidBits) del(r int64) {
	if b == nil || b.full || r < b.lo || r >= b.lo+int64(len(b.w))*64 {
		return
	}
	i := r - b.lo
	b.w[i/64] &^= 1 << uint(i%64)
}

// has reports whether r may be in the set: false only when it certainly is not.
func (b *rowidBits) has(r int64) bool {
	if b == nil || b.full {
		return true
	}
	if r < b.lo || r >= b.lo+int64(len(b.w))*64 {
		return false
	}
	i := r - b.lo
	return b.w[i/64]&(1<<uint(i%64)) != 0
}
