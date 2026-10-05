// Oracle tests for UPDATE <view> ... FROM operations. Tests that the
// promotion did not break anything by comparing answers against the oracle.
package compat

import (
	"fmt"
	"testing"
)

var viewUpdateFromCases = []struct {
	name  string
	stmts []string
}{
	// The census shape, verbatim from engine/vdbe_total_test.go.
	{"census-shape", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v WHERE k=old.k; INSERT INTO log VALUES(old.k||':'||old.v||'->'||new.v); END`,
		`INSERT INTO b VALUES(1,'a'),(2,'b'),(3,'c')`,
		`INSERT INTO m VALUES(3,'C'),(1,'A')`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT k,v FROM b ORDER BY k`,
		// ORDER BY x, not "ORDER BY rowid": the ORDER the fires run in is the
		// query PLAN's, and musql's planner is not SQLite's. See
		// TestViewUpdateFromFireOrderFollowsThePlan below, which measures that
		// divergence and pins it as PRE-EXISTING.
		`SELECT x FROM log ORDER BY x`,
		`SELECT changes(), total_changes()`,
	}},
	// A join that matches NOTHING: the ephemeral is empty and nothing fires.
	{"no-match", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.v); END`,
		`INSERT INTO b VALUES(1,'a')`,
		`INSERT INTO m VALUES(9,'Z')`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT count(*) FROM log`,
		`SELECT changes()`,
	}},
	// Simultaneous assignment: both right-hand sides are select-list items of
	// pass one, so "SET x=y, y=x" swaps rather than chains.
	{"simultaneous-set-swap", []string{
		`CREATE TABLE b(x,y)`, `CREATE VIEW v1 AS SELECT x,y FROM b`, `CREATE TABLE m(k)`,
		`CREATE TABLE log(a,c)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.x,new.y); END`,
		`INSERT INTO b VALUES(1,2)`,
		`INSERT INTO m VALUES(1)`,
		`UPDATE v1 SET x=v1.y, y=v1.x FROM m WHERE m.k=v1.x`,
		`SELECT a,c FROM log`,
	}},
	// A repeated SET target -- last one wins (update.c:492's aXRef fill).
	{"repeated-set-target", []string{
		`CREATE TABLE b(x,y)`, `CREATE VIEW v1 AS SELECT x,y FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.y); END`,
		`INSERT INTO b VALUES(1,0)`, `INSERT INTO m VALUES(1,'m')`,
		`UPDATE v1 SET y=1, y=m.nv FROM m WHERE m.k=v1.x`,
		`SELECT a FROM log`,
	}},
	// The view's own column AFFINITY is applied to NEW, unguarded
	// (update.c:983 vs insert.c:1490-1491's "if( !isView )") -- so a
	// TEXT-affinity column turns the integer 5 into '5' and a "WHEN new.t='5'"
	// guard fires.
	{"new-affinity-applied", []string{
		`CREATE TABLE b(t TEXT, n INTEGER)`, `CREATE VIEW v1 AS SELECT t,n FROM b`,
		`CREATE TABLE m(k)`, `CREATE TABLE log(a,ty)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.t, typeof(new.t)); END`,
		`INSERT INTO b VALUES('x',1)`, `INSERT INTO m VALUES(1)`,
		`UPDATE v1 SET t=5 FROM m WHERE m.k=v1.n`,
		`SELECT a,ty FROM log`,
	}},
	// A SUBQUERY SET expression: pass one is an ordinary SELECT, so this is
	// compiled by the plain subquery machinery (update.c has no UPDATE-specific
	// handling at all).
	{"aggregate-set-expression", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(nv)`,
		`CREATE TABLE log(a)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.v); END`,
		`INSERT INTO b VALUES(1,0)`, `INSERT INTO m VALUES(10),(20),(30)`,
		`UPDATE v1 SET v=(SELECT sum(nv) FROM m) FROM m WHERE m.nv=10`,
		`SELECT a FROM log`,
	}},
	// A leading WITH clause naming a CTE as the FROM source.
	{"with-cte-from-source", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TABLE log(a,c)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.k,new.v); END`,
		`INSERT INTO b VALUES(1,'a'),(3,'c')`,
		`WITH data(dk,dv) AS (VALUES(3,'thirty'),(1,'ten')) UPDATE v1 SET v=dv FROM data WHERE k=dk`,
		`SELECT a,c FROM log ORDER BY a`,
	}},
	// An outer join INSIDE the FROM clause: the view is comma-joined after it,
	// so it can never be null-extended.
	{"left-join-inside-from", []string{
		`CREATE TABLE b(a,c1,c2)`, `CREATE VIEW v1 AS SELECT a,c1,c2 FROM b`,
		`CREATE TABLE m1(x,y)`, `CREATE TABLE m2(u,w)`, `CREATE TABLE log(p,q)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.c1,new.c2); END`,
		`INSERT INTO b VALUES(1,0,0)`, `INSERT INTO m1 VALUES(1,'Y')`,
		`UPDATE v1 SET c1=y, c2=w FROM m1 LEFT JOIN m2 ON (u=x) WHERE x=a`,
		`SELECT p,q,typeof(q) FROM log`,
	}},
	// An "AS <alias>" on the view target, qualified through the alias.
	{"target-alias", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a,c)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.k,new.v); END`,
		`INSERT INTO b VALUES(1,'a'),(2,'b')`, `INSERT INTO m VALUES(2,'B')`,
		`UPDATE v1 AS z SET v=m.nv FROM m WHERE m.k=z.k`,
		`SELECT a,c FROM log`,
	}},
	// RAISE(IGNORE) in the body abandons that row and the loop continues --
	// update.c:985's ignoreJump is labelContinue, not labelBreak.
	{"body-raise-ignore-continues", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN
		   SELECT CASE WHEN old.k=1 THEN RAISE(IGNORE) END;
		   INSERT INTO log VALUES(old.k); END`,
		`INSERT INTO b VALUES(1,'a'),(2,'b')`, `INSERT INTO m VALUES(1,'A'),(2,'B')`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT a FROM log ORDER BY a`,
	}},
	// The body writes the BASE table, and OLD stays the pre-statement image for
	// the whole statement (the third of the pre-promotion measurements).
	{"old-stays-pre-statement", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN
		   UPDATE b SET v=new.v WHERE k=old.k;
		   INSERT INTO log VALUES(old.k||':'||old.v||'->'||new.v); END`,
		`INSERT INTO b VALUES(1,'p'),(2,'q')`, `INSERT INTO m VALUES(1,'P'),(2,'Q')`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT a FROM log ORDER BY a`,
		`SELECT k,v FROM b ORDER BY k`,
	}},
	// A view over a JOIN, so the materialization pass one runs is not a trivial
	// one-table scan.
	{"view-over-a-join", []string{
		`CREATE TABLE b1(k,v)`, `CREATE TABLE b2(k,w)`,
		`CREATE VIEW v1 AS SELECT b1.k AS k, b1.v AS v, b2.w AS w FROM b1 JOIN b2 ON b2.k=b1.k`,
		`CREATE TABLE m(k,nv)`, `CREATE TABLE log(a,c,d)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.k,new.v,new.w); END`,
		`INSERT INTO b1 VALUES(1,'a'),(2,'b')`, `INSERT INTO b2 VALUES(1,'W1'),(2,'W2')`,
		`INSERT INTO m VALUES(2,'B')`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT a,c,d FROM log`,
	}},
	// A SET expression mixing both sides of the join.
	{"set-mixes-both-sides", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.v); END`,
		`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(1,'M')`,
		`UPDATE v1 SET v=v1.v||'/'||m.nv FROM m WHERE m.k=v1.k`,
		`SELECT a FROM log`,
	}},
	// NULLs on both sides, and a view column with no declared type.
	{"nulls-through-the-join", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a,ty)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.v, typeof(new.v)); END`,
		`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(1,NULL)`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT a,ty FROM log`,
	}},
	// A view that is a bare SELECT with an explicit column list on the CREATE,
	// so viewColumnInfos' names and the star expansion's have to agree.
	{"declared-column-list", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1(p,q) AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a,c)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.p,new.q); END`,
		`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(1,'A')`,
		`UPDATE v1 SET q=m.nv FROM m WHERE m.k=v1.p`,
		`SELECT a,c FROM log`,
	}},
	// A view with NO matching INSTEAD OF trigger: sqlite3IsReadOnly rejects it
	// at prepare time (delete.c:124-130), asked BEFORE update.c:457 allocates a
	// VM -- so this must error and leave changes() alone, FROM clause or not.
	{"no-instead-of-trigger", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`INSERT INTO b VALUES(1,'a'),(2,'b'),(3,'c')`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT changes()`,
	}},
	// A SET target that is not a view column: update.c:500's prepare-time "no
	// such column".
	{"set-target-not-a-view-column", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v; END`,
		`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(1,'A')`,
		`UPDATE v1 SET nosuchcol=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT k,v FROM b`,
	}},
	// A schema-qualified view target. The qualifier PICKS A CATALOG, and pass
	// one's own FROM item has to resolve in the same one the write target
	// resolved in -- see compileViewMaterialize's identical carry and its
	// "fromItemScope and writeTargetScope are ONE function" note.
	{"schema-qualified-target", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a,c)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.k,new.v); END`,
		`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(1,'A')`,
		`UPDATE main.v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT a,c FROM log`,
	}},
	// A TEMP view shadowing a MAIN one of the same name: "main.v1" must write
	// MAIN's, and both the target lookup and pass one's FROM item have to agree
	// about that (tkt2817.test's rule for a table target).
	{"schema-qualified-target-over-a-temp-shadow", []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TEMP TABLE tb(k,v)`, `CREATE TEMP VIEW v1 AS SELECT k,v FROM tb`,
		`CREATE TABLE m(k,nv)`, `CREATE TABLE log(a,c)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON main.v1 BEGIN INSERT INTO log VALUES('main:'||old.k,new.v); END`,
		`CREATE TEMP TRIGGER tvu INSTEAD OF UPDATE ON temp.v1 BEGIN INSERT INTO log VALUES('temp:'||old.k,new.v); END`,
		`INSERT INTO b VALUES(1,'a')`, `INSERT INTO tb VALUES(9,'z')`,
		`INSERT INTO m VALUES(1,'A'),(9,'Z')`,
		`UPDATE main.v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT a,c FROM log ORDER BY a`,
	}},
	// The TABLE twin of the shadow case: buildUpdateFromSelect builds the same
	// synthetic join for an ordinary target, so it has to carry the qualifier
	// for the same reason (tkt2817.test).
	{"table-schema-qualified-target-over-a-temp-shadow", []string{
		`CREATE TABLE t(k,v)`, `CREATE TEMP TABLE t(k,v)`, `CREATE TABLE m(k,nv)`,
		`INSERT INTO main.t VALUES(1,'a')`, `INSERT INTO temp.t VALUES(9,'z')`,
		`INSERT INTO m VALUES(1,'A'),(9,'Z')`,
		`UPDATE main.t SET v=m.nv FROM m WHERE m.k=t.k`,
		`SELECT k,v FROM main.t ORDER BY k`,
		`SELECT k,v FROM temp.t ORDER BY k`,
	}},
	// The whole shape inside a TRIGGER BODY, which takes the LIVE pass-one
	// route (compileLiveSubProgram) rather than the frozen one.
	{"inside-a-trigger-body", []string{
		`CREATE TABLE fire(z)`, `CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`,
		`CREATE TABLE m(k,nv)`, `CREATE TABLE log(a)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.k||'->'||new.v); END`,
		`CREATE TRIGGER tf AFTER INSERT ON fire BEGIN UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k; END`,
		`INSERT INTO b VALUES(1,'a')`, `INSERT INTO m VALUES(1,'A')`,
		`INSERT INTO fire VALUES(1)`,
		`SELECT a FROM log`,
	}},
}

