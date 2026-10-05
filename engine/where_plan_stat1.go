package engine

import (
	"reflect"
	"strings"
)

// SQLITE_STAT1 AS THE PLANNER SEES IT.
//
// The planner reads analysis statistics from sqlite_stat1 into Schema objects.
// These objects persist across operations and are updated when ANALYZE finishes.
// planStat1 holds these objects as the last load left them, kept in step with DDL
// changes (forgetTable, forgetIndex).
type planStat1 struct {
	tables  map[string]*stat1Table // folded table name
	indexes map[string]*stat1Index // folded index name; stat1PKKey for a WITHOUT ROWID key
}

// stat1Table is a Table's nRowLogEst and, when an idx=NULL row carried "sz=",
// its szTabRow.
type stat1Table struct {
	nRow  logEst
	sz    logEst
	szSet bool
}

// stat1Index is one Index's statistics: base and decoded row estimates, flags,
// and size. Kept as decoded rows rather than numbers because nKeyCol bounds decode.
type stat1Index struct {
	table                       string // folded owning table
	partial                     bool
	base, a0                    logEst
	decodes                     []string
	hasStat1, unordered, noSkip bool
	sz                          logEst
	szSet                       bool
}

// stat1PKKey is the planStat1.indexes key of a WITHOUT ROWID table's PRIMARY
// KEY, which has no sqlite_schema row and which a row reaches through its
// table's name (sqlite3PrimaryKeyIndex).
func stat1PKKey(table string) string { return "\x00" + r33sFoldIdent(table) }

// loadAnalysis loads statistics over the current schema objects. An unchanged
// snapshot keeps the same identity, so no plans are dropped.
func (db *DB) loadAnalysis() {
	var prev *planStat1
	if db.stat1Loaded {
		prev = db.stat1
	}
	db.loadAnalysisFrom(prev)
}

// loadAnalysisFresh is the load a schema (re)load does: every object new.
func (db *DB) loadAnalysisFresh() { db.loadAnalysisFrom(nil) }

func (db *DB) loadAnalysisFrom(prev *planStat1) {
	db.stat1Loaded = true
	var raw [][3]*string
	if t := db.findTableMetaIn(scopeMain, "sqlite_stat1"); t != nil && db.ensureTableLoaded(t) == nil {
		t.rows.eachSorted(func(_ uint64, vals []Value) {
			raw = append(raw, stat1RawRow(vals))
		})
	}
	if st := buildStat1(prev, db.segmentSchemaRows(), raw); !planStat1Equal(st, db.stat1) {
		db.stat1 = st
	}
}

// planStats is the snapshot in force, loading it afresh on first use.
func (db *DB) planStats() *planStat1 {
	if !db.stat1Loaded {
		db.loadAnalysisFresh()
	}
	return db.stat1
}

// planStats returns the snapshot used for planning on this pager: either the
// connection's stamped snapshot or a fresh load of its own sqlite_stat1.
func (p *ReadOnlyPager) planStats() *planStat1 {
	if p == nil {
		return nil
	}
	if !p.stat1Set {
		p.stat1Set = true
		p.stat1 = p.loadStat1()
	}
	return p.stat1
}

// setPlanStats stamps the connection's snapshot onto p. A different snapshot
// drops p's plans, which were compiled under the old one.
func (p *ReadOnlyPager) setPlanStats(st *planStat1) {
	if p.stat1Set && p.stat1 != st {
		p.planCache = nil
	}
	p.stat1, p.stat1Set = st, true
}

func (p *ReadOnlyPager) loadStat1() *planStat1 {
	rows, err := p.Schema()
	if err != nil {
		return nil
	}
	for i := range rows {
		r := &rows[i]
		if r.Type != "table" || r.Temp || !equalFoldName(r.Name, "sqlite_stat1") {
			continue
		}
		seq, errFn := p.ScanTable(r.RootPage)
		var raw [][3]*string
		for _, vals := range seq {
			raw = append(raw, stat1RawRow(vals))
		}
		if errFn() != nil {
			return nil
		}
		return buildStat1(nil, rows, raw)
	}
	return nil
}

// stat1RawRow is one row as sqlite3_exec hands analysisLoader its argv: text,
// or nil for a NULL.
func stat1RawRow(vals []Value) [3]*string {
	var out [3]*string
	for i := range out {
		if i < len(vals) && vals[i].Typ != Null {
			s := valueToText(vals[i])
			out[i] = &s
		}
	}
	return out
}

