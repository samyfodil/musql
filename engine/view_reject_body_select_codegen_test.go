package engine

// Engine-side verification for view write rejection, schema-qualified column references in view updates,
// and trigger body bare SELECT statements. These must compile to bytecode, not decline.

import (
	"fmt"
	"strings"
	"testing"
)

// ---- view writes with no INSTEAD OF trigger: a HARD compile error ----

// viewRejectShapes: every spelling that must be rejected outright.
var viewRejectShapes = []conflictShapeCase{
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `INSERT INTO v VALUES(1,2)`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `UPDATE v SET c=9`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `DELETE FROM v WHERE a=1`},

	// RETURNING does not save a view write.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `INSERT INTO v VALUES(1,2) RETURNING a`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `UPDATE v SET c=9 RETURNING a`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `DELETE FROM v RETURNING a`},

	// Row sources and clauses this compiler does not model for a view.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE s(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `INSERT INTO v SELECT a,c FROM s`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `INSERT INTO v DEFAULT VALUES`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE TABLE m(k,nv)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `UPDATE v SET c=m.nv FROM m WHERE m.k=v.a`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `INSERT OR REPLACE INTO v VALUES(1,2)`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `UPDATE v INDEXED BY nope SET c=9`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`}, `DELETE FROM v AS x WHERE x.a=1`},

	// A view carrying only one verb's INSTEAD OF trigger rejects the other two.
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`}, `UPDATE v SET c=9`},
	{[]string{`CREATE TABLE b(a,c)`, `CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vi INSTEAD OF INSERT ON v BEGIN INSERT INTO b VALUES(new.a,new.c); END`}, `DELETE FROM v`},
}

// TestViewNoInsteadOfIsAHardCompileError asserts that compileWrite returns no program for view writes without INSTEAD OF triggers.
func TestViewNoInsteadOfIsAHardCompileError(t *testing.T) {
	const want = "cannot modify v because it is a view"
	for i, tc := range viewRejectShapes {
		prog, err := compileShape(t, tc)
		if err == nil {
			t.Errorf("RULE #1 REGRESSION: [%d] %q COMPILED.\n"+
				"sqlite3IsReadOnly rejects it at PREPARE time (delete.c:124-130), so this must be a\n"+
				"hard compile error -- which is also what leaves changes() alone, by construction\n"+
				"rather than by markPrepareFailed's simulation.", i, tc.stmt)
			continue
		}
		if prog != nil {
			t.Errorf("[%d] %q returned BOTH a program and an error %v", i, tc.stmt, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("[%d] %q error is %q, want it to contain %q -- the wording is\n"+
				"db.viewModifyError's, CALLED rather than restated so the wordings cannot drift.",
				i, tc.stmt, err, want)
		}
	}
}

// TestViewNoInsteadOfLeavesChangesAlone verifies that a hard compile error never enters runWrite,
// so its deferred setChanges never executes and changes() is left untouched.
func TestViewNoInsteadOfLeavesChangesAlone(t *testing.T) {
	for _, stmt := range []string{
		`INSERT INTO v VALUES(9,9)`,
		`UPDATE v SET c=9`,
		`DELETE FROM v WHERE a=1`,
		`INSERT INTO v VALUES(9,9) RETURNING a`,
		`INSERT INTO v SELECT a,c FROM b`,
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{
			`CREATE TABLE b(a,c)`,
			`CREATE VIEW v AS SELECT a,c FROM b`,
			`INSERT INTO b VALUES(1,1),(2,2),(3,3)`,
		} {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		if err := db.Exec(stmt); err == nil {
			t.Errorf("%q: want an error, got none", stmt)
		}
		ch, tot, _, _, _ := db.ConnState()
		if ch != 3 || tot != 3 {
			t.Errorf("%q: changes()=%d total_changes()=%d after the rejection, want 3 and 3 --\n"+
				"the previous statement's counts, which a statement that never reached RUN state\n"+
				"must leave exactly where they were.", stmt, ch, tot)
		}
		db.Discard()
	}
}

// ---- a schema-qualified column reference in a view write ----

var schemaQualifiedViewShapes = []conflictShapeCase{
	{[]string{`CREATE TABLE b(x,b)`, `CREATE VIEW v5 AS SELECT x,b FROM b`,
		`CREATE TRIGGER v5u INSTEAD OF UPDATE ON v5 BEGIN UPDATE b SET b=new.b; END`},
		`UPDATE v5 SET b = main.v5.b+9900000 WHERE main.v5.x BETWEEN 3 AND 5`},
	{[]string{`CREATE TABLE b(x,b)`, `CREATE VIEW v5 AS SELECT x,b FROM b`,
		`CREATE TRIGGER v5u INSTEAD OF UPDATE ON v5 BEGIN UPDATE b SET b=new.b; END`},
		`UPDATE v5 SET b = b+1 WHERE main.v5.x>2`},
	{[]string{`CREATE TABLE b(x,b)`, `CREATE VIEW v5 AS SELECT x,b FROM b`,
		`CREATE TRIGGER v5u INSTEAD OF UPDATE ON v5 BEGIN UPDATE b SET b=new.b; END`},
		`UPDATE v5 SET b = main.v5.b+1`},
	{[]string{`CREATE TABLE b(x,b)`, `CREATE TABLE log(k)`, `CREATE VIEW v5 AS SELECT x,b FROM b`,
		`CREATE TRIGGER v5d INSTEAD OF DELETE ON v5 BEGIN INSERT INTO log VALUES(old.x); END`},
		`DELETE FROM v5 WHERE main.v5.x>2`},
}

func TestSchemaQualifiedViewWriteCompilesToBytecode(t *testing.T) {
	for i, tc := range schemaQualifiedViewShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// TestSchemaQualifiedViewWriteAnswers verifies the compiled program correctly resolves schema-qualified columns.
func TestSchemaQualifiedViewWriteAnswers(t *testing.T) {
	setup := []string{
		`CREATE TABLE b(x,b)`,
		`CREATE TABLE log(k,v)`,
		`CREATE VIEW v5 AS SELECT x,b FROM b`,
		`CREATE TRIGGER v5u INSTEAD OF UPDATE ON v5 BEGIN INSERT INTO log VALUES(new.x,new.b); END`,
		`INSERT INTO b VALUES(1,10),(3,30),(4,40),(6,60)`,
	}
	for _, tc := range []struct {
		stmt    string
		wantErr string
		want    string
	}{
		{`UPDATE v5 SET b = main.v5.b+9900000 WHERE main.v5.x BETWEEN 3 AND 5`, "", "3=9900030 4=9900040"},
		{`UPDATE v5 SET b = b+9900000 WHERE x BETWEEN 3 AND 5`, "", "3=9900030 4=9900040"},
		{`UPDATE v5 SET b = nosuchdb.v5.b+1`, "no such column: nosuchdb.v5.b", ""},
		{`UPDATE v5 SET b = main.v5.nosuchcol+1`, "no such column", ""},
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		err = db.Exec(tc.stmt)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tc.stmt, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%q: want an error containing %q, got %v", tc.stmt, tc.wantErr, err)
		case tc.wantErr == "":
			if got := logPairs(t, db); got != tc.want {
				t.Errorf("%q: fired %s, want %s", tc.stmt, got, tc.want)
			}
		}
		db.Discard()
	}
}

// logPairs reads the two-column log table this file's INSTEAD OF bodies write.
func logPairs(t *testing.T, db *Session) string {
	t.Helper()
	pgr, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	sel, perr := ParseSelect(`SELECT k,v FROM log ORDER BY k`)
	if perr != nil {
		t.Fatal(perr)
	}
	_, rows, qerr := pgr.execSelect(sel, nil, nil, "")
	if qerr != nil {
		t.Fatal(qerr)
	}
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%d=%d", r[0].I, r[1].I))
	}
	return strings.Join(out, " ")
}

// Shapes for trigger body SELECT statements.

var bodySelectShapes = []conflictShapeCase{
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s WHERE x>1; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT count(*) FROM s; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`, `CREATE TABLE u(y)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT s.x FROM s JOIN u ON u.y=s.x; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s LIMIT 1; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT y FROM (SELECT x AS y FROM s); END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`, `CREATE TABLE u(y)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s UNION ALL SELECT y FROM u; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s ORDER BY x; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT DISTINCT x FROM s; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT count(*) FROM t; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER DELETE ON t BEGIN SELECT x FROM s; END`}, `DELETE FROM t WHERE a=1`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tb AFTER UPDATE ON t BEGIN SELECT x FROM s; END`}, `UPDATE t SET b=9`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM f; END`}, `INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT id FROM r WHERE x0<5; END`}, `INSERT INTO t VALUES(9)`},
	{[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT count(*) FROM f; END`}, `INSERT INTO t VALUES(1)`},
	{[]string{`CREATE TABLE t(a,b)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT value FROM json_each('[1,2,3]'); END`},
		`INSERT INTO t VALUES(1,2)`},
	{[]string{`CREATE TABLE t(a,b)`,
		`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT value FROM json_each(new.b); END`},
		`INSERT INTO t VALUES(1,'[4,5]')`},
}

func TestTriggerBodySelectCompilesToBytecode(t *testing.T) {
	for i, tc := range bodySelectShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// TestTriggerBodySelectIsLiveAndUnsorted verifies the body's source is re-lowered per firing (LIVE)
// and carries no ORDER BY or DISTINCT (UNSORTED), which the oracle drops before resolving.
func TestTriggerBodySelectIsLiveAndUnsorted(t *testing.T) {
	for i, tc := range bodySelectShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			continue // reported by the test above
		}
		// The BODY STEP's own program, not the whole graph.
		var derived int
		for _, body := range triggerBodyPrograms(prog) {
			for j := range body.Insns {
				ds, ok := body.Insns[j].P4.(*derivedSource)
				if !ok || body.Insns[j].Op != OpOpenDerived {
					continue
				}
				derived++
				if ds.prog == nil || ds.prog.LiveSource == nil {
					t.Errorf("[%d] %q: the body SELECT's source is FROZEN (no LiveSource); a trigger\n"+
						"body reads the database as of the FIRING, not as of this compile.", i, tc.stmt)
					continue
				}
				if len(ds.prog.LiveSource.OrderBy) != 0 || ds.prog.LiveSource.Distinct {
					t.Errorf("[%d] %q: the lowered body SELECT still carries ORDER BY/DISTINCT.\n"+
						"select.c:7637-7653 deletes both for SRT_Discard before names are resolved.", i, tc.stmt)
				}
			}
		}
		if derived == 0 {
			t.Errorf("[%d] %q: the trigger's body step opens no derived source -- the body SELECT is\n"+
				"not being run at all, which would pass the compile check above while evaluating\n"+
				"nothing.", i, tc.stmt)
		}
	}
}

// TestTriggerBodySelectDoesNotPublishChanges verifies a bare SELECT step does not emit OP_ResetCount.
func TestTriggerBodySelectDoesNotPublishChanges(t *testing.T) {
	prog, err := compileShape(t, conflictShapeCase{
		[]string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s; END`},
		`INSERT INTO t VALUES(1,2)`,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, in := range prog.Insns {
		fp, ok := in.P4.(*triggerFirePlan)
		if !ok {
			continue
		}
		for _, ct := range fp.triggers {
			for _, body := range ct.body {
				if body.CountsChanges {
					t.Errorf("a bare SELECT body step is marked CountsChanges; trigger.c:1179-1186\n" +
						"emits no OP_ResetCount for it, unlike the three DML arms.")
				}
			}
		}
	}
}

