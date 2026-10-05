package engine

// Tests engine-side UPDATE ... FROM compilation to bytecode, verifying that
// it emits correct two-pass ephemeral table logic and produces correct answers.

import (
	"errors"
	"strings"
	"testing"
)

// updateFromShapes: statements whose promotion this batch is about.
var updateFromShapes = []conflictShapeCase{
	// The census shape, verbatim from vdbe_total_test.go's list.
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`},

	// No WHERE at all: a bare cross join.
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(nv)`}, `UPDATE t SET v=m.nv FROM m`},

	// Several SET targets, and a SET whose right-hand side mixes both sides.
	{[]string{`CREATE TABLE t(k,v,w)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv, w=t.k||m.nv FROM m WHERE m.k=t.k`},

	// An "AS alias" on the target -- the synthetic SELECT's rowid reference has
	// to be qualified by whichever name is in scope (buildUpdateFromSelect).
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t AS o SET v=m.nv FROM m WHERE m.k=o.k`},

	// A leading WITH clause naming a CTE as a FROM source (upfrom2.test 1.x.1).
	{[]string{`CREATE TABLE t1(x,z)`},
		`WITH data(k,v) AS (VALUES(3,'thirty'),(1,'ten')) UPDATE t1 SET z=v FROM data WHERE x=k`},

	// An outer join INSIDE the FROM clause -- the target is comma-joined after
	// it, so it can never be null-extended (upfrom4.test 120/210).
	{[]string{`CREATE TABLE t5(a,b,c)`, `CREATE TABLE m1(x,y)`, `CREATE TABLE m2(u,v)`},
		`UPDATE t5 SET b=y, c=v FROM m1 LEFT JOIN m2 ON (u=x) WHERE x=a`},

	// A derived table as the FROM source.
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=s.nv FROM (SELECT k,nv FROM m WHERE k=1) AS s WHERE s.k=t.k`},

	// An INTEGER PRIMARY KEY target, and one whose SET MOVES the rowid.
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,v)`, `CREATE TABLE m(k,nk)`},
		`UPDATE t SET k=m.nk FROM m WHERE m.k=t.k`},

	// A WITHOUT ROWID target: the leading key columns are its PRIMARY KEY tuple
	// (upfrom3.test's own shape).
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY,v) WITHOUT ROWID`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`},

	// Constraint machinery on the second pass, which is shared with the
	// single-table path (update.c:1031 sits outside every nChangeFrom test).
	{[]string{`CREATE TABLE t(k, v NOT NULL)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`},
	{[]string{`CREATE TABLE t(k INT, v TEXT) STRICT`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`},
	{[]string{`CREATE TABLE t(a, g AS (a*2), v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET a=m.nv FROM m WHERE m.k=t.a`},
	{[]string{`CREATE TABLE t(a, v CHECK(v<100))`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.a`},

	// An explicit OR-clause (update.c:984 hands onError to the trigger coder
	// without ever consulting nChangeFrom).
	{[]string{`CREATE TABLE t(a UNIQUE, v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE OR REPLACE t SET v=m.nv FROM m WHERE m.k=t.a`},
	{[]string{`CREATE TABLE t(a UNIQUE, v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE OR IGNORE t SET v=m.nv FROM m WHERE m.k=t.a`},

	// BEFORE and AFTER UPDATE triggers on the target.
	{[]string{`CREATE TABLE t1(x,z)`, `CREATE TABLE log(s)`, `CREATE TABLE d(k,v)`,
		`CREATE TRIGGER trb BEFORE UPDATE ON t1 BEGIN INSERT INTO log VALUES(old.z); END`,
		`CREATE TRIGGER tra AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES(new.z); END`},
		`UPDATE t1 SET z=v FROM d WHERE x=k`},

	// A schema-qualified target.
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE main.t SET v=m.nv FROM m WHERE m.k=t.k`},

	// A bare aggregate SET expression (upfrom1.test 3.1, an OSS-Fuzz case):
	// the whole synthetic join collapses to one row.
	{[]string{`CREATE TABLE t0(a)`, `CREATE TABLE t1(b)`}, `UPDATE t1 SET b=sum(a) FROM t0`},

	// A SET whose right-hand side carries its own scalar subquery -- pass one
	// reads the untouched table by construction, so this is NOT the
	// "UPDATE SET subquery over the target table" decline.
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=(SELECT count(*) FROM t) FROM m WHERE m.k=t.k`},

	// RETURNING. It was in updateFromDeclined below, on the reasoning that
	// "updateFromExec declines this too, as a hard error" -- which described
	// the INTERPRETER's gap, not this emitter's, and cost every one of these
	// statements an error where the 3.53.3 oracle answers. It needed nothing
	// built: update.c codes RETURNING at :1119's TRIGGER_AFTER call, which sits
	// in pass TWO, outside every nChangeFrom test -- the same loop, the same
	// position, as the single-table path. Measured against the oracle: "UPDATE
	// t SET v=m.nv FROM m WHERE m.k=t.k RETURNING t.k" and the four below now
	// agree row for row where the shipped tree raised "UPDATE ... FROM ...
	// RETURNING is not supported by this write path".
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k RETURNING v`},
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k RETURNING *`},
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k RETURNING k, v, rowid`},
	// An IPK target, and one whose SET MOVES the rowid: RETURNING reports the
	// DECIDED rowid, which is emitReturning's own ipkIndex remap.
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k RETURNING k, v`},
	// RETURNING beside the target's own AFTER trigger -- both are entries in
	// one AFTER list in C (trigger.c:68-78), and this is the FROM spelling of
	// the crossing compileUpdateStmt's own guard now allows.
	{[]string{`CREATE TABLE t1(x,z)`, `CREATE TABLE log(s)`, `CREATE TABLE d(k,v)`,
		`CREATE TRIGGER tra2 AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES(new.z); END`},
		`UPDATE t1 SET z=v FROM d WHERE x=k RETURNING x, z`},
	// A RETURNING subquery over a table this statement is not writing -- C's
	// ONCE lifetime, which this batch implemented (emitReturning). A FROM
	// clause is the one context where that shape is easy to reach by accident,
	// so it is pinned here as well as in returning_subquery_codegen_test.go.
	{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
		`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k RETURNING k, (SELECT count(*) FROM m)`},

	// A trigger BODY's UPDATE ... FROM is promoted too, and is deliberately NOT
	// in this table: every assertion below is about a TOP-LEVEL statement's own
	// program (its SRT_Upfrom source sits in the top-level Insns, and its
	// WritePager must be non-nil) and a body inverts both -- the source lives
	// in the body sub-program, and the firing program must carry NO frozen
	// pager at all, which is exactly what makes the body's pass one live. It is
	// pinned in TestUpdateFromTriggerBodyIsLive
	// (view_update_from_codegen_test.go) instead, which asserts both halves.
}

// updateFromDeclined: UPDATE ... FROM shapes this compiler must still refuse.
// A promotion that swallowed one of these would be a wrong answer, not a wider
// capability.
var updateFromDeclined = []conflictShapeCase{
	// RETURNING used to head this list. It is in updateFromShapes now -- see
	// the note there for why its stated reason described a gap somewhere else
	// rather than this emitter's.

	// A trigger BODY's UPDATE ... FROM whose join resolves NEW./OLD.
	// (triggerupfrom.test 1.0) was the last entry here, and it is SERVED now:
	// the live seam carries the trigger context through both lowerings
	// (Program.LiveTrig + trigOnlyOuter, vdbe_live_read.go), so "new.a"
	// resolves to an OpParam read of the firing row instead of to nothing.
	// Verified against the 3.53.3 oracle over two firings before it moved --
	// the mapped update lands on the row the firing row names, on both engines.

	// A VIEW target with a FROM clause used to sit here. It is a different
	// EMITTER, which is what update.c's own "nChangeFrom==0 && isView"
	// materialize guard (:629-630) says -- but a different emitter is something
	// to write, not something to decline. See vdbe_view_update_from.go and
	// view_update_from_codegen_test.go.
}

// TestUpdateFromCompilesToBytecode is the RULE #1 assertion the differential
// harness cannot make: every shape must LOWER, trigger bodies included. Every
// behavioural gate goes on passing whether or not it does, which is exactly why
// this assertion exists.
func TestUpdateFromCompilesToBytecode(t *testing.T) {
	for i, tc := range updateFromShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// TestUpdateFromEmitsTheEphemeralTwoPassLoop is the non-vacuity half, and the
// assertion that separates a real port of update.c from a single-table scan
// with the join bolted on.
//
// update.c's UPDATE ... FROM is TWO PASSES: pass one runs the join into an
// ephemeral table keyed on the target row (update.c:693-696 through
// select.c:1366-1373), and pass two walks THAT with OP_Rewind/OP_Next, seeking
// the real table per entry off the key it just read (update.c:849-864). An
// emitter that instead scanned the target table would answer differently for
// every multi-row join, and would still pass the test above. So this pins the
// structure: an OpOpenDerived whose derivedSource carries an upfrom
// destination, an OpRewind and OpNext over THAT cursor, and a key-register
// OpNotExists (P5 set) on the target cursor between them.
func TestUpdateFromEmitsTheEphemeralTwoPassLoop(t *testing.T) {
	for i, tc := range updateFromShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		ephCur, openAt := -1, -1
		for j := range prog.Insns {
			if prog.Insns[j].Op != OpOpenDerived {
				continue
			}
			ds, ok := prog.Insns[j].P4.(*derivedSource)
			if !ok || ds == nil || ds.upfrom == nil {
				continue
			}
			if ds.prog == nil {
				t.Errorf("[%d] %q: upfrom source carries no compiled pass-one program", i, tc.stmt)
			}
			if ds.pager == nil {
				t.Errorf("[%d] %q: upfrom source carries no frozen snapshot pager", i, tc.stmt)
			}
			ephCur, openAt = prog.Insns[j].P1, j
			break
		}
		if ephCur < 0 {
			t.Errorf("RULE #1 (vacuous): [%d] %q emitted no SRT_Upfrom ephemeral source. It may be\n"+
				"compiling, but not as update.c's two-pass shape.", i, tc.stmt)
			continue
		}
		var rewind, next, seek, updateRow bool
		for j := range prog.Insns {
			in := &prog.Insns[j]
			switch in.Op {
			case OpRewind:
				rewind = rewind || (in.P1 == ephCur && j > openAt)
			case OpNext:
				// P2 must land back INSIDE the loop, i.e. after the open.
				next = next || (in.P1 == ephCur && in.P2 > openAt && in.P2 < j)
			case OpNotExists:
				// The key-register form: P5 set, P1 the TARGET cursor (0).
				seek = seek || (in.P5 != 0 && in.P1 == 0 && j > openAt)
			case OpUpdateRow:
				updateRow = updateRow || in.P1 == 0
			}
		}
		if !rewind || !next {
			t.Errorf("[%d] %q: the ephemeral cursor is opened but not SCANNED (rewind=%v next=%v)",
				i, tc.stmt, rewind, next)
		}
		if !seek {
			t.Errorf("[%d] %q: no key-register OpNotExists on the target cursor -- update.c:862-864's\n"+
				"per-entry re-seek is what makes a row a cascade deleted drop out of the loop", i, tc.stmt)
		}
		if !updateRow {
			t.Errorf("[%d] %q: the loop stores nothing (no OpUpdateRow on the target cursor)", i, tc.stmt)
		}
	}
}

// TestUpdateFromReadsSetValuesFromTheEphemeralTable pins update.c:949-955: with
// nChangeFrom set, SET expression j is NOT coded in the update loop -- it is
// column j of the ephemeral table, already evaluated in the join's scope. An
// emitter that re-coded the expressions here would resolve them against the
// target row alone and fail (or, worse, silently bind a same-named column of
// the target), so this asserts one OpColumn per SET target on the eph cursor.
func TestUpdateFromReadsSetValuesFromTheEphemeralTable(t *testing.T) {
	// One SET target, then two, so the assertion is a count and not a presence.
	for _, tc := range []struct {
		c     conflictShapeCase
		nSets int
	}{
		{conflictShapeCase{[]string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`},
			`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`}, 1},
		{conflictShapeCase{[]string{`CREATE TABLE t(k,v,w)`, `CREATE TABLE m(k,nv)`},
			`UPDATE t SET v=m.nv, w=t.k||m.nv FROM m WHERE m.k=t.k`}, 2},
	} {
		prog, err := compileShape(t, tc.c)
		if err != nil {
			t.Errorf("%q: compile: %v", tc.c.stmt, err)
			continue
		}
		ephCur := -1
		for j := range prog.Insns {
			if ds, ok := prog.Insns[j].P4.(*derivedSource); ok && ds != nil && ds.upfrom != nil {
				ephCur = prog.Insns[j].P1
				break
			}
		}
		if ephCur < 0 {
			t.Errorf("%q: no upfrom source", tc.c.stmt)
			continue
		}
		want := map[int]bool{}
		for j := 0; j < tc.nSets; j++ {
			want[j] = false
		}
		for j := range prog.Insns {
			in := &prog.Insns[j]
			if in.Op == OpColumn && in.P1 == ephCur {
				want[in.P2] = true
			}
		}
		for col, seen := range want {
			if !seen {
				t.Errorf("%q: SET expression %d is not read out of the ephemeral table --\n"+
					"update.c:952's \"OP_Column iEph, nOff+j\" is what evaluates it in the JOIN's scope",
					tc.c.stmt, col)
			}
		}
	}
}

