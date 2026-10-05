package engine

// This file tests that correlated subqueries in UPDATE/DELETE WHERE and SET
// clauses compile to bytecode and correctly read the outer row. The differential
// harness cannot prove this because the pre-compilation route also produces correct answers,
// so this file verifies the compiled programs themselves.

import (
	"strings"
	"testing"
)

// writeCorrelatedShapes are UPDATE/DELETE statements with correlated subqueries
// in SET or WHERE clauses.
var writeCorrelatedShapes = []conflictShapeCase{
	// The census shape, verbatim from writeShapeResidualCases.
	{[]string{`CREATE TABLE t1(a,b)`, `CREATE TABLE t2(x,y)`},
		`UPDATE t1 SET b=b+(SELECT y FROM t2 WHERE x=a)`},
	{[]string{`CREATE TABLE t11(a,b)`},
		`DELETE FROM t11 AS xyz WHERE EXISTS(SELECT 1 FROM t11 WHERE t11.a>xyz.a AND t11.b<=xyz.b)`},

	// Target qualified by name, not alias.
	{[]string{`CREATE TABLE o(a,b)`, `CREATE TABLE inr(x)`},
		`UPDATE o SET b=(SELECT count(*) FROM inr WHERE x = o.a)`},

	// FROM-less subquery: only reads the outer frame.
	{[]string{`CREATE TABLE c1(a,b)`}, `UPDATE c1 SET b=(SELECT CASE WHEN c1.a='ABC' THEN 'HIT' ELSE b END)`},
	{[]string{`CREATE TABLE c1(a,b)`}, `UPDATE c1 SET b='HIT' WHERE (SELECT c1.a='ABC')`},
	{[]string{`CREATE TABLE c1(a,b)`}, `DELETE FROM c1 WHERE (SELECT c1.a='ABC')`},

	// Compound body: parent frame passed directly to each arm.
	{[]string{`CREATE TABLE c1(a,b)`}, `UPDATE c1 SET b='HIT' WHERE EXISTS (SELECT c1.a INTERSECT SELECT 'ABC')`},

	// Nested: correlated reference two frames down inside IN and nested EXISTS.
	{[]string{`CREATE TABLE n1(k,v)`, `CREATE TABLE n2(k,w)`},
		`UPDATE n1 SET v=(SELECT count(*) FROM n2 WHERE k IN (SELECT k FROM n2 z WHERE z.w > n1.k))`},
	{[]string{`CREATE TABLE n1(k,v)`, `CREATE TABLE n2(k,w)`},
		`DELETE FROM n1 WHERE k IN (SELECT k FROM n2 WHERE n2.w > n1.k)`},

	// Rowid pseudo-column across the frame.
	{[]string{`CREATE TABLE r1(a,b)`, `CREATE TABLE r2(k,v)`},
		`UPDATE r1 SET b=(SELECT v FROM r2 WHERE r2.k = r1.rowid)`},

	// With leading CTE.
	{[]string{`CREATE TABLE w1(a,b)`, `CREATE TABLE w2(k,v)`},
		`WITH cc AS (SELECT k,v FROM w2) UPDATE w1 SET b=(SELECT v FROM cc WHERE cc.k=w1.a)`},

	// View write: same shape over a different cursor.
	{[]string{`CREATE TABLE b(x,c)`, `CREATE TABLE s(k,v)`, `CREATE VIEW v AS SELECT x,c FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN DELETE FROM b WHERE x=old.x; END`},
		`DELETE FROM v WHERE EXISTS(SELECT 1 FROM s WHERE s.k=v.x)`},
	{[]string{`CREATE TABLE b(x,c)`, `CREATE TABLE s(k,v)`, `CREATE VIEW v AS SELECT x,c FROM b`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET c=new.c WHERE x=old.x; END`},
		`UPDATE v SET c=(SELECT v FROM s WHERE s.k=v.x)`},
}

// compileWriteShape is the shared prologue: build a database, run setup, compile
// the statement. It compiles, never executes -- this file measures the COMPILER.
func compileWriteShape(t *testing.T, tc conflictShapeCase) (*Session, *Program, error) {
	t.Helper()
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, s := range tc.setup {
		if err := db.Exec(s); err != nil {
			db.Discard()
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	prog, cerr := db.compileWrite(tc.stmt)
	return db, prog, cerr
}

func TestWriteCorrelatedCompilesToBytecode(t *testing.T) {
	for _, tc := range writeCorrelatedShapes {
		db, _, cerr := compileWriteShape(t, tc)
		if cerr != nil {
			t.Errorf("%s\n  compile error: %v -- see vdbe_write_rowlive.go", tc.stmt, cerr)
		}
		db.Discard()
	}
}

// walkPrograms visits prog and all sub-Programs reachable through P4 payloads.
func walkPrograms(prog *Program, seen map[*Program]bool, visit func(p *Program)) {
	if prog == nil || seen[prog] {
		return
	}
	seen[prog] = true
	visit(prog)
	if prog.Compound != nil {
		for _, arm := range prog.Compound.arms {
			walkPrograms(arm, seen, visit)
		}
	}
	for i := range prog.Insns {
		for _, sub := range subProgramsOf(prog.Insns[i].P4) {
			walkPrograms(sub, seen, visit)
		}
	}
}

// TestWriteCorrelatedReadsTheOuterFrame checks that sub-programs actually read
// the outer frame's cursor and are marked correlated for per-row re-execution.
func TestWriteCorrelatedReadsTheOuterFrame(t *testing.T) {
	cases := []struct {
		tc      conflictShapeCase
		wantOp  OpCode
		wantP1  int // the enclosing frame's cursor number: the write scan's own
		wantP5  uint16
		subOnly bool // the opcode must live in a SUB-program, never the top level
	}{
		{conflictShapeCase{[]string{`CREATE TABLE t1(a,b)`, `CREATE TABLE t2(x,y)`},
			`UPDATE t1 SET b=b+(SELECT y FROM t2 WHERE x=a)`}, OpOuterColumn, 0, 1, true},
		{conflictShapeCase{[]string{`CREATE TABLE t11(a,b)`},
			`DELETE FROM t11 AS xyz WHERE EXISTS(SELECT 1 FROM t11 WHERE t11.a>xyz.a AND t11.b<=xyz.b)`},
			OpOuterColumn, 0, 1, true},
		{conflictShapeCase{[]string{`CREATE TABLE r1(a,b)`, `CREATE TABLE r2(k,v)`},
			`UPDATE r1 SET b=(SELECT v FROM r2 WHERE r2.k = r1.rowid)`}, OpOuterRowid, 0, 1, true},
	}
	for _, cs := range cases {
		db, prog, cerr := compileWriteShape(t, cs.tc)
		if cerr != nil {
			t.Errorf("%s: compile: %v", cs.tc.stmt, cerr)
			db.Discard()
			continue
		}
		found, atTop := 0, 0
		top := true
		walkPrograms(prog, map[*Program]bool{}, func(p *Program) {
			hit := 0
			for i := range p.Insns {
				in := p.Insns[i]
				if in.Op != cs.wantOp {
					continue
				}
				if in.P1 != cs.wantP1 || in.P5 != cs.wantP5 {
					t.Errorf("%s: %v against cursor %d at P5=%d, want cursor %d at P5=%d",
						cs.tc.stmt, in.Op, in.P1, in.P5, cs.wantP1, cs.wantP5)
				}
				hit++
			}
			if hit > 0 {
				found += hit
				if top {
					atTop += hit
				}
				if !p.Correlated {
					t.Errorf("%s: the sub-program holding %v is not marked Correlated, so runSub would cache it "+
						"through runSubOnce -- whose exec has no parent frame for the opcode to read",
						cs.tc.stmt, cs.wantOp)
				}
			}
			top = false
		})
		if found == 0 {
			t.Errorf("%s: no %v anywhere in the program graph -- the correlated reference was not read across the frame",
				cs.tc.stmt, cs.wantOp)
		}
		if cs.subOnly && atTop > 0 {
			t.Errorf("%s: %v emitted in the TOP-LEVEL write program, which has no parent frame at all",
				cs.tc.stmt, cs.wantOp)
		}
		db.Discard()
	}
}

// TestWriteCorrelatedReturningReadsTheSnapshotNotTheCursor pins where the span
// stops AND what took over past it.
//
// emitReturning is emitted BELOW the row's rewrite (compileUpdateStmt) and below
// OpDelete (compileDeleteStmt), so the write scan's cursor no longer stands on
// the row a correlated read there would mean, and rowLiveSpan's undo runs before
// both. That much is unchanged, and it is why these two shapes DECLINED: a
// reference to the affected row inside a RETURNING subquery had no frame to
// cross and nothing else to bind to.
//
// It does not have to cross one. C's own road is the one this engine now takes:
// sqlite3ProcessReturningSubqueries codes RETURNING IN-LINE rather than as a
// sub-program (trigger.c:1015-1017) and lookupName rewrites the reference to a
// TK_REGISTER naming the affected row's block (resolve.c:587-593, off
// NC_UBaseReg). emitReturning snapshots that block onto the machine
// (OpPseudoRow) and the reference becomes an OpParam read of it -- a VALUE, not
// a cursor, so where the scan cursor stands by then is irrelevant.
//
// The two statements below are the ones this function used to assert declined.
// Both are answered by 3.53.3 and both now agree with it, verified end to end
// in compat-harness/pseudorow_subquery_test.go
// (TestReturningSubqueryPastTheCorrelationSpan), including the spellings whose
// RETURNING subquery reads the MODIFIED table itself.
func TestWriteCorrelatedReturningReadsTheSnapshotNotTheCursor(t *testing.T) {
	cases := []conflictShapeCase{
		{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x,y)`},
			`UPDATE t SET b=(SELECT y FROM s WHERE x=a) RETURNING a,(SELECT count(*) FROM s WHERE s.x=t.a)`},
		{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x,y)`},
			`DELETE FROM t WHERE a=1 RETURNING a,(SELECT count(*) FROM s WHERE s.x=t.a)`},
	}
	for _, tc := range cases {
		db, prog, cerr := compileWriteShape(t, tc)
		if cerr != nil {
			t.Errorf("%s\n  declined: %v", tc.stmt, cerr)
			db.Discard()
			continue
		}
		if !progHas(prog, pseudoRowOp(3)) || !progHas(prog, paramOp(3)) {
			t.Errorf("%s\n  the RETURNING subquery's reference to the affected row did not become an "+
				"OpParam read of the snapshotted row", tc.stmt)
		}
		// The point of the mechanism: NO sub-program reachable from the
		// RETURNING block may be Correlated, because a correlated one is run by
		// execWithParent against the enclosing FRAME -- which is the write
		// scan's cursor, standing past the row by then. A live sub-program is
		// refused outright if it is (compileLiveSubProgram), so this is a
		// second reading of a guarantee the compiler already enforces.
		walkPrograms(prog, map[*Program]bool{}, func(p *Program) {
			for i := range p.Insns {
				if in := p.Insns[i]; in.Op == OpParam && in.P1 == 3 && p.Correlated {
					t.Errorf("%s\n  the sub-program reading the affected row is marked Correlated, so it "+
						"would be re-run against the write scan's frame instead of off the machine", tc.stmt)
					return
				}
			}
		})
		db.Discard()
	}
}

// TestWriteCorrelatedSelfReadAnswersLive is the other half: a correlated SET
// subquery over the TARGET table is re-evaluated per row against the LIVE,
// partially-updated table in C (EP_VarSelect, resolve.c:1403-1404), so each row
// sees the rows before it already rewritten. The frozen writeSubqueryPager
// snapshot answers 2,4,6 here; the live lowering (liveRowCtx,
// vdbe_live_read.go) answers C's 2,4,5 (compat-harness
// TestUpdateSetLiveSelfRead's "running count over x"). Run twice ENGINE-DIRECT,
// because the harness re-prepares every statement and so cannot see a cached
// program answering the second run from the first one's image.
func TestWriteCorrelatedSelfReadAnswersLive(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{`CREATE TABLE t(x)`, `INSERT INTO t VALUES(1),(2),(3)`} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for run, want := range []string{"2,4,5", "3,6,7"} {
		if err := db.Exec(`UPDATE t SET x=x+(SELECT count(*) FROM t t2 WHERE t2.x<=t.x)`); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		p, err := db.SnapshotPager()
		if err != nil {
			t.Fatal(err)
		}
		_, rows, err := p.Query(`SELECT group_concat(x) FROM (SELECT x FROM t ORDER BY rowid)`)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(rows[0][0].S); got != want {
			t.Errorf("run %d: x=%s, want %s", run, got, want)
		}
	}
}

// TestWriteCorrelatedAnswers runs the promoted shapes and checks the values,
// because (1) and (2) together still only say the program has the right SHAPE.
// Every expectation here was taken from the 3.53.3 oracle first -- see
// compat-harness/write_correlated_test.go, which runs the same statements
// through differ().
func TestWriteCorrelatedAnswers(t *testing.T) {
	cases := []struct {
		setup []string
		stmts []string
		query string
		want  string
	}{
		{[]string{`CREATE TABLE t1(a,b)`, `CREATE TABLE t2(x,y)`,
			`INSERT INTO t1 VALUES(1,10),(2,20),(3,30)`, `INSERT INTO t2 VALUES(1,100),(2,200)`},
			[]string{`UPDATE t1 SET b=b+(SELECT y FROM t2 WHERE x=a)`},
			`SELECT a,b FROM t1 ORDER BY a`, "1|110 2|220 3|NULL"},

		{[]string{`CREATE TABLE t11(a,b)`, `INSERT INTO t11 VALUES(1,5),(2,4),(3,9)`},
			[]string{`DELETE FROM t11 AS xyz WHERE EXISTS(SELECT 1 FROM t11 WHERE t11.a>xyz.a AND t11.b<=xyz.b)`},
			`SELECT a,b FROM t11 ORDER BY a`, "2|4 3|9"},

		{[]string{`CREATE TABLE o(a,b)`, `CREATE TABLE inr(x)`,
			`INSERT INTO o VALUES(1,0),(2,0),(3,0)`, `INSERT INTO inr VALUES(1),(1),(2)`},
			[]string{`UPDATE o SET b=(SELECT count(*) FROM inr WHERE x = o.a)`},
			`SELECT a,b FROM o ORDER BY a`, "1|2 2|1 3|0"},

		{[]string{`CREATE TABLE r1(a,b)`, `CREATE TABLE r2(k,v)`,
			`INSERT INTO r1 VALUES(1,0),(2,0)`, `INSERT INTO r2 VALUES(1,'p'),(2,'q')`},
			[]string{`UPDATE r1 SET b=(SELECT v FROM r2 WHERE r2.k = r1.rowid)`},
			`SELECT a,b FROM r1 ORDER BY a`, "1|p 2|q"},

		// A FROM-less correlated subquery: the sub-program's only read IS the
		// outer frame.
		{[]string{`CREATE TABLE c1(a,b)`, `INSERT INTO c1 VALUES('ABC','x'),('def','y')`},
			[]string{`UPDATE c1 SET b=(SELECT CASE WHEN c1.a='ABC' THEN 'HIT' ELSE b END)`},
			`SELECT a,b FROM c1 ORDER BY a`, "ABC|HIT def|y"},

		// A VIEW write, whose correlated read crosses the frame into the
		// MATERIALIZED view's cursor rather than a table's.
		{[]string{`CREATE TABLE b(x,c)`, `CREATE TABLE s(k,v)`, `CREATE VIEW v AS SELECT x,c FROM b`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN UPDATE b SET c=new.c WHERE x=old.x; END`,
			`INSERT INTO b VALUES(1,'a'),(2,'b'),(3,'c')`, `INSERT INTO s VALUES(1,'P'),(3,'R')`},
			[]string{`UPDATE v SET c=(SELECT v FROM s WHERE s.k=v.x)`},
			`SELECT x,c FROM b ORDER BY x`, "1|P 2|NULL 3|R"},

		// Two frames down, through an IN inside an aggregate body.
		{[]string{`CREATE TABLE n1(k,v)`, `CREATE TABLE n2(k,w)`,
			`INSERT INTO n1 VALUES(1,0),(2,0),(3,0)`, `INSERT INTO n2 VALUES(1,10),(2,20)`},
			[]string{`DELETE FROM n1 WHERE k IN (SELECT k FROM n2 WHERE n2.w > n1.k)`},
			`SELECT k,v FROM n1 ORDER BY k`, "3|0"},

		// The correlated read is re-run per row, not cached: a run-once cache
		// would give every row the FIRST row's answer, which this asserts is not
		// what happens (0,1,2 rather than 0,0,0).
		{[]string{`CREATE TABLE p1(a,b)`, `CREATE TABLE p2(x)`,
			`INSERT INTO p1 VALUES(1,-1),(2,-1),(3,-1)`, `INSERT INTO p2 VALUES(1),(2)`},
			[]string{`UPDATE p1 SET b=(SELECT count(*) FROM p2 WHERE x < p1.a)`},
			`SELECT a,b FROM p1 ORDER BY a`, "1|0 2|1 3|2"},
	}
	for _, tc := range cases {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		bad := false
		for _, s := range append(append([]string(nil), tc.setup...), tc.stmts...) {
			if err := db.Exec(s); err != nil {
				t.Errorf("%q: %v", s, err)
				bad = true
				break
			}
		}
		if bad {
			db.Discard()
			continue
		}
		p, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatal(perr)
		}
		_, rows, qerr := p.Query(tc.query)
		if qerr != nil {
			t.Errorf("%q: %v", tc.query, qerr)
			db.Discard()
			continue
		}
		var got []string
		for _, r := range rows {
			cells := make([]string, len(r))
			for i, v := range r {
				cells[i] = valueDebugString(v)
			}
			got = append(got, strings.Join(cells, "|"))
		}
		if g := strings.Join(got, " "); g != tc.want {
			t.Errorf("%v\n  %s\n  got  %s\n  want %s", tc.stmts, tc.query, g, tc.want)
		}
		db.Discard()
	}
}
