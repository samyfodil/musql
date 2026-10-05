// This file ports, like where_plan.go, the half of src/where.c that file left
// out: whereLoopAddBtreeIndex, the cost model for a real index, plus the Index
// metadata its constants depend on (build.c's estimateTableWidth,
// estimateIndexWidth, sqlite3DefaultRowEst, recomputeColumnsNotIndexed).
//
// Why a plan matters: when SQLite scans an index, rows arrive in the index's
// key order (key columns under their collations and directions, then rowid),
// and group_concat, bare columns in an aggregate and LIMIT without a total
// ORDER BY all expose that order. Same rows, different order is a wrong answer.
//
// The second section ports the ORDER BY / GROUP BY / DISTINCT half:
// wherePathSatisfiesOrderBy and the solver machinery around it. An index that
// satisfies an ORDER BY changes which b-tree is walked and, through
// pWInfo->revMask, in which direction.
//
// Scope: everything answers only for the subset it reproduces exactly and
// reports ok == false otherwise, leaving rowid order. The subset is on
// markWherePlanIndexEligibility (where_plan_gate.go).
package engine

import (
	"slices"
	"strings"
)

// WO_* : whereexpr.c's operatorMask output. Only the bits this port can price
// are given names; a term outside them carries op == 0, which makes it inert to
// every index (it still contributes its whereLoopOutputAdjust decrement, exactly
// as a WhereTerm with eOperator 0 does).
const (
	woIn uint16 = 1 << iota
	woEq
	woLT
	woLE
	woGT
	woGE
	woIsNull
	woIs
	// woEquiv is WO_EQUIV: "of the form A==B, both columns", the bit
	// whereScanNext follows to make a constraint on B a constraint on A. It is
	// NOT in any index's opMask, so a term carrying only this bit constrains
	// nothing directly -- but it is not inert either.
	woEquiv
	// woOr is WO_OR: exprAnalyzeOrTerm's case 3 verdict on a TK_OR term. Like
	// WO_EQUIV it is in no index's opMask -- the term drives no index by itself
	// -- but it is what whereLoopAddOr looks for (see addOr,
	// where_plan_multior_r37a.go). The C also sets leftCursor to -1 for such a
	// term, which is where a non-column term already starts here.
	woOr
	// woRowval is WO_ROWVAL: a row-value equality tag-20220128a has sliced
	// into per-field terms and disabled (where_plan_rowvalue.go). It is in no
	// opMask, and the term is TERM_VIRTUAL with leftCursor -1 besides.
	woRowval
)

// whereIndexInfo is src/sqliteInt.h's Index, restricted to the fields
// whereLoopAddBtree/whereLoopAddBtreeIndex actually read. aiColumn/azColl/
// aSortOrder are nColumn long: the nKeyCol declared key columns followed by the
// table key SQLite appends -- for a rowid table exactly one entry, XN_ROWID
// (-1) under BINARY, ascending (build.c's "Append the table key to the end of
// the index").
type whereIndexInfo struct {
	name string
	// root is the index b-tree's root page, which is NOT part of the C's cost
	// model at all: it is carried so the chosen index can be IDENTIFIED
	// downstream -- see autoIndexKey.root and joinSeekReproducesKey
	// (vdbe_join_seek.go). 0 for the fake sPk.
	root       uint32
	aiColumn   []int
	azColl     []string
	aSortOrder []bool
	nKeyCol    int
	// aiRowLogEst is sqlite3DefaultRowEst's output, nKeyCol+1 long. [0] is the
	// number of rows in the index; [k] is how many rows share any particular
	// value of the first k key columns.
	aiRowLogEst []logEst
	szIdxRow    logEst
	// hasStat1, unordered and noSkipScan are Index.hasStat1, bUnordered and
	// noSkipScan: whether sqlite_stat1 had a row for this index when the
	// statistics were loaded, and the two flags decodeIntArray read off its
	// tail (planStat1.apply).
	hasStat1   bool
	unordered  bool
	noSkipScan bool
	// partial is Index.pPartIdxWhere, mirrored (where_plan_partial.go); nil
	// for an index that has none.
	partial *pcx
	// onError is IsUniqueIndex(pIdx): the index came from a UNIQUE or PRIMARY
	// KEY constraint, or a CREATE UNIQUE INDEX. uniqNotNull additionally
	// requires every key column to be NOT NULL.
	onError     bool
	uniqNotNull bool
	// ipk marks whereLoopAddBtree's FAKE Index sPk (idxType SQLITE_IDXTYPE_IPK),
	// which stands for a rowid table's own rowid: one key column, XN_ROWID,
	// szIdxRow 3, aiRowLogEst {nRowLogEst, 0}.
	ipk bool
	// primaryKey marks a WITHOUT ROWID table's own PRIMARY KEY index (idxType
	// SQLITE_IDXTYPE_PRIMARYKEY), which is that table's b-tree rather than a
	// separate one: no fake sPk stands in front of it ("else if( !HasRowid(pTab)
	// ){ pProbe = pTab->pIndex; }", where.c:4034-4035), its key columns are
	// implicitly NOT NULL and it is unconditionally covering
	// (convertToWithoutRowidTable, build.c:2362-2374/2435-2436), and
	// whereLoopAddBtreeIndex refuses to walk PAST its declared key into the
	// table columns appended behind it (where.c:3591-3592's
	// "pNew->u.btree.nEq<pProbe->nKeyCol || pProbe->idxType!=SQLITE_IDXTYPE_PRIMARYKEY").
	primaryKey  bool
	colNotIdxed uint64
	isCovering  bool
	// exprs is parallel to aiColumn: nil for a plain-column entry, or that key
	// column's CREATE INDEX expression when aiColumn[k] == xnExpr. Mirrors
	// src/sqliteInt.h's Index.aColExpr, restricted (like the rest of this
	// struct) to what the single-table, no-seek-term covering-scan slice of
	// whereLoopAddBtreeIndex needs -- see where_plan_exprindex_cover.go's
	// package comment for the exact scope.
	exprs []Expr
}

// xnRowid is where.c's XN_ROWID: the aiColumn[] entry standing for the rowid.
// It is also the value wherePlanColumnRef gives a rowid/INTEGER PRIMARY KEY
// reference, which is what lets the two be compared directly.
const xnRowid = -1

// xnExpr is where.c's XN_EXPR: the aiColumn[] entry marking a key column
// that is an EXPRESSION (whereIndexInfo.exprs[k], not a table column at
// all) rather than a plain column reference.
const xnExpr = -2

// whereStdType / whereStdTypeAff are global.c's sqlite3StdType[] and
// sqlite3StdTypeAffinity[]: the six datatype spellings sqlite3AddColumn
// recognises WHOLE (an exact, case-insensitive, length-equal match of the
// dequoted type name) and prices without running sqlite3AffinityType.
var whereStdType = [6]string{"ANY", "BLOB", "INT", "INTEGER", "REAL", "TEXT"}
var whereStdTypeAff = [6]affinity{affNumeric, affNone, affInteger, affInteger, affReal, affText}

// whereColumnSzEst ports Column.szEst -- sqlite3AddColumn's own value for a
// column with no declared type or a standard one, and sqlite3AffinityType's
// otherwise. It is scaled so that "the size of an INT is 1"; estimateTableWidth
// and estimateIndexWidth sum it, and the ratio of those two sums is what decides
// whether SQLite prefers scanning a covering index to scanning the table.
//
// The "affinity <= SQLITE_AFF_TEXT" test in the C is over SQLITE_AFF_BLOB
// (0x41) and SQLITE_AFF_TEXT (0x42), which this package spells affNone and
// affText; NUMERIC/INTEGER/REAL are all above it.
func whereColumnSzEst(declType string) uint32 {
	if declType == "" {
		// "If there is no type specified, columns have the default affinity
		// 'BLOB' with a default size of 4 bytes" -- szEst 1.
		return 1
	}
	if len(declType) >= 3 {
		for i, std := range whereStdType {
			if len(declType) == len(std) && equalFoldName(declType, std) {
				if whereStdTypeAff[i] == affNone || whereStdTypeAff[i] == affText {
					return 5
				}
				return 1
			}
		}
	}
	return whereAffinityTypeSzEst(declType)
}

