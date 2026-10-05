//go:build sqlite_fts5

// FTS5 tests that require the sqlite_fts5 build tag. Uses flLockstep to test
// against the oracle directly since differ() workers don't have the tag.
package compat

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestFts5R35ERankCommand verifies that the 'rank' command sets a default
// rank function and stores it correctly in %_config.
func TestFts5R35ERankCommand(t *testing.T) {
	flLockstep(t, "rank-bm25", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t VALUES('one two','three'),('one','one one one'),('two two','x y z')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'bm25(10.0,1.0)')`,
	},
		`SELECT k, quote(v) FROM t_config ORDER BY k`,
		`SELECT rowid, rank FROM t WHERE t MATCH 'one' ORDER BY rowid`,
		`SELECT rowid FROM t WHERE t MATCH 'one' ORDER BY rank`,
		`SELECT rowid, rank FROM t WHERE t MATCH 'one OR two' ORDER BY rowid`,
	)
	// A weightless bm25() must still be the default's exact equal, and the
	// stored row must appear even then.
	flLockstep(t, "rank-bm25-noargs", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t VALUES('one two','three'),('one','one one one')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'bm25()')`,
	},
		`SELECT k, quote(v) FROM t_config ORDER BY k`,
		`SELECT rowid, rank FROM t WHERE t MATCH 'one' ORDER BY rowid`,
	)
	// rank may name ANY auxiliary function, not just bm25 -- fts5FindRankFunction
	// looks the name up in the same table bm25()/highlight()/snippet() live in,
	// and passes the parsed arguments straight through.
	flLockstep(t, "rank-highlight", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t VALUES('one two','three'),('one','one one one')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'highlight(0,''<'',''>'')')`,
	},
		`SELECT k, quote(v) FROM t_config ORDER BY k`,
		`SELECT rowid, rank FROM t WHERE t MATCH 'one' ORDER BY rowid`,
	)
	flLockstep(t, "rank-snippet", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t VALUES('one two','three'),('one','one one one')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'snippet(1,''['','']'',''...'',3)')`,
	},
		`SELECT rowid, rank FROM t WHERE t MATCH 'one' ORDER BY rowid`,
	)
}

// TestFts5R35ERankValidation verifies rank string validation and function
// lookup behavior.
func TestFts5R35ERankValidation(t *testing.T) {
	flLockstep(t, "rank-syntax", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t VALUES('one two','three')`,
		// Accepted: trailing text after the ')' is ignored, and a name fts5
		// cannot resolve is still a well-formed rank string.
		`INSERT INTO t(t, rank) VALUES('rank', 'bm25()trailing')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'nosuchfunc()')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'bm25( 1.5 , -2 , ''x'' , NULL )')`,
		// Rejected: no argument list, an unclosed one, a non-literal argument,
		// and values that are not text at all.
		`INSERT INTO t(t, rank) VALUES('rank', 'garbage')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'bm25(')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'bm25(a+1)')`,
		`INSERT INTO t(t, rank) VALUES('rank', 5)`,
		`INSERT INTO t(t, rank) VALUES('rank', NULL)`,
		`INSERT INTO t(t, rank) VALUES('rank', '')`,
	},
		`SELECT k, quote(v) FROM t_config ORDER BY k`,
	)
	// "no such function: nosuchfunc" -- raised by the READ, not by the command
	// that stored the name.
	flLockstep(t, "rank-unresolved", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t VALUES('one two','three')`,
		`INSERT INTO t(t, rank) VALUES('rank', 'nosuchfunc()')`,
	},
		`SELECT rowid, rank FROM t WHERE t MATCH 'one'`,
		// With no MATCH at all the rank column is NULL and the function is
		// never looked up, so this one SUCCEEDS even with a bad rank stored.
		`SELECT rowid, rank FROM t`,
	)
}

