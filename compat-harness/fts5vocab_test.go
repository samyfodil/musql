//go:build sqlite_fts5

// This file gates the fts5vocab module (engine/vtab_fts5vocab.go) against
// C SQLite 3.53.3 -DSQLITE_ENABLE_FTS5, the same oracle
// fts5_diff_test.go uses and by the same means: fts5Exec/fts5Query drive
// each engine in-process over its own database/sql connection, never
// run()/runWithDSN() (those spawn worker binaries built without the
// sqlite_fts5 tag -- see fts5_diff_test.go's package comment).
package compat

import (
	"path/filepath"
	"testing"
)

func TestFts5VocabAnswerDiff(t *testing.T) {
	cases := []struct {
		name   string
		setup  []string
		probes []string
		// allowDecline marks a group this engine currently DECLINES -- see
		// TestFts5AnswerDiff's own field for why that is logged, not failed.
		allowDecline bool
	}{
		{
			name: "row/col/instance shapes, columns and ordering",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('one two two','two three')`,
				`INSERT INTO t VALUES('one','four')`,
				`INSERT INTO t VALUES('five six','six six')`,
				`CREATE VIRTUAL TABLE vrow USING fts5vocab(t, 'row')`,
				`CREATE VIRTUAL TABLE vcol USING fts5vocab(t, 'col')`,
				`CREATE VIRTUAL TABLE vinst USING fts5vocab(t, 'instance')`,
			},
			probes: []string{
				`PRAGMA table_info(vrow)`,
				`PRAGMA table_info(vcol)`,
				`PRAGMA table_info(vinst)`,
				// PRAGMA table_xinfo of a virtual table is served for the
				// modules whose DECLARED column list has been verified against
				// the oracle (vtabDeclaredColumnsAsSQLiteReportsThem,
				// engine/pragma.go); fts5vocab is not one of them yet, so it
				// still declines and stays out of scope here.
				`SELECT rowid, * FROM vrow ORDER BY rowid`,
				`SELECT rowid, * FROM vcol ORDER BY rowid`,
				`SELECT rowid, * FROM vinst ORDER BY rowid`,
				`SELECT typeof(term), typeof(doc), typeof(cnt) FROM vrow LIMIT 1`,
				`SELECT typeof(col) FROM vcol LIMIT 1`,
				`SELECT sql FROM sqlite_master WHERE name IN ('vrow','vcol','vinst') ORDER BY name`,
			},
		},
		{
			// A term repeated several times in ONE row must count 1 document
			// (not 3), and cnt must still count every occurrence.
			name: "a repeated term counts one document, every occurrence",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('dup dup dup')`,
				`INSERT INTO t VALUES('dup once')`,
				`CREATE VIRTUAL TABLE vrow USING fts5vocab(t, 'row')`,
				`CREATE VIRTUAL TABLE vcol USING fts5vocab(t, 'col')`,
				`CREATE VIRTUAL TABLE vinst USING fts5vocab(t, 'instance')`,
			},
			probes: []string{
				`SELECT * FROM vrow ORDER BY term`,
				`SELECT * FROM vcol ORDER BY term`,
				`SELECT * FROM vinst ORDER BY term, doc, offset`,
			},
		},
		{
			// 'col' and 'instance' order by the column's DECLARATION index,
			// not its name -- these columns are named backwards
			// alphabetically on purpose.
			name: "column order follows declaration, not the column name",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(zed, alpha)`,
				`INSERT INTO t VALUES('shared word', 'shared word')`,
				`CREATE VIRTUAL TABLE vcol USING fts5vocab(t, 'col')`,
				`CREATE VIRTUAL TABLE vinst USING fts5vocab(t, 'instance')`,
			},
			probes: []string{
				`SELECT rowid, * FROM vcol ORDER BY rowid`,
				`SELECT rowid, * FROM vinst ORDER BY rowid`,
			},
		},
		{
			// 'instance' orders by DOC first, column second: a term split
			// across two rows in swapped columns (row 1 col b, row 2 col a)
			// must list the row-1 instance before the row-2 one.
			name: "instance order is doc-major, not column-major",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t(rowid,a,b) VALUES(1, 'other', 'x')`,
				`INSERT INTO t(rowid,a,b) VALUES(2, 'x', 'other')`,
				`CREATE VIRTUAL TABLE vinst USING fts5vocab(t, 'instance')`,
			},
			probes: []string{
				`SELECT rowid, * FROM vinst WHERE term='x' ORDER BY rowid`,
			},
		},
		{
			name: "a NULL column contributes nothing",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t(rowid, a, b) VALUES(1, 'hello', NULL)`,
				`CREATE VIRTUAL TABLE vrow USING fts5vocab(t, 'row')`,
			},
			probes: []string{
				`SELECT * FROM vrow ORDER BY term`,
			},
		},
		{
			// The index the module reads is %_content, so a DELETEd or
			// UPDATEd row must be reflected on the very next query -- no
			// caching across statements.
			name: "delete and update are reflected immediately",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t(rowid,a) VALUES(1,'alpha beta')`,
				`INSERT INTO t(rowid,a) VALUES(2,'beta gamma')`,
				`CREATE VIRTUAL TABLE vrow USING fts5vocab(t, 'row')`,
			},
			probes: []string{
				`SELECT * FROM vrow ORDER BY term`,
				`DELETE FROM t WHERE rowid=1`,
				`SELECT * FROM vrow ORDER BY term`,
				`UPDATE t SET a='delta' WHERE rowid=2`,
				`SELECT * FROM vrow ORDER BY term`,
			},
		},
		{
			// Real fts5vocab creates the table without checking the target and
			// only errors when it is READ -- including after the target is
			// dropped -- exactly like fts4aux.
			name: "a missing, non-fts5 or dropped target fails at SELECT, not CREATE",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('keep me')`,
				`CREATE TABLE plain(a)`,
				`CREATE VIRTUAL TABLE v1 USING fts5vocab(does_not_exist, 'row')`,
				`CREATE VIRTUAL TABLE v2 USING fts5vocab(plain, 'row')`,
				`CREATE VIRTUAL TABLE v3 USING fts5vocab(t, 'row')`,
			},
			probes: []string{
				`SELECT * FROM v1`,
				`SELECT * FROM v2`,
				`SELECT * FROM v3`,
			},
		},
		{
			name: "dropping the target fts5 table is an error, not stale rows",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('keep me')`,
				`CREATE VIRTUAL TABLE vrow USING fts5vocab(t, 'row')`,
			},
			probes: []string{
				`SELECT * FROM vrow`,
				`DROP TABLE t`,
				`SELECT * FROM vrow`,
			},
		},
		{
			// Wrong argument counts (0, 1, 3 -- including the "schema, table,
			// type" 3-argument spelling) all fail at CREATE, and an
			// unrecognized type string fails at CREATE too. Each CREATE is
			// its own probe (not setup): every one of them is expected to
			// FAIL, and setup aborts the whole case on its first error.
			name: "argument-count and type-string errors at CREATE",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
			},
			probes: []string{
				`CREATE VIRTUAL TABLE v1 USING fts5vocab()`,
				`CREATE VIRTUAL TABLE v2 USING fts5vocab(t)`,
				`CREATE VIRTUAL TABLE v3 USING fts5vocab(t, 'row', 'extra')`,
				`CREATE VIRTUAL TABLE v4 USING fts5vocab(main, t, 'row')`,
				`CREATE VIRTUAL TABLE v5 USING fts5vocab(t, 'bogus')`,
				`CREATE VIRTUAL TABLE v6 USING fts5vocab(t, '')`,
			},
		},
		{
			// The type argument is matched case-insensitively and needs no
			// quoting, and the target may be simply-quoted.
			name: "case-insensitive, unquoted type and quoted target",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('hello')`,
				`CREATE VIRTUAL TABLE va USING fts5vocab(t, 'ROW')`,
				`CREATE VIRTUAL TABLE vb USING fts5vocab('t', 'row')`,
				`CREATE VIRTUAL TABLE vc USING fts5vocab(t, row)`,
				`CREATE VIRTUAL TABLE vd USING fts5vocab( t , 'row' )`,
			},
			probes: []string{
				`SELECT * FROM va`,
				`SELECT * FROM vb`,
				`SELECT * FROM vc`,
				`SELECT * FROM vd`,
			},
		},
		{
			// A schema-qualified target ("main.t") is passed through as ONE
			// literal argument, exactly like fts4aux -- it is not split into
			// schema + table, so it never resolves to the real table "t".
			name: "a schema-qualified target argument is not split",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('hello')`,
				`CREATE VIRTUAL TABLE vc USING fts5vocab(main.t, 'row')`,
			},
			probes: []string{
				`SELECT * FROM vc`,
			},
		},
		{
			// The term constraint is pushed into the term scan and compared
			// as RAW BYTES there, identically to fts4aux's own term
			// constraint (vtab_fts3aux.go) -- this is the SAME probe
			// fts4aux_test.go uses, retargeted at fts5vocab.
			name: "a term constraint is compared as raw bytes",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('a b c 234567890123456789')`,
				`CREATE VIRTUAL TABLE x USING fts5vocab(t, 'row')`,
			},
			probes: []string{
				`SELECT term FROM x ORDER BY term`,
				`SELECT term FROM x WHERE term<X'625f003334'`,
				`SELECT term FROM x WHERE term<X'6200'`,
				`SELECT term FROM x WHERE term<=X'62'`,
				`SELECT term FROM x WHERE term>X'62'`,
				`SELECT term FROM x WHERE term=X'62'`,
				`SELECT term FROM x WHERE term>5`,
				`SELECT term FROM x WHERE term<5`,
				`SELECT term FROM x WHERE term<2.5`,
				`SELECT term FROM x WHERE term=234567890123456789`,
				`SELECT term FROM x WHERE term<NULL`,
				`SELECT term FROM x WHERE term>'a' AND term<'c'`,
				`SELECT term FROM x WHERE term BETWEEN X'61' AND X'62'`,
				`SELECT term FROM x WHERE term=x''`,
				`SELECT term FROM x WHERE term>x''`,
				`SELECT term FROM x WHERE term>=x'61' AND term<=x'63'`,
			},
		},
		{
			// The 'col' column is a plain TEXT column name: ordinary SQL
			// comparison (no byte trick, and no aggregate '*' row -- unlike
			// fts4aux's 'col').
			name: "col is compared as ordinary text, with no aggregate row",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('one two', 'three four')`,
				`CREATE VIRTUAL TABLE x USING fts5vocab(t, 'col')`,
			},
			probes: []string{
				`SELECT * FROM x WHERE col='a'`,
				`SELECT * FROM x WHERE col=0`,
				`SELECT * FROM x WHERE col='*'`,
			},
		},
		{
			name: "an empty target table yields no rows",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`CREATE VIRTUAL TABLE vrow USING fts5vocab(t, 'row')`,
			},
			probes: []string{
				`SELECT * FROM vrow`,
				`SELECT count(*) FROM vrow`,
			},
		},
		{
			name: "an fts5vocab table is read-only",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('w')`,
				`CREATE VIRTUAL TABLE vrow USING fts5vocab(t, 'row')`,
			},
			probes: []string{
				`INSERT INTO vrow VALUES('x',1,1)`,
				`DELETE FROM vrow`,
				`UPDATE vrow SET cnt=9`,
				`SELECT * FROM vrow`,
			},
		},
		{
			name: "dropping the fts5vocab table leaves the fts5 table alone",
			setup: []string{
				`CREATE VIRTUAL TABLE t USING fts5(a)`,
				`INSERT INTO t VALUES('keep me')`,
				`CREATE VIRTUAL TABLE vrow USING fts5vocab(t, 'row')`,
				`DROP TABLE vrow`,
			},
			probes: []string{
				`SELECT type, name FROM sqlite_master ORDER BY name`,
				`SELECT rowid, a FROM t`,
				`SELECT rowid FROM t WHERE t MATCH 'keep'`,
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], c.setup); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			for _, p := range c.probes {
				goOut, goErr := fts5Query(t, "sqlite", dsn["sqlite"], p)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], p)
				switch {
				case goErr != nil && cgoErr != nil:
					// Both decline: agreement, not a divergence.
				case goErr != nil && c.allowDecline:
					t.Logf("declined (tracked, not wrong): %s\n  err: %v", p, goErr)
				case goErr != nil:
					t.Errorf("%s: this engine declined a query C fts5vocab answers\n  sql: %s\n  err: %v\n  cgo: %s", c.name, p, goErr, cgoOut)
				case cgoErr != nil:
					t.Errorf("%s: this engine ACCEPTED a query C fts5vocab rejects\n  sql: %s\n  go:  %s\n  cgo err: %v", c.name, p, goOut, cgoErr)
				case goOut != cgoOut:
					t.Errorf("%s DIVERGES from C fts5vocab\n  sql: %s\n  go:  %q\n  cgo: %q", c.name, p, goOut, cgoOut)
				}
			}
		})
	}
}
