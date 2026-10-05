package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAddColumnConstantDefault verifies that ADD COLUMN with a constant
// DEFAULT accepts only literal constants and simple CAST, not expressions or
// functions, and only on non-empty tables.
func TestAddColumnConstantDefault(t *testing.T) {
	for _, c := range []struct {
		dflt   string
		accept bool
		typ    ValueType
		i      int64
		f      float64
	}{
		{"(-(-9223372036854775808))", true, Float, 0, 9.223372036854776e+18},
		{"(-9223372036854775808)", true, Int, -9223372036854775808, 0},
		{"(CAST('7' AS INTEGER))", true, Int, 7, 0},
		{"(42)", true, Int, 42, 0},
		{"42", true, Int, 42, 0},
		{"(1+2)", false, 0, 0, 0},
		{"(abs(-3))", false, 0, 0, 0},
		{"CURRENT_TIME", false, 0, 0, 0},
	} {
		db, err := Create(filepath.Join(t.TempDir(), "d.musq"))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		for _, s := range []string{`CREATE TABLE t5(a)`, `INSERT INTO t5 DEFAULT VALUES`} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		err = db.Exec(`ALTER TABLE t5 ADD COLUMN c INTEGER DEFAULT ` + c.dflt)
		if !c.accept {
			if err == nil {
				t.Errorf("DEFAULT %s: accepted on a non-empty table, want the non-constant refusal", c.dflt)
			} else if !strings.Contains(err.Error(), "non-constant default") {
				t.Errorf("DEFAULT %s: %v, want the non-constant refusal", c.dflt, err)
			}
			db.Close()
			continue
		}
		if err != nil {
			t.Errorf("DEFAULT %s: %v, want it accepted", c.dflt, err)
			db.Close()
			continue
		}
		pg, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager: %v", perr)
		}
		_, rows, qerr := pg.QueryArgs(`SELECT c FROM t5`, nil)
		if qerr != nil {
			t.Errorf("DEFAULT %s: select: %v", c.dflt, qerr)
			db.Close()
			continue
		}
		got := rows[0][0]
		if got.Typ != c.typ || (c.typ == Int && got.I != c.i) || (c.typ == Float && got.F != c.f) {
			t.Errorf("DEFAULT %s back-filled %+v, want typ %v i=%d f=%v", c.dflt, got, c.typ, c.i, c.f)
		}
		db.Close()
	}
}