// 'flush' writes fts5's pending in-memory index out to %_data. This engine has
// no pending buffer, so the command is a no-op -- but it must be an ACCEPTED
// no-op.
func TestFts5R35EFlushCommand(t *testing.T) {
	flLockstep(t, "flush", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t VALUES('one two','three'),('one','one one one')`,
		`INSERT INTO t(t) VALUES('flush')`,
		`INSERT INTO t VALUES('four','five')`,
		`INSERT INTO t(t) VALUES('FLUSH')`,
	},
		`SELECT k, quote(v) FROM t_config ORDER BY k`,
		`SELECT rowid, a, b FROM t ORDER BY rowid`,
		`SELECT rowid FROM t WHERE t MATCH 'one' ORDER BY rowid`,
	)
}

// r35eShadowDump is read by the CGO driver over BOTH engines' files, so a
// command that leaves the wrong bytes behind cannot hide behind this engine's
// own reader. %_data carries the structure record, whose leading 4 bytes are
// the configuration COOKIE -- the one thing 'rank' must bump and 'flush' must
// not.
var r35eShadowDump = []string{
	`SELECT id, quote(block) FROM t_data ORDER BY id`,
	`SELECT quote(segid), quote(term), quote(pgno) FROM t_idx ORDER BY segid, term`,
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
}

// TestFts5R35ECommandShadowBytes: build the same table with each engine and
// compare every shadow table BYTE FOR BYTE, reading both files with real
// SQLite. Each script ends in one statement per command, so a divergence names
// the command that caused it.
//
// The comparison is only meaningful while there is exactly one segment and
// nothing has been deleted (this engine re-encodes the whole index per write,
// C fts5 appends and merges on its own schedule -- see
// fts5_r34w_contentless_delete_test.go's own note), which is why every script
// here fills the table with a SINGLE insert statement.
func TestFts5R35ECommandShadowBytes(t *testing.T) {
	for _, sc := range []struct {
		name  string
		stmts []string
	}{
		{"baseline", nil},
		{"flush", []string{`INSERT INTO t(t) VALUES('flush')`}},
		{"rank", []string{`INSERT INTO t(t, rank) VALUES('rank', 'bm25(10.0,1.0)')`}},
		{"rank twice", []string{
			`INSERT INTO t(t, rank) VALUES('rank', 'bm25(10.0,1.0)')`,
			`INSERT INTO t(t, rank) VALUES('rank', 'highlight(0,''<'',''>'')')`,
		}},
		{"rank verbatim key", []string{`INSERT INTO t(t, rank) VALUES('RaNk', 'bm25()')`}},
		{"rank then flush", []string{
			`INSERT INTO t(t, rank) VALUES('rank', 'bm25(2.0)')`,
			`INSERT INTO t(t) VALUES('flush')`,
		}},
	} {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := append([]string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t VALUES('one two','three'),('one','one one one'),('two two','x y z')`,
			}, sc.stmts...)
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range r35eShadowDump {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("[%s] DIVERGES\n  sql: %s\n%s", sc.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
			// The rank a musql-written file stores must also be the rank real
			// SQLite reads back out of it.
			for _, q := range []string{
				`SELECT rowid, rank FROM t WHERE t MATCH 'one' ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'one' ORDER BY rank`,
			} {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if (goErr == nil) != (cgoErr == nil) {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goErr == nil && goOut != cgoOut {
					t.Errorf("[%s] DIVERGES\n  sql: %s\n%s", sc.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// tokendata=1 asks fts5 to remember, per query, which SOURCE token produced
// each indexed term. Nothing reachable from SQL reads that mapping (see
// vtab_fts5.go's apply()), so the option is accepted and inert -- which is a
// claim about BYTES, gated here by building the same table with each engine and
// comparing every shadow table with C SQLite as the reader.
func TestFts5R35ETokendata(t *testing.T) {
	for _, sc := range []struct {
		name, create string
	}{
		{"default tokenizer", `CREATE VIRTUAL TABLE t USING fts5(a, b, tokendata=1)`},
		{"tokendata=0", `CREATE VIRTUAL TABLE t USING fts5(a, b, tokendata=0)`},
		{"quoted value", `CREATE VIRTUAL TABLE t USING fts5(a, b, tokendata='1')`},
		{"abbreviated key", `CREATE VIRTUAL TABLE t USING fts5(a, b, tokend=1)`},
		{"repeated, last wins", `CREATE VIRTUAL TABLE t USING fts5(a, b, tokendata=0, tokendata=1)`},
		{"porter ascii", `CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize="porter ascii", tokendata=1)`},
		{"trigram", `CREATE VIRTUAL TABLE t USING fts5(a, b, tokenize=trigram, tokendata=1)`},
		{"unindexed column", `CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, tokendata=1)`},
		{"prefix index", `CREATE VIRTUAL TABLE t USING fts5(a, b, prefix='2 3', tokendata=1)`},
		{"no option (control)", `CREATE VIRTUAL TABLE t USING fts5(a, b)`},
	} {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := []string{
				sc.create,
				`INSERT INTO t VALUES('running dogs','the Quick brown fox'),('one','two three'),('','')`,
			}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			for _, q := range append(append([]string{}, r35eShadowDump...),
				`SELECT rowid, quote(a), quote(b) FROM t ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'dogs' ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'run*' ORDER BY rowid`,
				`SELECT rowid FROM t WHERE t MATCH 'one OR three' ORDER BY rowid DESC`,
				`SELECT rowid, bm25(t) FROM t WHERE t MATCH 'one OR three' ORDER BY rowid`,
				`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			) {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("[%s] DIVERGES\n  sql: %s\n%s", sc.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// Only "0" and "1" are tokendata values, matched literally -- '01' and '2' are
// "malformed tokendata=... directive" in C fts5 too.
func TestFts5R35ETokendataMalformed(t *testing.T) {
	flLockstep(t, "tokendata-malformed", []string{
		`CREATE VIRTUAL TABLE b1 USING fts5(x, tokendata=2)`,
		`CREATE VIRTUAL TABLE b2 USING fts5(x, tokendata=01)`,
		`CREATE VIRTUAL TABLE b3 USING fts5(x, tokendata=)`,
		`CREATE VIRTUAL TABLE b4 USING fts5(x, tokendata=yes)`,
		`CREATE VIRTUAL TABLE g1 USING fts5(x, tokendata=1)`,
	},
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
	)
}

// contentless_unindexed=1 over a table that HAS an UNINDEXED column is fts5's
// third content mode, FTS5_CONTENT_UNINDEXED: the table is still contentless
// (indexed columns read back NULL, DELETE is refused without
// contentless_delete=1), but it grows a %_content holding ONLY the unindexed
// columns, each under its ORIGINAL c<i> name.
//
// r35eUnindexedSchemas varies what the last round's batteries held fixed: which
// POSITION the unindexed column is in (a table whose stored column is not the
// first one is the shape a positional %_content read gets wrong), how many
// there are, whether every column is unindexed, and whether contentless_delete
// is on too.
var r35eUnindexedSchemas = []struct {
	name, create string
	nCol         int
}{
	{"unindexed last", `CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, content='', contentless_unindexed=1)`, 2},
	{"unindexed first", `CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b, content='', contentless_unindexed=1)`, 2},
	{"unindexed middle", `CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, c, content='', contentless_unindexed=1)`, 3},
	{"two unindexed", `CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b, c UNINDEXED, d, content='', contentless_unindexed=1)`, 4},
	{"all unindexed", `CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b UNINDEXED, content='', contentless_unindexed=1)`, 2},
	{"with contentless_delete", `CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b, c UNINDEXED, d, content='', contentless_delete=1, contentless_unindexed=1)`, 4},
	{"option order reversed", `CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, contentless_unindexed=1, content='')`, 2},
	{"no unindexed column (control)", `CREATE VIRTUAL TABLE t USING fts5(a, b, content='', contentless_unindexed=1)`, 2},
}

// r35eUnindexedFill is one INSERT of three rows for an nCol-column table. Row 3
// is all empty strings: it has no postings at all, so it is only in the index's
// row set because %_docsize records it, and it is the row a reconstruction that
// forgot the stored columns still gets wrong.
func r35eUnindexedFill(nCol int) string {
	rows := [][]string{
		{"'alpha shared'", "'stored one'", "'beta'", "'stored two'"},
		{"'gamma shared'", "'stored three'", "'delta'", "'stored four'"},
		{"''", "''", "''", "''"},
	}
	var b strings.Builder
	b.WriteString("INSERT INTO t VALUES")
	for i, r := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(" + strings.Join(r[:nCol], ",") + ")")
	}
	return b.String()
}

// r35eUnindexedProbes are read back over the SAME file by both engines.
func r35eUnindexedProbes(nCol int) []string {
	q := []string{
		`SELECT count(*) FROM t`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t ORDER BY rowid)`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'shared' ORDER BY rowid)`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'stored' ORDER BY rowid)`,
		`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
	}
	for _, c := range []string{"a", "b", "c", "d"}[:nCol] {
		q = append(q,
			fmt.Sprintf(`SELECT ifnull(group_concat(quote(%s)),'') FROM (SELECT %s FROM t ORDER BY rowid)`, c, c),
			fmt.Sprintf(`SELECT count(*) FROM t WHERE %s IS NULL`, c),
		)
	}
	return q
}

// TestFts5R35EContentlessUnindexedBytes: the same table built by each engine
// must leave byte-identical shadow tables, read by C SQLite over both files.
func TestFts5R35EContentlessUnindexedBytes(t *testing.T) {
	for _, sc := range r35eUnindexedSchemas {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := map[string]string{
				"sqlite":  filepath.Join(dir, "musql.db"),
				"sqlite3": filepath.Join(dir, "cgo.db"),
			}
			stmts := []string{sc.create, r35eUnindexedFill(sc.nCol)}
			for _, drv := range []string{"sqlite", "sqlite3"} {
				if err := fts5Exec(t, drv, dsn[drv], stmts); err != nil {
					t.Fatalf("%s: %v", drv, err)
				}
			}
			dump := []string{
				`SELECT id, quote(block) FROM t_data ORDER BY id`,
				`SELECT quote(segid), quote(term), quote(pgno) FROM t_idx ORDER BY segid, term`,
				`SELECT k, quote(v) FROM t_config ORDER BY k`,
				`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
			}
			for _, q := range dump {
				goOut, goErr := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), q)
				cgoOut, cgoErr := fts5Query(t, "sqlite3", dsn["sqlite3"], q)
				if goErr != nil || cgoErr != nil {
					t.Fatalf("%s\n  over this engine's file: %v\n  over C SQLite's:     %v", q, goErr, cgoErr)
				}
				if goOut != cgoOut {
					t.Errorf("[%s] DIVERGES\n  sql: %s\n%s", sc.name, q, fts5DiffLines(goOut, cgoOut))
				}
			}
		})
	}
}

