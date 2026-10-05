// Tests for the schema-time expression seam: CHECK constraints, generated
// columns, partial index WHERE clauses, and expression index keys are all
// compiled once to bytecode and run per row. TestSelfRowExprSeamIsCompiled
// verifies compilation; the remaining tests verify they run correctly in
// paths with no enclosing program (ALTER TABLE ADD COLUMN, foreign key
// cascades, index materialization, UNIQUE validation).
package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

func seamDB(t *testing.T, stmts ...string) (*Session, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seam.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	return db, path
}

func seamExec(t *testing.T, db *Session, sqlText string) {
	t.Helper()
	if err := db.Exec(sqlText); err != nil {
		t.Fatalf("Exec(%s): %v", sqlText, err)
	}
}

func seamExecErr(t *testing.T, db *Session, sqlText, want string) {
	t.Helper()
	err := db.Exec(sqlText)
	if err == nil {
		t.Fatalf("Exec(%s): expected an error containing %q, got success", sqlText, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Exec(%s): error %q does not contain %q", sqlText, err, want)
	}
}

// seamRows closes db and reads tableName back through the ordinary read path,
// so what is asserted is what was actually MATERIALIZED to the file.
func seamRows(t *testing.T, db *Session, path, sqlText string) [][]Value {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	_, rows, err := p.Query(sqlText)
	if err != nil {
		t.Fatalf("Query(%s): %v", sqlText, err)
	}
	return rows
}

func seamInts(rows [][]Value) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 && r[0].Typ == Int {
			out = append(out, r[0].I)
		}
	}
	return out
}

// TestSelfRowExprSeamIsCompiled verifies that CHECK constraints, generated
// columns, partial indexes, and expression indexes all compile to bytecode at
// schema time rather than being interpreted per row.
func TestSelfRowExprSeamIsCompiled(t *testing.T) {
	db, _ := seamDB(t,
		`CREATE TABLE t(a INT, b INT, `+
			`v INT AS (a+b) VIRTUAL, s TEXT AS (a || '-' || b) STORED, `+
			`ts TEXT DEFAULT CURRENT_TIMESTAMP, ep INT DEFAULT (unixepoch()), `+
			`CHECK(a > 0), CONSTRAINT bsmall CHECK(b < 100))`,
		`CREATE INDEX ix ON t(a*3, b) WHERE b > 1`,
		`CREATE UNIQUE INDEX uix ON t(a+b) WHERE a > 0`,
	)
	defer db.Close()

	tbl := db.findTableMeta("t")
	if tbl == nil {
		t.Fatal("no table t")
	}
	if len(tbl.checks) != 2 {
		t.Fatalf("got %d CHECK constraints, want 2", len(tbl.checks))
	}
	for _, cc := range tbl.checks {
		if cc.prog == nil || cc.prog.prog == nil {
			t.Errorf("CHECK(%s) was NOT compiled at schema time -- eval would re-compile it per row", cc.exprText)
		}
	}
	gen := 0
	for _, c := range tbl.cols {
		if !c.IsGenerated() {
			continue
		}
		gen++
		if c.genProg == nil || c.genProg.prog == nil {
			t.Errorf("generated column %s was NOT compiled", c.Name)
		}
	}
	if gen != 2 {
		t.Errorf("got %d generated columns, want 2", gen)
	}
	idxSeen := 0
	for _, idx := range db.indexes {
		if !idx.exprOrPartial {
			continue
		}
		idxSeen++
		whereProg, keyProgs := compileIndexExprs(tbl, idx)
		if idx.where != nil && (whereProg == nil || whereProg.prog == nil) {
			t.Errorf("index %s: partial WHERE was NOT compiled", idx.name)
		}
		for i, k := range idx.keys {
			if k.colIdx >= 0 {
				continue // a plain column key needs no expression
			}
			if keyProgs[i] == nil || keyProgs[i].prog == nil {
				t.Errorf("index %s: key %d was NOT compiled", idx.name, i)
			}
		}
	}
	if idxSeen != 2 {
		t.Errorf("got %d expression/partial indexes, want 2", idxSeen)
	}
	def := 0
	for _, c := range tbl.cols {
		if c.DefaultDeferred == nil {
			continue
		}
		def++
		if c.defaultProg == nil || c.defaultProg.prog == nil {
			t.Errorf("deferred DEFAULT on column %s was NOT compiled at schema time -- eval would re-compile it per row", c.Name)
		}
	}
	if def != 2 {
		t.Errorf("got %d deferred DEFAULT columns, want 2", def)
	}
}

