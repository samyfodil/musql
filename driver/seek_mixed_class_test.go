package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestIndexSeekOverMixedClasses: a column whose rows are not all of one
// storage class -- NULLs, REALs among INTEGERs, TEXT among numbers -- is
// seeked through an index over the rest of its rows, the others tested one by
// one. Every probe class answers through the index (x) exactly as the scan of
// the same rows does (y), in segments of every column type.
func TestIndexSeekOverMixedClasses(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "x.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// More rows than one segment holds, so a table has several.
	const rows = 70000
	// Sparse odd values, so the segments keep a type of their own: the
	// first holds INTEGERs and REALs (PhysNumeric where the column allows),
	// the second an integral REAL and an INTEGER too wide for a double, which
	// leave its REALs exceptions.
	value := `CASE WHEN value % 101 = 0 THEN NULL
		WHEN value % 2111 = 0 THEN 'x' || (value % 5)
		WHEN value % 1301 = 0 THEN value % 97 + 0.5
		WHEN value = 66000 THEN 9007199254740993
		WHEN value = 66001 THEN 9007199254740992.0
		WHEN value = 66002 THEN '7'
		WHEN value = 66003 THEN 7.0
		WHEN value = 66004 THEN x'37'
		ELSE value % 97 END`
	setup := []string{`CREATE TABLE p(id INTEGER PRIMARY KEY, v)`}
	probes := []string{"0", "7", "7.0", "7.5", "'7'", "'x3'", "9007199254740993", "9007199254740992", "9007199254740992.0", "NULL", "-0.0", "96", "1e300", "x'37'", "'x1'", "7.5 + 0", "39.5"}
	for i, v := range probes {
		setup = append(setup, fmt.Sprintf(`INSERT INTO p VALUES(%d, %s)`, i+1, v))
	}
	types := []string{"INTEGER", "REAL", "NUMERIC", "TEXT", ""}
	for _, ty := range types {
		for _, tb := range []string{"x", "y"} {
			setup = append(setup,
				fmt.Sprintf(`CREATE TABLE %s%s(id INTEGER PRIMARY KEY, g %s, l TEXT)`, tb, ty, ty),
				fmt.Sprintf(`INSERT INTO %s%s SELECT value, %s, 'l' || value FROM generate_series(1, %d)`, tb, ty, value, rows))
		}
		setup = append(setup, fmt.Sprintf(`CREATE INDEX x%s_g ON x%s(g)`, ty, ty))
	}
	setup = append(setup, `VACUUM`)
	for _, s := range setup {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	answer := func(q string) string {
		rows, err := db.Query(q)
		if err != nil {
			return "error: " + err.Error()
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		var out []string
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				out = append(out, fmt.Sprintf("%T:%v", v, v))
			}
		}
		return strings.Join(out, " ")
	}
	for _, ty := range types {
		var qs []string
		for _, v := range probes {
			qs = append(qs,
				fmt.Sprintf(`SELECT count(*), group_concat(id) FROM (SELECT id FROM %%s WHERE g = %s ORDER BY id)`, v),
				fmt.Sprintf(`SELECT count(*), sum(id) FROM %%s WHERE g = %s`, v),
				fmt.Sprintf(`SELECT count(*) FROM %%s WHERE g = %s`, v))
		}
		qs = append(qs,
			`SELECT p.id, (SELECT group_concat(id) FROM (SELECT id FROM %s WHERE g = p.v ORDER BY id)) FROM p`,
			`SELECT p.id, (SELECT count(*) FROM %s WHERE g = p.v) FROM p`,
			`SELECT p.id, (SELECT min(id) FROM %s WHERE g = p.v) FROM p`)
		for _, q := range qs {
			x, y := fmt.Sprintf(q, "x"+ty), fmt.Sprintf(q, "y"+ty)
			if got, want := answer(x), answer(y); got != want {
				t.Errorf("%s:\n got  %.300s\n want %.300s", x, got, want)
			}
		}
	}
}
