//go:build sqlite_fts5

// This file tests fts5's column filter syntax on parenthesized groups.
// Filters are pushed down into phrases and NEAR sets, with proper intersection
// handling.
package compat

import (
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// fts5R30ColsetRows spread the terms across three columns so that a filter
// naming the wrong column is visibly different from one naming the right one,
// and so that a term appearing in TWO columns can distinguish "matched in a"
// from "matched anywhere".
var fts5R30ColsetRows = []string{
	`(1,'alpha bravo','charlie delta','echo')`,
	`(2,'charlie','alpha','bravo')`,
	`(3,'delta echo','bravo alpha','charlie')`,
	`(4,'alpha','alpha','alpha')`,
	`(5,'bravo charlie delta','echo alpha','bravo')`,
	`(6,'zulu','yankee','xray')`,
}

var fts5R30ColsetProbes = []string{
	// The shape itself, in each of its spellings.
	`SELECT rowid FROM t WHERE t MATCH 'a:(alpha OR bravo)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{a}:(alpha OR bravo)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{a b}:(alpha OR bravo)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{a b c}:(alpha AND bravo)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'b:(alpha NOT bravo)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'c:("alpha")' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:(alph*)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:(NEAR(alpha bravo))' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{a c}:(NEAR(bravo delta, 3) OR echo)' ORDER BY rowid`,
	// The filter pushes into EVERY leaf, however deep.
	`SELECT rowid FROM t WHERE t MATCH 'a:((alpha OR bravo) AND (charlie OR delta))' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{a b}:((alpha AND bravo) OR (charlie AND delta))' ORDER BY rowid`,
	// ...and INTERSECTS one a leaf already carries, including down to the
	// empty set, which matches nothing rather than everything.
	`SELECT rowid FROM t WHERE t MATCH 'a:(b:alpha)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:(a:alpha)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{a b}:(b:alpha OR c:charlie)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{a b}:({b c}:alpha)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:(b:(alpha OR bravo))' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:(a:(alpha OR b:bravo))' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:(c:NEAR(alpha bravo))' ORDER BY rowid`,
	// An empty-intersection leaf under OR must not swallow its sibling, and
	// under NOT must not remove everything.
	`SELECT rowid FROM t WHERE t MATCH 'a:(b:alpha OR a:bravo)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:(alpha NOT b:alpha)' ORDER BY rowid`,
	// The unfiltered forms, so the filtered answers are read against a
	// baseline rather than against themselves.
	`SELECT rowid FROM t WHERE t MATCH '(alpha OR bravo)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'alpha' ORDER BY rowid`,
	// The auxiliary functions read the same phrase list, so a colset pushed to
	// the wrong leaf shows up in what gets marked.
	`SELECT rowid, highlight(t,0,'[',']'), highlight(t,1,'[',']') FROM t WHERE t MATCH 'a:(alpha OR bravo)' ORDER BY rowid`,
	`SELECT rowid, highlight(t,0,'[',']'), highlight(t,1,'[',']') FROM t WHERE t MATCH '{a b}:(b:alpha OR c:charlie)' ORDER BY rowid`,
	`SELECT rowid, round(bm25(t),6) FROM t WHERE t MATCH 'a:(alpha OR bravo)' ORDER BY rowid`,
	// Shapes C fts5 REJECTS: a colset before something that is not a group,
	// an unknown column, an empty colset.
	`SELECT rowid FROM t WHERE t MATCH 'nosuchcol:(alpha)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{a nosuchcol}:(alpha)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH '{}:(alpha)' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:(' ORDER BY rowid`,
	`SELECT rowid FROM t WHERE t MATCH 'a:()' ORDER BY rowid`,
}

func TestFts5R30ColumnFilterOnGroup(t *testing.T) {
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	stmts := []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b, c)`,
		`INSERT INTO t(rowid,a,b,c) VALUES` + joinRows(fts5R30ColsetRows),
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
			t.Fatalf("%s: %v", drv, err)
		}
	}
	for _, q := range fts5R30ColsetProbes {
		goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
		cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
		if goErr != nil && cgoErr != nil {
			continue
		}
		if goErr == nil && cgoErr != nil {
			t.Errorf("this engine ANSWERED a MATCH C fts5 rejects\n  sql: %s\n  go: %s\n  cgo err: %v", q, goOut, cgoErr)
			continue
		}
		if goErr != nil {
			t.Errorf("this engine declined a MATCH C fts5 answers\n  sql: %s\n  err: %v\n  cgo: %s", q, goErr, cgoOut)
			continue
		}
		if goOut != cgoOut {
			t.Errorf("column-filtered group DIVERGES\n  sql: %s\n%s", q, fts5DiffLines(goOut, cgoOut))
		}
	}
}
