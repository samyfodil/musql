package engine

import (
	"fmt"
	"strings"
)

// "EXPLAIN <stmt>" lists the compiled program in eight columns: addr, opcode,
// p1, p2, p3, p4, p5, comment. This engine's instruction stream is rendered,
// not C SQLite's (per vdbe.c:9225: the format is for testing and debugging only).

// IsExplainStatement reports whether sqlText is an EXPLAIN (either form).
func IsExplainStatement(sqlText string) bool { return stmtTextFlagsFor(sqlText).isExplain }

// ExplainWrapsWrite reports whether sqlText is an EXPLAIN of a write statement.
func ExplainWrapsWrite(sqlText string) bool {
	inner, mode := splitExplain(strings.TrimSpace(sqlText))
	return mode != explainNone && isExplainableWrite(inner)
}

// explainMode says which of the two EXPLAIN forms a statement asked for.
type explainMode int

const (
	explainNone      explainMode = iota
	explainListing               // EXPLAIN <stmt>: the instruction listing
	explainQueryPlan             // EXPLAIN QUERY PLAN <stmt>
)

// explainColumns is the listing's fixed column set, matching C SQLite's.
func explainColumns() []string {
	return []string{"addr", "opcode", "p1", "p2", "p3", "p4", "p5", "comment"}
}

// splitExplain reports whether sqlText begins with EXPLAIN (optionally
// EXPLAIN QUERY PLAN) and returns the statement it wraps.
func splitExplain(sqlText string) (inner string, mode explainMode) {
	if !hasLeadingExplainWord(sqlText) {
		// Every statement on the read path reaches here, so the common answer
		// -- "no" -- is given without lexing the whole statement first.
		return sqlText, explainNone
	}
	toks, err := lex(strings.TrimSpace(sqlText))
	if err != nil || len(toks) < 2 || toks[0].kind != tkIdent || !toks[0].isKw("EXPLAIN") {
		return sqlText, explainNone
	}
	rest, m := toks[1:], explainListing
	if len(rest) >= 3 && rest[0].kind == tkIdent && rest[0].isKw("QUERY") &&
		rest[1].kind == tkIdent && rest[1].isKw("PLAN") {
		rest, m = rest[2:], explainQueryPlan
	}
	if len(rest) == 0 || rest[0].Start >= len(sqlText) {
		return sqlText, explainNone // "EXPLAIN" with nothing after it: not a prefix
	}
	return sqlText[rest[0].Start:], m
}

// hasLeadingExplainWord reports whether sqlText's first word is EXPLAIN,
// skipping leading whitespace and comments. It avoids lexing if false.
func hasLeadingExplainWord(sqlText string) bool {
	s := sqlText
	for {
		i := 0
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f' || s[i] == '\v') {
			i++
		}
		s = s[i:]
		switch {
		case strings.HasPrefix(s, "--"):
			if j := strings.IndexByte(s, '\n'); j >= 0 {
				s = s[j+1:]
				continue
			}
			return false // a line comment running to the end: nothing follows
		case strings.HasPrefix(s, "/*"):
			j := strings.Index(s[2:], "*/")
			if j < 0 {
				return false // unterminated: the lexer will report it
			}
			s = s[2+j+2:]
			continue
		}
		return len(s) >= 7 && strings.EqualFold(s[:7], "EXPLAIN") &&
			(len(s) == 7 || !isIdentContinueByte(s[7]))
	}
}

