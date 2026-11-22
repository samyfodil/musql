package engine

import (
	"strings"
	"testing"
)

// TestKeywordSetAgreesWithMap: token.in answers what the map lookup it
// replaces answered, m[t.upper()], for every keyword in any case, near misses,
// words longer than the length index holds, and a set holding such a word.
func TestKeywordSetAgreesWithMap(t *testing.T) {
	long := map[string]bool{"NOT": true, "AUTOINCREMENTALLY_LONG": true}
	sets := []map[string]bool{tableConstraintStart, columnConstraintStart, nonIdentifierKeywords, pragmaColConstraintStart, long}
	words := []string{"", "x", "cs_ext_discount_amt", "autoincrementally_long", "Autoincrementally_Long", "NOTX", "NO", "ñot", "nOt"}
	for _, m := range sets {
		for kw := range m {
			words = append(words, kw, strings.ToLower(kw), strings.ToLower(kw[:1])+kw[1:], kw+"S", kw[:len(kw)-1])
		}
	}
	for _, m := range sets {
		s := newKeywordSet(m)
		for _, w := range words {
			tok := token{kind: tkIdent, text: w}
			if got, want := tok.in(s), m[tok.upper()]; got != want {
				t.Errorf("%q in a set of %d: %v, the map says %v", w, len(m), got, want)
			}
		}
	}
}

// TestTypeAffinityASCIIAgrees: the ASCII path of typeAffinity answers as the
// upper-cased comparison does.
func TestTypeAffinityASCIIAgrees(t *testing.T) {
	ref := func(declType string) affinity {
		u := strings.ToUpper(declType)
		switch {
		case strings.Contains(u, "INT"):
			return affInteger
		case strings.Contains(u, "CHAR") || strings.Contains(u, "CLOB") || strings.Contains(u, "TEXT"):
			return affText
		case strings.Contains(u, "BLOB") || u == "":
			return affNone
		case strings.Contains(u, "REAL") || strings.Contains(u, "FLOA") || strings.Contains(u, "DOUB"):
			return affReal
		default:
			return affNumeric
		}
	}
	for _, d := range []string{"", "integer", "INT", "bigInt", "point", "Floating Point", "varchar(50)", "nchar",
		"Clob", "text", "blob", "BLOBINT", "real", "float", "DOUBLE PRECISION", "doub", "decimal(7,2)", "date",
		"numeric", "boolean", "chara", "tex", "flo", "dou", "rea", "in", "bl"} {
		if got, want := typeAffinity(d), ref(d); got != want {
			t.Errorf("typeAffinity(%q) = %v, want %v", d, got, want)
		}
	}
}