// TestUpdateFromDeclinedShapesDeclineCleanly is the other half of never-wrong:
// what this emitter does not model must decline, not be half-served.
func TestUpdateFromDeclinedShapesDeclineCleanly(t *testing.T) {
	for i, tc := range updateFromDeclined {
		prog, err := compileShape(t, tc)
		if err != nil {
			// A clean decline is the correct outcome; what must NOT happen is
			// this emitter half-serving a shape it does not model.
			if !errors.Is(err, errVDBEUnsupported) {
				t.Errorf("[%d] %q declined with %v, which is not errVDBEUnsupported.", i, tc.stmt, err)
			}
			continue
		}
		if prog != nil {
			t.Errorf("NEVER-WRONG REGRESSION: [%d] %q now COMPILES. Every one of these shapes needs\n"+
				"something this emitter does not model; compiling it answers a question that has\n"+
				"not been proved. See updateFromDeclined's own comments.", i, tc.stmt)
		}
	}
}

// TestUpdateFromProgramIsNotCacheable pins the one thing that made "INSERT INTO
// ev SELECT y FROM ev" wrong when compileInsertSelectWrite first grew the same
// seam: pass one reads a FROZEN snapshot, so reusing the program for a later
// execution would answer the second statement from the first one's image.
// Program.WritePager is what cachedWriteProgram checks.
func TestUpdateFromProgramIsNotCacheable(t *testing.T) {
	for i, tc := range updateFromShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		if prog.WritePager == nil {
			t.Errorf("[%d] %q: WritePager is nil, so cachedWriteProgram would CACHE a program whose\n"+
				"pass one holds a frozen snapshot -- every later run of the same SQL would re-read\n"+
				"this run's image.", i, tc.stmt)
		}
	}
}

