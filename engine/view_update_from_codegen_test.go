package engine

// Tests that UPDATE ... FROM on views compiles and executes correctly,
// including trigger bodies with live row context.

import (
	"strings"
	"testing"
)

// viewUpfromShapes: the statements this batch promotes.
var viewUpfromShapes = []conflictShapeCase{
	// The census shape, verbatim from vdbe_total_test.go's residual list.
	{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`},

	// No WHERE at all: a bare cross join.
	{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
		`UPDATE v1 SET v=m.nv FROM m`},

	// Several SET targets, one of them mixing both sides of the join.
	{[]string{`CREATE TABLE b(k,v,w)`, `CREATE VIEW v1 AS SELECT k,v,w FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
		`UPDATE v1 SET v=m.nv, w=v1.k||m.nv FROM m WHERE m.k=v1.k`},

	// An "AS alias" on the view target -- an ordinary FROM-item alias inside
	// pass one's SELECT, unlike the materialize-and-scan path where it is the
	// name sqlite3MaterializeView's aliasless scope cannot answer for.
	{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
		`UPDATE v1 AS z SET v=m.nv FROM m WHERE m.k=z.k`},

	// A leading WITH clause naming a CTE as the FROM source.
	{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
		`WITH data(dk,dv) AS (VALUES(3,'thirty')) UPDATE v1 SET v=dv FROM data WHERE k=dk`},

	// An outer join INSIDE the FROM clause -- the view is comma-joined after
	// it, so it can never be null-extended.
	{[]string{`CREATE TABLE b(a,c1,c2)`, `CREATE VIEW v1 AS SELECT a,c1,c2 FROM b`,
		`CREATE TABLE m1(x,y)`, `CREATE TABLE m2(u,w)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET c1=new.c1; END`},
		`UPDATE v1 SET c1=y, c2=w FROM m1 LEFT JOIN m2 ON (u=x) WHERE x=a`},

	// A derived table as the FROM source.
	{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
		`UPDATE v1 SET v=s.nv FROM (SELECT k,nv FROM m WHERE k=1) AS s WHERE s.k=v1.k`},

	// A SET right-hand side carrying its own scalar subquery -- pass one reads
	// the untouched tables by construction, so nothing here is the
	// "SET subquery over the target" decline.
	{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
		`UPDATE v1 SET v=(SELECT count(*) FROM m) FROM m WHERE m.k=v1.k`},

	// A view over a JOIN, and a view with a declared column list -- two
	// different routes through viewColumnInfos, both of which the star
	// expansion in pass one has to agree with.
	{[]string{`CREATE TABLE b1(k,v)`, `CREATE TABLE b2(k,w)`,
		`CREATE VIEW v1 AS SELECT b1.k AS k, b1.v AS v, b2.w AS w FROM b1 JOIN b2 ON b2.k=b1.k`,
		`CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b1 SET v=new.v; END`},
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`},
	{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1(p,q) AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.q; END`},
		`UPDATE v1 SET q=m.nv FROM m WHERE m.k=v1.p`},

	// The whole shape inside a TRIGGER BODY, which takes the LIVE pass-one
	// route. What is pinned is the BODY sub-program, not the INSERT that fires
	// it: the INSERT compiles either way.
	{[]string{`CREATE TABLE fire(z)`, `CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`,
		`CREATE TRIGGER tf AFTER INSERT ON fire BEGIN UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k; END`},
		`INSERT INTO fire VALUES(1)`},
}

// viewUpfromDeclined: shapes the view-FROM emitter refuses.
var viewUpfromDeclined = []struct {
	why string
	tc  conflictShapeCase
}{
	{"INDEXED BY over a view",
		conflictShapeCase{[]string{`CREATE TABLE b(k,v)`, `CREATE INDEX ix ON b(k)`,
			`CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
			`UPDATE v1 INDEXED BY ix SET v=m.nv FROM m WHERE m.k=v1.k`}},
	{"a SET target that is not a view column",
		conflictShapeCase{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
			`UPDATE v1 SET nosuchcol=m.nv FROM m WHERE m.k=v1.k`}},
	{"a bare aggregate SET expression",
		conflictShapeCase{[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(nv)`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
			`UPDATE v1 SET v=sum(m.nv) FROM m`}},
}

func TestViewUpdateFromCompilesToBytecode(t *testing.T) {
	for i, tc := range viewUpfromShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
			continue
		}
	}
}

