package engine

// Tests compile-time behavior of conflict clauses with triggers:
// lowering of shapes, label placement, trigger compilation under firing statement's policy,
// and UPDATE OF gate and upsert AFTER fire positioning.

import "testing"

// Test cases for conflict x trigger shapes that previously didn't compile.
var conflictTriggerShapes = []conflictShapeCase{
	// A declared ON CONFLICT default meeting an AFTER INSERT trigger -- the
	// shape whose oracle probe the old decline's own comment recorded.
	{[]string{`CREATE TABLE t(a INTEGER UNIQUE ON CONFLICT IGNORE, b TEXT)`, `CREATE TABLE log(tag,x,y)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`},
		`INSERT INTO t VALUES(1,'dup'),(2,'two')`},

	// Explicit OR-clauses, every action, plus the REPLACE INTO spelling.
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two')`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'dup'),(3,'three')`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`REPLACE INTO t VALUES(1,'dup'),(3,'three')`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR FAIL INTO t VALUES(1,'a'),(9,'dup'),(3,'c')`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR ABORT INTO t VALUES(1,'a'),(9,'dup'),(3,'c')`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR ROLLBACK INTO t VALUES(9,'dup')`},

	// The pre-store constraint skips emitInsertRowBody owns.
	{[]string{`CREATE TABLE t(a,b NOT NULL)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t VALUES(1,'a'),(2,NULL),(3,'c')`},
	{[]string{`CREATE TABLE t(a, b NOT NULL ON CONFLICT IGNORE)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,'a'),(2,NULL),(3,'c')`},
	{[]string{`CREATE TABLE t(a, b NOT NULL ON CONFLICT REPLACE DEFAULT 'dd')`, `CREATE TABLE log(x,y)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a,new.b); END`},
		`INSERT INTO t VALUES(1,'a'),(2,NULL)`},
	{[]string{`CREATE TABLE t(a,b, CHECK(a<10))`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t VALUES(1,'a'),(20,'b'),(3,'c')`},

	// A REPLACE whose implicit victim delete meets this table's own DELETE
	// triggers, with recursive_triggers both off and on.
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I',new.a); END`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('D',old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'dup')`},
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I',new.a); END`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES('D',old.a); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'dup')`},

	// trigger.c:1137 -- a body statement carrying its OWN clause, under a
	// firing statement that does and does not carry one.
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a UNIQUE,tag)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT OR REPLACE INTO u VALUES(new.a,'body'); END`},
		`INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a UNIQUE,tag)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT OR REPLACE INTO u VALUES(new.a,'body'); END`},
		`INSERT OR IGNORE INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE TABLE u(a UNIQUE,tag)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,'body'); END`},
		`INSERT OR REPLACE INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE u(a UNIQUE,tag)`,
		`CREATE TRIGGER tr AFTER UPDATE ON t BEGIN UPDATE u SET a=2 WHERE a=1; END`},
		`UPDATE OR IGNORE t SET b='y'`},

	// INSERT ... DEFAULT VALUES into a triggered table, both timings.
	{[]string{`CREATE TABLE t(a DEFAULT 7, b DEFAULT 'dd', c)`, `CREATE TABLE log(x,y,z,r)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a,new.b,new.c,new.rowid); END`},
		`INSERT INTO t DEFAULT VALUES`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b DEFAULT 'dd')`, `CREATE TABLE log(tag,x,y)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a,new.b); END`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`},
		`INSERT INTO t DEFAULT VALUES`},

	// "UPDATE OF <col-list>" -- the overlapping and the non-overlapping SET.
	{[]string{`CREATE TABLE t(a,b,c)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES('a',new.a); END`},
		`UPDATE t SET a=10`},
	{[]string{`CREATE TABLE t(a,b,c)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES('a',new.a); END`},
		`UPDATE t SET b=20`},
	{[]string{`CREATE TABLE t(a,b,c)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER tu AFTER UPDATE OF a,c ON t BEGIN INSERT INTO log VALUES('ac',new.c); END`},
		`UPDATE t SET b=21,a=11`},
	{[]string{`CREATE TABLE t(Abc,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE OF aBC ON t BEGIN INSERT INTO log VALUES(new.Abc); END`},
		`UPDATE t SET ABc=9`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER tu BEFORE UPDATE OF a ON t BEGIN INSERT INTO log VALUES('B',old.a); END`},
		`UPDATE t SET a=10`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER tp AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('plain',new.b); END`,
		`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES('ofa',new.a); END`},
		`UPDATE t SET b=20`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu BEFORE UPDATE OF a ON t BEGIN SELECT RAISE(IGNORE); END`},
		`UPDATE t SET a=a+100`},

	// UPDATE under a conflict clause / declared default with AFTER triggers.
	{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE)`, `CREATE TABLE log(x,y)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.b,new.b); END`},
		`UPDATE OR IGNORE t SET b=b+10`},
	{[]string{`CREATE TABLE t(a,b UNIQUE)`, `CREATE TABLE log(x,y)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.b,new.b); END`},
		`UPDATE OR REPLACE t SET b=b+10`},
	{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE)`, `CREATE TABLE log(x,y)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.b,new.b); END`},
		`UPDATE OR FAIL t SET b=b+10`},
	{[]string{`CREATE TABLE t(a, b INTEGER UNIQUE ON CONFLICT IGNORE)`, `CREATE TABLE log(x,y)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.b,new.b); END`},
		`UPDATE t SET b=b+10`},
	{[]string{`CREATE TABLE t(a,b NOT NULL)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`UPDATE OR IGNORE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`},
	{[]string{`CREATE TABLE t(a,b, CHECK(b<10))`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`},
		`UPDATE OR IGNORE t SET b=b+1`},

	// RETURNING beside the conflict clause and the trigger.
	{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`},
		`UPDATE OR IGNORE t SET b=b+10 RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two') RETURNING a,b`},

	// FOREIGN KEYS crossed with the conflict clause and the trigger: the child
	// side (sqlite3FkCheck, insert.c:1572-1573) and a REPLACE whose victim
	// delete runs an ON DELETE CASCADE (sqlite3GenerateRowDelete from the
	// OE_Replace arm, insert.c:2339 for a rowid collision and :2613 for a
	// UNIQUE index one -- both inside sqlite3GenerateConstraintChecks).
	{[]string{`PRAGMA foreign_keys=ON`, `CREATE TABLE p(k INTEGER PRIMARY KEY)`,
		`CREATE TABLE c(a UNIQUE, k REFERENCES p(k))`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON c BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO c VALUES(1,1),(2,99),(3,1)`},
	{[]string{`PRAGMA foreign_keys=ON`, `CREATE TABLE p(k INTEGER PRIMARY KEY, v UNIQUE)`,
		`CREATE TABLE c(id, k REFERENCES p(k) ON DELETE CASCADE)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER ta AFTER INSERT ON p BEGIN INSERT INTO log VALUES('I',new.k); END`},
		`INSERT OR REPLACE INTO p VALUES(2,'one')`},

	// UPSERT beside this table's own INSERT triggers: DO NOTHING, DO UPDATE, a
	// bare target, a WHERE, a rowid-moving SET, and the SELECT row source.
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`},
		`INSERT INTO t VALUES(1,'dup'),(2,'two') ON CONFLICT(a) DO NOTHING`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(tag,x,y)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('B',new.a,new.b); END`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`},
		`INSERT INTO t VALUES(1,'dup'),(2,'two') ON CONFLICT(a) DO UPDATE SET b=excluded.b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`},
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE 0`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE log(tag,x,y)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a,new.b); END`},
		`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET a=7`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`},
		`INSERT INTO t VALUES(1,'dup'),(3,'three') ON CONFLICT DO NOTHING`},
	{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(tag,x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('A',new.a); END`},
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=coalesce(c,0)+1`},

	// An AFTER program's RAISE(IGNORE) under a conflict clause.
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN SELECT RAISE(IGNORE); END`},
		`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two')`},

	// The row-source cluster crossed with this one.
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT OR IGNORE INTO t SELECT a,b FROM s`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`, `CREATE TABLE s(a,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t SELECT a,b FROM s`},

	// An INTEGER PRIMARY KEY rowid collision, and a WITHOUT ROWID target.
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE log(tag,x,y)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I',new.a,new.b); END`},
		`INSERT OR REPLACE INTO t VALUES(1,'dup'),(2,'two')`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `CREATE TABLE log(tag,x,y)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('I',new.a,new.b); END`},
		`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two')`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, `CREATE TABLE log(x,y)`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.k,new.v); END`},
		`INSERT OR REPLACE INTO t VALUES('a',9),('b',2)`},
}

// Verifies all conflict x trigger shapes compile.
func TestConflictTriggerShapesCompileToBytecode(t *testing.T) {
	for i, tc := range conflictTriggerShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// Verifies IGNORE skip label lands past the AFTER fire, not at it.
func TestConflictSkipLandsPastTheAfterFire(t *testing.T) {
	for _, tc := range []conflictShapeCase{
		{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT OR IGNORE INTO t VALUES(1,'dup'),(2,'two')`},
		{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO t VALUES(1,'dup'),(2,'two')`},
	} {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Fatalf("%q: %v", tc.stmt, err)
		}
		// Last OpFireTriggers of each row is its AFTER fire.
		lastFire := -1
		for i := range prog.Insns {
			if prog.Insns[i].Op == OpFireTriggers {
				lastFire = i
			}
		}
		if lastFire < 0 {
			t.Fatalf("%q: no OpFireTriggers emitted -- the shape under test is not the shape compiled", tc.stmt)
		}
		nInsert := 0
		for i := range prog.Insns {
			ip, ok := prog.Insns[i].P4.(*insertPlan)
			if !ok || prog.Insns[i].Op != OpInsert {
				continue
			}
			nInsert++
			if ip.skipAddr <= i {
				t.Errorf("%q: OpInsert at %d has skipAddr %d -- it must jump FORWARD to the row end",
					tc.stmt, i, ip.skipAddr)
			}
			// Find this row's AFTER fire: first OpFireTriggers after this OpInsert.
			after := -1
			for j := i + 1; j < len(prog.Insns); j++ {
				if prog.Insns[j].Op == OpFireTriggers {
					after = j
					break
				}
			}
			if after < 0 {
				t.Errorf("%q: OpInsert at %d is followed by no AFTER fire", tc.stmt, i)
				continue
			}
			if ip.skipAddr <= after {
				t.Errorf("%q: OpInsert at %d skips to %d, which is AT OR BEFORE its AFTER fire at %d.\n"+
					"An IGNOREd row would fire the AFTER trigger; C SQLite's ignoreDest is endOfLoop\n"+
					"(insert.c:1569-1571), resolved BELOW the AFTER call (insert.c:1604-1613).",
					tc.stmt, i, ip.skipAddr, after)
			}
		}
		if nInsert != 2 {
			t.Errorf("%q: %d OpInsert emitted, want 2 (one per VALUES tuple)", tc.stmt, nInsert)
		}
	}
}

// UPDATE twin: skip label lands past AFTER fire for UPDATE statements.
func TestUpdateConflictSkipLandsPastTheAfterFire(t *testing.T) {
	tc := conflictShapeCase{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER ta AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`},
		`UPDATE OR IGNORE t SET b=b+10`}
	prog, err := compileShape(t, tc)
	if err != nil {
		t.Fatalf("%q: %v", tc.stmt, err)
	}
	seen := 0
	for i := range prog.Insns {
		up, ok := prog.Insns[i].P4.(*updatePlan)
		if !ok || prog.Insns[i].Op != OpUpdateRow {
			continue
		}
		seen++
		after := -1
		for j := i + 1; j < len(prog.Insns); j++ {
			if prog.Insns[j].Op == OpFireTriggers {
				after = j
				break
			}
		}
		if after < 0 {
			t.Fatalf("%q: OpUpdateRow at %d is followed by no AFTER fire", tc.stmt, i)
		}
		if up.skipAddr <= after {
			t.Errorf("%q: OpUpdateRow at %d skips to %d, which is AT OR BEFORE its AFTER fire at %d",
				tc.stmt, i, up.skipAddr, after)
		}
	}
	if seen != 1 {
		t.Fatalf("%q: %d OpUpdateRow emitted, want 1", tc.stmt, seen)
	}
}

