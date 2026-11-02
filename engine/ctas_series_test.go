package engine_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestCreateTableAsFromSeries: a CREATE TABLE AS over generate_series that
// cuts its segments column by column holds the rows, types and bits the
// row-by-row fill inside a transaction holds, before and after a reopen, and
// the shapes it does not take still fill row by row.
func TestCreateTableAsFromSeries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		return db
	}
	db := open()
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dump := func(q string) string {
		t.Helper()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var b strings.Builder
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if f, ok := v.(float64); ok {
					fmt.Fprintf(&b, "%x ", f)
					continue
				}
				fmt.Fprintf(&b, "%T:%v ", v, v)
			}
			b.WriteByte('|')
		}
		return b.String()
	}
	cases := []struct {
		sel   string
		where string // an equality the column's index answers
		taken bool
	}{
		{`SELECT value%7 a, value%11 b, value i FROM generate_series(1, 200000)`, `a = 3`, true},
		{`SELECT value * 0.5 h, sqrt(value) s, round(value / 3.0) r FROM generate_series(1, 70000)`, `r = 7`, true},
		{`SELECT value v, -value n FROM generate_series(1, 100000) WHERE value % 3 = 0`, `v = 300`, true},
		{`SELECT value v FROM generate_series(1, 100000, 7) WHERE value > 50000`, `v = 50002`, true},
		{`SELECT value v FROM generate_series(5, 1)`, `v = 1`, true},
		{`SELECT CASE WHEN value % 2 = 0 THEN value ELSE -value END c FROM generate_series(1, 100000)`, `c = -5`, true},
		{`SELECT ln(value - 5) l FROM generate_series(1, 1000)`, `l IS NULL`, false},
		{`SELECT 'x' || value s FROM generate_series(1, 1000)`, `s = 'x5'`, false},
		{`SELECT value * 9223372036854775807 o FROM generate_series(1, 10)`, `o > 0`, false},
		{`SELECT CAST(value AS REAL) f FROM generate_series(1, 1000)`, `f = 5`, false},
		{`SELECT start s, value v FROM generate_series(1, 100)`, `v = 5`, false},
		{`SELECT DISTINCT value % 10 d FROM generate_series(1, 1000)`, `d = 5`, false},
	}
	for i, c := range cases {
		engine.CTASSeriesHitsForTest()
		exec(fmt.Sprintf("CREATE TABLE d%d AS %s", i, c.sel))
		hits := engine.CTASSeriesHitsForTest()
		exec("BEGIN")
		exec(fmt.Sprintf("CREATE TABLE r%d AS %s", i, c.sel))
		exec("COMMIT")
		if engine.JITEnabled() && (hits > 0) != c.taken {
			t.Errorf("%s: column by column %v, want %v", c.sel, hits > 0, c.taken)
		}
	}
	check := func(when string) {
		for i, c := range cases {
			for _, q := range []string{"SELECT * FROM %s%d ORDER BY rowid", "SELECT count(*), sum(rowid) FROM %s%d WHERE " + c.where} {
				if got, want := dump(fmt.Sprintf(q, "d", i)), dump(fmt.Sprintf(q, "r", i)); got != want {
					t.Errorf("%s, %s: %s\n column by column %.300s\n row by row       %.300s", when, c.sel, q, got, want)
				}
			}
			if got, want := dump(fmt.Sprintf("SELECT replace(sql, 'd%d', 'X') FROM sqlite_schema WHERE name = 'd%d'", i, i)),
				dump(fmt.Sprintf("SELECT replace(sql, 'r%d', 'X') FROM sqlite_schema WHERE name = 'r%d'", i, i)); got != want {
				t.Errorf("%s: schema %s, row by row %s", c.sel, got, want)
			}
		}
	}
	check("before a reopen")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	defer db.Close()
	check("after a reopen")
}