// TestSelfRowExprDeferredDefaultCompiles verifies that deferred DEFAULT
// clauses (CURRENT_TIMESTAMP, date(), etc.) are compiled to bytecode and
// produce well-formed clock values of the correct type.
func TestSelfRowExprDeferredDefaultCompiles(t *testing.T) {
	const create = `CREATE TABLE t(a INTEGER PRIMARY KEY, b, ` +
		`ts DEFAULT CURRENT_TIMESTAMP, d DEFAULT CURRENT_DATE, m DEFAULT CURRENT_TIME, ` +
		`y DEFAULT (strftime('%Y','now')), dt DEFAULT (datetime('now')), ` +
		`jd DEFAULT (julianday('now')), ep DEFAULT (unixepoch()), ` +
		`cat DEFAULT (date('now') || 'x'))`
	db, path := seamDB(t, create)

	tbl := db.findTableMeta("t")
	if tbl == nil {
		t.Fatal("no table t")
	}
	deferred := 0
	for _, c := range tbl.cols {
		if c.DefaultDeferred == nil {
			continue
		}
		deferred++
		if c.defaultProg == nil || c.defaultProg.prog == nil {
			t.Errorf("deferred DEFAULT on column %s was NOT compiled", c.Name)
		}
	}
	if deferred != 8 {
		t.Fatalf("got %d deferred DEFAULT columns, want 8 -- the shapes this test covers changed", deferred)
	}

	const stmt = `INSERT INTO t(a,b) VALUES(2,20)`
	_, cerr := db.compileWrite(stmt)
	if cerr != nil {
		t.Fatalf("compile %q: %v", stmt, cerr)
	}
	seamExec(t, db, stmt)

	rows := seamRows(t, db, path, `SELECT ts,d,m,y,dt,jd,ep,cat FROM t ORDER BY a`)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	// Check types and lengths, not exact values (which vary with the clock).
	want := []struct {
		name string
		typ  ValueType
		n    int // expected TEXT length, 0 when not TEXT
	}{
		{"ts", Text, 19},  // YYYY-MM-DD HH:MM:SS
		{"d", Text, 10},   // YYYY-MM-DD
		{"m", Text, 8},    // HH:MM:SS
		{"y", Text, 4},    // YYYY
		{"dt", Text, 19},  // YYYY-MM-DD HH:MM:SS
		{"jd", Float, 0},  // a julian day number
		{"ep", Int, 0},    // seconds since the epoch
		{"cat", Text, 11}, // YYYY-MM-DD || 'x'
	}
	for i, w := range want {
		got := rows[0][i]
		if got.Typ != w.typ {
			t.Errorf("column %s has type %v, want %v", w.name, got.Typ, w.typ)
			continue
		}
		if w.typ == Text && len(got.S) != w.n {
			t.Errorf("column %s = %q (%d bytes), want %d", w.name, got.S, len(got.S), w.n)
		}
	}
}

// TestSelfRowExprDeferredDefaultAfterAddColumn verifies that ALTER TABLE
// ADD COLUMN carries the compiled DEFAULT program from the schema parse.
func TestSelfRowExprDeferredDefaultAfterAddColumn(t *testing.T) {
	// The table must be EMPTY at the ALTER: C SQLite refuses to add a
	// non-constant default to a table with rows ("Cannot add a column with
	// non-constant default"), and so does this engine.
	db, path := seamDB(t,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`ALTER TABLE t ADD COLUMN ts DEFAULT CURRENT_TIMESTAMP`,
		`INSERT INTO t(a,b) VALUES(1,10)`,
	)
	tbl := db.findTableMeta("t")
	if tbl == nil {
		t.Fatal("no table t")
	}
	last := tbl.cols[len(tbl.cols)-1]
	if last.Name != "ts" {
		t.Fatalf("last column is %s, want ts", last.Name)
	}
	if last.DefaultDeferred == nil {
		t.Fatal("added column ts has no deferred DEFAULT")
	}
	if last.defaultProg == nil || last.defaultProg.prog == nil {
		t.Error("added column ts lost its compiled DEFAULT -- alter_write.go must carry defaultProg across the splice")
	}
	// And it still means the right thing on the route that runs it.
	seamExec(t, db, `REPLACE INTO t(a,b) VALUES(2,20)`)
	rows := seamRows(t, db, path, `SELECT ts FROM t WHERE a = 2`)
	if len(rows) != 1 || rows[0][0].Typ != Text || len(rows[0][0].S) != 19 {
		t.Fatalf("added column's DEFAULT stored %+v, want a 19-char timestamp", rows)
	}
}

