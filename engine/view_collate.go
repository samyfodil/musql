// This file adds VIEW-body validation: rejecting unrecognized COLLATE clauses
// that the parser must accept for view bodies.
// suppressed (p.allowUnknownCollation), so an unrecognized name is retained,
// unresolved, as an ordinary CollateExpr node -- exactly the shape a
// RECOGNIZED name already had. Verified directly against C SQLite
// (mattn/go-sqlite3 3.53.3): "CREATE VIEW v AS SELECT a COLLATE bogus FROM
// t" and "CREATE VIEW v AS SELECT a FROM t ORDER BY 1 COLLATE bogus" both
// succeed outright.
//
// Validation happens LATER, the first time anything genuinely resolves the
// view's body -- build.c:3115-3183 sqlite3ViewGetColumnNames, called by a
// live FROM reference, PRAGMA table_info/table_xinfo, an INSTEAD OF
// trigger's NEW.<col> mapping, or the flattening optimizer inlining the view
// into its caller. It duplicates the stored Select again and unconditionally
// calls sqlite3ResultSetOfSelect, which in turn calls
// sqlite3SubqueryColumnTypes (select.c:2346): a loop over the (leftmost-arm)
// result-column list that calls `sqlite3ExprCollSeq(pParse, p)`
// (select.c:2421) for EVERY one of them, regardless of whether the outer
// query does anything more with the column than SELECT it. That is what
// makes a COLLATE clause in a view's OWN result list validate even where the
// identical clause in an ordinary top-level statement's result list would
// not: "SELECT a COLLATE bogus FROM t" alone is fine (parseCollate's own doc
// comment), but wrapped in "CREATE VIEW v AS SELECT a COLLATE bogus FROM t"
// and then queried ("SELECT * FROM v"), it isn't -- and even bare "PRAGMA
// table_info(v)" already fails on it, verified directly, with no SELECT at
// all.
//
// A view genuinely QUERIED (not just introspected for its column list) goes
// further still: the whole body gets compiled/executed, so every ordinary
// consuming site downstream of codegen -- WHERE comparisons, ORDER BY/GROUP
// BY key info, aggregate min/max -- resolves its own COLLATE clauses on
// demand, exactly like an ordinary top-level statement's would. Measured
// directly against the oracle: an unknown COLLATE anywhere at all in a
// queried view's body -- WHERE-only, ORDER-BY-only, buried in a CASE arm --
// fails "SELECT * FROM v", even though the identical WHERE-only/ORDER-BY-only
// placement does NOT fail a bare "PRAGMA table_info(v)" over the same view
// (table_info never asks for anything beyond the result-column types).
//
// Reproducing sqlite3ExprCollSeq's own narrow, single-path EP_Collate
// propagation rule (expr.c:248-278) exactly -- for every position it is and
// is not reachable from, and separately for each of the several real call
// sites above with their genuinely different scopes -- is exactly the risk
// the general "make COLLATE fully lazy" approach was already declined for
// (see parseCollate's own doc comment, sql_parser.go): missing a
// propagation path would ACCEPT a statement C SQLite rejects, a wrong
// answer. So this file does not attempt that precision. It answers one
// coarser question instead -- "does ANY CollateExpr anywhere in this SELECT,
// and everything nested directly inside it (WHERE, HAVING, GROUP BY, ORDER
// BY, window specs, its own CTEs, and any subquery reachable without leaving
// this same view body), name a collation this engine doesn't implement?" --
// and every genuine view-body reference site rejects the view the moment the
// answer is yes. Those sites are exactly trusted_schema.go's own
// checkTrustedSchemaSelect call sites (view.go's viewOutputRows,
// vdbe_join_codegen.go's resolveViewColumns, pragma.go's pragmaViewInfo,
// flatten_limit_r35c.go's r37cViewBody, view_trigger.go's viewColumnInfos)
// -- the SAME choke points C SQLite's own analogous "does this reach a
// DDL-origin expression" rule already needed, because both questions are
// really "has anything genuinely resolved this view's body yet".
//
// This is coarser than C SQLite in exactly one direction: it also
// declines a few shapes C SQLite still permits through a schema-only site
// like PRAGMA table_info (an unknown COLLATE that sits only in WHERE/ORDER
// BY/GROUP BY, never in a result column). A decline is the safe side of
// "never wrong" -- and every one of these shapes was ALREADY completely
// unreachable before this change (parseCollate rejected it at CREATE VIEW
// time, unconditionally, for every position without exception), so being
// coarser here costs nothing against anything this engine used to answer.
//
// It never recurses into a FROM item that names ANOTHER view: that view's
// own body gets this identical check the next time IT is genuinely
// resolved, at its own call sites -- resolveViewRowsGuarded's
// circular-reference stack (view.go) already bounds that recursion for the
// analogous no-such-table/trusted-schema checks this mirrors, so nothing
// here needs its own cycle guard.
package engine

import "fmt"

// errUnknownViewCollation formats C SQLite's own error text
// (callback.c:230's sqlite3FindCollSeq/sqlite3GetCollSeq failure path) for a
// name found while resolving a referenced view's body -- byte-identical to
// parseCollate's own wording for an ordinary top-level statement
// (sql_parser.go), including preserving the name's original case.
func errUnknownViewCollation(name string) error {
	return fmt.Errorf("engine: no such collation sequence: %s", name)
}

