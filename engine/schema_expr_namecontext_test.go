// Tests name context resolution for schema-time row expressions (CHECK, partial
// index WHERE, generated column, index KEY). Database qualifiers and rowid are
// silently dropped or rejected depending on context; these used to fall back to AST eval.
package engine

import "testing"

// TestSchemaExprNameContextCompiles checks that schema expressions compile.
func TestSchemaExprNameContextCompiles(t *testing.T) {
	db, _ := seamDB(t,
		`CREATE TABLE t(a INT, CHECK(rowid < 100))`,
		`CREATE TABLE q(a INTEGER PRIMARY KEY, b INT, CHECK(rowid < 100))`,
		`CREATE TABLE c(a INT, CHECK(xyzzy.c.a > 0))`,
		`CREATE TABLE m(a INT, CHECK(main.m.a > 0))`,
		`CREATE TABLE p(a INT, b INT)`,
		`CREATE INDEX pi ON p(a) WHERE xyzzy.p.b BETWEEN 5 AND 10`,
	)
	defer db.Close()

	for _, name := range []string{"t", "q", "c", "m"} {
		tbl := db.findTableMeta(name)
		if tbl == nil {
			t.Fatalf("no table %s", name)
		}
		if len(tbl.checks) != 1 {
			t.Fatalf("table %s: got %d CHECK constraints, want 1 -- the assertion below would be vacuous", name, len(tbl.checks))
		}
		if tbl.checks[0].prog == nil || tbl.checks[0].prog.prog == nil {
			t.Errorf("table %s: CHECK(%s) was NOT compiled", name, tbl.checks[0].exprText)
		}
	}

	ptbl := db.findTableMeta("p")
	if ptbl == nil {
		t.Fatal("no table p")
	}
	seen := 0
	for _, idx := range db.indexes {
		if idx.name != "pi" {
			continue
		}
		seen++
		whereProg, _ := compileIndexExprs(ptbl, idx)
		if whereProg == nil || whereProg.prog == nil {
			t.Error("index pi: the partial WHERE's database qualifier stopped it compiling (resolve.c:316 drops it)")
		}
	}
	if seen != 1 {
		t.Fatalf("got %d indexes named pi, want 1 -- the assertion above would be vacuous", seen)
	}
}

// TestIgnoreDbQualifierIsTheCheckAndPartialIndexMask checks database qualifier handling.
func TestIgnoreDbQualifierIsTheCheckAndPartialIndexMask(t *testing.T) {
	cols := []columnInfo{{Name: "a", Aff: affInteger}}
	scope := tableScope{name: "t", cols: cols, colIndex: buildColIndex(cols), noRowid: true}
	e, err := parseCheckExprText("xyzzy.t.a > 0")
	if err != nil {
		t.Fatal(err)
	}
	if p := compileSelfRowExpr(scope, e, true, pureCtxNone); p.prog == nil {
		t.Error("NC_IsCheck/NC_PartIdx: a database-qualified reference must COMPILE -- resolve.c:313-321 drops the qualifier")
	}
	if p := compileSelfRowExpr(scope, e, false, pureCtxNone); p.prog != nil {
		t.Error("NC_IdxExpr/NC_GenCol are NOT in resolve.c:316's mask -- a database-qualified reference must not silently resolve there")
	}
	// Non-vacuity: the same expression WITHOUT the qualifier compiles either
	// way, so neither answer above is just "this parser produced nothing".
	plain, err := parseCheckExprText("a > 0")
	if err != nil {
		t.Fatal(err)
	}
	for _, ignore := range []bool{true, false} {
		if p := compileSelfRowExpr(scope, plain, ignore, pureCtxNone); p.prog == nil {
			t.Fatalf("ignoreDbQualifier=%v: an unqualified reference did not compile -- the fixture is broken", ignore)
		}
	}
}

// TestCheckProgramScopeFollowsVisibleRowid checks rowid visibility in CHECK.
func TestCheckProgramScopeFollowsVisibleRowid(t *testing.T) {
	cols := []columnInfo{{Name: "a", Aff: affInteger}}
	e, err := parseCheckExprText("rowid < 100")
	if err != nil {
		t.Fatal(err)
	}
	if p := compileSelfRowExpr(checkProgramScope("t", cols, false), e, true, pureCtxNone); p.prog == nil {
		t.Error("a rowid table's CHECK must compile a rowid reference (resolve.c:564-568, VisibleRowid true)")
	}
	if p := compileSelfRowExpr(checkProgramScope("t", cols, true), e, true, pureCtxNone); p.prog != nil {
		t.Error("a WITHOUT ROWID table has no rowid pseudo-column at all (build.c:2731 sets TF_NoVisibleRowid)")
	}
}

// TestCheckRowidRejectedOnWithoutRowidTable checks rowid is rejected in WITHOUT ROWID CHECKs.
func TestCheckRowidRejectedOnWithoutRowidTable(t *testing.T) {
	db, _ := seamDB(t)
	defer db.Close()
	seamExecErr(t, db,
		`CREATE TABLE b(x INT PRIMARY KEY, CHECK(rowid < 100)) WITHOUT ROWID`,
		"no such column: rowid")
}

