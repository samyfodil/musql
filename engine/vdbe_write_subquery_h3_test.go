package engine

// Tests for subqueries in INSERT VALUES and UPDATE SET: must compile to
// bytecode, and unhandled correlations must decline.

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

type h3CompileCase struct {
	name  string
	setup []string
	stmt  string
	// want is "bytecode" (it lowers, whole) or "error" (a hard compile error).
	// There is no third outcome: a shape this compiler cannot lower is refused,
	// never routed elsewhere (AGENTS.md Rule 1).
	want string
}

var h3CompileCases = []h3CompileCase{
	// --- promoted: an UNCORRELATED SET subquery over the target table -------
	//
	// expr.c:3889 brackets an uncorrelated subquery in OP_Once, so it runs once
	// over the pre-update image, which is exactly what this write path's frozen
	// snapshot hands it. See emitSetValue (vdbe_write.go).
	{"update set count over target", []string{`CREATE TABLE t(a,b)`},
		`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`, "bytecode"},
	{"update set sum over target", []string{`CREATE TABLE t(x)`},
		`UPDATE t SET x=(SELECT sum(x) FROM t)`, "bytecode"},
	{"update set EXISTS over target", []string{`CREATE TABLE t(a,b)`},
		`UPDATE t SET b=(SELECT EXISTS(SELECT 1 FROM t WHERE b IS NULL))`, "bytecode"},
	{"update set IN over target", []string{`CREATE TABLE t(a,b)`},
		`UPDATE t SET b=(a IN (SELECT a FROM t WHERE b=0))`, "bytecode"},
	{"update set over target, aliased target", []string{`CREATE TABLE t(a,b)`},
		`UPDATE t AS z SET b=(SELECT count(*) FROM t)`, "bytecode"},
	{"update set over target under a WHERE subquery", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE o(k)`},
		`UPDATE t SET b=(SELECT sum(b) FROM t) WHERE a IN (SELECT k FROM o)`, "bytecode"},

	// --- promoted LIVE: a CORRELATED SET subquery over the target table -----
	//
	// It is re-evaluated per row against the LIVE, partially-updated table
	// (EP_VarSelect, resolve.c:1403-1404), so it is lowered again for each row
	// (liveRowCtx, vdbe_live_read.go). The FILTER/OVER/WINDOW spellings are the
	// ones an EARLIER narrowing got WRONG by hiding the correlation from an AST
	// predicate; they bind now, and lower live like the plain one.
	{"update set correlated over target", []string{`CREATE TABLE t(x)`},
		`UPDATE t SET x=x+(SELECT count(*) FROM t t2 WHERE t2.x<=t.x)`, "bytecode"},
	{"update set nested-correlated over target", []string{`CREATE TABLE t(a,b)`},
		`UPDATE t SET b=(SELECT count(*) FROM t x WHERE x.a IN (SELECT y.a FROM t y WHERE y.a<=t.a))`, "bytecode"},
	{"update set FILTER hides the correlation", []string{`CREATE TABLE t(a,b)`},
		`UPDATE t AS o SET b=(SELECT count(*) FILTER (WHERE a<o.a) FROM t)`, "bytecode"},
	{"update set OVER hides the correlation", []string{`CREATE TABLE t(a,b)`},
		`UPDATE t AS o SET b=(SELECT count(*) OVER (PARTITION BY o.a) FROM t LIMIT 1)`, "bytecode"},
	{"update set WINDOW clause hides the correlation", []string{`CREATE TABLE t(a,b)`},
		`UPDATE t AS o SET b=(SELECT sum(a) OVER w FROM t WINDOW w AS (ORDER BY o.a) LIMIT 1)`, "bytecode"},
	{"update set table-valued-function arg hides the correlation", []string{`CREATE TABLE t(a TEXT,b)`, `CREATE TABLE u(p,q,r)`},
		`UPDATE t AS o SET b=(SELECT count(*) FROM t, pragma_table_info(o.a))`, "bytecode"},
	{"update set correlated over target behind a WITH", []string{`CREATE TABLE t(a,b)`},
		`WITH c AS (SELECT 1) UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a)`, "bytecode"},
	// A one-pass WHERE an index could drive: the ported planner says which
	// order update.c walks (DB.updateOnePassOrder) -- here rowid, since the SET
	// updates that index and C falls back to two passes.
	{"update set correlated over target, indexed one-pass WHERE", []string{`CREATE TABLE t(a,b)`, `CREATE INDEX tb ON t(b)`},
		`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE b>0`, "bytecode"},
	// The same after ANALYZE, through an index the SET leaves alone: the
	// ported planner prices sqlite_stat1 now (where_plan_stat1.go).
	{"update set correlated over target, indexed one-pass WHERE after ANALYZE", []string{`CREATE TABLE t(a,b,c)`, `CREATE INDEX tc ON t(c)`, `INSERT INTO t VALUES(1,1,1)`, `ANALYZE`},
		`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE c>0`, "bytecode"},
	// A LIKE beside it is priced too (where_plan_like.go).
	{"update set correlated over target, indexed one-pass WHERE with LIKE", []string{`CREATE TABLE t(a,b,c)`, `CREATE INDEX tc ON t(c)`},
		`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE c>0 AND a LIKE 'x%'`, "bytecode"},
	// Still refused: a LIKE whose pattern is a bound parameter, which C plans
	// with the value bound at the time and re-prepares on a change -- so the
	// visit order is not known at this compile.
	{"update set correlated over target, indexed one-pass WHERE with LIKE ?", []string{`CREATE TABLE t(a,b,c)`, `CREATE INDEX tc ON t(c)`},
		`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE c>0 AND a LIKE ?1`, "error"},

	// --- promoted: a subquery inside an INSERT ... VALUES tuple -------------
	{"insert values scalar subquery", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`INSERT INTO t VALUES((SELECT count(*) FROM s),'x')`, "bytecode"},
	{"insert values self-reading subquery", []string{`CREATE TABLE t(a)`},
		`INSERT INTO t VALUES((SELECT count(*) FROM t))`, "bytecode"},
	{"insert values multi-row subquery", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`INSERT INTO t VALUES((SELECT count(*) FROM s),'p'),((SELECT count(*) FROM s),'q')`, "bytecode"},
	{"insert values EXISTS and IN", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`INSERT INTO t VALUES(EXISTS(SELECT 1 FROM s), 1 IN (SELECT x FROM s))`, "bytecode"},
	{"insert values subquery behind a leading WITH", []string{`CREATE TABLE log(x)`},
		`WITH cte AS (SELECT 5 AS x) INSERT INTO log VALUES((SELECT x FROM cte))`, "bytecode"},
	{"insert values subquery, named column list", []string{`CREATE TABLE s(x)`, `CREATE TABLE t(a INTEGER, b TEXT DEFAULT 'd', c)`},
		`INSERT INTO t(a,c) VALUES((SELECT x FROM s), (SELECT x||x FROM s))`, "bytecode"},

	// H3 declined this because its guard needed a read resolved at FIRE time
	// and this compiler had only a compile-time snapshot to offer. Batch K
	// built that seam (vdbe_live_read.go) and the shape is now REAL BYTECODE;
	// engine/live_read_codegen_test.go owns the assertions that say so, and
	// compat-harness/live_read_k_test.go the oracle agreement. Kept here so the
	// row still moves if the promotion ever unwinds.
	{"subquery in trigger WHEN", []string{
		`CREATE TABLE t(a,b)`, `CREATE TABLE log(x)`, `CREATE TABLE s(x)`,
		`CREATE TRIGGER tw AFTER INSERT ON t WHEN (SELECT count(*) FROM s)>0 BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t VALUES(1,'x')`, "bytecode"},

	// A VALUES-tuple subquery BESIDE a RETURNING subquery: two subqueries in one
	// statement on two different lifetimes -- the tuple's reads the frozen
	// pre-statement snapshot (OP_Once, expr.c:3889) and the RETURNING one is
	// re-lowered live per row (EP_VarSelect, trigger.c:998). Both compile now;
	// this row moved from declined to "bytecode" when the RETURNING half was
	// promoted (see engine/returning_subquery_codegen_test.go), and it is the
	// case that proves the two do not share a pager.
	{"subquery in RETURNING beside a VALUES subquery", []string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`},
		`INSERT INTO t VALUES((SELECT count(*) FROM s),'x') RETURNING a,(SELECT count(*) FROM t)`, "bytecode"},

	// --- still declined: the shapes cluster H3 did NOT promote --------------
	// A name the subquery cannot resolve is a PREPARE-time error rather than a
	// decline routed anywhere -- which is what C SQLite does with it
	// ("no such table: nosuchtable", verified against the 3.53.3 oracle: the
	// statement fails and inserts nothing on both sides).
	{"insert values subquery over an unknown table", []string{`CREATE TABLE t(a,b)`},
		`INSERT INTO t VALUES((SELECT count(*) FROM nosuchtable),'x')`, "error"},
}

