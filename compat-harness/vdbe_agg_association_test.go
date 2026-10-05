// Test aggregate re-association rules in SELECT, HAVING, and ORDER BY clauses.
// Verify declined statements would produce wrong row counts if evaluated per row.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// buildAggAssocDB creates a test fixture with t1 (3 rows) and t2 (2 rows).
func buildAggAssocDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aggassoc.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t1(a1 INTEGER)`,
		`INSERT INTO t1 VALUES (1), (2), (3)`,
		`CREATE TABLE t2(b1 INTEGER)`,
		`INSERT INTO t2 VALUES (4), (5)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// aggAssocDecline is one statement whose aggregate re-associates outward, plus
// the number of rows C SQLite returns for it. wantRows is always 1 (the
// re-associated aggregate makes the enclosing three-row query produce a single
// row); the engine's pre-fix answer was always 3. clause names which of the
// four aggregate-owning clauses the aggregate is written in.
type aggAssocDecline struct {
	clause   string
	sql      string
	wantRows int
}

var aggAssocDeclines = []aggAssocDecline{
	// HAVING of a whole-table aggregate subquery. Planned AFTER
	// planNoGroupAggregate returned, so it is checked by compileScanAggregate's
	// own direct checkAggregateAssociation call -- the one aggregate-owning
	// clause the outward hoist (engine/vdbe_agg_hoist.go) does not route
	// through, so these still decline.
	{"no-GROUP-BY HAVING", `SELECT (SELECT sum(b1) FROM t2 HAVING sum(a1)>0) FROM t1`, 1},
	{"no-GROUP-BY HAVING", `SELECT (SELECT count(*) FROM t2 HAVING sum(a1)>0) FROM t1`, 1},
}

// aggAssocHoisted is what those declines became once the OUTWARD half of the
// rule landed (engine/vdbe_agg_hoist.go): the aggregate really is re-associated
// with the enclosing query, so the statement really is ONE row -- and now this
// engine produces it. The row count these entries were written to protect is
// still asserted, but against the ANSWER rather than against a refusal, and the
// cells are compared too, which the decline gate could never do.
var aggAssocHoisted = []aggAssocDecline{
	// Select list of a whole-table aggregate subquery.
	{"no-GROUP-BY select list", `SELECT (SELECT sum(a1) FROM t2) FROM t1`, 1},
	{"no-GROUP-BY select list", `SELECT (SELECT group_concat(a1,'x') FROM t2) FROM t1`, 1},
	// ...even when a SECOND aggregate in the same item is legitimately local:
	// sum(b1) stays with t2, sum(a1) leaves, and the whole statement is one row.
	{"no-GROUP-BY select list", `SELECT (SELECT sum(b1)+sum(a1) FROM t2) FROM t1`, 1},

	// ORDER BY of a GROUP BY subquery. It was in the DECLINE list above, on the
	// reasoning that planGroupByStmt's ORDER BY item plans are checked by
	// compileScanAggregate's own checkAggregateAssociation rather than routed
	// through the outward hoist. The hoist reaches it now: the item compiles,
	// the aggregate really is the enclosing query's, and the whole statement is
	// the ONE row C SQLite answers -- compared cell by cell here, which the
	// decline gate could not do.
	{"GROUP BY ORDER BY", `SELECT (SELECT b1 FROM t2 GROUP BY b1 ORDER BY sum(a1)) FROM t1`, 1},

	// Select list of a GROUP BY subquery: the BODY groups by b1 and returns its
	// first group, while the hoisted aggregate is the enclosing query's.
	{"GROUP BY select list", `SELECT (SELECT sum(a1) FROM t2 GROUP BY b1) FROM t1`, 1},
	{"GROUP BY select list", `SELECT (SELECT max(a1) FROM t2 GROUP BY b1) FROM t1`, 1},
	{"GROUP BY select list", `SELECT (SELECT sum(a1)+sum(b1) FROM t2 GROUP BY b1) FROM t1`, 1},

	// HAVING of a GROUP BY subquery, folded into the same groupPlanRefs union.
	{"GROUP BY HAVING", `SELECT (SELECT sum(b1) FROM t2 GROUP BY b1 HAVING sum(a1)>2) FROM t1`, 1},
}

