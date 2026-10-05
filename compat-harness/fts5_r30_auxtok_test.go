//go:build sqlite_fts5

// Tests fts5 auxiliary functions (highlight, snippet, bm25) use the table's
// tokenizer, not the default one. Every query must match the oracle exactly.
package compat

import (
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// fts5R30AuxTokSpecs tests various tokenizers that differ from the default
var fts5R30AuxTokSpecs = []string{
	`tokenize=trigram`,
	`tokenize='trigram case_sensitive 1'`,
	`tokenize=ascii`,
	`tokenize='ascii tokenchars ''-'''`,
	`tokenize='unicode61 remove_diacritics 0'`,
	`tokenize='unicode61 tokenchars ''-'''`,
	`tokenize='unicode61 categories ''L*'''`,
	`tokenize=porter`,
	`tokenize='porter ascii'`,
}

var fts5R30AuxTokRows = []string{
	`(1,'abcdef','one')`,
	`(2,'café here','two')`,
	`(3,'cafe here','three')`,
	`(4,'HELLOS hello','four')`,
	`(5,'running runs run','five')`,
	`(6,'well-known word','six')`,
	`(7,'AT&T 123 abc','seven')`,
	`(8,'the quick brown fox jumps over the lazy dog again and again','eight')`,
}

// fts5R30AuxTokProbes tests auxiliary functions across different queries
var fts5R30AuxTokProbes = []string{
	`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'bcd' ORDER BY rowid`,
	`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'cafe' ORDER BY rowid`,
	`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'caf' ORDER BY rowid`,
	`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'hello' ORDER BY rowid`,
	`SELECT rowid, highlight(t,0,'[',']') FROM t WHERE t MATCH 'runs' ORDER BY rowid`,
	`SELECT rowid, highlight(t,0,'<','>') FROM t WHERE t MATCH 'known' ORDER BY rowid`,
	`SELECT rowid, highlight(t,-1,'[',']') FROM t WHERE t MATCH 'the' ORDER BY rowid`,
	`SELECT rowid, snippet(t,0,'[',']','...',4) FROM t WHERE t MATCH 'bcd' ORDER BY rowid`,
	`SELECT rowid, snippet(t,0,'[',']','...',6) FROM t WHERE t MATCH 'the' ORDER BY rowid`,
	`SELECT rowid, snippet(t,-1,'[',']','...',3) FROM t WHERE t MATCH 'hello' ORDER BY rowid`,
	`SELECT rowid, snippet(t,0,'[',']','...',5) FROM t ORDER BY rowid`,
	`SELECT rowid, highlight(t,0,'[',']') FROM t ORDER BY rowid`,
	`SELECT rowid, round(bm25(t),6) FROM t WHERE t MATCH 'hello' ORDER BY rowid`,
	`SELECT rowid, round(bm25(t),6) FROM t WHERE t MATCH 'the' ORDER BY rowid`,
	`SELECT rowid, round(rank,6) FROM t WHERE t MATCH 'the' ORDER BY rank, rowid`,
}

// TestFts5R30AuxTokenizer runs every aux probe over every tokenizer
func TestFts5R30AuxTokenizer(t *testing.T) {
	for _, spec := range fts5R30AuxTokSpecs {
		spec := spec
		t.Run(spec, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b, ` + spec + `)`,
				`INSERT INTO t(rowid,a,b) VALUES` + joinRows(fts5R30AuxTokRows),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range fts5R30AuxTokProbes {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil && cgoErr != nil {
					continue
				}
				if goErr != nil || cgoErr != nil {
					t.Errorf("one engine answered and the other did not\n  sql: %s\n  this engine: %v\n  C SQLite: %v", q, goErr, cgoErr)
					continue
				}
				if goOut != cgoOut {
					t.Errorf("fts5 auxiliary function DIVERGES\n  sql: %s\n%s", q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// joinRows is strings.Join with a comma, kept local so this file adds no
// dependency on the tokenizer gate's own corpus helpers.
func joinRows(rows []string) string {
	out := ""
	for i, r := range rows {
		if i > 0 {
			out += ","
		}
		out += r
	}
	return out
}
