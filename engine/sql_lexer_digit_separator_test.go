package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestNumericDigitSeparator pins sqlite3DequoteNumber (util.c:332-352): a '_'
// inside a numeric literal is removed, and is legal only BETWEEN two digits --
// hex digits for an "0x" literal, so "0x1_F" is legal while "1_F" is not.
//
// The type rule comes from the same function: the result is FLOAT if what
// survives contains 'e', 'E' or '.', INTEGER otherwise, and a hex literal is
// ALWAYS integer ("if( bHex ) p->op = TK_INTEGER").
func TestNumericDigitSeparator(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "sep.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()
	pg, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}

	for _, c := range []struct {
		expr string
		typ  ValueType
		i    int64
		f    float64
	}{
		{"1_000_000_000_000", Int, 1_000_000_000_000, 0},
		{"1_0", Int, 10, 0},
		{"0x1_F", Int, 31, 0},        // hex neighbours; always INTEGER
		{"1_0.5", Float, 0, 10.5},    // '.' makes it real
		{"1_0e1_0", Float, 0, 10e10}, // separators in the exponent too
		{".5", Float, 0, 0.5},        // unaffected control
		{"1000000", Int, 1000000, 0}, // unaffected control
	} {
		_, rows, qerr := pg.QueryArgs("SELECT "+c.expr, nil)
		if qerr != nil {
			t.Errorf("SELECT %s: %v", c.expr, qerr)
			continue
		}
		got := rows[0][0]
		if got.Typ != c.typ {
			t.Errorf("SELECT %s: type %v, want %v", c.expr, got.Typ, c.typ)
			continue
		}
		if c.typ == Int && got.I != c.i {
			t.Errorf("SELECT %s = %d, want %d", c.expr, got.I, c.i)
		}
		if c.typ == Float && got.F != c.f {
			t.Errorf("SELECT %s = %v, want %v", c.expr, got.F, c.f)
		}
	}

	// A separator that is not between two digits is "unrecognized token", and
	// the message carries the WHOLE literal, separators included -- which is
	// what C reports because it errors during dequote, on p->u.zToken.
	for _, bad := range []string{"1_", "1__0", "1_.5", "1_e5", "0x1_G"} {
		_, _, qerr := pg.QueryArgs("SELECT "+bad, nil)
		if qerr == nil {
			t.Errorf("SELECT %s: accepted, want an unrecognized-token error", bad)
			continue
		}
		if !strings.Contains(qerr.Error(), "unrecognized token") {
			t.Errorf("SELECT %s: %v, want an unrecognized-token error", bad, qerr)
		}
	}
}
