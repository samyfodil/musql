package engine

// Expr is a parsed scalar expression. See sql_eval.go for evaluation and
// sql_parser.go for parsing; the concrete types below cover exactly the
// expression grammar documented in query.go.
type Expr interface{ exprNode() }

// LiteralExpr is a literal constant: integer, real, 'text', X'blob', or NULL.
//
// A literal has no affinity or collation of its own (sqlite3ExprAffinity's
// "return pExpr->affExpr" is 0 for TK_STRING/TK_INTEGER; sqlite3ExprCollSeq
// finds no pColl). The three unexported fields below are set only on the
// literal rewriteExprOuterRefs substitutes for a correlated outer COLUMN
// reference, which must keep the column's affinity, storage-class defense and
// declared collation or its comparisons change meaning (see its ColumnExpr
// arm).
//
// They live here rather than on a node type of their own so every walker that
// treats a LiteralExpr as a constant leaf stays correct; only the four
// affinity/collation helpers (exprAffinity, isMaterializedRef,
// declaredColumnCollation, topExprCollation) look at them. A separate node type
// made every walker without a case for it decline.
type LiteralExpr struct {
	Val Value

	// aff is the substituted column's own type affinity (affNone for an
	// ordinary literal, which is what sqlite3ExprAffinity answers for one).
	aff affinity
	// materialized says the value came from a real, stored column, so it
	// defends its storage class in a comparison (isMaterializedRef).
	materialized bool
	// coll is the substituted column's DECLARED collating sequence, "" for an
	// ordinary literal and for a column that has none to contribute (the rowid
	// pseudo-column) -- the same "" convention groupBareColExpr uses.
	coll string

	// deferredErr, when non-"", is a parse-time error literalNumber would have
	// raised, held so a CREATE TRIGGER parse can finish. C checks a numeric
	// literal's magnitude at code generation (codeInteger, expr.c:4330-4362,
	// from expr.c:5109-5111), which for a trigger body happens per firing
	// statement (sqlite3CodeRowTrigger, trigger.c:1468), never at CREATE TRIGGER
	// (sqlite3FinishTrigger, trigger.c:323-410). Set by parsePrimary's tkNumber
	// case only inside a trigger body; raised by compileExpr's LiteralExpr case.
	deferredErr string
}

// ColumnExpr is a bare column reference, optionally written table.column.
// Qualifier, if non-empty, is matched (case-insensitively) against a table
// alias/name found while walking the evalCtx scope chain outward -- see
// resolveColumn. This is what lets a correlated subquery use
// e.g. "x.a" for its own (aliased) table and "t1.a" for the enclosing
// query's table.
type ColumnExpr struct {
	// Schema, if non-empty, is the database-qualifier of a THREE-part column
	// reference "schema.table.column" (e.g. "main" in "main.t.c"). "" for the
	// ordinary two-part "table.column" or bare "column" forms. Validated (and
	// otherwise ignored) exactly like FromItem.Schema: a locally-resolving
	// qualifier is treated as if the reference were the plain two-part
	// "Qualifier.Name"; a non-resolving one (temp., a foreign attached
	// database, or "main" under TEMP taint) declines the read. See
	// qualifierResolvesLocally (schema_qualifier.go) and its use in
	// validateColumnRefs (query.go) and compileColumn (vdbe_codegen.go).
	Schema string

	Qualifier string // "" if the reference was unqualified
	Name      string

	// UsingRepr is true only for the representative-table side of a
	// USING/NATURAL condition synthesized by desugarJoinItem; false (the zero
	// value) for every other ColumnExpr, including a user-written "t1.a",
	// which always reads t1's raw value.
	//
	// The synthesized condition is a qualified equality against the
	// representative by scope name, but it means the representative's logical
	// value, COALESCEd when a RIGHT/FULL fallback applies
	// (tableScope.coalesceFallback), as a bare "a" would. This flag lets the
	// qualified resolver branch tell the two apart.
	UsingRepr bool

	// UsingReprOwnItem, meaningful only with UsingRepr, is the FROM-item index
	// of the join item this synthesized condition belongs to (the right side
	// of the "=").
	//
	// When this item is itself the RIGHT/FULL JOIN that installed the
	// representative's fallback, the representative has not been
	// NULL-extended at this step; reading COALESCE(t1.a, t2.a) would make the
	// condition true for every row whose t1.a is NULL ("t1 RIGHT JOIN t2
	// USING(a)" with a NULL-keyed t1 row must match no t2 row). So the
	// fallback applies only when UsingReprOwnItem is strictly later than the
	// installing item: a later join's condition on an earlier RIGHT/FULL
	// join's representative (joinB.test).
	UsingReprOwnItem int

	// UsingPinned/UsingPinnedItem bind this reference directly to FROM item
	// UsingPinnedItem of the innermost scope level, bypassing name matching.
	// Set only by desugarJoinItem, for a side of a synthesized USING/NATURAL
	// equality its scope name cannot reach:
	//
	//   - an unaliased derived table ("... NATURAL JOIN (SELECT ...)"), which
	//     has no scope name;
	//   - an unaliased self-join ("t1 NATURAL JOIN t1", "t1 x JOIN t2 x
	//     USING(a)", "t1 NATURAL JOIN main.t1"), whose name reaches only the
	//     first item.
	//
	// Without it both sides resolve to the same item and the condition becomes
	// the tautology "t1.a = t1.a" (9 rows where C returns 2). The
	// representative side needs the pin too: an unqualified read applies a
	// RIGHT/FULL fallback without the UsingReprOwnItem guard.
	//
	// The index is the item's absolute FROM position, which equals its scope
	// position only without a parenthesized join group; a group alongside
	// either shape is declined (checkDerivedJoinSupported,
	// checkSelfJoinGroupSupported). A bool so the zero value means "not
	// pinned".
	UsingPinned     bool
	UsingPinnedItem int

	// FallbackLiteral, if non-nil, is the value this reference takes when no
	// column matches anywhere in scope: SQLite's TRUE/FALSE keywords. The parser
	// (parsePrimary) sets it only for a bare, unqualified, unquoted "true"/"false"
	// (any case), to 1/0; nil otherwise.
	//
	// A matching column wins ("CREATE TABLE t1(true INT); SELECT true FROM t1"
	// reads the column), so it is decided at resolution. "t1.true" never falls
	// back, and neither does a quoted spelling ("true", [true], `true`), which
	// C's tokenizer makes a plain ID, bypassing the keyword table.
	//
	// Consumed wherever an unqualified reference could fail with "no such column":
	// compileColumn, columnRowValue (row_scope.go) and validateColumnRefs
	// (query.go). "ambiguous column name" is not fallback-eligible
	// (unqualifiedColumnNotFoundErr).
	FallbackLiteral *Value

	// R32NDQSpan is the byte span of this reference's bare double-quoted name
	// token (quotes included) in the parsed text; zero otherwise, and recorded
	// only while parser.r32nDQRecord is on, which only the renameFixQuotes port
	// (alter_write.go) sets. sameWindowSpec's reflect.DeepEqual is the only
	// whole-tree comparison, and it never sees a non-zero span.
	//
	// renameFixQuotes (alter.c:646, via sqlite_rename_quotefix, alter.c:1937)
	// maps a resolution decision back to a byte offset: it rewrites the TK_STRING
	// nodes carrying EP_DblQuoted (renameQuotefixExprCb, alter.c:1903), the
	// identifiers lookupName demoted to strings because they named no column
	// (resolve.c:721). Only a bare unqualified "..." is eligible, as in
	// identLiteralFallback.
	R32NDQSpan byteSpan
}

// UnaryExpr is a prefix operator: "-", "+", "NOT", or "~" (bitwise NOT).
type UnaryExpr struct {
	Op string
	X  Expr
}