// TestFts5R35EContentlessUnindexedInterchange: a file either engine wrote must
// read back the same in the other -- which for this mode also exercises the
// REOPEN path, where the stored columns have to be overlaid onto rows the index
// reconstruction produced as all-NULL.
func TestFts5R35EContentlessUnindexedInterchange(t *testing.T) {
	for _, sc := range r35eUnindexedSchemas {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			for _, writer := range []string{"sqlite", "sqlite3"} {
				dsn := filepath.Join(t.TempDir(), "fts5.db")
				if err := fts5Exec(t, writer, dsn, []string{sc.create, r35eUnindexedFill(sc.nCol)}); err != nil {
					t.Fatalf("[%s] writes: %v", writer, err)
				}
				for _, q := range r35eUnindexedProbes(sc.nCol) {
					goOut, goErr := fts5Query(t, "sqlite", musqlPathFor(t, writer, dsn), q)
					cgoOut, cgoErr := fts5Query(t, "sqlite3", oraclePathFor(t, writer, dsn), q)
					if goErr != nil || cgoErr != nil {
						t.Fatalf("[%s] %s\n  this engine: %v\n  cgo:         %v", writer, q, goErr, cgoErr)
					}
					if goOut != cgoOut {
						t.Errorf("[%s/written by %s] DIVERGES\n  sql: %s\n%s", sc.name, writer, q, fts5DiffLines(goOut, cgoOut))
					}
				}
			}
		})
	}
}