// TestSelfRowExprCheckOnUpdate verifies CHECK constraint semantics on
// UPDATE statements.
func TestSelfRowExprCheckOnUpdate(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT, b INT, CHECK(a > 0), CHECK(b < a * 10))`,
		`INSERT INTO t VALUES(5, 3)`,
		`INSERT INTO t VALUES(7, 9)`,
	)
	seamExec(t, db, `UPDATE t SET b = a - 1 WHERE a = 5`)
	seamExecErr(t, db, `UPDATE t SET a = -1 WHERE a = 7`, "CHECK constraint failed")
	seamExecErr(t, db, `UPDATE t SET b = 900 WHERE a = 7`, "CHECK constraint failed")
	// A CHECK that names no assigned column is not evaluated at all
	// (sqlite3ExprReferencesUpdatedColumn, insert.c:1718) -- assert the
	// surviving rows rather than the skip, which is checkChangeSet's own test.
	got := seamInts(seamRows(t, db, path, `SELECT b FROM t ORDER BY a`))
	if len(got) != 2 || got[0] != 4 || got[1] != 9 {
		t.Fatalf("got b = %v, want [4 9]", got)
	}
}

// TestSelfRowExprCheckOnAddColumnBackfill verifies CHECK validation during
// ALTER TABLE ADD COLUMN back-fill.
func TestSelfRowExprCheckOnAddColumnBackfill(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT)`,
		`INSERT INTO t VALUES(1)`,
		`INSERT INTO t VALUES(2)`,
	)
	// A CHECK every existing row satisfies is accepted...
	seamExec(t, db, `ALTER TABLE t ADD COLUMN c INT DEFAULT 5 CHECK(c IS NULL OR c > 0)`)
	// ...and one no existing row can satisfy is rejected, leaving the table as
	// it was. C SQLite rejects this at back-fill time too.
	seamExecErr(t, db, `ALTER TABLE t ADD COLUMN d INT DEFAULT 0 CHECK(d > 100)`, "CHECK constraint failed")
	seamExec(t, db, `INSERT INTO t(a) VALUES(3)`)
	got := seamInts(seamRows(t, db, path, `SELECT c FROM t ORDER BY a`))
	if len(got) != 3 {
		t.Fatalf("got %v, want three rows", got)
	}
	for _, v := range got {
		if v != 5 {
			t.Fatalf("got c = %v, want every row 5", got)
		}
	}
}

// TestSelfRowExprCheckOnForeignKeyCascade verifies CHECK validation during
// foreign key cascades.
func TestSelfRowExprCheckOnForeignKeyCascade(t *testing.T) {
	db, path := seamDB(t,
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(pid INT REFERENCES p(id) ON UPDATE CASCADE, k INT, CHECK(pid IS NULL OR pid > 0))`,
		`INSERT INTO p VALUES(1)`,
		`INSERT INTO c VALUES(1, 10)`,
	)
	seamExec(t, db, `UPDATE p SET id = 4 WHERE id = 1`)
	got := seamInts(seamRows(t, db, path, `SELECT pid FROM c`))
	if len(got) != 1 || got[0] != 4 {
		t.Fatalf("cascade left pid = %v, want [4]", got)
	}
}

// TestSelfRowExprCheckConflictClause verifies that CHECK constraints respect
// conflict clauses like INSERT OR IGNORE.
func TestSelfRowExprCheckConflictClause(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT, CHECK(a > 0))`,
		`INSERT OR IGNORE INTO t VALUES(1)`,
		`INSERT OR IGNORE INTO t VALUES(-1)`,
		`INSERT OR IGNORE INTO t VALUES(2)`,
	)
	got := seamInts(seamRows(t, db, path, `SELECT a FROM t ORDER BY a`))
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v, want [1 2]", got)
	}
}

// TestSelfRowExprGeneratedColumns verifies VIRTUAL and STORED generated
// columns, including CHECKs that reference them.
func TestSelfRowExprGeneratedColumns(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT, b INT, `+
			`v INT AS (a + b) VIRTUAL, `+
			`s INT AS (v * 10) STORED, `+
			`CHECK(v < 1000))`,
		`INSERT INTO t(a,b) VALUES(1,2)`,
		`INSERT INTO t(a,b) VALUES(30,4)`,
	)
	seamExecErr(t, db, `INSERT INTO t(a,b) VALUES(999,999)`, "CHECK constraint failed")
	rows := seamRows(t, db, path, `SELECT a, b, v, s FROM t ORDER BY a`)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	want := [][]int64{{1, 2, 3, 30}, {30, 4, 34, 340}}
	for i, r := range rows {
		for j := range want[i] {
			if r[j].Typ != Int || r[j].I != want[i][j] {
				t.Fatalf("row %d col %d = %+v, want %d", i, j, r[j], want[i][j])
			}
		}
	}
}

// TestSelfRowExprUniquePartialIndex verifies that partial UNIQUE indexes
// exclude rows where the WHERE clause is false.
func TestSelfRowExprUniquePartialIndex(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a) WHERE b > 0`,
		`INSERT INTO t VALUES(1, 1)`,
		`INSERT INTO t VALUES(1, -1)`,
		`INSERT INTO t VALUES(1, -2)`,
	)
	seamExecErr(t, db, `INSERT INTO t VALUES(1, 5)`, "UNIQUE constraint failed")
	got := seamInts(seamRows(t, db, path, `SELECT count(*) FROM t`))
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("got count %v, want [3]", got)
	}
}