// BinaryExpr is an infix operator: one of
// "||" "*" "/" "%" "+" "-" "<" "<=" ">" ">=" "=" "==" "!=" "<>" "AND" "OR"
// "IS" "IS NOT".
type BinaryExpr struct {
	Op   string
	L, R Expr
	// row is set on the ROOT of a tree the parser desugared a row-value
	// comparison into (desugarRowCompare and friends, sql_parser.go), and
	// names the comparison as written. Execution never reads it: the tree
	// below is the whole meaning. The planner does, because C's never sees
	// that tree -- exprAnalyze analyses the vector comparison itself
	// (whereexpr.c:1080, 1455-1520), and pricing "a>3 OR (a=3 AND b>1)" in
	// its place walks a different index. See where_plan_rowvalue.go.
	row *rowValueOrigin
}

// IsNullExpr is "X IS NULL" / "X IS NOT NULL" (equivalently ISNULL/NOTNULL).
type IsNullExpr struct {
	X   Expr
	Not bool
}

// InExpr is "X [NOT] IN (list...)", "X [NOT] IN (SELECT ...)", or "X [NOT]
// IN <table-name>" (SQLite's bare-table-name form, meaning membership in
// that table's rows). Exactly one of List or Sub is set: List for the
// value-list form, Sub for both subquery-shaped forms -- the parser
// (parseInRHS, sql_parser.go) desugars "IN <table-name>" into Sub = "SELECT
// * FROM <table-name>" at parse time, since that's exactly the subquery form
// SQLite itself documents as equivalent. Sub's SelectStmt must yield exactly
// one column -- checked at compile time in compileInSubquery
// (vdbe_codegen.go), which is what rejects (rather than guesses at) a
// table-name form naming a table with more than one column. Both forms share
// the same membership/NULL semantics; see compileIn's doc comment.
type InExpr struct {
	X    Expr
	List []Expr
	Sub  *SelectStmt
	Not  bool
}

// BetweenExpr is "X [NOT] BETWEEN Lo AND Hi".
type BetweenExpr struct {
	X, Lo, Hi Expr
	Not       bool
}

// LikeExpr is "X [NOT] LIKE Pattern [ESCAPE Escape]". Escape is nil without
// an ESCAPE clause; otherwise any expression (evaluated per row, not required
// constant) that must be NULL (propagates) or a single character (rune, not
// byte); anything else is "ESCAPE expression must be a single character",
// raised on every row it is evaluated for, even when X or Pattern is NULL.
// See likeMatch for the escaping semantics.
type LikeExpr struct {
	X, Pattern Expr
	Escape     Expr // nil if no ESCAPE clause
	Not        bool
}

// GlobExpr is "X [NOT] GLOB Pattern". Unlike LikeExpr, GLOB is
// case-SENSITIVE, has no ESCAPE clause, and uses '*'/'?'/'[...]' wildcards
// instead of '%'/'_' -- see globMatch for the exact,
// verified-against-real-SQLite matching semantics.
type GlobExpr struct {
	X, Pattern Expr
	Not        bool
}

// MatchExpr is "X MATCH Pattern" (and "X NOT MATCH Pattern"). X names either a
// virtual table (the whole-table form, "t MATCH q") or one of its columns
// ("col MATCH q" / "t.col MATCH q"); Pattern is the query string. Only the fts5
// virtual table gives MATCH a meaning here -- evalMatch (fts5_match.go) errors
// (exactly as C SQLite does) when X is not an fts5 table or column. The VDBE
// lowers it to OpMatch (compileMatchExpr, vdbe_codegen.go), which hands evalMatch
// the current row's whole set of table scopes rather than a single register.
type MatchExpr struct {
	X, Pattern Expr
	Not        bool
}

// CollateExpr is "X COLLATE name": a postfix operator (precedence: see
// parseCollate) attaching a collation to X without changing its value; it
// compiles to X's code alone, as sqlite3ExprCodeTarget does
// (expr.c:5547-5548). It decides the collation of comparisons, ORDER BY, GROUP
// BY and DISTINCT over X or expressions built from it (exprCollation). Name is
// as written, for "no such collation sequence: %s", and is validated at parse
// time against BINARY/NOCASE/RTRIM.
type CollateExpr struct {
	X    Expr
	Name string
}

// FuncExpr is a function call, scalar (supportedFuncs) or aggregate. Star is
// set for "name(*)" (meaningful only for count(*); rejected elsewhere at plan
// time). Distinct is set for "DISTINCT" as the first argument token; it is
// parsed for any call, as in SQLite's grammar, and gives dedup only for an
// aggregate (planAggregateCall). A scalar call ignores it, as C does
// ("abs(DISTINCT x)" is abs(x)).
type FuncExpr struct {
	Name     string
	Args     []Expr
	Star     bool
	Distinct bool

	// NameAsWritten is the call's spelling before Name was folded, kept only
	// because one diagnostic echoes it VERBATIM: C SQLite reports
	// "LIKELIHOOD()" for an upper-case call and "likelihood()" for a
	// lower-case one (verified directly). Empty for a FuncExpr this package
	// synthesizes rather than parses, in which case Name is the spelling.
	NameAsWritten string

	// Over, if non-nil, makes this a WINDOW-FUNCTION call ("<fn>(...) OVER
	// <window>"): the call is evaluated per row over a partition/frame of the
	// result set rather than as a plain scalar (row-mode) or aggregate
	// (collapsing) call. See sql_window.go for execution and the WindowSpec
	// type below for the parsed window definition. A FuncExpr with Over set is
	// NOT treated as a plain aggregate by isAggregateCall/containsAggregate --
	// window aggregates have entirely different one-row-per-input semantics.
	Over *WindowSpec

	// Filter, if non-nil, is the "FILTER (WHERE <cond>)" modifier that can
	// precede OVER on an aggregate window function. Parsed so it can be cleanly
	// DECLINED (an unsupported error), never silently ignored -- see
	// sql_window.go.
	Filter Expr

	// OrderBy is an aggregate call's own ORDER BY: "group_concat(x ORDER BY y
	// DESC)", SQLite 3.44's aggregate ORDER BY. The grammar is parse.y:1238
	// ("idj LP distinct exprlist ORDER BY sortlist RP") and the node C builds
	// for it is a TK_ORDER on the call's pLeft (sqlite3ExprAddFunctionOrderBy,
	// expr.c:1219). Every walker that visits Filter must visit these terms
	// too: they are per-row expressions of the aggregate's own source, and
	// C's resolver and its owner test both walk them (resolve.c:1326-1330,
	// expr.c:7215-7220). Nil for a zero-argument call, where C drops the
	// clause unresolved (expr.c:1238-1242). See aggItem.orderBy.
	OrderBy []OrderTerm

	// CurrentTimeKw marks the three KEYWORD spellings CURRENT_DATE,
	// CURRENT_TIME and CURRENT_TIMESTAMP, which the parser lowers to date(),
	// time() and datetime(). In C they are three REGISTERED FUNCTIONS of
	// their own (currentTimeFunc, date.c), and the difference is observable
	// in exactly one place: currentTimeFunc does not call sqlite3NotPureFunc,
	// so CURRENT_DATE inside a CHECK constraint is legal where date() is
	// "non-deterministic use of date() in a CHECK constraint" -- while the
	// RESOLVER refuses all four alike inside a generated column or an index
	// (resolve.c:1228), since none of them is SQLITE_FUNC_CONSTANT. Verified
	// against 3.53.3 on all four shapes.
	CurrentTimeKw bool
}

// orderByExprs is x.OrderBy's expressions, for the walkers that visit every
// per-row expression of a call (see FuncExpr.OrderBy).
func (x FuncExpr) orderByExprs() []Expr {
	if len(x.OrderBy) == 0 {
		return nil
	}
	out := make([]Expr, len(x.OrderBy))
	for i, t := range x.OrderBy {
		out[i] = t.Expr
	}
	return out
}

