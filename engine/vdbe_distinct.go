// This file implements the ephemeral row-set for the DISTINCT opcode.
// It mirrors vdbe_sorter's framing for exact-match dedup instead of ordering.
// memory for a dedup set too big to hold at once, a scale concern, not a
// semantic one. A linear O(n) scan per probe is the exact cost this engine's
// other []Value dedup already pays (see dedupRows, sql_compound.go: "kept :=
// rows[:0]; for _, r := range rows { for _, k := range kept { if
// keysEqualCollated(k, r, ...) ... } }"), so this reproduces it exactly, not
// merely approximates it.
package engine

// vdbeDistinctSet is the DISTINCT opcode's ephemeral tracker: every unique
// dedup key seen so far, in first-seen order (irrelevant to membership, kept
// only because a plain slice is the simplest shape, matching the sorter
// cursor's own entries-as-a-slice representation).
type vdbeDistinctSet struct {
	seen [][]Value

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
	for _, k := range d.seen {
		if keysEqualGrouping(k, key, d.colls, d.enc) {
			return true
		}
	}
	d.seen = append(d.seen, key)
	return false
}