// whereAffinityTypeSzEst is the second half of sqlite3AffinityType (build.c):
// the same rolling 4-byte hash over the lower-cased type name, kept because the
// szEst it computes depends on WHERE in the name "char"/"blob(" matched, which
// this package's own substring-based typeAffinity does not record.
//
//	BLOB/TEXT/CLOB with no width  -> v = 16 -> szEst 5
//	VARCHAR(20), BLOB(7), ...     -> v = the first integer after the match
//	CHAR / VARCHAR / CHARACTER    -> no digits follow, so v stays 0 -> szEst 1
//	anything numeric              -> v stays 0 -> szEst 1
func whereAffinityTypeSzEst(z string) uint32 {
	var h uint32
	aff := affNumeric
	zChar := -1
	hash := func(s string) uint32 {
		var x uint32
		for i := 0; i < len(s); i++ {
			x = (x << 8) + uint32(s[i])
		}
		return x
	}
	hChar, hClob, hText := hash("char"), hash("clob"), hash("text")
	hBlob, hReal, hFloa, hDoub := hash("blob"), hash("real"), hash("floa"), hash("doub")
	hInt := hash("int")
	for i := 0; i < len(z); i++ {
		c := z[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		h = (h << 8) + uint32(c)
		switch {
		case h == hChar:
			aff = affText
			zChar = i + 1
		case h == hClob, h == hText:
			aff = affText
		case h == hBlob && (aff == affNumeric || aff == affReal):
			aff = affNone // SQLITE_AFF_BLOB
			if i+1 < len(z) && z[i+1] == '(' {
				zChar = i + 1
			}
		case (h == hReal || h == hFloa || h == hDoub) && aff == affNumeric:
			aff = affReal
		case (h & 0x00FFFFFF) == hInt:
			aff = affInteger
			i = len(z) // the C "break"s out of the whole loop here
		}
	}
	v := uint32(0)
	if aff == affNone || aff == affText { // aff < SQLITE_AFF_NUMERIC
		if zChar >= 0 {
			for j := zChar; j < len(z); j++ {
				if z[j] >= '0' && z[j] <= '9' {
					// sqlite3GetInt32 stops at the first non-digit.
					n := uint32(0)
					for ; j < len(z) && z[j] >= '0' && z[j] <= '9'; j++ {
						n = n*10 + uint32(z[j]-'0')
						if n > 1<<30 {
							break
						}
					}
					v = n
					break
				}
			}
		} else {
			v = 16 // "BLOB, TEXT, CLOB -> r=5 (approx 20 bytes)"
		}
	}
	v = v/4 + 1
	if v > 255 {
		v = 255
	}
	return v
}

// whereTableSzRow ports estimateTableWidth (build.c): the sum of every column's
// szEst, plus one for the implicit rowid when the table has no INTEGER PRIMARY
// KEY, scaled by 4 and taken through sqlite3LogEst.
func whereTableSzRow(cols []columnInfo, ipkIndex int) logEst {
	var w uint32
	for i := range cols {
		w += whereColumnSzEst(cols[i].DeclType)
	}
	if ipkIndex < 0 {
		w++
	}
	return logEstFromInt(uint64(w) * 4)
}

// whereIndexSzRow ports estimateIndexWidth (build.c): the same sum over the
// index's own columns, with the trailing rowid counting as 1.
func whereIndexSzRow(idx *whereIndexInfo, cols []columnInfo) logEst {
	var w uint32
	for _, x := range idx.aiColumn {
		if x < 0 {
			w++
		} else {
			w += whereColumnSzEst(cols[x].DeclType)
		}
	}
	return logEstFromInt(uint64(w) * 4)
}

// whereDefaultRowEst ports sqlite3DefaultRowEst (build.c), the row estimates an
// index gets when the schema is parsed, from its table's default nRowLogEst of
// 200 (whereDefaultRowLogEst, where_plan.go). The statistics sqlite_stat1 loads
// then replace them or re-run this from the table's loaded estimate
// (planStat1.apply, via whereDefaultRowEstFrom).
func whereDefaultRowEst(idx *whereIndexInfo) {
	whereDefaultRowEstFrom(idx, whereDefaultRowPartial(idx, whereDefaultRowLogEst))
}

// whereDefaultRowPartial is sqlite3DefaultRowEst's "if( pIdx->pPartIdxWhere!=0
// ){ x -= 10; }" (build.c:4577): a partial index is guessed at half its table.
func whereDefaultRowPartial(idx *whereIndexInfo, x logEst) logEst {
	if idx.partial != nil {
		return x - 10
	}
	return x
}

// whereDefaultRowEstFrom is sqlite3DefaultRowEst for a table whose nRowLogEst
// is x, already raised to 99 if it was less (the caller owns that clamp, since
// it changes the TABLE) and already halved for a partial index
// (whereDefaultRowPartial).
func whereDefaultRowEstFrom(idx *whereIndexInfo, x logEst) {
	aVal := [5]logEst{33, 32, 30, 28, 26}
	// nKeyCol+2, not nKeyCol+1: sqlite3AllocateIndexObject sizes aiRowLogEst at
	// nColumn+1, and for a rowid table nColumn is nKeyCol+1 (the appended
	// XN_ROWID). sqlite3DefaultRowEst fills only 0..nKeyCol, so the last entry
	// keeps the zero sqlite3DbMallocZero left there -- and it is REACHED: an
	// equality on the rowid matches that trailing key column, taking nEq to
	// nKeyCol+1 and reading aiRowLogEst[nKeyCol+1]. Sizing it nKeyCol+1 instead
	// panicked on 7 statements of the corpus (indexedby.test, intpkey.test).
	a := make([]logEst, idx.nKeyCol+2)
	a[0] = x
	nCopy := len(aVal)
	if idx.nKeyCol < nCopy {
		nCopy = idx.nKeyCol
	}
	for i := 0; i < nCopy; i++ {
		a[i+1] = aVal[i]
	}
	for i := nCopy + 1; i <= idx.nKeyCol; i++ {
		a[i] = 23 // sqlite3LogEst(5)
	}
	if idx.onError {
		a[idx.nKeyCol] = 0 // sqlite3LogEst(1)
	}
	idx.aiRowLogEst = a
}

// whereRecomputeColumnsNotIndexed ports recomputeColumnsNotIndexed (build.c):
// a 0 bit for every indexed column below 63, a 1 for everything else, and the
// high bit always 1. AND-ed with SrcItem.colUsed it answers "is this a covering
// index".
func whereRecomputeColumnsNotIndexed(idx *whereIndexInfo) {
	var m uint64
	for _, x := range idx.aiColumn {
		if x >= 0 && x < 63 {
			m |= uint64(1) << uint(x)
		}
	}
	idx.colNotIdxed = ^m
}

// wherePlanIndexList assembles the Index chain whereLoopAddBtree iterates for
// one plain rowid base table: the fake sPk first, then every real index.
// ok == false declines the statement -- an index this cannot reproduce exactly
// is one SQLite would price and this port would not.
//
// Declined:
//
//   - a partial index when stmt == nil, or when stmt.From has two or more
//     sources: whereUsablePartialIndex (where.c:3700) tests the one shared
//     WhereClause, which then also holds every INNER join's ON
//     (sqlite3ProcessJoin, select.c:657), and indexProvablyIrrelevant never
//     looks at joinSource.on. A single-table stmt asks indexProvablyIrrelevant
//     instead. wherePlanPartialIndexList chains a partial index whose condition
//     where_plan_partial.go mirrors.
//   - a pure expression index only when stmt == nil, since isCovering needs a
//     statement. Otherwise it is represented (XN_EXPR, whereIndexInfo.exprs)
//     and its cost and order reproduced. A real expression seek
//     (whereScanInitIndexExpr, where.c:461) is not, which costs only speed:
//     WHERE is always applied per row from the row itself.
//   - an index whose CREATE INDEX text will not parse, or whose leading entry
//     is not a declared column;
//   - an automatic index (from UNIQUE or a non-INTEGER PRIMARY KEY) whose shape
//     cannot be recovered from the table's DDL. It is matched by ordinal and
//     its name verified as sqlite_autoindex_<table>_<n>.
//
// "INDEXED BY <name>" returns only that index and no sPk, as whereLoopAddBtree
// does ("pProbe = pSrc->u2.pIBIndex", next = 0 when isIndexedBy). With one
// candidate b-tree, the row order is settled before any cost is computed.
func wherePlanIndexList(p *ReadOnlyPager, tbl *resolvedTable, tableName, indexedBy string, notIndexed bool, stmt *SelectStmt) ([]whereIndexInfo, logEst, bool) {
	return wherePlanIndexChain(p, tbl, tableName, indexedBy, notIndexed, stmt, false)
}

// wherePlanPartialIndexList is wherePlanIndexList for a caller whose loops are
// built by addBtree over the statement's own WhereClause, which is where
// whereUsablePartialIndex (where.c:4124) runs: a partial index whose condition
// where_plan_partial.go can mirror is chained like any other, carrying that
// condition (whereIndexInfo.partial), and addBtree decides per WhereClause
// whether to skip it. Every other caller keeps wherePlanIndexList's verdict,
// because it reads the chain without asking that question.
func wherePlanPartialIndexList(p *ReadOnlyPager, tbl *resolvedTable, tableName, indexedBy string, notIndexed bool, stmt *SelectStmt) ([]whereIndexInfo, logEst, bool) {
	return wherePlanIndexChain(p, tbl, tableName, indexedBy, notIndexed, stmt, true)
}

func wherePlanIndexChain(p *ReadOnlyPager, tbl *resolvedTable, tableName, indexedBy string, notIndexed bool, stmt *SelectStmt, partialOK bool) ([]whereIndexInfo, logEst, bool) {
	if p == nil || tbl == nil || tableName == "" {
		return nil, 0, false
	}
	szTabRow := whereTableSzRow(tbl.cols, tbl.ipkIndex)
	if tbl.withoutRowid {
		return wherePlanWithoutRowidIndexList(p, tbl, tableName, indexedBy, notIndexed, szTabRow)
	}

	// sPk: whereLoopAddBtree's fake Index for the rowid. memset to zero first,
	// so uniqNotNull/isCovering/colNotIdxed are all 0 and only the fields
	// assigned below are set.
	sPk := whereIndexInfo{
		name: "sqlite_rowid", aiColumn: []int{xnRowid}, azColl: []string{"BINARY"},
		aSortOrder: []bool{false}, nKeyCol: 1,
		aiRowLogEst: []logEst{whereDefaultRowLogEst, 0},
		szIdxRow:    3, // TUNING: interior rows of an IPK table are very small
		onError:     true, ipk: true,
	}
	out := []whereIndexInfo{sPk}
	if indexedBy != "" {
		out = out[:0] // isIndexedBy drops sPk entirely
	}
	if notIndexed {
		// "pProbe = (pSrc->fg.notIndexed ? 0 : pProbe->pNext)": the chain stops
		// at sPk, so no real index of this table is ever priced -- and none of
		// them has to be reproducible for the answer to be exact.
		return out, szTabRow, true
	}

	rows, err := p.Schema()
	if err != nil {
		return nil, 0, false
	}
	var autoSpecs []autoIndexSpec
	autoSeen := 0
	colIndexOf := func(name string) (int, bool) {
		for j := range tbl.cols {
			if equalFoldName(tbl.cols[j].Name, name) {
				return j, true
			}
		}
		return 0, false
	}
	for i := range rows {
		r := &rows[i]
		if r.Type != "index" || !equalFoldName(r.TblName, tableName) {
			continue
		}
		if indexedBy != "" && !equalFoldName(r.Name, indexedBy) {
			// Still counted below, because an automatic index's ordinal has to
			// keep advancing for the sqlite_autoindex_<table>_<n> name check.
			if r.SQL == "" {
				autoSeen++
			}
			continue
		}
		if r.RootPage == 0 && !p.ScannedFromSegments(tbl.root) {
			return nil, 0, false
		}
		// A segment table's indexes have RootPage 0 (the format stores an
		// index's SQL, not its data), and the seek paths refuse them on that.
		// The plan must not: it decides what order C would visit rows in, and
		// declining made every order-sensitive aggregate over a table that merely
		// has an index decline. The candidate is priced from its SQL; the result
		// is a sort key applied to scanned rows (emitJoinLoops), not a b-tree.
		var cols, colls []string
		var desc []bool
		var exprs []Expr
		var partial *pcx
		unique := false
		if r.SQL == "" {
			// An automatic index. Recover its shape from the TABLE's own DDL.
			if autoSpecs == nil {
				tblSQL := ""
				for j := range rows {
					if rows[j].Type == "table" && equalFoldName(rows[j].Name, tableName) {
						tblSQL = rows[j].SQL
						break
					}
				}
				if tblSQL == "" {
					return nil, 0, false
				}
				_, _, specs, perr := parseCreateTableColumnsAndAutoIndexes(tblSQL, false)
				if perr != nil {
					return nil, 0, false
				}
				autoSpecs = specs
				if autoSpecs == nil {
					autoSpecs = []autoIndexSpec{}
				}
			}
			if autoSeen >= len(autoSpecs) {
				return nil, 0, false
			}
			spec := autoSpecs[autoSeen]
			autoSeen++
			// sqlite3CreateIndex names these sqlite_autoindex_<table>_<n> with
			// n counting from 1 in constraint order. Verifying it is what makes
			// the ORDINAL match above safe rather than assumed.
			want := "sqlite_autoindex_" + tableName + "_" + itoaSmall(autoSeen)
			if !equalFoldName(r.Name, want) {
				return nil, 0, false
			}
			if spec.onConflict == conflictReplace {
				// sqlite3CreateIndex's exit path moves every ON CONFLICT REPLACE
				// index to the END of pTab->pIndex, re-running after each
				// prepend. That reordering is not reproduced, and the chain
				// ORDER decides every cost tie below, so decline instead.
				return nil, 0, false
			}
			cols, colls, desc, unique = spec.cols, spec.colls, spec.desc, true
		} else {
			ixStmt, perr := parseCreateIndexStmt(r.SQL)
			if perr != nil || len(ixStmt.cols) == 0 {
				return nil, 0, false
			}
			if ixStmt.exprOrPartial {
				// A partial index is skippable rather than a whole-table decline
				// only when the caller supplied the query and the index is
				// provably irrelevant to it (where_plan_exprindex_skip.go) --
				// unless partialOK, where one whose condition
				// where_plan_partial.go mirrors is chained and addBtree runs
				// whereUsablePartialIndex (where.c:3700) itself. An
				// expression-keyed partial index is not mirrored: its
				// isCovering would need whereIsCoveringIndex over the condition
				// too. stmt is nil from callers not proven safe for any of this.
				if stmt == nil {
					return nil, 0, false
				}
				if ixStmt.where != nil && partialOK && !slices.ContainsFunc(ixStmt.exprs, func(e Expr) bool { return e != nil }) {
					partial = wherePlanPartialWhere(tbl, tableName, ixStmt.where)
				}
				if ixStmt.where != nil && partial == nil {
					// whereUsablePartialIndex (where.c:3700) tests sqlite3WhereBegin's
					// whole WhereClause, which for two or more FROM items also holds
					// every INNER join's ON (sqlite3ProcessJoin, select.c:657).
					// indexProvablyIrrelevant never reads joinSource.on, so it could
					// drop an index an ON clause would have made usable. Multi-table
					// callers keep the whole-table decline.
					if len(stmt.From) > 1 || !indexProvablyIrrelevant(ixStmt, stmt) {
						return nil, 0, false
					}
					continue
				}
				// A PURE expression index (ixStmt.where == nil, at least one
				// key column an expression): represented directly by the
				// column-building loop below, rather than skipped or
				// declined -- see where_plan_exprindex_cover.go's own
				// comment for exactly what this reproduces and this file's
				// own SCOPE note above for the one piece that stays a
				// missed optimization rather than a correctness gap.
			}
			cols, colls, desc, unique, exprs = ixStmt.cols, ixStmt.collate, ixStmt.desc, ixStmt.unique, ixStmt.exprs
		}

		idx := whereIndexInfo{name: r.Name, root: r.RootPage, nKeyCol: len(cols), onError: unique, uniqNotNull: unique, partial: partial}
		for k, cn := range cols {
			if cn == "" {
				// An expression key column (parseCreateIndexStmt leaves cols[k]
				// empty and exprs[k] set for anything but a bare column name;
				// only reachable here for a PURE expression index, since a
				// partial one still returns/continues above). Represented as
				// XN_EXPR: no table column to resolve, no uniqNotNull test (its
				// NULL-ness is unknown, so it is left exactly as unique already
				// set it -- matching build.c's own "expression is assumed to be
				// already unique" non-treatment, since sqlite3CreateIndex never
				// clears uniqNotNull for an expression term either), and its
				// collation comes from the CREATE INDEX text alone (an
				// expression has no underlying column to fall back to).
				coll := "BINARY"
				if k < len(colls) && colls[k] != "" {
					coll = strings.ToUpper(colls[k])
				}
				d := k < len(desc) && desc[k]
				idx.aiColumn = append(idx.aiColumn, xnExpr)
				idx.azColl = append(idx.azColl, coll)
				idx.aSortOrder = append(idx.aSortOrder, d)
				var e Expr
				if k < len(exprs) {
					e = exprs[k]
				}
				idx.exprs = append(idx.exprs, e)
				continue
			}
			ci, ok := colIndexOf(cn)
			if !ok {
				return nil, 0, false
			}
			// build.c: a reference resolving to the INTEGER PRIMARY KEY has
			// iColumn -1, and "if( j<0 ) j = pTab->iPKey" puts the real column
			// number back -- while SKIPPING the notNull test that would
			// otherwise clear uniqNotNull.
			if !tbl.cols[ci].IsRowidAlias && !tbl.cols[ci].NotNull {
				idx.uniqNotNull = false
			}
			idx.exprs = append(idx.exprs, nil)
			coll := effectiveCollation(tbl.cols[ci].Collation)
			if k < len(colls) && colls[k] != "" {
				coll = strings.ToUpper(colls[k])
			}
			d := k < len(desc) && desc[k]
			idx.aiColumn = append(idx.aiColumn, ci)
			idx.azColl = append(idx.azColl, coll)
			idx.aSortOrder = append(idx.aSortOrder, d)
		}
		// "Append the table key to the end of the index": for a rowid table
		// that is exactly one XN_ROWID column under BINARY, ascending.
		idx.aiColumn = append(idx.aiColumn, xnRowid)
		idx.azColl = append(idx.azColl, "BINARY")
		idx.aSortOrder = append(idx.aSortOrder, false)
		idx.exprs = append(idx.exprs, nil)

		whereDefaultRowEst(&idx)
		idx.szIdxRow = whereIndexSzRow(&idx, tbl.cols)
		whereRecomputeColumnsNotIndexed(&idx)
		// isCovering: build.c sets it only for an explicit CREATE INDEX
		// (pTblName!=0) whose nColumn reaches the table's column count and
		// which holds every column but the INTEGER PRIMARY KEY.
		if r.SQL != "" && len(idx.aiColumn) >= len(tbl.cols) {
			idx.isCovering = true
			for j := range tbl.cols {
				if j == tbl.ipkIndex {
					continue
				}
				found := false
				for _, x := range idx.aiColumn {
					if x == j {
						found = true
						break
					}
				}
				if !found {
					idx.isCovering = false
					break
				}
			}
		}
		if !idx.isCovering && stmt != nil && len(stmt.From) == 1 {
			// The build.c check above only looks at plain columns, so it can
			// never mark an expression index covering. stmtCoveredByExprIndex
			// is the query-aware fallback, as whereIsCoveringIndex
			// (where.c:3831) runs when pIdx->bHasExpr. Skipped when the index
			// has no expression column.
			//
			// Single-table only: its colIndexed closure matches a bare
			// ColumnExpr by name alone (exprCoveredForIndexScan ignores the
			// qualifier), so with a second FROM item another table's
			// same-named column would read as covered -- a falsely cheap loop,
			// not just a missed optimization.
			hasExpr := false
			for _, e := range idx.exprs {
				if e != nil {
					hasExpr = true
					break
				}
			}
			if hasExpr {
				colIndexed := func(name string) bool {
					if ci, ok := colIndexOf(name); ok {
						for _, x := range idx.aiColumn {
							if x == ci {
								return true
							}
						}
						return false
					}
					// Not a declared column: a rowid pseudo-name is always
					// covered (every index implicitly carries XN_ROWID);
					// anything else is left uncovered rather than guessed.
					return isRowidName(r33sFoldIdent(name))
				}
				idx.isCovering = stmtCoveredByExprIndex(stmt, colIndexed, idx.exprs)
			}
		}
		out = append(out, idx)
	}
	if indexedBy != "" {
		if len(out) != 1 {
			return nil, 0, false // the hint names no index of this table
		}
		return out, szTabRow, true
	}
	if autoSpecs != nil && autoSeen != len(autoSpecs) {
		// The table declares a UNIQUE/PRIMARY KEY constraint with no matching
		// automatic-index row (or the other way round). Whatever the reason,
		// the Index chain here is not the one SQLite has.
		return nil, 0, false
	}
	// sqlite3CreateIndex prepends ("pIndex->pNext = pTab->pIndex; pTab->pIndex =
	// pIndex;", build.c) and the schema loads in rowid order, so pTab->pIndex is
	// newest first, with sPk in front. This is the candidates' generation order,
	// the solver's last tie-break once costs are equal -- common for two indexes
	// on the same leading column.
	for i, j := 1, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, szTabRow, true
}

// itoaSmall renders a small non-negative int without pulling in strconv, which
// nothing else in this file needs.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// The WhereLoop factory and the path solver. Everything below is MULTI-loop:
// there is one WhereLoop type, one whereLoopInsert, one wherePathSatisfiesOrderBy
// and one wherePathSolver, shared by the one-table and the multi-table arms
// exactly as in the C. A FROM clause of one item is simply nLoop == 1.
// ---------------------------------------------------------------------------

// whereIdxTerm is one WHERE conjunct as exprAnalyze (whereexpr.c) leaves it --
// a WhereTerm, restricted to the fields this port's cost model reads.
type whereIdxTerm struct {
	// op is eOperator: operatorMask(pExpr->op) masked by WO_ALL or WO_EQUIV,
	// plus WO_EQUIV itself for a transitive-eligible column-to-column equality.
	// 0 marks a conjunct no index can use -- which still contributes its
	// whereLoopOutputAdjust decrement.
	op uint16
	// cursor/column are leftCursor/leftColumn AFTER exprCommute: the FROM item
	// and the column of it this term constrains, with column == xnRowid for a
	// rowid/INTEGER PRIMARY KEY reference. cursor is -1 when neither operand is
	// a plain column, in which case op is 0 too.
	cursor int
	column int
	// rCursor/rColumn are the same for the term's RIGHT operand after the
	// commute -- whereRightSubexprIsColumn (where.c), which whereScanNext needs
	// both to EXTEND the equivalence set and to skip a term that merely states
	// the equality it is already following.
	rCursor int
	rColumn int
	// prereqAll / prereqRight are the C's own Bitmasks over FROM items.
	prereqAll   whereMask
	prereqRight whereMask
	// virt is TERM_VIRTUAL: exprAnalyze's commuted copy, or the "col > NULL"
	// child it derives from "col IS NOT NULL". whereLoopOutputAdjust walks only
	// pWC->nBase -- the terms through the last non-virtual one -- so a virtual
	// term contributes no nOut decrement of its own.
	virt bool
	// parent is WhereTerm.iParent: the index of the term this one is a derived
	// CHILD of, or -1. whereLoopOutputAdjust treats a parent as "used by the
	// access method" whenever one of its children is.
	parent int
	// vnull is TERM_VNULL, which whereRangeAdjust reads: the "col > NULL" bound
	// derived from "col IS NOT NULL" does NOT get the flat -20 an ordinary range
	// bound does, because it excludes only the NULL rows.
	vnull bool
	// outerON is EP_OuterON and onItem is w.iJoin: the term came from the ON
	// clause of an OUTER join, and this is the item that join NULL-extends.
	outerON bool
	onItem  int
	// aff is the comparison's affinity, for sqlite3IndexAffinityOk; coll is
	// sqlite3ExprCompareCollSeq's answer, which whereScanNext requires to equal
	// the index column's own azColl entry.
	aff  affinity
	coll string
	// smallInt is sqlite3ExprIsInteger(pRight,&k,0) && -1<=k<=1, which decides
	// whereLoopOutputAdjust's iReduce cap.
	smallInt bool
	// inCount is pExpr->x.pList->nExpr for a woIn term -- the K of
	// whereLoopAddBtreeIndex's "nIn = sqlite3LogEst(nExpr)" -- or -1 for
	// "IN (SELECT ...)". Zero otherwise.
	inCount int
	// iField is WhereTerm.u.x.iField: for one field's child of a row-value
	// IN (whereRowInFields), that field's 1-based position; 0 otherwise. The
	// children of one IN share its pExpr, which here is their common parent.
	iField int
	// colMask[i] is the set of item i's columns the WHOLE term references, with
	// the rowid (iColumn -1) contributing no bit -- what
	// sqlite3ExprCoveredByIndex walks. colsKnown is false when the walk met a
	// node it could not attribute to a column, in which case the term counts as
	// not covered.
	colMask   []uint64
	colsKnown bool
	// orInfo is WhereTerm.u.pOrInfo, set only on a term carrying woOr: the
	// broken-out disjuncts and the set of tables all of them constrain. See
	// whereOrInfo (where_plan_multior_r37a.go).
	orInfo *whereOrInfo
	// truthProb is WhereTerm.truthProb: 1 for no hint, else the LogEst a
	// likelihood()/likely()/unlikely() at the conjunct's root gave it (see
	// whereSkipCollateAndLikely). Terms built without one -- a zero value --
	// read as a hint of probability 1.0, so every construction site sets it.
	truthProb logEst
	// likeInfix is a LIKE/GLOB written as an operator (EP_InfixFunc), whose
	// pattern's likeSz non-wildcard bytes whereLoopOutputAdjust charges 2 each
	// (where_plan_like.go). likeOpt is TERM_LIKEOPT: one of the LIKE
	// optimization's virtual pair, the lower bound immediately followed by the
	// upper.
	likeInfix bool
	likeSz    int
	likeOpt   bool
	// inRefuse is wherePlanInRhsRefuse: an "x IN (SELECT ...)" whose IN loop
	// visits a row set the WHERE does not describe, refused by plan() only in
	// a winning loop that uses it.
	inRefuse bool
	// pcx is pExpr as exprAnalyze leaves it -- commuted in place, or the
	// constant false tag-20230504-1 makes it -- mirrored for
	// whereUsablePartialIndex (where_plan_partial.go). nil is unknown.
	pcx *pcx
}

// whereIdxLoop is one priced WhereLoop. Only what decides the winner and its
// observable consequence is kept: the costs, WHICH item it reads and WHICH
// index it scans (which -- and nothing else in the loop -- decides the order the
// rows arrive in), and the wsFlags/nEq/aLTerm fields wherePathSatisfiesOrderBy,
// whereLoopInsert and whereInterstageHeuristic read back off it.
type whereIdxLoop struct {
	iTab       int
	maskSelf   whereMask
	prereq     whereMask
	rSetup     logEst
	rRun, nOut logEst
	// idx is u.btree.pIndex, nil for the transient automatic index (autoIdx).
	idx *whereIndexInfo

	// nEq is WhereLoop.u.btree.nEq and lTerm is aLTerm[] (indices into
	// build.terms). nSkip is WhereLoop.nSkip: a skip-scan's leading columns,
	// whose aLTerm slots hold no term (-1 here, a NULL pointer in C) --
	// otherwise aLTerm[j] for j < nEq is the term constraining index column j.
	nEq   int
	nSkip int
	lTerm []int

	// The wsFlags bits this port reproduces. oneRow is WHERE_ONEROW, idxOnly is
	// WHERE_IDX_ONLY, indexed is WHERE_INDEXED (false for the fake sPk, whose
	// loops carry WHERE_IPK instead, and for an automatic index), autoIdx is
	// WHERE_AUTO_INDEX, and colEq/colNull/colRange are the WHERE_COLUMN_* trio
	// that makes up WHERE_CONSTRAINT.
	oneRow   bool
	idxOnly  bool
	indexed  bool
	autoIdx  bool
	colEq    bool
	colNull  bool
	colRange bool
	colIn    bool
	// multiOr is WHERE_MULTI_OR: the union of one sub-scan per disjunct of the
	// single OR term lTerm[0] names. u is memset to 0 alongside it in the C, so
	// idx stays nil -- which is what makes wherePathSatisfiesOrderBy answer "this
	// path satisfies no ORDER BY term" for such a loop.
	multiOr bool

	// sortIdx is WhereLoop.iSortIdx: the 1-based position of this loop's index
	// in the probe chain when indexMightHelpWithOrderBy said so, else 0. It is
	// half of whereLoopFindLesser's pool key -- two loops with different
	// iSortIdx never displace one another.
	sortIdx int

	// disabled is whereInterstageHeuristic's "pLoop->prereq = ALLBITS", which
	// keeps a loop out of the SECOND wherePathSolver pass only.
	disabled bool
}

// wherePlanIdxBuild is WhereLoopBuilder plus the immutable inputs the cost model
// reads out of the FROM clause.
type wherePlanIdxBuild struct {
	items []whereItem
	terms []whereIdxTerm
	loops []whereIdxLoop
	// sort is pWInfo->pOrderBy plus the wctrlFlags bits that decide what
	// wherePathSatisfiesOrderBy makes of it. nil, or an empty term list, means
	// the statement asks no ordering question at all -- in which case
	// wherePathSolver never runs its second, ORDER-BY-aware pass and every
	// candidate's isOrdered stays 0.
	sort *whereSortCtl
	// noAutoIndex is "(pParse->db->flags & SQLITE_AutoIndex)==0" -- PRAGMA
	// automatic_index off -- which skips whereLoopAddBtree's automatic-index
	// block entirely.
	noAutoIndex bool

	// nQueryLoop seeds solve()'s search: see wherePlanInput.nQueryLoop, whose
	// value this is copied from at construction (every (b *wherePlanIdxBuild)
	// builder in this package is built from a wherePlanInput or from literal
	// fields matching one). Zero everywhere except a trusted nested compile.
	nQueryLoop logEst

	// orSet is WhereLoopBuilder.pOrSet: non-nil while whereLoopAddOr is PRICING
	// one disjunct of an OR term, in which case whereLoopInsert records only the
	// (prereq, rRun, nOut) triple and builds no WhereLoop at all (where.c:2851).
	// It also switches off the automatic-index block (where.c:4067).
	orSet *whereOrSet

	// nBase and mainTerms exist because whereLoopAddOr's sub-builder reads
	// three different WhereClauses:
	//
	//   - pBuilder->pWC holds just the disjunct, but whereScanNext walks out to
	//     pOuter (where.c:444), so terms is the disjunct followed by every outer
	//     term;
	//   - whereLoopOutputAdjust walks only pWC->nBase (where.c:3048), the
	//     disjunct, so nBase caps that walk;
	//   - the non-covering lookup discount reads pWInfo->sWC (where.c:4239), the
	//     main clause: mainTerms.
	//
	// Both are zero/nil for an ordinary build.
	nBase     int
	mainTerms []whereIdxTerm
	// nWC is pBuilder->pWC's own nTerm when terms also holds its pOuter chain
	// (a whereLoopAddOr sub-build): whereUsablePartialIndex reads pWC->a
	// alone. 0 means all of terms.
	nWC int
	// partialUnknown records a partial index whereUsablePartialIndex could not
	// be answered for; the plan declines (plan()).
	partialUnknown bool

	// starDone/starUsed are pWInfo->bStarDone / bStarUsed: computeMxChoice's
	// star-query detection runs at most once per statement, and its verdict then
	// fixes mxChoice for BOTH solver passes.
	starDone, starUsed bool
	// planLimit is WhereLoopBuilder.iPlanLimit and planLimitHit records that it
	// ran out -- see wherePlannerLimit.
	planLimit    int
	planLimitHit bool
	// cache is wherePlanInput.loops.
	cache *whereLoopCache
	// orSubclause is wherePlanInput.orSubclause.
	orSubclause bool
}

// wherePlannerLimit and wherePlannerLimitIncr are SQLITE_QUERY_PLANNER_LIMIT and
// _INCR (whereInt.h:456): a budget of 20000 whereLoopInsert calls plus 1000 per
// FROM term, after which C abandons the term's remaining access methods
// ("abbreviated query algorithm search", where.c:5026).
//
// That search is not reproduced; running out declines (planLimitHit). It is
// unreachable for any FROM clause the gate admits, but asserted rather than
// assumed, because past the limit C picks a different plan, not a slower one.
const (
	wherePlannerLimit     = 20000
	wherePlannerLimitIncr = 1000
)

// whereIdxState is the mutable half of WhereLoop that whereLoopAddBtreeIndex
// saves and restores around each term (pNew->u.btree.nEq, nLTerm, wsFlags,
// nOut, prereq). Copying it by value is what the C's saved_* locals achieve.
type whereIdxState struct {
	nEq     int
	nSkip   int   // WhereLoop.nSkip; > 0 is WHERE_SKIPSCAN
	lTerm   []int // indices into build.terms, in aLTerm order; -1 is a skipped column
	nOut    logEst
	prereq  whereMask
	idxOnly bool // WHERE_IDX_ONLY
	btmLim  bool // WHERE_BTM_LIMIT
	topLim  bool // WHERE_TOP_LIMIT
	isRange bool // WHERE_COLUMN_RANGE
	oneRow  bool // WHERE_ONEROW -- accumulates across the recursion, as wsFlags does
	colEq   bool // WHERE_COLUMN_EQ
	colNull bool // WHERE_COLUMN_NULL
	colIn   bool // WHERE_COLUMN_IN
	sortIdx int  // iSortIdx, fixed for the whole index by addBtree
}

// whereScan ports where.c's WhereScan: the iterator whereScanInit/whereScanNext
// drive over the WhereClause, INCLUDING the WO_EQUIV transitive closure. When a
// search for constraints on X meets a term "X = Y" that termIsEquivalence
// accepted, Y joins the search, so a constraint on Y is also a constraint on X.
// That closure needs a second cursor and so was unreachable while this file
// answered only for one table; with a join it is live, and omitting it would
// hide loops SQLite builds.
//
// The index-expression arm (XN_EXPR) is not reproduced: wherePlanIndexList
// declines an expression index outright.
type whereScan struct {
	b        *wherePlanIdxBuild
	aiCur    []int // aiCur/aiColumn are the equivalence set; [0] is the original
	aiColumn []int
	iEquiv   int // 1-based, as the C's is
	k        int // resume position in b.terms
	opMask   uint16
	idxaff   affinity
	collName string
	hasColl  bool
}

// whereScanInitCol is whereScanInit with pIdx == 0: no affinity or collation
// filter, and iColumn is a column of the TABLE.
func (b *wherePlanIdxBuild) whereScanInitCol(iTab, iColumn int, opMask uint16) *whereScan {
	return &whereScan{
		b: b, opMask: opMask, iEquiv: 1,
		aiCur: []int{iTab}, aiColumn: []int{iColumn},
	}
}

// whereScanInitIdx is whereScanInit with a pIdx: iColumn is index column j,
// which is translated to the table column it names -- with whereScanInit's own
// "if( iColumn==pIdx->pTable->iPKey ) iColumn = XN_ROWID", which ALSO leaves
// idxaff/zCollName unset, so an index on the INTEGER PRIMARY KEY column matches
// a rowid term with no affinity or collation check at all.
func (b *wherePlanIdxBuild) whereScanInitIdx(iTab, j int, opMask uint16, idx *whereIndexInfo) *whereScan {
	s := &whereScan{b: b, opMask: opMask, iEquiv: 1, aiCur: []int{iTab}}
	iColumn := idx.aiColumn[j]
	if iColumn == b.items[iTab].ipkIndex {
		iColumn = xnRowid
	} else if iColumn >= 0 {
		s.idxaff = b.items[iTab].cols[iColumn].Aff
		s.collName = idx.azColl[j]
		s.hasColl = true
	}
	s.aiColumn = []int{iColumn}
	return s
}

// next ports whereScanNext (where.c). It answers the INDEX of the next matching
// term in b.terms, or -1.
func (s *whereScan) next() int {
	b := s.b
	for {
		iColumn := s.aiColumn[s.iEquiv-1]
		iCur := s.aiCur[s.iEquiv-1]
		for ; s.k < len(b.terms); s.k++ {
			t := &b.terms[s.k]
			if t.cursor != iCur || t.column != iColumn {
				continue
			}
			// "pScan->iEquiv<=1 || !ExprHasProperty(pTerm->pExpr, EP_OuterON)":
			// an outer join's ON term may not be followed transitively.
			if s.iEquiv > 1 && t.outerON {
				continue
			}
			if t.op&woEquiv != 0 && len(s.aiCur) < 11 && t.rCursor >= 0 {
				seen := false
				for j := range s.aiCur {
					if s.aiCur[j] == t.rCursor && s.aiColumn[j] == t.rColumn {
						seen = true
						break
					}
				}
				if !seen {
					s.aiCur = append(s.aiCur, t.rCursor)
					s.aiColumn = append(s.aiColumn, t.rColumn)
				}
			}
			if t.op&s.opMask == 0 {
				continue
			}
			if s.hasColl && t.op&woIsNull == 0 {
				// Verify the affinity and collating sequence match.
				if !indexAffinityOk(t.aff, s.idxaff) {
					continue
				}
				if !equalFoldName(t.coll, s.collName) {
					continue
				}
			}
			// Skip "Y = X" while searching for X through the equivalence Y: it
			// states the equality being followed and constrains nothing new.
			if t.op&(woEq|woIs) != 0 && t.rCursor == s.aiCur[0] && t.rColumn == s.aiColumn[0] {
				continue
			}
			k := s.k
			s.k = k + 1
			return k
		}
		if s.iEquiv >= len(s.aiCur) {
			return -1
		}
		s.k = 0
		s.iEquiv++
	}
}

// whereIndexColumnNotNull ports indexColumnNotNull (where.c).
func (b *wherePlanIdxBuild) whereIndexColumnNotNull(iTab int, idx *whereIndexInfo, iCol int) bool {
	j := idx.aiColumn[iCol]
	if j >= 0 {
		return b.items[iTab].cols[j].NotNull
	}
	return true // XN_ROWID is never NULL
}

// whereIdxOutputAdjust ports whereLoopOutputAdjust (where.c): every WHERE term
// on this loop's table not used by its access method reduces the estimated
// output. Heuristic 1 is one LogEst unit per unused term. Heuristic 2 caps it
// at 1/4 of the table (iReduce 20) for "x == EXPR", or 1/2 (iReduce 10) when
// EXPR is -1, 0 or 1. Heuristic 3 (LIKE/GLOB) is not reproduced; the gate
// declines it. The iParent walk is reproduced, for "col IS NOT NULL"'s
// TERM_VNULL child and BETWEEN's two range children.
func (b *wherePlanIdxBuild) whereIdxOutputAdjust(st *whereIdxState, prereq, maskSelf whereMask, nRow logEst) {
	notAllowed := ^(prereq | maskSelf)
	var iReduce logEst
	nBase := len(b.terms)
	if b.nBase > 0 {
		nBase = b.nBase
	}
	for i := 0; i < nBase; i++ {
		t := &b.terms[i]
		if t.virt {
			// "if( (pTerm->wtFlags & TERM_VIRTUAL)!=0 ) continue;" -- a
			// derived term, or a row-value equality sliced into non-virtual
			// terms appended after it (tag-20220128a), which is why nBase can
			// reach past virtual terms at all.
			continue
		}
		if t.prereqAll&notAllowed != 0 {
			continue
		}
		if t.prereqAll&maskSelf == 0 {
			continue
		}
		used := false
		for _, k := range st.lTerm {
			// "if( pX==pTerm ) break; if( pX->iParent>=0 && &pWC->a[pX->iParent]
			// ==pTerm ) break;" -- a term whose derived CHILD the access method
			// uses is itself used, which is what stops "col IS NOT NULL" being
			// counted again once its "col > NULL" child drove the index.
			if k >= 0 && (k == i || b.terms[k].parent == i) {
				used = true
				break
			}
		}
		if used {
			continue
		}
		if t.truthProb <= 0 {
			// "If a truth probability is specified using the likelihood()
			// hints, then use the probability provided by the application."
			st.nOut += t.truthProb
			continue
		}
		st.nOut--
		if t.op&(woEq|woIs) != 0 {
			k := logEst(20)
			if t.smallInt {
				k = 10
			}
			if iReduce < k {
				iReduce = k
			}
		} else if t.likeInfix && t.likeSz > 0 {
			// where.c:3103-3118: 2 per non-wildcard byte of the pattern.
			st.nOut -= logEst(t.likeSz * 2)
		}
	}
	if st.nOut > nRow-iReduce {
		st.nOut = nRow - iReduce
	}
}

// whereRangeScanEst ports whereRangeScanEst's non-STAT4 body (where.c) --
// mattn/go-sqlite3 v1.14.48 compiles SQLite without SQLITE_ENABLE_STAT4, so the
// histogram arm this file omits is not in the oracle either.
//
// whereRangeAdjust is folded in below: a bound with a likelihood() hint costs
// its truthProb, and any other a flat -20 UNLESS it is the TERM_VNULL bound
// "col > NULL" derived from "col IS NOT NULL", which excludes only the NULLs.
func whereRangeScanEst(nOut logEst, lower, upper *whereIdxTerm) logEst {
	// whereRangeAdjust (where.c:1916).
	adjust := func(t *whereIdxTerm, n int) int {
		switch {
		case t == nil:
			return n
		case t.truthProb <= 0:
			return n + int(t.truthProb)
		case t.vnull:
			return n
		}
		return n - 20
	}
	n := int(nOut)
	nNew := adjust(upper, adjust(lower, n))
	// TUNING: a closed range is assumed 75% more selective than the product of
	// two open ones -- unless a bound carries an explicit probability.
	if lower != nil && lower.truthProb > 0 && upper != nil && upper.truthProb > 0 {
		nNew -= 20
	}
	if lower != nil {
		n--
	}
	if upper != nil {
		n--
	}
	if nNew < 10 {
		nNew = 10
	}
	if nNew < n {
		n = nNew
	}
	return logEst(n)
}

// addBtreeIndex ports whereLoopAddBtreeIndex (where.c) over the operators this
// subset admits, sqlite_stat1's arms included (IN statistics, skip-scan,
// "unordered"). Not reproduced, and unreachable under the gate:
//
//   - the STAT4 arms (whereEqualScanEst / whereInScanEst), not compiled into
//     the oracle. With them goes a vector range's nBtm/nTop
//     (whereRangeVectorLen, where.c:3430), which outside STAT4 reaches only the
//     seek key, never a cost (whereRangeScanEst, where.c:2103).
func (b *wherePlanIdxBuild) addBtreeIndex(iTab int, idx *whereIndexInfo, st whereIdxState, nInMul logEst) {
	it := &b.items[iTab]
	maskSelf := whereMask(1) << uint(iTab)
	opMask := woEq | woIn | woGT | woGE | woLT | woLE | woIsNull | woIs
	if st.btmLim {
		// "Do not allow the upper bound of a range constraint to mix with a
		// lower range bound from some other source."
		opMask = woLT | woLE
	}
	if idx.unordered {
		// sqlite_stat1's "unordered": the index is no use for a range.
		opMask &^= woGT | woGE | woLT | woLE
	}
	rSize := idx.aiRowLogEst[0]
	rLogSize := estLog(rSize)
	savedNEq := st.nEq
	savedNOut := st.nOut
	savedPrereq := st.prereq

	// nEq indexes aiColumn (0..nColumn-1) and, once incremented, aiRowLogEst
	// (0..nColumn). Both hold by construction -- aiRowLogEst is nColumn+1 long,
	// see whereDefaultRowEst -- but this project's hardest failure is a panic, so
	// the invariant is checked rather than assumed.
	if savedNEq >= len(idx.aiColumn) || savedNEq+1 >= len(idx.aiRowLogEst) {
		return
	}
	// eqOut is the ==/IN/IS NULL arm's nOut (where.c:3479-3528): an explicit
	// likelihood() on the term replaces the index's per-column estimate --
	// on a real column; a rowid or expression column keeps the estimate.
	eqOut := func(t *whereIdxTerm, nEq int, nIn, extra logEst) logEst {
		if t.truthProb <= 0 && idx.aiColumn[savedNEq] >= 0 {
			return savedNOut + t.truthProb - nIn
		}
		return savedNOut + (idx.aiRowLogEst[nEq] - idx.aiRowLogEst[nEq-1]) + extra
	}
	scan := b.whereScanInitIdx(iTab, savedNEq, opMask, idx)
	for ti := scan.next(); ti >= 0; ti = scan.next() {
		t := &b.terms[ti]
		if t.op&woIsNull != 0 && b.whereIndexColumnNotNull(iTab, idx, savedNEq) {
			continue // "ignore IS [NOT] NULL constraints on NOT NULL columns"
		}
		if t.prereqRight&maskSelf != 0 {
			continue
		}
		if t.likeOpt && t.op == woLT {
			// "Do not allow the upper bound of a LIKE optimization range
			// constraint to mix with a lower range bound from some other
			// source": it only ever comes in with its lower (below).
			continue
		}
		if it.outer && !constraintCompatibleWithOuterJoin(t, iTab) {
			continue
		}
		cur := st
		cur.lTerm = append(append([]int{}, st.lTerm...), ti)
		cur.nOut = savedNOut
		cur.prereq = (savedPrereq | t.prereqRight) &^ maskSelf
		// pBtm/pTop, the actual bound TERMS whereRangeScanEst prices.
		var pBtm, pTop *whereIdxTerm
		// nIn is log(K) for an "x IN (v1,...,vK)" term: the loop runs K times
		// over, once per value, so both its cost and its row count grow by it
		// and the RECURSION down the index carries it in nInMul.
		var nIn logEst

		switch {
		case t.op&woIn != 0:
			// whereLoopAddBtreeIndex's WO_IN arm: sqlite3LogEst(pList->nExpr),
			// or a flat 46 for "IN (SELECT ...)" (where.c:3337) -- which a
			// row-value IN's field is, its list being "IN (VALUES ...)".
			nIn = 46
			if t.inCount >= 0 {
				nIn = logEstFromInt(uint64(t.inCount))
			}
			if t.iField > 0 {
				// where.c:3340-3357: the fields of one row-value IN share its
				// pExpr, and nIn is charged only for the FIRST of them this
				// loop uses; a field an earlier index column already took
				// (tag-20250707-01) is dropped from the loop altogether.
				redundant := false
				for _, k := range st.lTerm {
					if k >= 0 && b.terms[k].iField > 0 && b.terms[k].parent == t.parent {
						nIn = 0
						if b.terms[k].iField == t.iField {
							redundant = true
						}
					}
				}
				if redundant {
					continue
				}
			}
			if idx.hasStat1 && rLogSize >= 10 {
				// With real statistics an IN can lose to scanning the M rows
				// the index's earlier columns select and testing each: it
				// keeps the index when M*log(K) >= K*log(N), with a margin of
				// 10 toward the index (where.c:3365-3409). Losing, it becomes
				// WHERE_IN_SEEKSCAN -- a code-generation choice, the same loop
				// to the planner -- unless an earlier IN already multiplies it,
				// in which case the loop is not built at all.
				m := idx.aiRowLogEst[savedNEq]
				if x := m + estLog(nIn) + 10 - (nIn + rLogSize); x < 0 && nInMul >= 2 {
					continue
				}
			}
			cur.nEq = savedNEq + 1
			cur.colIn = true
			cur.nOut = eqOut(t, cur.nEq, nIn, 0)
		case t.op&(woEq|woIs) != 0:
			cur.nEq = savedNEq + 1
			cur.colEq = true
			// WHERE_ONEROW, which wherePathSatisfiesOrderBy reads to skip the
			// whole index-column walk: a loop that returns at most one row is
			// trivially in every order. WHERE_UNQ_WANTED, its else-arm, is read
			// only by the one-pass machinery and is not carried.
			iCol := idx.aiColumn[savedNEq]
			if iCol == xnRowid || (iCol >= 0 && nInMul == 0 && savedNEq == idx.nKeyCol-1) {
				if iCol == xnRowid || idx.uniqNotNull ||
					(idx.nKeyCol == 1 && idx.onError && t.op&woEq != 0) {
					cur.oneRow = true
				}
			}
			cur.nOut = eqOut(t, cur.nEq, 0, 0)
		case t.op&woIsNull != 0:
			cur.nEq = savedNEq + 1
			cur.colNull = true
			// TUNING: "col IS NULL" is assumed to match twice as many rows as
			// "col = ?".
			cur.nOut = eqOut(t, cur.nEq, 0, 10)
		default: // WO_GT|WO_GE|WO_LT|WO_LE
			cur.isRange = true
			if t.op&(woGT|woGE) != 0 {
				cur.btmLim = true
				cur.topLim = st.topLim
				pBtm, pTop = t, nil
				if t.likeOpt && ti+1 < len(b.terms) && b.terms[ti+1].likeOpt {
					// "Range constraints that come from the LIKE optimization
					// are always used in pairs": the upper is pTerm[1].
					cur.lTerm = append(cur.lTerm, ti+1)
					cur.topLim = true
					pTop = &b.terms[ti+1]
				}
			} else {
				cur.topLim = true
				pTop = t
				if st.btmLim && len(cur.lTerm) >= 2 {
					// "pBtm = pNew->aLTerm[pNew->nLTerm-2]": the lower bound
					// survives from the enclosing call.
					pBtm = &b.terms[cur.lTerm[len(cur.lTerm)-2]]
				}
			}
			cur.nOut = whereRangeScanEst(savedNOut, pBtm, pTop)
		}

		// The cost of visiting the selected rows in the index: one search by
		// key, then nOut steps forward.
		var rCostIdx logEst
		if idx.ipk {
			// szIdxRow is low for an IPK table (small interior pages) but its
			// leaves are full-size, so a flat 16 is used instead of the ratio.
			rCostIdx = cur.nOut + 16
		} else {
			rCostIdx = cur.nOut + 1 + logEst((15*int(idx.szIdxRow))/int(it.szTabRow))
		}
		rCostIdx = logEstAdd(rLogSize, rCostIdx)
		rRun := rCostIdx
		if !cur.idxOnly && !idx.ipk {
			// Some data comes out of the main table, so add nOut row lookups.
			rRun = logEstAdd(rRun, cur.nOut+16)
		}
		nOutUnadjusted := cur.nOut
		rRun += nInMul + nIn
		cur.nOut += nInMul + nIn
		b.whereIdxOutputAdjust(&cur, cur.prereq, maskSelf, rSize)
		nOut := cur.nOut
		if b.items[iTab].fromExists {
			nOut = 0 // where.c:3580
		}
		b.insertLoop(whereIdxLoop{
			iTab: iTab, maskSelf: maskSelf, prereq: cur.prereq,
			rRun: rRun, nOut: nOut, idx: idx,
			nEq: cur.nEq, nSkip: cur.nSkip, lTerm: append([]int(nil), cur.lTerm...),
			oneRow: cur.oneRow, idxOnly: cur.idxOnly, indexed: !idx.ipk,
			colEq: cur.colEq, colNull: cur.colNull, colRange: cur.isRange,
			colIn: cur.colIn, sortIdx: cur.sortIdx,
		})

		if cur.isRange {
			cur.nOut = savedNOut
		} else {
			cur.nOut = nOutUnadjusted
		}
		// where.c:3589-3593. The PRIMARY KEY arm is what stops the walk at a
		// WITHOUT ROWID table's DECLARED key: aiColumn continues past nKeyCol
		// into every remaining table column (build.c:2485-2506), but those are
		// payload, not key, and aiRowLogEst says nothing about them.
		if !cur.topLim && cur.nEq < len(idx.aiColumn) &&
			(cur.nEq < idx.nKeyCol || !idx.primaryKey) {
			b.addBtreeIndex(iTab, idx, cur, nInMul+nIn)
		}
	}

	// SKIP-SCAN (where.c:3624-3649): no WHERE term for this column, but
	// sqlite_stat1 says each of its values repeats at least 18 times
	// (aiRowLogEst >= 42), so seeking past each distinct value and then using
	// the terms on the NEXT column beats a full scan. The column takes an aLTerm
	// slot with no term in it; the rest of the index is priced by the same
	// recursion, the distinct-value count multiplying every loop it builds.
	if savedNEq == st.nSkip && savedNEq+1 < idx.nKeyCol && savedNEq == len(st.lTerm) &&
		!idx.noSkipScan && idx.hasStat1 && savedNEq+1 < len(idx.aiRowLogEst) && !b.items[iTab].fromExists &&
		idx.aiRowLogEst[savedNEq+1] >= 42 {
		nIter := idx.aiRowLogEst[savedNEq] - idx.aiRowLogEst[savedNEq+1]
		sk := st
		sk.nEq, sk.nSkip = savedNEq+1, st.nSkip+1
		sk.lTerm = append(append([]int{}, st.lTerm...), -1)
		sk.nOut = savedNOut - nIter
		// TUNING: +5 (x1.375) makes a skip-scan slightly less likely, for the
		// uncertainty in its estimates.
		b.addBtreeIndex(iTab, idx, sk, nIter+5+nInMul)
	}
}

// addBtree ports whereLoopAddBtree (where.c) for a plain rowid base table: the
// transient automatic indexes first (so whereLoopFindLesser's SETUP-INVARIANT
// holds), then, for every index of the probe chain, the sPk full table scan or
// the "full scan via index" candidate, and whereLoopAddBtreeIndex's constrained
// loops.
func (b *wherePlanIdxBuild) addBtree(iTab int, mPrereq whereMask) {
	it := &b.items[iTab]
	maskSelf := whereMask(1) << uint(iTab)
	rSize := it.rowLogEst() // pTab->nRowLogEst

	// Automatic indexes. pOrSet IS reachable now (whereLoopAddOr prices each
	// disjunct through this same routine, and the C's very first guard is
	// "!pBuilder->pOrSet"); the rest -- WHERE_RIGHT_JOIN, a correlated or
	// recursive subquery, and the right table of a RIGHT JOIN -- are unreachable
	// under the gate, and WHERE_OR_SUBCLAUSE arrives as noAutoIndex from
	// wherePlanMultiOrOrder.
	if b.orSet == nil && !b.noAutoIndex && !it.indexedBy && !it.notIndexed {
		rLogSize := estLog(rSize)
		for i := range b.terms {
			t := &b.terms[i]
			if t.prereqRight&maskSelf != 0 {
				continue
			}
			if !termCanDriveIndex(t, *it, iTab, 0) {
				continue
			}
			// TUNING (where.c): the one-time cost of building the automatic
			// index is X*N*log2(N) with X == 7 (LogEst 28) for a real table;
			// each lookup is then assumed to yield 20 rows (LogEst 43), more
			// than the usual guess of 10 because how selective the index turns
			// out to be is unknown.
			b.insertLoop(whereIdxLoop{
				iTab: iTab, maskSelf: maskSelf,
				prereq:  mPrereq | t.prereqRight,
				rSetup:  rLogSize + rSize + 28,
				nOut:    43,
				rRun:    logEstAdd(rLogSize, 43),
				autoIdx: true,
				nEq:     1,
				lTerm:   []int{i},
			})
		}
	}

	// iSortIdx starts at 1, exactly as the C's local does, and advances once per
	// index of the probe chain -- so it is a POSITION, not an identity, and two
	// loops over different indexes can never share it.
	for i := range it.idxs {
		idx := &it.idxs[i]
		if idx.partial != nil {
			// "Partial index inappropriate for this query" (where.c:4124).
			// Skipped, iSortIdx still advancing: it counts the probe chain.
			switch b.whereUsablePartialIndex(iTab, idx.partial) {
			case triNo:
				continue
			case triUnknown:
				b.partialUnknown = true
				continue
			}
		}
		rSize := idx.aiRowLogEst[0]
		st := whereIdxState{nOut: rSize, prereq: mPrereq}
		// b = indexMightHelpWithOrderBy(pBuilder, pProbe, pSrc->iCursor). When
		// it answers 1 the loop records its iSortIdx, which splits
		// whereLoopFindLesser's candidate pools, and the "full scan via index"
		// candidate is built even for an index that covers nothing.
		mightSort := b.sort.indexMightHelpWithOrderBy(idx, iTab)
		if mightSort {
			st.sortIdx = i + 1
		}

		if idx.ipk {
			// TUNING: a full table scan costs 3.0*N, the extra 3.0 discouraging
			// full scans because an index lookup has the better worst case when
			// the size guesses are wrong. 16 == LogEst(3).
			scan := st
			b.whereIdxOutputAdjust(&scan, mPrereq, maskSelf, rSize)
			if it.fromExists {
				scan.nOut = 0 // where.c:4182
			}
			b.insertLoop(whereIdxLoop{
				iTab: iTab, maskSelf: maskSelf, prereq: mPrereq,
				rRun: rSize + 16, nOut: scan.nOut, idx: idx, sortIdx: st.sortIdx,
			})
		} else {
			m := it.colUsed & idx.colNotIdxed
			if idx.isCovering {
				m = 0
			} else if idx.partial != nil {
				m = wherePartIdxMask(idx.partial, it.cols, m) // where.c:4195
			}
			st.idxOnly = m == 0
			// "Full scan via index" (where.c:4231-4241): b
			// (indexMightHelpWithOrderBy), "!HasRowid(pTab)" -- a WITHOUT ROWID
			// table's every index loop is this arm, because it has no sPk arm to
			// fall into -- a partial index, the INDEXED BY hint, and the
			// covering-index scan, which additionally requires the index row to
			// be narrower than the table row (sqlite3GlobalConfig.bUseCis and
			// SQLITE_CoverIdxScan are both on by default).
			if mightSort || it.noRowid || idx.partial != nil || it.indexedBy || (m == 0 && !idx.unordered && idx.szIdxRow < it.szTabRow && !it.onePass) {
				scan := st
				scan.nOut = rSize
				// The cost of visiting the index rows is N*K, K between 1.1 and
				// 3.0 depending on the relative row sizes.
				rRun := rSize + 1 + logEst((15*int(idx.szIdxRow))/int(it.szTabRow))
				if m != 0 {
					// A non-covering index scan also pays for the table
					// lookups: 3x the number of them (rSize+16), less one for
					// every WHERE term the index alone can evaluate -- and 20
					// for an equality one. sqlite3ExprCoveredByIndex is over the
					// terms in order and STOPS at the first one it cannot
					// cover, which is why this is a prefix walk rather than a
					// filter. It walks pWC->nTerm, so the commuted virtual
					// terms count too.
					nLookup := rSize + 16
					mainTerms := b.terms
					if b.mainTerms != nil {
						// "WhereClause *pWC2 = &pWInfo->sWC" -- the statement's OWN
						// clause, which for a whereLoopAddOr sub-build is neither the
						// one-term disjunct clause nor its pOuter chain.
						mainTerms = b.mainTerms
					}
					for ti := range mainTerms {
						t := &mainTerms[ti]
						if !b.termCoveredByIndex(iTab, idx, t) {
							break
						}
						if t.truthProb <= 0 {
							nLookup += t.truthProb
							continue
						}
						nLookup--
						if t.op&(woEq|woIs) != 0 {
							nLookup -= 19
						}
					}
					rRun = logEstAdd(rRun, nLookup)
				}
				b.whereIdxOutputAdjust(&scan, mPrereq, maskSelf, rSize)
				if it.fromExists {
					scan.nOut = 0 // where.c:4286
				}
				b.insertLoop(whereIdxLoop{
					iTab: iTab, maskSelf: maskSelf, prereq: mPrereq,
					rRun: rRun, nOut: scan.nOut, idx: idx,
					idxOnly: st.idxOnly, indexed: true, sortIdx: st.sortIdx,
				})
			}
		}
		st.nOut = rSize
		b.addBtreeIndex(iTab, idx, st, 0)
	}
}

// addAll ports whereLoopAddAll (where.c): it walks the FROM clause left to
// right accumulating the prerequisite mask that stops items from being
// reordered across a CROSS or outer join, and builds every item's access
// methods under that mask.
//
// The accumulator is a SNAPSHOT, not a prefix cut, and that distinction is
// load-bearing. `mPrereq |= mPrior` runs only AT a barrier item; a plain item
// after one inherits whatever mPrereq held THEN -- the barrier's LEFT side
// only -- so it may be hoisted above the barrier's own right operand. For
// "FROM t1 CROSS JOIN t2, t3 WHERE t1.a=t2.c" SQLite runs t1, t3, t2.
func (b *wherePlanIdxBuild) addAll() {
	var mPrereq, mPrior whereMask
	hasRightCrossJoin := false
	b.planLimit = wherePlannerLimit
	for iTab := range b.items {
		b.planLimit += wherePlannerLimitIncr
		it := &b.items[iTab]
		maskSelf := whereMask(1) << uint(iTab)
		switch {
		case it.outer || it.cross:
			if it.cross {
				// JT_LTORJ is the other flag that sets this; RIGHT/FULL joins
				// are declined before this file runs.
				hasRightCrossJoin = true
			}
			mPrereq |= mPrior
		case it.fromExists:
			// where.c:4992: "joins that result from the EXISTS-to-JOIN
			// optimization should not be moved to the left of any of their
			// dependencies" -- every earlier item a term naming this one names.
			for ti := range b.terms {
				if pa := b.terms[ti].prereqAll; pa&maskSelf != 0 {
					mPrereq |= pa & (maskSelf - 1)
				}
			}
		case !hasRightCrossJoin:
			mPrereq = 0
		}
		b.addBtree(iTab, mPrereq)
		// "rc = whereLoopAddOr(pBuilder, mPrereq, mUnusable)" (where.c:5021), the
		// third and last access-method family, run for every item right after its
		// btree loops.
		b.addOr(iTab, mPrereq)
		mPrior |= maskSelf
	}
}

// termCoveredByIndex ports sqlite3ExprCoveredByIndex (expr.c): a term is covered
// when every column of THIS item it references is one the index holds. A column
// of another FROM item is irrelevant (the walk is over one cursor), and a
// reference to the rowid (or its INTEGER PRIMARY KEY alias, which lookupName
// rewrites to iColumn -1) is always covered, since every index of a rowid table
// carries XN_ROWID as its last column. A term whose column set this port could
// not enumerate answers false, which only ends the C's own prefix walk early.
func (b *wherePlanIdxBuild) termCoveredByIndex(iTab int, idx *whereIndexInfo, t *whereIdxTerm) bool {
	if iTab >= len(t.colMask) {
		return false
	}
	if t.colMask[iTab] == 0 {
		return true
	}
	if !t.colsKnown {
		return false
	}
	rest := t.colMask[iTab]
	for _, x := range idx.aiColumn {
		if x >= 0 && x < 63 {
			rest &^= uint64(1) << uint(x)
		}
	}
	return rest == 0
}

// whereIdxOperatorMask is operatorMask (whereexpr.c), optionally through
// exprCommute's operator swap (whereexpr.c: "<" becomes ">" and so on, while
// "=" and IS are their own mirror).
func whereIdxOperatorMask(op string, commuted bool) uint16 {
	o := strings.ToUpper(op)
	if commuted {
		switch o {
		case "<":
			o = ">"
		case "<=":
			o = ">="
		case ">":
			o = "<"
		case ">=":
			o = "<="
		}
	}
	switch o {
	case "=", "==":
		return woEq
	case "IS":
		return woIs
	case "<":
		return woLT
	case "<=":
		return woLE
	case ">":
		return woGT
	case ">=":
		return woGE
	}
	return 0
}

// ---------------------------------------------------------------------------
// ORDER BY / GROUP BY / DISTINCT.
//
// The second half of the port: wherePathSatisfiesOrderBy (where.c) and what
// wherePathSolver wraps around it -- whereSortingCost,
// whereInterstageHeuristic, the second solver pass, and the
// whereLoopInsert/whereLoopFindLesser/whereLoopAdjustCost pruning that matters
// once iSortIdx can differ between two loops on one table.
//
// An index that satisfies the ORDER BY, groups the GROUP BY or makes DISTINCT
// adjacent changes which b-tree is walked and, through revMask, in which
// direction; this engine reports both.
// ---------------------------------------------------------------------------

// xnNoColumn marks an ORDER BY / GROUP BY / DISTINCT term that is not a plain
// column reference -- "pOBExpr->op != TK_COLUMN", which
// wherePathSatisfiesOrderBy skips everywhere it looks at iColumn. It is
// deliberately distinct from xnRowid (-1), which IS a column reference.
const xnNoColumn = -2

// whereSortTerm is one entry of pWInfo->pOrderBy, reduced to the things
// wherePathSatisfiesOrderBy reads off it.
type whereSortTerm struct {
	// tab/col are the term's iTable/iColumn after
	// sqlite3ExprSkipCollateAndLikely: a FROM item and a column of it, with col
	// xnRowid for the rowid (or its INTEGER PRIMARY KEY alias, which lookupName
	// rewrites to -1), or col xnNoColumn when it is not a column reference at
	// all.
	tab int
	col int
	// coll is sqlite3ExprNNCollSeq of the UNSKIPPED term -- an explicit COLLATE
	// first, then the column's declared collation, then BINARY.
	coll string
	// desc/bigNull are ExprList_item.fg.sortFlags: KEYINFO_ORDER_DESC and
	// KEYINFO_ORDER_BIGNULL. sqlite3ExprListSetSortOrder (expr.c) sets BIGNULL
	// exactly when an explicit NULLS clause disagrees with the ASC/DESC
	// direction, which is this package's "NullsFirst() == Desc".
	desc    bool
	bigNull bool
	// uses is sqlite3WhereExprUsage -- the set of FROM items the term
	// references -- and isConst is sqlite3ExprIsConstant, the two the final
	// "mark off any other ORDER BY terms" block consults.
	uses    whereMask
	isConst bool
}

// whereSortCtl is pWInfo->pOrderBy plus the wctrlFlags bits that decide what
// wherePathSatisfiesOrderBy and whereSortingCost make of it. Which list ends up
// in ob, and which flags are set, is select.c's decision per statement shape --
// see wherePlanSortCtlFor (where_plan_gate.go), which reproduces it.
type whereSortCtl struct {
	ob []whereSortTerm

	groupBy     bool // WHERE_GROUPBY
	distinctBy  bool // WHERE_DISTINCTBY
	sortByGroup bool // WHERE_SORTBYGROUP
	// minMax is WHERE_ORDERBY_MIN|WHERE_ORDERBY_MAX: ob is minMaxQuery's
	// pMinMaxOrderBy rather than a written ORDER BY. It is carried because
	// wherePathSatisfiesOrderBy widens eqOpMask by WO_IN for it (where.c:5214).
	minMax bool

	wantDistinct bool // WHERE_WANT_DISTINCT
	useLimit     bool // WHERE_USE_LIMIT (== SF_FixedLimit)
	iLimit       logEst
	// nEList is pWInfo->pSelect->pEList->nExpr, the only thing whereSortingCost
	// reads out of the statement besides the LIMIT.
	nEList int

	// distinctCandidate marks the shape sqlite3WhereBegin turns into
	// WHERE_DISTINCTBY only after isDistinctRedundant has answered no: "DISTINCT
	// with no ORDER BY", whose pOrderBy is the RESULT SET. Resolved in
	// wherePlanIdxBuild.plan, the first place holding the index list
	// isDistinctRedundant needs.
	distinctCandidate bool
}

// nOrderBy is pOrderBy->nExpr, or 0 when the statement asks no ordering
// question -- in which case wherePathSolver never computes an isOrdered at all.
func (s *whereSortCtl) nOrderBy() int {
	if s == nil {
		return 0
	}
	return len(s.ob)
}

// indexMightHelpWithOrderBy ports the where.c routine of that name: could this
// index on item iTab possibly satisfy any term of pOrderBy? A true answer both
// admits the "full scan via index" candidate that the covering test would
// otherwise refuse and gives the loop a non-zero iSortIdx, which splits
// whereLoopFindLesser's candidate pools.
func (s *whereSortCtl) indexMightHelpWithOrderBy(idx *whereIndexInfo, iTab int) bool {
	if idx.unordered || s.nOrderBy() == 0 { // "if( pIndex->bUnordered ) return 0;"
		return false
	}
	for i := range s.ob {
		t := &s.ob[i]
		if t.col == xnNoColumn || t.tab != iTab {
			continue
		}
		if t.col < 0 {
			// "if( pExpr->iColumn<0 ) return 1": a rowid ORDER BY term can be
			// satisfied by ANY index of a rowid table, since every one of them
			// ends in XN_ROWID.
			return true
		}
		for j := 0; j < idx.nKeyCol && j < len(idx.aiColumn); j++ {
			if idx.aiColumn[j] == t.col {
				return true
			}
		}
	}
	return false
}

// whereIdxFindTerm ports sqlite3WhereFindTerm (where.c) for the one call
// wherePathSatisfiesOrderBy makes: pIdx == 0 (no affinity or collation filter)
// and notReady == ~ready, so the right-hand side must reference only loops
// already bound.
func (b *wherePlanIdxBuild) whereIdxFindTerm(iTab, col int, opMask uint16, notReady whereMask) *whereIdxTerm {
	var result *whereIdxTerm
	scan := b.whereScanInitCol(iTab, col, opMask)
	eqMask := opMask & (woEq | woIs)
	for i := scan.next(); i >= 0; i = scan.next() {
		t := &b.terms[i]
		if t.prereqRight&notReady != 0 {
			continue
		}
		if t.prereqRight == 0 && t.op&eqMask != 0 {
			return t
		}
		if result == nil {
			result = t
		}
	}
	return result
}

// wherePathSatisfiesOrderBy ports the where.c routine of that name. prefix is
// pPath->aLoop[0..nLoop-1] as indices into b.loops and last is pLast; the
// answer is SQLite's three-valued isOrdered (N>0 terms satisfied, 0 none, -1
// unknown yet) plus pRevMask, the nesting levels walked backwards.
//
// groupBy/distinctBy are WHERE_GROUPBY and WHERE_DISTINCTBY: terms may match in
// any order, and with GROUP BY alone direction is irrelevant too.
// orderByLimit is WHERE_ORDERBY_LIMIT (skips every loop but the innermost);
// minMax is WHERE_ORDERBY_MIN|MAX (shares only the eqOpMask widening).
//
// They are separate parameters because C's three call sites disagree: the main
// one passes pWInfo->wctrlFlags, the ORDER-BY-LIMIT retry (where.c:6219) a bare
// WHERE_ORDERBY_LIMIT, and the WHERE_SORTBYGROUP re-ask 0.
func (b *wherePlanIdxBuild) wherePathSatisfiesOrderBy(
	ob []whereSortTerm, prefix []int, last int, groupBy, distinctBy, orderByLimit, minMax bool,
) (int8, whereMask) {
	nOrderBy := len(ob)
	if nOrderBy == 0 || nOrderBy > 62 {
		// "if( nOrderBy>BMS-1 ) return 0": too many terms to track in a mask.
		return 0, 0
	}
	nLoop := len(prefix)
	isOrderDistinct := true
	obDone := uint64(1)<<uint(nOrderBy) - 1
	var obSat uint64
	var orderDistinctMask, ready, revMask whereMask
	eqOpMask := woEq | woIs | woIsNull
	if orderByLimit || minMax {
		// WHERE_ORDERBY_LIMIT/_MIN/_MAX also admit WO_IN (where.c:5214).
		//
		// The MIN/MAX half is load-bearing, not decorative: minMaxQuery hands
		// the planner "ORDER BY <arg> DESC" for a lone max(), and with WO_IN
		// outside the mask an IN-constrained leading index column would fail to
		// mark that term off, match it as an ORDERED column instead and set
		// revMask -- reversing the scan. "SELECT a,b,max(a) FROM t WHERE a IN
		// (2)" over "CREATE INDEX i1 ON t(a,b,c)" then read its bare `b` off
		// the LAST tied row ('z') where the oracle reads the first ('x').
		eqOpMask |= woIn
	}

	var l *whereIdxLoop
	for iLoop := 0; isOrderDistinct && obSat < obDone && iLoop <= nLoop; iLoop++ {
		if iLoop > 0 {
			ready |= l.maskSelf
		}
		if iLoop < nLoop {
			l = &b.loops[prefix[iLoop]]
			if orderByLimit {
				continue
			}
		} else {
			l = &b.loops[last]
		}
		iTab := l.iTab

		// Mark off any ORDER BY term X that is a column of this loop's table for
		// which the WHERE clause holds "X IS NULL" or "X = <expr over outer
		// loops only>": every row this loop produces then has the same X, so X
		// is trivially in order.
		for i := range ob {
			if obSat&(uint64(1)<<uint(i)) != 0 {
				continue
			}
			t := &ob[i]
			if t.col == xnNoColumn || t.tab != iTab {
				continue
			}
			term := b.whereIdxFindTerm(iTab, t.col, eqOpMask, ^ready)
			if term == nil {
				continue
			}
			if term.op == woIn {
				// "IN terms are only valid for sorting in the ORDER BY LIMIT
				// optimization, and then only if they are actually used by the
				// query plan" -- so the term found has to be one of THIS loop's
				// own aLTerm entries. (eqOpMask only carries woIn under
				// orderByLimit, which is the C's own assert.)
				used := false
				for _, li := range l.lTerm {
					if li >= 0 && &b.terms[li] == term {
						used = true
						break
					}
				}
				if !used {
					continue
				}
			}
			if term.op&(woEq|woIs) != 0 && t.col >= 0 {
				// The ORDER BY term's own collating sequence must equal the
				// comparison's, or the equality does not actually pin the
				// ordering. (Only for a real column: for the rowid the C skips
				// this test, and sqlite3ExprCompareCollSeq answers 0 there.)
				if !equalFoldName(t.coll, term.coll) {
					continue
				}
			}
			obSat |= uint64(1) << uint(i)
		}

		if !l.oneRow {
			var pIndex *whereIndexInfo
			nKeyCol, nColumn := 0, 1
			if l.idx == nil {
				// A transient automatic index. The C never reaches here with
				// one: whereLoopAddBtree gives it no pIndex, so pLoop->u.btree
				// .pIndex is 0 and the "(pIndex = ...)==0" arm returns 0 --
				// "this path satisfies no ORDER BY term".
				return 0, revMask
			}
			if !l.idx.ipk {
				if l.idx.unordered {
					return 0, revMask // "|| pIndex->bUnordered ){ return 0;"
				}
				pIndex = l.idx
				nKeyCol = pIndex.nKeyCol
				nColumn = len(pIndex.aiColumn)
				// "All relevant terms of the index must also be non-NULL in
				// order for isOrderDistinct to be true. So the value computed
				// here might be a false positive. Corrections are made at
				// tag-20210426-1."
				isOrderDistinct = pIndex.onError && l.nSkip == 0
			}

			rev, revSet := false, false
			distinctColumns := false
			for j := 0; j < nColumn; j++ {
				bOnce := true
				// A skip-scan's leading nSkip columns hold no term (aLTerm[j]==0)
				// and VARY across the scan, so they are ordering columns.
				if j < l.nEq && j >= l.nSkip && j < len(l.lTerm) {
					eOp := b.terms[l.lTerm[j]].op
					if eOp&eqOpMask != 0 {
						// Skip over == / IS / IS NULL terms. IS and ISNULL imply
						// the index is not UNIQUE NOT NULL, in which case the
						// loop can hold repeated NULL rows and is not
						// order-distinct.
						if eOp&(woIsNull|woIs) != 0 {
							isOrderDistinct = false
						}
						continue
					}
					// The C's remaining "else if( ALWAYS(eOp & WO_IN) )" arm does not
					// continue: an IN column is an ordering column. codeINTerm
					// (wherecode.c:684) walks the RHS's sorted ephemeral b-tree in the
					// index's direction, so the per-value seeks concatenate to the
					// restricted index scan. Its only body is the vector-IN test: a
					// later column of this loop from the same row-value IN makes this
					// column match no ORDER BY term (where.c:5339-5348).
					if t := &b.terms[l.lTerm[j]]; t.iField > 0 {
						for i := j + 1; i < l.nEq && i < len(l.lTerm); i++ {
							if k := l.lTerm[i]; k >= 0 && b.terms[k].iField > 0 && b.terms[k].parent == t.parent {
								bOnce = false
								break
							}
						}
					}
				}

				iColumn := xnRowid
				revIdx := false
				if pIndex != nil {
					iColumn = pIndex.aiColumn[j]
					revIdx = pIndex.aSortOrder[j]
					if iColumn == b.items[iTab].ipkIndex {
						iColumn = xnRowid
					}
				}

				// tag-20210426-1: an unconstrained column that might be NULL
				// means this loop is not well-ordered. (XN_EXPR is unreachable
				// -- the gate declines an expression index.)
				if isOrderDistinct && iColumn >= 0 && j >= l.nEq &&
					!b.items[iTab].cols[iColumn].NotNull {
					isOrderDistinct = false
				}

				// Find the ORDER BY term the j-th index column corresponds to.
				isMatch, iMatch := false, 0
				for i := 0; bOnce && i < nOrderBy; i++ {
					if obSat&(uint64(1)<<uint(i)) != 0 {
						continue
					}
					t := &ob[i]
					if !groupBy && !distinctBy {
						// A strict ORDER BY must be matched left to right, so
						// only the FIRST unsatisfied term is ever a candidate.
						bOnce = false
					}
					if t.col == xnNoColumn || t.tab != iTab || t.col != iColumn {
						continue
					}
					if iColumn != xnRowid && !equalFoldName(t.coll, pIndex.azColl[j]) {
						continue
					}
					isMatch, iMatch = true, i
					break
				}

				if isMatch && !groupBy {
					// The sort DIRECTION must be compatible. Irrelevant for
					// GROUP BY, which only needs equal rows adjacent.
					if revSet {
						if (rev != revIdx) != ob[iMatch].desc {
							isMatch = false
						}
					} else {
						rev = revIdx != ob[iMatch].desc
						if rev {
							revMask |= whereMask(1) << uint(iLoop)
						}
						revSet = true
					}
				}
				if isMatch && ob[iMatch].bigNull && j != l.nEq {
					// WHERE_BIGNULL_SORT is only available on the first
					// unconstrained index column.
					isMatch = false
				}
				if isMatch {
					if iColumn == xnRowid {
						distinctColumns = true
					}
					obSat |= uint64(1) << uint(iMatch)
				} else {
					if j == 0 || j < nKeyCol {
						isOrderDistinct = false
					}
					break
				}
			}
			if distinctColumns {
				// The rowid is UNIQUE and NOT NULL, so a loop whose ORDER BY
				// match reached it is order-distinct whatever the columns before
				// it were.
				isOrderDistinct = true
			}
		}

		// Mark off any other ORDER BY term that references only well-ordered
		// loops.
		if isOrderDistinct {
			orderDistinctMask |= l.maskSelf
			for i := range ob {
				if obSat&(uint64(1)<<uint(i)) != 0 {
					continue
				}
				if ob[i].uses == 0 && !ob[i].isConst {
					continue
				}
				if ob[i].uses&^orderDistinctMask == 0 {
					obSat |= uint64(1) << uint(i)
				}
			}
		}
	}

	if obSat == obDone {
		return int8(nOrderBy), revMask
	}
	if !isOrderDistinct {
		for i := nOrderBy - 1; i > 0; i-- {
			m := uint64(1)<<uint(i) - 1
			if obSat&m == m {
				return int8(i), revMask
			}
		}
		return 0, revMask
	}
	return -1, revMask
}

// whereSortingCost ports the where.c routine of that name: the cost of sorting
// nRow rows on nOrderBy columns of which the first nSorted are already in order.
func whereSortingCost(s *whereSortCtl, nRow logEst, nOrderBy, nSorted int) logEst {
	// TUNING: sorting cost proportional to the number of output columns.
	nCol := logEstFromInt(uint64((s.nEList + 59) / 30))
	rSortCost := nRow + nCol
	if nSorted > 0 {
		// Scale the result by (Y/X): only the last (nOrderBy-nSorted) terms are
		// out of order, so a block sort does that much less work.
		rSortCost += logEstFromInt(uint64((nOrderBy-nSorted)*100/nOrderBy)) - 66
	}
	switch {
	case s.useLimit:
		rSortCost += 10 // TUNING: extra 2.0x if using LIMIT
		if nSorted != 0 {
			rSortCost += 6 // TUNING: extra 1.5x if also using a partial sort
		}
		if s.iLimit < nRow {
			nRow = s.iLimit
		}
	case s.wantDistinct:
		// TUNING: assume DISTINCT halves the number of output rows.
		if nRow > 10 {
			nRow -= 10
		}
	}
	return rSortCost + estLog(nRow)
}

// whereLoopIsNoBetter ports the where.c routine of that name: two loops of equal
// cost, is the candidate even slightly better? True means "cannot tell", which
// keeps the baseline.
func whereLoopIsNoBetter(candidate, baseline *whereIdxLoop) bool {
	if !candidate.indexed || !baseline.indexed {
		return true
	}
	return candidate.idx.szIdxRow >= baseline.idx.szIdxRow
}

// whereLoopCheaperProperSubset ports the where.c routine of that name.
func whereLoopCheaperProperSubset(x, y *whereIdxLoop) bool {
	if x.rRun > y.rRun && x.nOut > y.nOut {
		return false // (1d) and (2a)
	}
	if x.nEq < y.nEq && x.idx == y.idx && x.nSkip == 0 && y.nSkip == 0 { // (1a),(1b),(1c)
		return true
	}
	if len(x.lTerm)-x.nSkip >= len(y.lTerm)-y.nSkip {
		return false // (2b)
	}
	if y.nSkip > x.nSkip {
		return false // (2d)
	}
	for _, xi := range x.lTerm {
		if xi < 0 {
			continue // "if( pX->aLTerm[i]==0 ) continue;"
		}
		found := false
		for _, yi := range y.lTerm {
			if yi == xi {
				found = true
				break
			}
		}
		if !found {
			return false // (2c)
		}
	}
	if x.idxOnly && !y.idxOnly {
		return false // (2e)
	}
	return true
}

// whereLoopAdjustCost ports the where.c routine of that name: nudge a template's
// cost so it is cheaper than every proper subset of itself and costlier than
// every loop it is a proper subset of. Only real-index templates take part --
// the C returns immediately for anything without WHERE_INDEXED, which is every
// sPk and every automatic-index loop.
func (b *wherePlanIdxBuild) whereLoopAdjustCost(t *whereIdxLoop) {
	if !t.indexed {
		return
	}
	for i := range b.loops {
		p := &b.loops[i]
		if p.iTab != t.iTab || !p.indexed {
			continue
		}
		switch {
		case whereLoopCheaperProperSubset(p, t):
			if p.rRun < t.rRun {
				t.rRun = p.rRun
			}
			if p.nOut-1 < t.nOut {
				t.nOut = p.nOut - 1
			}
		case whereLoopCheaperProperSubset(t, p):
			if p.rRun > t.rRun {
				t.rRun = p.rRun
			}
			if p.nOut+1 > t.nOut {
				t.nOut = p.nOut + 1
			}
		}
	}
}

// whereLoopFindLesser ports the where.c routine of that name, scanning from
// position `from`. It answers -1 to DISCARD the template, an index to OVERWRITE,
// or len(b.loops) to APPEND. The pool key is (iTab, iSortIdx): two loops that
// differ in either are never candidates to replace one another.
func (b *wherePlanIdxBuild) whereLoopFindLesser(from int, t *whereIdxLoop) int {
	for i := from; i < len(b.loops); i++ {
		p := &b.loops[i]
		if p.iTab != t.iTab || p.sortIdx != t.sortIdx {
			continue
		}
		// "Any loop using an application-defined index (or PRIMARY KEY or
		// UNIQUE constraint) with one or more == constraints is better than an
		// automatic index." Unless it is a skip-scan.
		if p.autoIdx && t.nSkip == 0 && t.indexed && t.colEq && p.prereq&t.prereq == t.prereq {
			return i
		}
		if p.prereq&t.prereq == p.prereq &&
			p.rSetup <= t.rSetup && p.rRun <= t.rRun && p.nOut <= t.nOut {
			return -1 // an existing loop is better; discard the template
		}
		if p.prereq&t.prereq == t.prereq &&
			p.rRun >= t.rRun && p.nOut >= t.nOut {
			return i // the template is better; overwrite p
		}
	}
	return len(b.loops)
}

// insertLoop ports whereLoopInsert (where.c:2832): the plan limit, the cost
// adjustment, the pOrSet costs-only path, and the find-lesser replace/delete
// dance.
func (b *wherePlanIdxBuild) insertLoop(t whereIdxLoop) {
	// "Stop the search once we hit the query planner search limit." The C
	// abbreviates the search from here on; this port declines instead -- see
	// wherePlannerLimit.
	if b.planLimit <= 0 {
		b.planLimitHit = true
		if b.orSet != nil {
			b.orSet.n = 0
		}
		return
	}
	b.planLimit--
	b.whereLoopAdjustCost(&t)
	if b.orSet != nil {
		// "If pBuilder->pOrSet is defined, then only keep track of the costs and
		// prereqs" -- and only for a CONSTRAINED loop: "if( pTemplate->nLTerm )".
		// A full scan has no aLTerm, so a disjunct no index can serve records
		// nothing, which empties sSum and cancels the whole WHERE_MULTI_OR
		// candidate. That is exactly why "a=1 OR b='x'" over an index on a alone
		// comes back as a plain SCAN.
		if len(t.lTerm) > 0 {
			b.orSet.insert(t.prereq, t.rRun, t.nOut)
		}
		return
	}
	i := b.whereLoopFindLesser(0, &t)
	if i < 0 {
		return
	}
	if i >= len(b.loops) {
		b.loops = append(b.loops, t)
		return
	}
	// Overwriting b.loops[i]: first delete every LATER entry this template also
	// supplants, exactly as the C's ppTail walk does.
	tail := i + 1
	for tail < len(b.loops) {
		j := b.whereLoopFindLesser(tail, &t)
		if j < 0 || j >= len(b.loops) {
			break
		}
		b.loops = append(b.loops[:j], b.loops[j+1:]...)
		tail = j
	}
	b.loops[i] = t
}

// wherePath is wherePathSolver's WherePath: a partial nesting of loops.
type wherePath struct {
	maskLoop  whereMask
	revLoop   whereMask
	nRow      logEst
	rCost     logEst
	rUnsort   logEst
	isOrdered int8
	loops     []int
}

// wherePlanResult is what the solver finally decides: which loop runs at each
// nesting level (indices into b.loops), and whether that level is walked
// BACKWARDS (pWInfo->revMask).
type wherePlanResult struct {
	loops []int
	rev   []bool
	// isOrdered is pWInfo->nOBSat, the three-valued answer wherePathSolver
	// settled on for the winning path. wherePlanSingleIndexKey turns it into
	// autoIndexKey.groupsSorted.
	isOrdered int8
	// nRow is pWInfo->nRowOut as sqlite3WhereBegin leaves it once planning is
	// done: the winning path's own row-count accumulator (wherePath.nRow),
	// reduced by the DISTINCT-without-ORDER-BY heuristic where.c applies on
	// top of it. See reportedRowEstimate for the exact rule and its C
	// citations. No current caller reads this -- it exists so a later
	// nQueryLoop consumer has a correct number, without this pass wiring one
	// up (a separate, already-scoped follow-up). Adding it changes no
	// existing caller's behavior: every current reader of *wherePlanResult
	// ignores the field.
	nRow logEst
}

// computeMxChoice ports the where.c routine of that name: how many partial
// paths wherePathSolver keeps per generation, plus the star-query cost rewrite
// that decides it.
//
// With three or fewer FROM items it is the constant 12 and the detection cannot
// fire (constraint (aa), nLoop >= 4). With four it can: mxChoice becomes 18 and
// every unconstrained scan of a dimension table has its rRun raised past the
// fact table's most expensive access method -- a cost rewrite that changes the
// winner.
//
// A fact table is one three or more other loops depend on (constraint (cc)),
// searching only items not separated by an outer or cross join ((bb)).
// Reproduced verbatim, including two details that look like slips: the
// self-join guard records the candidate fact table's bit, and pStart only moves
// forward.
//
// bStarDone runs detection once per statement; the mxChoice and the in-place
// rRun rewrite are seen by both of plan()'s solver passes.
func (b *wherePlanIdxBuild) computeMxChoice() int {
	nLoop := len(b.items)
	if nLoop >= 4 && !b.starDone {
		b.starDone = true
		var mSelfJoin whereMask // tables that cannot be dimension tables
		pStart := 0             // where to start searching for dimension tables
		for iFromIdx := 0; iFromIdx < nLoop; iFromIdx++ {
			m := whereMask(1) << uint(iFromIdx)
			nDep := 0
			var mSeen whereMask
			fact := &b.items[iFromIdx]
			if fact.outer || fact.cross {
				// The candidate is the right table of an outer or cross join, so
				// only tables to its RIGHT may be dimensions -- constraint (bb).
				if iFromIdx+3 > nLoop {
					break // nDep > 2 is now unreachable -- constraint (cc)
				}
				for pStart < len(b.loops) && b.loops[pStart].iTab <= iFromIdx {
					pStart++
				}
			}
			for li := pStart; li < len(b.loops); li++ {
				w := &b.loops[li]
				if b.items[w.iTab].outer || b.items[w.iTab].cross {
					break // constraint (bb)
				}
				if w.prereq&m != 0 && w.maskSelf&mSeen == 0 && w.maskSelf&mSelfJoin == 0 {
					// "aFromTabs[pWLoop->iTab].pSTab==pFactTab->pSTab": the SAME
					// Table object, i.e. a self-join. Every item here is a plain
					// local base table in the main schema (the gate declines the
					// rest), so equal folded names is exactly that identity.
					if b.items[w.iTab].tabName == fact.tabName {
						mSelfJoin |= m
					} else {
						nDep++
						mSeen |= w.maskSelf
					}
				}
			}
			if nDep <= 2 {
				continue // constraint (cc)
			}
			b.starUsed = true
			// The maximum cost of any WhereLoop for the fact table, plus one
			// epsilon. LOGEST_MIN is -32768 (sqliteInt.h:905); the C's
			// ALWAYS(mxRun<LOGEST_MAX) guard on the increment never fires.
			mxRun := logEst(-32768)
			for li := pStart; li < len(b.loops); li++ {
				w := &b.loops[li]
				if w.iTab < iFromIdx {
					continue
				}
				if w.iTab > iFromIdx {
					break
				}
				if w.rRun > mxRun {
					mxRun = w.rRun
				}
			}
			mxRun++
			// Raise the cost of a dimension table's FULL SCAN (nLTerm == 0, i.e.
			// no term drives it) to slightly more than the fact table's.
			for li := pStart; li < len(b.loops); li++ {
				w := &b.loops[li]
				if w.maskSelf&mSeen == 0 || len(w.lTerm) != 0 {
					continue
				}
				if w.rRun < mxRun {
					w.rRun = mxRun
				}
			}
		}
	}
	if b.starUsed {
		return 18
	}
	return 12
}

// solve ports wherePathSolver (where.c): a dynamic program over generations of
// partial paths, keeping the mxChoice cheapest at each level, with the sort cost
// folded in whenever nRowEst is non-zero.
func (b *wherePlanIdxBuild) solve(nRowEst logEst) (*wherePath, bool) {
	nLoop := len(b.items)
	// TUNING: mxChoice by FROM-clause size -- 1, 5, then computeMxChoice's 12
	// or 18. (The C's third arm, "else if( pParse->nErr ) mxChoice = 1", is a
	// compile that has already failed.)
	mxChoice := 1
	if nLoop == 2 {
		mxChoice = 5
	} else if nLoop >= 3 {
		mxChoice = b.computeMxChoice()
	}
	// "If nRowEst is zero and there is an ORDER BY clause, ignore it."
	nOrderBy := 0
	if b.sort.nOrderBy() > 0 && nRowEst != 0 {
		nOrderBy = b.sort.nOrderBy()
	}
	aSortCost := make([]logEst, nOrderBy)
	aSortSet := make([]bool, nOrderBy)

	// TUNING (where.c): the seed path's row count is MIN(nQueryLoop, 48)
	// (where.c:5921), read UNCONDITIONALLY on entry -- independent of this
	// function's own nRowEst parameter, which is a different value (see the
	// "ignore ORDER BY" comment above). nQueryLoop is 0 for a true top-level
	// statement (the only kind this package accepted before this field
	// existed) and, now, for a nested compile too -- b.nQueryLoop is nonzero
	// only when the caller proved it trustworthy (compiler.nQueryLoopKnown;
	// see wherePlanSingleTableIndexOrder/wherePlanMultiTableOrder).
	seedNRow := b.nQueryLoop
	if seedNRow > 48 {
		seedNRow = 48 // assert( 48==sqlite3LogEst(28) ), where.c:5921.
	}
	seed := wherePath{nRow: seedNRow}
	if nOrderBy > 0 {
		seed.isOrdered = -1 // nLoop > 0 always
	}
	aFrom := []wherePath{seed}

	var mxCost, mxUnsort logEst
	mxI := 0

	for iLoop := 0; iLoop < nLoop; iLoop++ {
		var aTo []wherePath
		for i := range aFrom {
			pFrom := &aFrom[i]
			for li := range b.loops {
				w := &b.loops[li]
				if w.disabled {
					continue // whereInterstageHeuristic's "prereq = ALLBITS"
				}
				if w.prereq&^pFrom.maskLoop != 0 {
					continue
				}
				if w.maskSelf&pFrom.maskLoop != 0 {
					continue
				}
				if w.autoIdx && pFrom.nRow < 3 {
					// Do not build an automatic index for a loop expected to
					// run less than 1.25 times.
					continue
				}

				rUnsort := w.rRun + pFrom.nRow
				if w.rSetup != 0 {
					rUnsort = logEstAdd(w.rSetup, rUnsort)
				}
				rUnsort = logEstAdd(rUnsort, pFrom.rUnsort)
				nOut := pFrom.nRow + w.nOut
				maskNew := pFrom.maskLoop | w.maskSelf
				isOrdered := pFrom.isOrdered
				var revMask whereMask
				if isOrdered < 0 {
					isOrdered, revMask = b.wherePathSatisfiesOrderBy(
						b.sort.ob, pFrom.loops, li,
						b.sort.groupBy, b.sort.distinctBy, false, b.sort.minMax)
				} else {
					revMask = pFrom.revLoop
				}
				var rCost logEst
				if isOrdered >= 0 && int(isOrdered) < nOrderBy {
					if !aSortSet[isOrdered] {
						aSortCost[isOrdered] = whereSortingCost(b.sort, nRowEst, nOrderBy, int(isOrdered))
						aSortSet[isOrdered] = true
					}
					// TUNING: a small extra penalty (3) on sorting, to encourage
					// a plan whose rows emerge in the right order with no sort.
					rCost = logEstAdd(rUnsort, aSortCost[isOrdered]) + 3
				} else {
					rCost = rUnsort
					rUnsort -= 2 // TUNING: a slight bias in favour of no-sort plans
				}

				jj := 0
				for ; jj < len(aTo); jj++ {
					// "Compatible isOrdered value": both -1, both >=0, or the
					// last round of the solver, where ordering no longer matters.
					if aTo[jj].maskLoop == maskNew &&
						((aTo[jj].isOrdered^isOrdered)&int8(-128) == 0 || iLoop == nLoop-1) {
						break
					}
				}
				if jj >= len(aTo) {
					if len(aTo) >= mxChoice &&
						(rCost > mxCost || (rCost == mxCost && rUnsort >= mxUnsort)) {
						continue // no better than any of the mxChoice best so far
					}
					if len(aTo) < mxChoice {
						jj = len(aTo)
						aTo = append(aTo, wherePath{})
					} else {
						jj = mxI // replace the prior worst
					}
				} else {
					// An existing best-so-far path covers the same loops and has
					// a compatible isOrdered. This is an expanded vector
					// comparison equivalent to
					// (pTo->rCost,pTo->nRow,pTo->rUnsort) <= (rCost,nOut,rUnsort),
					// with whereLoopIsNoBetter breaking a full tie -- so an
					// exact tie it cannot separate keeps the INCUMBENT, which is
					// what makes FROM order the last word.
					pTo := &aTo[jj]
					if pTo.rCost < rCost ||
						(pTo.rCost == rCost && pTo.nRow < nOut) ||
						(pTo.rCost == rCost && pTo.nRow == nOut && pTo.rUnsort < rUnsort) ||
						(pTo.rCost == rCost && pTo.nRow == nOut && pTo.rUnsort == rUnsort &&
							whereLoopIsNoBetter(w, &b.loops[pTo.loops[iLoop]])) {
						continue
					}
				}

				next := make([]int, iLoop+1)
				copy(next, pFrom.loops)
				next[iLoop] = li
				aTo[jj] = wherePath{
					maskLoop: maskNew, revLoop: revMask, nRow: nOut, rCost: rCost,
					rUnsort: rUnsort, isOrdered: isOrdered, loops: next,
				}
				if len(aTo) >= mxChoice {
					mxI = 0
					mxCost = aTo[0].rCost
					// NOTE: the C tracks the worst path by (rCost, nRow) here
					// even though the field is named mxUnsort and the admission
					// test above compares it against rUnsort. That is
					// reproduced, not corrected.
					mxUnsort = aTo[0].nRow
					for k := 1; k < len(aTo); k++ {
						if aTo[k].rCost > mxCost ||
							(aTo[k].rCost == mxCost && aTo[k].rUnsort > mxUnsort) {
							mxCost = aTo[k].rCost
							mxUnsort = aTo[k].rUnsort
							mxI = k
						}
					}
				}
			}
		}
		aFrom = aTo
		if len(aFrom) == 0 {
			return nil, false // "no query solution"
		}
	}
	return &aFrom[0], true
}

// whereInterstageHeuristic ports the where.c routine of that name: if the first
// solver pass chose an index-constrained loop for an OUTER level, disable every
// UNCONSTRAINED loop on that table so the second, ORDER-BY-aware pass cannot
// trade the index search for a full scan just to avoid a sort. The walk STOPS at
// the first level whose loop is not index-constrained, which is what still
// allows the rewrite "SCAN t1 / SEARCH t2" -> "SCAN t2 / SEARCH t1".
func (b *wherePlanIdxBuild) whereInterstageHeuristic(path *wherePath) {
	for _, li := range path.loops {
		p := &b.loops[li]
		if !p.colEq && !p.colNull && !p.colIn {
			return
		}
		iTab := p.iTab
		for i := range b.loops {
			l := &b.loops[i]
			if l.iTab != iTab {
				continue
			}
			if l.colEq || l.colNull || l.colRange || l.colIn || l.autoIdx {
				continue // WHERE_CONSTRAINT|WHERE_AUTO_INDEX loops may remain
			}
			l.disabled = true
		}
	}
}

// plan runs the whole of sqlite3WhereBegin's planning half: build every access
// method (whereLoopAddAll), solve once ignoring sort costs, then -- if the
// statement asks an ordering question at all -- apply the interstage heuristic
// and solve again with the row estimate the first pass produced. The tail
// reproduces wherePathSolver's own revMask assignment, which is the ONLY thing
// downstream of the solver that this engine can observe.
func (b *wherePlanIdxBuild) plan() (*wherePlanResult, bool) {
	if b.sort != nil && b.sort.distinctCandidate {
		// sqlite3WhereBegin's own order: isDistinctRedundant first, and only if
		// it says no does "DISTINCT with no ORDER BY" become WHERE_DISTINCTBY
		// over the result set. isDistinctRedundant answers 0 outright for a FROM
		// clause of more than one item.
		if len(b.items) == 1 && b.whereIdxIsDistinctRedundant(b.items[0].idxs, b.sort.ob) {
			b.sort.ob = nil
		} else {
			b.sort.distinctBy = true
		}
	}
	// "if( nTabList!=1 || whereShortCut(&sWLB)==0 )" (where.c): a one-table
	// equality on the rowid or on every column of a UNIQUE index is planned
	// before, and instead of, every other loop.
	if res, ok := b.shortCut(); ok {
		return res, true
	}
	if b.cache != nil && b.cache.populated {
		// Copies: the solver disables loops (whereInterstageHeuristic) and
		// raises star-query costs (computeMxChoice) in its own.
		b.loops, b.planLimitHit, b.partialUnknown = slices.Clone(b.cache.loops), b.cache.limitHit, b.cache.partialUnknown
	} else {
		b.addAll()
		if b.cache != nil {
			*b.cache = whereLoopCache{loops: slices.Clone(b.loops), limitHit: b.planLimitHit,
				partialUnknown: b.partialUnknown, populated: true}
		}
	}
	if len(b.loops) == 0 || b.planLimitHit || b.partialUnknown {
		return nil, false
	}
	best, ok := b.solve(0)
	if !ok {
		return nil, false
	}
	if b.sort.nOrderBy() > 0 {
		// "if( pWInfo->pOrderBy ){ whereInterstageHeuristic(pWInfo);
		//   wherePathSolver(pWInfo, pWInfo->nRowOut<0 ? 1 : pWInfo->nRowOut+1); }"
		// -- a second pass whose only difference is that nRowEst is no longer 0,
		// so aSortCost[] stops being free.
		nRowEst := best.nRow + 1
		if best.nRow < 0 {
			nRowEst = 1
		}
		b.whereInterstageHeuristic(best)
		if b2, ok2 := b.solve(nRowEst); ok2 {
			best = b2
		}
	}

	nLoop := len(best.loops)
	revMask := whereMask(0)
	if b.sort.nOrderBy() > 0 && !b.sort.distinctBy {
		// pWInfo->revMask = pFrom->revLoop, but ONLY outside the
		// WHERE_DISTINCTBY arm -- a DISTINCT-with-no-ORDER-BY plan always scans
		// forward however its terms matched.
		revMask = best.revLoop
		if best.isOrdered <= 0 && nLoop > 0 {
			// The ORDER BY LIMIT optimization: the path satisfies no ORDER BY
			// term, but the INNERMOST loop alone might satisfy all of them, in
			// which case that one loop is walked in the ORDER BY's direction.
			last := &b.loops[best.loops[nLoop-1]]
			// "(wsFlags&(WHERE_IPK|WHERE_COLUMN_IN))!=(WHERE_IPK|WHERE_COLUMN_IN)":
			// a "rowid IN (...)" loop is excluded from the ORDER BY LIMIT
			// optimization outright. WHERE_IPK is what the fake sPk index sets.
			if !last.oneRow && !(last.idx != nil && last.idx.ipk && last.colIn) {
				n, m := b.wherePathSatisfiesOrderBy(
					b.sort.ob, best.loops[:nLoop-1], best.loops[nLoop-1], false, false, true, false)
				if int(n) == b.sort.nOrderBy() {
					revMask = m
				}
			}
		}
	}
	if b.sort.nOrderBy() > 0 && b.sort.sortByGroup &&
		int(best.isOrdered) == b.sort.nOrderBy() && nLoop > 0 {
		// WHERE_SORTBYGROUP re-asks the STRICT question (wctrlFlags 0) of a
		// fully-grouping plan, which is the only way a GROUP BY ever reverses --
		// WHERE_GROUPBY itself never sets revLoop.
		n, m := b.wherePathSatisfiesOrderBy(
			b.sort.ob, best.loops[:nLoop-1], best.loops[nLoop-1], false, false, false, false)
		if int(n) == b.sort.nOrderBy() {
			revMask = m
		}
	}

	for _, li := range best.loops {
		for _, ti := range b.loops[li].lTerm {
			if ti >= 0 && b.terms[ti].inRefuse {
				return nil, false
			}
		}
	}
	res := &wherePlanResult{
		loops: best.loops, rev: make([]bool, nLoop), isOrdered: best.isOrdered,
		nRow: b.reportedRowEstimate(best),
	}
	for i := range res.rev {
		res.rev[i] = revMask&(whereMask(1)<<uint(i)) != 0
	}
	return res, true
}

// shortCut ports whereShortCut (where.c:6351-6432): for ONE table with no
// INDEXED BY or NOT INDEXED, a "rowid = <expr of no other table>" term plans a
// WHERE_IPK|WHERE_ONEROW lookup at rRun 33 outright, and failing that the
// first UNIQUE, non-partial index of the probe chain (pTab->pIndex order, the
// fake sPk skipped) with an == term on every key column -- three at most, the
// size of aLTermSpace -- plans a WHERE_ONEROW index lookup at rRun 39. No
// other loop is built and no cost is compared: with sqlite_stat1 in play a
// tiny table's full scan can price at or below such a lookup, and C still takes
// the lookup. nRowOut is set to 1, and an ORDER BY counts as satisfied.
func (b *wherePlanIdxBuild) shortCut() (*wherePlanResult, bool) {
	// "if( pWInfo->wctrlFlags & WHERE_OR_SUBCLAUSE ) return 0;" (where.c:6364):
	// a WHERE_MULTI_OR sub-scan's own sqlite3WhereBegin never takes the
	// shortcut.
	if len(b.items) != 1 || b.orSet != nil || b.orSubclause {
		return nil, false
	}
	it := &b.items[0]
	if it.indexedBy || it.notIndexed {
		return nil, false
	}
	first := func(s *whereScan) int {
		ti := s.next()
		for ti >= 0 && b.terms[ti].prereqRight != 0 {
			ti = s.next()
		}
		return ti
	}
	var l *whereIdxLoop
	for i := range it.idxs {
		if ix := &it.idxs[i]; ix.ipk {
			if ti := first(b.whereScanInitIdx(0, 0, woEq|woIs, ix)); ti >= 0 {
				l = &whereIdxLoop{idx: ix, rRun: 33, nEq: 1, lTerm: []int{ti}}
			}
			break
		}
	}
	if l == nil {
		for i := range it.idxs {
			ix := &it.idxs[i]
			if ix.ipk || !ix.onError || ix.partial != nil || ix.nKeyCol > 3 {
				continue
			}
			opMask := woEq
			if ix.uniqNotNull {
				opMask |= woIs
			}
			var lTerm []int
			for j := 0; j < ix.nKeyCol; j++ {
				ti := first(b.whereScanInitIdx(0, j, opMask, ix))
				if ti < 0 {
					break
				}
				lTerm = append(lTerm, ti)
			}
			if len(lTerm) != ix.nKeyCol {
				continue
			}
			l = &whereIdxLoop{idx: ix, rRun: 39, nEq: len(lTerm), lTerm: lTerm, indexed: true,
				idxOnly: ix.isCovering || it.colUsed&ix.colNotIdxed == 0}
			break
		}
	}
	if l == nil {
		return nil, false
	}
	l.maskSelf, l.oneRow, l.colEq, l.nOut = 1, true, true, 1
	b.loops = append(b.loops, *l)
	res := &wherePlanResult{loops: []int{len(b.loops) - 1}, rev: []bool{false}, nRow: 1}
	if n := b.sort.nOrderBy(); n > 0 {
		res.isOrdered = int8(n)
	}
	return res, true
}

// reportedRowEstimate is pWInfo->nRowOut after sqlite3WhereBegin's general
// solver: best.nRow, reduced by the DISTINCT heuristic ("TUNING: Assume that a
// DISTINCT clause on a subquery reduces the output size by a factor of 8"),
// applied only when the general solver ran:
//
//	if( nTabList!=1 || whereShortCut(&sWLB)==0 ){
//	  ...
//	  wherePathSolver(pWInfo, 0);
//	  ...
//	  if( (pWInfo->wctrlFlags & WHERE_WANT_DISTINCT)!=0 ){
//	    pWInfo->nRowOut -= 30;
//	  }
//	}
//
// (where.c:7080-7121). shortCut is plan()'s first step, so everything reaching
// here took the general solver.
func (b *wherePlanIdxBuild) reportedRowEstimate(best *wherePath) logEst {
	if b.sort == nil || !b.sort.wantDistinct {
		return best.nRow
	}
	return best.nRow - 30 // where.c:7118-7121
}

// wherePlanSingleIndexKey returns the key of the index SQLite scans a
// one-table statement through, or nil when it scans the table (rowid order).
// ok == false declines; the caller keeps rowid order. The winner comes from
// the same wherePathSolver as multi-table plans, with one generation and
// mxChoice 1.
//
// Ties are resolved, not declined: whereLoopInsert breaks them by generation
// order, which is reverse creation order (wherePlanIndexList). Declining would
// not be safe anyway -- a plain SELECT falls back to rowid order, which is
// wrong whenever SQLite walked an index.
//
// reverseOrder is PRAGMA reverse_unordered_selects. It is not a solver input:
// C applies it to the already-chosen path (where.c:7126), so it must not
// influence the winner.
//
// nRow is wherePlanResult.nRow for the winning plan, meaningless when ok is
// false. wherePlanSingleTableIndexOrder is the only caller.
func wherePlanSingleIndexKey(in wherePlanInput, reverseOrder bool) (key *autoIndexKey, nRow logEst, ok bool) {
	b := &wherePlanIdxBuild{
		items: in.items, terms: in.terms,
		sort: in.sort, noAutoIndex: in.noAutoIndex, nQueryLoop: in.nQueryLoop,
		cache: in.loops, orSubclause: in.orSubclause,
	}
	res, pok := b.plan()
	if !pok || len(res.loops) != 1 {
		return nil, 0, false
	}
	nRow = res.nRow
	w := &b.loops[res.loops[0]]
	if w.multiOr {
		// The winner is a WHERE_MULTI_OR union of sub-scans, whose order is a
		// CONCATENATION rather than one b-tree key -- see multiOrOrder
		// (vdbe_op.go). res.rev[0] is necessarily false (a MULTI_OR loop has no
		// pIndex, so wherePathSatisfiesOrderBy answered 0 for the whole path) and
		// whereReverseScanOrder's bit is never read for such a level either: the
		// case-5 arm of sqlite3WhereCodeOneLoopStart uses no bRev, only its
		// sub-scans do -- which is where reverseOrder is applied instead.
		info := b.terms[w.lTerm[0]].orInfo
		if info == nil {
			return nil, 0, false
		}
		nEList := 0
		if in.sort != nil {
			nEList = in.sort.nEList
		}
		mo, ok := wherePlanMultiOrOrder(in.items, nEList, info, reverseOrder)
		if !ok {
			return nil, 0, false
		}
		return &autoIndexKey{multiOr: mo}, nRow, true
	}
	if in.winner != nil {
		in.winner.auto, in.winner.oneRow = w.autoIdx, w.oneRow
	}
	if w.autoIdx {
		// A trusted nested compile's seed is MIN(nQueryLoop,48) (where.c:5921),
		// which can clear solve()'s "pFrom->nRow < 3" gate for a loop expected
		// to run often enough to pay an automatic index back -- and sqlite_stat1
		// makes that ordinary: a correlated subquery over a table it says is
		// small. The rows then arrive in the automatic index's key order, its
		// covering columns included, which is NOT rowid order: over
		// "(SELECT group_concat(t2.b) FROM t t2 WHERE t2.d = t.d)" C walks
		// (d, b, rowid) and answered "3,6,9,..." where the rowid scan this used
		// to fall back to answered "60,57,54,...".
		k := wherePlanAutoIndexKey(in, []int{0}, 0)
		if k == nil {
			return nil, 0, false
		}
		if reverseOrder && in.sort.nOrderBy() == 0 {
			k = whereReversedScanKey(k)
		}
		return k, nRow, true
	}
	// whereReverseScanOrder (where.c:6727) OR-s MASKBIT(ii) into revMask for
	// every FROM item, and sqlite3WhereBegin calls it only when it was handed no
	// pOrderBy at all. The guard is asked HERE rather than by the caller because
	// pWInfo->pOrderBy is not settled until b.plan() has run: a "DISTINCT with no
	// ORDER BY" arrives carrying the result set as its ordering question, and
	// plan() drops it again exactly when isDistinctRedundant says the DISTINCT
	// was pointless -- which is the case where the C leaves pOrderBy 0 and DOES
	// reverse. (The C's own order, where.c:7045-7051 then 7126.)
	rev := res.rev[0] || (reverseOrder && in.sort.nOrderBy() == 0)
	k := wherePlanIndexOrderKey(w.idx, rev)
	if k != nil {
		// The two answers the GROUP BY codegen cannot re-derive; see
		// autoIndexKey.nEq / .groupsSorted (where_plan.go). A reversed-rowid key
		// carries no index columns, so nEq is meaningless there and stays 0.
		if w.idx != nil && !w.idx.ipk {
			k.nEq = b.whereIdxConstantPrefix(w)
		}
		k.groupsSorted = in.sort != nil && in.sort.groupBy &&
			in.sort.nOrderBy() > 0 && int(res.isOrdered) == in.sort.nOrderBy()
	}
	return k, nRow, true
}

// whereIdxConstantPrefix is autoIndexKey.nEq's contract: how many leading key
// columns the WHERE pins to a constant for the whole scan, so they say nothing
// about row or group order.
//
// It is smaller than WhereLoop.u.btree.nEq, which also counts an IN column. An
// IN loop re-seeks per value (codeINTerm, wherecode.c:684), so the column
// varies like an unconstrained one -- wherePathSatisfiesOrderBy does not
// "continue" past WO_IN (where.c:5337). Counting it made "GROUP BY a" with
// "a IN (...)" over an index on t(a DESC) emit groups ascending.
//
// It is a prefix count, stopping at the first IN; under-counting falls back to
// the plan's scan order, which is safe.
func (b *wherePlanIdxBuild) whereIdxConstantPrefix(l *whereIdxLoop) int {
	n := 0
	for n < l.nEq && n < len(l.lTerm) {
		if l.lTerm[n] < 0 || b.terms[l.lTerm[n]].op&(woEq|woIs|woIsNull) == 0 {
			break
		}
		n++
	}
	return n
}

// whereIdxIsDistinctRedundant ports isDistinctRedundant (where.c) for a ONE-table
// FROM clause (the C answers 0 outright for any other): a DISTINCT is pointless
// when some subset of its columns is collectively UNIQUE and individually NOT
// NULL, in which case sqlite3WhereBegin never sets WHERE_DISTINCTBY and the
// statement asks the planner no ordering question at all. Both of the C's tests
// are reproduced -- the IPK one and the unique-index one -- because getting this
// wrong the OTHER way would hand the solver an ORDER BY that SQLite never gave
// it.
func (b *wherePlanIdxBuild) whereIdxIsDistinctRedundant(idxs []whereIndexInfo, ob []whereSortTerm) bool {
	for i := range ob {
		if ob[i].col == xnRowid {
			return true
		}
	}
	for i := range idxs {
		idx := &idxs[i]
		if idx.ipk || !idx.onError || idx.partial != nil { // where.c:680
			continue
		}
		ok := true
		for j := 0; j < idx.nKeyCol; j++ {
			if b.whereIdxFindTermForIndexCol(idx, j) != nil {
				continue
			}
			if !b.whereIdxFindDistinctCol(idx, j, ob) || !b.whereIndexColumnNotNull(0, idx, j) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// whereIdxFindTermForIndexCol is sqlite3WhereFindTerm's OTHER call shape --
// with a pIdx, so the affinity and collation of index column j must match the
// comparison's -- and WO_EQ only. One table, so the cursor is item 0.
func (b *wherePlanIdxBuild) whereIdxFindTermForIndexCol(idx *whereIndexInfo, j int) *whereIdxTerm {
	scan := b.whereScanInitIdx(0, j, woEq, idx)
	for i := scan.next(); i >= 0; i = scan.next() {
		if b.terms[i].prereqRight == 0 {
			return &b.terms[i]
		}
	}
	return nil
}

// whereIdxFindDistinctCol ports findIndexCol (where.c): is index column j one of
// the DISTINCT list's own terms, under the index's own collating sequence?
func (b *wherePlanIdxBuild) whereIdxFindDistinctCol(idx *whereIndexInfo, j int, ob []whereSortTerm) bool {
	for i := range ob {
		// findIndexCol compares against pIdx->aiColumn[iCol] WITHOUT the
		// XN_ROWID rewrite, so a DISTINCT on the INTEGER PRIMARY KEY (iColumn
		// -1) never matches an index naming that column. Reproduced as-is.
		if ob[i].col >= 0 && ob[i].col == idx.aiColumn[j] &&
			equalFoldName(ob[i].coll, idx.azColl[j]) {
			return true
		}
	}
	return false
}