// TestViewUpdateFromEmitsTheJoinEphemeralLoop verifies the correct program
// structure: OpOpenDerived with viewUpfrom source and an OpRewind/OpNext loop.
func TestViewUpdateFromEmitsTheJoinEphemeralLoop(t *testing.T) {
	for i, tc := range viewUpfromShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		for _, p := range append([]*Program{prog}, allSubPrograms(prog)...) {
			ephCur, openAt, plain := -1, -1, 0
			for j := range p.Insns {
				if p.Insns[j].Op != OpOpenDerived {
					continue
				}
				ds, ok := p.Insns[j].P4.(*derivedSource)
				if !ok || ds == nil {
					continue
				}
				if ds.viewUpfrom == nil {
					plain++
					continue
				}
				if ds.prog == nil {
					t.Errorf("[%d] %q: view-upfrom source carries no compiled pass-one program", i, tc.stmt)
				}
				ephCur, openAt = p.Insns[j].P1, j
			}
			if ephCur < 0 {
				continue // not the program holding the UPDATE; try the next
			}
			if plain != 0 {
				t.Errorf("[%d] %q: %d plain OpOpenDerived beside the join ephemeral",
					i, tc.stmt, plain)
			}
			var rewind, next, fire bool
			for j := range p.Insns {
				in := &p.Insns[j]
				switch in.Op {
				case OpRewind:
					rewind = rewind || (in.P1 == ephCur && j > openAt)
				case OpNext:
					next = next || (in.P1 == ephCur && in.P2 > openAt && in.P2 < j)
				case OpFireTriggers:
					fire = true
				}
			}
			if !rewind || !next || !fire {
				t.Errorf("[%d] %q: not the correct loop structure", i, tc.stmt)
			}
			goto found
		}
		t.Errorf("[%d] %q emitted no view-upfrom source in its program graph", i, tc.stmt)
	found:
	}
}

// TestViewUpdateFromReadsSetValuesFromTheEphemeral verifies that SET values
// are read from the ephemeral cursor, not re-compiled.
func TestViewUpdateFromReadsSetValuesFromTheEphemeral(t *testing.T) {
	tc := conflictShapeCase{[]string{`CREATE TABLE b(k,v,w)`, `CREATE VIEW v1 AS SELECT k,v,w FROM b`,
		`CREATE TABLE m(k,nv,nw)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`},
		`UPDATE v1 SET v=m.nv, w=m.nw FROM m WHERE m.k=v1.k`}
	prog, err := compileShape(t, tc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	const nv = 3 // the view's own column count
	want := map[int]bool{nv: false, nv + 1: false}
	for i := range prog.Insns {
		in := &prog.Insns[i]
		if in.Op == OpColumn && in.P1 == 0 {
			if _, ok := want[in.P2]; ok {
				want[in.P2] = true
			}
		}
		if in.Op == OpAggStep || in.Op == OpHashAggStep {
			t.Errorf("aggregate opcode found; SET expressions re-coded instead of read")
		}
	}
	for col, seen := range want {
		if !seen {
			t.Errorf("no OpColumn reading ephemeral column %d", col)
		}
	}
}

// TestUpdateFromTriggerBodyIsLive verifies that trigger body UPDATE ... FROM
// compiles with live pass-one context.
func TestUpdateFromTriggerBodyIsLive(t *testing.T) {
	for _, tc := range []conflictShapeCase{
		{[]string{`CREATE TABLE t1(a,c)`, `CREATE TABLE mp(k,v)`, `CREATE TABLE src(a)`,
			`CREATE TRIGGER tr2 AFTER INSERT ON src BEGIN UPDATE t1 SET c=v FROM mp WHERE k=1; END`},
			`INSERT INTO src VALUES(1)`},
		{[]string{`CREATE TABLE fire(z)`, `CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`,
			`CREATE TABLE m(k,nv)`,
			`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`,
			`CREATE TRIGGER tf AFTER INSERT ON fire BEGIN UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k; END`},
			`INSERT INTO fire VALUES(1)`},
	} {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("%q: compile: %v", tc.stmt, err)
			continue
		}
		// Check the body program, not the firing INSERT.
		if prog.WritePager != nil {
			t.Errorf("%q: firing program should not carry WritePager", tc.stmt)
		}
		found := false
		for _, p := range append([]*Program{prog}, allSubPrograms(prog)...) {
			for i := range p.Insns {
				ds, ok := p.Insns[i].P4.(*derivedSource)
				if !ok || ds == nil || (ds.upfrom == nil && ds.viewUpfrom == nil) {
					continue
				}
				found = true
				if ds.prog == nil || ds.prog.LiveSource == nil {
					t.Errorf("%q: body's pass one is frozen", tc.stmt)
				}
			}
		}
		if !found {
			t.Errorf("%q emitted no UPDATE ... FROM pass-one source", tc.stmt)
		}
	}
}

