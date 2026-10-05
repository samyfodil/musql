//go:build sqlite_fts5

// This file tests fts5's stored rank= arguments compiled to bytecode.
// The test verifies that rank argument values match the oracle and that
// bm25 weights are correctly applied.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// TestI4Fts5RankArgsCompiled verifies rank argument values and orderings match the oracle.
func TestI4Fts5RankArgsCompiled(t *testing.T) {
	for _, tc := range []struct {
		name string
		rank string
	}{
		// No arguments at all: the default, and the control that says the rest
		// of the harness is wired right.
		{"noargs", "bm25()"},
		// Column weights, heavily asymmetric in each direction so the ORDER of
		// the two values is observable: swapping them changes the answer.
		{"weight_a", "bm25(10.0, 1.0)"},
		{"weight_b", "bm25(1.0, 10.0)"},
		// A negative literal -- fts5's own rank grammar admits a leading sign
		// (fts5ConfigSkipLiteral, fts5_config.c:69), and it is the arm a
		// literal reader is most likely to drop.
		{"negative", "bm25(-1.0, 2.0)"},
		// An integer literal where a real is expected, and a third weight for
		// a column that does not exist (fts5 defaults a missing weight to 1.0
		// and ignores a surplus one -- fts5_aux.c:722).
		{"int_and_surplus", "bm25(2, 1, 7)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := i4Fts5RankOrder(t, tc.rank)
			want := i4Fts5RankOrderOracle(t, tc.rank)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("rank=%q: MATCH order diverges\n  go : %v\n  cgo: %v", tc.rank, got, want)
			}
			if len(want) != 3 {
				t.Fatalf("rank=%q: the oracle returned %d rows, want 3 -- the MATCH did not run, so this gate proved nothing", tc.rank, len(want))
			}
		})
	}
}

// TestI4Fts5RankArgsAreLoadBearing verifies that different rank arguments produce different orderings.
func TestI4Fts5RankArgsAreLoadBearing(t *testing.T) {
	a := strings.Join(i4Fts5RankOrder(t, "bm25(10.0, 1.0)"), ",")
	b := strings.Join(i4Fts5RankOrder(t, "bm25(1.0, 10.0)"), ",")
	if a == b {
		t.Fatalf("bm25(10,1) and bm25(1,10) give the same order (%s) on this corpus -- "+
			"TestI4Fts5RankArgsCompiled cannot tell a dropped argument list from a working one; "+
			"change the corpus, not this assertion", a)
	}
}

// i4Fts5Corpus is the three-document, two-column table both engines build. The
// documents are shaped so the query term's distribution across the two columns
// differs per row, which is what makes a per-column WEIGHT change the order.
var i4Fts5Corpus = []struct{ a, b string }{
	{"apple apple apple", "pear"},
	{"pear", "apple apple apple"},
	{"apple", "apple"},
}

func i4Fts5Setup(exec func(string, ...any) error) error {
	if err := exec(`CREATE VIRTUAL TABLE ft USING fts5(a, b)`); err != nil {
		return err
	}
	for _, d := range i4Fts5Corpus {
		if err := exec(`INSERT INTO ft(a, b) VALUES(?, ?)`, d.a, d.b); err != nil {
			return err
		}
	}
	return nil
}

// i4Fts5RankOrder runs the query on the pure-Go engine.
func i4Fts5RankOrder(t *testing.T, rank string) []string {
	t.Helper()
	dir := t.TempDir()
	db, err := engine.Create(filepath.Join(dir, "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Discard()
	exec := func(sqlText string, args ...any) error {
		vals := make([]engine.Value, len(args))
		for i, a := range args {
			vals[i] = engine.Value{Typ: engine.Text, S: []byte(a.(string))}
		}
		_, _, e := db.ExecArgs(sqlText, vals)
		return e
	}
	if err := i4Fts5Setup(exec); err != nil {
		t.Fatalf("engine setup: %v", err)
	}
	if err := exec(`INSERT INTO ft(ft, rank) VALUES('rank', ?)`, rank); err != nil {
		t.Fatalf("engine rank=%q: %v", rank, err)
	}
	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pager.Close()
	_, rows, err := pager.QueryArgs(`SELECT rowid FROM ft WHERE ft MATCH 'apple' ORDER BY rank`, nil)
	if err != nil {
		t.Fatalf("engine query (rank=%q): %v", rank, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%d", r[0].I))
	}
	return out
}

// i4Fts5RankOrderOracle runs the identical query on C SQLite.
func i4Fts5RankOrderOracle(t *testing.T, rank string) []string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	exec := func(sqlText string, args ...any) error {
		_, e := db.Exec(sqlText, args...)
		return e
	}
	if err := i4Fts5Setup(exec); err != nil {
		t.Fatalf("oracle setup: %v", err)
	}
	if err := exec(`INSERT INTO ft(ft, rank) VALUES('rank', ?)`, rank); err != nil {
		t.Fatalf("oracle rank=%q: %v", rank, err)
	}
	rows, err := db.Query(`SELECT rowid FROM ft WHERE ft MATCH 'apple' ORDER BY rank`)
	if err != nil {
		t.Fatalf("oracle query (rank=%q): %v", rank, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%d", id))
	}
	return out
}
