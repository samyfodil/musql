package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestWindowOrderNumericKeys: a window's ORDER BY over REALs, INTEGERs, both
// mixed, NULLs and TEXT, ascending and descending, ranks each row as the
// count of rows ahead of it does, and row_number() keeps ties in the order
// the rows came.
func TestWindowOrderNumericKeys(t *testing.T) {
	setup := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, p, f, i, m)`}
	for k := range 400 {
		f := fmt.Sprintf("%d.25", k%37-18)
		if k%41 == 0 {
			f = "NULL"
		}
		if k%53 == 0 {
			f = "-0.0"
		}
		m := fmt.Sprint(k % 9)
		switch k % 4 {
		case 1:
			m = fmt.Sprintf("%d.0", k%9)
		case 2:
			m = fmt.Sprintf("%d.5", k%9)
		case 3:
			m = fmt.Sprintf("'%d'", k%9)
		}
		setup = append(setup, fmt.Sprintf("INSERT INTO t VALUES(%d, %d, %s, %d, %s)", k, k%3, f, (k*7919)%23-11, m))
	}
	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path, setup...)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ask := func(q string) string {
		t.Helper()
		_, rows, err := p.QueryArgs(q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return fmt.Sprint(typedRows(rows))
	}
	for _, c := range []struct{ by, ahead string }{
		{"f", "u.f < t.f OR (u.f IS NULL AND t.f IS NOT NULL)"},
		{"f DESC", "u.f > t.f OR (t.f IS NULL AND u.f IS NOT NULL)"},
		{"i", "u.i < t.i"},
		{"i DESC", "u.i > t.i"},
		{"m", "u.m < t.m"},
		{"m DESC", "u.m > t.m"},
		{"i, f DESC", "u.i < t.i OR (u.i = t.i AND (u.f > t.f OR (t.f IS NULL AND u.f IS NOT NULL)))"},
	} {
		for _, part := range []string{"", "PARTITION BY p"} {
			same := ""
			if part != "" {
				same = "u.p = t.p AND "
			}
			got := ask(fmt.Sprintf("SELECT id, rank() OVER (%s ORDER BY %s) FROM t ORDER BY id", part, c.by))
			want := ask(fmt.Sprintf("SELECT id, 1 + (SELECT count(*) FROM t u WHERE %s(%s)) FROM t ORDER BY id", same, c.ahead))
			if got != want {
				t.Errorf("rank() OVER (%s ORDER BY %s):\n got  %.300s\n want %.300s", part, c.by, got, want)
			}
			got = ask(fmt.Sprintf("SELECT id, row_number() OVER (%s ORDER BY %s) FROM t ORDER BY id", part, c.by))
			want = ask(fmt.Sprintf("SELECT id, 1 + (SELECT count(*) FROM t u WHERE %s(coalesce((%s), 0) OR (NOT coalesce((%s), 0) AND NOT coalesce((%s), 0) AND u.id < t.id))) FROM t ORDER BY id",
				same, c.ahead, c.ahead, swapUT(c.ahead)))
			if got != want {
				t.Errorf("row_number() OVER (%s ORDER BY %s):\n got  %.300s\n want %.300s", part, c.by, got, want)
			}
		}
	}
}

func swapUT(s string) string {
	b := []byte(s)
	for i := 0; i+1 < len(b); i++ {
		if b[i+1] == '.' && (b[i] == 'u' || b[i] == 't') && (i == 0 || b[i-1] == ' ' || b[i-1] == '(') {
			if b[i] == 'u' {
				b[i] = 't'
			} else {
				b[i] = 'u'
			}
		}
	}
	return string(b)
}