// seamCloseErr asserts what db.Close reports.
func seamCloseErr(t *testing.T, db *Session, want string) {
	t.Helper()
	err := db.Close()
	switch {
	case want == "" && err != nil:
		t.Fatalf("Close: %v, want success", err)
	case want != "" && err == nil:
		t.Fatalf("Close: expected an error containing %q, got success", want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Fatalf("Close: error %q does not contain %q", err, want)
	}
}

// TestSelfRowExprUniquePartialIndexAdmitsOnly verifies the partial-UNIQUE
// rule during index creation: only rows matching the WHERE clause count
// toward uniqueness constraints.
func TestSelfRowExprUniquePartialIndexAdmitsOnly(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t(a INT, b INT)`,
		`INSERT INTO t VALUES(1, 1)`,  // admitted by "b > 0"
		`INSERT INTO t VALUES(1, -1)`, // excluded: duplicate a, but b <= 0
		`INSERT INTO t VALUES(2, 3)`,  // admitted
	}
	db, _ := seamDB(t, append(append([]string(nil), stmts...), `CREATE UNIQUE INDEX ok1 ON t(a) WHERE b > 0`)...)
	seamCloseErr(t, db, "")

	// A WHERE wide enough to admit BOTH a=1 rows collides, which the CREATE
	// reports as sqlite3RefillIndex's sorter compare does (build.c:3834-3836).
	db, _ = seamDB(t, stmts...)
	seamExecErr(t, db, `CREATE UNIQUE INDEX bad1 ON t(a) WHERE b > -5`, "UNIQUE constraint failed")
	seamCloseErr(t, db, "")
}

// TestSelfRowExprUniqueExpressionIndex verifies that expression-key indexes
// enforce uniqueness on the expression value, not the column.
func TestSelfRowExprUniqueExpressionIndex(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT, b INT)`,
		`CREATE UNIQUE INDEX ux ON t(a+b)`,
		`INSERT INTO t VALUES(1, 2)`,
	)
	seamExecErr(t, db, `INSERT INTO t VALUES(2, 1)`, "UNIQUE constraint failed")
	seamExec(t, db, `INSERT INTO t VALUES(4, 5)`)
	// NULL keys are all distinct from each other.
	seamExec(t, db, `INSERT INTO t VALUES(NULL, 1)`)
	seamExec(t, db, `INSERT INTO t VALUES(NULL, 2)`)
	got := seamInts(seamRows(t, db, path, `SELECT count(*) FROM t`))
	if len(got) != 1 || got[0] != 4 {
		t.Fatalf("got count %v, want [4]", got)
	}
}

// TestSelfRowExprUniqueExpressionIndexKeyIsTheValue verifies that only the
// expression value must be distinct, not the underlying column values.
func TestSelfRowExprUniqueExpressionIndexKeyIsTheValue(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t(a INT, b INT)`,
		`INSERT INTO t VALUES(1, 2)`, // a+b = 3
		`INSERT INTO t VALUES(1, 9)`, // a+b = 10 -- same a, different key
	}
	db, _ := seamDB(t, append(append([]string(nil), stmts...), `CREATE UNIQUE INDEX ok1 ON t(a+b)`)...)
	seamCloseErr(t, db, "")

	db, _ = seamDB(t, stmts...)
	seamExecErr(t, db, `CREATE UNIQUE INDEX bad1 ON t(a * 1)`, "UNIQUE constraint failed")
	seamCloseErr(t, db, "")
}

// TestSelfRowExprExpressionIndexRejectsNonDeterministic verifies that
// expression indexes reject non-deterministic functions.
func TestSelfRowExprExpressionIndexRejectsNonDeterministic(t *testing.T) {
	db, _ := seamDB(t, `CREATE TABLE t(a INT, b INT)`)
	defer db.Close()
	for _, s := range []string{
		`CREATE INDEX ix ON t(random())`,
		`CREATE INDEX ix ON t(a + random())`,
		`CREATE INDEX ix ON t(a) WHERE b > random()`,
	} {
		if err := db.Exec(s); err == nil {
			t.Errorf("Exec(%s): expected a rejection, got success", s)
		}
	}
}

// TestSelfRowExprDeferredDefault verifies that clock-reading DEFAULTs
// (CURRENT_TIMESTAMP) are evaluated per row while constant DEFAULTs are folded.
func TestSelfRowExprDeferredDefault(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT, k INT DEFAULT 7, ts TEXT DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO t(a) VALUES(1)`,
		`INSERT INTO t(a, k) VALUES(2, 9)`,
	)
	rows := seamRows(t, db, path, `SELECT a, k, ts FROM t ORDER BY a`)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0][1].Typ != Int || rows[0][1].I != 7 {
		t.Fatalf("constant DEFAULT gave %+v, want 7", rows[0][1])
	}
	if rows[1][1].Typ != Int || rows[1][1].I != 9 {
		t.Fatalf("explicit value gave %+v, want 9", rows[1][1])
	}
	for i, r := range rows {
		// "YYYY-MM-DD HH:MM:SS" -- the shape SQLite's CURRENT_TIMESTAMP has.
		if r[2].Typ != Text || len(r[2].S) != 19 {
			t.Fatalf("row %d CURRENT_TIMESTAMP DEFAULT = %+v, want a 19-char timestamp", i, r[2])
		}
	}
}