func TestViewUpdateFromCompiledAnswers(t *testing.T) {
	for _, c := range viewUpdateFromCases {
		t.Run(c.name, func(t *testing.T) { differ(t, c.name, c.stmts) })
	}
}

// TestViewUpdateFromBareAggregateSetIsAPreExistingGap records a WRONG ANSWER
// this promotion neither caused nor cured, found by mutation-testing it.
//
// "UPDATE v1 SET v=sum(m.nv) FROM m" is answered by the 3.53.3 oracle (60 over
// m=10,20,30) and ERRORS on both musql routes. The root cause is measured and
// it is in the READ compiler, not in either write path: pass one is
// "SELECT v1.*, sum(m.nv) FROM m, v1", and musql declines every spelling of
// it --
//
//	SELECT v1.*, sum(m.nv) FROM m, v1        -> `"*" with an aggregate`
//	SELECT v1.k, v1.v, sum(m.nv) FROM m, v1  -> "a bare column ... in an
//	    aggregate query over a multi-table FROM clause whose JOIN LOOP ORDER
//	    the ported planner declined"
//
// so replacing the star with one column reference per view column -- which is
// what update.c:243-247 actually codes (exprRowColumn per column, no star) --
// does not fix it either. Closing this is a read-path aggregate-anchor job.
//
// Pinned rather than left silent so the gap is attributable: if the read
// compiler later serves it, this test says so and the case moves up into
// viewUpdateFromCases.
func TestViewUpdateFromBareAggregateSetIsAPreExistingGap(t *testing.T) {
	stmts := []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(nv)`,
		`CREATE TABLE log(a)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.v); END`,
		`INSERT INTO b VALUES(1,0)`, `INSERT INTO m VALUES(10),(20),(30)`,
		`UPDATE v1 SET v=sum(m.nv) FROM m`,
		`SELECT a FROM log`,
	}
	if got := run(t, "musql", stmts)[7]["kind"]; got != "error" {
		t.Fatalf("the bare-aggregate view UPDATE ... FROM reported %v, not an error -- the read\n"+
			"compiler's aggregate-anchor decline is gone, so fold this into viewUpdateFromCases", got)
	}
	if got := run(t, "cgo", stmts)[7]["kind"]; got != "rows" {
		t.Fatalf("the oracle no longer answers it either (%v); re-derive this gap before trusting the note", got)
	}
}

// TestViewUpdateFromFireOrderFollowsThePlan records the ONE thing that differs
// from the oracle here, and records it as the PRE-EXISTING divergence it is:
// measured before the promotion and after it, musql produces the same two
// fires in the same two orders either way.
//
// A view's pass-one ephemeral is SRT_Table (update.c:247), so nothing collapses
// and nothing is keyed -- pass two walks the join's rows in the order the
// PLANNER produced them. The pre-promotion route's own doc comment recorded
// the 3.53.3 measurement it was written against (m outer, t1 inner: "SCAN m |
// SEARCH t1" in five physical configurations), and this fixture is the shape
// that flips it: m holds fewer rows than the view's base, and the oracle scans
// the VIEW and searches m instead. musql's join planner does not make the same
// choice, so the fires land in the other order.
//
// This is NOT the multi-match ambiguity below (each view row here matches
// exactly one join row, so the RESULT -- b's contents -- is identical and
// order-independent, which is why the case in the table above reads its log
// with ORDER BY). Only the observable ORDER of the trigger bodies' own side
// effects differs, which SQLite itself leaves to the plan.
func TestViewUpdateFromFireOrderFollowsThePlan(t *testing.T) {
	stmts := []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(old.k); END`,
		`INSERT INTO b VALUES(1,'a'),(2,'b'),(3,'c')`,
		`INSERT INTO m VALUES(3,'C'),(1,'A')`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT x FROM log ORDER BY rowid`,
	}
	got, oracle := run(t, "musql", stmts)[8]["rows"], run(t, "cgo", stmts)[8]["rows"]
	// The SET of fires must agree; only the order may not.
	gotSorted, oracleSorted := run(t, "musql", append(stmts[:8:8], `SELECT x FROM log ORDER BY x`))[8]["rows"],
		run(t, "cgo", append(stmts[:8:8], `SELECT x FROM log ORDER BY x`))[8]["rows"]
	if fmt.Sprint(gotSorted) != fmt.Sprint(oracleSorted) {
		t.Fatalf("the SET of INSTEAD OF fires differs from the oracle, which is not a plan-order\n"+
			"question at all:\n  cgo:    %v\n  musql: %v", oracleSorted, gotSorted)
	}
	if fmt.Sprint(got) == fmt.Sprint(oracle) {
		t.Logf("the fire orders now AGREE (%v). If that is stable, fold this fixture back into\n"+
			"viewUpdateFromCases with an ORDER BY rowid read and delete this test.", got)
	}
}

// TestViewUpdateFromMultiMatchStillRefused pins the one shape neither route
// answers, as the PRE-EXISTING refusal it is.
//
// Two join rows carrying the same view row both fire the INSTEAD OF trigger in
// C SQLite (the view's pass-one ephemeral is SRT_Table, so nothing is
// collapsed -- update.c:247), and which one lands last is the query plan's
// choice. The route this promotion replaced refused it, and
// checkViewUpfromRows refuses it now, with the SAME message; what is pinned
// here is that the promotion did not turn a refusal into a guess.
func TestViewUpdateFromMultiMatchStillRefused(t *testing.T) {
	stmts := []string{
		`CREATE TABLE b(k,v)`, `CREATE VIEW v1 AS SELECT k,v FROM b`, `CREATE TABLE m(k,nv)`,
		`CREATE TABLE log(a)`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN INSERT INTO log VALUES(new.v); END`,
		`INSERT INTO b VALUES(1,'a')`,
		`INSERT INTO m VALUES(1,'X'),(1,'Y')`,
		`UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k`,
		`SELECT count(*) FROM log`,
	}
	res := run(t, "musql", stmts)
	if got := res[7]["kind"]; got != "error" {
		t.Fatalf("the multi-match UPDATE reported %v, not an error -- a compiled answer here is "+
			"a guess at which of two fires the planner ran last", got)
	}
	// The refusal must not have leaked a fire: nothing may have run.
	if rows, _ := res[8]["rows"].([]any); len(rows) != 1 {
		t.Fatalf("read-back after the refusal returned %v rows, want 1", len(rows))
	} else if row, _ := rows[0].([]any); len(row) != 1 || row[0] != "I:0" {
		t.Fatalf("the refused UPDATE still fired the trigger: log holds %v, want 0 rows", row)
	}
}