// Verifies trigger body statements inherit firing statement's conflict policy.
func TestFiringStatementOrconfReachesTheBody(t *testing.T) {
	const trigReplace = `CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT OR REPLACE INTO u VALUES(new.a,'body'); END`
	const trigPlain = `CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO u VALUES(new.a,'body'); END`
	setup := func(trig string) []string {
		return []string{`CREATE TABLE t(a)`, `CREATE TABLE u(a UNIQUE,tag)`, trig}
	}
	for _, tc := range []struct {
		stmt   string
		trig   string
		want   conflictAction
		wantEx bool
		why    string
	}{
		{`INSERT INTO t VALUES(1)`, trigReplace, conflictReplace, true,
			"a bare INSERT is OE_Default (parse.y:489), so the body keeps its own OR REPLACE"},
		{`INSERT OR IGNORE INTO t VALUES(1)`, trigReplace, conflictIgnore, true,
			"the firing OR IGNORE is not OE_Default, so it OVERRIDES the body's own OR REPLACE"},
		{`INSERT OR REPLACE INTO t VALUES(1)`, trigPlain, conflictReplace, true,
			"a clause-less body INHERITS the firing statement's REPLACE"},
		{`INSERT INTO t VALUES(1)`, trigPlain, conflictAbort, false,
			"neither names a policy: the body stays at the ABORT default with no explicit clause"},
	} {
		prog, err := compileShape(t, conflictShapeCase{setup(tc.trig), tc.stmt})
		if err != nil {
			t.Fatalf("%q: %v", tc.stmt, err)
		}
		plans := bodyInsertPlansInto(prog, "u")
		if len(plans) != 1 {
			t.Fatalf("%q: found %d body insertPlans for u, want 1 -- the body did not compile",
				tc.stmt, len(plans))
		}
		if plans[0].action != tc.want || plans[0].explicitOr != tc.wantEx {
			t.Errorf("%q: body compiled with action=%v explicitOr=%v, want %v/%v (%s -- trigger.c:1137)",
				tc.stmt, plans[0].action, plans[0].explicitOr, tc.want, tc.wantEx, tc.why)
		}
	}
}

