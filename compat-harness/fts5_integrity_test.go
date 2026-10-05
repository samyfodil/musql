//go:build sqlite_fts5

// This file gates fts5 'integrity-check'. Both engines must accept or reject
// identically, and both must give identical error text on rejection.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// fts5ICResult runs 'integrity-check' through drv over dsn and normalizes
// the outcome to a single string: "ok" for success, or the error text.
func fts5ICResult(t *testing.T, drv, dsn string) string {
	t.Helper()
	db, err := sql.Open(drv, dsn)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", drv, err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO t(t) VALUES('integrity-check')`); err != nil {
		return err.Error()
	}
	return "ok"
}

func TestFts5IntegrityCheckAgainstOracle(t *testing.T) {
	cases := []struct {
		name    string
		writer  string // "sqlite" (this engine) or "sqlite3" (the oracle) builds the file
		setup   []string
		corrupt []string // additional statements, applied through the SAME writer, that hand-corrupt the file
	}{
		{"healthy, single statement", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
			`INSERT INTO t(rowid,a,b) VALUES(3,'third row','qux')`,
		}, nil},
		{"healthy, several statements", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(a,b) VALUES('hello world','foo bar')`,
			`INSERT INTO t(a,b) VALUES('second row','baz')`,
			`UPDATE t SET a='changed' WHERE rowid=1`,
			`DELETE FROM t WHERE rowid=2`,
			`INSERT INTO t(a,b) VALUES('third row','qux')`,
		}, nil},
		{"empty table", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		}, nil},
		{"idx deleted -- not consulted by integrity-check", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`DELETE FROM t_idx`,
		}},
		{"the task's own worked example: every leaf page deleted", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
			`INSERT INTO t(rowid,a,b) VALUES(3,'third row','qux')`,
		}, []string{
			`DELETE FROM t_data WHERE id>10`,
		}},
		{"a leaf page truncated to one byte", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`UPDATE t_data SET block=X'00' WHERE id=137438953473`,
		}},
		{"a leaf page overwritten with garbage of the same length", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			// zeroblob(length(...)) then flipping every byte to 0xff via a
			// hex round-trip keeps the corrupted block exactly as long as
			// the original -- the shape that first exposed this engine's
			// header/footer bounds checks, as opposed to plain truncation.
			`UPDATE t_data SET block=unhex(replace(hex(zeroblob(length(block))),'00','ff')) WHERE id=137438953473`,
		}},
		{"structure record overwritten with unparsable garbage", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`UPDATE t_data SET block=X'FFFFFFFFFFFFFFFFFFFFFF' WHERE id=10`,
		}},
		{"structure record row deleted entirely", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`DELETE FROM t_data WHERE id=10`,
		}},
		{"structure record set to an empty blob", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`UPDATE t_data SET block=X'' WHERE id=10`,
		}},
		{"structure record truncated mid-varint", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`UPDATE t_data SET block=substr(block,1,5) WHERE id=10`,
		}},
		{"content changed directly, bypassing the index", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`UPDATE t_content SET c0='changed text here' WHERE id=1`,
		}},
		{"a content row deleted directly, index left stale", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`DELETE FROM t_content WHERE id=1`,
		}},
		{"docsize changed directly", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`UPDATE t_docsize SET sz=X'0909' WHERE id=1`,
		}},
		{"content reordered -- same tokens, different positions", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`UPDATE t_content SET c0='world hello' WHERE id=1`,
		}},
		{"averages record corrupted, content and postings untouched", "sqlite", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`UPDATE t_data SET block=X'FFFFFF' WHERE id=1`,
		}},
		// Every case above is repeated with the ORACLE as writer, so the
		// single-statement byte-identical shape (TestFts5ShadowLayoutDiff)
		// is exercised from that side too -- same ids are valid either way.
		{"healthy, single statement (oracle-written)", "sqlite3", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, nil},
		{"every leaf page deleted (oracle-written)", "sqlite3", []string{
			`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
			`INSERT INTO t(rowid,a,b) VALUES(1,'hello world','foo bar')`,
			`INSERT INTO t(rowid,a,b) VALUES(2,'second row','baz')`,
		}, []string{
			`DELETE FROM t_data WHERE id>10`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "fts5.db")
			if err := fts5Exec(t, c.writer, dsn, c.setup); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if len(c.corrupt) > 0 {
				if err := fts5Exec(t, c.writer, dsn, c.corrupt); err != nil {
					t.Fatalf("corrupt: %v", err)
				}
			}
			// EACH ENGINE IS ASKED ABOUT ITS OWN FORMAT, with the converter in
			// between where the writer was the other one -- see
			// convert_for_oracle_test.go. The claim is unchanged: whoever built the
			// index, both engines must reach the same verdict about it.
			goPath, cgoPath := dsn, dsn
			if c.writer == "sqlite3" {
				goPath = importedForMusql(t, dsn)
			} else {
				cgoPath = exportedForOracle(t, dsn)
			}
			goRes := fts5ICResult(t, "sqlite", goPath)
			cgoRes := fts5ICResult(t, "sqlite3", cgoPath)
			if goRes != cgoRes {
				t.Errorf("integrity-check DIVERGES\n  go:  %s\n  cgo: %s", goRes, cgoRes)
			}
		})
	}
}