// walkArgs is x.Args followed by x's ORDER BY expressions: what a read-only
// walker asking "does this call mention X" must visit, since C's resolver and
// its reference tests visit both (FuncExpr.OrderBy). A walker that REBUILDS
// the call must not use it -- the terms go back into OrderBy, not Args.
func (x FuncExpr) walkArgs() []Expr {
	if len(x.OrderBy) == 0 {
		return x.Args
	}
	return append(append([]Expr(nil), x.Args...), x.orderByExprs()...)
}

// RaiseExpr is "RAISE(...)", valid only inside a trigger body or WHEN (else
// "RAISE() may only be used within a trigger-program", as in C). Ignore is set
// for RAISE(IGNORE); otherwise Action is conflictRollback/Abort/Fail and Msg
// the message expression (evaluated at fire time, coerced to text; NULL gives
// "constraint failed"). It never yields a value: OpRaise raises errRaiseIgnore
// (caught by the firing row loop, which abandons the row's remaining triggers
// and, for BEFORE, its write) or a *raiseError carrying the conflict scope. See
// trigger.go.
type RaiseExpr struct {
	Ignore bool
	Action conflictAction // meaningful only when !Ignore
	Msg    Expr           // message expression, only when !Ignore
}

func (RaiseExpr) exprNode() {}

// FrameMode selects the row-vs-value framing rule of a window frame.
type FrameMode int

const (
	// FrameRange frames by ORDER-BY VALUE peers: CURRENT ROW includes every
	// row whose ORDER BY key equals the current row's (its "peers").
	FrameRange FrameMode = iota
	// FrameRows frames by physical row COUNT within the partition.
	FrameRows
	// FrameGroups frames by peer-GROUP count -- parsed only so it can be
	// declined (unsupported), never guessed at.
	FrameGroups
)

// FrameBoundType is one endpoint kind of a window frame's BETWEEN ... AND ...
type FrameBoundType int

const (
	FrameUnboundedPreceding FrameBoundType = iota
	FramePreceding                         // "<N> PRECEDING"
	FrameCurrentRow
	FrameFollowing // "<N> FOLLOWING"
	FrameUnboundedFollowing
)

// FrameBound is one endpoint of a window frame.
type FrameBound struct {
	Type   FrameBoundType
	Offset Expr // the "<N>" for FramePreceding/FrameFollowing; nil otherwise
}

// FrameExclude selects which rows an "EXCLUDE ..." clause removes from the
// otherwise-computed frame.
type FrameExclude int

const (
	// ExcludeNoOthers ("EXCLUDE NO OTHERS", or no EXCLUDE at all) removes
	// nothing -- the frame is used as computed.
	ExcludeNoOthers FrameExclude = iota
	// ExcludeCurrentRow ("EXCLUDE CURRENT ROW") removes just the current row.
	ExcludeCurrentRow
	// ExcludeGroup ("EXCLUDE GROUP") removes the current row and all its peers
	// (rows sharing its ORDER BY value).
	ExcludeGroup
	// ExcludeTies ("EXCLUDE TIES") removes the current row's peers but keeps
	// the current row itself.
	ExcludeTies
)

// WindowFrame is an explicit "ROWS|RANGE|GROUPS BETWEEN <start> AND <end>"
// frame specification. Exclude is set when an "EXCLUDE ..." clause was written;
// ExcludeKind records which kind (meaningful only when Exclude is true).
type WindowFrame struct {
	Mode        FrameMode
	Start       FrameBound
	End         FrameBound
	Exclude     bool
	ExcludeKind FrameExclude
}

// WindowSpec is a parsed window definition: the contents of "OVER (...)", the
// referenced name of "OVER <windowname>", or a top-level "WINDOW <name> AS
// (...)" definition. Ref, if non-empty, names a base window (the bare
// "OVER win" form, or the leading name inside "OVER (base ORDER BY ...)"); the
// referencing spec inherits the base's PARTITION BY (and ORDER BY/frame when
// it doesn't supply its own) -- see resolveWindowSpec (sql_window.go).
type WindowSpec struct {
	Ref         string
	PartitionBy []Expr
	OrderBy     []OrderTerm
	Frame       *WindowFrame
}

// NamedWindow is one entry of a top-level "WINDOW <name> AS (<spec>)" clause.
type NamedWindow struct {
	Name string
	Spec *WindowSpec
}

// CastExpr is "CAST(X AS Type)"; Type is one of
// INTEGER/REAL/TEXT/BLOB/NUMERIC (upper-cased).
type CastExpr struct {
	X    Expr
	Type string
}

// WhenClause is one "WHEN <when> THEN <then>" arm of a CaseExpr. For a
// searched CASE, When is a boolean condition; for a simple CASE, When is a
// value compared against CaseExpr.Base using "=" semantics.
type WhenClause struct {
	When Expr
	Then Expr
}

// CaseExpr is a CASE expression, either form:
//
//	searched: CASE WHEN <when> THEN <then> ... [ELSE <else>] END  (Base == nil)
//	simple:   CASE <base> WHEN <when> THEN <then> ... [ELSE <else>] END
type CaseExpr struct {
	Base  Expr // nil for the searched form
	Whens []WhenClause
	Else  Expr // nil if no ELSE clause

	// isBool marks desugarIsBool's "X IS [NOT] TRUE/FALSE" (sql_parser.go),
	// which C builds as a TK_TRUTH (resolve.c) rather than a TK_CASE -- a
	// difference only whereUsablePartialIndex's tree comparison
	// (where_plan_partial.go) and compileCase's keyword-or-column test
	// (vdbe_codegen.go) can see.
	isBool bool
	// isBoolFalse marks the FALSE form of an isBool node, whose keyword
	// stands in ELSE rather than THEN (desugarIsBool).
	isBoolFalse bool
}

// ParamExpr is a bound-parameter reference: "?", "?NNN", ":name", "@name", or
// "$name", numbered as sqlite3_bind_parameter_index does (paramTracker). Index
// is the resolved 1-based number; Name is the raw text including its sigil for
// a named parameter, "" for "?"/"?NNN". ":foo" and "@foo" are distinct
// parameters in C, hence the sigil. It compiles to OpVariable; an index beyond
// the bound arguments reads NULL, as for an unbound parameter.
type ParamExpr struct {
	Index int
	Name  string // "" for an anonymous ?/?NNN parameter
}

// SubqueryExpr is a parenthesized SELECT used as a scalar expression: it
// yields the first column of the first result row, or NULL if the subquery
// returns no rows (an error if it returns more than one column). Stmt may
// reference the enclosing query's columns (a correlated subquery); see
// evalCtx.outer and resolveColumn.
type SubqueryExpr struct {
	Stmt *SelectStmt
}

// ExistsExpr is "[NOT] EXISTS (SELECT ...)". Unlike SubqueryExpr, the
// subquery's column count and values are irrelevant -- only whether it
// yields at least one row -- so a multi-column or "SELECT *" body is fine.
// The result is always 1 or 0, never NULL. Not is set when this node is
// parsed directly from "NOT EXISTS (...)"; "NOT (EXISTS (...))" written via
// a separate top-level NOT instead produces UnaryExpr{Op: "NOT", X:
// ExistsExpr{...}}, which evaluates identically since ExistsExpr never
// yields NULL.
type ExistsExpr struct {
	Stmt *SelectStmt
	Not  bool
}

// RowExpr is a parenthesized row value of two or more elements, "(a, b)".
// SQLite allows one only as an operand of a row-value comparison; elsewhere it
// is "row value misused". Most supported shapes are desugared at parse time
// into scalar boolean trees (desugarRowCompare for "=" "==" "!=" "<>" "IS" "IS
// NOT" and, via desugarRowLex, "<" "<=" ">" ">="; desugarRowBetween;
// desugarRowIn over a value list), so the node rarely survives parsing.
//
// Two shapes keep the node because the subquery side must be evaluated once:
// "(a,b) IN (SELECT x,y FROM t)" (compileInSubquery) and "(a,b) <op> (SELECT
// x,y FROM t)" in either order (compileRowSubCompare -> OpRowSub).
//
// Any other surviving RowExpr is declined by compileExpr's default case: a
// scalar misuse, an arity mismatch, a row value in CASE, "IN (VALUES ...)",
// BETWEEN with a subquery, two subquery operands, "=" / "==" / "IS" whose
// collation comes from the subquery (see compileRowSubCompare), and an element
// random()/randomblob() makes unsafe to duplicate (desugarRowLex).
type RowExpr struct {
	Elems []Expr
}