// TestSelfRowExprCheckReferencesOtherColumns verifies CHECKs that reference
// columns other than the one they're defined on.
func TestSelfRowExprCheckReferencesOtherColumns(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT CHECK(a < b), b INT, CHECK(a + b > 0))`,
		`INSERT INTO t VALUES(1, 2)`,
	)
	seamExecErr(t, db, `INSERT INTO t VALUES(5, 2)`, "CHECK constraint failed")
	seamExecErr(t, db, `INSERT INTO t VALUES(-9, -1)`, "CHECK constraint failed")
	seamExec(t, db, `UPDATE t SET b = 100 WHERE a = 1`)
	seamExecErr(t, db, `UPDATE t SET a = 200 WHERE a = 1`, "CHECK constraint failed")
	got := seamInts(seamRows(t, db, path, `SELECT b FROM t`))
	if len(got) != 1 || got[0] != 100 {
		t.Fatalf("got %v, want [100]", got)
	}
}

// Tests below verify that compiled schema expressions remain valid when the
// column list changes, and that stale programs are detected and rejected.

// TestSelfRowExprStaleAfterDropThenAddColumn verifies that DROP COLUMN
// followed by ADD COLUMN re-stamps generated column programs.
func TestSelfRowExprStaleAfterDropThenAddColumn(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(x INT, y INT, g INT AS (y*100) STORED)`,
		`CREATE INDEX ig ON t(g)`,
		`INSERT INTO t(x,y) VALUES(1,2)`,
		`ALTER TABLE t DROP COLUMN x`,
		`ALTER TABLE t ADD COLUMN z INT`,
		`INSERT INTO t(y) VALUES(3)`,
	)
	// The value AS STORED, read out of the live write session's own row store:
	// a plain SELECT after reopening would recompute the generated column from
	// a freshly parsed schema and so could not see a wrong stored value.
	tbl := db.findTableMeta("t")
	if tbl == nil {
		t.Fatal("no table t")
	}
	for rowid, vals := range tbl.rows.all() {
		if len(vals) < 2 {
			t.Fatalf("row %d is %d wide, want at least 2", rowid, len(vals))
		}
		want := vals[0].I * 100
		if vals[1].Typ != Int || vals[1].I != want {
			t.Errorf("row %d stored g=%+v, want %d (y=%+v)", rowid, vals[1], want, vals[0])
		}
	}
	// ... and the index built from those rows must agree with them, which is
	// what structuralCheckAfterCommit decides inside Close.
	seamCloseErr(t, db, "")
	p, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	_, rows, err := p.Query(`SELECT y FROM t WHERE g = 300`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 || rows[0][0].Typ != Int || rows[0][0].I != 3 {
		t.Fatalf("index lookup g=300 returned %+v, want one row y=3", rows)
	}
}

// TestSelfRowExprCheckStaysCompiledAcrossAddColumn verifies that CHECK
// constraint programs remain valid after ADD COLUMN.
func TestSelfRowExprCheckStaysCompiledAcrossAddColumn(t *testing.T) {
	db, _ := seamDB(t,
		`CREATE TABLE t(a INT, b INT, CHECK(a < b))`,
		`INSERT INTO t VALUES(1,2)`,
		`ALTER TABLE t ADD COLUMN c INT`,
	)
	defer db.Close()
	assertRowProgramsCurrent(t, db, "after ADD COLUMN")
	// And it still ENFORCES, through whichever evaluator: both directions, so
	// a CHECK that admitted everything (or nothing) would fail here.
	seamExec(t, db, `INSERT INTO t(a,b) VALUES(3,4)`)
	seamExecErr(t, db, `INSERT INTO t(a,b) VALUES(9,4)`, "CHECK constraint failed")
}

// assertRowProgramsCurrent is the anti-silent-revert gate: every generated
// column and every CHECK of every live table must carry a program compiled
// against the column list that table holds RIGHT NOW. A program that is merely
// present is not enough -- eval refuses one compiled against a different list
// (sameColumnList), so "present but stale" and "absent" have the same
// observable behaviour: a re-compile of the body on every single row.
func assertRowProgramsCurrent(t *testing.T, db *Session, when string) {
	t.Helper()
	for _, tbl := range db.tables {
		assertTableRowProgramsCurrent(t, tbl, when)
	}
}

// assertTableRowProgramsCurrent checks that all compiled programs in a table
// match the current column list.
func assertTableRowProgramsCurrent(t *testing.T, tbl *tableMeta, when string) {
	t.Helper()
	for _, c := range tbl.cols {
		if !c.IsGenerated() {
			continue
		}
		switch {
		case c.genProg == nil:
			t.Errorf("%s: %s.%s has no compiled program", when, tbl.name, c.Name)
		case c.genProg.prog == nil:
			t.Errorf("%s: %s.%s did not compile", when, tbl.name, c.Name)
		case !sameColumnList(c.genProg.cols, tbl.cols):
			t.Errorf("%s: %s.%s's program is STALE -- compiled against a different column list", when, tbl.name, c.Name)
		}
	}
	for _, cc := range tbl.checks {
		switch {
		case cc.prog == nil:
			t.Errorf("%s: %s CHECK(%s) has no compiled program", when, tbl.name, cc.exprText)
		case cc.prog.prog == nil:
			t.Errorf("%s: %s CHECK(%s) did not compile", when, tbl.name, cc.exprText)
		case !sameColumnList(cc.prog.cols, tbl.cols):
			t.Errorf("%s: %s CHECK(%s)'s program is STALE", when, tbl.name, cc.exprText)
		}
	}
}