// Collects every insertPlan targeting table `name` from sub-programs (trigger bodies).
func bodyInsertPlansInto(prog *Program, name string) []*insertPlan {
	var out []*insertPlan
	seen := map[*Program]bool{prog: true}
	var walk func(p *Program, top bool)
	walk = func(p *Program, top bool) {
		if p == nil {
			return
		}
		for i := range p.Insns {
			if !top {
				if ip, ok := p.Insns[i].P4.(*insertPlan); ok && ip.tbl != nil && equalFoldName(ip.tbl.name, name) {
					out = append(out, ip)
				}
			}
			for _, sub := range subProgramsOf(p.Insns[i].P4) {
				if seen[sub] {
					continue
				}
				seen[sub] = true
				walk(sub, false)
			}
		}
	}
	walk(prog, true)
	return out
}

// Verifies upsert AFTER fire sits on the plain-insert branch only.
func TestUpsertAfterFireSitsOnThePlainInsertBranch(t *testing.T) {
	for _, tc := range []conflictShapeCase{
		{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO NOTHING`},
		{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO t VALUES(1,'dup') ON CONFLICT(a) DO UPDATE SET b='u'`},
	} {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Fatalf("%q: %v", tc.stmt, err)
		}
		fire, insert := -1, -1
		var up *upsertPlan
		for i := range prog.Insns {
			switch prog.Insns[i].Op {
			case OpFireTriggers:
				fire = i
			case OpInsert:
				insert = i
			case OpUpsertFind:
				up, _ = prog.Insns[i].P4.(*upsertPlan)
			}
		}
		if up == nil || fire < 0 || insert < 0 {
			t.Fatalf("%q: upsertPlan=%v fire=%d insert=%d -- the shape under test is not the shape compiled",
				tc.stmt, up != nil, fire, insert)
		}
		if fire < insert {
			t.Errorf("%q: the AFTER fire at %d is ABOVE the plain-insert OpInsert at %d",
				tc.stmt, fire, insert)
		}
		if up.skipAddr <= fire {
			t.Errorf("%q: OpUpsertFind's skipAddr is %d, at or before the AFTER fire at %d.\n"+
				"A DO NOTHING / DO UPDATE candidate would fire the AFTER INSERT trigger, which\n"+
				"reaches insert.c:1604-1607 only on the plain-insert path.", tc.stmt, up.skipAddr, fire)
		}
	}
}