func TestH3SubqueryWriteShapesCompile(t *testing.T) {
	for _, tc := range h3CompileCases {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("[%s] create: %v", tc.name, err)
		}
		for _, s := range tc.setup {
			if err := db.Exec(s); err != nil {
				t.Fatalf("[%s] setup %q: %v", tc.name, s, err)
			}
		}
		prog, cerr := db.compileWrite(tc.stmt)
		got := "bytecode"
		switch {
		case cerr != nil:
			got = "error"
		}
		// A shape this compiler cannot lower reports "error": there is no third
		// outcome to distinguish. What these cases pin -- which shapes lower,
		// and that the rest are refused rather than mis-served -- is unchanged.
		_ = prog
		if got != tc.want {
			t.Errorf("[%s] %s\n  compiled to %s, want %s (err=%v)", tc.name, tc.stmt, got, tc.want, cerr)
		}
		db.Discard()
	}
}

// TestH3PromotedProgramsCarryTheirSnapshot verifies that WritePager and
// NSubCache are properly set in promoted INSERT programs.
func TestH3PromotedProgramsCarryTheirSnapshot(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{`CREATE TABLE t(a,b)`, `CREATE TABLE s(x)`} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}

	withSub, err := db.compileWrite(`INSERT INTO t VALUES((SELECT count(*) FROM s),'p'),((SELECT count(*) FROM s),'q')`)
	if err != nil {
		t.Fatalf("compile with subquery: %v", err)
	}
	if withSub.WritePager == nil {
		t.Error("INSERT VALUES with a subquery: WritePager is nil, so cachedWriteProgram would cache this program and answer a later statement from this run's snapshot")
	}
	if withSub.NSubCache < 2 {
		t.Errorf("INSERT VALUES with two uncorrelated subqueries: NSubCache=%d, want >= 2 (one run-once slot per tuple)", withSub.NSubCache)
	}

	plain, err := db.compileWrite(`INSERT INTO t VALUES(1,'x')`)
	if err != nil {
		t.Fatalf("compile without subquery: %v", err)
	}
	if plain.WritePager != nil {
		t.Error("INSERT VALUES with no subquery: WritePager set, which needlessly makes every plain INSERT uncacheable and pays for a whole-database image")
	}
	if plain.NSubCache != 0 {
		t.Errorf("INSERT VALUES with no subquery: NSubCache=%d, want 0", plain.NSubCache)
	}
}