// TestAddColumnCheckTableIsProgramCurrent verifies that the throwaway table
// used for ADD COLUMN validation has current CHECK programs.
func TestAddColumnCheckTableIsProgramCurrent(t *testing.T) {
	db, _ := seamDB(t,
		`CREATE TABLE t(a INT, g INT AS (a*2) STORED, CHECK(a < 100))`,
		`INSERT INTO t(a) VALUES(1)`,
	)
	defer db.Close()
	tbl := db.findTableMeta("t")
	if tbl == nil {
		t.Fatal("no table t")
	}
	// Build the column list as addColumn does: re-parse the altered schema.
	newCols, rawChecks, _, err := parseCreateTableColumnsAndAutoIndexes(
		`CREATE TABLE t(a INT, g INT AS (a*2) STORED, c INT CHECK(c IS NULL OR c > 0), CHECK(a < 100))`, false)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := finalizeCheckConstraints("t", newCols, rawChecks, false)
	if err != nil {
		t.Fatal(err)
	}
	tmp := addColumnCheckTable(tbl, newCols[len(newCols)-1], checks)
	assertTableRowProgramsCurrent(t, tmp, "the ADD COLUMN back-fill table")
}

// TestSelfRowExprProgramsStayCurrent verifies that all paths that modify a
// column list refresh the compiled programs.
func TestSelfRowExprProgramsStayCurrent(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT, b INT, g INT AS (a+b) STORED, CHECK(a < 1000))`,
		`INSERT INTO t(a,b) VALUES(1,2)`,
	)
	assertRowProgramsCurrent(t, db, "after CREATE TABLE")

	steps := []struct{ what, sql string }{
		{"after ADD COLUMN", `ALTER TABLE t ADD COLUMN c INT`},
		{"after ADD COLUMN with a CHECK", `ALTER TABLE t ADD COLUMN d INT CHECK(d IS NULL OR d > 0)`},
		{"after DROP COLUMN", `ALTER TABLE t DROP COLUMN c`},
		{"after SAVEPOINT", `SAVEPOINT sp1`},
		{"after a write inside the savepoint", `INSERT INTO t(a,b) VALUES(4,5)`},
		{"after ROLLBACK TO", `ROLLBACK TO sp1`},
		{"after RELEASE", `RELEASE sp1`},
	}
	for _, st := range steps {
		seamExec(t, db, st.sql)
		assertRowProgramsCurrent(t, db, st.what)
	}
	seamCloseErr(t, db, "")

	// Reopening re-derives the column lists from stored schema.
	db2, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	defer db2.Close()
	assertRowProgramsCurrent(t, db2, "after reopen")
}

// TestSelfRowExprRenameColumnKeepsItsProgram verifies that ALTER TABLE RENAME
// COLUMN does not require re-stamping the programs.
func TestSelfRowExprRenameColumnKeepsItsProgram(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t(a INT, b INT, g INT AS (a*10) STORED)`,
		`INSERT INTO t(a,b) VALUES(1,2)`,
		`ALTER TABLE t RENAME COLUMN a TO aa`,
	)
	assertRowProgramsCurrent(t, db, "after RENAME COLUMN")
	seamExec(t, db, `INSERT INTO t(aa,b) VALUES(5,6)`)
	tbl := db.findTableMeta("t")
	for rowid, vals := range tbl.rows.all() {
		if vals[2].Typ != Int || vals[2].I != vals[0].I*10 {
			t.Errorf("row %d stored g=%+v, want %d", rowid, vals[2], vals[0].I*10)
		}
	}
	got := seamInts(seamRows(t, db, path, `SELECT g FROM t ORDER BY aa`))
	if len(got) != 2 || got[0] != 10 || got[1] != 50 {
		t.Fatalf("got g = %v, want [10 50]", got)
	}
}

// TestCompileGeneratedColumnsClearsAStaleProgram verifies that
// compileGeneratedColumns unconditionally clears stale programs.
func TestCompileGeneratedColumnsClearsAStaleProgram(t *testing.T) {
	cols := []columnInfo{
		{Name: "a", Aff: affInteger},
		{Name: "g", Aff: affInteger, GeneratedExpr: "a+1"},
	}
	compileGeneratedColumns("t", cols)
	if cols[1].genProg == nil || cols[1].genProg.prog == nil {
		t.Fatal("fixture did not compile")
	}
	// Now the body is no longer parseable -- the shape a re-stamp must not
	// paper over by leaving the old program attached.
	cols[1].GeneratedExpr = "a +* "
	compileGeneratedColumns("t", cols)
	if cols[1].genProg != nil && cols[1].genProg.prog != nil {
		t.Fatal("a stale program survived a re-stamp")
	}
}

