package engine

// ftsMatchForcedOrder recognizes the ONE join-nesting shape whose order is
// FORCED by a dependency rather than chosen by cost, and so is provable
// without this port's ported multi-table solver at all (which
// wherePlanMultiTableSources still declines to enter for any vtab source --
// see that function's own doc comment for the general question this does
// NOT answer): an exactly-two-item FROM clause, joined by comma/CROSS/plain
// INNER JOIN (never LEFT/RIGHT/FULL -- see below), where ONE item is an
// fts3/4/5 vtab and its MATCH argument correlates to EXACTLY the other item.
//
// C citations (round 17's own where_plan_gate.go doc comment has the fuller
// version; this is the round-18 follow-up that answers the question that
// comment left open):
//
//   - whereLoopAddVirtualOne (where.c:4390, "pTerm->prereqRight & mUsable")
//     marks a MATCH constraint usable in a given call only once every table
//     its RIGHT-hand side depends on is already in mUsable -- i.e. already
//     bound earlier in the nesting.
//   - fts3's own xBestIndex (fts3.c:1636-1646) sets an astronomical 1e50
//     "discourage this" cost the INSTANT an unusable MATCH constraint
//     remains in the constraint list for that call, before even looking at
//     any OTHER (possibly usable) constraint -- so a MIX of a correlated and
//     a non-correlated MATCH term on the same vtab is genuinely ambiguous
//     (see the "exactly one qualifying term" loop below) rather than simply
//     "the correlated one wins".
//   - fts5's own (ext/fts5/fts5_main.c:657-661) refuses outright with
//     SQLITE_CONSTRAINT for the identical case, which
//     whereLoopAddVirtualOne (where.c:4412-4419) turns into NO loop AT ALL
//     for that position -- a hard exclusion, not merely a cost penalty.
//
// Verified this is not a disguised cost tie for the 2-item shape
// specifically, not assumed "usually true": fabricating the most extreme
// row-count estimate sqlite_stat1 can express (2^63-1, the max int64 its
// own reader parses) on the correlated OTHER item does not flip the chosen
// nesting -- 9.2e18 is still ~31 orders of magnitude short of fts3's 1e50
// penalty, and no larger estimate is expressible through that mechanism.
// FROM-clause spelling order does not matter either: "t JOIN x ON t MATCH
// x.c" and "x JOIN t ON t MATCH x.c" both plan x-then-t (verified live,
// EXPLAIN QUERY PLAN, both fts3/4 and fts5 oracle builds).
//
// Deliberately narrower than the general rule C SQLite implements --
// each bullet below is a live EXPLAIN QUERY PLAN finding from this round's
// own investigation, not a guess:
//
//   - EXACTLY TWO FROM items. A THIRD, unrelated item's position relative to
//     the correlated one is a genuine cost tie, broken (in every fixture
//     tried) by literal FROM order alone: a 3-item FROM with the vtab
//     correlated to one of two OTHERWISE-unconstrained other items pins the
//     vtab last but leaves the other two in their ORIGINAL FROM order
//     regardless of which is written first -- exactly what a cost TIE
//     (not a structural rule) looks like, and this port does not replicate
//     the cost model that would turn that tie into a proof. Confirms round
//     17's own "3-way probe" finding rather than merely repeating it.
//   - A MIXED set of MATCH terms on the same vtab, where ANY of them is NOT
//     correlated to the other item (a literal, or self-referential), stays
//     declined: fts3BestIndexMethod's loop (fts3.c:1668-1674) overwrites its
//     chosen constraint on every qualifying MATCH term it iterates,
//     LAST ONE WINS -- so whether the correlated one "escapes" and forces
//     the order, or a literal one wins the slot and does not, depends on
//     aConstraint's own iteration order, a question this port does not
//     attempt to answer. Every MATCH term whose subject is the vtab must
//     independently prove correlated to exactly the other item, or this
//     declines the whole shape.
//   - An OR of MATCH terms is a different mechanism entirely
//     (WHERE_MULTI_OR -- verified live: C SQLite plans it as MULTI-INDEX
//     OR, one vtab sub-scan per disjunct) -- anchorMultiOrInPlay's own,
//     separate gap, untouched here; a MatchExpr found inside an OR is never
//     even reached by this function, since it only walks TOP-LEVEL AND
//     conjuncts (splitTopLevelAnd).
//
// Two callers ask the identical question and must never drift apart:
// computeExecOrder (this file) uses it to decide the PHYSICAL nesting the
// bytecode actually executes; anchorLoopOrderProvable (vdbe_agg_codegen.go)
// uses it to decide whether the aggregate anchor guard may TRUST that
// nesting. Sharing this one function is what keeps them from disagreeing.
func ftsMatchForcedOrder(jts []joinedTable, scopes []tableScope, where Expr) ([]int, bool) {
	if len(jts) != 2 || len(scopes) != 2 {
		return nil, false
	}
	if jts[0].left || jts[1].left {
		// Either already forced to literal FROM order by the caller's own
		// pre-existing 2-item LEFT JOIN rule (computeExecOrder's hasGroups
		// check does not apply here, and anchorLoopOrderProvable's own
		// "len(jts)==2 && !jts[0].left && jts[1].left" shortcut already
		// covers the vtab-as-LEFT-JOIN-target case unconditionally,
		// vtab-ness aside), or a LEFT JOIN shape this function has not
		// investigated (the vtab as the FIRST item with the SECOND
		// LEFT-JOINed onto it via a MATCH correlated back to the first --
		// not the shape either citation above was verified against).
		return nil, false
	}
	v := -1
	for i := range jts {
		if jts[i].isFtsVtab {
			if v >= 0 {
				// Both items are fts vtabs: unproven (neither this round nor
				// round 17 investigated a fts-vtab-to-fts-vtab MATCH
				// correlation), decline rather than guess.
				return nil, false
			}
			v = i
		}
	}
	if v < 0 {
		return nil, false
	}
	w := 1 - v

	var conjuncts []Expr
	conjuncts = append(conjuncts, splitTopLevelAnd(where)...)
	conjuncts = append(conjuncts, splitTopLevelAnd(jts[v].on)...)
	conjuncts = append(conjuncts, splitTopLevelAnd(jts[w].on)...)

	found := false
	for _, cj := range conjuncts {
		m, ok := cj.(MatchExpr)
		if !ok || m.Not {
			// "NOT <vtab> MATCH q" is not a usable vtab index constraint at
			// all (allowedOp, whereexpr.c, admits only the bare comparison
			// ops -- MATCH negated by NOT is not among them), so it forces
			// nothing and disqualifies nothing either: skip it exactly like
			// any other non-MATCH conjunct.
			continue
		}
		if matchSubjectSource(m, scopes) != v {
			continue // a MATCH on some OTHER column/table -- irrelevant here
		}
		dep := map[int]bool{}
		collectTableRefs(m.Pattern, scopes, dep)
		if len(dep) != 1 || !dep[w] {
			// Not correlated to exactly the other item (a literal, a
			// self-reference, or -- impossible in a 2-item FROM, but
			// checked anyway for defense-in-depth -- something wider): per
			// this function's own doc comment, a MIXED set of MATCH terms
			// targeting the vtab is a genuine ambiguity fts3's own
			// "last-wins" constraint-selection loop does not resolve
			// predictably from here, so the WHOLE shape declines rather
			// than trusting the term(s) that happen to look correlated.
			return nil, false
		}
		found = true
	}
	if !found {
		return nil, false
	}
	return []int{w, v}, true
}

