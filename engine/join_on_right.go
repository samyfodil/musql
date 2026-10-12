package engine

// checkOnReferencesRight is sqlite3SelectCheckOnClauses (select.c:7488) for one
// FROM level: an ON clause of an outer join -- or of any join, once the level
// has a RIGHT or FULL join -- may not name a FROM item to the right of the
// item it is attached to. C resolves every name against the whole FROM first
// and checks afterwards, so "t1 LEFT JOIN t2 ON t3.c = b JOIN t3" is "ON clause
// references tables to its right", not "no such column", and a reference
// hidden in a subquery ("ON (SELECT t3.c) = b") is rejected too rather than
// bound to some other scope.
//
// C compares cursor numbers, not FROM positions (selectCheckOnClausesExpr). A
// recursive CTE's own reference in its recursive arm got its cursor when the
// CTE was expanded, before any other table of the arm (withExpand), so it is
// the smallest: an outer join ON whose right side is the recursive table may
// name no other table of the arm, even one to its left ("edge LEFT JOIN path
// ON edge.parent = path.node" is rejected by 3.53.3).
//
// A column inside a subquery counts when it is qualified by a name this level
// binds and the subquery's own FROM does not, or when it is unqualified in a
// subquery that has no FROM of its own; anything else is left to ordinary
// resolution.
func checkOnReferencesRight(items []FromItem, srcs []joinSource, absToSrcs map[int]int) error {
	hasRight, hasUpdateTarget := false, false
	for i, it := range items {
		if _, ok := absToSrcs[i]; ok && (it.Join == JoinRight || it.Join == JoinFull) {
			hasRight = true
		}
		hasUpdateTarget = hasUpdateTarget || it.UpdateTarget
	}
	if err := checkUpdateFromUsingAmbiguity(items, srcs, absToSrcs, hasRight); err != nil {
		return err
	}
	rank := func(k int) int {
		if srcs[k].cteItem != nil && srcs[k].cteItem.recSelf != nil {
			return -1
		}
		return k
	}
	// byQualifier: the source a qualifier names at this level, or -1.
	byQualifier := func(q string) int {
		for k := range srcs {
			s := &srcs[k]
			if equalFoldName(s.scope.name, q) {
				return k
			}
			for _, ms := range s.memberScopes {
				if equalFoldName(ms.name, q) || (ms.groupAlias != "" && equalFoldName(ms.groupAlias, q)) {
					return k
				}
			}
			if s.scope.groupAlias != "" && equalFoldName(s.scope.groupAlias, q) {
				return k
			}
		}
		return -1
	}
	// byName: the one source exposing an unqualified column, or -1 when none
	// or several do (several is resolution's "ambiguous column name").
	byName := func(n string) int {
		lname := r33sFoldIdent(n)
		found := -1
		for k := range srcs {
			s := &srcs[k]
			scopes := []compileScope{s.scope}
			if len(s.memberScopes) > 0 {
				scopes = s.memberScopes
			}
			for _, sc := range scopes {
				if _, ok := sc.colIndex[lname]; !ok {
					continue
				}
				if _, hidden := sc.coalesced[lname]; hidden {
					continue
				}
				if found != -1 && found != k {
					return -1
				}
				found = k
			}
		}
		return found
	}
	for i, it := range items {
		k, ok := absToSrcs[i]
		if !ok || it.On == nil {
			continue
		}
		outer := it.Join == JoinLeft || it.Join == JoinRight || it.Join == JoinFull
		checkRight := outer || hasRight
		if !checkRight && !hasUpdateTarget {
			continue
		}
		limit := rank(k)
		bad := false
		var hidden *ColumnExpr // a reference to the UPDATE ... FROM target
		var visitExpr func(e Expr, shadow map[string]bool, inSub, subHasFrom bool)
		var visitSelect func(s *SelectStmt, shadow map[string]bool)
		visitExpr = func(e Expr, shadow map[string]bool, inSub, subHasFrom bool) {
			if e == nil || bad {
				return
			}
			switch x := e.(type) {
			case ColumnExpr:
				if x.UsingPinned || x.UsingRepr {
					return
				}
				target := -1
				if x.Qualifier != "" {
					if shadow[r33sFoldIdent(x.Qualifier)] {
						return
					}
					target = byQualifier(x.Qualifier)
				} else if !inSub || !subHasFrom {
					target = byName(x.Name)
				}
				if target >= 0 && srcs[target].updateTarget {
					hidden = &x
					bad = true
				} else if checkRight && target >= 0 && rank(target) > limit {
					bad = true
				}
				return
			case SubqueryExpr:
				visitSelect(x.Stmt, shadow)
				return
			case ExistsExpr:
				visitSelect(x.Stmt, shadow)
				return
			case InExpr:
				visitExpr(x.X, shadow, inSub, subHasFrom)
				for _, li := range x.List {
					visitExpr(li, shadow, inSub, subHasFrom)
				}
				visitSelect(x.Sub, shadow)
				return
			}
			walkExprOperands(e, func(op Expr) { visitExpr(op, shadow, inSub, subHasFrom) })
		}
		visitSelect = func(s *SelectStmt, shadow map[string]bool) {
			if s == nil || bad {
				return
			}
			inner := make(map[string]bool, len(shadow)+len(s.From))
			for n := range shadow {
				inner[n] = true
			}
			for _, fi := range s.From {
				if fi.Alias != "" {
					inner[r33sFoldIdent(fi.Alias)] = true
				}
				if fi.Table != "" {
					inner[r33sFoldIdent(fi.Table)] = true
				}
			}
			hasFrom := len(s.From) > 0
			for _, sc := range s.Columns {
				visitExpr(sc.Expr, inner, true, hasFrom)
			}
			for _, fi := range s.From {
				visitExpr(fi.On, inner, true, hasFrom)
			}
			visitExpr(s.Where, inner, true, hasFrom)
			visitExpr(s.Having, inner, true, hasFrom)
			for _, g := range s.GroupBy {
				visitExpr(g, inner, true, hasFrom)
			}
			for _, ot := range s.OrderBy {
				visitExpr(ot.Expr, inner, true, hasFrom)
			}
			for _, arm := range s.Compound {
				visitSelect(arm.Stmt, shadow)
			}
		}
		visitExpr(it.On, nil, false, false)
		if hidden != nil {
			// UPDATE ... FROM with two or more sources wraps them in an
			// SF_NestedFrom subquery (parse.y's UPDATE rule), and a FROM
			// subquery cannot see its sibling, the target: "no such column".
			if hidden.Qualifier != "" {
				return semanticf("engine: no such column: %s.%s", hidden.Qualifier, hidden.Name)
			}
			return semanticf("engine: no such column: %s", hidden.Name)
		}
		if bad {
			return semanticf("engine: ON clause references tables to its right")
		}
	}
	return nil
}

