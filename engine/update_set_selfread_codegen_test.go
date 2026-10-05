package engine

// UPDATE SET with subqueries over the target table is lowered to emit a scan
// plus a sub-program. The tests verify compilation, program structure, handling
// of correlated vs uncorrelated shapes, and correct answers.

import (
	"strconv"
	"strings"
	"testing"
)

// updateSetSelfReadShapes: uncorrelated subqueries reading the target table.
var updateSetSelfReadShapes = []conflictShapeCase{
	{[]string{`CREATE TABLE t(a,b)`}, `UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`},

	{[]string{`CREATE TABLE t(x)`}, `UPDATE t SET x=(SELECT sum(x) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`}, `UPDATE t SET b=(SELECT EXISTS(SELECT 1 FROM t WHERE b IS NULL))`},
	{[]string{`CREATE TABLE t(a,b)`}, `UPDATE t SET b=(a IN (SELECT a FROM t WHERE b=0))`},
	// With AS alias.
	{[]string{`CREATE TABLE t(a,b)`}, `UPDATE t AS z SET b=(SELECT count(*) FROM t)`},
	// With WHERE subquery.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE o(k)`},
		`UPDATE t SET b=(SELECT sum(b) FROM t) WHERE a IN (SELECT k FROM o)`},
	// In CASE expression.
	{[]string{`CREATE TABLE t(a,b)`},
		`UPDATE t SET b=CASE WHEN a IN (SELECT a FROM t WHERE b>15) THEN 1 ELSE 0 END`},
	// Compound subquery.
	{[]string{`CREATE TABLE t(a,b)`},
		`UPDATE t SET b=(SELECT count(*) FROM (SELECT a FROM t UNION ALL SELECT a FROM t))`},
	// Through VIEW and WITH.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT * FROM t`},
		`UPDATE t SET b=(SELECT count(*) FROM v)`},
	{[]string{`CREATE TABLE t(a,b)`},
		`WITH c AS (SELECT count(*) AS n FROM t) UPDATE t SET b=(SELECT n FROM c)`},
	// With rowid update.
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`},
		`UPDATE t SET k=k+10, b=(SELECT count(*) FROM t)`},
	// With REPLACE conflict resolution.
	{[]string{`CREATE TABLE t(a,b UNIQUE)`}, `UPDATE OR REPLACE t SET b=(SELECT count(*) FROM t)`},
}

// updateSetSelfReadLive: correlated subqueries evaluated per row.
var updateSetSelfReadLive = []conflictShapeCase{
	{[]string{`CREATE TABLE t(x)`}, `UPDATE t SET x=x+(SELECT count(*) FROM t t2 WHERE t2.x<=t.x)`},
	{[]string{`CREATE TABLE t(a,b)`},
		`UPDATE t SET b=(SELECT count(*) FROM t x WHERE x.a IN (SELECT y.a FROM t y WHERE y.a<=t.a))`},
	{[]string{`CREATE TABLE t(a,b)`},
		`UPDATE t AS o SET b=(SELECT count(*) FILTER (WHERE a<o.a) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`},
		`UPDATE t AS o SET b=(SELECT count(*) OVER (PARTITION BY o.a) FROM t LIMIT 1)`},
	{[]string{`CREATE TABLE t(a,b)`},
		`UPDATE t AS o SET b=(SELECT sum(a) OVER w FROM t WINDOW w AS (ORDER BY o.a) LIMIT 1)`},
	{[]string{`CREATE TABLE t(a TEXT,b)`, `CREATE TABLE u(p,q,r)`},
		`UPDATE t AS o SET b=(SELECT count(*) FROM t, pragma_table_info(o.a))`},
	{[]string{`CREATE TABLE t(a,b)`},
		`WITH c AS (SELECT 1) UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a)`},
	// The target reached only through a CTE or a view: no name in the SET
	// expression names it, the compiled program does (programReadsTable).
	{[]string{`CREATE TABLE t(a,b)`},
		`WITH c AS (SELECT a, b FROM t) UPDATE t SET b=(SELECT sum(b) FROM c WHERE c.a<=t.a)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT a, b FROM t`},
		`UPDATE t SET b=(SELECT sum(b) FROM v WHERE v.a<=t.a)`},
	// Index-driven WHERE.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE INDEX tb ON t(b)`},
		`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE b>0`},
	// INDEXED BY.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE INDEX tb ON t(b)`},
		`UPDATE t INDEXED BY tb SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a)`},
	// WITHOUT ROWID.
	{[]string{`CREATE TABLE t(a PRIMARY KEY,b) WITHOUT ROWID`},
		`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a)`},
	// WITH ANALYZE.
	{[]string{`CREATE TABLE t(a,b,c)`, `CREATE INDEX tc ON t(c)`, `INSERT INTO t VALUES(1,1,1),(2,2,2)`, `ANALYZE`},
		`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE c>0`},
}