func (LiteralExpr) exprNode()  {}
func (ParamExpr) exprNode()    {}
func (ColumnExpr) exprNode()   {}
func (UnaryExpr) exprNode()    {}
func (BinaryExpr) exprNode()   {}
func (IsNullExpr) exprNode()   {}
func (InExpr) exprNode()       {}
func (BetweenExpr) exprNode()  {}
func (LikeExpr) exprNode()     {}
func (GlobExpr) exprNode()     {}
func (MatchExpr) exprNode()    {}
func (CollateExpr) exprNode()  {}
func (FuncExpr) exprNode()     {}
func (CastExpr) exprNode()     {}
func (CaseExpr) exprNode()     {}
func (SubqueryExpr) exprNode() {}
func (ExistsExpr) exprNode()   {}
func (RowExpr) exprNode()      {}

// exprContainsSubquery reports whether e's expression tree contains a subquery
// in any form -- a scalar SubqueryExpr, an [NOT] EXISTS ExistsExpr, or an
// InExpr's "[NOT] IN (SELECT ...)" (InExpr.Sub) -- WITHOUT descending into any
// such subquery's own body (a subquery is its own scope). Used by the VDBE
// join codegen to decline a subquery inside a JOIN ON condition (see
// emitJoinLevel, vdbe_join_codegen.go).
func exprContainsSubquery(e Expr) bool {
	switch x := e.(type) {
	case SubqueryExpr, ExistsExpr:
		return true
	case UnaryExpr:
		return exprContainsSubquery(x.X)
	case BinaryExpr:
		return exprContainsSubquery(x.L) || exprContainsSubquery(x.R)
	case IsNullExpr:
		return exprContainsSubquery(x.X)
	case InExpr:
		if x.Sub != nil {
			return true
		}
		if exprContainsSubquery(x.X) {
			return true
		}
		for _, it := range x.List {
			if exprContainsSubquery(it) {
				return true
			}
		}
		return false
	case BetweenExpr:
		return exprContainsSubquery(x.X) || exprContainsSubquery(x.Lo) || exprContainsSubquery(x.Hi)
	case LikeExpr:
		return exprContainsSubquery(x.X) || exprContainsSubquery(x.Pattern)
	case GlobExpr:
		return exprContainsSubquery(x.X) || exprContainsSubquery(x.Pattern)
	case MatchExpr:
		return exprContainsSubquery(x.X) || exprContainsSubquery(x.Pattern)
	case CollateExpr:
		return exprContainsSubquery(x.X)
	case FuncExpr:
		for _, a := range x.walkArgs() {
			if exprContainsSubquery(a) {
				return true
			}
		}
		return false
	case CastExpr:
		return exprContainsSubquery(x.X)
	case RowExpr:
		for _, el := range x.Elems {
			if exprContainsSubquery(el) {
				return true
			}
		}
		return false
	case CaseExpr:
		if x.Base != nil && exprContainsSubquery(x.Base) {
			return true
		}
		for _, w := range x.Whens {
			if exprContainsSubquery(w.When) || exprContainsSubquery(w.Then) {
				return true
			}
		}
		return x.Else != nil && exprContainsSubquery(x.Else)
	default:
		return false
	}
}

// ParamInfo is one parsed statement's bound-parameter shape, computed by the
// parser (paramTracker). NumParams is sqlite3_bind_parameter_count, the
// highest index used anywhere (including an otherwise unused "?NNN").
// ParamNames maps each named parameter's raw text, sigil included (":x" and
// "@x" differ), to its 1-based index, for BindByName. With no parameters,
// NumParams is 0 and ParamNames nil.
type ParamInfo struct {
	NumParams  int
	ParamNames map[string]int
}

// BindByName builds a positional args slice (sized to info.NumParams, one
// entry per resolved parameter index, ready to pass as QueryArgs/ExecArgs'
// args) from a name->Value binding, using info's ParamNames map to resolve
// each name to its index. A name with no entry in named, and any index with
// no bound name at all (an anonymous "?"/"?NNN" parameter, or a named
// parameter the caller simply didn't supply), is left at its zero Value
// (Typ: Null) -- exactly SQLite's own unbound-parameter-is-NULL behavior
// (see ParamExpr's doc comment in this file). named's keys must include each
// parameter's sigil (":x", "@y", or "$z"), matching ParamInfo.ParamNames'
// own keys.
func BindByName(info ParamInfo, named map[string]Value) []Value {
	args := make([]Value, info.NumParams)
	for name, idx := range info.ParamNames {
		if v, ok := named[name]; ok && idx >= 1 && idx <= len(args) {
			args[idx-1] = v
		}
	}
	return args
}

// SelectColumn is one item of the select-list: either "*"/"<qualifier>.*" or
// an expression with an optional alias.
type SelectColumn struct {
	Star bool
	// StarQualifier is "" for a bare "*" (every joined table's columns); for
	// "t1.*"/"x.*" it holds the qualifier text ("t1"/"x"), matched against a
	// FROM-item's scope name (alias, or its own table name if unaliased)
	// exactly like an ordinary qualified ColumnExpr -- see expandSelectList
	// (query.go) and resolveColumn. Meaningless unless Star is
	// true.
	StarQualifier string
	Expr          Expr
	Alias         string // meaningful only if HasAlias; "" otherwise
	// HasAlias reports whether an alias was written at all (AS or bare,
	// identifier or string; aliasTokenText), as distinct from Alias being "": "AS
	// ''" sets HasAlias with Alias=="" and names the column "", not the source
	// text (misc1.test: "CREATE TABLE t19 AS SELECT 1, 2 AS '', 3" has a middle
	// column named "").
	HasAlias bool
	Text     string // verbatim source text of Expr, for default column naming (only used when !HasAlias)
	// RawText is the verbatim source-text SPAN of Expr exactly as typed
	// (whitespace between tokens preserved, qualifier NOT stripped) --
	// UNLIKE Text, which for a bare ColumnExpr is reduced to just the
	// column identifier. It is the result-column name C SQLite reports
	// for a column reference ONLY under "PRAGMA short_column_names=OFF" with
	// "full_column_names=OFF" (its else-branch "span" name), e.g. "SELECT
	// test1 . f1 FROM test1" is named "test1 . f1" and "SELECT oid FROM t"
	// is named "oid". Never consulted under the default pragma state (where
	// Text / the declared column name is used instead -- see
	// expandSelectList, query.go), so it does not affect default naming.
	RawText string
}

// NullsOrder selects where NULLs sort within an ORDER BY term, overriding
// the direction-derived default. See OrderTerm.Nulls.
type NullsOrder int

const (
	// NullsDefault means no explicit NULLS FIRST/LAST clause was given: NULLs
	// sort as the smallest value, i.e. first for ASC, last for DESC (SQLite's
	// baseline behavior -- verified against mattn/go-sqlite3).
	NullsDefault NullsOrder = iota
	// NullsFirst is an explicit "NULLS FIRST": NULLs sort before all
	// non-NULLs regardless of ASC/DESC.
	NullsFirst
	// NullsLast is an explicit "NULLS LAST": NULLs sort after all non-NULLs
	// regardless of ASC/DESC.
	NullsLast
)

// OrderTerm is one ORDER BY term.
type OrderTerm struct {
	Expr  Expr
	Desc  bool
	Nulls NullsOrder
}