// TestH3TriggerBodyValuesSubqueryReadsLive verifies that trigger body subqueries
// read the live database, not a frozen snapshot.
func TestH3TriggerBodyValuesSubqueryReadsLive(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE s(x)`,
		`CREATE TABLE log(n)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES((SELECT count(*) FROM s)); END`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	prog, cerr := db.compileWrite(`INSERT INTO t VALUES(1)`)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	body := onlyTriggerBody(t, prog)
	if programReadsFrozenSnapshot(body) {
		t.Error("the compiled trigger body reads a snapshot frozen at the FIRING statement's compile time, not the live database the body sees")
	}
}

// TestH3UpdateSetCorrelationPredicateStaysBlind verifies that the
// setSubqueryCorrelatesToRow AST predicate remains blind to certain correlation
// patterns, which is intentional since the compiler uses a different check.
func TestH3UpdateSetCorrelationPredicateStaysBlind(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{`CREATE TABLE t(a,b)`, `CREATE TABLE u(p,q,r)`, `CREATE INDEX tb ON t(b)`,
		`CREATE TABLE w(a,b,c)`, `CREATE INDEX wc ON w(c)`, `INSERT INTO w VALUES(1,1,1)`, `ANALYZE`} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}

	// The compiler does not consult the predicate at all: every SET subquery
	// over the target below COMPILES -- the TVF spelling this predicate reports
	// uncorrelated too, and an indexed one-pass WHERE whose index the SET
	// updates (rowid order whatever the plan, updateEveryIndexForcesTwoPass) --
	// and one the compiled program says cannot be served per row is refused:
	// an index-driven one-pass WHERE holding a LIKE on a bound parameter, whose
	// visit order the ported planner cannot say.
	for _, tc := range []struct {
		sql     string
		refused bool
	}{
		{`UPDATE t SET b=(SELECT count(*) FROM t WHERE b=0)`, false},
		{`UPDATE t AS o SET b=(SELECT count(*) FROM t, pragma_table_info(o.a))`, false},
		{`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE b>0`, false},
		{`UPDATE w SET b=(SELECT count(*) FROM w w2 WHERE w2.a<=w.a) WHERE c>0`, false},
		{`UPDATE w SET b=(SELECT count(*) FROM w w2 WHERE w2.a<=w.a) WHERE c>0 AND a LIKE 'x%'`, false},
		{`UPDATE w SET b=(SELECT count(*) FROM w w2 WHERE w2.a<=w.a) WHERE c>0 AND a LIKE ?1`, true},
	} {
		// tc.refused means "the compiler SEES the correlation and declines".
		// The uncorrelated case must still COMPILE -- that half is what proves
		// the compiler is not simply refusing everything.
		prog, cerr := db.compileWrite(tc.sql)
		if tc.refused {
			if cerr == nil {
				t.Errorf("%s: COMPILED, but the ported planner cannot say which order update.c walks this one-pass WHERE in", tc.sql)
			} else if !errors.Is(cerr, errVDBEUnsupported) {
				t.Errorf("%s: declined with %v, which is not errVDBEUnsupported", tc.sql, cerr)
			}
			continue
		}
		if cerr != nil {
			t.Fatalf("%s: compileWrite: %v", tc.sql, cerr)
		}
		if prog == nil {
			t.Errorf("%s: no program", tc.sql)
		}
	}

	cases := []struct {
		expr       string
		correlated bool
		why        string
	}{
		{`(SELECT count(*) FROM t WHERE b=0)`, false, "genuinely uncorrelated"},
		{`(SELECT sum(b) FROM t)`, false, "genuinely uncorrelated"},
		{`(SELECT count(*) FROM t t2 WHERE t2.a<=t.a)`, true, "genuinely correlated, and seen"},
		{`(SELECT count(*) FROM t x WHERE x.a IN (SELECT y.a FROM t y WHERE y.a<=t.a))`, true, "genuinely correlated, and seen"},
		// BLIND SPOTS. Each of these IS correlated; "false" is the walk's
		// incompleteness, pinned deliberately -- it is the state the measured
		// answers above were taken against. Changing any of them to true is a
		// change to this predicate's contract, not a bug fix; see the doc
		// comment for what was measured when the walk was completed.
		{`(SELECT count(*) FILTER (WHERE a<o.a) FROM t)`, false, "correlated via FuncExpr.Filter, never walked"},
		{`(SELECT count(*) OVER (PARTITION BY o.a) FROM t LIMIT 1)`, false, "correlated via FuncExpr.Over, never walked"},
		{`(SELECT sum(a) OVER w FROM t WINDOW w AS (ORDER BY o.a) LIMIT 1)`, false, "correlated via SelectStmt.Windows, never walked"},
		{`(SELECT count(*) FROM t, pragma_table_info(o.a))`, false, "correlated via FromItem.TableFuncArgs, never walked"},
	}
	for _, tc := range cases {
		sql := `UPDATE t AS o SET b=` + tc.expr
		stmt, err := parseUpdateStmt(sql)
		if err != nil {
			t.Fatalf("%s: parse: %v", sql, err)
		}
		e := stmt.sets[0].expr
		if !setSubqueryReadsTable(e, "t") {
			t.Fatalf("%s: setSubqueryReadsTable said the subquery does not read t, so this case tests nothing", sql)
		}
		if got := setSubqueryCorrelatesToRow(e, nil); got != tc.correlated {
			t.Errorf("%s: setSubqueryCorrelatesToRow=%v, want %v (%s)", sql, got, tc.correlated, tc.why)
		}
	}
}

// TestH3LeadingWithReachesValuesSubqueries verifies that leading WITH clauses
// are preserved on VALUES forms for subquery resolution.
func TestH3LeadingWithReachesValuesSubqueries(t *testing.T) {
	stmt, err := parseInsertStmt(`WITH cte AS (SELECT 5 AS x) INSERT INTO log VALUES((SELECT x FROM cte))`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(stmt.ctes) != 1 || !strings.EqualFold(stmt.ctes[0].Name, "cte") {
		t.Fatalf("leading WITH not kept on the VALUES form: ctes=%+v", stmt.ctes)
	}
	// The SELECT form still attaches it to the SELECT, not here.
	sel, err := parseInsertStmt(`WITH cte AS (SELECT 5 AS x) INSERT INTO log SELECT x FROM cte`)
	if err != nil {
		t.Fatalf("parse select form: %v", err)
	}
	if len(sel.ctes) != 0 {
		t.Errorf("SELECT form: leading WITH duplicated onto insertStmt.ctes (%+v); it belongs on selectStmt.CTEs alone", sel.ctes)
	}
	if sel.selectStmt == nil || len(sel.selectStmt.CTEs) != 1 {
		t.Errorf("SELECT form: leading WITH lost from selectStmt.CTEs")
	}
}

// TestH3ValuesSubqueryRunsAgainstAFreshSnapshot verifies that repeated
// executions see fresh snapshots, not a cached program's stale image.
func TestH3ValuesSubqueryRunsAgainstAFreshSnapshot(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	if err := db.Exec(`CREATE TABLE t(a)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO t VALUES(0)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := db.Exec(`INSERT INTO t VALUES((SELECT count(*) FROM t))`); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := p.Query(`SELECT a FROM t ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		if r[0].Typ != Int {
			t.Fatalf("row %d is not an integer: %+v", len(got), r[0])
		}
		got = append(got, strconv.FormatInt(r[0].I, 10))
	}
	if want := "0 1 2 3"; strings.Join(got, " ") != want {
		t.Errorf("self-reading VALUES subquery repeated: got %q, want %q -- a cached program would answer \"0 1 1 1\"", strings.Join(got, " "), want)
	}
}

// TestH3DeclinesAreUnsupportedNotFatal verifies that unsupported shapes decline
// with errVDBEUnsupported, not other error types.
func TestH3DeclinesAreUnsupportedNotFatal(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{`CREATE TABLE t(a,b,c)`, `CREATE INDEX tc ON t(c)`, `INSERT INTO t VALUES(1,1,1)`, `ANALYZE`} {
		if err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, sql := range []string{
		`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE c>0 AND a LIKE ?1`,
	} {
		_, cerr := db.compileUpdateWrite(sql)
		if cerr == nil || !errors.Is(cerr, errVDBEUnsupported) {
			t.Fatalf("%s: got %v, want an errVDBEUnsupported decline", sql, cerr)
		}
	}
	// The WORDING is the assertion, not decoration: it is emitSetValue's own
	// guard. Without this, that guard could be deleted and every other test
	// here would still pass -- code no test can distinguish is where the next
	// regression hides.
	_, cerr := db.compileUpdateWrite(`UPDATE t SET b=(SELECT count(*) FROM t t2 WHERE t2.a<=t.a) WHERE c>0 AND a LIKE ?1`)
	if cerr == nil || !strings.Contains(cerr.Error(), "UPDATE SET subquery over the target table") {
		t.Errorf("deferred-argument SET subquery declined by the wrong guard: %v", cerr)
	}
}
