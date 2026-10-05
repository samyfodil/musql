// Value comparison and sort order.
//
// compareValues implements SQLite's storage-class ordering (NULL < INTEGER =
// REAL < TEXT < BLOB, classOrder), with numeric comparison exact across the
// Int/Float boundary and text comparison routed through the resolved collating
// sequence. orderTermLess is the ORDER BY predicate built on top of it.
//
// This is sqlite3MemCompare's job (vdbemem.c) -- a value primitive the
// executor calls, which is why SQLite also keeps it outside its evaluator.
package engine

import (
	"bytes"
	"math"
)

// classOrder implements SQLite's storage-class sort order: NULL < INTEGER/
// REAL (as one numeric class) < TEXT < BLOB.
func classOrder(v Value) int {
	switch v.Typ {
	case Null:
		return 0
	case Int, Float:
		return 1
	case Text:
		return 2
	case Blob:
		return 3
	}
	return 0
}

// compareValues compares two values per SQLite's ordering rules. Callers
// handle NULL specially where NULL propagation (rather than ordering) is
// required (e.g. inside comparison operators); this function is also used
// directly for ORDER BY, where NULL does participate in the ordering.
func compareValues(a, b Value) int {
	ca, cb := classOrder(a), classOrder(b)
	if ca != cb {
		if ca < cb {
			return -1
		}
		return 1
	}
	switch ca {
	case 0:
		return 0
	case 1:
		return compareNumeric(a, b)
	case 2, 3:
		return bytes.Compare(a.S, b.S) // BINARY collation: memcmp
	}
	return 0
}

// compareValuesCollatedEnc compares under a collation in a database whose TEXT
// encoding may not be UTF-8. BINARY (and RTRIM) comparison is memcmp over the
// ENCODED bytes, so the encoding genuinely changes the ORDER -- see utf16.go's
// doc comment for the three different orders the same four strings produce.
// Every caller that can reach a UTF-16 database passes its encoding; the
// UTF-8 shim above is kept for the callers that provably cannot (a fixed
// collation over ASCII-only internal keys).
func compareValuesCollatedEnc(a, b Value, collation string, enc TextEncoding) int {
	ca, cb := classOrder(a), classOrder(b)
	if ca != cb {
		if ca < cb {
			return -1
		}
		return 1
	}
	switch ca {
	case 0:
		return 0
	case 1:
		return compareNumeric(a, b)
	case 2:
		return collatedTextCompareEnc(collation, a.S, b.S, enc)
	case 3:
		return bytes.Compare(a.S, b.S)
	}
	return 0
}

// orderTermLess reports whether value a sorts strictly before value b for
// the given ORDER BY term, and whether they compare equal. NULL placement
// is decided by the term's effective NullsFirst() -- which is independent
// of ASC/DESC unless no explicit NULLS clause was given, in which case it
// falls back to the direction-derived default (NULL is the smallest value:
// first for ASC, last for DESC) -- so an explicit "NULLS FIRST"/"NULLS
// LAST" is NOT re-flipped by the Desc flag the way a non-NULL comparison
// is. Non-NULL values compare via compareValuesCollatedEnc (honoring any
// COLLATE on the term, completely independent of NULLS placement) with the
// term's direction applied. This is the single comparator every ORDER BY
// sort site in the VDBE must use -- the sorter (vdbe_sorter.go), window
// partition and frame ordering (vdbe_window.go), GROUP BY/DISTINCT
// (vdbe_group_distinct.go), compound ORDER BY (vdbe_compound_codegen.go)
// and recursive-CTE ordering (cte.go) -- so NULLS FIRST/LAST and the
// unadorned default agree everywhere.
func orderTermLess(ot OrderTerm, a, b Value, collation string, enc TextEncoding) (less, equal bool) {
	aNull := a.Typ == Null
	bNull := b.Typ == Null
	if aNull && bNull {
		return false, true
	}
	if aNull != bNull {
		nullsFirst := ot.NullsFirst()
		if aNull {
			return nullsFirst, false
		}
		return !nullsFirst, false
	}
	c := compareValuesCollatedEnc(a, b, collation, enc)
	if c == 0 {
		return false, true
	}
	if ot.Desc {
		return c > 0, false
	}
	return c < 0, false
}

// effectiveCollation normalizes a possibly-empty declared/explicit collation
// name to its effective value: "BINARY" for "" (columnInfo.Collation is
// never actually "" for a real column -- CREATE TABLE/ALTER TABLE always
// store "BINARY" explicitly -- but callers building a collation from other
// optional sources, e.g. an unspecified per-index-column COLLATE, use ""
// as their own "unspecified" sentinel; this is where that sentinel
// resolves to the SQLite default), unchanged otherwise.
func effectiveCollation(name string) string {
	if name == "" {
		return "BINARY"
	}
	return name
}

// atOrEmpty returns s[i], or "" if i is out of range -- used where a
// collation slice may be shorter than the value slice it parallels (e.g. an
// index record's trailing rowid column, which idx.colCollation never
// covers).
func atOrEmpty(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return ""
	}
	return s[i]
}