// TestAggAssociationReassociationDeclines is the hard gate: every statement
// above must DECLINE on both the QueryVDBE and integrated p.Query paths, and
// C SQLite must answer it with the row count that proves the decline is
// covering a genuine re-association (one row, not the enclosing query's three).
func TestAggAssociationReassociationDeclines(t *testing.T) {
	path := buildAggAssocDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, tc := range aggAssocHoisted {
		// These ANSWER now, and must match the oracle exactly -- the row count
		// this list exists for, and every cell.
		cCols, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", tc.sql, cErr)
			continue
		}
		if len(cRows) != tc.wantRows {
			t.Errorf("[%s] C SQLite returned %d row(s), the re-association premise says %d -- this gate's oracle evidence no longer holds",
				tc.sql, len(cRows), tc.wantRows)
			continue
		}
		vCols, vVals, vErr := p.QueryArgs(tc.sql, nil)
		if vErr != nil {
			t.Errorf("[%s / %s] QueryVDBE declined a re-associated aggregate C SQLite answers with %d row(s): %v", tc.clause, tc.sql, len(cRows), vErr)
			continue
		}
		if len(vVals) != len(cRows) {
			t.Errorf("[%s / %s] QueryVDBE returned %d row(s), C SQLite %d -- a wrong ROW COUNT", tc.clause, tc.sql, len(vVals), len(cRows))
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: rows=%d %v\n  cgo:  rows=%d %v",
				tc.sql, reason, len(vVals), engineRowsToStrings(vVals), len(cRows), cRows)
			continue
		}
		eCols, eVals, eErr := p.Query(tc.sql)
		if eErr != nil {
			t.Errorf("[%s / %s] p.Query declined a re-associated aggregate C SQLite answers: %v", tc.clause, tc.sql, eErr)
			continue
		}
		if len(eVals) != len(cRows) {
			t.Errorf("[%s / %s] p.Query returned %d row(s), C SQLite %d -- a wrong ROW COUNT", tc.clause, tc.sql, len(eVals), len(cRows))
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: rows=%d %v\n  cgo:    rows=%d %v",
				tc.sql, reason, len(eVals), engineRowsToStrings(eVals), len(cRows), cRows)
		}
	}

	for _, tc := range aggAssocDeclines {
		// The oracle half: prove this really is a re-association. If real
		// SQLite ever stopped collapsing these to one row the decline would be
		// covering nothing, and this gate would be pinning a phantom.
		_, cRows, cErr := cgoSelect(t, cdb, tc.sql, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", tc.sql, cErr)
			continue
		}
		if len(cRows) != tc.wantRows {
			t.Errorf("[%s] C SQLite returned %d row(s), the re-association premise says %d -- this gate's oracle evidence no longer holds",
				tc.sql, len(cRows), tc.wantRows)
			continue
		}
		// The engine half: it must not answer at all. Answering per enclosing
		// row would give 3 rows here -- a wrong ROW COUNT, silently.
		if _, rows, err := p.QueryArgs(tc.sql, nil); err == nil {
			t.Errorf("[%s / %s] QueryVDBE answered %d row(s) where the aggregate re-associates to the enclosing query (C SQLite: %d row(s)) -- must decline",
				tc.clause, tc.sql, len(rows), len(cRows))
		}
		if _, rows, err := p.Query(tc.sql); err == nil {
			t.Errorf("[%s / %s] p.Query answered %d row(s) where the aggregate re-associates to the enclosing query -- must decline",
				tc.clause, tc.sql, len(rows))
		}
	}
}

