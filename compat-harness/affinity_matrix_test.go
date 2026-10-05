package compat

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// The comparison-affinity matrix: six declared affinities, eighteen values
// each, and five comparison shapes -- run once over a table SCAN and again
// with an INDEX on the column, which must answer the same.
//
// 558 statements each way. It is what found the TRUE/FALSE keyword bug
// (true_false_affinity_test.go): every other cell agreed, and the three
// shapes that did not all had a bare keyword on one side.
func affMismatch(t *testing.T, tag string, setup []string, stmts []string) {
	t.Helper()
	c, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m, err := sql.Open("sqlite", t.TempDir()+"/aff.db")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, s := range setup {
		if _, err := c.Exec(s); err != nil {
			t.Fatalf("cgo setup %s: %v", s, err)
		}
		if _, err := m.Exec(s); err != nil {
			t.Fatalf("musql setup %s: %v", s, err)
		}
	}
	for _, q := range stmts {
		cv, mv := affOne(c, q), affOne(m, q)
		if cv != mv {
			t.Errorf("[%s] %s\n  cgo:    %s\n  musql: %s", tag, q, cv, mv)
		}
	}
}

func affOne(db *sql.DB, q string) string {
	rows, err := db.Query(q)
	if err != nil {
		return "ERR:" + err.Error()
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var sb strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "SCANERR:" + err.Error()
		}
		for _, v := range vals {
			if b, ok := v.([]byte); ok {
				v = "b:" + string(b)
			}
			sb.WriteString(fmt.Sprintf("%v,", v))
		}
		sb.WriteString(";")
	}
	if rows.Err() != nil {
		return "ERR:" + rows.Err().Error()
	}
	return sb.String()
}

func TestAffinityComparisonMatrix(t *testing.T) {
	setup := []string{
		`CREATE TABLE ti(x INTEGER)`, `CREATE TABLE tt(x TEXT)`, `CREATE TABLE tr(x REAL)`,
		`CREATE TABLE tb(x BLOB)`, `CREATE TABLE tn(x NUMERIC)`, `CREATE TABLE tz(x)`,
	}
	vals := []string{"1", "'1'", "1.0", "'1.0'", "x'31'", "NULL", "''", "'abc'", "0", "'0'",
		"-0.0", "'  1 '", "1e0", "'1e0'", "9223372036854775807", "'9223372036854775807'", "true", "false"}
	for _, tbl := range []string{"ti", "tt", "tr", "tb", "tn", "tz"} {
		for _, v := range vals {
			setup = append(setup, fmt.Sprintf("INSERT INTO %s VALUES(%s)", tbl, v))
		}
	}
	var qs []string
	for _, tbl := range []string{"ti", "tt", "tr", "tb", "tn", "tz"} {
		qs = append(qs, fmt.Sprintf("SELECT quote(x), typeof(x) FROM %s ORDER BY rowid", tbl))
		for _, v := range vals {
			qs = append(qs,
				fmt.Sprintf("SELECT count(*) FROM %s WHERE x = %s", tbl, v),
				fmt.Sprintf("SELECT count(*) FROM %s WHERE x < %s", tbl, v),
				fmt.Sprintf("SELECT count(*) FROM %s WHERE x IS %s", tbl, v),
				fmt.Sprintf("SELECT count(*) FROM %s WHERE x IN (%s)", tbl, v),
				fmt.Sprintf("SELECT count(*) FROM %s WHERE x BETWEEN %s AND 9", tbl, v),
			)
		}
		qs = append(qs, fmt.Sprintf("SELECT group_concat(quote(x)) FROM (SELECT x FROM %s ORDER BY x)", tbl))
		qs = append(qs, fmt.Sprintf("SELECT count(DISTINCT x) FROM %s", tbl))
	}
	// ...and the same with an index, which must agree with the table scan.
	affMismatch(t, "table scan", setup, qs)
	idx := append(append([]string{}, setup...),
		`CREATE INDEX i1 ON ti(x)`, `CREATE INDEX i2 ON tt(x)`, `CREATE INDEX i3 ON tr(x)`,
		`CREATE INDEX i4 ON tb(x)`, `CREATE INDEX i5 ON tn(x)`, `CREATE INDEX i6 ON tz(x)`)
	affMismatch(t, "index", idx, qs)
}