// NullsFirst reports whether, for this term's effective ordering (explicit
// NULLS clause or the ASC/DESC-derived default), NULLs sort before
// non-NULLs. This is the single source of truth consulted by every sort
// comparator (orderTermLess, value_compare.go) so the default and the
// explicit override stay in agreement everywhere.
func (t OrderTerm) NullsFirst() bool {
	switch t.Nulls {
	case NullsFirst:
		return true
	case NullsLast:
		return false
	default:
		// Default: NULL is the smallest value, so NULLs come first in
		// ascending order and last in descending order.
		return !t.Desc
	}
}

// JoinKind identifies how one FromItem is joined onto the tables that
// precede it in a SelectStmt.From list. It is meaningless for From[0] (the
// base table), which isn't "joined" onto anything.
type JoinKind int

const (
	// JoinCross is a comma join ("FROM t1, t2") or an explicit "CROSS JOIN":
	// no ON condition, no NULL-extension -- every combination of rows from
	// the tables joined so far and this table is a candidate, filtered only
	// by WHERE (or, for an explicit "CROSS JOIN t ON ...", by that ON
	// condition, applied exactly like JoinInner's -- see the parser).
	JoinCross JoinKind = iota
	// JoinInner is "[INNER] JOIN t [ON <cond>]": cond, if present, is
	// applied inline as each candidate row is considered, exactly like a
	// pushed-down WHERE conjunct (no NULL-extension). The join-constraint is
	// OPTIONAL (verified against C SQLite): with no ON/USING/NATURAL at
	// all, it's a plain cross product, identical to JoinCross -- see the
	// parser (sql_parser.go's parseFromClause), which leaves On/Using nil in
	// that case exactly as it already does for a comma/CROSS join.
	JoinInner
	// JoinLeft is "LEFT [OUTER] JOIN t [ON <cond>]": if no row of t
	// satisfies cond for a given combination of the preceding tables' rows,
	// one row is still emitted with every column of t set to NULL. See
	// join.go's package doc comment for the ON-vs-WHERE distinction this
	// preserves. cond (and USING/NATURAL) is OPTIONAL: with none present, the
	// condition is always-true (nil On, exactly like an unconstrained
	// JoinInner above), so every row of t matches every preceding
	// combination -- meaning the NULL-extension case above can only ever
	// fire when t is genuinely empty (verified against C SQLite).
	JoinLeft
	// JoinRight is "RIGHT [OUTER] JOIN t [ON <cond>]": every row of t appears at
	// least once. A row of t that matches no combination of the preceding tables
	// (the whole search space, WHERE ignored) is emitted once, after the main
	// loop, with every preceding table NULL (emitRightOuterSweep). Column order is
	// unchanged. cond (or USING/NATURAL) is optional; with none, on is nil
	// ("always matches"), so the extension fires only when the preceding search
	// space is empty.
	JoinRight
	// JoinFull is "FULL [OUTER] JOIN t [ON <cond>]": JoinLeft (inline
	// NULL-extension of t per unmatched preceding combination) plus JoinRight's
	// sweep (t's unmatched rows, NULL-extending every preceding table), which
	// matches C's row order and NULL pattern. cond is optional: with none,
	// non-empty sides give a cross product, an empty side's extension still
	// fires, and two empty sides give no rows.
	JoinFull
)

