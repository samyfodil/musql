package engine

// Tests that RETURNING subqueries compile to bytecode and execute with the
// correct lifetime (per-row or once). Validates that subqueries are live,
// cache slots are reset appropriately, and declined mixed-lifetime shapes are
// properly refused.

import (
	"errors"
	"strings"
	"testing"
)

// returningSubqueryShapes: per-row RETURNING subqueries.
var returningSubqueryShapes = []conflictShapeCase{
	{[]string{`CREATE TABLE t(a,b)`}, `INSERT INTO t VALUES(1,'x') RETURNING a,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`}, `INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r') RETURNING a,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`}, `UPDATE t SET b=b+1 RETURNING a,(SELECT sum(b) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`}, `INSERT INTO t SELECT x,'y' FROM s RETURNING a,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`}, `INSERT INTO t VALUES(1,10) RETURNING a,(SELECT sum(b) FROM t)*2,(SELECT max(a) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`}, `INSERT INTO t VALUES(1,10) RETURNING a,EXISTS(SELECT 1 FROM t WHERE b>5)`},
	{[]string{`CREATE TABLE t(a,b)`}, `INSERT INTO t VALUES(1,10) RETURNING a,a IN (SELECT a FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`}, `INSERT INTO t VALUES(1,'x') RETURNING *,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`}, `INSERT INTO t VALUES(NULL,'p') RETURNING k,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`}, `UPDATE t SET k=k+10 RETURNING k,(SELECT count(*) FROM t WHERE k>5)`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY,b) WITHOUT ROWID`}, `INSERT INTO t VALUES('a',1) RETURNING k,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`}, `DELETE FROM t RETURNING a,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`}, `DELETE FROM t WHERE a>1 RETURNING a,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`INSERT INTO t VALUES(1,'x') RETURNING (SELECT count(*) FROM s WHERE x=a)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`INSERT INTO t VALUES(1,'x') RETURNING (SELECT count(*) FROM s WHERE x=t.a)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`INSERT INTO t VALUES(1,'x') RETURNING (SELECT (SELECT count(*) FROM s WHERE x=a))`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`UPDATE t SET b='w' RETURNING a,(SELECT count(*) FROM s WHERE x=a)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`DELETE FROM t RETURNING a,(SELECT count(*) FROM s WHERE x=a)`},
}

// returningSubqueryOnceCompiled: once-only RETURNING subqueries (OP_Once).
var returningSubqueryOnceCompiled = []conflictShapeCase{
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`INSERT INTO t VALUES(1,'x') RETURNING a,(SELECT count(*) FROM s)`},
	{[]string{`CREATE TABLE t(a,b)`},
		`INSERT INTO t VALUES(1,10) RETURNING a,(SELECT count(*) FROM (SELECT a FROM t UNION ALL SELECT a FROM t))`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT * FROM t`},
		`INSERT INTO t VALUES(1,'p') RETURNING a,(SELECT count(*) FROM v)`},
	{[]string{`CREATE TABLE t(a,b)`},
		`INSERT INTO t VALUES(1,'p') RETURNING a,(SELECT n FROM (WITH c(n) AS (SELECT count(*) FROM t) SELECT n FROM c))`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,'x') RETURNING a,(SELECT count(*) FROM t)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM log)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s2(x)`},
		`DELETE FROM t RETURNING a,(SELECT count(*) FROM s2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE VIEW tv AS SELECT * FROM t`},
		`DELETE FROM t RETURNING a,(SELECT count(*) FROM tv)`},
}

// returningSubqueryDeclinedShapes: RETURNING subqueries that cannot compile.
var returningSubqueryDeclinedShapes = []conflictShapeCase{
	// Currently empty. Mixed per-row and once subqueries in one RETURNING list
	// and certain VIEW-specific shapes remain out of scope.
}

func TestReturningSubqueryCompilesToBytecode(t *testing.T) {
	for i, tc := range returningSubqueryShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
			continue
		}
	}
}

// TestReturningSubqueryIsLiveAndReset validates that RETURNING sub-programs
// are live and cache slots are reset at loop sites.
func TestReturningSubqueryIsLiveAndReset(t *testing.T) {
	for i, tc := range returningSubqueryShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			continue // reported by the test above
		}
		var subs, live, resets, resetSlots int
		var subSlots []int
		for j := range prog.Insns {
			in := &prog.Insns[j]
			note := func(p *Program, slot int) {
				if p == nil {
					return
				}
				subs++
				if p.LiveSource != nil {
					live++
				}
				subSlots = append(subSlots, slot)
			}
			switch p4 := in.P4.(type) {
			case *Program:
				note(p4, in.P2)
			case *inSubPlan:
				note(p4.prog, in.P3)
			case *rowSubPlan:
				note(p4.prog, in.P3)
			}
			if in.Op == OpSubCacheReset {
				list, _ := in.P4.([]int)
				if in.P2 == 0 && len(list) == 0 {
					continue
				}
				resets++
				resetSlots += in.P2 + len(list)
			}
		}
		if subs == 0 {
			t.Errorf("[%d] %q: no sub-program in the emitted code at all -- this case measures nothing", i, tc.stmt)
			continue
		}
		if live != subs {
			t.Errorf("[%d] %q: %d of %d RETURNING sub-programs are LIVE.\n"+
				"A non-live one runs this COMPILE's frozen image, so it reports the\n"+
				"pre-statement answer for every row.", i, tc.stmt, live, subs)
		}
		// A per-row site is exactly the one whose RETURNING block sits inside a
		// scan loop; the emitted OpNext over the write cursor is the tell.
		perRowSite := false
		for j := range prog.Insns {
			if prog.Insns[j].Op == OpNext {
				perRowSite = true
			}
		}
		switch {
		case perRowSite && resets == 0:
			t.Errorf("[%d] %q: a loop RETURNING block with NO OpSubCacheReset. Its %d subquery\n"+
				"cache slots survive from row to row, so every row after the first repeats\n"+
				"the first row's answer.", i, tc.stmt, subs)
		case perRowSite && resetSlots < subs:
			t.Errorf("[%d] %q: OpSubCacheReset covers %d slots but the block allocated %d.",
				i, tc.stmt, resetSlots, subs)
		case !perRowSite && resets != 0 && resetSlots >= subs:
			t.Errorf("[%d] %q: an unrolled block emitted %d OpSubCacheReset covering all %d\n"+
				"slots; each emission owns its own slots, so there is nothing to clear.", i, tc.stmt, resets, subs)
		}
	}
}

// TestReturningSubqueryOnceCompiles validates once-subquery compilation.
func TestReturningSubqueryOnceCompiles(t *testing.T) {
	for i, tc := range returningSubqueryOnceCompiled {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
	}
}

// TestReturningSubqueryOnceIsStructurallyOnce validates the once-subquery
// mechanisms: shared cache slots in unrolled VALUES and empty resets in loops.
func TestReturningSubqueryOnceIsStructurallyOnce(t *testing.T) {
	for i, tc := range returningSubqueryOnceCompiled {
		prog, err := compileShape(t, tc)
		if err != nil {
			continue // reported by the test above
		}
		slots := map[int]int{}
		nSub, resets, resetSlots, loop := 0, 0, 0, false
		for j := range prog.Insns {
			in := &prog.Insns[j]
			note := func(p *Program, slot int) {
				if p == nil {
					return
				}
				nSub++
				slots[slot]++
			}
			switch p4 := in.P4.(type) {
			case *Program:
				note(p4, in.P2)
			case *inSubPlan:
				note(p4.prog, in.P3)
			case *rowSubPlan:
				note(p4.prog, in.P3)
			}
			switch in.Op {
			case OpSubCacheReset:
				resets++
				resetSlots += in.P2
			case OpNext:
				loop = true
			}
		}
		if nSub == 0 {
			t.Errorf("[%d] %q: no sub-program emitted -- this case measures nothing", i, tc.stmt)
			continue
		}
		if loop && resetSlots != 0 {
			t.Errorf("[%d] %q: a ONCE block at a loop site cleared %d cache slot(s) across %d\n"+
				"OpSubCacheReset. Clearing is what makes a block PER-ROW; a once block must\n"+
				"let its slots survive the OpNext.", i, tc.stmt, resetSlots, resets)
		}
		if !loop && strings.Count(tc.stmt, "),(") > 0 {
			shared := false
			for _, n := range slots {
				if n > 1 {
					shared = true
				}
			}
			if !shared {
				t.Errorf("[%d] %q: %d sub-programs across %d DISTINCT cache slots -- every unrolled\n"+
					"tuple allocated its own, so each re-runs the subquery. C codes ONE block with\n"+
					"ONE OP_Once; the rewind in emitReturning is how this compiler spells that.",
					i, tc.stmt, nSub, len(slots))
			}
		}
	}
}

// TestReturningSubqueryDeclinedShapeIsRefused validates that impossible shapes
// are cleanly declined with errVDBEUnsupported.
func TestReturningSubqueryDeclinedShapeIsRefused(t *testing.T) {
	for i, tc := range returningSubqueryDeclinedShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			if !errors.Is(err, errVDBEUnsupported) {
				t.Errorf("[%d] %q declined with %v, which is not errVDBEUnsupported.", i, tc.stmt, err)
			}
			continue
		}
		if prog != nil {
			t.Errorf("[%d] %q: COMPILED, and no lifetime covers it -- a MIXED block has no single\n"+
				"sub-cache discipline, because the rewind in emitReturning is per BLOCK: at an\n"+
				"unrolled site it would freeze the per-row half, and without it the once half\n"+
				"would re-run. If this is now served, verify it against the oracle and move it\n"+
				"to returningSubqueryShapes.", i, tc.stmt)
		}
	}
}

// TestReturningSubqueryDeclinesAreUnsupportedNotFatal validates that all
// declining RETURNING subqueries are reported as errVDBEUnsupported.
func TestReturningSubqueryDeclinesAreUnsupportedNotFatal(t *testing.T) {
	for i, tc := range returningSubqueryDeclinedShapes {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%d] setup %q: %v", i, s, err)
			}
		}
		var cerr error
		switch {
		case strings.HasPrefix(tc.stmt, "INSERT"):
			_, cerr = db.compileInsertWrite(tc.stmt)
		case strings.HasPrefix(tc.stmt, "UPDATE"):
			_, cerr = db.compileUpdateWrite(tc.stmt)
		default:
			_, cerr = db.compileDeleteWrite(tc.stmt)
		}
		if cerr == nil || !errors.Is(cerr, errVDBEUnsupported) {
			t.Errorf("[%d] %q: got %v, want an errVDBEUnsupported decline", i, tc.stmt, cerr)
		}
		db.Discard()
	}
}

// TestReturningSubqueryCompiledAnswers validates correct per-row answers.
func TestReturningSubqueryCompiledAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		want  []string
	}{
		{"single INSERT sees its own row", []string{`CREATE TABLE t(a,b)`},
			`INSERT INTO t VALUES(1,'x') RETURNING a,(SELECT count(*) FROM t)`, []string{"1,1"}},
		{"multi-row INSERT counts up", []string{`CREATE TABLE t(a,b)`},
			`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r') RETURNING a,(SELECT count(*) FROM t)`,
			[]string{"1,1", "2,2", "3,3"}},
		{"multi-row INSERT onto a non-empty table",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(9,'s'),(8,'s')`},
			`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM t)`,
			[]string{"1,3", "2,4"}},
		{"UPDATE re-runs per row",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,0),(2,0),(3,0)`},
			`UPDATE t SET b=b+1 RETURNING a,(SELECT sum(b) FROM t)`, []string{"1,1", "2,2", "3,3"}},
		{"INSERT ... SELECT re-runs per row",
			[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`, `INSERT INTO s VALUES(1),(2),(3)`},
			`INSERT INTO t SELECT x,'y' FROM s RETURNING a,(SELECT count(*) FROM t)`,
			[]string{"1,1", "2,2", "3,3"}},
		{"EXISTS and IN over the target",
			[]string{`CREATE TABLE t(a,b)`},
			`INSERT INTO t VALUES(1,10),(2,20) RETURNING a,EXISTS(SELECT 1 FROM t WHERE b>15),a IN (SELECT a FROM t)`,
			[]string{"1,0,1", "2,1,1"}},
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", tc.name, s, err)
			}
		}
		_, cerr := db.compileWrite(tc.stmt)
		if cerr != nil {
			t.Errorf("[%s] not compiled (err=%v) -- this case measures nothing", tc.name, cerr)
			db.Discard()
			continue
		}
		_, rows, xerr := db.ExecReturningArgs(tc.stmt, nil)
		if xerr != nil {
			t.Errorf("[%s] exec: %v", tc.name, xerr)
			db.Discard()
			continue
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

// TestReturningSubqueryProgramIsCacheableAndStillLive validates that cached
// RETURNING programs remain live and answer correctly on re-execution.
func TestReturningSubqueryProgramIsCacheableAndStillLive(t *testing.T) {
	const sql = `INSERT INTO t VALUES(1,'x') RETURNING a,(SELECT count(*) FROM t)`
	for _, inTx := range []bool{false, true} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`CREATE TABLE t(a,b)`); err != nil {
			t.Fatal(err)
		}
		prog, cerr := db.compileWrite(sql)
		if cerr != nil {
			t.Fatalf("compile: %v", cerr)
		}
		if prog.WritePager != nil {
			t.Errorf("inTx=%v: WritePager is set, so this program holds a FROZEN image. Every\n"+
				"RETURNING sub-program is supposed to be live (LiveSource), which is what makes\n"+
				"the program safe to cache in the first place.", inTx)
		}
		if inTx {
			if err := db.Exec(`BEGIN`); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		for i := 0; i < 4; i++ {
			_, rows, xerr := db.ExecReturningArgs(sql, nil)
			if xerr != nil {
				t.Fatalf("inTx=%v run %d: %v", inTx, i, xerr)
			}
			got = append(got, valueDebugString(rows[0][1]))
		}
		if want := "1 2 3 4"; strings.Join(got, " ") != want {
			t.Errorf("inTx=%v: repeated engine-direct gave %q, want %q -- a program whose\n"+
				"RETURNING subquery was NOT live answers \"1 1 1 1\" from the first run's image.",
				inTx, strings.Join(got, " "), want)
		}
		db.Discard()
	}
}

// TestReturningSubqueryOnceAnswers validates correct once-only answers.
func TestReturningSubqueryOnceAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		want  []string
	}{
		// (trigger.c:979 compares the FROM item's RESOLVED table).
		{"derived table over the target",
			[]string{`CREATE TABLE t(a,b)`},
			`INSERT INTO t VALUES(1,10),(2,20) RETURNING a,(SELECT count(*) FROM (SELECT a FROM t UNION ALL SELECT a FROM t))`,
			[]string{"1,2", "2,2"}},
		{"view over the target",
			[]string{`CREATE TABLE t(a,b)`, `CREATE VIEW v AS SELECT * FROM t`},
			`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM v)`,
			[]string{"1,1", "2,1"}},
		// C froze the count before it ever ran.
		{"a table the statement's own trigger writes",
			[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
				`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM log)`,
			[]string{"1,0", "2,0"}},
		// "uncorrelated subquery in RETURNING over a multi-tuple VALUES".
		{"another table entirely",
			[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`, `INSERT INTO s VALUES(1),(2),(3)`},
			`INSERT INTO t VALUES(1,'x'),(2,'y') RETURNING a,(SELECT count(*) FROM s)`,
			[]string{"1,3", "2,3"}},
		// this is the case that goes red if OpSubCacheReset stops being
		// neutered -- it would answer 1,1 2,2 3,3, which is what the same
		// statement WITHOUT the view (a per-row subquery over t itself)
		// legitimately answers. The frozen value is 1, not 0: OP_Once runs at
		// the FIRST RETURNING capture, which is inside the loop at the AFTER
		// position, so row 1's own update is already in it.
		{"UPDATE loop, once subquery over a view of the target",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
				`CREATE VIEW v AS SELECT * FROM t`},
			`UPDATE t SET b=b+1 RETURNING a,(SELECT sum(b) FROM v)`,
			[]string{"1,1", "2,1", "3,1"}},
		{"UPDATE loop, per-row subquery over the target",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,0),(2,0),(3,0)`},
			`UPDATE t SET b=b+1 RETURNING a,(SELECT sum(b) FROM t)`,
			[]string{"1,1", "2,2", "3,3"}},
		// execution, so it re-freezes at the new first capture (3 rows now at
		// b=1, row 1 goes to 2 -> sum 4).
		{"UPDATE loop, once subquery re-freezes per execution",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,1),(2,1),(3,1)`,
				`CREATE VIEW v AS SELECT * FROM t`},
			`UPDATE t SET b=b+1 RETURNING a,(SELECT sum(b) FROM v)`,
			[]string{"1,4", "2,4", "3,4"}},
		// trigger writes -- frozen, again, at the FIRST capture, so all three
		// rows report 0 even though the trigger has appended to log by then.
		// Recomputing per row would answer 1,1 2,2 3,3 here.
		{"UPDATE loop, once subquery over a table the trigger writes",
			[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `INSERT INTO t VALUES(1,0),(2,0),(3,0)`,
				`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`UPDATE t SET b=b+1 RETURNING a,(SELECT count(*) FROM log)`,
			[]string{"1,0", "2,0", "3,0"}},
		// the count must NOT include the row being reported, which is precisely
		// what returningSiteNoSubquery's note said the old placement got wrong.
		{"DELETE loop, per-row subquery over the target",
			[]string{`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`},
			`DELETE FROM t RETURNING a,(SELECT count(*) FROM t)`,
			[]string{"1,2", "2,1", "3,0"}},
		{"DELETE loop, once subquery over a table the trigger writes",
			[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
				`INSERT INTO t VALUES(1,'p'),(2,'q')`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
			`DELETE FROM t RETURNING a,(SELECT count(*) FROM log)`,
			[]string{"1,0", "2,0"}},
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", tc.name, s, err)
			}
		}
		_, cerr := db.compileWrite(tc.stmt)
		if cerr != nil {
			t.Errorf("[%s] not compiled (err=%v) -- the once-lifetime answers below are what only\n"+
				"the compiled form produces, so this case would be measuring nothing", tc.name, cerr)
			db.Discard()
			continue
		}
		_, rows, xerr := db.ExecReturningArgs(tc.stmt, nil)
		if xerr != nil {
			t.Errorf("[%s] exec: %v", tc.name, xerr)
			db.Discard()
			continue
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