// aggAssocServed is the other half of the rule, and the reason the check cannot
// simply reject every aggregate that mentions an outer column: SQLite's
// sqlite3ReferencesSrcList returns 1 (aggregate stays here) as soon as ONE
// referenced column is local, and -1 (also stays here) when the argument names
// no table at all or only tables of its own nested subqueries. Both must keep
// ANSWERING, byte-identically to the oracle.
var aggAssocServed = []string{
	// 1: one local column beside the correlated one -- the aggregate is t2's.
	`SELECT (SELECT group_concat(b1, a1) FROM t2) FROM t1 ORDER BY t1.rowid`,
	`SELECT (SELECT sum(a1 + b1) FROM t2) FROM t1 ORDER BY t1.rowid`,
	// -1: no column reference at all.
	`SELECT (SELECT count(*) FROM t2) FROM t1 ORDER BY t1.rowid`,
	`SELECT (SELECT sum(1) FROM t2) FROM t1 ORDER BY t1.rowid`,
	// -1 via a nested subquery of the argument itself: b1 belongs to the
	// argument's OWN FROM, which selectRefEnter excludes, so the aggregate
	// stays with the (row-per-outer-row) subquery.
	`SELECT (SELECT total((SELECT b1 FROM t2)) FROM t2) FROM t1 ORDER BY t1.rowid`,
	// A GROUP BY subquery whose aggregate is entirely local still answers.
	`SELECT (SELECT sum(b1) FROM t2 GROUP BY b1) FROM t1 ORDER BY t1.rowid`,
	// Ordinary whole-table and GROUP BY aggregates, unaffected.
	`SELECT sum(a1) FROM t1`,
	`SELECT b1, sum(b1) FROM t2 GROUP BY b1 ORDER BY b1`,
	`SELECT sum(b1) FROM t2 HAVING sum(b1)>0`,
	`SELECT b1 FROM t2 GROUP BY b1 HAVING sum(b1)>4 ORDER BY sum(b1)`,
}

// TestAggAssociationLocalAggregatesStillAnswer is the anti-over-decline gate:
// the check must not turn a locally-owned aggregate into a decline. Every
// statement must RUN on both paths and match C SQLite exactly, rows and
// row count alike.
func TestAggAssociationLocalAggregatesStillAnswer(t *testing.T) {
	path := buildAggAssocDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range aggAssocServed {
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
			continue
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("[%s] QueryVDBE declined where C SQLite answered %d row(s): %v", sqlText, len(cRows), vErr)
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: rows=%d %v\n  cgo:  rows=%d %v",
				sqlText, reason, len(vVals), engineRowsToStrings(vVals), len(cRows), cRows)
			continue
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			t.Errorf("[%s] p.Query declined where C SQLite answered: %v", sqlText, eErr)
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: rows=%d %v\n  cgo:    rows=%d %v",
				sqlText, reason, len(eVals), engineRowsToStrings(eVals), len(cRows), cRows)
		}
	}
}

// ---------------------------------------------------------------------------
// The other half of aggnested's bucket: an UNQUALIFIED correlated reference
// out of a subquery written in an AGGREGATE query's select list / HAVING /
// ORDER BY. Nothing to do with re-association -- the aggregate here belongs
// exactly where it is written; what was missing was the SCOPE REACH.
//
// A subquery reached through evalSubquery/evalExists/evalIn compiles standalone
// (no enclosing *compiler), so the only thing that used to make an enclosing
// column visible was materializeOuterRefs, which matches on
// the FROM-item NAME and therefore substituted QUALIFIED references only. Real
// SQLite draws no such distinction: lookupName walks out through the
// NameContext chain either way. engine/vdbe_scan.go's compileSelectScanRow now
// hands the live enclosing row to compileColumn as its documented last resort
// (compiler.rowOuter), so the unqualified spelling resolves exactly where the
// qualified one already did -- AFTER the subquery's own fully-expanded scopes,
// which is what keeps shadowing right.
// ---------------------------------------------------------------------------

// buildAggUnqualDB gives the inner query its OWN column named "a" (tc.a) so the
// shadowing rule is exercised, not just the reach: a reference the subquery's
// own FROM can satisfy must bind THERE, never to the enclosing row.
func buildAggUnqualDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aggunqual.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE ta(a,b)`,
		`INSERT INTO ta VALUES (1,10), (2,20), (1,30)`,
		`CREATE TABLE tb(c,d)`,
		`INSERT INTO tb VALUES (1,'one'), (2,'two')`,
		`CREATE TABLE tc(a,e)`,
		`INSERT INTO tc VALUES (1,'x'), (9,'y')`,
		`CREATE TABLE td(a,f)`,
		`INSERT INTO td VALUES (1,'p'), (2,'q')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// aggUnqualServed must all ANSWER, matching the oracle cell-for-cell.