// TestUpdateFromCompiledAnswers is the behavioural half. Every case here is
// answered correctly by updateFromExec too, so none of it proves the
// promotion -- it proves the promotion did not BREAK anything, which the
// assertions above cannot.
func TestUpdateFromCompiledAnswers(t *testing.T) {
	type ans struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
		// wantErr, when non-empty, is a substring the statement's error must
		// contain (and the statement must fail).
		wantErr string
	}
	cases := []ans{
		{"plain", []string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`,
			`INSERT INTO t VALUES(1,'a'),(2,'b'),(3,'c')`, `INSERT INTO m VALUES(1,'X'),(3,'Z')`},
			`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`, `SELECT k,v FROM t ORDER BY k`,
			[]string{"1,X", "2,b", "3,Z"}, ""},

		// An unmatched target row is LEFT ALONE (inner-join semantics), which a
		// "scan the target and code the SET" emitter would get wrong.
		{"unmatched-left-alone", []string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`,
			`INSERT INTO t VALUES(1,'a'),(2,'b')`, `INSERT INTO m VALUES(9,'X')`},
			`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`, `SELECT k,v FROM t ORDER BY k`,
			[]string{"1,a", "2,b"}, ""},

		// Triggers fire in ASCENDING TARGET ROWID order, not the join's order --
		// upfrom2.test 1.1's own setup, verified against 3.53.3 in
		// update_from.go's doc comment. The data CTE lists 3 before 1.
		{"trigger-order", []string{`CREATE TABLE t1(x,z)`, `CREATE TABLE log(s)`,
			`INSERT INTO t1 VALUES(1,'one'),(2,'i'),(3,'three'),(4,'iii')`,
			`CREATE TRIGGER tr AFTER UPDATE ON t1 BEGIN INSERT INTO log VALUES(old.z||'->'||new.z); END`},
			`WITH data(k,v) AS (VALUES(3,'thirty'),(1,'ten')) UPDATE t1 SET z=v FROM data WHERE x=k`,
			`SELECT s FROM log`, []string{"one->ten", "three->thirty"}, ""},

		// The ambiguous multi-match DECLINE, and the identical-tuple case that
		// is NOT ambiguous (setValueTuplesEqual).
		{"multimatch-differs", []string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`,
			`INSERT INTO t VALUES(1,'a')`, `INSERT INTO m VALUES(1,'X'),(1,'Y')`},
			`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`, `SELECT k,v FROM t`,
			[]string{"1,a"}, "matched more than one FROM row"},
		{"multimatch-identical", []string{`CREATE TABLE t(k,v)`, `CREATE TABLE m(k,nv)`,
			`INSERT INTO t VALUES(1,'a')`, `INSERT INTO m VALUES(1,'X'),(1,'X')`},
			`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`, `SELECT k,v FROM t`,
			[]string{"1,X"}, ""},

		// A bare aggregate SET collapses the WHOLE join to one row: exactly one
		// target row is updated, at sum over the cross join (6*3), never 6
		// broadcast to each row (upfrom1.test 3.1).
		{"aggregate-set", []string{`CREATE TABLE t0(a)`, `CREATE TABLE t1(b)`,
			`INSERT INTO t0 VALUES(1),(2),(3)`, `INSERT INTO t1 VALUES(0),(0),(0)`},
			`UPDATE t1 SET b=sum(a) FROM t0`, `SELECT b FROM t1 ORDER BY rowid`,
			[]string{"18", "0", "0"}, ""},
		// ...and a zero-match aggregate join is a NO-OP, not an error
		// (select.c:1366's OP_IsNull).
		{"aggregate-empty", []string{`CREATE TABLE t0(a)`, `CREATE TABLE t1(b)`,
			`INSERT INTO t1 VALUES(7)`},
			`UPDATE t1 SET b=sum(a) FROM t0`, `SELECT b FROM t1`, []string{"7"}, ""},

		// A SET that MOVES the rowid.
		{"set-ipk", []string{`CREATE TABLE t(k INTEGER PRIMARY KEY,v)`, `CREATE TABLE m(k,nk)`,
			`INSERT INTO t VALUES(1,'a'),(2,'b')`, `INSERT INTO m VALUES(1,9)`},
			`UPDATE t SET k=m.nk FROM m WHERE m.k=t.k`, `SELECT k,v FROM t ORDER BY k`,
			[]string{"2,b", "9,a"}, ""},

		// A WITHOUT ROWID target (upfrom3.test's shape).
		{"without-rowid", []string{`CREATE TABLE t(k TEXT PRIMARY KEY,v) WITHOUT ROWID`,
			`CREATE TABLE m(k,nv)`, `INSERT INTO t VALUES('a','1'),('b','2')`,
			`INSERT INTO m VALUES('a','X')`},
			`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`, `SELECT k,v FROM t ORDER BY k`,
			[]string{"a,X", "b,2"}, ""},

		// An outer join inside FROM: m1's unmatched row still updates its target
		// row, with the m2 column NULL (upfrom4.test 120/210).
		{"leftjoin", []string{`CREATE TABLE t5(a,b,c)`, `CREATE TABLE m1(x,y)`, `CREATE TABLE m2(u,v)`,
			`INSERT INTO t5 VALUES(1,0,0),(2,0,0)`, `INSERT INTO m1 VALUES(1,'y1'),(2,'y2')`,
			`INSERT INTO m2 VALUES(1,'v1')`},
			`UPDATE t5 SET b=y, c=v FROM m1 LEFT JOIN m2 ON (u=x) WHERE x=a`,
			`SELECT a,b,c FROM t5 ORDER BY a`, []string{"1,y1,v1", "2,y2,<NULL>"}, ""},

		// Constraints on the second pass, shared with the single-table path.
		{"notnull", []string{`CREATE TABLE t(k, v NOT NULL)`, `CREATE TABLE m(k,nv)`,
			`INSERT INTO t VALUES(1,'a'),(2,'b')`, `INSERT INTO m VALUES(1,NULL),(2,'Y')`},
			`UPDATE t SET v=m.nv FROM m WHERE m.k=t.k`, `SELECT k,v FROM t ORDER BY k`,
			[]string{"1,a", "2,b"}, "NOT NULL constraint failed"},
		{"generated", []string{`CREATE TABLE t(a, g AS (a*2), v)`, `CREATE TABLE m(k,nv)`,
			`INSERT INTO t(a,v) VALUES(1,'x')`, `INSERT INTO m VALUES(1,5)`},
			`UPDATE t SET a=m.nv FROM m WHERE m.k=t.a`, `SELECT a,g,v FROM t`,
			[]string{"5,10,x"}, ""},
		{"unique", []string{`CREATE TABLE t(a UNIQUE, v)`, `CREATE TABLE m(k,nv)`,
			`INSERT INTO t VALUES(1,'x'),(2,'y')`, `INSERT INTO m VALUES(1,2)`},
			`UPDATE t SET a=m.nv FROM m WHERE m.k=t.a`, `SELECT a,v FROM t ORDER BY a`,
			[]string{"1,x", "2,y"}, "UNIQUE constraint failed"},
	}
	for _, tc := range cases {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("[%s] create: %v", tc.name, err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", tc.name, s, err)
			}
		}
		// The statement must actually COMPILE, or this whole function proves
		// nothing about the emitter.
		if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
			t.Errorf("[%s] compile: %v", tc.name, cerr)
		}
		eerr := db.Exec(tc.stmt)
		switch {
		case tc.wantErr == "" && eerr != nil:
			t.Errorf("[%s] exec: %v", tc.name, eerr)
		case tc.wantErr != "" && eerr == nil:
			t.Errorf("[%s] expected an error containing %q, got success", tc.name, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(eerr.Error(), tc.wantErr):
			t.Errorf("[%s] error %q does not contain %q", tc.name, eerr, tc.wantErr)
		}
		got := rvdRowStrings(rvdQuery(t, db, tc.query))
		if !equalStrSlices(got, tc.want) {
			t.Errorf("[%s] rows = %v, want %v", tc.name, got, tc.want)
		}
		db.Discard()
	}
}