// TestTriggerBodySelectAnswers is the behavioral test, running ENGINE-DIRECT on one connection.
func TestTriggerBodySelectAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   []string
		stmts   []string
		wantErr []bool
	}{{
		// Per-row errors in discarded expressions abort the firing statement.
		name: "per-row error surfaces",
		setup: []string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT abs(x) FROM s; END`},
		stmts:   []string{`INSERT INTO s VALUES(-9223372036854775808)`, `INSERT INTO t VALUES(1)`},
		wantErr: []bool{false, true},
	}, {
		// No rows means no error.
		name: "no rows, no error",
		setup: []string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT abs(x) FROM s; END`},
		stmts:   []string{`INSERT INTO t VALUES(1)`},
		wantErr: []bool{false},
	}, {
		// The body sees the row the firing statement just stored.
		name: "the body sees the firing statement's own row",
		setup: []string{`CREATE TABLE s(x)`,
			`CREATE TRIGGER tb AFTER INSERT ON s BEGIN SELECT abs(x) FROM s; END`},
		stmts:   []string{`INSERT INTO s VALUES(-9223372036854775808)`},
		wantErr: []bool{true},
	}, {
		// ORDER BY is dropped before name resolution, so missing columns are silently ignored.
		name: "ORDER BY naming no column at all is ignored",
		setup: []string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT x FROM s ORDER BY nosuchcol; END`},
		stmts:   []string{`INSERT INTO s VALUES(1)`, `INSERT INTO t VALUES(1)`},
		wantErr: []bool{false, false},
	}, {
		// Two firings must re-lower against each firing's image.
		name: "two firings on one connection",
		setup: []string{`CREATE TABLE s(x)`,
			`CREATE TRIGGER tb AFTER INSERT ON s BEGIN SELECT abs(x) FROM s; END`},
		stmts:   []string{`INSERT INTO s VALUES(1)`, `INSERT INTO s VALUES(-9223372036854775808)`},
		wantErr: []bool{false, true},
	}, {
		// The body's source is re-lowered per firing, not carried in the cached program.
		name: "same statement text twice, base changed in between",
		setup: []string{`CREATE TABLE t(a)`, `CREATE TABLE s(x)`,
			`CREATE TRIGGER tb AFTER INSERT ON t BEGIN SELECT abs(x) FROM s; END`},
		stmts: []string{
			`INSERT INTO t VALUES(1)`,
			`INSERT INTO s VALUES(-9223372036854775808)`,
			`INSERT INTO t VALUES(1)`,
		},
		wantErr: []bool{false, false, true},
	}} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", tc.name, s, err)
			}
		}
		for i, s := range tc.stmts {
			gotErr := db.Exec(s) != nil
			if gotErr != tc.wantErr[i] {
				t.Errorf("[%s] %q: error=%v, want %v", tc.name, s, gotErr, tc.wantErr[i])
			}
		}
		db.Discard()
	}
}

// triggerBodyPrograms returns every trigger BODY STEP program reachable from prog.
func triggerBodyPrograms(prog *Program) []*Program {
	seen := map[*Program]bool{}
	var out []*Program
	var walk func(*Program)
	walk = func(p *Program) {
		if p == nil || seen[p] {
			return
		}
		seen[p] = true
		for i := range p.Insns {
			if fp, ok := p.Insns[i].P4.(*triggerFirePlan); ok {
				for _, ct := range fp.triggers {
					out = append(out, ct.body...)
					for _, b := range ct.body {
						walk(b)
					}
				}
			}
		}
	}
	walk(prog)
	return out
}
