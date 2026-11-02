package compat

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestUnaryMinusSignOfZero holds unary minus to C's two forms: a numeric
// literal negated where it stands ("-0.0" is -0.0), any other operand
// computed as 0 - X, which is +0.0 for a stored 0.0 and for the text '-0.0'.
// abs() negates only below zero, so abs(-0.0) is -0.0.
func TestUnaryMinusSignOfZero(t *testing.T) {
	db, err := engine.Create(filepath.Join(t.TempDir(), "neg.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	for _, s := range []string{
		`CREATE TABLE z(x REAL, i INTEGER, s TEXT)`,
		`INSERT INTO z VALUES(0.0, -9223372036854775808, '0.0'), (1.5, 5, '-0.0'), (-2.0, 0, 'abc')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
		if _, err := sdb.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`SELECT -x FROM z`, `SELECT -(x+0) FROM z`, `SELECT -(-x) FROM z`, `SELECT -s FROM z`, `SELECT typeof(-s) FROM z`,
		`SELECT -i FROM z`, `SELECT typeof(-i) FROM z`, `SELECT -(-i) FROM z`,
		`SELECT -0.0 FROM z`, `SELECT -(0.0) FROM z`, `SELECT -9223372036854775808 FROM z`, `SELECT -'0.0' FROM z`,
		`SELECT sum(-x) FROM z`, `SELECT min(-x) FROM z`, `SELECT max(-x) FROM z`,
		`SELECT abs(-0.0) FROM z`, `SELECT abs(-x) FROM z`, `SELECT abs(s) FROM z`, `SELECT abs(-0.0) || '' FROM z`,
	} {
		pager, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		_, rows, merr := pager.Query(q)
		pager.Close()
		if merr != nil {
			t.Fatalf("%s: %v", q, merr)
		}
		crows, err := sdb.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		k := 0
		for crows.Next() {
			var cv any
			if err := crows.Scan(&cv); err != nil {
				t.Fatal(err)
			}
			if k >= len(rows) {
				t.Errorf("%s: musql has %d rows, C more", q, len(rows))
				break
			}
			got := rows[k][0]
			if c, ok := cv.(float64); ok && got.Typ == engine.Float {
				if math.Float64bits(got.F) != math.Float64bits(c) {
					t.Errorf("%s row %d: musql %v (sign %v), C %v (sign %v)", q, k, got.F, math.Signbit(got.F), c, math.Signbit(c))
				}
			} else if !scalarValueMatchesReal(got, cv) {
				t.Errorf("%s row %d: musql %s, C %#v", q, k, describeEngineValue(got), cv)
			}
			k++
		}
		crows.Close()
		if k != len(rows) {
			t.Errorf("%s: musql %d rows, C %d", q, len(rows), k)
		}
	}
}