// collatedTextCompareEnc is collatedTextCompare in a database whose text
// encoding may not be UTF-8. NOCASE is unaffected -- it folds ASCII case and
// C SQLite gives the identical answers in all three encodings (verified:
// "'ABC' = 'abc' COLLATE NOCASE" is 1 and "'É' = 'é' COLLATE NOCASE" is 0
// everywhere) -- while BINARY and RTRIM are memcmp over the ENCODED bytes and
// therefore order differently. utf16CompareUTF8 computes that order straight
// from the internal UTF-8 bytes; see utf16.go.
func collatedTextCompareEnc(collation string, a, b []byte, enc TextEncoding) int {
	// A collation NAME is an identifier: sqlite3FindCollSeq looks it up in
	// db->aCollSeq, a hash.c table that folds with sqlite3UpperToLower and
	// compares with sqlite3StrICmp -- ASCII-only, like every other identifier
	// here (see r33sFoldIdent).
	//
	// This is REACHED PER TEXT COMPARISON PER ROW, because a comparison carries
	// its collation as a name and not as the resolved CollSeq* C puts in P4 and
	// hands to sqlite3MemCompare (vdbe.c:2383). Until that is resolved at
	// compile time, the name match at least must not rebuild a folded copy of
	// the name to look at it: asciiFold is []byte(s) then string(b), and while
	// the compiler keeps both off the heap here (measured: 0 allocs), the fold
	// still walks the whole name before the switch walks it again.
	// BenchmarkCollationDispatch, against a plain bytes.Compare of the same
	// operands (1.99 ns, the floor): BINARY 10.23 -> 4.90, RTRIM 10.23 -> 6.97,
	// NOCASE 19.20 -> 16.46. BINARY is the overwhelmingly common case and is now
	// within 2.9 ns of the floor, which is the two name matches below.
	//
	// asciiEqualFold is sqlite3StrICmp's own shape -- compare in place, fold
	// each byte as it is read, stop at the first difference -- so it answers
	// exactly what the folded switch answered, including for a name this engine
	// does not know (which falls through to BINARY, as it did before).
	switch {
	case asciiEqualFold(collation, "NOCASE"):
		return nocaseCompare(a, b)
	case asciiEqualFold(collation, "RTRIM"):
		a, b = rtrimTrailingSpaces(a), rtrimTrailingSpaces(b)
	}
	if isUTF16(enc) {
		return utf16CompareUTF8(enc, a, b)
	}
	return bytes.Compare(a, b)
}

// nocaseCompare implements the NOCASE collation's memcmp-with-ASCII-fold.
// Critically, it folds to LOWERCASE, not uppercase -- verified directly
// against C SQLite (which folds via its own sqlite3UpperToLower table,
// i.e. sqlite3StrNICmp) that the fold DIRECTION is observable, not just an
// internal implementation detail: "']' < 'B' COLLATE NOCASE" is TRUE but
// "']' < 'b' COLLATE NOCASE" is also TRUE, and ']' (0x5D) sits BETWEEN
// uppercase 'B' (0x42) and lowercase 'b' (0x62) in ASCII -- so folding to
// UPPERCASE (an earlier version of this function did, matching
// asciiFold/asciiUpperRune's existing upper-folding convention used
// elsewhere for lower()/upper()/LIKE) makes ']' compare GREATER than 'B',
// while C SQLite's actual lower-folding NOCASE puts it in between,
// producing a different, WRONG sort order for any text containing a byte in
// the 0x5B-0x60 range next to a letter (verified via this package's own
// vdbe_glob_collate_test.go: "a]b" sorts before "ABC"/"abc" under real
// SQLite's NOCASE, not after). A byte-wise fold is otherwise safe even for
// multi-byte UTF-8 text because every UTF-8 continuation/lead byte for a
// non-ASCII rune is >= 0x80, well outside the 'A'-'Z' fold range.
func nocaseCompare(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ca, cb := asciiLowerByte(a[i]), asciiLowerByte(b[i])
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func asciiLowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// r33sFoldIdent folds an identifier for comparison exactly the way SQLite does:
// byte-wise through sqlite3UpperToLower (global.c:24), a 256-entry table whose
// ONLY non-identity entries are 65..90 -> 97..122, i.e. 'A'-'Z' -> 'a'-'z'.
// Every identifier comparison in SQLite reaches that table -- sqlite3StrICmp
// (util.c:416) walks two byte strings comparing UpperToLower[c] to
// UpperToLower[x]; sqlite3_strnicmp (util.c:435) is the bounded form; hash.c's
// strHash/findElementWithHash, which back the schema, function and collation
// lookups, fold and compare with the same two. The table's own header states
// the reason it stops at ASCII:
//
//	SQLite only considers US-ASCII (or EBCDIC) characters.  We do not
//	handle case conversions for the UTF character set since the tables
//	involved are nearly as big or bigger than SQLite itself.
//
// So this is deliberately NOT strings.ToLower. Unicode folding collapses pairs
// SQLite keeps DISTINCT -- "Ä"/"ä", "Σ"/"σ", "Ω"/"ω" -- and on two of them it
// even changes byte length ("İ" -> "i", the Kelvin sign U+212A -> "k"), so a
// Unicode-folded map key can collide with a shorter, unrelated identifier.
// Folding byte-wise is safe for multi-byte text because every UTF-8 lead and
// continuation byte is >= 0x80, outside the folded range, and is therefore
// carried through unchanged.
//
// Identifier folding is a SEPARATE question from the NOCASE collation, which
// compares VALUES rather than names; that one has its own (also ASCII-only,
// also byte-wise) rule in nocaseCompare.
func r33sFoldIdent(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for ; i < len(b); i++ {
				b[i] = asciiLowerByte(b[i])
			}
			return string(b)
		}
	}
	return s
}