// TestSelfRowExprEvalGuards verifies the safety checks in selfRowExpr.eval.
// Each sub-test ensures a guard condition is necessary.
func TestSelfRowExprEvalGuards(t *testing.T) {
	// One real, compiled program over a three-column row, reused below.
	cols := []columnInfo{{Name: "a", Aff: affInteger}, {Name: "b", Aff: affInteger}, {Name: "v", Aff: affInteger}}
	e, err := parseCheckExprText("a*10 + b")
	if err != nil {
		t.Fatal(err)
	}
	scope := tableScope{name: "t", cols: cols, colIndex: buildColIndex(cols), noRowid: true}
	p := compileSelfRowExpr(scope, e, false, pureCtxNone)
	if p.prog == nil {
		t.Fatal("the fixture expression did not compile -- every sub-test below would be vacuous")
	}
	row := []Value{{Typ: Int, I: 3}, {Typ: Int, I: 4}, {}}
	ctx := &evalCtx{tables: []tableScope{scope}, vals: row}
	if v, err := p.eval(ctx); err != nil || v.Typ != Int || v.I != 34 {
		t.Fatalf("baseline: got %+v, %v; want 34", v, err)
	}

	t.Run("nil receiver", func(t *testing.T) {
		// Drop "p == nil" and this is a nil dereference on p.prog. With the
		// guard it reaches restamp, which has nothing to re-compile and
		// REPORTS rather than crashing -- the distinction invariant 3 cares
		// about.
		var nilP *selfRowExpr
		if _, err := nilP.eval(ctx); err == nil {
			t.Fatal("a nil selfRowExpr must report, not answer")
		}
	})

	t.Run("no program", func(t *testing.T) {
		// The shape every caller substitutes for a schema object that carries
		// no prepared program. Drop "p.prog == nil" and this dereferences it.
		raw := &selfRowExpr{expr: e}
		v, err := raw.eval(ctx)
		if err != nil || v.Typ != Int || v.I != 34 {
			t.Fatalf("got %+v, %v; want the re-compiled 34", v, err)
		}
	})

	t.Run("nil context", func(t *testing.T) {
		// Drop "ctx == nil" and this dereferences ctx.tables -- and there is
		// nothing to re-compile against either, since the scope a re-stamp
		// needs lives on ctx. Measured: without the guard this sub-test
		// SEGVs.
		if _, err := p.eval(nil); err == nil {
			t.Fatal("a nil evalCtx must produce an error, not a value")
		}
	})

	t.Run("wrong column list", func(t *testing.T) {
		// The stale-program shape at unit level: an EQUAL-length list, freshly
		// allocated, with the columns rotated. Drop sameColumnList and the
		// program answers from the wrong slots -- 34 instead of the correct 43
		// -- which is a wrong answer, not a crash, and so invisible without
		// this assertion.
		moved := []columnInfo{{Name: "b", Aff: affInteger}, {Name: "a", Aff: affInteger}, {Name: "v", Aff: affInteger}}
		movedScope := tableScope{name: "t", cols: moved, colIndex: buildColIndex(moved), noRowid: true}
		// row is now (b=3, a=4): the correct answer is a*10+b = 43.
		v, err := p.eval(&evalCtx{tables: []tableScope{movedScope}, vals: row})
		if err != nil || v.Typ != Int || v.I != 43 {
			t.Fatalf("got %+v, %v; want the re-compiled 43", v, err)
		}
	})

	t.Run("short row", func(t *testing.T) {
		// A row narrower than the list the program was compiled against. Drop
		// the width check and copy leaves register colBase+1 cleared, so
		// "a*10 + b" quietly answers NULL. With it, no re-stamp can make the
		// program runnable either and eval REPORTS -- which is the whole
		// difference: a silent NULL where a value was expected is exactly
		// invariant 2's failure mode.
		short := []Value{{Typ: Int, I: 3}}
		v, err := p.eval(&evalCtx{tables: []tableScope{scope}, vals: short})
		if err == nil {
			t.Fatalf("a short row must report, not answer: got %+v", v)
		}
	})

	t.Run("no table scope", func(t *testing.T) {
		// Drop the len(ctx.tables) check and this indexes an empty slice.
		if _, err := p.eval(&evalCtx{vals: row}); err == nil {
			t.Fatal("an evalCtx with no table scope must not resolve a column")
		}
	})

	t.Run("no rowid", func(t *testing.T) {
		// A scope that ADMITS the rowid pseudo-column, paired with a ctx that
		// carries no rowid to seed the register with. Drop the guard and
		// "rowid + a" answers NULL from a cleared register -- the same silent
		// wrong answer the width check above exists to prevent, and the one
		// this seam can produce now that a CHECK may name rowid at all.
		rowidScope := tableScope{name: "t", cols: cols, colIndex: buildColIndex(cols)}
		re, err := parseCheckExprText("rowid + a")
		if err != nil {
			t.Fatal(err)
		}
		rp := compileSelfRowExpr(rowidScope, re, true, pureCtxNone)
		if rp.prog == nil {
			t.Fatal("the fixture expression did not compile -- this sub-test would be vacuous")
		}
		withRowid := &evalCtx{tables: []tableScope{rowidScope}, vals: row, rowids: []Value{{Typ: Int, I: 7}}}
		if v, err := rp.eval(withRowid); err != nil || v.Typ != Int || v.I != 10 {
			t.Fatalf("with a rowid: got %+v, %v; want 10", v, err)
		}
		if v, err := rp.eval(&evalCtx{tables: []tableScope{rowidScope}, vals: row}); err == nil {
			t.Fatalf("without a rowid: got %+v, want an error rather than a cleared register", v)
		}
	})
}