// TestCheckRowidIsEnforced verifies CHECK constraints using rowid are enforced.
func TestCheckRowidIsEnforced(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE a(x INT, CHECK(rowid < 100))`,
		`CREATE TABLE q(x INTEGER PRIMARY KEY, CHECK(rowid < 100))`,
		`INSERT INTO a VALUES(1)`,
		`INSERT INTO q VALUES(5)`,
	)
	seamExecErr(t, db, `INSERT INTO a(rowid,x) VALUES(500,3)`, "CHECK constraint failed")
	seamExecErr(t, db, `INSERT INTO q VALUES(500)`, "CHECK constraint failed")
	seamExecErr(t, db, `UPDATE q SET x=900 WHERE x=5`, "CHECK constraint failed")
	// The rowid does not move here, so the CHECK is not re-evaluated at all
	// (sqlite3ExprReferencesUpdatedColumn -- see checkChangeSet).
	seamExec(t, db, `UPDATE a SET x=9 WHERE x=1`)
	if got := seamInts(seamRows(t, db, path, `SELECT x FROM a`)); len(got) != 1 || got[0] != 9 {
		t.Fatalf("got a.x = %v, want [9]", got)
	}
}

// TestDbQualifiedCheckIsEnforced verifies db-qualified CHECKs are enforced.
func TestDbQualifiedCheckIsEnforced(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE i(x INT, CHECK(xyzzy.i.x > 0))`,
		`INSERT INTO i VALUES(1)`,
	)
	seamExecErr(t, db, `INSERT INTO i VALUES(-1)`, "CHECK constraint failed")
	if got := seamInts(seamRows(t, db, path, `SELECT x FROM i`)); len(got) != 1 || got[0] != 1 {
		t.Fatalf("got i.x = %v, want [1]", got)
	}
}

// TestDbQualifiedPartialIndexMaterializes checks db-qualified partial index WHERE.
func TestDbQualifiedPartialIndexMaterializes(t *testing.T) {
	db, path := seamDB(t,
		`CREATE TABLE t3(a INT, b INT)`,
		`CREATE INDEX t3i ON t3(a) WHERE xyzzy.t3.b BETWEEN 5 AND 10`,
		`INSERT INTO t3 VALUES(1,7)`,
		`INSERT INTO t3 VALUES(2,70)`,
		`INSERT INTO t3 VALUES(3,5)`,
	)
	got := seamInts(seamRows(t, db, path, `SELECT a FROM t3 WHERE b BETWEEN 5 AND 10 ORDER BY a`))
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("got %v, want [1 3] -- the partial index admitted the wrong rows", got)
	}
}

// TestSelfRowExprUnlowerableIsAnError checks that unlowerable expressions error.
func TestSelfRowExprUnlowerableIsAnError(t *testing.T) {
	cols := []columnInfo{{Name: "a", Aff: affInteger}}
	scope := tableScope{name: "t", cols: cols, colIndex: buildColIndex(cols), noRowid: true}
	sub, err := parseCheckExprText("a > (SELECT max(z) FROM other)")
	if err != nil {
		t.Fatal(err)
	}
	p := compileSelfRowExpr(scope, sub, false, pureCtxNone)
	if p.prog != nil {
		t.Fatal("a cursor-driven subquery must not compile here -- this program has no row source to read (see compileSelfRowExpr)")
	}
	ctx := &evalCtx{tables: []tableScope{scope}, vals: []Value{{Typ: Int, I: 1}}}
	if v, err := p.eval(ctx); err == nil {
		t.Fatalf("got %+v, want an error -- an un-lowerable expression must never be answered another way (RULE #1)", v)
	}
}

// TestDbQualifiedCheckCompiles checks db-qualified CHECKs compile successfully.
func TestDbQualifiedCheckCompiles(t *testing.T) {
	for _, tc := range []struct{ name, ddl, stmt string }{
		{"three-part in a CHECK", `CREATE TABLE i(x INT, CHECK(xyzzy.i.x > 0))`, `INSERT INTO i VALUES(1)`},
		{"three-part, named constraint", `CREATE TABLE j(x INT, CONSTRAINT c1 CHECK(nosuchdb.j.x > 0))`, `INSERT INTO j VALUES(1)`},
		{"three-part on an UPDATE", `CREATE TABLE k(x INT, CHECK(zzz.k.x > 0))`, `UPDATE k SET x=2`},
		{"ordinary CHECK still compiles", `CREATE TABLE m(x INT, CHECK(x > 0))`, `INSERT INTO m VALUES(1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			if err := db.Exec(tc.ddl); err != nil {
				t.Fatalf("%q: %v", tc.ddl, err)
			}
			_, cerr := db.compileWrite(tc.stmt)
			if cerr != nil {
				t.Fatalf("%q: %v -- C SQLite accepts a database qualifier here and ignores it", tc.stmt, cerr)
			}
		})
	}
}