// FromItem is one table reference in SelectStmt.From: a table name (or a
// subquery for a derived table), an optional alias, and, for every item but
// the first, how it joins to the tables before it. On is nil for From[0], a
// comma/CROSS join, and any join with no ON/USING/NATURAL (all optional, as in
// C); nil means "always matches" (desugarJoinItem, emitJoinLevel).
type FromItem struct {
	Table string

	// Schema, if non-empty, is the database-qualifier written in front of the
	// table name ("main"/"temp"/an ATTACHed database name) -- e.g. "main" for
	// "FROM main.t". "" for an ordinary unqualified reference (the common
	// case). Consumed by resolveFrom (join.go): a qualifier that resolves
	// against this pager's own schema (qualifierResolvesLocally, see
	// schema_qualifier.go) is stripped and the table resolved normally; one
	// that does not (temp., a foreign attached database, or "main" under TEMP
	// taint) declines with a "no such table" error. The driver Conn routes
	// a statement referencing an ATTACHed database to that database's own file
	// BEFORE it reaches the engine, so a genuine attached-name qualifier only
	// ever reaches here already matching the owning file's local schema name.
	Schema string

	Alias string // "" if this table reference has no alias
	Join  JoinKind
	On    Expr

	// Subquery, if non-nil, makes this item a derived table: the "( SELECT ... )"
	// whose rows are this item's rows (Table is ""; only Alias names it). Its
	// columns are the subquery's result names. It is uncorrelated (no LATERAL):
	// run once with no access to sibling or outer columns ("no such column", as
	// in C). resolveFrom and resolveJoinSources turn it into a materialized row
	// source. "(SELECT ...) x(c1, c2)" is a syntax error in C and here.
	Subquery *SelectStmt

	// TableFunc marks this FROM item as a TABLE-VALUED FUNCTION call -- the
	// "name(arg, arg, ...)" form, e.g. "FROM generate_series(1,10)" (vtab.go).
	// Table names the (eponymous) virtual-table module; TableFuncArgs holds the
	// parenthesized argument expressions (possibly empty for "name()"). Each
	// argument binds positionally to the module's i-th HIDDEN column, exactly
	// like SQLite's table-valued-function argument-to-hidden-column mapping. nil
	// TableFuncArgs with TableFunc false is an ordinary table reference.
	TableFunc     bool
	TableFuncArgs []Expr

	// tvfWhere carries the statement's top-level WHERE conjuncts down to a
	// virtual-table row source so hidden-column EQ constraints ("FROM
	// generate_series WHERE start=1 AND stop=10") reach the module's
	// BestIndex/Filter, as xBestIndex takes them from the WHERE. Set only by
	// withVtabWhere (vtab.go); elsewhere nil, and a vtab that needs bounds
	// declines.
	tvfWhere []Expr

	// tvfOmitPlus1 is 1 + the index into tvfWhere of the conjunct compileScanPlain
	// removed from the residual WHERE because this item's module consumes and
	// guarantees it (omittingVtabModule); 0 for none. +1 so the zero value means
	// "none".
	//
	// Stamped only for sources whose WHERE the scan compiler actually rebuilt,
	// never by resolveVtabSource (which also sees items nested in a join group
	// resolveJoinSources does not return). A module reads it via
	// VtabConstraint.Omitted and must keep declining what it cannot serve without
	// the omission, so a path that skips the rewrite loses an answer rather than a
	// row.
	tvfOmitPlus1 int

	// fts3ContentUnneeded marks a FROM item proved to need no read of its
	// %_content (fts3StmtContentUnneeded). Set with tvfWhere by withVtabWhere and
	// read only by materializeFts3Ascending, which then also offers docids the
	// index holds but %_content lacks; the same predicate gates fts3MatchDocids'
	// existence check, so the two cannot disagree.
	fts3ContentUnneeded bool

	// fts3ContentWanted, when non-nil, is fts3StmtContentUnneeded's own
	// pre-gathered lazy term set for THIS item's MATCH (fts3WantedTermsForMatch,
	// fts3_search.go) -- set alongside fts3ContentUnneeded, by the same call,
	// so materializeFts3Ascending's extra-candidate-docid lookup can stay LAZY
	// (only decode the terms this MATCH could ever touch) instead of loading
	// the table's whole segment index, which would defeat fts3LoadIndex's own
	// lazy-per-token reader for exactly the query shapes fts3ContentUnneeded
	// marks true. nil (fall back to an eager whole-index load, still safe,
	// only not lazy) whenever the MATCH's pattern isn't a compile-time literal.
	fts3ContentWanted *fts3WantedTerms

	// Using is the column-name list from "JOIN ... USING (a, b, ...)", or nil
	// if this item wasn't joined with USING. Mutually exclusive with On and
	// Natural -- the parser rejects any combination (parseFromClause). The
	// effective join condition ("t_left.a = t_right.a AND ...", t_left being
	// the earliest FROM item sharing that column name -- see join.go's
	// desugarJoinItem) is computed at plan time, once the joined tables'
	// schemas are known, not by the parser.
	Using []string

	// Natural is true for a "NATURAL [INNER|LEFT|CROSS] JOIN" item: the
	// effective join condition is exactly as if USING(...) had named every
	// column common to this table and the tables joined so far (case-
	// insensitively) -- or no condition at all or if there's no such common
	// column (a true cross join). Mutually exclusive with On and Using. See
	// join.go's desugarJoinItem for the exact computation (it must run at
	// plan time: the parser alone can't know either table's columns).
	Natural bool

	// IndexedBy is the index name named by an "INDEXED BY <name>" hint, or
	// "" if this table reference had no such hint (including a "NOT
	// INDEXED" one, which needs no name at all). resolveFrom (join.go)
	// validates the NAME, matching C SQLite's own "no such index" rejection
	// of a stale/misspelled one; the ported planner walks only that index
	// (wherePlanIndexList); and a PARTIAL index the WHERE does not imply is
	// C's "no query solution" (where_plan_indexed_by.go).
	IndexedBy string

	// NotIndexed is the "NOT INDEXED" hint, which takes no name of its own.
	// Like IndexedBy it is NOT a no-op for this engine: whereLoopAddBtree
	// (where.c) chains a table's real indexes onto its fake sPk only when
	// "pSrc->fg.notIndexed==0", so NOT INDEXED makes SQLite full-scan the table
	// -- in ROWID order -- where it would otherwise have walked an index, in
	// that index's key order. The ported planner therefore has to see it
	// (markWherePlanIndexEligibility, where_plan_gate.go); it was measured as a
	// wrong row ORDER on evidence/slt_lang_aggfunc.test, whose
	// "SELECT group_concat(x) FROM t1 NOT INDEXED" reads 1,0,2,2 against an
	// index-ordered 0,1,2,2.
	NotIndexed bool

	// flattened marks a table flattenSubquery moved up out of a view, CTE or
	// FROM-subquery body (flatten_projection.go). Execution reads it as the
	// table it is; the one question it changes is C's pSTab identity, which was
	// settled before flattening against the VIEW -- see returningSubqueryLifetimes.
	flattened bool

	// GroupLen, when > 0, marks this item as the first of a parenthesized join
	// group spanning the next GroupLen items (this one included). Set by
	// checkFlattenSafe for a paren group containing an outer join, directly or
	// transitively, where flattening would change an outer join's NULL-extension
	// scope ("(t0 LEFT JOIN t1) JOIN t2" vs "t0 LEFT JOIN (t1 JOIN t2)"). This
	// item's Join/On/Using/Natural describe how the whole group attaches to what
	// precedes it; every item in the span keeps its own fields describing the
	// group's internal structure, resolved recursively over the span
	// (resolveGroupSource). Nested groups are GroupLen set on more items along
	// the span. Zero otherwise.
	GroupLen int

	// GroupAlias is the alias after a parenthesized join group of two or more
	// items, "(t1 JOIN t2 ON a=c) AS j1", set by parseFromElement on every item
	// of the group (it may be flattened). "" otherwise, including "(t1) AS x",
	// whose alias SQLite moves onto the item.
	//
	// It names the group's columns as an additional qualifier beside the
	// members' own names. Over t1(a,b)/t2(c,d):
	//
	//	SELECT * FROM (t1 JOIN t2 ON a=c) AS j1        -> cols a b c d
	//	SELECT j1.a, j1.c FROM (t1 JOIN t2 ON a=c) AS j1 -> both resolve
	//	SELECT t1.a FROM (t1 JOIN t2 ON a=c) AS j1     -> ALSO resolves
	//	SELECT * FROM (t1 AS p JOIN t2 AS q ON p.a=q.c) AS j1 -> p.a and j1.a both
	//	SELECT j1.rowid FROM (t1 JOIN t2 ON a=c) AS j1 -> no such column
	//	SELECT j1.* FROM (t1 JOIN t2 ON a=c) AS j1     -> "no such table: j1"
	//
	// It is a qualifier, not a scope: no pseudo-rowid, no star. Where SQLite
	// rebuilds the group's column list the statement is declined
	// (checkRebuiltJoinGroups, keyed on GroupRebuildID).
	GroupAlias string

	// HasGroupAlias records that an alias TOKEN was written after a
	// parenthesized join group of TWO OR MORE items, whatever that token's
	// TEXT was. GroupAlias alone cannot express that: a Go string has no
	// room for the difference between "no alias at all" and the EMPTY alias
	// "(...) AS \"\"" (equivalently "AS ``" or "AS []"), and C SQLite
	// tests the alias token's LENGTH (`Z.n`, parse.y rule 115) -- so all
	// three empty spellings trigger the same column-list REBUILD a named
	// alias does. Set on every item of the group, exactly like GroupAlias.
	HasGroupAlias bool

	// GroupRebuildID, when > 0, identifies a parenthesized join group of two or
	// more items (shared by all its members) whose column list C rebuilds rather
	// than splicing flat. parse.y rule 115 ("seltablist ::= stl_prefix LP
	// seltablist RP as on_using") splices only when the group leads its FROM
	// clause, has no alias (HasGroupAlias) and no ON/USING follows; otherwise it
	// wraps the group in an SF_NestedFrom subquery. A trailing ON/USING already
	// implies non-leading ("SELECT * FROM (t1 JOIN t2 ON 1) ON 1" is "a JOIN
	// clause is required before ON").
	//
	// Set by parseFromElement on every item the group resolves to, including a
	// nested group's items (overwriting their id: the outer group's census is a
	// superset). Read only by checkRebuiltJoinGroups, which declines the
	// rebuilt-list differences not reproduced here.
	GroupRebuildID int

	// NestFromWrapDepth counts the SF_NestedFrom layers (rule 115, "hasAlias ||
	// !leading" at each paren level) enclosing this item. Unlike GroupRebuildID,
	// which keeps only the outermost id, it increments per layer. Set beside
	// GroupRebuildID in parseFromElement; a leading unaliased inner paren adds no
	// layer.
	//
	// Over q1(a) q2(b) q3(c) q4(d): in "q1 JOIN (q2 JOIN (q3 JOIN q4))", q2.rowid
	// answers (one layer) while q3.rowid/q4.rowid are "no such column", since
	// selectExpander (select.c ~6228-6232) does not re-expose a nested
	// SF_NestedFrom's ENAME_ROWID entries. In "q1 JOIN ((q2 JOIN q3) JOIN q4)" all
	// three answer (q3 as "_ROWID_:1", q4 as "_ROWID_:2"): the inner paren is the
	// outer's leading unaliased member and splices flat, leaving all at depth 1.
	//
	// Read only by annotateNestedRowidNames, which requires depth 1 (a group with
	// mixed depths stays declined).
	NestFromWrapDepth int

	// NestedGroupSpan, when non-nil, makes this item a single-slot stand-in for a
	// whole parenthesized join group wrapped in further parentheses: "((t1 JOIN t2
	// ON t1.a=t2.a) AS x JOIN t2x ON x.b=t2x.b) AS y" (tkt-7a31705a7e6.test).
	// parse.y:777-816's seltablist rule, in its else branch (parse.y:810-816, for
	// nSrc>1 and an aliased, non-leading or ON/USING group), wraps the parsed
	// inner list into one SF_NestedFrom Select and appends it like a derived table
	// (sqlite3SrcListAppendFromTerm, parse.y:774-776), so the stand-in is
	// derived-table shaped. Parsing is inside-out, so deeper rewraps collapse level
	// by level.
	//
	// Alias is the inner group's alias ("x"); Table is "". NestedGroupSpan is an
	// independent copy of the inner group's items with their own
	// GroupLen/GroupAlias/HasGroupAlias/GroupRebuildID/NestFromWrapDepth; this
	// item's copies of those describe the outer group.
	//
	// Set by parseFromElement when a closing paren's content starts with an
	// aliased group's connector, before the alias/rebuild logic runs, so
	// checkFlattenSafe sees an ordinary item. nil otherwise.
	//
	// resolveGroupSource resolves it twice when this item is a group's connector
	// (once for the column list, once for real; see its doc for the hazards).
	// itemScopes (nestedGroupLeafCount, combinedMemberScope),
	// checkSelfJoinGroupSupported, checkDerivedJoinSupported and fromItemIsDerived
	// also read it.
	NestedGroupSpan []FromItem

	// GroupRebuildStarred reports that the statement's select list has a "*" that
	// expands over this item's rebuilt group (bare "*", or "X.*" naming a member
	// or the group alias). Stamped by markRebuiltGroupStars on every member; false
	// when GroupRebuildID is 0 and for a FROM with no select list (UPDATE ...
	// FROM).
	//
	// It decides whether the rebuilt list's order is observable: the rebuild moves
	// a USING/NATURAL-coalesced column to the front, which only a "*" shows. Over
	// ta(a,b)/td(b,g)/tc(z):
	//
	//	SELECT * FROM tc, (ta JOIN td USING(b))     -> z,b,a,g  vs  z,a,b,g
	//	SELECT a, b, g FROM tc, (ta JOIN td USING(b))          -> identical
	//	SELECT b FROM ...  / coalesce(ta.b,td.b) / ORDER BY 2 / a UNION arm
	//	pairing by position / INSERT ... SELECT / CTAS over an explicit list
	//	                                                       -> identical
	//
	// "SELECT ta.b" differs only in the ":N" auto-name (b:1 / b:2), the same
	// unspecified naming already served for a rebuilt group repeating a name
	// (checkRebuiltJoinGroups), so it is served.
	GroupRebuildStarred bool

	// GroupRebuildQualStarred reports that a qualified "X.*" (naming a member or
	// the group alias), not only a bare "*", set GroupRebuildStarred; stamped by
	// markRebuiltGroupStars. SF_NestedFrom expansion suppresses a USING/NATURAL
	// right-hand column's raw copy only for a bare star (select.c ~6250,
	// "(colFlags & COLFLAG_NOEXPAND)!=0 && zTName==0"); "t1.*" can reach the
	// suppressed ":N" copy this engine does not reproduce, so
	// checkOneRebuiltGroup's coalesced-name exemption needs this false.
	GroupRebuildQualStarred bool

	// NestedNonLeading is true for an item from a parenthesized join subtree
	// written anywhere but first in its enclosing FROM (set by
	// parseFromClause/parseFromElement via their `leading` flag). "t1 JOIN (t2
	// JOIN t3)" marks t2 and t3; "(t1 JOIN t2) JOIN t3" marks none.
	//
	// It exists for one naming rule: "SELECT t2.rowid FROM t1 JOIN (t2 JOIN t3)"
	// names the column "_ROWID_"/"OID" (uppercase, whatever was written) rather
	// than the usual rowid name. Values are unaffected.
	// columnRefNameParts reproduces that for the case
	// NestFromWrapDepth/annotateNestedRowidNames cover and declines the rest.
	NestedNonLeading bool

	// NestedGroupID identifies the outermost non-leading parenthesized join group
	// an item came from (0 for none). C materializes such a group as an
	// SF_NestedFrom subquery, so sqlite3ColumnsFromExprList ":N"-renames duplicate
	// names within each group, restarting per group. Over t(a,b), u(a,c), w(a,d),
	// y(a,e):
	//
	//	SELECT * FROM u JOIN (t JOIN w ON t.a=w.a)
	//	  -> a c a b a:1 d              (the group's second "a" is renamed)
	//	SELECT * FROM u JOIN (t JOIN w ...) JOIN (u AS u2 JOIN y ...)
	//	  -> a c a b a:1 d a c a:1 e    (each group restarts at :1)
	//	SELECT * FROM u JOIN (t JOIN (w JOIN y ...) ...)
	//	  -> a c a b a:1 d a:2 e        (a nested group shares the outer's)
	//	SELECT * FROM (t JOIN w ON t.a=w.a) JOIN u
	//	  -> a b a d a c                (a LEADING group is not renamed)
	//
	// An ID, not a flag: consecutive nested items can span two sibling groups.
	NestedGroupID int

	// UpdateTarget marks the synthetic comma-joined item update_from.go appends
	// last for UPDATE ... FROM's target. C detaches the target from the
	// synthetic SELECT's SrcList before resolving (update.c:222-230), so a FROM
	// clause ON referencing it never resolves; consulted only by
	// wherePlanMultiTableOrder (where_plan_gate.go).
	UpdateTarget bool

	// CrossKeyword records that the CROSS keyword was actually written, which
	// Join alone cannot say: JoinCross is JoinKind's ZERO value and is what a
	// plain comma join gets, and "CROSS JOIN ... ON/USING" is promoted to
	// JoinInner by the parser. The distinction has no semantic effect -- which
	// is why the promotion is harmless -- but it does have a PLANNING effect
	// that is observable through row order: SQLite's whereLoopAddAll makes
	// JT_CROSS a reorder barrier ("no real-world query that cares about
	// performance actually uses the CROSS JOIN syntax") while a comma join is
	// freely reorderable, and it stays a barrier with an ON clause present.
	// Read only by the ported query planner (where_plan.go), which declines to
	// reorder anything it cannot classify.
	CrossKeyword bool
}