// TestViewUpdateFromDeclinedShapesDeclineCleanly is the other half of
// never-wrong: what this emitter does not model must decline, not be
// half-served.
func TestViewUpdateFromDeclinedShapesDeclineCleanly(t *testing.T) {
	for _, d := range viewUpfromDeclined {
		prog, err := compileShape(t, d.tc)
		if err != nil {
			// A hard error is the correct outcome; what must not happen is a
			// compiled program.
			continue
		}
		if prog != nil {
			t.Errorf("%q COMPILED, but this batch does not model it: %s.\n"+
				"Serving it here without the semantics that shape needs is a WRONG ANSWER,\n"+
				"not a promotion (AGENTS.md invariant 1).", d.tc.stmt, d.why)
		}
	}
}

// TestViewUpdateFromCacheability verifies that top-level view UPDATE ... FROM
// programs are not cached, since they read a frozen snapshot.
func TestViewUpdateFromCacheability(t *testing.T) {
	for i, tc := range viewUpfromShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		// Trigger body case must not carry WritePager.
		body := len(tc.setup) > 0 && tc.stmt == `INSERT INTO fire VALUES(1)`
		if body == (prog.WritePager != nil) {
			t.Errorf("[%d] %q: WritePager non-nil is %v, want %v", i, tc.stmt, prog.WritePager != nil, !body)
		}
	}

	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.k||':'||old.v||'->'||new.v); END`,
		`INSERT INTO b VALUES(1,'a')`,
		`INSERT INTO m VALUES(1,'A')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	const upd = `UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`
	if err := db.Exec(upd); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Run the same statement twice to check the cache.
	if err := db.Exec(`UPDATE b SET v='z' WHERE k=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(upd); err != nil {
		t.Fatalf("second: %v", err)
	}
	got := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log ORDER BY rowid`))
	want := []string{"1:a->A", "1:z->A"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("double execution logged %v, want %v", got, want)
	}
}