func TestUpdateSetSelfReadCompilesToBytecode(t *testing.T) {
	for i, tc := range updateSetSelfReadShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// TestUpdateSetSelfReadEmitsAScanAndASubProgram is the non-vacuity half.
//
// Assertion (1) alone is satisfiable by an emitter that never opens a cursor and
// applies the SET once -- which answers a one-row table correctly, and every
// TestUpdateSetSelfReadEmitsAScanAndASubProgram verifies the program structure:
// a scan loop (OpRewind/OpNext) with OpUpdateRow and compiled subquery programs.
func TestUpdateSetSelfReadEmitsAScanAndASubProgram(t *testing.T) {
	for i, tc := range updateSetSelfReadShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		var openWrite, rewind, next, updateRow, subOps int
		var subProgs int
		for j := range prog.Insns {
			in := &prog.Insns[j]
			switch in.Op {
			case OpOpenWrite:
				openWrite++
			case OpRewind:
				if in.P1 == 0 {
					rewind++
				}
			case OpNext:
				if in.P1 == 0 {
					next++
				}
			case OpUpdateRow:
				updateRow++
			case OpSubquery, OpExists, OpInSub, OpRowSub, OpOpenDerived:
				subOps++
			}
			switch p4 := in.P4.(type) {
			case *Program:
				subProgs++
			case *inSubPlan:
				if p4.prog != nil {
					subProgs++
				}
			case *rowSubPlan:
				if p4.prog != nil {
					subProgs++
				}
			case *derivedSource:
				if p4.prog != nil {
					subProgs++
				}
			}
		}
		if openWrite == 0 || rewind == 0 || next == 0 || updateRow == 0 {
			t.Errorf("[%d] %q: not a scan loop -- OpOpenWrite=%d OpRewind(cur 0)=%d OpNext(cur 0)=%d OpUpdateRow=%d.\n"+
				"An emitter that fires once with no cursor answers a one-row table correctly and\n"+
				"would otherwise pass every other assertion in this file.",
				i, tc.stmt, openWrite, rewind, next, updateRow)
		}
		if subOps == 0 || subProgs == 0 {
			t.Errorf("[%d] %q: no compiled sub-program in the emitted code (subquery opcodes=%d, *Program payloads=%d).\n"+
				"The SET value was folded or dropped, which is not what this promotion claims to do.",
				i, tc.stmt, subOps, subProgs)
		}
	}
}

// TestUpdateSetSelfReadCorrelatedShapesLowerLive verifies correlated subqueries
// compile to live programs re-evaluated per row.
func TestUpdateSetSelfReadCorrelatedShapesLowerLive(t *testing.T) {
	for i, tc := range updateSetSelfReadLive {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		live := 0
		for j := range prog.Insns {
			if p, ok := prog.Insns[j].P4.(*Program); ok && p.LiveRow != nil && p.Correlated {
				live++
			}
		}
		if live == 0 {
			t.Errorf("[%d] %q: no correlated LIVE sub-program. C re-evaluates this subquery per row\n"+
				"against the partially-updated table (EP_VarSelect, resolve.c:1403-1404); a frozen\n"+
				"snapshot answers it wrong.", i, tc.stmt)
		}
	}
}


