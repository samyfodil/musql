// This file implements the ephemeral row-set for the DISTINCT opcode.
// It mirrors vdbe_sorter's framing for exact-match dedup instead of ordering.
// Seen keys are bucketed by distinctHash, a hash every pair of keys that
// keysEqualGrouping calls equal shares, and a probe compares only its bucket.
// A linear scan over every seen key made "SELECT DISTINCT" quadratic in the
// number of distinct rows.
package engine

import "math"

// vdbeDistinctSet is the DISTINCT opcode's ephemeral tracker: every unique
// dedup key seen so far, in first-seen order (irrelevant to membership, kept
// only because a plain slice is the simplest shape, matching the sorter
// cursor's own entries-as-a-slice representation).
type vdbeDistinctSet struct {
	seen    [][]Value
	buckets map[uint64][]int32 // distinctHash -> indexes into seen

	// colls is the per-output-column collating sequence this dedup compares
	// under, or nil when every column is BINARY -- exactly OpGroup's own P4
	// convention, and keysEqualGrouping answers a nil list with keysEqual
	// itself, byte for byte as this opcode always did.
	//
	// DISTINCT is collation-sensitive and this used to be DECLINED rather
	// than answered: over a COLLATE NOCASE column holding 'one' and 'ONE',
	// "SELECT DISTINCT a FROM t1" is ONE row in C SQLite (in5.test) and
	// two under a BINARY dedup.
	colls []string
	enc   TextEncoding
}

// newDistinctSet creates an empty tracker for OpDistinctOpen.
func newDistinctSet(colls []string, enc TextEncoding) *vdbeDistinctSet {
	return &vdbeDistinctSet{colls: colls, enc: enc}
}

// checkAndAdd reports whether key duplicates a row already seen (found==true,
// via keysEqual's NULL-equal row comparison -- key is NOT added again in that
// case); otherwise it remembers key as newly seen and returns found==false.
// key is never mutated or aliased elsewhere afterward (OpMakeRecord builds a
// fresh, deep-copied record for every call, exactly like the sorter's own
// entries), so it needs no defensive copy of its own here either.
func (d *vdbeDistinctSet) checkAndAdd(key []Value) (found bool) {
	h := d.hash(key)
	for _, i := range d.buckets[h] {
		if keysEqualGrouping(d.seen[i], key, d.colls, d.enc) {
			return true
		}
	}
	if d.buckets == nil {
		d.buckets = make(map[uint64][]int32)
	}
	d.buckets[h] = append(d.buckets[h], int32(len(d.seen)))
	d.seen = append(d.seen, key)
	return false
}

// hash combines distinctHash over the key's values under their collations.
func (d *vdbeDistinctSet) hash(key []Value) uint64 {
	h := uint64(14695981039346656037)
	for i, v := range key {
		h = (h ^ distinctHash(v, atOrEmpty(d.colls, i))) * 1099511628211
	}
	return h
}

// distinctHash hashes v so that values equal under coll hash alike:
//   - NULL is one value.
//   - INTEGER and REAL compare by numeric value, so an integral REAL in int64
//     range hashes as that integer.
//   - TEXT and BLOB under BINARY are their bytes. A TEXT under NOCASE or RTRIM
//     is its ASCII-folded or right-trimmed bytes when it is pure ASCII; any
//     other TEXT under a non-BINARY collation hashes to one constant, since
//     such a collation may equate different bytes (a malformed sequence read
//     as U+FFFD, or a collation of the application's own).
func distinctHash(v Value, coll string) uint64 {
	switch v.Typ {
	case Null:
		return 1
	case Int:
		return mix64(uint64(v.I))
	case Float:
		if f := v.F; f == math.Trunc(f) && f >= -9223372036854775808 && f < 9223372036854775808 {
			return mix64(uint64(int64(f)))
		}
		return mix64(math.Float64bits(v.F) ^ 0x5bd1e995)
	case Blob:
		return 3 ^ fnvBytes(v.S, false, false)
	case Text:
		if coll == "" || equalFoldName(coll, "BINARY") {
			return 2 ^ fnvBytes(v.S, false, false)
		}
		nocase, rtrim := equalFoldName(coll, "NOCASE"), equalFoldName(coll, "RTRIM")
		if !nocase && !rtrim {
			return 4
		}
		for _, c := range v.S {
			if c >= 0x80 {
				return 4
			}
		}
		return 2 ^ fnvBytes(v.S, nocase, rtrim)
	}
	return 5
}

func fnvBytes(b []byte, fold, rtrim bool) uint64 {
	if rtrim {
		for len(b) > 0 && b[len(b)-1] == ' ' {
			b = b[:len(b)-1]
		}
	}
	h := uint64(14695981039346656037)
	for _, c := range b {
		if fold {
			c = asciiUpperByte(c)
		}
		h = (h ^ uint64(c)) * 1099511628211
	}
	return h
}

func mix64(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return x
}