// buildStat1 builds statistics from the previous state and raw rows, applying
// defaults to indices with no analysis rows. Returns nil when all estimates are defaults.
func buildStat1(prev *planStat1, schema []SchemaRow, raw [][3]*string) *planStat1 {
	st := &planStat1{tables: map[string]*stat1Table{}, indexes: map[string]*stat1Index{}}
	named := map[string]bool{} // what sqlite3FindTable finds: tables, views, virtual tables
	for i := range schema {
		r := &schema[i]
		if r.Temp || r.Type != "table" && r.Type != "view" {
			continue
		}
		k := r33sFoldIdent(r.Name)
		named[k] = true
		if r.Type != "table" || isCreateVirtualTableSQL(r.SQL) {
			continue
		}
		ts := &stat1Table{nRow: whereDefaultRowLogEst}
		if prev != nil && prev.tables[k] != nil {
			*ts = *prev.tables[k]
		}
		st.tables[k] = ts
	}
	// Add each index from the previous state or with default estimates.
	addIndex := func(key, table string, partial bool) {
		ts := st.tables[table]
		if ts == nil {
			return
		}
		if prev != nil {
			if old := prev.indexes[key]; old != nil && old.table == table {
				is := *old
				is.decodes = append([]string(nil), old.decodes...)
				st.indexes[key] = &is
				return
			}
		}
		x := ts.nRow
		if prev != nil && x < 99 {
			ts.nRow, x = 99, 99
		}
		if partial {
			x -= 10
		}
		st.indexes[key] = &stat1Index{table: table, partial: partial, base: x, a0: x}
	}
	for i := range schema {
		r := &schema[i]
		if r.Temp {
			continue
		}
		switch {
		case r.Type == "index":
			addIndex(r33sFoldIdent(r.Name), r33sFoldIdent(r.TblName), indexSQLIsPartial(r.SQL))
		case r.Type == "table" && sqlTextTableIsWithoutRowid(r.SQL):
			addIndex(stat1PKKey(r.Name), r33sFoldIdent(r.Name), false)
		}
	}
	for _, is := range st.indexes {
		is.hasStat1 = false
	}

	// analysisLoader, row by row.
	for _, a := range raw {
		if a[0] == nil || a[2] == nil {
			continue // "if( argv==0 || argv[0]==0 || argv[2]==0 ) return 0;"
		}
		tk := r33sFoldIdent(*a[0])
		if !named[tk] {
			continue // "if( pTable==0 ) return 0;"
		}
		ts := st.tables[tk] // nil for a view or a virtual table
		var is *stat1Index
		switch {
		case a[1] == nil:
		case equalFoldName(*a[0], *a[1]):
			is = st.indexes[stat1PKKey(*a[0])] // sqlite3PrimaryKeyIndex; none for a rowid table
		default:
			is = st.indexes[r33sFoldIdent(*a[1])]
		}
		if is == nil {
			// The fakeIdx branch: one number into the TABLE's nRowLogEst.
			if ts != nil {
				n := []logEst{ts.nRow}
				f := decodeStat1(*a[2], 1, n)
				ts.nRow = n[0]
				if f.szSet {
					ts.sz, ts.szSet = f.sz, true
				}
			}
			continue
		}
		n := []logEst{is.a0}
		f := decodeStat1(*a[2], 1, n)
		is.a0 = n[0]
		is.decodes = append(is.decodes, *a[2])
		is.unordered, is.noSkip = f.unordered, f.noSkipScan
		if f.szSet {
			is.sz, is.szSet = f.sz, true
		}
		is.hasStat1 = true
		if ts != nil && !is.partial {
			ts.nRow = is.a0
		}
	}

	// The default pass: sqlite3DefaultRowEst for every index no row reached.
	for _, is := range st.indexes {
		if is.hasStat1 {
			continue
		}
		ts := st.tables[is.table]
		x := ts.nRow
		if x < 99 {
			ts.nRow, x = 99, 99
		}
		if is.partial {
			x -= 10
		}
		is.base, is.a0, is.decodes = x, x, nil
	}
	if prev == nil && len(raw) == 0 {
		return nil // a fresh schema with nothing to load: every estimate the default
	}
	return st
}

// indexSQLIsPartial is Index.pPartIdxWhere != 0, from its CREATE INDEX text.
func indexSQLIsPartial(sql string) bool {
	if strings.TrimSpace(sql) == "" {
		return false // an automatic index is never partial
	}
	ix, err := parseCreateIndexStmt(sql)
	return err == nil && ix.where != nil
}

func planStat1Equal(a, b *planStat1) bool {
	return reflect.DeepEqual(a, b)
}

// forgetTable and forgetIndex are what a DROP does to the objects: the Table or
// Index is gone, and one CREATEd later under the same name starts afresh.
func (st *planStat1) forgetTable(name string) *planStat1 {
	if st == nil {
		return nil
	}
	k := r33sFoldIdent(name)
	out := &planStat1{tables: map[string]*stat1Table{}, indexes: map[string]*stat1Index{}}
	for tk, ts := range st.tables {
		if tk != k {
			out.tables[tk] = ts
		}
	}
	for ik, is := range st.indexes {
		if is.table != k {
			out.indexes[ik] = is
		}
	}
	return out
}

func (st *planStat1) forgetIndex(name string) *planStat1 {
	if st == nil {
		return nil
	}
	k := r33sFoldIdent(name)
	out := &planStat1{tables: st.tables, indexes: map[string]*stat1Index{}}
	for ik, is := range st.indexes {
		if ik != k {
			out.indexes[ik] = is
		}
	}
	return out
}