// Verifies UPDATE OF clause gates trigger firing to overlapping columns only.
func TestUpdateOfGateCodesOnlyOverlappingTriggers(t *testing.T) {
	for _, tc := range []struct {
		setup []string
		stmt  string
		fires int
	}{
		{[]string{`CREATE TABLE t(a,b,c)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`UPDATE t SET a=10`, 1},
		{[]string{`CREATE TABLE t(a,b,c)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`UPDATE t SET b=20`, 0},
		{[]string{`CREATE TABLE t(a,b,c)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE OF a,c ON t BEGIN INSERT INTO log VALUES(new.c); END`},
			`UPDATE t SET b=21,a=11`, 1},
		{[]string{`CREATE TABLE t(a,b,c)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE OF a,c ON t BEGIN INSERT INTO log VALUES(new.c); END`},
			`UPDATE t SET b=21`, 0},
		{[]string{`CREATE TABLE t(Abc,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tu AFTER UPDATE OF aBC ON t BEGIN INSERT INTO log VALUES(new.Abc); END`},
			`UPDATE t SET ABc=9`, 1},
		// Plain trigger fires regardless; OF-gated one fires only on overlap.
		{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tp AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`,
			`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`UPDATE t SET b=20`, 1},
		{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER tp AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.b); END`,
			`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`UPDATE t SET a=10`, 2},
	} {
		prog, err := compileShape(t, conflictShapeCase{tc.setup, tc.stmt})
		if err != nil {
			t.Fatalf("%q: %v", tc.stmt, err)
		}
		coded := 0
		for i := range prog.Insns {
			fp, ok := prog.Insns[i].P4.(*triggerFirePlan)
			if !ok || prog.Insns[i].Op != OpFireTriggers {
				continue
			}
			coded += len(fp.triggers)
		}
		if coded != tc.fires {
			t.Errorf("%q: %d trigger(s) coded, want %d -- checkColumnOverlap (trigger.c:781-788) "+
				"decides this at COMPILE time", tc.stmt, coded, tc.fires)
		}
	}
}

