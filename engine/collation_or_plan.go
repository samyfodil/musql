// This file decides what to DO about the one WHERE/ON shape
// riskyCrossTableDeclaredCollationOr flags: an OR of
// cross-table "col = col" equalities where a bare column's DECLARED
// non-BINARY collation is in play. That shape used to be declined outright
// (and only on the plain/sorted scan path); it is now classified into three
// outcomes -- serve with a collation override, serve as written, or decline
// -- from the rule pinned against mattn/go-sqlite3 3.53.3:
//
// The ONLY plan that makes C SQLite's answer diverge from the ordinary
// per-comparison collation rule (resolveCompareCollation's left-wins
// resolution, applied term by term) is the OR-to-IN transform: when EVERY
// disjunct of the OR is an equality against ONE shared column c, and c's
// table carries an index whose leading column is c under c's own DECLARED
// collation, SQLite rewrites the OR as "c IN (x, y, ...)" and probes that
// index -- and an IN's comparisons all use the LEFT operand c's collation,
// regardless of which side each equality was written with. Everything else
// -- a plain scan, or the multi-index OR (which only accepts a disjunct
// whose own per-term resolved collation EQUALS the index's) -- is
// observationally identical to per-term evaluation. Evidence:
//
//   - t614a(a NOCASE, b NOCASE) x t614b(c TEXT, indexed): all four operand
//     orders of "a=c OR b=c" answer 0 rows (c's BINARY governs every
//     branch), where per-term predicts {aaa,bbb}/{}/{bbb}/{aaa}. With c
//     NOCASE instead (t615b), all four orders answer BOTH rows. Without the
//     index, all four orders follow per-term exactly. EXPLAIN QUERY PLAN
//     shows a single "SEARCH ... (c=?)" -- the IN probe, not MULTI-INDEX OR.
//   - the index's collation must MATCH c's declared collation for the
//     transform: "CREATE INDEX tf_c ON tf(c COLLATE NOCASE)" over a BINARY c
//     does NOT transform -- "a=c OR b=c" (terms resolving NOCASE) runs as a
//     MULTI-INDEX OR whose probes equal the per-term collation, and
//     "c=a OR c=b" (terms resolving BINARY, mismatching the NOCASE index)
//     falls all the way to a SCAN; both spellings equal per-term evaluation.
//   - the transform fires with the shared column leftmost in a multi-column
//     index, with a UNIQUE or DESC index, with a WITHOUT ROWID inner table,
//     with three disjuncts, under GROUP BY / count(*) / DISTINCT / ORDER BY,
//     in an INNER JOIN's ON, in a LEFT JOIN's ON (probing the right table;
//     the unmatched left row still NULL-completes), in UPDATE ... FROM, and
//     regardless of FROM order (the planner probes the indexed side either
//     way). It does NOT fire when the shared column is only a NON-leading
//     index column (plain SCAN, per-term), nor when the disjuncts have no
//     shared column at all ("a=c OR b=d", indexes on both c and d: SCAN,
//     because each term's NOCASE per-term collation mismatches its BINARY
//     index -- the multi-index OR's collation gate again).
//   - a PARTIAL index on c (WHERE c IS NOT NULL) IS chosen for the probe
//     when the disjunct implies its predicate -- reproducing SQLite's
//     implication analysis is out of scope, so any expression/partial index
//     on the probed table keeps the DECLINE.
//   - ANALYZE with these tiny tables did not flip the plan (still SEARCH),
//     and neither did a hand-hostile sqlite_stat1 row ('1000000 500000');
//     but the transform's cost gate under statistics is C SQLite's cost
//     model, which this engine does not reproduce, so ANY sqlite_stat1
//     table present keeps the DECLINE rather than betting on it.
//
// The override itself is a WHERE/ON rewrite, not a plan: each disjunct's
// shared-column operand is wrapped in an explicit COLLATE <c's declared
// collation>, which resolveCompareCollation then obeys on every downstream
// path (plain, sorted, grouped, aggregate, window, UPDATE ... FROM). That
// wrap IS the IN-transform's semantics -- same rows, same NULL handling --
// without adopting any plan machinery.
//
// serve-as-written (no rewrite, no decline) is returned when NO transform
// can fire: the disjuncts share no column, or the shared column has no
// leading-column index under its own declared collation. Per the multi-index
// OR collation gate above, every plan then equals per-term evaluation.
package engine

import (
	"fmt"
)

// collationOrDecline is the historical decline for this shape, kept verbatim
// so histograms and gates keyed on the message still recognize the bucket.
func collationOrDecline() error {
	return fmt.Errorf("%w: cross-table declared-collation equality combined with OR", errVDBEUnsupported)
}