// TestUpdateSetSelfReadProgramIsNotCacheable verifies programs are not cached,
// since subqueries read frozen snapshots that would be stale on reuse.
func TestUpdateSetSelfReadProgramIsNotCacheable(t *testing.T) {
	for i, tc := range updateSetSelfReadShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		if prog.WritePager == nil {
			t.Errorf("[%d] %q: WritePager is nil, so cachedWriteProgram would CACHE a program whose\n"+
				"SET subquery holds a frozen snapshot -- every later run of the same SQL would\n"+
				"re-read this run's image.", i, tc.stmt)
		}
	}

	db, err := Create(t.TempDir()+"/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,0),(2,0),(3,0)`} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for run := 0; run < 2; run++ {
		if err := db.Exec(`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		p, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatal(perr)
		}
		_, rows, qerr := p.Query(`SELECT b FROM t ORDER BY rowid`)
		if qerr != nil {
			t.Fatal(qerr)
		}
		if rows[0][0].Typ != Int {
			t.Fatalf("run %d: b is not an integer: %+v", run, rows[0][0])
		}
		got = append(got, strconv.FormatInt(rows[0][0].I, 10))
	}
	if want := "3 0"; strings.Join(got, " ") != want {
		t.Errorf("self-reading SET subquery repeated engine-direct: got %q, want %q -- "+
			"a cached program answers \"3 3\"", strings.Join(got, " "), want)
	}
}

// TestUpdateSetSelfReadCompiledAnswers verifies correct answers across various
// UPDATE patterns with self-reading subqueries.
func TestUpdateSetSelfReadCompiledAnswers(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		{"once over the pre-update image",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,0),(2,0),(3,0)`},
			`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`,
			`SELECT a,b FROM t ORDER BY a`, []string{"1,3", "2,3", "3,3"}},
		{"filtering WHERE",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,0),(2,0),(3,0),(4,0)`},
			`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0) WHERE a>2`,
			`SELECT a,b FROM t ORDER BY a`, []string{"1,0", "2,0", "3,4", "4,4"}},
		{"WHERE matches nothing",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,10),(2,20)`},
			`UPDATE t SET b=(SELECT count(*) FROM t) WHERE a>99`,
			`SELECT a,b FROM t ORDER BY a`, []string{"1,10", "2,20"}},
		{"rowid moves",
			[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`, `INSERT INTO t VALUES(1,0),(2,0),(3,0)`},
			`UPDATE t SET k=k+10, b=(SELECT count(*) FROM t)`,
			`SELECT k,b FROM t ORDER BY k`, []string{"11,3", "12,3", "13,3"}},
		{"CTE shadows a real table",
			[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE c(n)`,
				`INSERT INTO t VALUES(1,0),(2,0)`, `INSERT INTO c VALUES(99)`},
			`WITH c AS (SELECT count(*) AS n FROM t) UPDATE t SET b=(SELECT n FROM c)`,
			`SELECT a,b FROM t ORDER BY a`, []string{"1,2", "2,2"}},
		{"OR REPLACE collapses onto one row",
			[]string{`CREATE TABLE t(a,b UNIQUE)`, `INSERT INTO t VALUES(1,10),(2,20),(3,30)`},
			`UPDATE OR REPLACE t SET b=(SELECT count(*) FROM t)`,
			`SELECT a,b FROM t ORDER BY rowid`, []string{"3,3"}},
		{"empty subquery is NULL",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,10),(2,20)`},
			`UPDATE t SET b=(SELECT b FROM t WHERE a=99)`,
			`SELECT a,quote(b) FROM t ORDER BY a`, []string{"1,NULL", "2,NULL"}},
	}
	for _, tc := range cases {
		db, err := Create(t.TempDir()+"/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", tc.name, s, err)
			}
		}
		if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
			t.Errorf("[%s] compile: %v", tc.name, cerr)
			db.Discard()
			continue
		}
		if err := db.Exec(tc.stmt); err != nil {
			t.Errorf("[%s] exec: %v", tc.name, err)
			db.Discard()
			continue
		}
		p, perr := db.SnapshotPager()
		if perr != nil {
			t.Fatal(perr)
		}
		_, rows, qerr := p.Query(tc.query)
		if qerr != nil {
			t.Fatalf("[%s] read back: %v", tc.name, qerr)
		}
		var got []string
		for _, r := range rows {
			var cells []string
			for _, v := range r {
				cells = append(cells, valueDebugString(v))
			}
			got = append(got, strings.Join(cells, ","))
		}
		if strings.Join(got, " | ") != strings.Join(tc.want, " | ") {
			t.Errorf("[%s] %q\n  got  %v\n  want %v (the 3.53.3 oracle's answer)", tc.name, tc.stmt, got, tc.want)
		}
		db.Discard()
	}
}

// valueDebugString renders a Value for test output.
func valueDebugString(v Value) string {
	switch v.Typ {
	case Null:
		return "NULL"
	case Int:
		return strconv.FormatInt(v.I, 10)
	case Float:
		return strconv.FormatFloat(v.F, 'g', -1, 64)
	default:
		return string(v.S)
	}
}