// rtrimTrailingSpaces returns b with only trailing ASCII 0x20 space bytes
// removed (not a general whitespace trim -- verified directly that a
// trailing TAB is left alone by the RTRIM collation).
func rtrimTrailingSpaces(b []byte) []byte {
	i := len(b)
	for i > 0 && b[i-1] == ' ' {
		i--
	}
	return b[:i]
}

func compareNumeric(a, b Value) int {
	if a.Typ == Int && b.Typ == Int {
		switch {
		case a.I < b.I:
			return -1
		case a.I > b.I:
			return 1
		default:
			return 0
		}
	}
	if a.Typ == Int && b.Typ == Float {
		return compareIntFloat(a.I, b.F)
	}
	if a.Typ == Float && b.Typ == Int {
		return -compareIntFloat(b.I, a.F)
	}
	af, bf := valueAsFloat(a), valueAsFloat(b)
	switch {
	case af < bf:
		return -1
	case af > bf:
		return 1
	default:
		return 0
	}
}

func valueAsFloat(v Value) float64 {
	if v.Typ == Int {
		return float64(v.I)
	}
	return v.F
}

// compareIntFloat compares an INTEGER i against a REAL r EXACTLY -- as
// SQLite itself does (sqlite3IntFloatCompare in vdbe.c) -- without ever
// converting i to float64 (which, for |i| >= 2^53, silently rounds it and
// can make two genuinely DIFFERENT int64 values compare equal to the same
// stored REAL, or compare on the wrong side of it). Verified directly
// against C SQLite (affinity2.test): a REAL column storing the literal
// 3175546974276630385 (which itself already lost precision converting TO a
// REAL at INSERT time, per REAL affinity -- that part is expected and
// correct) must still compare as GREATER than that same literal re-parsed
// as an INTEGER in a later query, because the two are compared via their
// EXACT values, not by re-rounding the integer down to whatever double the
// column's value already occupies.
//
// The trick: any float64 magnitude >= 2^53 has no representable fractional
// part at all (its ULP already exceeds 1), so int64(r) recovers r's exact
// value with NO precision loss whenever r is in int64's range -- only
// magnitudes below 2^53 can have a genuine fraction, and those (along with
// i itself) all convert exactly between int64 and float64 anyway.
func compareIntFloat(i int64, r float64) int {
	switch {
	case math.IsNaN(r):
		// SQLite has no NULL-free NaN value to compare against (a REAL
		// column can't actually store one via ordinary SQL), so this is
		// unreachable in practice; treat it as "greater than everything"
		// defensively rather than panicking or picking an arbitrary side.
		return -1
	case r < -9223372036854775808.0: // less than any possible int64
		return 1
	case r >= 9223372036854775808.0: // greater than any possible int64 (2^63)
		return -1
	}
	ri := int64(r) // exact: r is now known to be within int64's range
	switch {
	case i < ri:
		return -1
	case i > ri:
		return 1
	}
	// i == ri (equal integer parts): whatever's left of r beyond that
	// integer part (r's fractional remainder, exactly recoverable -- see
	// doc comment above) decides the tie.
	switch frac := r - float64(ri); {
	case frac > 0:
		return -1
	case frac < 0:
		return 1
	default:
		return 0
	}
}

// asciiEqualFold reports whether a and b are equal ignoring ASCII case, with no
// allocation and no folded copy of either -- sqlite3StrICmp (util.c), which
// walks both strings through sqlite3UpperToLower and stops at the first
// difference.
//
// ASCII-ONLY, deliberately, and that is observable rather than an internal
// detail: strings.EqualFold applies Unicode simple folding, under which a name
// containing the Kelvin sign (U+212A) equals one containing "k". SQLite's
// identifier comparison does not, so using EqualFold here would make this
// engine accept a collation name C SQLite rejects -- the same class of
// defect commit 6803c57 fixed for keyword folding, where musql EXECUTED SQL
// the oracle refused. want is always an ASCII literal at every call site.
func asciiEqualFold(s, want string) bool {
	if len(s) != len(want) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c != want[i] {
			return false
		}
	}
	return true
}
