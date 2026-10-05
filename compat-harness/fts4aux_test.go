// This file gates the fts4aux module (engine/vtab_fts3aux.go) against real C
// SQLite 3.53.3. fts4aux exposes an fts3/fts4 table's term index as rows, so it
// is a second, independent reader of the same segment bytes -- which makes it a
// sharp check on the index itself as well as on the module: every count below
// is recomputed from the encoded doclists.
package compat

import (
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestFts4auxMatchesCSQLite(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"columns, ordering and the two types of col", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, b)`,
			`INSERT INTO t VALUES('one two two','two three')`,
			`INSERT INTO t VALUES('one','four')`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`SELECT sql FROM sqlite_master ORDER BY name`,
			`SELECT rowid, * FROM x`,
			`SELECT term, col, documents, occurrences, typeof(col), typeof(documents) FROM x ORDER BY term, col`,
			`SELECT languageid FROM x`,
			`SELECT * FROM x WHERE languageid=0`,
		}},
		{"constraints and aggregates push through", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a, b)`,
			`INSERT INTO t VALUES('one two two','two three')`,
			`INSERT INTO t VALUES('one','four')`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`SELECT * FROM x WHERE term='two'`,
			`SELECT * FROM x WHERE term>'one'`,
			`SELECT * FROM x WHERE col='*'`,
			`SELECT * FROM x WHERE col=0`,
			`SELECT count(*) FROM x`,
			`SELECT sum(occurrences) FROM x WHERE col='*'`,
			`SELECT count(*) FROM x, t`,
			`SELECT x.term FROM x JOIN t ON t.docid=1 ORDER BY x.term`,
		}},
		// The index the module reads is the one the DELETE markers live in, so a
		// term whose only document is gone must disappear entirely.
		{"delete markers remove a term", []string{
			`CREATE VIRTUAL TABLE t USING fts3(a)`,
			`INSERT INTO t VALUES('alpha beta')`,
			`INSERT INTO t VALUES('beta gamma')`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`SELECT * FROM x`,
			`DELETE FROM t WHERE docid=1`,
			`SELECT * FROM x`,
			`UPDATE t SET a='delta' WHERE docid=2`,
			`SELECT * FROM x`,
		}},
		// ...and after a level merge, where the markers may have been resolved
		// away entirely (engine/fts3_merge.go).
		{"the same counts after a level merge", concat(
			[]string{`CREATE VIRTUAL TABLE t USING fts4(a)`},
			fts4Inserts(1, 10),
			[]string{`DELETE FROM t WHERE docid=3`},
			fts4Inserts(11, 18),
			[]string{
				`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
				`SELECT level, count(*) FROM t_segdir GROUP BY level ORDER BY level`,
				`SELECT * FROM x WHERE term='common'`,
				`SELECT count(*) FROM x`,
				`SELECT term FROM x WHERE col='*' ORDER BY term`,
			},
		)},
		{"an empty fts table yields no rows", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`SELECT * FROM x`,
			`SELECT count(*) FROM x`,
		}},
		{"a quoted target name is dequoted", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('q')`,
			`CREATE VIRTUAL TABLE x USING fts4aux('t')`,
			`SELECT * FROM x`,
		}},
		// C SQLite creates the fts4aux table without checking the target and
		// only errors when it is READ -- including after the target is dropped.
		{"a missing or non-fts target fails at SELECT, not at CREATE", []string{
			`CREATE VIRTUAL TABLE x USING fts4aux(does_not_exist)`,
			`SELECT * FROM x`,
			`CREATE TABLE plain(a)`,
			`CREATE VIRTUAL TABLE y USING fts4aux(plain)`,
			`SELECT * FROM y`,
		}},
		{"the target being dropped is an error, not stale rows", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('dup dup dup')`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`SELECT * FROM x`,
			`DROP TABLE t`,
			`SELECT * FROM x`,
		}},
		// Both of these fail at CREATE against the oracle, so they must fail
		// here: the corpus uses the database-qualified form three times.
		{"the database-qualified and empty argument forms are refused", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`CREATE VIRTUAL TABLE x USING fts4aux(main, t)`,
			`CREATE VIRTUAL TABLE y USING fts4aux()`,
			`CREATE VIRTUAL TABLE z USING fts4aux(t, t)`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
		}},
		// The term constraint is pushed into the segment scan and compared as
		// RAW BYTES there, which differs from the SQL comparison whenever the
		// value is not TEXT (SQL orders every TEXT below every BLOB). fts3aux2
		// .test asks the first of these directly.
		{"a term constraint is compared as raw bytes", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('a b c 234567890123456789')`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`SELECT term FROM x WHERE col='*' ORDER BY term`,
			`SELECT term FROM x WHERE col='*' AND term<X'625f003334'`,
			`SELECT term FROM x WHERE col='*' AND term<X'6200'`,
			`SELECT term FROM x WHERE col='*' AND term<=X'62'`,
			`SELECT term FROM x WHERE col='*' AND term>X'62'`,
			`SELECT term FROM x WHERE col='*' AND term=X'62'`,
			`SELECT term FROM x WHERE col='*' AND term>5`,
			`SELECT term FROM x WHERE col='*' AND term<5`,
			`SELECT term FROM x WHERE col='*' AND term<2.5`,
			`SELECT term FROM x WHERE col='*' AND term=234567890123456789`,
			`SELECT term FROM x WHERE col='*' AND term<NULL`,
			`SELECT term FROM x WHERE col='*' AND term>'a' AND term<'c'`,
			`SELECT term FROM x WHERE col='*' AND term BETWEEN X'61' AND X'62'`,
			`SELECT term FROM x WHERE col='*' AND term=x''`,
			`SELECT term FROM x WHERE col='*' AND term>x''`,
			`SELECT term FROM x WHERE col='*' AND term>=x'61' AND term<=x'63'`,
		}},
		{"an fts4aux table is read-only", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('w')`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`INSERT INTO x VALUES('a','b',1,1)`,
			`DELETE FROM x`,
			`UPDATE x SET documents=9`,
			`SELECT * FROM x`,
		}},
		{"dropping the fts4aux table leaves the fts table alone", []string{
			`CREATE VIRTUAL TABLE t USING fts4(a)`,
			`INSERT INTO t VALUES('keep me')`,
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
			`DROP TABLE x`,
			`SELECT type, name FROM sqlite_master ORDER BY name`,
			`SELECT docid, a FROM t`,
			`SELECT docid FROM t WHERE t MATCH 'keep'`,
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) { differ(t, "fts4aux/"+tc.name, tc.stmts) })
	}
}

// TestFts4auxReadsCSQLitesIndex closes the loop the other way: C SQLite
// writes the fts4 database, and this engine's fts4aux recomputes every count
// from the segment bytes it never wrote.
func TestFts4auxReadsCSQLitesIndex(t *testing.T) {
	write := concat(
		[]string{`CREATE VIRTUAL TABLE t USING fts4(a, b)`},
		[]string{
			`INSERT INTO t VALUES('one two two','two three')`,
			`INSERT INTO t VALUES('one','four')`,
			`INSERT INTO t VALUES('five six','six six')`,
			`DELETE FROM t WHERE docid=2`,
			// Created by the WRITER: the read program below runs once per
			// engine against the same file, so creating it there would hit
			// "already exists" on the second pass.
			`CREATE VIRTUAL TABLE x USING fts4aux(t)`,
		},
	)
	read := []string{
		`SELECT rowid, * FROM x`,
		`SELECT count(*) FROM x`,
		`SELECT sum(occurrences) FROM x WHERE col='*'`,
	}
	for _, writer := range engineOrder {
		writer := writer
		t.Run("written-by-"+writer, func(t *testing.T) {
			dsn := fmt.Sprintf("%s/aux.db", t.TempDir())
			runWithDSN(t, writer, dsn, write)
			var baseline string
			// Each engine over its OWN format, with the converter in between where the
			// writer was the other one (convert_for_oracle_test.go).
			goPath, cgoPath := pathsForBothEngines(t, writerEngineName(writer), dsn)
			for _, reader := range engineOrder {
				readPath := goPath
				if reader == "cgo" {
					readPath = cgoPath
				}
				got := fmt.Sprintf("%v", runWithDSN(t, reader, readPath, read))
				if baseline == "" {
					baseline = got
					continue
				}
				if got != baseline {
					t.Errorf("[%s writes] readers disagree\n  %s\n  %s reads: %s", writer, baseline, reader, got)
				}
			}
		})
	}
}