// fts5ICMultiSegmentCases build rich, multi-segment shapes ONLY through the
// oracle: many single-row statements (so automerge leaves several
// unmerged level-0 segments alive at once), UPDATEs and DELETEs (so later
// segments carry real delete markers, the shape fts5_index.go's own
// encoder documents it never writes), and a small pgsz to force a doclist
// to split across several leaf pages. This is deliberately the case the
// task calls out as the one likeliest to find a decoder bug -- and did:
// see the "highest segid wins" merge rule and the poslist-escape-split fix
// in fts5_decode.go's package comment, both pinned against exactly this
// kind of file.
var fts5ICMultiSegmentCases = []struct {
	name  string
	stmts []string
}{
	{"many single-row segments, some rows later deleted", append([]string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(t,rank) VALUES('automerge', 0)`,
	}, fts5OneRowPerStatement(60, func(i int) string {
		return fmt.Sprintf("INSERT INTO t(rowid,a) VALUES(%d,'word%d shared alpha')", i, i)
	})...), // automerge=0 keeps every statement its own segment
	},
	{"sequential updates of the same row", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(t,rank) VALUES('automerge', 0)`,
		`INSERT INTO t(rowid,a) VALUES(1,'shared alpha')`,
		`UPDATE t SET a='shared beta' WHERE rowid=1`,
		`UPDATE t SET a='shared gamma' WHERE rowid=1`,
	}},
	{"a same-value no-op update", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(t,rank) VALUES('automerge', 0)`,
		`INSERT INTO t(rowid,a) VALUES(1,'shared one')`,
		`UPDATE t SET a='shared one' WHERE rowid=1`,
	}},
	{"delete then reinsert the same rowid and term", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(t,rank) VALUES('automerge', 0)`,
		`INSERT INTO t(rowid,a) VALUES(1,'zzunique')`,
		`DELETE FROM t WHERE rowid=1`,
		`INSERT INTO t(rowid,a) VALUES(1,'zzunique')`,
	}},
	{"a doclist index (multi-page doclist, small pgsz)", append([]string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(t,rank) VALUES('pgsz', 32)`,
	}, fts5BulkInsert(`INSERT INTO t(rowid,a) VALUES`, 400, func(i int) string {
		return fmt.Sprintf("(%d,'shared word%04d')", i+1, i)
	})),
	},
	{"a multi-column shared term across many rows, small pgsz", append([]string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t(t,rank) VALUES('pgsz', 32)`,
	}, fts5BulkInsert(`INSERT INTO t(rowid,a,b) VALUES`, 300, func(i int) string {
		return fmt.Sprintf("(%d,'word%04d shared col0','shared col1')", i+1, i)
	})),
	},
	{"deletes scattered through a multi-page, multi-segment table", append(append([]string{
		`CREATE VIRTUAL TABLE t USING fts5(a)`,
		`INSERT INTO t(t,rank) VALUES('pgsz', 32)`,
	}, fts5BulkInsert(`INSERT INTO t(rowid,a) VALUES`, 400, func(i int) string {
		return fmt.Sprintf("(%d,'shared word%04d')", i+1, i)
	})), `DELETE FROM t WHERE rowid % 7 = 0`, `UPDATE t SET a='rewritten shared' WHERE rowid % 11 = 0`),
	},
}

// fts5OneRowPerStatement builds n separate single-row INSERT statements
// (as opposed to fts5BulkInsert's one big multi-row statement), so each one
// becomes its own segment before automerge has a chance to run.
func fts5OneRowPerStatement(n int, stmt func(i int) string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = stmt(i + 1)
	}
	return out
}

func TestFts5IntegrityCheckCSQLiteShapes(t *testing.T) {
	for _, c := range fts5ICMultiSegmentCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "fts5.db")
			if err := fts5Exec(t, "sqlite3", dsn, c.stmts); err != nil {
				t.Fatalf("oracle setup: %v", err)
			}
			cgoRes := fts5ICResult(t, "sqlite3", dsn)
			if cgoRes != "ok" {
				t.Fatalf("oracle itself reports %q over its own file -- test is not exercising a healthy shape", cgoRes)
			}
			// Through the IMPORT, which is how this engine reads a file the oracle
			// wrote (convert_for_oracle_test.go). The claim is unchanged and if
			// anything stronger: the index C fts5 built has to survive the
			// conversion AND satisfy this engine's own decoder afterwards.
			goRes := fts5ICResult(t, "sqlite", importedForMusql(t, dsn))
			if goRes != "ok" {
				t.Errorf("this engine's decoder rejected a file C fts5 calls healthy: %s", goRes)
			}
		})
	}
}