// matchSubjectSource returns the single joinedTable/scope index m's subject
// (m.X) resolves to, or -1 if it does not resolve to exactly one.
//
// Two forms, both C SQLite constraint shapes (whereLoopAddVirtualOne
// treats them identically -- only pExpr->pLeft's own TK_COLUMN resolution
// matters, not which spelling produced it):
//
//   - the bare "<vtab> MATCH q" idiom, where the unqualified column name
//     equals the vtab's OWN table/alias name (join.go's collectTableRefs
//     has the identical rule, considerMatchTable, for its own unrelated
//     WHERE-pushdown-dependency purpose -- this is that same syntax fact,
//     read for a different question).
//   - an ordinary column reference naming one of the vtab's real columns
//     (fts5's per-column MATCH, or an explicitly-qualified
//     "vtab.col MATCH q"), resolved through the general collectTableRefs
//     machinery exactly like any other column would be.
//
// Requires m.X to be a bare ColumnExpr: C SQLite's vtab constraint
// mechanism only ever identifies a term's "left column" when pExpr->pLeft
// is TK_COLUMN (exprAnalyze's term-identification precondition,
// whereexpr.c) -- an expression subject is never pushed to xBestIndex as a
// constraint at all, so it cannot be the thing this function is answering
// "does this force an order" about.
func matchSubjectSource(m MatchExpr, scopes []tableScope) int {
	ce, ok := m.X.(ColumnExpr)
	if !ok {
		return -1
	}
	out := map[int]bool{}
	if ce.Qualifier == "" {
		for i, ts := range scopes {
			if equalFoldName(ts.name, ce.Name) {
				out[i] = true
			}
		}
	}
	collectTableRefs(ce, scopes, out)
	if len(out) != 1 {
		return -1
	}
	for i := range out {
		return i
	}
	return -1
}

