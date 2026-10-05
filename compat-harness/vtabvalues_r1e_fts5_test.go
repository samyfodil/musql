//go:build sqlite_fts5

// Gates fts5's virtual table command channel on values. The batch refactored
// how VALUES rows are walked: previous code walked each row up to three times
// for different parts of the command. Now it evaluates once and hands the
// values to the channel, matching C fts5.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// vtabValuesFts5Run replays statements against both engines, comparing
// outcomes and probing after each statement so 'rebuild' commands don't hide
// earlier mutations. It ends with C SQLite checking the file.
func vtabValuesFts5Run(t *testing.T, stmts, probes []string) {
	t.Helper()
	dir := t.TempDir()
	dsn := map[string]string{
		"sqlite":  filepath.Join(dir, "musql.db"),
		"sqlite3": filepath.Join(dir, "cgo.db"),
	}
	conn := map[string]*sql.DB{}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		db, err := sql.Open(drv, dsn[drv])
		if err != nil {
			t.Fatalf("sql.Open(%s): %v", drv, err)
		}
		db.SetMaxOpenConns(1)
		conn[drv] = db
	}
	compare := func(after string) {
		for _, q := range probes {
			goOut, goErr := fts5QuerySameConn(t, conn["sqlite"], q)
			cgoOut, cgoErr := fts5QuerySameConn(t, conn["sqlite3"], q)
			if (goErr == nil) != (cgoErr == nil) {
				t.Errorf("after %s\n  %s: error divergence\n  go:  %v\n  cgo: %v", after, q, goErr, cgoErr)
				continue
			}
			if goErr != nil {
				continue // both refuse this probe in this state (e.g. before CREATE)
			}
			if goOut != cgoOut {
				t.Errorf("after %s\n  %s DIVERGES\n%s", after, q, fts5DiffLines(goOut, cgoOut))
			}
		}
	}
	for _, s := range stmts {
		goErr := fts5ExecSameConn(t, conn["sqlite"], []string{s})
		cgoErr := fts5ExecSameConn(t, conn["sqlite3"], []string{s})
		switch {
		case goErr != nil && cgoErr != nil:
			t.Logf("BOTH REJECT %s\n  go:  %v\n  cgo: %v", s, goErr, cgoErr)
		case goErr != nil:
			t.Fatalf("this engine REJECTS a statement C fts5 accepts\n  sql: %s\n  err: %v", s, goErr)
		case cgoErr != nil:
			t.Fatalf("this engine ACCEPTS a statement C fts5 rejects (a wrong answer)\n  sql: %s\n  cgo err: %v", s, cgoErr)
		}
		compare(s)
	}
	for _, drv := range []string{"sqlite", "sqlite3"} {
		if err := conn[drv].Close(); err != nil {
			t.Fatalf("close %s: %v", drv, err)
		}
	}
	// The bytes, judged by the engine that did not write them.
	if ic, err := fts5Query(t, "sqlite3", exportedForOracle(t, dsn["sqlite"]), `PRAGMA integrity_check`); err != nil || ic != "integrity_check\nT:ok" {
		t.Errorf("C SQLite reports integrity_check = %q over this engine's file (%v)", ic, err)
	}
}

// vtabValuesFts5Probes queries every comparable aspect of a two-column fts5
// table: rows, MATCH queries, and shadow tables.
var vtabValuesFts5Probes = []string{
	`SELECT rowid, a, b FROM t ORDER BY rowid`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'beta' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'gamma' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'delta' ORDER BY rowid)`,
	`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'epsilon' ORDER BY rowid)`,
	`SELECT count(*) FROM t`,
	`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
	`SELECT id, quote(c0), quote(c1) FROM t_content ORDER BY id`,
	`SELECT k, quote(v) FROM t_config ORDER BY k`,
	`PRAGMA integrity_check`,
}