var aggUnqualServed = []string{
	// subquery.test-4.1 shape: the outer GROUP BY key, referenced unqualified
	// from a correlated subquery in the select list. This is the statement the
	// bucket is named for; it declined with "no such column: a".
	`SELECT a, (SELECT d FROM tb WHERE a=c) FROM ta GROUP BY a ORDER BY a`,
	// ...and the whole-table aggregate spelling of the same thing.
	`SELECT max(a), (SELECT d FROM tb WHERE a=c) FROM ta`,
	// The bare column need not be in the select list at all: the anchor row
	// supplies it either way.
	`SELECT max(b), (SELECT d FROM tb WHERE a=c) FROM ta GROUP BY a ORDER BY a`,
	`SELECT a, (SELECT count(*) FROM tb WHERE c=a) FROM ta GROUP BY a ORDER BY a`,
	// The qualified spelling already worked; it must keep working, and the two
	// must agree with each other.
	`SELECT a, (SELECT d FROM tb WHERE ta.a=c) FROM ta GROUP BY a ORDER BY a`,
	// SHADOWING: tc has its own "a". "WHERE a=1" must bind to tc.a (both rows
	// then read tc's row 1, 'x'), NOT to the enclosing group key -- which would
	// answer 'x' then NULL.
	`SELECT a, (SELECT e FROM tc WHERE a=1) FROM ta GROUP BY a ORDER BY a`,
	`SELECT max(a), (SELECT e FROM tc WHERE a=9) FROM ta`,
	// Select-list position, not just WHERE: "SELECT a FROM tb" has no local a.
	`SELECT a, (SELECT a FROM tb LIMIT 1) FROM ta GROUP BY a ORDER BY a`,
	`SELECT max(b), (SELECT b FROM tb LIMIT 1) FROM ta`,
	// Two levels out.
	`SELECT a, (SELECT (SELECT d FROM tb WHERE c=a) FROM tb LIMIT 1) FROM ta GROUP BY a ORDER BY a`,
	// HAVING and ORDER BY of the GROUP BY query, and the EXISTS/IN spellings.
	`SELECT a FROM ta GROUP BY a HAVING EXISTS(SELECT 1 FROM tb WHERE c=a) ORDER BY a`,
	`SELECT a FROM ta GROUP BY a HAVING a IN (SELECT c FROM tb WHERE c=a) ORDER BY a`,
	`SELECT a, max(b) FROM ta GROUP BY a ORDER BY (SELECT d FROM tb WHERE c=a) DESC`,
	// Row mode, which already worked: the reach must not have changed it.
	`SELECT a, (SELECT d FROM tb WHERE c=a) FROM ta ORDER BY ta.rowid`,
}

// aggUnqualRejected must still be REJECTED BY BOTH engines: reaching the
// enclosing row must not invent a resolution SQLite itself refuses.
var aggUnqualRejected = []string{
	// Resolves nowhere at all.
	`SELECT max(b), (SELECT d FROM tb WHERE zzz=c) FROM ta`,
	// Ambiguous within the subquery's OWN FROM -- tc.a and td.a. The enclosing
	// row also has an "a", and it must NOT be allowed to break the tie.
	`SELECT max(b), (SELECT e FROM tc, td WHERE a=1) FROM ta`,
}

func TestAggUnqualifiedCorrelatedRef(t *testing.T) {
	path := buildAggUnqualDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range aggUnqualServed {
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
			continue
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("[%s] QueryVDBE declined where C SQLite answered %d row(s): %v", sqlText, len(cRows), vErr)
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: rows=%d %v\n  cgo:  rows=%d %v",
				sqlText, reason, len(vVals), engineRowsToStrings(vVals), len(cRows), cRows)
			continue
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			t.Errorf("[%s] p.Query declined where C SQLite answered: %v", sqlText, eErr)
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: rows=%d %v\n  cgo:    rows=%d %v",
				sqlText, reason, len(eVals), engineRowsToStrings(eVals), len(cRows), cRows)
		}
	}

	for _, sqlText := range aggUnqualRejected {
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr == nil {
			t.Errorf("[%s] C SQLite ACCEPTED this -- the rejection premise no longer holds", sqlText)
			continue
		}
		if _, rows, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] QueryVDBE answered %d row(s) where C SQLite rejects the statement", sqlText, len(rows))
		}
		if _, rows, err := p.Query(sqlText); err == nil {
			t.Errorf("[%s] p.Query answered %d row(s) where C SQLite rejects the statement", sqlText, len(rows))
		}
	}
}