// TestSelfRowExprProgramNeedsNoVMState verifies that compiled schema
// expressions use only the VM resources available at eval time.
func TestSelfRowExprProgramNeedsNoVMState(t *testing.T) {
	cols := []columnInfo{{Name: "a", Aff: affInteger}, {Name: "b", Aff: affText}}
	scope := tableScope{name: "t", cols: cols, colIndex: buildColIndex(cols), noRowid: true}
	for _, src := range []string{
		"a > 0",
		"a*2 + length(b)",
		"CASE WHEN a IS NULL THEN 0 ELSE a END",
		"b LIKE 'x%' AND a BETWEEN 1 AND 9",
		"coalesce(b, 'z') || cast(a AS TEXT)",
		"a IN (1,2,3)",
	} {
		e, err := parseCheckExprText(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		p := compileSelfRowExpr(scope, e, false, pureCtxNone)
		if p.prog == nil {
			t.Errorf("%q did not compile -- the decline arm may be over-firing", src)
			continue
		}
		if p.prog.NCursors != 0 || p.prog.NRecRegs != 0 || p.prog.NSorters != 0 || p.prog.NSubCache != 0 || p.prog.NDistinct != 0 {
			t.Errorf("%q compiled to a program wanting VM state eval does not allocate", src)
		}
	}
	// A FROM-less subquery compiles and takes a cache slot.
	fromless, err := parseCheckExprText("a > (SELECT 1)")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fp := compileSelfRowExpr(scope, fromless, false, pureCtxNone)
	if fp.prog == nil {
		t.Fatal("a FROM-less subquery must compile -- eval has nothing else to hand it to")
	}
	if fp.prog.NSubCache != 1 {
		t.Errorf("got NSubCache %d, want 1", fp.prog.NSubCache)
	}
	if fp.prog.NCursors != 0 || fp.prog.NRecRegs != 0 || fp.prog.NSorters != 0 || fp.prog.NDistinct != 0 {
		t.Error("a FROM-less subquery wanted VM state eval does not allocate")
	}
	if v, err := fp.eval(&evalCtx{tables: []tableScope{scope}, vals: []Value{{Typ: Int, I: 5}, {}}}); err != nil || !isTruthy(v) {
		t.Errorf("got %+v, %v; want true (5 > 1)", v, err)
	}

	// A FROM-clause subquery declines: eval cannot run cursor-driven code.
	sub, err := parseCheckExprText("a > (SELECT max(z) FROM other)")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p := compileSelfRowExpr(scope, sub, false, pureCtxNone); p.prog != nil {
		t.Error("a CHECK containing a cursor-driven subquery compiled into a cursor-less program")
	}
}

// BenchmarkSchemaRowExprCompiled measures compiled schema expression
// evaluation on a simple expression. Zero allocations verify that eval
// reuses pooled VM resources.
func benchSelfRowSetup(b *testing.B) (*selfRowExpr, *evalCtx) {
	b.Helper()
	cols := []columnInfo{
		{Name: "a", Aff: affInteger},
		{Name: "b", Aff: affInteger},
		{Name: "v", Aff: affInteger, GeneratedExpr: "a*2 + b"},
	}
	compileGeneratedColumns("t", cols)
	if _, err := cachedGeneratedExpr(cols[2].GeneratedExpr); err != nil {
		b.Fatal(err)
	}
	if cols[2].genProg == nil || cols[2].genProg.prog == nil {
		b.Fatal("generated column did not compile -- the benchmark would measure a per-row re-compile")
	}
	scope := tableScope{name: "t", cols: cols, colIndex: buildColIndex(cols)}
	ctx := &evalCtx{tables: []tableScope{scope}, vals: []Value{{Typ: Int, I: 3}, {Typ: Int, I: 4}, {}}}
	// Having a Program is not enough: eval also has to be WILLING to run it
	// against this ctx (selfRowExpr.runnable, vdbe_run.go). Without this the
	// "compiled" benchmark could quietly be measuring a per-row re-compile.
	if !cols[2].genProg.runnable(ctx) {
		b.Fatal("eval would decline this ctx -- the benchmark would measure a per-row re-compile")
	}
	return cols[2].genProg, ctx
}

func BenchmarkSchemaRowExprCompiled(b *testing.B) {
	p, ctx := benchSelfRowSetup(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.eval(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