// firstUnknownViewCollation returns the first collation name reachable from
// sel that is not in knownCollations (sql_parser.go), or "" if every COLLATE
// clause in sel's own body -- and every subquery/CTE/window spec nested
// directly inside it -- names one this engine implements.
func firstUnknownViewCollation(sel *SelectStmt) string {
	if sel == nil {
		return ""
	}
	for _, c := range sel.Columns {
		if n := firstUnknownCollationInExpr(c.Expr); n != "" {
			return n
		}
	}
	for i := range sel.From {
		it := &sel.From[i]
		if n := firstUnknownCollationInExpr(it.On); n != "" {
			return n
		}
		for _, a := range it.TableFuncArgs {
			if n := firstUnknownCollationInExpr(a); n != "" {
				return n
			}
		}
		// A derived-table subquery is part of THIS SAME view body (its text
		// sits inline inside the stored SELECT) -- unlike a FROM item naming
		// another view, which resolves as its own independent reference; see
		// this file's package doc comment.
		if n := firstUnknownViewCollation(it.Subquery); n != "" {
			return n
		}
	}
	if n := firstUnknownCollationInExpr(sel.Where); n != "" {
		return n
	}
	for _, g := range sel.GroupBy {
		if n := firstUnknownCollationInExpr(g); n != "" {
			return n
		}
	}
	if n := firstUnknownCollationInExpr(sel.Having); n != "" {
		return n
	}
	for _, o := range sel.OrderBy {
		if n := firstUnknownCollationInExpr(o.Expr); n != "" {
			return n
		}
	}
	for _, w := range sel.Windows {
		if n := firstUnknownCollationInWindowSpec(w.Spec); n != "" {
			return n
		}
	}
	for _, cte := range sel.CTEs {
		if n := firstUnknownViewCollation(cte.Select); n != "" {
			return n
		}
	}
	for _, arm := range sel.Compound {
		if n := firstUnknownViewCollation(arm.Stmt); n != "" {
			return n
		}
	}
	return ""
}

// firstUnknownCollationInWindowSpec checks a "OVER (...)" or "WINDOW name AS
// (...)" definition's PARTITION BY and ORDER BY expressions -- the only two
// places a window spec can carry a COLLATE clause (its optional frame bound
// is otherwise a literal/parameter, never a general expression).
func firstUnknownCollationInWindowSpec(w *WindowSpec) string {
	if w == nil {
		return ""
	}
	for _, p := range w.PartitionBy {
		if n := firstUnknownCollationInExpr(p); n != "" {
			return n
		}
	}
	for _, o := range w.OrderBy {
		if n := firstUnknownCollationInExpr(o.Expr); n != "" {
			return n
		}
	}
	return ""
}

// firstUnknownCollationInExpr descends into every sub-expression kind a view
// body can hold -- including a nested SELECT -- mirroring exprHasParam's
// identical traversal (view.go) over the same Expr node set.
func firstUnknownCollationInExpr(e Expr) string {
	switch x := e.(type) {
	case nil:
		return ""
	case CollateExpr:
		if !knownCollations[asciiFold(x.Name, false)] {
			return x.Name
		}
		return firstUnknownCollationInExpr(x.X)
	case UnaryExpr:
		return firstUnknownCollationInExpr(x.X)
	case BinaryExpr:
		if n := firstUnknownCollationInExpr(x.L); n != "" {
			return n
		}
		return firstUnknownCollationInExpr(x.R)
	case CastExpr:
		return firstUnknownCollationInExpr(x.X)
	case IsNullExpr:
		return firstUnknownCollationInExpr(x.X)
	case LikeExpr:
		if n := firstUnknownCollationInExpr(x.X); n != "" {
			return n
		}
		if n := firstUnknownCollationInExpr(x.Pattern); n != "" {
			return n
		}
		return firstUnknownCollationInExpr(x.Escape)
	case GlobExpr:
		if n := firstUnknownCollationInExpr(x.X); n != "" {
			return n
		}
		return firstUnknownCollationInExpr(x.Pattern)
	case MatchExpr:
		if n := firstUnknownCollationInExpr(x.X); n != "" {
			return n
		}
		return firstUnknownCollationInExpr(x.Pattern)
	case BetweenExpr:
		if n := firstUnknownCollationInExpr(x.X); n != "" {
			return n
		}
		if n := firstUnknownCollationInExpr(x.Lo); n != "" {
			return n
		}
		return firstUnknownCollationInExpr(x.Hi)
	case InExpr:
		if n := firstUnknownCollationInExpr(x.X); n != "" {
			return n
		}
		for _, a := range x.List {
			if n := firstUnknownCollationInExpr(a); n != "" {
				return n
			}
		}
		return firstUnknownViewCollation(x.Sub)
	case FuncExpr:
		for _, a := range x.Args {
			if n := firstUnknownCollationInExpr(a); n != "" {
				return n
			}
		}
		if n := firstUnknownCollationInExpr(x.Filter); n != "" {
			return n
		}
		for _, ob := range x.orderByExprs() {
			if n := firstUnknownCollationInExpr(ob); n != "" {
				return n
			}
		}
		return firstUnknownCollationInWindowSpec(x.Over)
	case CaseExpr:
		if n := firstUnknownCollationInExpr(x.Base); n != "" {
			return n
		}
		for _, w := range x.Whens {
			if n := firstUnknownCollationInExpr(w.When); n != "" {
				return n
			}
			if n := firstUnknownCollationInExpr(w.Then); n != "" {
				return n
			}
		}
		return firstUnknownCollationInExpr(x.Else)
	case SubqueryExpr:
		return firstUnknownViewCollation(x.Stmt)
	case ExistsExpr:
		return firstUnknownViewCollation(x.Stmt)
	case RowExpr:
		for _, v := range x.Elems {
			if n := firstUnknownCollationInExpr(v); n != "" {
				return n
			}
		}
	case RaiseExpr:
		return firstUnknownCollationInExpr(x.Msg)
	}
	return ""
}
