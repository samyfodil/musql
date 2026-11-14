package engine

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"slices"
)

// appendSortKey appends v to dst so that bytes.Compare of two keys built
// column by column orders them as compareValuesCollatedEnc does under BINARY
// in a UTF-8 database: a NULL first, then INTEGERs by value, then TEXTs and
// then BLOBs by their bytes. A TEXT or BLOB's zero bytes are escaped and it
// ends in two zero bytes, so a prefix sorts first. desc inverts the column's
// bytes. false for a REAL, which compares with an INTEGER by value.
func appendSortKey(dst []byte, v Value, desc bool) ([]byte, bool) {
	start := len(dst)
	switch v.Typ {
	case Null:
		dst = append(dst, 0)
	case Int:
		dst = binary.BigEndian.AppendUint64(append(dst, 1), uint64(v.I)^1<<63)
	case Text, Blob:
		tag := byte(2)
		if v.Typ == Blob {
			tag = 3
		}
		dst = append(dst, tag)
		for _, b := range v.S {
			if b == 0 {
				dst = append(dst, 0, 0xff)
			} else {
				dst = append(dst, b)
			}
		}
		dst = append(dst, 0, 0)
	default:
		return dst, false
	}
	if desc {
		for i := start; i < len(dst); i++ {
			dst[i] = ^dst[i]
		}
	}
	return dst, true
}

// binaryCollation reports whether collatedTextCompareEnc compares under
// coll as bytes.Compare does.
func binaryCollation(coll string) bool {
	return !asciiEqualFold(coll, "NOCASE") && !asciiEqualFold(coll, "RTRIM")
}

// sortBySortKeys sorts perm, n rows of nk key columns read by at, by the
// keys appendSortKey builds, ties in perm's index order; false, with perm
// untouched, where a value has no such key.
func sortBySortKeys(perm []int, nk int, at func(i, k int) Value, desc func(k int) bool, stop *sortStop) bool {
	n := len(perm)
	var arena []byte
	ends := make([]int, n)
	for i := range n {
		for k := range nk {
			var ok bool
			if arena, ok = appendSortKey(arena, at(i, k), desc(k)); !ok {
				return false
			}
		}
		ends[i] = len(arena)
	}
	key := func(i int) []byte {
		if i == 0 {
			return arena[:ends[0]]
		}
		return arena[ends[i-1]:ends[i]]
	}
	// Each row's first sixteen key bytes, compared as two integers, decide
	// most comparisons without reading the key.
	type rec struct {
		hi, lo uint64
		i      int
	}
	recs := make([]rec, n)
	for j, i := range perm {
		var pre [16]byte
		copy(pre[:], key(i))
		recs[j] = rec{binary.BigEndian.Uint64(pre[:8]), binary.BigEndian.Uint64(pre[8:]), i}
	}
	slices.SortFunc(recs, func(a, b rec) int {
		if stop.now() {
			return 0
		}
		if a.hi != b.hi {
			return cmp.Compare(a.hi, b.hi)
		}
		if a.lo != b.lo {
			return cmp.Compare(a.lo, b.lo)
		}
		if c := bytes.Compare(key(a.i), key(b.i)); c != 0 {
			return c
		}
		return a.i - b.i
	})
	for j, r := range recs {
		perm[j] = r.i
	}
	return true
}