// stat1Flags is what decodeIntArray records on an Index beside its numbers.
type stat1Flags struct {
	unordered, noSkipScan bool
	sz                    logEst
	szSet                 bool
}

// decodeStat1 decodes up to nOut integers into a[0..] as LogEsts, then trailing
// flags: "unordered", "sz=N", "noskipscan". Tokens taking slots as 0; numbers stop at nOut.
func decodeStat1(z string, nOut int, a []logEst) stat1Flags {
	pos := 0
	for i := 0; pos < len(z) && i < nOut; i++ {
		var v uint64
		for pos < len(z) && z[pos] >= '0' && z[pos] <= '9' {
			v = v*10 + uint64(z[pos]-'0')
			pos++
		}
		if i < len(a) {
			a[i] = logEstFromInt(v)
		}
		if pos < len(z) && z[pos] == ' ' {
			pos++
		}
	}
	var f stat1Flags
	for pos < len(z) {
		rest := z[pos:]
		switch {
		case strings.HasPrefix(rest, "unordered"):
			f.unordered = true
		case strings.HasPrefix(rest, "sz=") && len(rest) > 3 && rest[3] >= '0' && rest[3] <= '9':
			sz := stat1Atoi(rest[3:])
			if sz < 2 {
				sz = 2
			}
			f.sz, f.szSet = logEstFromInt(uint64(sz)), true
		case strings.HasPrefix(rest, "noskipscan"):
			f.noSkipScan = true
		}
		for pos < len(z) && z[pos] != ' ' {
			pos++
		}
		for pos < len(z) && z[pos] == ' ' {
			pos++
		}
	}
	return f
}

// stat1Atoi parses digits after "sz=", skipping leading zeros and capping at
// ten significant digits or 2147483647. Does not support hexadecimal prefix.
func stat1Atoi(s string) int64 {
	i := 0
	for i < len(s) && s[i] == '0' {
		i++
	}
	var v int64
	for n := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i, n = i+1, n+1 {
		if n >= 10 {
			return 0
		}
		v = v*10 + int64(s[i]-'0')
	}
	if v > 2147483647 {
		return 0
	}
	return v
}

// apply applies loaded statistics to a table's row estimate and index estimates.
// Indices not found in the loaded state get default estimates.
func (st *planStat1) apply(tableName string, idxs []whereIndexInfo, szTabRow logEst) (nRow, szTab logEst) {
	nRow, szTab = whereDefaultRowLogEst, szTabRow
	if st == nil {
		return nRow, szTab
	}
	k := r33sFoldIdent(tableName)
	if ts := st.tables[k]; ts != nil {
		nRow = ts.nRow
		if ts.szSet {
			szTab = ts.sz
		}
	}
	state := func(ix *whereIndexInfo) *stat1Index {
		key := r33sFoldIdent(ix.name)
		if ix.primaryKey {
			key = stat1PKKey(tableName)
		}
		if is := st.indexes[key]; is != nil && is.table == k {
			return is
		}
		return nil
	}
	for i := range idxs {
		if ix := &idxs[i]; !ix.ipk && state(ix) == nil && nRow < 99 {
			nRow = 99
		}
	}
	for i := range idxs {
		ix := &idxs[i]
		if ix.ipk {
			ix.aiRowLogEst[0] = nRow
			continue
		}
		is := state(ix)
		if is == nil {
			whereDefaultRowEstFrom(ix, whereDefaultRowPartial(ix, nRow))
			continue
		}
		whereDefaultRowEstFrom(ix, is.base)
		for _, z := range is.decodes {
			decodeStat1(z, ix.nKeyCol+1, ix.aiRowLogEst)
		}
		ix.hasStat1, ix.unordered, ix.noSkipScan = is.hasStat1, is.unordered, is.noSkip
		if is.szSet {
			ix.szIdxRow = is.sz
		}
	}
	return nRow, szTab
}

// wherePlanStat1For is the statistics a plan over tbl, found in p's catalog,
// is priced with: the loaded snapshot when tbl is a MAIN table, and none for a
// TEMP one.
func wherePlanStat1For(p *ReadOnlyPager, tbl *resolvedTable) *planStat1 {
	st := p.planStats()
	if st == nil || tbl == nil || p.localSchema != "" && !equalFoldName(p.localSchema, "main") {
		return nil
	}
	rows, err := p.Schema()
	if err != nil {
		return nil
	}
	for i := range rows {
		r := &rows[i]
		if r.Type == "table" && equalFoldName(r.Name, tbl.name) && r.RootPage == tbl.root {
			if r.Temp {
				return nil
			}
			return st
		}
	}
	return nil
}

// wherePlanItemStats is one planned item's pTab->nRowLogEst and szTabRow, with
// its probe chain's estimates rewritten, under the statistics in force.
func wherePlanItemStats(p *ReadOnlyPager, tbl *resolvedTable, idxs []whereIndexInfo, szTabRow logEst) (logEst, logEst) {
	return wherePlanStat1For(p, tbl).apply(tbl.name, idxs, szTabRow)
}
