// SELECT DISTINCT over a WINDOW query deduplicates using each column's
// COLLATING SEQUENCE, not BINARY. Tests collation in window deduplication.
// to survive into that synthesized table -- and it does:
//
//	select.c:2426   pColl = sqlite3ExprCollSeq(pParse, p);
//	select.c:2429   sqlite3ColumnSetColl(db, pCol, pColl->zName);
//
// with sqlite3ExprCollSeq reading it back off the ephemeral column
// (expr.c:262, "zColl = sqlite3ColumnColl(&p->y.pTab->aCol[j])"). The rewrite
// is COLLATION-TRANSPARENT, so the rule is exactly topExprCollation's -- the
// one distinctCollations already implements for an ordinary scan, which is why
// the fix reuses it rather than deriving a second answer.
//
// The cases below therefore come in pairs: the window spelling and the
// window-free control over the SAME fixture, so the two paths are measured
// against the same oracle answers and cannot drift apart.
package compat

import "testing"

var winDistinctFixture = []string{
	`CREATE TABLE nc(c TEXT COLLATE NOCASE, r TEXT COLLATE RTRIM, b TEXT, n)`,
	`INSERT INTO nc VALUES('apple','x  ','A',1),('APPLE','x','a',2),('pear','y ','B',3),` +
		`('PEAR','y','b',4),(NULL,NULL,NULL,5),('fig','z','C',6)`,
	`CREATE TABLE j(k, w TEXT COLLATE NOCASE)`,
	`INSERT INTO j VALUES(1,'p'),(2,'P'),(3,'q'),(5,'Q'),(6,'r')`,
}

func winDistinctParity(t *testing.T, name string, probes []string) {
	t.Helper()
	driverParity(t, name, append(append([]string(nil), winDistinctFixture...), probes...))
}

// TestWindowDistinctCollationEntryPoints drives the collated dedup through
// every compile path that builds a windowPlan: the plain row scan
// (vdbe_scan.go), the GROUP BY + window derived-table rewrite
// (vdbe_window_group.go), the FROM-less compiler (vdbe_window.go) and a JOIN,
// whose projection does NOT lower and so is walked as an AST -- the dedup is
// the same either way, which is the point of listing both.
//
// MEASURED, not assumed, that this file gates the fix: with the plan's
// distinctColls forced to nil, sixteen of these shapes diverge (four of them
// only reachable through the GROUP BY / join / derived-table entry points).
func TestWindowDistinctCollationEntryPoints(t *testing.T) {
	winDistinctParity(t, "window-distinct-collation", []string{
		// The plain row scan.
		`SELECT DISTINCT c, count(*) OVER () FROM nc`,
		`SELECT DISTINCT r, count(*) OVER () FROM nc`,
		`SELECT DISTINCT c, r, count(*) OVER () FROM nc`,
		`SELECT DISTINCT c, b, count(*) OVER () FROM nc`,
		`SELECT DISTINCT c, count(*) OVER () FROM nc ORDER BY c, 2`,
		// LIMIT is what shows this is not merely "extra rows": a duplicate that
		// survives the dedup consumes a LIMIT slot, so the rows RETURNED differ.
		`SELECT DISTINCT c, count(*) OVER () FROM nc LIMIT 3`,
		`SELECT DISTINCT c, count(*) OVER () FROM nc LIMIT 2 OFFSET 1`,

		// GROUP BY + window: compileScanGroupedWindow rewrites the groups into a
		// derived table and compiles THAT as the window query's whole FROM, so
		// the outCols distinctCollations resolves against are the derived ones.
		`SELECT DISTINCT c, sum(n) OVER () FROM nc GROUP BY c, n`,
		`SELECT DISTINCT c, count(*) OVER () FROM nc GROUP BY b`,
		`SELECT DISTINCT upper(c), count(*) OVER () FROM nc GROUP BY b`,

		// A JOIN scope -- a multi-source window query, whose projection is
		// lowered against a register block per source
		// (windowProjScopeLowerable, engine/vdbe_window_codegen.go). The dedup
		// still has to resolve the outer list's collations across both.
		`SELECT DISTINCT nc.c, count(*) OVER () FROM nc, j WHERE nc.n=j.k`,
		`SELECT DISTINCT j.w, count(*) OVER () FROM nc, j WHERE nc.n=j.k`,
		`SELECT DISTINCT nc.c, j.w, count(*) OVER () FROM nc LEFT JOIN j ON nc.n=j.k`,

		// Through a derived table, which must carry the declared collation out
		// of the subquery -- and must NOT when the subquery overrides it.
		`SELECT DISTINCT c, count(*) OVER () FROM (SELECT c FROM nc)`,
		`SELECT DISTINCT c, count(*) OVER () FROM (SELECT c COLLATE BINARY AS c FROM nc)`,

		// topExprCollation's rule, per column: an explicit COLLATE overrides the
		// declared one, a CAST and unary "+" are transparent, and any other
		// expression loses it (distinctCollations' own doc comment pins these
		// against 3.53.3 for the window-free path).
		`SELECT DISTINCT c COLLATE BINARY, count(*) OVER () FROM nc`,
		`SELECT DISTINCT c COLLATE RTRIM, count(*) OVER () FROM nc`,
		`SELECT DISTINCT c||'', count(*) OVER () FROM nc`,
		`SELECT DISTINCT +c, count(*) OVER () FROM nc`,
		`SELECT DISTINCT CAST(c AS TEXT), count(*) OVER () FROM nc`,
		`SELECT DISTINCT lower(c), count(*) OVER () FROM nc`,
		// Per column INDEPENDENTLY: column 0 dedups NOCASE and column 1 BINARY,
		// so 'apple'/'APPLE' stay two rows here and collapse to one above.
		`SELECT DISTINCT c, c COLLATE BINARY, count(*) OVER () FROM nc`,

		// FROM-less.
		`SELECT DISTINCT 'a', count(*) OVER ()`,
		`SELECT DISTINCT 'a' COLLATE NOCASE, count(*) OVER ()`,

		// The WINDOW VALUE itself is part of the dedup key, and carries no
		// collation of its own -- so two NOCASE-equal rows collapse only when
		// their window values are equal too.
		`SELECT DISTINCT c, row_number() OVER (ORDER BY n) FROM nc`,
		`SELECT DISTINCT c, sum(n) OVER (PARTITION BY c) FROM nc`,
		`SELECT DISTINCT c, first_value(b) OVER (PARTITION BY c ORDER BY n) FROM nc`,
	})
}

// TestWindowDistinctCollationNoWindowControl is the same dedup WITHOUT a window
// call, i.e. through OpDistinct's own collated set. It is the control the
// window path had to be made to agree with: every one of these already passed
// before slice I3, which is what identified the window path as the odd one out
// rather than the collation rule as wrong.
func TestWindowDistinctCollationNoWindowControl(t *testing.T) {
	winDistinctParity(t, "no-window-control", []string{
		`SELECT DISTINCT c FROM nc`,
		`SELECT DISTINCT r FROM nc`,
		`SELECT DISTINCT c, r FROM nc`,
		`SELECT DISTINCT c COLLATE BINARY FROM nc`,
		`SELECT DISTINCT c||'' FROM nc`,
		`SELECT DISTINCT c FROM nc LIMIT 3`,
		`SELECT DISTINCT nc.c FROM nc, j WHERE nc.n=j.k`,
		`SELECT DISTINCT c, c COLLATE BINARY FROM nc`,
	})
}
