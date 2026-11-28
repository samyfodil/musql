package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestIndexSeekOrderPinnedColumns: a correlated seek through an index on
// (g, h DESC) takes its rows in the index's order: the column the seek holds
// to one value ties, so they go by h descending, then rowid. The seek skips a
// numeric column it pins and checks the rest. Each unordered subquery answers
// as its twin with that order spelled out, and the first one as C SQLite does
// (l2151, not the lowest rowid's l1). A TEXT or untyped column compared with
// an INTEGER column cannot be searched through the index, so C scans those
// tables for a bare column, in rowid order, and the index itself (in its
// order) when it covers the subquery. The m tables hold values of several
// storage classes, which leaves their column without the segments' index.
func TestIndexSeekOrderPinnedColumns(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "x.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	setup := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INT, j INT)`,
		`INSERT INTO t SELECT value, value % 53, (value * 37) % 101 FROM generate_series(1, 60)`,
	}
	var tables []string
	order := map[string]string{}
	for _, ty := range []string{"INTEGER", "REAL", "NUMERIC", "TEXT", ""} {
		for _, b := range []string{"b" + ty, "m" + ty} {
			tables, order[b] = append(tables, b), "g, h DESC, id"
			if ty == "TEXT" || ty == "" {
				order[b] = "id"
			}
			setup = append(setup,
				fmt.Sprintf(`CREATE TABLE %s(id INTEGER PRIMARY KEY, g %s, h INT, l TEXT)`, b, ty),
				fmt.Sprintf(`CREATE INDEX %s_gh ON %s(g, h DESC)`, b, b),
				fmt.Sprintf(`INSERT INTO %s SELECT value, value %% 50, (value * 37) %% 101, 'l' || value FROM generate_series(1, 4000)`, b))
		}
		setup = append(setup, fmt.Sprintf(`INSERT INTO m%s VALUES(5001, 7.0, 3, 'real'), (5002, '7', 4, 'text'), (5003, 7.5, 5, 'half')`, ty))
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
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b any
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%v:%T:%v", a, b, b))
		}
		return strings.Join(out, " ")
	}
	first := answer(`SELECT t.id, (SELECT bINTEGER.l FROM bINTEGER WHERE bINTEGER.g = t.k) FROM t WHERE t.id <= 2`)
	if want := "1:string:l2151 2:string:l2252"; first != want {
		t.Errorf("first rows %s, want C's %s", first, want)
	}
	for _, b := range tables {
		for _, q := range [][2]string{
			{`SELECT t.id, (SELECT l FROM %[1]s WHERE g = t.k) FROM t`,
				`SELECT t.id, (SELECT l FROM %[1]s WHERE g = t.k ORDER BY %[2]s) FROM t`},
			{`SELECT t.id, (SELECT l FROM %[1]s WHERE g = t.k AND h = t.j) FROM t`,
				`SELECT t.id, (SELECT l FROM %[1]s WHERE g = t.k AND h = t.j ORDER BY id) FROM t`},
			{`SELECT t.id, (SELECT group_concat(id) FROM %[1]s WHERE g = t.k) FROM t WHERE t.id %% 10 = 3`,
				`SELECT t.id, (SELECT group_concat(id) FROM (SELECT id FROM %[1]s WHERE g = t.k ORDER BY g, h DESC, id)) FROM t WHERE t.id %% 10 = 3`},
			{`SELECT t.id, (SELECT group_concat(l) FROM %[1]s WHERE g = t.k + 0.0) FROM t WHERE t.k = 7`,
				`SELECT t.id, (SELECT group_concat(l) FROM (SELECT l FROM %[1]s WHERE g = t.k + 0.0 ORDER BY g, h DESC, id)) FROM t WHERE t.k = 7`},
		} {
			q, twin := fmt.Sprintf(q[0], b), fmt.Sprintf(q[1], b, order[b])
			if got, want := answer(q), answer(twin); got != want {
				t.Errorf("%s:\n got  %.300s\n want %.300s (%s)", q, got, want, twin)
			}
		}
	}
}