// CompoundOp identifies a set-operation connective joining one compound
// SELECT arm to the arms before it: "UNION", "UNION ALL", "INTERSECT", or
// "EXCEPT" (kept as the verbatim upper-cased keyword text, mostly for clear
// error messages -- combineCompound, query.go, is the only place that
// switches on it).
type CompoundOp string

// CompoundArm is one "<op> SELECT ..." arm following the first SELECT of a
// compound statement. Stmt is a select-core only: parseSelectCore never
// populates OrderBy/Limit/Offset/Compound on it, since in SQLite's grammar
// those belong to the compound statement as a whole (see SelectStmt.Compound
// and parseSelectStmt in sql_parser.go), not to an individual arm.
type CompoundArm struct {
	Op   CompoundOp
	Stmt *SelectStmt
}

// SelectStmt is a parsed SELECT. A compound ("... UNION [ALL]|INTERSECT|EXCEPT
// ...") keeps its first arm's Columns/Distinct/From/Where/GroupBy/Having here
// and appends later arms to Compound (nil for a simple SELECT).
// OrderBy/Limit/Offset belong to the whole statement, the combined result for
// a compound (vdbe_compound_codegen.go).
type SelectStmt struct {
	Columns  []SelectColumn
	Distinct bool // "SELECT DISTINCT ..."; mutually exclusive with the (no-op, default) "SELECT ALL ..."
	From     []FromItem
	Where    Expr
	GroupBy  []Expr // GROUP BY expressions, in order; nil if no GROUP BY
	Having   Expr   // HAVING expression; nil if none (only meaningful with GroupBy)
	Compound []CompoundArm
	OrderBy  []OrderTerm
	Limit    *int64
	Offset   *int64

	// ValuesArms is the number of leading Compound arms carrying the second and
	// later rows of a multi-row VALUES (0 otherwise). parseValuesSelectCore
	// desugars "VALUES(a),(b),(c)" into row a plus one UNION ALL arm per further
	// row, but sqlite3MultiValues treats the clause as one arm, so "SELECT g FROM
	// t2 UNION ALL VALUES('x'),('z')" types as two arms (see ValuesFold). Later
	// real compound arms appended to the same slice are ordinary.
	ValuesArms int

	// ValuesFold is how SQLite's sqlite3MultiValues folds those rows together,
	// which decides the compound's output-column AFFINITY (and hence a CTAS's
	// declared types, a derived table's coercions and PRAGMA table_info over a
	// view). Meaningful only when ValuesArms > 0.
	ValuesFold valuesFold

	// LimitParam/OffsetParam are set instead of Limit/Offset when LIMIT/
	// OFFSET was written as a bound-parameter placeholder rather than an
	// integer literal (e.g. "LIMIT ?", "LIMIT ?1 OFFSET :n") -- always a bare
	// ParamExpr, never a general expression (this engine's LIMIT/OFFSET
	// grammar is otherwise still literal-only, per parseSignedIntLiteral).
	// execSelect (query.go) resolves either one, once per execution, into a
	// concrete Limit/Offset *int64 via limitOffsetValueToCount BEFORE
	// dispatching to any of the row-mode/aggregate/GROUP-BY/compound
	// execution paths, all of which only ever consult the already-resolved
	// Limit/Offset fields -- so nothing downstream of that resolution step
	// needs to know LIMIT/OFFSET could ever have been a parameter at all.
	LimitParam  Expr
	OffsetParam Expr

	// FromParenthesized is true when this statement's FROM used a parenthesized
	// term (a derived table or a parenthesized join). It scopes
	// errIfDuplicateOutputNames: SQLite's ":N" renaming of duplicate "*" names is
	// not reproduced there, so such a FROM producing duplicate names is declined,
	// while "SELECT * FROM t1, t2" is unaffected. Set only on the statement whose
	// own FROM is parenthesized.
	FromParenthesized bool

	// FromNestedParenJoin is true when this statement's FROM used a NESTED
	// parenthesized join -- a parenthesized join group inside another
	// parenthesized join group, e.g. "(t2 JOIN (t3 JOIN t4) ON ...)". Real
	// SQLite materializes such a nested group as an internal subquery and
	// ":N"-disambiguates any duplicated column name (so "SELECT t3.a" over it
	// comes out "a:1"), a naming this engine's flatten-the-parentheses approach
	// does not reproduce -- so a statement with this flag set is declined (see
	// errIfDuplicateOutputNames, query.go). Set by parseFromElement.
	FromNestedParenJoin bool

	// Params is this statement's bound-parameter shape (see ParamInfo's doc
	// comment), computed once by ParseSelect over the WHOLE statement text --
	// including every subquery's own placeholders, since a subquery shares
	// its enclosing statement's single parameter-numbering sequence and bind
	// array, exactly like C SQLite (a subquery is not a separately
	// prepared statement). It is only ever populated on the OUTERMOST
	// SelectStmt ParseSelect returns; a subquery's own *SelectStmt (reached
	// via SubqueryExpr/ExistsExpr/InExpr.Sub or CompoundArm.Stmt) leaves this
	// at its zero value, since parsing a subquery doesn't rewalk it as a
	// standalone top-level parse.
	Params ParamInfo

	// Windows holds this statement's top-level "WINDOW <name> AS (<spec>)
	// [, ...]" clause (parsed after HAVING, before ORDER BY), or nil if none.
	// A window function's "OVER <name>" resolves against these -- see
	// sql_window.go.
	Windows []NamedWindow

	// CTEs holds this statement's own WITH clause in written order, or nil. Set
	// only on the SelectStmt parseSelectStmt parses after WITH (a top-level query
	// or any parenthesized subquery, CTE body, INSERT ... SELECT, CREATE ... AS
	// SELECT); a compound arm (parseSelectCore) never carries CTEs. A FROM item
	// naming one becomes the same derived-table row source a subquery or view
	// uses (resolveFrom, cte.go), except a recursive CTE (detectRecursiveShape),
	// which compiles to a queue program (compileRecursiveCTE).
	CTEs []CTEDef

	// r35dCompoundOf is set only on the copy compileCompound makes to compile a
	// compound's first arm, pointing at the compound. The aggregate hoist
	// (vdbe_agg_hoist.go) identifies a subquery body by AST pointer, and the
	// enclosing select list holds the compound node, so a hoist recorded against
	// the copy would be unreachable. The copy shares Columns/Where/Having/GroupBy
	// with the compound, so redirecting the pointer is enough.
	r35dCompoundOf *SelectStmt
}