// TestViewUpdateFromEngineDirectAnswers verifies correct behavior for various
// UPDATE ... FROM shapes against the oracle.
func TestViewUpdateFromEngineDirectAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		// Affinity pass on view columns.
		{"new-affinity-applied",
			[]string{`CREATE TABLE b(t TEXT, n INTEGER)`, `CREATE VIEW v1 AS SELECT t,n FROM b`,
				`CREATE TABLE m(k)`, `CREATE TABLE log(a,ty)`,
				`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.t, typeof(new.t)); END`,
				`INSERT INTO b VALUES('x',1)`, `INSERT INTO m VALUES(1)`},
			`UPDATE v1 SET t=5 FROM m WHERE m.k=v1.n`,
			`SELECT a,ty FROM log`, []string{"5,text"}},

		// Simultaneous assignment (SET x=y, y=x).
		{"simultaneous-set-swap",
			[]string{`CREATE TABLE b(x,y)`, `CREATE VIEW v1 AS SELECT x,y FROM b`, `CREATE TABLE m(k)`,
				`CREATE TABLE log(a,c)`,
				`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.x,new.y); END`,
				`INSERT INTO b VALUES(1,2)`, `INSERT INTO m VALUES(1)`},
			`UPDATE v1 SET x=v1.y, y=v1.x FROM m WHERE m.k=v1.x`,
			`SELECT a,c FROM log`, []string{"2,1"}},

		// OLD and NEW read from different offsets in the ephemeral.
		{"old-and-new-both-from-the-ephemeral",
			[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
				`CREATE TABLE log(a)`,
				`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.k||':'||old.v||'->'||new.v); END`,
				`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(1,'A')`},
			`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
			`SELECT a FROM log`, []string{"1:a->A"}},

		// Repeated SET target (last one wins).
		{"repeated-set-target",
			[]string{`CREATE TABLE b(x,y)`, `CREATE VIEW v1 AS SELECT x,y FROM b`, `CREATE TABLE m(k,nv)`,
				`CREATE TABLE log(a)`,
				`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.y); END`,
				`INSERT INTO b VALUES(1,0)`, `INSERT INTO m VALUES(1,'m')`},
			`UPDATE v1 SET y=1, y=m.nv FROM m WHERE m.k=v1.x`,
			`SELECT a FROM log`, []string{"m"}},

		// Join matching nothing fires nothing.
		{"no-match-fires-nothing",
			[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
				`CREATE TABLE log(a)`,
				`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.v); END`,
				`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(9,'Z')`},
			`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
			`SELECT count(*) FROM log`, []string{"0"}},

		// Multi-match is refused.
		{"multi-match-refused-before-any-fire",
			[]string{`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
				`CREATE TABLE log(a)`,
				`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.v); END`,
				`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(1,'X'),(1,'Y')`},
			``, // exec is expected to FAIL; see the driver below
			`SELECT count(*) FROM log`, []string{"0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			stmt := tc.stmt
			wantErr := stmt == ""
			if wantErr {
				stmt = `UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`
			}
			// Must compile for this case to be valid.
			_, cerr := db.compileWrite(stmt)
			if cerr != nil {
				t.Fatalf("compile %q: %v", stmt, cerr)
			}
			eerr := db.Exec(stmt)
			if wantErr && eerr == nil {
				t.Fatalf("%q succeeded; multi-match join should be refused", stmt)
			}
			if !wantErr && eerr != nil {
				t.Fatalf("exec %q: %v", stmt, eerr)
			}
			got := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("%q then %q:\n got %v\nwant %v", stmt, tc.query, got, tc.want)
			}
		})
	}
}

// TestViewUpdateFromBodyWritesReachTheFile verifies that trigger body writes
// are durably committed.
func TestViewUpdateFromBodyWritesReachTheFile(t *testing.T) {
	path := t.TempDir() + "/vuf.musq"
	db, err := Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE b(k,v)`,
		`CREATE VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v WHERE k=old.k; END`,
		`INSERT INTO b VALUES(1,'a'),(2,'b')`,
		`INSERT INTO m VALUES(1,'A')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close (setup): %v", err)
	}
	// Second session to isolate the update.
	db2, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	const stmt = `UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`
	_, cerr := db2.compileWrite(stmt)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if err := db2.Exec(stmt); err != nil {
		t.Fatalf("%q: %v", stmt, err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close (write): %v", err)
	}
	db3, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite (readback): %v", err)
	}
	defer db3.Discard()
	got := rvdRowStrings(rvdQuery(t, db3, `SELECT k,v FROM b ORDER BY k`))
	want := []string{"1,A", "2,b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("body write did not survive Close: got %v, want %v", got, want)
	}
}

// allSubPrograms returns every Program reachable through P4 payloads.
func allSubPrograms(prog *Program) []*Program {
	seen := map[*Program]bool{prog: true}
	var out []*Program
	var walk func(p *Program)
	walk = func(p *Program) {
		for i := range p.Insns {
			for _, sub := range subProgramsOf(p.Insns[i].P4) {
				if sub == nil || seen[sub] {
					continue
				}
				seen[sub] = true
				out = append(out, sub)
				walk(sub)
			}
		}
	}
	walk(prog)
	return out
}