// A contentless table's UPDATE rule is decided by WHICH columns the SET list
// names, not by what they now hold: no indexed column and no rowid move is a
// %_content-only write, every indexed column plus contentless_delete=1 is a
// delete-and-reindex, and anything between is one of fts5's two errors.
//
// The readouts vary deliberately: the rows, %_content, %_docsize (whose ORIGIN
// column is where a delete-and-reindex shows up even when the rowid does not
// move), and MATCH -- a SET that reindexed when it should not have, or did not
// when it should have, moves exactly one of them.
func TestFts5R35EContentlessUpdate(t *testing.T) {
	flLockstep(t, "unindexed-content-only", []string{
		`CREATE VIRTUAL TABLE ft1 USING fts5(a, b UNINDEXED, c UNINDEXED, d, contentless_unindexed=1, content='')`,
		`INSERT INTO ft1(rowid,a,b,c,d) VALUES(100,'a1','b1','c1','d1'),(200,'a2','b2','c2','d2')`,
		`UPDATE ft1 SET b='b1.1', c='c1.1' WHERE rowid=100`,
		`UPDATE ft1 SET b='b2.1' WHERE rowid=200`,
		// A SET whose value does not change anything is still a SET.
		`UPDATE ft1 SET b=b WHERE rowid=200`,
		// ...and each of these names an indexed column, or moves the rowid.
		`UPDATE ft1 SET a='zzz' WHERE rowid=200`,
		`UPDATE ft1 SET rowid=201 WHERE rowid=200`,
		`UPDATE ft1 SET b='x', a='y' WHERE rowid=200`,
	},
		`SELECT rowid, quote(a), quote(b), quote(c), quote(d) FROM ft1 ORDER BY rowid`,
		`SELECT id, quote(c1), quote(c2) FROM ft1_content ORDER BY id`,
		`SELECT id, quote(sz) FROM ft1_docsize ORDER BY id`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM ft1 WHERE ft1 MATCH 'a1' ORDER BY rowid)`,
	)
	flLockstep(t, "contentless-delete-full-update", []string{
		`CREATE VIRTUAL TABLE x1 USING fts5(a UNINDEXED, b, c UNINDEXED, d, content=, contentless_delete=1, contentless_unindexed=1)`,
		`INSERT INTO x1(rowid,a,b,c,d) VALUES(112,'A','bee','C','dee'),(113,'A3','bee3','C3','dee3')`,
		// Every indexed column named, and the rowid moved: a delete and a
		// re-insert, which takes a FRESH origin.
		`UPDATE x1 SET b='hello', d='world', rowid=1120 WHERE rowid=112`,
		// A SUBSET of the indexed columns is fts5's other error...
		`UPDATE x1 SET b='only' WHERE rowid=113`,
		// ...while naming only an UNINDEXED one is still content-only.
		`UPDATE x1 SET a='onlyu' WHERE rowid=113`,
	},
		`SELECT rowid, quote(a), quote(b), quote(c), quote(d) FROM x1 ORDER BY rowid`,
		`SELECT id, quote(c0), quote(c2) FROM x1_content ORDER BY id`,
		`SELECT id, quote(sz), quote(origin) FROM x1_docsize ORDER BY id`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM x1 WHERE x1 MATCH 'hello' ORDER BY rowid)`,
	)
	// An IN-PLACE full update (no rowid move) still takes a fresh origin.
	flLockstep(t, "contentless-delete-inplace", []string{
		`CREATE VIRTUAL TABLE x1 USING fts5(a UNINDEXED, b, c UNINDEXED, d, content=, contentless_delete=1, contentless_unindexed=1)`,
		`INSERT INTO x1(rowid,a,b,c,d) VALUES(1,'A','bee','C','dee'),(2,'A3','bee3','C3','dee3')`,
		`UPDATE x1 SET b='inplace', d='dd' WHERE rowid=1`,
		// A rowid COLLISION is "constraint failed", and leaves the table alone.
		`UPDATE x1 SET b='coll', d='coll', rowid=2 WHERE rowid=1`,
	},
		`SELECT rowid, quote(a), quote(b), quote(c), quote(d) FROM x1 ORDER BY rowid`,
		`SELECT id, quote(sz), quote(origin) FROM x1_docsize ORDER BY id`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM x1 WHERE x1 MATCH 'inplace' ORDER BY rowid)`,
	)
	// A contentless table with NO unindexed column has no successful UPDATE at
	// all without contentless_delete=1: every SET list either names an indexed
	// column (bSeenIndex) or leaves one unnamed (bSeenIndexNC).
	flLockstep(t, "plain-contentless-update", []string{
		`CREATE VIRTUAL TABLE p USING fts5(a, b, content='')`,
		`INSERT INTO p VALUES('one','two')`,
		`UPDATE p SET a='x' WHERE rowid=1`,
		`UPDATE p SET a='x', b='y' WHERE rowid=1`,
		`UPDATE p SET rowid=9 WHERE rowid=1`,
	},
		`SELECT rowid, quote(a), quote(b) FROM p ORDER BY rowid`,
	)
	// 'secure-delete' counts REMOVALS: a %_content-only UPDATE removes nothing,
	// so it must NOT move %_config's version row.
	flLockstep(t, "secure-delete-content-only", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, content='', contentless_unindexed=1)`,
		`INSERT INTO t VALUES('one two','stored')`,
		`INSERT INTO t(t, rank) VALUES('secure-delete', 1)`,
		`UPDATE t SET b='changed' WHERE rowid=1`,
	},
		`SELECT k, quote(v) FROM t_config ORDER BY k`,
		`SELECT rowid, quote(a), quote(b) FROM t ORDER BY rowid`,
	)
}