// ftsMatchLeftJoinUnusable reports whether a MATCH conjunct on scope idx can
// never be handed to fts3/fts4's xBestIndex because of outer-join rules. C
// then evaluates MATCH as a plain function, which always raises "unable to use
// function MATCH in the requested context" (main.c:2213); fts3MatchBindings
// poisons the scope so checkFts3Match raises the same error.
//
// Three C rules, each a local fact about one join step x:
//
//   - ON clause (whereexpr.c:1187, ticket #3015): an ON term's prereqRight is
//     padded with every table before x, so an ON-clause MATCH on idx < x is
//     unusable even with a literal pattern. sqlite3ProcessJoin (select.c:530)
//     sets EP_OuterON for LEFT, RIGHT and FULL alike, so this covers all three.
//   - Usability (where.c:4390): a MATCH whose pattern reads a LEFT JOIN's
//     right-hand item cannot drive an earlier item's scan; LEFT pairs are
//     never reordered.
//   - Re-evaluation (disableTerm, wherecode.c:419): a WHERE term on the
//     NULL-extended side has no EP_OuterON, so it is re-evaluated as a plain
//     expression after the seek, and plain MATCH always raises.
//
// RIGHT/FULL add constraintCompatibleWithOuterJoin (where.c:819, gate at
// where.c:1461): a plain WHERE term on an item tagged JT_LTORJ (before the
// rightmost RIGHT/FULL item, build.c:5214) is unusable even with a literal
// pattern. Checking each rightOuter x with idx < x gives the same verdict.
//
// The vtab as x itself, matched in x's own ON clause, is always usable.
//
// onOwner is the jts index of the join step owning this ON conjunct, or -1
// for a WHERE conjunct or an owner hidden inside a parenthesized join group;
// -1 with isOn falls back to treating jts[1] as the join's right-hand item.
func ftsMatchLeftJoinUnusable(jts []joinedTable, idx int, isOn bool, onOwner int, patternRefs map[int]bool) bool {
	if len(jts) < 2 || idx < 0 || idx >= len(jts) {
		return false
	}

	if isOn && onOwner < 0 {
		// Owner unknown: only the "plain base item, then one outer-joined
		// item" shape is answered.
		if len(jts) != 2 || jts[0].left || jts[0].rightOuter || !(jts[1].left || jts[1].rightOuter) {
			return false
		}
		return idx == 0 // ticket #3015; idx 1 is EP_OuterON-safe
	}

	if isOn {
		x := onOwner
		if x < 0 || x >= len(jts) || !(jts[x].left || jts[x].rightOuter) {
			return false // not an outer join step
		}
		// idx == x: EP_OuterON-safe. idx < x: ticket #3015, unconditional.
		// idx > x cannot appear in x's ON clause.
		return idx < x
	}

	// A WHERE conjunct is checked against every outer join step.
	for x := range jts {
		if !jts[x].left && !jts[x].rightOuter {
			continue
		}
		switch {
		case idx == x:
			// disableTerm (wherecode.c:419) for LEFT/FULL;
			// constraintCompatibleWithOuterJoin (where.c:819) for RIGHT/FULL.
			return true
		case idx < x && (jts[x].rightOuter || patternRefs[x]):
			// LEFT: unusable only when the pattern reads x (where.c:4390).
			// RIGHT/FULL: idx carries JT_LTORJ, unusable whatever the
			// pattern (where.c:1461).
			return true
		}
	}
	return false
}