// Verifies plans are reused correctly across multiple executions.
func TestConflictTriggerRepeatedStatementUsesTheCachedPlan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		want  []int64 // log contents after running stmt twice
	}{
		{"or-ignore", []string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT OR IGNORE INTO t VALUES(1,'x')`, []int64{1}},
		{"declared-ignore", []string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO t VALUES(1,'x')`, []int64{1}},
		{"upsert-do-nothing", []string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO NOTHING`, []int64{1}},
		// REPLACE stores on both runs, so it fires twice -- the case that
		// would go unnoticed if the assertion only ever expected one row.
		{"or-replace", []string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
			`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`INSERT OR REPLACE INTO t VALUES(1,'x')`, []int64{1, 1}},
		{"update-of-gate", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `INSERT INTO t VALUES(1,2)`,
			`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES(new.a); END`},
			`UPDATE t SET b=b+1`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("compile %q: %v -- this probe measures the COMPILED plan", tc.stmt, cerr)
			}
			for i := 0; i < 2; i++ {
				if err := db.Exec(tc.stmt); err != nil {
					t.Fatalf("run %d of %q: %v", i+1, tc.stmt, err)
				}
			}
			var got []int64
			for _, r := range rvdQuery(t, db, `SELECT x FROM log ORDER BY rowid`) {
				got = append(got, r[0].I)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%q run twice: log holds %v, want %v", tc.stmt, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("%q run twice: log holds %v, want %v", tc.stmt, got, tc.want)
				}
			}
		})
	}
}

// Verifies UPDATE OF gate survives plan caching and schema invalidation.
func TestUpdateOfGateSurvivesThePlanCache(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `INSERT INTO t VALUES(1,2)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	// No trigger yet.
	for _, s := range []string{`UPDATE t SET a=a+1`, `UPDATE t SET b=b+1`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
	if err := db.Exec(`CREATE TRIGGER tu AFTER UPDATE OF a ON t BEGIN INSERT INTO log VALUES(new.a); END`); err != nil {
		t.Fatal(err)
	}
	// Same texts after trigger creation: schemaGen invalidates cache.
	for _, s := range []string{`UPDATE t SET a=a+1`, `UPDATE t SET b=b+1`, `UPDATE t SET b=b+1`, `UPDATE t SET a=a+1`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
	rows := rvdQuery(t, db, `SELECT count(*) FROM log`)
	if len(rows) != 1 || rows[0][0].I != 2 {
		t.Fatalf("log holds %v, want a single row of 2 -- only the two SETs naming `a` may fire", rows)
	}
}
