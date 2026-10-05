package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

func buildRowidSeekTestDB(t *testing.T) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seek.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`); err != nil {
		t.Fatal(err)
	}
	ids := []int64{-100, -1, 1, 2, 3, 50, 5000}
	for _, id := range ids {
		if _, _, err := db.ExecArgs(`INSERT INTO t(id,v,s) VALUES(?,?,?)`,
			[]Value{{Typ: Int, I: id}, {Typ: Int, I: id * 7}, {Typ: Text, S: []byte("s")}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// TestRowidSeekResultNeutral tests that rowid-seek optimization is
// result-neutral across various WHERE shapes.
func TestRowidSeekResultNeutral(t *testing.T) {
	p := buildRowidSeekTestDB(t)

	cases := []struct {
		name string
		sql  string
		args []Value
		want [][]int64 // each row's [v]; nil means zero rows
	}{
		{"ipk-hit", "SELECT v FROM t WHERE id = ?", []Value{{Typ: Int, I: 3}}, [][]int64{{21}}},
		{"ipk-miss", "SELECT v FROM t WHERE id = ?", []Value{{Typ: Int, I: 4}}, nil},
		{"ipk-negative", "SELECT v FROM t WHERE id = ?", []Value{{Typ: Int, I: -100}}, [][]int64{{-700}}},
		{"ipk-literal", "SELECT v FROM t WHERE id = 50", nil, [][]int64{{350}}},
		{"rowid-pseudo", "SELECT v FROM t WHERE rowid = 2", nil, [][]int64{{14}}},
		{"oid-pseudo", "SELECT v FROM t WHERE oid = 5000", nil, [][]int64{{35000}}},
		{"key-reversed", "SELECT v FROM t WHERE 1 = id", nil, [][]int64{{7}}},
		{"extra-conjunct-pass", "SELECT v FROM t WHERE id = 3 AND v > 0", nil, [][]int64{{21}}},
		{"extra-conjunct-fail", "SELECT v FROM t WHERE id = 3 AND v > 1000", nil, nil},
		{"float-key-integral", "SELECT v FROM t WHERE id = 3.0", nil, [][]int64{{21}}},
		{"float-key-frac", "SELECT v FROM t WHERE id = 3.5", nil, nil},
		{"text-key", "SELECT v FROM t WHERE id = '3'", nil, [][]int64{{21}}},
		{"or-not-seekable", "SELECT v FROM t WHERE id = 3 OR id = 50", nil, [][]int64{{21}, {350}}},
		{"non-rowid-col", "SELECT v FROM t WHERE v = 21", nil, [][]int64{{21}}},
	}

	run := func(sql string, args []Value) [][]Value {
		_, rows, err := p.QueryArgs(sql, args)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return rows
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vrows := run(tc.sql, tc.args)
			if len(vrows) != len(tc.want) {
				t.Fatalf("row count: got %d want %d (%v)", len(vrows), len(tc.want), vrows)
			}
			for i := range tc.want {
				if len(vrows[i]) != len(tc.want[i]) {
					t.Fatalf("row %d width: got %d want %d", i, len(vrows[i]), len(tc.want[i]))
				}
				for j, w := range tc.want[i] {
					if vrows[i][j].Typ != Int || vrows[i][j].I != w {
						t.Fatalf("row %d col %d: got %v want Int(%d)", i, j, vrows[i][j], w)
					}
				}
			}
		})
	}
}

func rowsEqual(a, b [][]Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if !valuesEqualExact(a[i][j], b[i][j]) {
				return false
			}
		}
	}
	return true
}

// TestRowidSeekFiresWhenEligible checks the compiled program actually contains
// the OpSeekRowidHint for eligible statements and NOT for ineligible ones --
// i.e. the optimization is really engaged (a result-only test could pass while
// silently always full-scanning).
func TestRowidSeekFiresWhenEligible(t *testing.T) {
	p := buildRowidSeekTestDB(t)
	fires := []string{
		"SELECT v FROM t WHERE id = 5",
		"SELECT v FROM t WHERE rowid = 5",
		"SELECT v FROM t WHERE id = ?",
		"SELECT v FROM t WHERE 5 = id",
		"SELECT v FROM t WHERE id = 5 AND v > 0",
	}
	for _, sql := range fires {
		d, err := DisassembleScan(p, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if !strings.Contains(d, "SeekRowidHint") {
			t.Errorf("expected SeekRowidHint for %q, got:\n%s", sql, d)
		}
	}
	noFire := []string{
		"SELECT v FROM t WHERE v = 5",           // non-rowid column
		"SELECT v FROM t WHERE id = v",          // key references a column
		"SELECT v FROM t WHERE id > 5",          // not equality
		"SELECT v FROM t WHERE id = 5 OR v = 1", // under OR
	}
	for _, sql := range noFire {
		d, err := DisassembleScan(p, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if strings.Contains(d, "SeekRowidHint") {
			t.Errorf("did NOT expect SeekRowidHint for %q, got:\n%s", sql, d)
		}
	}
}