// ---------------------------------------------------------------------------
// The third piece: an aggregate written in a correlated subquery that DOES own
// a local column, evaluated for an enclosing AGGREGATE (or GROUP BY) query.
//
// SQLite's sqlite3ReferencesSrcList returns 1 for it -- one local column is
// enough -- so the aggregate stays with the subquery and the outer reference
// beside it is an ordinary per-group constant, read from the same anchor row a
// bare column of the enclosing query reads. Two separate guards used to decline
// it anyway, one per spelling: materializedOuterRefInAggArg (engine/sql_group.go)
// flagged any QUALIFIED outer column inside an aggregate argument without ever
// asking whether a local one sat beside it, and bindOuterAggRef
// (engine/vdbe_agg_codegen.go) had no way to reach the enclosing row for the
// UNQUALIFIED spelling. Both now apply the rule instead of the shape.
//
// This is the highest-risk relaxation in this file, so its decline corpus is
// larger than its served one: every way of writing an aggregate whose argument
// names NO local column must still decline, in all four clauses, both
// spellings.
// ---------------------------------------------------------------------------

// buildAggAnchorDB is aggnested-3.10's own fixture (t1/t2 renamed): two groups,
// whose anchor rows carry DIFFERENT value1 values (12 and 34), so a result that
// merely happens to be constant across groups cannot pass by accident.
func buildAggAnchorDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agganchor.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE g1(id1, value1)`,
		`INSERT INTO g1 VALUES (4469,12), (4469,11), (4470,34)`,
		`CREATE TABLE g2(value2)`,
		`INSERT INTO g2 VALUES (12), (34), (34)`,
		`CREATE TABLE ha(a,b)`,
		`INSERT INTO ha VALUES (1,10), (2,20), (1,30)`,
		`CREATE TABLE hb(c,d)`,
		`INSERT INTO hb VALUES (1,'one'), (2,'two')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// aggAnchorServed: one local column in the aggregate's argument, so it stays
// with the subquery. Must ANSWER, matching the oracle.
var aggAnchorServed = []string{
	// aggnested-3.10/3.11: the corpus statements this closes. Both orders of
	// the select list, because the anchor row is chosen per group, not per item.
	`SELECT max(value1), (SELECT sum(value2=value1) FROM g2) FROM g1 GROUP BY id1 ORDER BY id1`,
	`SELECT (SELECT sum(value2=value1) FROM g2), max(value1) FROM g1 GROUP BY id1 ORDER BY id1`,
	// The qualified spelling of the same statement -- previously the
	// materializedOuterRefInAggArg decline, now the same answer.
	`SELECT max(value1), (SELECT sum(value2=g1.value1) FROM g2) FROM g1 GROUP BY id1 ORDER BY id1`,
	`SELECT max(g1.value1), (SELECT sum(g2.value2=g1.value1) FROM g2) FROM g1 GROUP BY g1.id1 ORDER BY g1.id1`,
	// The group key itself as the correlated value, and a whole-table aggregate
	// enclosing query rather than a GROUP BY one.
	`SELECT id1, (SELECT sum(value2=value1) FROM g2) FROM g1 GROUP BY id1 ORDER BY id1`,
	`SELECT max(b), (SELECT sum(c=a) FROM hb) FROM ha`,
	`SELECT max(b), (SELECT sum(hb.c=ha.a) FROM hb) FROM ha`,
	`SELECT a, (SELECT sum(c=a) FROM hb) FROM ha GROUP BY a ORDER BY a`,
	// group_concat's SEPARATOR is the correlated operand, the value local.
	`SELECT a, (SELECT group_concat(d,a) FROM hb) FROM ha GROUP BY a ORDER BY a`,
	// Row mode, which already worked (aggnested-3.13): unchanged.
	`SELECT value1, (SELECT sum(value2=value1) FROM g2) FROM g1 ORDER BY g1.rowid`,
	// NO local column in the argument at all, so SQLite re-associates the whole
	// aggregate with the ENCLOSING query -- which these two now do rather than
	// decline (engine/sql_group.go's itemAggHoists). The enclosing query is
	// already an aggregate query in both cases, so its row count does not move;
	// what moves is the value. Verified against C SQLite over
	// ha = (1,10),(2,20),(1,30): (30,4) -- max(b) is 30 and sum(ha.a) is
	// 1+2+1 over EVERY ha row, not the anchor row's single a -- and
	// (1,40),(2,20) for the grouped form, each group's own sum(b).
	`SELECT max(b), (SELECT sum(ha.a) FROM hb) FROM ha`,
	`SELECT a, (SELECT sum(ha.b) FROM hb) FROM ha GROUP BY a ORDER BY a`,
}