// valuesFold is how sqlite3MultiValues folded a multi-row VALUES clause, which
// decides that arm's affinity (SelectStmt.ValuesFold). The co-routine method
// is the default; SQLite falls back to real UNION ALL arms when (a) the
// statement has a WITH clause, (b) the SQL comes from sqlite_schema, (c) a row
// is non-constant, (d) a first-row value has an affinity (a CAST), or (e) it is
// an ALTER rename re-parse. Over t2(f NUMERIC, g VARCHAR(9)):
//
//	CREATE TABLE c AS VALUES('x'),('z') UNION ALL SELECT g FROM t2
//	  -> c(column1)         co-routine: the VALUES is ONE no-affinity arm
//	CREATE TABLE c AS WITH q(z) AS (SELECT 1)
//	                  VALUES('x'),('z') UNION ALL SELECT g FROM t2
//	  -> c(column1 TEXT)    (a): three real arms, and g's TEXT wins
//	CREATE TABLE c AS VALUES(CAST('x' AS TEXT)),('z') UNION ALL SELECT g FROM t2
//	  -> c(column1 TEXT)    (d): same, established by the CAST itself
//
// (b) makes a view body always the fallback: even the creating connection
// re-parses the stored text (sqlite3EndTable's OP_ParseSchema).
type valuesFold uint8

const (
	// valuesFoldNone is every core that is not a multi-row VALUES clause.
	valuesFoldNone valuesFold = iota
	// valuesFoldCoroutine is the co-routine method: SQLite reads the whole
	// clause as ONE arm that is a TK_COLUMN over an AFF_NONE pseudo-table, so
	// it has no affinity, its storage class is unconstrained, and its
	// columnType is NULL -- whatever the rows were written as. (Condition (d)
	// is what guarantees that: the co-routine is only ever started when the
	// FIRST row's values have no affinity of their own.)
	valuesFoldCoroutine
	// valuesFoldUnionAll is the fallback: each row really is its own UNION ALL
	// arm. On the RIGHT of a compound operator those arms are still only ONE
	// arm of the enclosing compound, because parse.y's "selectnowith ::=
	// selectnowith multiselect_op oneselect" wraps a compound right operand in
	// a derived table; in the leading or bare position they splice in flat.
	valuesFoldUnionAll
	// valuesFoldUnanalyzable is a multi-row VALUES with a non-constant row.
	// SQLite starts the co-routine and then abandons it mid-clause, leaving a
	// mixed chain (co-routine over rows 1..k, then real arms, and possibly a
	// second co-routine after that) which this engine does not model -- so
	// callers decline rather than guess.
	valuesFoldUnanalyzable
)

// CTEDef is one WITH-clause definition: "name [(col-list)] AS (<select>)".
// The literal RECURSIVE keyword (if the WITH clause had one at all) is
// parsed but otherwise carries no meaning here -- verified directly against
// C SQLite (mattn/go-sqlite3) that a CTE's own SELECT shape (a compound
// whose trailing arm, joined via UNION/UNION ALL, references the CTE's own
// name exactly once in its own top-level FROM) is evaluated recursively
// REGARDLESS of whether RECURSIVE was written at all, and a name collision
// between two CTEs in the same WITH clause ("duplicate WITH table name: %s")
// is rejected the same way either way. See cte.go's detectRecursiveShape,
// which does this same structural recognition, independent of the keyword.
type CTEDef struct {
	Name     string
	ColNames []string // explicit "(col, ...)" rename list; nil if none was given
	Select   *SelectStmt
	// Materialized is the "AS MATERIALIZED" / "AS NOT MATERIALIZED" hint, which
	// UNLIKE RECURSIVE is observable: select.c:7797 makes AS MATERIALIZED an
	// outright optimization fence, so a CTE written that way is never flattened
	// into the query reading it and keeps the pre-flattening answer. See
	// CTEMaterialization (cte.go) and r38cExpandCTERef (flatten_limit_r35c.go).
	Materialized CTEMaterialization
}