// applyRiskyCollationOrPlan classifies stmt's constraints per this file's
// package comment. It returns (nil, nil, nil) when nothing risky is present
// OR the risky shape is provably per-term (serve as written); a non-nil
// newWhere/newOns[i] when the matching constraint must be served with the
// collation override; and a non-nil error to decline. Called by
// compileScanAttempt AFTER scopes/alias substitution and BEFORE any dispatch,
// so every downstream compile path (grouped, aggregate, window, plain,
// sorted -- and through them UPDATE ... FROM) inherits one decision;
// previously the decline lived only on the plain/sorted path and only read
// stmt.Where, which let the GROUP BY / aggregate paths and every JOIN ON
// spelling of the same shape run per-term -- a measured WRONG (0 rows from
// the oracle vs 2 from this engine on the t614 shape spelled "JOIN ... ON",
// "GROUP BY c", or "count(*)").
func applyRiskyCollationOrPlan(p *ReadOnlyPager, c *compiler, stmt *SelectStmt, srcs []joinSource) (Expr, []Expr, error) {
	ctx := c.affCtx()
	riskyWhere := riskyCrossTableDeclaredCollationOr(stmt.Where, ctx)
	riskyOn := -1
	for i := range srcs {
		if riskyCrossTableDeclaredCollationOr(srcs[i].on, ctx) {
			if riskyOn >= 0 || riskyWhere {
				// Two independent risky constraints: each would need its own
				// analysis against a shared planner choice. Not attempted.
				return nil, nil, collationOrDecline()
			}
			riskyOn = i
		}
	}
	if !riskyWhere && riskyOn < 0 {
		return nil, nil, nil
	}

	// Statement-level preconditions for EITHER serve outcome. Anything
	// outside them keeps the historical decline.
	if groupPresent(srcs) || len(srcs) != 2 {
		return nil, nil, collationOrDecline()
	}
	for i := range srcs {
		s := &srcs[i]
		if s.derived != nil || s.vtabItem != nil || s.cteItem != nil ||
			s.catalogScope != scopeAny ||
			s.tbl == nil || s.tbl.name == "" || s.scope.dbIdx != 0 ||
			s.rightOuter {
			return nil, nil, collationOrDecline()
		}
	}
	// The risky constraint must be the statement's SOLE constraint: another
	// WHERE conjunct or ON referencing the probed table could hand the
	// planner a competing index driver, demoting the OR to a per-term
	// filter -- a choice this engine has no cost model to predict.
	if riskyWhere && (srcs[0].on != nil || srcs[1].on != nil) {
		return nil, nil, collationOrDecline()
	}
	if riskyOn >= 0 && (stmt.Where != nil || riskyOn != 1) {
		return nil, nil, collationOrDecline()
	}
	// A risky WHERE over a LEFT JOIN mixes the transform with NULL-completed
	// rows this analysis has no probe evidence for; the ON spelling is the
	// one that was measured.
	if riskyWhere && srcs[1].left {
		return nil, nil, collationOrDecline()
	}

	e := stmt.Where
	if riskyOn >= 0 {
		e = srcs[riskyOn].on
	}

	// Shape: the whole constraint is an OR-tree of "col = col" equalities.
	var leaves []BinaryExpr
	var collect func(Expr) bool
	collect = func(x Expr) bool {
		b, ok := x.(BinaryExpr)
		if !ok {
			return false
		}
		switch b.Op {
		case "OR":
			return collect(b.L) && collect(b.R)
		case "=", "==":
			if _, lok := b.L.(ColumnExpr); !lok {
				return false
			}
			if _, rok := b.R.(ColumnExpr); !rok {
				return false
			}
			leaves = append(leaves, b)
			return true
		}
		return false
	}
	if !collect(e) || len(leaves) < 2 {
		return nil, nil, collationOrDecline()
	}

	scopes := [2]tableScope{srcs[0].scope.tableScope, srcs[1].scope.tableScope}
	resolve := func(x Expr) (int, int, bool) {
		ce, ok := x.(ColumnExpr)
		if !ok || ce.Schema != "" {
			return 0, 0, false
		}
		ln := r33sFoldIdent(ce.Name)
		found, fs, fc := false, 0, 0
		for si := range scopes {
			ts := &scopes[si]
			if ce.Qualifier != "" && !equalFoldName(ts.name, ce.Qualifier) {
				continue
			}
			ci, ok := ts.colIndex[ln]
			if !ok {
				continue
			}
			if _, coal := ts.coalesced[ln]; coal || ts.cols[ci].Hidden {
				return 0, 0, false
			}
			if found {
				return 0, 0, false // ambiguous
			}
			found, fs, fc = true, si, ci
		}
		return fs, fc, found
	}

	type ref struct{ scope, col int }
	// shared starts as leaf 0's two sides and is intersected across the rest:
	// the OR-to-IN transform needs ONE column present in every disjunct.
	var shared map[ref]bool
	refs := make([][2]ref, len(leaves))
	for i, lf := range leaves {
		ls, lc, lok := resolve(lf.L)
		rs, rc, rok := resolve(lf.R)
		if !lok || !rok || ls == rs {
			return nil, nil, collationOrDecline()
		}
		// The rule was pinned over TEXT-affinity columns only; an affinity
		// conflict is its own transform-disabling condition in SQLite
		// (where2.test's t2249 block) that this analysis has not reproduced.
		if scopes[ls].cols[lc].Aff != affText || scopes[rs].cols[rc].Aff != affText {
			return nil, nil, collationOrDecline()
		}
		refs[i] = [2]ref{{ls, lc}, {rs, rc}}
		cur := map[ref]bool{refs[i][0]: true, refs[i][1]: true}
		if i == 0 {
			shared = cur
			continue
		}
		for r := range shared {
			if !cur[r] {
				delete(shared, r)
			}
		}
	}
	if len(shared) == 0 {
		// No shared column -- no IN transform possible; every remaining plan
		// equals per-term evaluation (see package comment). Serve as written.
		return nil, nil, nil
	}
	if len(shared) > 1 {
		// Both sides shared (every disjunct is the SAME column pair): the
		// planner may transform on either table. Not disambiguated.
		return nil, nil, collationOrDecline()
	}
	var yRef ref
	for r := range shared {
		yRef = r
	}

	// LEFT JOIN's ON: only the RIGHT table can be probed (the left table's
	// rows are never filtered by an outer ON); a shared column on the LEFT
	// table therefore leaves the OR a per-term filter.
	if riskyOn == 1 && srcs[1].left && yRef.scope == 0 {
		return nil, nil, nil
	}

	y := &srcs[yRef.scope]
	ycol := scopes[yRef.scope].cols[yRef.col]
	// A WITHOUT ROWID table whose PRIMARY KEY leads with the shared column:
	// the table's own b-tree is an index on it, a probe target this analysis
	// has no evidence for.
	if y.tbl.withoutRowid && len(y.tbl.pkColIdx) > 0 && y.tbl.pkColIdx[0] == yRef.col {
		return nil, nil, collationOrDecline()
	}
	collName := effectiveCollation(ycol.Collation)

	rows, err := p.Schema()
	if err != nil {
		return nil, nil, err
	}
	// Discriminate same-named main/temp tables by root page.
	yTemp, foundY := false, false
	for i := range rows {
		r := &rows[i]
		if r.Type == "table" && r.RootPage == y.tbl.root && equalFoldName(r.Name, y.tbl.name) {
			yTemp, foundY = r.Temp, true
			break
		}
	}
	if !foundY {
		return nil, nil, collationOrDecline()
	}
	candidate := false
	for i := range rows {
		r := &rows[i]
		if equalFoldName(r.Name, "sqlite_stat1") && r.Type == "table" {
			// Statistics put the transform under C SQLite's cost model,
			// which this engine does not reproduce. (A hostile stat1 did not
			// flip the probed plan, but that is one data point, not a rule.)
			return nil, nil, collationOrDecline()
		}
		if r.Type != "index" || !equalFoldName(r.TblName, y.tbl.name) || r.Temp != yTemp {
			continue
		}
		if r.SQL == "" {
			// An automatic (UNIQUE/PRIMARY KEY) index: its column list is not
			// recoverable from the index row itself.
			return nil, nil, collationOrDecline()
		}
		pci, perr := parseCreateIndexStmt(r.SQL)
		if perr != nil {
			return nil, nil, collationOrDecline()
		}
		if pci.exprOrPartial {
			// See package comment: a partial index CAN be chosen for the
			// probe (implication analysis); declined wholesale.
			return nil, nil, collationOrDecline()
		}
		if len(pci.cols) == 0 || !equalFoldName(pci.cols[0], ycol.Name) {
			continue
		}
		idxColl := pci.collate[0]
		if idxColl == "" {
			idxColl = collName
		}
		if equalFoldName(idxColl, collName) {
			candidate = true
		}
		// A leading-column index under a DIFFERENT collation is neither a
		// candidate nor a decline: measured to leave the answer per-term.
	}
	if !candidate {
		return nil, nil, nil // no probe target -- per-term everywhere
	}

	// Override: wrap each disjunct's shared-column operand in an explicit
	// COLLATE, pinning every comparison to the IN transform's collation.
	li := 0
	var rewrite func(Expr) Expr
	rewrite = func(x Expr) Expr {
		b := x.(BinaryExpr) // collect validated the tree's shape
		if b.Op == "OR" {
			return BinaryExpr{Op: "OR", L: rewrite(b.L), R: rewrite(b.R)}
		}
		wrapL := refs[li][0] == yRef
		li++
		if wrapL {
			return BinaryExpr{Op: b.Op, L: CollateExpr{X: b.L, Name: collName}, R: b.R}
		}
		return BinaryExpr{Op: b.Op, L: b.L, R: CollateExpr{X: b.R, Name: collName}}
	}
	ne := rewrite(e)
	if riskyOn >= 0 {
		ons := make([]Expr, len(srcs))
		ons[riskyOn] = ne
		return nil, ons, nil
	}
	return ne, nil, nil
}