// TestVtabValuesR1EFts5Cycle runs the full mutation cycle with a MATCH read
// after each operation.
func TestVtabValuesR1EFts5Cycle(t *testing.T) {
	vtabValuesFts5Run(t, []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'alpha one','x y')`,
		`INSERT INTO t(rowid,a,b) VALUES(2,'beta two','y z')`,
		`INSERT INTO t(a,b) VALUES('gamma three','z w')`, // auto-assigned rowid
		`SELECT rowid FROM t WHERE t MATCH 'beta'`,

		// UPDATE: WHERE through writeRowSelected, SET through writeApplySetList.
		`UPDATE t SET a='delta four' WHERE rowid=2`,
		`SELECT rowid FROM t WHERE t MATCH 'delta'`,
		// A SET RHS reading the OLD row, and a multi-row WHERE.
		`UPDATE t SET b = b || '!' WHERE rowid > 1`,
		`SELECT rowid FROM t WHERE t MATCH 'alpha'`,
		// A WHERE that is NULL, not false.
		`UPDATE t SET a='never' WHERE NULL`,
		`DELETE FROM t WHERE NULL`,

		// A ROWID-MOVING update onto a free rowid.
		`UPDATE t SET rowid=10 WHERE rowid=1`,
		`SELECT rowid FROM t WHERE t MATCH 'alpha'`,
		// ... and onto an OCCUPIED one, under OR REPLACE (fts5's own
		// per-module forgiveness, fts5_main.c's fts5UpdateMethod).
		`UPDATE OR REPLACE t SET rowid=2 WHERE rowid=10`,
		`SELECT rowid FROM t WHERE t MATCH 'alpha'`,

		// INSERT and INSERT OR REPLACE onto an occupied rowid.
		`INSERT INTO t(rowid,a,b) VALUES(2,'clash','clash')`,
		// UPSERT, which insert.c:1291-1294 rejects for EVERY virtual table --
		// so both engines must, and the state after must be untouched.
		`INSERT INTO t(rowid,a,b) VALUES(2,'clash','clash') ON CONFLICT DO NOTHING`,
		`INSERT OR REPLACE INTO t(rowid,a,b) VALUES(2,'epsilon five','q')`,
		`SELECT rowid FROM t WHERE t MATCH 'epsilon'`,

		`DELETE FROM t WHERE rowid=3`,
		`SELECT rowid FROM t WHERE t MATCH 'gamma'`,

		// The COMMAND CHANNEL -- the primitives this batch moved onto values.
		`INSERT INTO t(t) VALUES('optimize')`,
		`SELECT rowid FROM t WHERE t MATCH 'epsilon'`,
		`INSERT INTO t(t) VALUES('rebuild')`,
		`SELECT rowid FROM t WHERE t MATCH 'epsilon'`,
		`INSERT INTO t(t) VALUES('integrity-check')`,
		`INSERT INTO t(t, rank) VALUES('pgsz', 64)`,
		`INSERT INTO t(t, rank) VALUES('secure-delete', 1)`,
		`DELETE FROM t WHERE rowid=2`,
		`SELECT rowid FROM t WHERE t MATCH 'epsilon'`,
		`INSERT INTO t(t) VALUES('optimize')`,
	}, vtabValuesFts5Probes)
}

// TestVtabValuesR1EFts5CommandArgumentIsAValue verifies that the command's
// argument is the evaluated value, not re-read from the expression.
func TestVtabValuesR1EFts5CommandArgumentIsAValue(t *testing.T) {
	for _, arg := range []string{
		`32+32`, `abs(-64)`, `'6'||'4'`, `CAST('64' AS INTEGER)`,
		`(SELECT 64)`, `coalesce(NULL, 64)`, `64.0`, `x'3634'`,
	} {
		t.Run(strings.NewReplacer("'", "", "(", "", ")", "", " ", "_", "*", "x").Replace(arg), func(t *testing.T) {
			vtabValuesFts5Run(t, []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t(rowid,a,b) VALUES(1,'alpha one','x y')`,
				`INSERT INTO t(rowid,a,b) VALUES(2,'beta two','y z')`,
				fmt.Sprintf(`INSERT INTO t(t, rank) VALUES('pgsz', %s)`, arg),
				`INSERT INTO t(rowid,a,b) VALUES(3,'gamma three','z w')`,
				`INSERT INTO t(t) VALUES('optimize')`,
			}, vtabValuesFts5Probes)
		})
	}
}