// The mode changes NOTHING about which statements a contentless table accepts:
// DELETE is still refused without contentless_delete=1, 'rebuild' is still
// refused, and 'delete-all' still empties the table -- %_content included.
func TestFts5R35EContentlessUnindexedStatements(t *testing.T) {
	flLockstep(t, "unindexed-refusals", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, content='', contentless_unindexed=1)`,
		`INSERT INTO t VALUES('one two','stored one'),('three','stored two')`,
		`DELETE FROM t WHERE rowid=1`,
		`INSERT INTO t(t) VALUES('rebuild')`,
		`INSERT INTO t(t) VALUES('optimize')`,
		`INSERT INTO t(t) VALUES('integrity-check')`,
	},
		`SELECT rowid, quote(a), quote(b) FROM t ORDER BY rowid`,
		`SELECT id, quote(c1) FROM t_content ORDER BY id`,
	)
	flLockstep(t, "unindexed-delete-all", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a UNINDEXED, b, content='', contentless_unindexed=1)`,
		`INSERT INTO t VALUES('stored one','one two'),('stored two','three')`,
		`INSERT INTO t(t) VALUES('delete-all')`,
		`INSERT INTO t VALUES('stored three','four')`,
	},
		`SELECT rowid, quote(a), quote(b) FROM t ORDER BY rowid`,
		`SELECT id, quote(c0) FROM t_content ORDER BY id`,
		`SELECT count(*) FROM t WHERE t MATCH 'one'`,
	)
	// ALTER TABLE ... RENAME renames the shadow tables sqlite3Fts5StorageRename
	// renames, which is %_content only for FTS5_CONTENT_NORMAL. A CONTENTLESS
	// table (with or without contentless_unindexed=1) therefore leaves its
	// %_content -- if it has one at all -- behind under the OLD name, and every
	// read of the renamed table is then "no such table: <new>_content".
	//
	// This is the wrong answer contentless_unindexed=1 exposed: renaming the
	// %_content too made the reads SUCCEED. It also un-declines a plain
	// contentless table's rename, which used to fail on a %_content that never
	// existed (fts5alter.test).
	flLockstep(t, "unindexed-rename", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, content='', contentless_unindexed=1)`,
		`INSERT INTO t VALUES('one two','stored one')`,
		`ALTER TABLE t RENAME TO t2`,
		`SELECT rowid, quote(a), quote(b) FROM t2`,
	},
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		`SELECT rowid, quote(a), quote(b) FROM t2`,
		`SELECT id, quote(c1) FROM t_content ORDER BY id`,
	)
	flLockstep(t, "plain-contentless-rename", []string{
		`CREATE VIRTUAL TABLE p USING fts5(a, content='')`,
		`INSERT INTO p VALUES('one two')`,
		`ALTER TABLE p RENAME TO p2`,
	},
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		`SELECT rowid, quote(a) FROM p2`,
		`SELECT rowid FROM p2 WHERE p2 MATCH 'one'`,
	)
	// DROP TABLE drops what sqlite3Fts5DropAll drops, which is %_content only
	// for FTS5_CONTENT_NORMAL -- so a contentless_unindexed table's %_content
	// SURVIVES the DROP.
	flLockstep(t, "unindexed-drop", []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b UNINDEXED, content='', contentless_unindexed=1)`,
		`INSERT INTO t VALUES('one two','stored one')`,
		`DROP TABLE t`,
	},
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
		`SELECT id, quote(c1) FROM t_content ORDER BY id`,
	)
	// contentless_unindexed=1 on a table that is NOT contentless is still
	// "contentless_unindexed=1 requires a contentless table".
	flLockstep(t, "unindexed-requires-contentless", []string{
		`CREATE VIRTUAL TABLE b1 USING fts5(a, b UNINDEXED, contentless_unindexed=1)`,
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a)`,
		`CREATE VIRTUAL TABLE b2 USING fts5(a, content=src, contentless_unindexed=1)`,
		`CREATE VIRTUAL TABLE g1 USING fts5(a, b UNINDEXED, content='', contentless_unindexed=0)`,
	},
		`SELECT type, name, sql FROM sqlite_master ORDER BY name`,
	)
}