// isIdentContinueByte reports whether b can continue an unquoted SQL
// identifier, so "explainer" is not read as the keyword EXPLAIN.
func isIdentContinueByte(b byte) bool {
	return b == '_' || b == '$' || b >= 0x80 ||
		(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// explainQuery answers an EXPLAIN. The listing form compiles the wrapped
// statement exactly as running it would and renders the resulting program; the
// QUERY PLAN form is a separate surface (explainQueryPlanRows).
func (p *ReadOnlyPager) explainQuery(inner string, mode explainMode) (cols []string, rows [][]Value, err error) {
	if mode == explainQueryPlan {
		return p.explainQueryPlanRows(inner)
	}
	arms, err := p.explainPrograms(inner)
	if err != nil {
		return nil, nil, err
	}
	// Every program the listing's programs run follows them, in the order
	// they are reached: a FROM subquery's, a scalar or EXISTS subquery's, an
	// IN subquery's. Each runs on a machine of its own here, where C codes it
	// into the same program as a co-routine or a subroutine.
	seen := map[*Program]bool{}
	for _, arm := range arms {
		seen[arm.prog] = true
	}
	base := 0
	for k := 0; k < len(arms); k++ {
		arm := arms[k]
		for addr, in := range arm.prog.Insns {
			for _, sub := range explainSubPrograms(in) {
				if !seen[sub] {
					seen[sub] = true
					arms = append(arms, explainSubArms(sub, fmt.Sprintf("subquery of %d", base+addr))...)
				}
			}
			in, err := explainRelocate(in, base)
			if err != nil {
				return nil, nil, err
			}
			comment := disasmComment(arm.prog, base+addr, in)
			if addr == 0 && arm.op != "" {
				comment = arm.op + ": " + comment
			}
			rows = append(rows, []Value{
				{Typ: Int, I: int64(base + addr)},
				{Typ: Text, S: []byte(in.Op.String())},
				{Typ: Int, I: int64(in.P1)},
				{Typ: Int, I: int64(in.P2)},
				{Typ: Int, I: int64(in.P3)},
				explainTextOrNull(formatP4(in)),
				{Typ: Int, I: int64(in.P5)},
				explainTextOrNull(comment),
			})
		}
		base += len(arm.prog.Insns)
	}
	return explainColumns(), rows, nil
}

// explainArm is one program of an EXPLAIN listing, and what brings it in: ""
// for the statement's own, a compound operator for a later arm, or the
// instruction that runs a subquery.
type explainArm struct {
	op   string
	prog *Program
}

// explainSubPrograms is every program in runs: a subquery's, a FROM item's.
func explainSubPrograms(in Instruction) []*Program {
	var sub *Program
	switch x := in.P4.(type) {
	case *Program:
		sub = x
	case *derivedSource:
		sub = x.prog
	case *inSubPlan:
		sub = x.prog
	case *rowSubPlan:
		sub = x.prog
	}
	if sub == nil {
		return nil
	}
	return []*Program{sub}
}

// explainSubArms is sub as listed, brought in by what: a compound's arms one
// after another, each named by its operator after the first.
func explainSubArms(sub *Program, by string) []explainArm {
	if sub.Compound == nil {
		return []explainArm{{op: by, prog: sub}}
	}
	arms := make([]explainArm, 0, len(sub.Compound.arms))
	for i, a := range sub.Compound.arms {
		op := by
		if i > 0 {
			op += ", " + string(sub.Compound.ops[i-1])
		}
		arms = append(arms, explainArm{op: op, prog: a})
	}
	return arms
}

// explainRelocate is in as it reads base instructions further down a listing:
// every jump target moved with it. A jump is never to address 0, where the
// program's OpInit is, so a zero target is "no jump" and stays. The opcodes
// whose target is computed while they run have nothing here to move.
func explainRelocate(in Instruction, base int) (Instruction, error) {
	if base == 0 {
		return in, nil
	}
	switch {
	case explainJumpsComputed(in.Op):
		return in, fmt.Errorf("%w: EXPLAIN of a compound SELECT whose arm holds %s, whose jump is computed as it runs", errVDBEUnsupported, in.Op)
	case explainJumpsP2(in) && in.P2 > 0:
		in.P2 += base
	case explainJumpsP3(in.Op) && in.P3 > 0:
		in.P3 += base
	}
	return in, nil
}

// explainJumpsP2, explainJumpsP3 and explainJumpsComputed are the opcodes
// whose jump the VDBE takes to P2, to P3, or to an address it works out
// (vdbe.go; TestExplainJumpTablesMatchTheVDBE keeps them in step).
func explainJumpsP2(in Instruction) bool {
	switch in.Op {
	case OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
		return in.P5&p5StoreP2 == 0
	case OpDistinct, OpGosub, OpGoto, OpGroupSame, OpHashAggNext, OpHashAggSort, OpIf, OpIfEmpty, OpIfNot,
		OpInit, OpIsNull, OpMustBeInt, OpNext, OpNotExists, OpNotNull, OpRecQueueLimit, OpRecQueueOffset,
		OpRecQueuePop, OpRewind, OpRightJoinSweepNext, OpRightJoinSweepRewind, OpRowidBound, OpSegEmitRow,
		OpSegHashAgg, OpSegJoinAgg, OpSegJoinGroups, OpSegJoinHash, OpSegJoinNext, OpSegJoinTop,
		OpSkipIfRowGone, OpSorterCheck, OpSorterNext, OpSorterSort:
		return true
	}
	return false
}

func explainJumpsP3(op OpCode) bool {
	switch op {
	case OpSegDistinct, OpSegFilterCount, OpSegOrderLimit, OpSegProgram, OpSegVectorTopK:
		return true
	}
	return false
}

func explainJumpsComputed(op OpCode) bool {
	switch op {
	case OpJIT, OpFireTriggers, OpInsert, OpUpdateRow, OpUpsertFind:
		return true
	}
	return false
}

// explainTextOrNull renders one of the two nullable columns: C SQLite leaves
// p4 and comment NULL when the opcode has nothing to say there (verified
// against 3.53.3: "EXPLAIN SELECT a FROM t WHERE a=1" begins
// "0|Init|0|7|0|<nil>|0|<nil>").
func explainTextOrNull(s string) Value {
	if s == "" {
		return Value{Typ: Null}
	}
	return Value{Typ: Text, S: []byte(s)}
}

// explainPrograms compiles the wrapped statement to the programs EXPLAIN
// lists. It is the SAME compile a plain run does -- so a statement this engine
// cannot lower still declines here, with its own reason, rather than EXPLAIN
// quietly answering for something that could not be run.
//
// A compound compiles to one program per arm (compoundProgram.arms), not to
// the single stream C SQLite emits with the arms inlined, and the rows the arms
// make are combined, sorted and limited outside any program. So its listing is
// each arm's program in turn, as the plan lists each arm's scans
// (explainCompoundPlan).
func (p *ReadOnlyPager) explainPrograms(inner string) ([]explainArm, error) {
	if isExplainableWrite(inner) {
		prog, err := p.explainWriteProgram(inner)
		if err != nil {
			return nil, err
		}
		return []explainArm{{prog: prog}}, nil
	}
	stmt, err := p.explainParse(inner)
	if err != nil {
		return nil, err
	}
	if len(stmt.CTEs) > 0 {
		defer p.pushCTEScope(stmt.CTEs)()
	}
	first := *stmt
	first.Compound = nil
	if len(stmt.Compound) > 0 {
		first.OrderBy, first.Limit, first.Offset = nil, nil, nil
	}
	arms := []explainArm{{}}
	cores := []*SelectStmt{&first}
	for _, arm := range stmt.Compound {
		arms = append(arms, explainArm{op: string(arm.Op)})
		cores = append(cores, arm.Stmt)
	}
	for i, core := range cores {
		if arms[i].prog, err = p.explainCompile(core); err != nil {
			return nil, err
		}
	}
	return arms, nil
}

// isExplainableWrite reports whether the wrapped statement is one the WRITE
// compiler owns. EXPLAIN describes a statement's program without running it,
// and a write's program is compiled by a different entry point than a query's
// -- so the verb has to pick, exactly as Exec/Query already do.
func isExplainableWrite(inner string) bool {
	verb, ok := LeadingStatementVerb(inner)
	if !ok {
		return false
	}
	switch verb {
	case "SELECT", "VALUES", "WITH":
		return false
	}
	return true
}

// explainWriteProgram compiles a write statement's program for EXPLAIN. It
// COMPILES only -- nothing is executed and nothing is written, which is real
// SQLite's contract too: sqlite3_prepare builds the program and EXPLAIN steps
// the DESCRIPTION rather than the program (vdbe.c's OP_Init arm checks
// p->explain before running anything).
//
// It needs the live write session, because that is what holds the schema a
// write compiles against (a read-only snapshot has no write compiler). A
// pager without one -- an ATTACHed reader opened on its own -- declines.
func (p *ReadOnlyPager) explainWriteProgram(inner string) (*Program, error) {
	if p.writeSession == nil {
		return nil, fmt.Errorf("%w: EXPLAIN of a write statement needs the write session this snapshot came from", errVDBEUnsupported)
	}
	return p.writeSession.compileWriteProgram(inner)
}

// explainParse parses and resolves the wrapped statement, the step both
// EXPLAIN forms share.
func (p *ReadOnlyPager) explainParse(inner string) (*SelectStmt, error) {
	stmt, err := ParseSelect(inner)
	if err != nil {
		return nil, err
	}
	if err := resolveNoFromLimitOffset(p, stmt, nil); err != nil {
		return nil, err
	}
	return stmt, nil
}

// explainCompile lowers the parsed statement exactly as running it would.
func (p *ReadOnlyPager) explainCompile(stmt *SelectStmt) (*Program, error) {
	if len(stmt.From) == 0 {
		// A FROM-less SELECT is compiled by the other entry point -- execSelect
		// picks between the two the same way.
		return compileSelectNoFromPager(p, stmt, nil)
	}
	// The rewrites execSelect makes before it compiles, so the plan shown is
	// the one that runs.
	stmt = p.r37cFlattenTransparent(stmt)
	nCTE := len(stmt.CTEs)
	stmt = p.r41FlattenProjection(stmt)
	if len(stmt.CTEs) > nCTE {
		defer p.pushCTEScope(stmt.CTEs[nCTE:])()
	}
	if len(stmt.Compound) > 0 {
		return nil, fmt.Errorf("%w: EXPLAIN of a SELECT whose subquery flattened into a compound", errVDBEUnsupported)
	}
	return compileSelectScan(p, p.r41PushDown(stmt), nil)
}

// explainQueryPlanRows answers "EXPLAIN QUERY PLAN", whose four columns are
// id/parent/notused/detail. The detail text is C SQLite's own vocabulary --
// "SCAN t", "SEARCH t USING INDEX i (a=?)", "SEARCH t USING INTEGER PRIMARY KEY
// (rowid=?)" -- built by the rules sqlite3WhereExplainOneScan owns
// (wherecode.c:145-200): SEARCH when the loop has an equality or a range bound
// and SCAN otherwise, then the index clause.
//
// It is read back off the COMPILED PROGRAM rather than published by the
// codegen, because the program already records every fact the text needs: one
// OpOpenRead/OpOpenDerived per FROM source, and the seek hint (if any) that
// turns that source's scan into a search. Reading it back keeps the plan and
// the program that runs from disagreeing, which a second, parallel description
// built during codegen could.
//
// Two departures from C's text, both structural rather than cosmetic: a
// materialized subquery is listed as "(subquery-N)" numbered within this
// statement (C's selId is a connection-wide counter, so its N is not
// reproducible by anything but C itself), and nothing below the top-level FROM
// is listed -- a subquery's own scans live in its own program and C's nested
// id/parent tree is not modelled here. Both are safe under the format's own
// contract: "The output ... is intended for interactive debugging only. The
// output format may change between SQLite releases" (lang_explain.html).
func (p *ReadOnlyPager) explainQueryPlanRows(inner string) (cols []string, rows [][]Value, err error) {
	cols = []string{"id", "parent", "notused", "detail"}
	var lines []explainPlanLine
	if isExplainableWrite(inner) {
		// A write's plan is the plan of the SCAN it runs over its target (C
		// prints an INSERT's as nothing at all, since it scans nothing), which
		// is the same read-back of the same opcodes.
		prog, perr := p.explainWriteProgram(inner)
		if perr != nil {
			return nil, nil, perr
		}
		lines = p.explainProgPlan(prog, &explainPlanState{})
	} else {
		stmt, perr := p.explainParse(inner)
		if perr != nil {
			return nil, nil, perr
		}
		if len(stmt.CTEs) > 0 {
			defer p.pushCTEScope(stmt.CTEs)()
		}
		if lines, err = p.explainPlanOf(stmt); err != nil {
			return nil, nil, err
		}
	}
	rows = make([][]Value, 0, len(lines))
	for i, ln := range lines {
		rows = append(rows, []Value{
			{Typ: Int, I: int64(i + 1)},
			{Typ: Int, I: int64(ln.parent)},
			{Typ: Int, I: 0},
			{Typ: Text, S: []byte(ln.detail)},
		})
	}
	return cols, rows, nil
}

// explainPlanLine is one QUERY PLAN row before its id is assigned: ids are
// positions (the Nth line is id N), so a line names its parent by that same
// position, 0 for a top-level one.
type explainPlanLine struct {
	parent int
	detail string
}

// topLevelPlanLines is the ordinary, flat case: every line a child of nothing.
func topLevelPlanLines(details []string) []explainPlanLine {
	out := make([]explainPlanLine, len(details))
	for i, d := range details {
		out[i] = explainPlanLine{detail: d}
	}
	return out
}

// explainPlanOf is the QUERY PLAN of one parsed statement: one line per
// scanned source, and for a statement with no FROM clause at all the single
// line C SQLite prints there instead -- sqlite3WhereBegin's nTabList==0 arm
// (where.c:6945-6956) says "SCAN CONSTANT ROW" for every FROM-less core except
// a multi-row VALUES, which names itself "SCAN %d-ROW VALUES CLAUSE" (printf.c:
// 1022-1025, reached through select.c:2883). The programs it runs (a FROM
// subquery's, an expression's) are lines under the line that names them
// (explainProgPlan).
func (p *ReadOnlyPager) explainPlanOf(stmt *SelectStmt) ([]explainPlanLine, error) {
	return p.explainStmtPlan(stmt, &explainPlanState{})
}

// explainPlanState is what one statement's plan numbers across its programs:
// the subqueries, so that each name is the statement's own.
type explainPlanState struct{ subs int }

func (p *ReadOnlyPager) explainStmtPlan(stmt *SelectStmt, st *explainPlanState) ([]explainPlanLine, error) {
	if stmt.ValuesArms > 0 && stmt.ValuesArms == len(stmt.Compound) && len(stmt.From) == 0 {
		// A multi-row VALUES: desugared here into one compound arm per row
		// (parseValuesSelectCore), but ONE clause to SQLite, which is how it
		// counts the rows it names. No compile -- every arm is a constant row.
		return topLevelPlanLines([]string{fmt.Sprintf("SCAN %d-ROW VALUES CLAUSE", stmt.ValuesArms+1)}), nil
	}
	if len(stmt.Compound) > 0 {
		return p.explainCompoundPlan(stmt, st)
	}
	prog, err := p.explainCompile(stmt)
	if err != nil {
		return nil, err
	}
	lines := p.explainProgPlan(prog, st)
	if len(lines) == 0 && len(stmt.From) == 0 {
		return topLevelPlanLines([]string{"SCAN CONSTANT ROW"}), nil
	}
	return lines, nil
}

// explainProgPlan is prog's plan: a line per scan in loop order, a FROM
// subquery's own plan under a "MATERIALIZE (subquery-N)" line before its
// scan, and each subquery an expression runs under a line naming it, as C
// prints its "SCALAR SUBQUERY N" and "LIST SUBQUERY N". Every one is a
// program of its own here, materialized or run per evaluation, where C may
// code it as a co-routine of the statement's.
func (p *ReadOnlyPager) explainProgPlan(prog *Program, st *explainPlanState) []explainPlanLine {
	if prog.Compound != nil {
		cp := prog.Compound
		return p.explainArmsPlan(cp.arms, cp.ops, len(cp.orderBy) > 0, st)
	}
	var out []explainPlanLine
	for _, sc := range p.explainPlanScans(prog, st) {
		if sc.derived != nil {
			out = append(out, explainPlanLine{detail: "MATERIALIZE " + sc.name})
			appendPlanUnder(&out, len(out), p.explainProgPlan(sc.derived, st))
		}
		out = append(out, explainPlanLine{detail: sc.line})
	}
	for _, in := range prog.Insns {
		var sub *Program
		var kind string
		correlated := false
		switch x := in.P4.(type) {
		case *Program:
			if in.Op != OpSubquery && in.Op != OpExists {
				continue
			}
			sub, kind, correlated = x, "SCALAR SUBQUERY", in.P5&p5Correlated != 0
		case *inSubPlan:
			sub, kind, correlated = x.prog, "LIST SUBQUERY", x.correlated
		case *rowSubPlan:
			sub, kind, correlated = x.prog, "SCALAR SUBQUERY", x.correlated
		}
		if sub == nil {
			continue
		}
		if correlated {
			kind = "CORRELATED " + kind
		}
		st.subs++
		out = append(out, explainPlanLine{detail: fmt.Sprintf("%s %d", kind, st.subs)})
		appendPlanUnder(&out, len(out), p.explainProgPlan(sub, st))
	}
	return out
}

// explainArmsPlan is a compound's plan from its arms' programs, the tree
// explainCompoundPlan prints for a statement's own.
func (p *ReadOnlyPager) explainArmsPlan(arms []*Program, ops []CompoundOp, ordered bool, st *explainPlanState) []explainPlanLine {
	out := []explainPlanLine{{detail: "COMPOUND QUERY"}}
	for i, arm := range arms {
		label := "LEFT-MOST SUBQUERY"
		if i > 0 && i-1 < len(ops) {
			label = string(ops[i-1])
		}
		out = append(out, explainPlanLine{parent: 1, detail: label})
		appendPlanUnder(&out, len(out), p.explainProgPlan(arm, st))
	}
	if ordered {
		out = append(out, explainPlanLine{detail: "USE TEMP B-TREE FOR ORDER BY"})
	}
	return out
}

// appendPlanUnder appends lines, a plan of their own, under the line at
// position parent of out: their top level hangs from it, and the rest keep
// their parents, renumbered past what out held.
func appendPlanUnder(out *[]explainPlanLine, parent int, lines []explainPlanLine) {
	off := len(*out)
	for _, ln := range lines {
		if ln.parent == 0 {
			ln.parent = parent
		} else {
			ln.parent += off
		}
		*out = append(*out, ln)
	}
}

// explainCompoundPlan is the QUERY PLAN of a compound SELECT: a TREE, which is
// the one shape the flat rows above cannot express. For a flat UNION ALL with
// no ORDER BY it is C SQLite's own, measured against 3.53.3 for two and three
// arms:
//
//	1|0 COMPOUND QUERY
//	2|1 LEFT-MOST SUBQUERY
//	4|2 SCAN t1
//	9|1 UNION ALL
//	11|9 SCAN t2
//	16|1 UNION ALL
//	18|16 SCAN t1
//
// (its ids are program addresses; these are positions, which the format's own
// contract allows -- the TREE is what carries meaning.)
//
// Every other compound is printed the same way, each arm under its operator,
// because that is how this engine runs it. C MERGES those: "MERGE (<op>)" over
// LEFT and RIGHT children, a left-deep binary tree, each leaf sorted into a
// temp b-tree. This engine runs each arm and combines their rows by equality
// (combineCompound, vdbe_compound_codegen.go), sorting only for an ORDER BY,
// which is the one "USE TEMP B-TREE FOR ORDER BY" line it prints. C's tree
// would describe a program this engine does not run.
func (p *ReadOnlyPager) explainCompoundPlan(stmt *SelectStmt, st *explainPlanState) ([]explainPlanLine, error) {
	// Arm 0 is the statement's own core; planning it alone means dropping what
	// belongs to the compound as a whole (its arms, and the ORDER BY/LIMIT the
	// grammar attaches to the statement -- see SelectStmt.Compound).
	first := *stmt
	first.Compound, first.OrderBy, first.Limit, first.Offset = nil, nil, nil, nil

	lines := []explainPlanLine{{detail: "COMPOUND QUERY"}}
	appendArm := func(label string, arm *SelectStmt) error {
		lines = append(lines, explainPlanLine{parent: 1, detail: label})
		armLines, err := p.explainStmtPlan(arm, st)
		if err != nil {
			return err
		}
		// Every arm's lines hang from the arm's own node. parseSelectStmt
		// flattens a compound into ONE Compound slice on the first core (see
		// SelectStmt.Compound), so an arm never carries a compound of its own.
		appendPlanUnder(&lines, len(lines), armLines)
		return nil
	}
	if err := appendArm("LEFT-MOST SUBQUERY", &first); err != nil {
		return nil, err
	}
	for _, arm := range stmt.Compound {
		if err := appendArm(string(arm.Op), arm.Stmt); err != nil {
			return nil, err
		}
	}
	if len(stmt.OrderBy) > 0 {
		lines = append(lines, explainPlanLine{detail: "USE TEMP B-TREE FOR ORDER BY"})
	}
	return lines, nil
}

// explainScan is one cursor's line of the query plan, recovered from the
// program: what it opens, and the seek (if any) that makes it a SEARCH.
type explainScan struct {
	cursor   int
	name     string
	rowidKey bool             // an OpSeekRowidHint reached this cursor
	idxRoot  uint32           // an OpSeekIndexHint's index root, 0 for none
	idxName  string           // its index's name: a segment table's index has root 0
	plan     wherePlanExplain // the loop C's planner chose, when the port decided it
	write    bool             // opened by OpOpenWrite: a DML target, scanned only if rewound
	order    int              // rank in nested-loop order: the address of this cursor's OpRewind
	derived  *Program         // a FROM subquery's program, which makes its rows
}

// explainPlanScans walks the program once and returns one plan line per
// scanned source, in nested-loop order. The opens are emitted in FROM order
// (emitJoinLoops) but the LOOPS are emitted outermost-first, so a cursor's own
// OpRewind is what ranks it -- the same order C lists its levels in.
func (p *ReadOnlyPager) explainPlanScans(prog *Program, st *explainPlanState) []explainScanLine {
	var scans []*explainScan
	byCursor := map[int]*explainScan{}
	for _, in := range prog.Insns {
		switch in.Op {
		case OpOpenRead:
			s := &explainScan{cursor: in.P1, name: explainSourceName(in.P4), order: 1 << 30}
			scans = append(scans, s)
			byCursor[in.P1] = s
		case OpOpenDerived:
			s := &explainScan{cursor: in.P1, name: explainSourceName(in.P4), order: 1 << 30}
			if ds, ok := in.P4.(*derivedSource); ok {
				s.derived = ds.prog
			}
			scans = append(scans, s)
			byCursor[in.P1] = s
		case OpOpenWrite:
			s := &explainScan{cursor: in.P1, name: explainSourceName(in.P4), order: 1 << 30, write: true}
			scans = append(scans, s)
			byCursor[in.P1] = s
		case OpSeekRowidHint, OpSeekRowidSetHint:
			if s := byCursor[in.P1]; s != nil {
				s.rowidKey = true
			}
		case OpSeekIndexHint:
			if s, ok := byCursor[in.P1]; ok && s != nil {
				if h, ok := in.P4.(*indexSeekHint); ok && h.slot == 0 {
					s.idxRoot, s.idxName, s.plan = h.root, h.index, h.plan
				}
			}
		}
	}
	for addr, in := range prog.Insns {
		if in.Op != OpRewind {
			continue
		}
		if s := byCursor[in.P1]; s != nil && s.order == 1<<30 {
			s.order = addr
		}
	}
	sortStableByOrder(scans)
	// A plan line describes a LOOP, and a write's target cursor is not always
	// one: an INSERT opens its table to insert INTO, never rewinding it, and
	// C SQLite prints no plan row at all for such a statement (verified:
	// "EXPLAIN QUERY PLAN INSERT INTO t VALUES(...)" answers zero rows). An
	// UPDATE/DELETE, whose WHERE really does drive a scan over the same
	// cursor, keeps its line. Only the write cursor is filtered this way --
	// every read cursor this engine opens is driven by a loop.
	out := make([]explainScanLine, 0, len(scans))
	for _, s := range scans {
		if s.write && s.order == 1<<30 {
			continue
		}
		name := s.name
		if name == "" {
			st.subs++
			name = fmt.Sprintf("(subquery-%d)", st.subs)
		}
		line := "SCAN " + name
		switch {
		case s.rowidKey:
			line = "SEARCH " + name + " USING INTEGER PRIMARY KEY (rowid=?)"
		case s.plan.verb != "":
			line = s.plan.line(name)
		case s.idxRoot != 0 || s.idxName != "":
			if idx, col := p.explainIndexAt(s.idxRoot, s.idxName); idx != "" {
				line = "SEARCH " + name + " USING INDEX " + idx + " (" + col + "=?)"
			}
		}
		out = append(out, explainScanLine{line: line, name: name, derived: s.derived})
	}
	return out
}

// explainScanLine is one scan's plan line, the name it gives the source, and
// the program a FROM subquery runs to make its rows.
type explainScanLine struct {
	line, name string
	derived    *Program
}

// sortStableByOrder puts the scans in nested-loop order (insertion sort: a FROM
// clause is a handful of items, and the order must be stable for the cursors
// that share the sentinel rank because they are never rewound).
func sortStableByOrder(scans []*explainScan) {
	for i := 1; i < len(scans); i++ {
		for j := i; j > 0 && scans[j].order < scans[j-1].order; j-- {
			scans[j], scans[j-1] = scans[j-1], scans[j]
		}
	}
}

// explainSourceName names a source the way SQLite's "%S" conversion does
// (printf.c:1000-1028): the TABLE's own name when there is one -- an alias does
// NOT replace it -- and only a subquery, which has no name at all, falls
// through to the synthetic "(subquery-N)" this returns "" for.
func explainSourceName(p4 any) string {
	switch v := p4.(type) {
	case *resolvedTable:
		return v.name
	case *tableMeta:
		return v.name // OpOpenWrite: an UPDATE/DELETE scanning its own target
	case *derivedSource:
		switch {
		case v.cte != nil:
			return v.cte.name
		case v.recSelf != nil:
			return ""
		case v.vtab != nil:
			return v.vtab.Table
		case v.catalogScope != scopeAny:
			return "sqlite_schema"
		case v.tbl != nil && v.tbl.name != "":
			return v.tbl.name
		}
	}
	return ""
}

// explainIndexAt names the index rooted at root, with its leading column, for
// the "USING INDEX <name> (<col>=?)" clause. Both come from the index's own
// CREATE INDEX text, which is what secondaryIndexSeekCandidates read to choose
// it in the first place; an index it cannot name declines to "SCAN" rather than
// print a plan line naming nothing.
func (p *ReadOnlyPager) explainIndexAt(root uint32, byName string) (name, col string) {
	rows, err := p.Schema()
	if err != nil {
		return "", ""
	}
	for i := range rows {
		r := &rows[i]
		// A segment table's index has root 0 and is known by its name.
		if r.Type != "index" || r.SQL == "" || r.RootPage != root || root == 0 && !equalFoldName(r.Name, byName) {
			continue
		}
		stmt, perr := parseCreateIndexStmt(r.SQL)
		if perr != nil || len(stmt.cols) == 0 {
			return "", ""
		}
		return r.Name, stmt.cols[0]
	}
	return "", ""
}