// aggAnchorDeclines: NO local column in the aggregate's argument, so SQLite
// re-associates it outward and the enclosing query's row count changes. Must
// still DECLINE -- in every clause, in both spellings. This is the corpus that
// proves the relaxation above did not become a blanket accept.
var aggAnchorDeclines = []string{
	// Unqualified, select list of the subquery. The QUALIFIED spellings of
	// these two are no longer here: they are re-associated to the enclosing
	// query and SERVED (see aggAnchorServed). Unqualified still declines
	// because materializeOuterRefs only ever rewrites a QUALIFIED reference,
	// so the call cannot be matched to a body at run time.
	`SELECT max(b), (SELECT sum(a) FROM hb) FROM ha`,
	`SELECT a, (SELECT sum(b) FROM hb) FROM ha GROUP BY a`,
	`SELECT a, (SELECT group_concat(b,'-') FROM hb) FROM ha GROUP BY a`,
	// The subquery's own HAVING, GROUP BY-HAVING and ORDER BY.
	`SELECT max(b), (SELECT sum(d) FROM hb HAVING sum(a)>0) FROM ha`,
	`SELECT max(b), (SELECT sum(d) FROM hb GROUP BY c HAVING sum(a)>0) FROM ha`,
	`SELECT max(b), (SELECT c FROM hb GROUP BY c ORDER BY sum(a)) FROM ha`,
	// An EXISTS body in the enclosing query's HAVING.
	`SELECT a FROM ha GROUP BY a HAVING EXISTS(SELECT sum(b) FROM hb)`,
}

func TestAggAnchorRowCorrelatedAggregate(t *testing.T) {
	path := buildAggAnchorDB(t)

	// The ORACLE reads the EXPORT: the file this engine built is a segment file
	// (convert_for_oracle_test.go explains the seam).
	cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, sqlText := range aggAnchorServed {
		cCols, cRows, cErr := cgoSelect(t, cdb, sqlText, nil)
		if cErr != nil {
			t.Errorf("[%s] C SQLite unexpectedly errored: %v", sqlText, cErr)
			continue
		}
		vCols, vVals, vErr := p.QueryArgs(sqlText, nil)
		if vErr != nil {
			t.Errorf("[%s] QueryVDBE declined where C SQLite answered %d row(s): %v", sqlText, len(cRows), vErr)
			continue
		}
		if ok, reason := queryResultsMatch(vCols, engineRowsToStrings(vVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] QueryVDBE DIVERGES from C SQLite: %s\n  vdbe: rows=%d %v\n  cgo:  rows=%d %v",
				sqlText, reason, len(vVals), engineRowsToStrings(vVals), len(cRows), cRows)
			continue
		}
		eCols, eVals, eErr := p.Query(sqlText)
		if eErr != nil {
			t.Errorf("[%s] p.Query declined where C SQLite answered: %v", sqlText, eErr)
			continue
		}
		if ok, reason := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, true); !ok {
			t.Errorf("[%s] p.Query DIVERGES from C SQLite: %s\n  engine: rows=%d %v\n  cgo:    rows=%d %v",
				sqlText, reason, len(eVals), engineRowsToStrings(eVals), len(cRows), cRows)
		}
	}

	for _, sqlText := range aggAnchorDeclines {
		// C SQLite answers these; the point is that it answers them by
		// RE-ASSOCIATING, which this engine does not model. Answering per anchor
		// row would give a plausible-looking but wrong value.
		if _, _, cErr := cgoSelect(t, cdb, sqlText, nil); cErr != nil {
			t.Errorf("[%s] C SQLite errored (%v) -- this entry is not a re-association case", sqlText, cErr)
			continue
		}
		if _, rows, err := p.QueryArgs(sqlText, nil); err == nil {
			t.Errorf("[%s] QueryVDBE answered %d row(s) for an aggregate that re-associates to the enclosing query -- must decline", sqlText, len(rows))
		}
		if _, rows, err := p.Query(sqlText); err == nil {
			t.Errorf("[%s] p.Query answered %d row(s) for an aggregate that re-associates to the enclosing query -- must decline", sqlText, len(rows))
		}
	}
}