// checkUpdateFromUsingAmbiguity: UPDATE ... FROM with two or more sources wraps
// them in an SF_NestedFrom subquery (parse.y's UPDATE rule), whose expanded
// list holds each USING/NATURAL column as a bare name (selectExpander's
// bUsingTerm entry). lookupName skips only the sources whose own USING names
// it, so a column two other sources declare is "ambiguous column name"
// ("FROM s1 JOIN s2 USING(x) JOIN s3 ON ..." with x in s3), as it is for a
// parenthesized group (checkRebuiltJoinGroups). A RIGHT or FULL join resolves
// such a name differently and keeps the ordinary path.
func checkUpdateFromUsingAmbiguity(items []FromItem, srcs []joinSource, absToSrcs map[int]int, hasRight bool) error {
	if hasRight {
		return nil
	}
	var from []int // the FROM sources, the target excluded
	var fromItems []FromItem
	hasTarget := false
	for i, it := range items {
		k, ok := absToSrcs[i]
		if !ok {
			continue
		}
		if it.UpdateTarget {
			hasTarget = true
			continue
		}
		from = append(from, k)
		fromItems = append(fromItems, it)
	}
	if !hasTarget || len(from) < 2 {
		return nil
	}
	for fi, k := range from {
		// In the rebuilt list's order: each USING (or NATURAL's synthesized
		// list, the right table's columns in order) as written.
		names := fromItems[fi].Using
		if len(names) == 0 {
			for _, c := range srcs[k].scope.cols {
				if _, ok := srcs[k].scope.coalesced[r33sFoldIdent(c.Name)]; ok {
					names = append(names, c.Name)
				}
			}
		}
		for _, name := range names {
			lname := r33sFoldIdent(name)
			declaring, merged := 0, 0
			for _, j := range from {
				if _, ok := srcs[j].scope.colIndex[lname]; ok {
					declaring++
				}
				if _, ok := srcs[j].scope.coalesced[lname]; ok {
					merged++
				}
			}
			if declaring > merged+1 {
				return semanticf("engine: ambiguous column name: %s", name)
			}
		}
	}
	return nil
}