// TestVtabValuesR1EFts5CommandRowIsEvaluatedWhole verifies that all named
// columns are evaluated before OP_VUpdate calls the module, even if the
// command doesn't read them. This is the one place the refactor fixes a bug.
func TestVtabValuesR1EFts5CommandRowIsEvaluatedWhole(t *testing.T) {
	for _, c := range []struct{ name, stmt string }{
		{"optimize", `INSERT INTO t(t, rank) VALUES('optimize', abs(-9223372036854775807-1))`},
		{"rebuild", `INSERT INTO t(t, rank) VALUES('rebuild', abs(-9223372036854775807-1))`},
		{"integrity-check", `INSERT INTO t(t, rank) VALUES('integrity-check', abs(-9223372036854775807-1))`},
		// And the command NAME's own cell, which every command does read.
		{"name cell", `INSERT INTO t(t) VALUES(char(abs(-9223372036854775807-1)))`},
	} {
		t.Run(c.name, func(t *testing.T) {
			vtabValuesFts5Run(t, []string{
				`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
				`INSERT INTO t(rowid,a,b) VALUES(1,'alpha one','x y')`,
				`INSERT INTO t(rowid,a,b) VALUES(2,'beta two','y z')`,
				c.stmt,
				`INSERT INTO t(rowid,a,b) VALUES(3,'gamma three','z w')`,
			}, vtabValuesFts5Probes)
		})
	}
}

// TestVtabValuesR1EFts5CommandRowidMustBeInt verifies that OP_MustBeInt is
// coded before OP_VUpdate, so non-integer rowids fail before fts5 is called.
func TestVtabValuesR1EFts5CommandRowidMustBeInt(t *testing.T) {
	for _, c := range []struct{ name, rowid string }{
		{"text", `'x'`},
		{"real with fraction", `5.5`},
		{"blob", `x'0102'`},
		// The ACCEPTED spelling, which must still delete.
		{"integer", `1`},
		// An EXPRESSION that evaluates to one, which is the half this batch
		// moved: the value the row path produced is the value the primitive
		// reads.
		{"integer expression", `4-3`},
	} {
		t.Run(c.name, func(t *testing.T) {
			vtabValuesFts5Run(t, []string{
				`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
				`INSERT INTO src VALUES(1,'alpha one','x y'),(2,'beta two','y z')`,
				`CREATE VIRTUAL TABLE t USING fts5(a, b, content='src')`,
				`INSERT INTO t(rowid,a,b) SELECT id,a,b FROM src`,
				fmt.Sprintf(`INSERT INTO t(t, rowid, a, b) VALUES('delete', %s, 'alpha one', 'x y')`, c.rowid),
			}, []string{
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha' ORDER BY rowid)`,
				`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'beta' ORDER BY rowid)`,
				`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
				`SELECT k, quote(v) FROM t_config ORDER BY k`,
				`PRAGMA integrity_check`,
			})
		})
	}
}

// TestVtabValuesR1EFts5ExternalContent verifies the 'delete' command on
// external-content tables, with a MATCH after each mutation to catch regressions.
func TestVtabValuesR1EFts5ExternalContent(t *testing.T) {
	vtabValuesFts5Run(t, []string{
		`CREATE TABLE src(id INTEGER PRIMARY KEY, a, b)`,
		`INSERT INTO src VALUES(1,'alpha one','x y'),(2,'beta two','y z'),(3,'gamma three','z w')`,
		`CREATE VIRTUAL TABLE t USING fts5(a, b, content='src')`,
		`INSERT INTO t(rowid,a,b) SELECT id,a,b FROM src`,
		`SELECT rowid FROM t WHERE t MATCH 'beta'`,
		// The 'delete' command: rowid in one cell, the document in the rest.
		`INSERT INTO t(t, rowid, a, b) VALUES('delete', 2, 'beta two', 'y z')`,
		`SELECT rowid FROM t WHERE t MATCH 'beta'`,
		// (A 'delete' naming only SOME of the columns is a documented decline
		// here: C fts5 subtracts the postings of the values it is given,
		// which would leave the index holding the difference, while this
		// engine re-encodes the whole index from the documents -- see
		// engine/fts5_extcontent.go. It cannot go through this runner, which
		// fails on any one-sided reject.)
		//
		// An expression payload, to pin that the values reaching the primitive
		// are the ones the row path evaluated.
		`INSERT INTO t(t, rowid, a, b) VALUES('delete', 1/1, 'alpha'||' '||'one', 'x'||' '||'y')`,
		`SELECT rowid FROM t WHERE t MATCH 'alpha'`,
		// 'rebuild' puts the index back from the content table.
		`INSERT INTO t(t) VALUES('rebuild')`,
		`SELECT rowid FROM t WHERE t MATCH 'alpha'`,
		`INSERT INTO t(t) VALUES('optimize')`,
		`INSERT INTO t(t) VALUES('integrity-check')`,
		`INSERT INTO t(t) VALUES('delete-all')`,
		`SELECT rowid FROM t WHERE t MATCH 'alpha'`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}, []string{
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'alpha' ORDER BY rowid)`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'beta' ORDER BY rowid)`,
		`SELECT ifnull(group_concat(rowid),'') FROM (SELECT rowid FROM t WHERE t MATCH 'gamma' ORDER BY rowid)`,
		`SELECT rowid, a, b FROM t ORDER BY rowid`,
		`SELECT id, quote(sz) FROM t_docsize ORDER BY id`,
		`SELECT k, quote(v) FROM t_config ORDER BY k`,
		`SELECT id, a, b FROM src ORDER BY id`,
		`PRAGMA integrity_check`,
	})
}

// TestVtabValuesR1EFts5Aux verifies that aux functions (like bm25, highlight,
// snippet) receive evaluated values, not expressions that might be re-evaluated.
func TestVtabValuesR1EFts5Aux(t *testing.T) {
	vtabValuesFts5Run(t, []string{
		`CREATE VIRTUAL TABLE t USING fts5(a, b)`,
		`INSERT INTO t(rowid,a,b) VALUES(1,'alpha one two','x y')`,
		`INSERT INTO t(rowid,a,b) VALUES(2,'beta two three','y z alpha')`,
		`INSERT INTO t(rowid,a,b) VALUES(3,'gamma three four','z w')`,
		`UPDATE t SET b = b || ' alpha' WHERE rowid=3`,
		`DELETE FROM t WHERE rowid=1`,
		`INSERT INTO t(t) VALUES('rebuild')`,
	}, []string{
		`SELECT rowid, round(bm25(t), 6) FROM t WHERE t MATCH 'alpha' ORDER BY rowid`,
		`SELECT rowid, round(bm25(t, 1.0+9.0, 1.0), 6) FROM t WHERE t MATCH 'alpha' ORDER BY rowid`,
		`SELECT rowid, round(bm25(t, abs(-10.0), 0.5*2), 6) FROM t WHERE t MATCH 'alpha' ORDER BY rowid`,
		`SELECT rowid, highlight(t, 0, '['||'', ']') FROM t WHERE t MATCH 'three' ORDER BY rowid`,
		`SELECT rowid, highlight(t, 1-1, '<b>', '</'||'b>') FROM t WHERE t MATCH 'alpha' ORDER BY rowid`,
		`SELECT rowid, snippet(t, 0, '[', ']', '...', 2+2) FROM t WHERE t MATCH 'three' ORDER BY rowid`,
		`SELECT rowid, snippet(t, -1, '[', ']', ltrim(' ...'), 5) FROM t WHERE t MATCH 'alpha' ORDER BY rowid`,
		`SELECT rowid, round(rank, 6) FROM t WHERE t MATCH 'alpha' ORDER BY rowid`,
		`SELECT rowid, a, b FROM t ORDER BY rowid`,
		`PRAGMA integrity_check`,
	})
}
