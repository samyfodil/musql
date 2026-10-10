// This file implements libSQL's vector index:
//
//	CREATE INDEX i ON t(libsql_vector_idx(emb [, 'metric=l2', ...]))
//	SELECT id FROM vector_top_k('i', vector('[...]'), 10)
//
// libSQL keeps an approximate DiskANN graph in shadow tables and answers
// vector_top_k from it, so its rows are a guess at the nearest k. Here the
// index stores nothing -- on this format no index does -- and vector_top_k
// ranks rows by their exact distance, through the JIT search over the column
// when it can (segment_vector.go) and a scan of the table otherwise. Every row
// returned is one an exact nearest-neighbour search returns, in distance
// order; equal distances go by rowid, and a zero vector's cosine (NaN) ranks
// last.
//
// The SQL is libSQL's: the same DDL and options, the same checks when the
// index is created and when a row is written (type and dimensions), the same
// error texts, and the same output column, id, the row's rowid. Ported from
// libSQL's vectorIndex.c (vectorIndexCreate, parseVectorIdxParam,
// vectorIdxParseColumnType, vectorIndexSearch) and vectorvtab.c. Not
// reproduced: the shadow tables libSQL adds to sqlite_schema, and its
// graph-tuning options, which are validated and otherwise have nothing to
// tune. A WITHOUT ROWID table's index is accepted, as in libSQL, and its
// search declines.
package engine

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// vecIndex is what a libsql_vector_idx index means: which column, holding
// which vectors, ranked by which distance.
type vecIndex struct {
	col  int
	typ  vecType
	dims int
	l2   bool
}

// vecColumnTypes is libSQL's VECTOR_COLUMN_TYPES, in its order: a declared
// type matches the first name it starts with.
var vecColumnTypes = []struct {
	name string
	typ  vecType
}{
	{"FLOAT32", vecF32}, {"F32_BLOB", vecF32},
	{"FLOAT64", vecF64}, {"F64_BLOB", vecF64},
	{"FLOAT1BIT", vec1Bit}, {"F1BIT_BLOB", vec1Bit},
	{"FLOAT8", vecF8}, {"F8_BLOB", vecF8},
	{"FLOAT16", vecF16}, {"F16_BLOB", vecF16},
	{"FLOATB16", vecFB16}, {"FB16_BLOB", vecFB16},
}

// vecColumnType parses a declared type such as "F32_BLOB(384)", as
// vectorIdxParseColumnType does, returning libSQL's message when it is not one.
func vecColumnType(decl string) (vecType, int, string) {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' }
	z := strings.TrimLeftFunc(decl, func(r rune) bool { return r < 0x80 && isSpace(byte(r)) })
	for _, ct := range vecColumnTypes {
		if len(z) < len(ct.name) || !strings.EqualFold(z[:len(ct.name)], ct.name) {
			continue
		}
		i := len(ct.name)
		skip := func() {
			for i < len(z) && isSpace(z[i]) {
				i++
			}
		}
		skip()
		if i == len(z) || z[i] != '(' {
			break
		}
		i++
		skip()
		dims := 0
		for i < len(z) && z[i] != ')' && !isSpace(z[i]) {
			if z[i] < '0' || z[i] > '9' {
				return 0, 0, "non digit symbol in vector column parameter"
			}
			dims = dims*10 + int(z[i]-'0')
			if dims > vecMaxDims {
				return 0, 0, "max vector dimension exceeded"
			}
			i++
		}
		skip()
		if i == len(z) || z[i] != ')' {
			return 0, 0, "missed closing brace for vector column type"
		}
		i++
		skip()
		if i != len(z) {
			return 0, 0, "extra data after dimension parameter for vector column type"
		}
		if dims <= 0 {
			return 0, 0, "vector column must have non-zero dimension for index"
		}
		return ct.typ, dims, ""
	}
	return 0, 0, "unexpected vector column type"
}

// vecIndexParams is libSQL's VECTOR_PARAM_NAMES: kind 0 takes one of the
// listed words, 1 a positive integer, 2 a float.
var vecIndexParams = []struct {
	name  string
	kind  int
	words []string
}{
	{"type", 0, []string{"diskann"}},
	{"metric", 0, []string{"cosine", "l2"}},
	{"compress_neighbors", 0, []string{"float1bit", "float8", "float16", "floatb16", "float32"}},
	{"alpha", 2, nil},
	{"search_l", 1, nil},
	{"insert_l", 1, nil},
	{"max_neighbors", 1, nil},
}

// parseVecIndexParam is parseVectorIdxParam: one "name=value" option. It
// reports the option's name and value as matched, or libSQL's message.
func parseVecIndexParam(param string) (name, word, msg string) {
	eq := strings.IndexByte(param, '=')
	if eq < 0 {
		return "", "", "unexpected parameter format"
	}
	key, value := param[:eq], param[eq+1:]
	for _, p := range vecIndexParams {
		if !strings.EqualFold(p.name, key) {
			continue
		}
		switch p.kind {
		case 1:
			n := vecAtoi(value)
			if n == 0 {
				return "", "", "invalid representation of integer vector index parameter"
			}
			if n < 0 {
				return "", "", "integer vector index parameter must be positive"
			}
			return p.name, "", ""
		case 2:
			if _, rc := sqliteAtoF([]byte(value)); rc <= 0 {
				return "", "", "invalid representation of floating point vector index parameter"
			}
			return p.name, "", ""
		}
		// A value matches a word it is a prefix of, ignoring case, as
		// sqlite3_strnicmp(word, value, len(value)) does: "metric=co" is
		// cosine, and "metric=" is too.
		for _, w := range p.words {
			if len(value) <= len(w) && strings.EqualFold(w[:len(value)], value) {
				return p.name, w, ""
			}
		}
	}
	return "", "", "invalid parameter"
}

// vecAtoi is sqlite3Atoi: an optional sign and leading digits, 0 for none or
// for a value outside 32 bits.
func vecAtoi(z string) int64 {
	i, neg := 0, false
	if i < len(z) && (z[i] == '-' || z[i] == '+') {
		neg = z[i] == '-'
		i++
	}
	for i < len(z) && z[i] == '0' {
		i++
	}
	var v int64
	for n := 0; i < len(z) && z[i] >= '0' && z[i] <= '9'; i, n = i+1, n+1 {
		if n == 10 {
			return 0
		}
		v = v*10 + int64(z[i]-'0')
	}
	if neg {
		v = -v
	}
	if v > math.MaxInt32 || v < math.MinInt32 {
		return 0
	}
	return v
}

// vecIndexMarker reports whether e is a libsql_vector_idx() call under any
// COLLATE, and whether a COLLATE was there.
func vecIndexMarker(e Expr) (call *FuncExpr, collated bool) {
	for {
		c, ok := e.(CollateExpr)
		if !ok {
			break
		}
		e, collated = c.X, true
	}
	if f, ok := e.(FuncExpr); ok && r33sFoldIdent(f.Name) == "libsql_vector_idx" {
		return &f, collated
	}
	return nil, collated
}

// vectorIndexOf is vectorIndexCreate's validation of a CREATE INDEX whose key
// list holds a libsql_vector_idx() call: nil, nil for any other index.
func vectorIndexOf(stmt *parsedCreateIndex, tbl *tableMeta) (*vecIndex, error) {
	var call *FuncExpr
	collated := false
	for _, e := range stmt.exprs {
		if e == nil {
			continue
		}
		f, c := vecIndexMarker(e)
		collated = collated || c
		if f != nil && call == nil {
			call = f
		}
	}
	if call == nil {
		return nil, nil
	}
	if collated {
		return nil, semanticf("vector index: collation in expression is forbidden")
	}
	if len(stmt.exprs) != 1 {
		return nil, semanticf("vector index: must contain exactly one column wrapped into the libsql_vector_idx function")
	}
	col, ok := call.Args[0].(ColumnExpr)
	if !ok {
		return nil, semanticf("vector index: libsql_vector_idx first argument must be a column token")
	}
	ci := indexOfColumn(tbl, col.Name)
	if ci < 0 {
		// The rowid, which resolves (checkAndCollectIndexExprCols ran first)
		// without being a declared column.
		return nil, semanticf("vector index: libsql_vector_idx first argument must be column with vector type")
	}
	decl := tbl.cols[ci].DeclType
	typ, dims, msg := vecColumnType(decl)
	if msg != "" {
		return nil, semanticf("vector index: %s: %s", msg, decl)
	}
	vi := &vecIndex{col: ci, typ: typ, dims: dims}
	compress := ""
	for _, a := range call.Args[1:] {
		lit, ok := a.(LiteralExpr)
		if !ok || lit.Val.Typ != Text {
			return nil, semanticf("vector index: all arguments after first must be strings")
		}
		name, word, msg := parseVecIndexParam(string(lit.Val.S))
		if msg != "" {
			return nil, semanticf("vector index: invalid vector index parameter '%s': %s", lit.Val.S, msg)
		}
		// A repeated option takes its last value, as vectorIdxParamsGetU64 reads them.
		switch name {
		case "metric":
			vi.l2 = word == "l2"
		case "compress_neighbors":
			compress = word
		}
	}
	if tbl.withoutRowid && tbl.pkIndex != nil && len(tbl.pkIndex.colIdx) != 1 {
		return nil, semanticf("vector index: unsupported for tables without ROWID and composite primary key")
	}
	if compress == "float1bit" && vi.l2 {
		return nil, semanticf("vector index: unable to initialize diskann: 1-bit compression available only for cosine metric")
	}
	return vi, nil
}

// vecIndexRowVector is the vector a row puts in the index, as vectorInRowAlloc
// and diskAnnInsert read it: nil for NULL, an error naming the operation for
// a value that is not a vector of the index's type and size.
func (vi *vecIndex) rowVector(v Value, op string) (*vector, error) {
	if v.Typ == Null {
		return nil, nil
	}
	x, err := parseVector(v, 0)
	if err != nil {
		return nil, err
	}
	if err := vi.check(x, op); err != nil {
		return nil, err
	}
	return &x, nil
}

// check is diskAnnInsert's and diskAnnSearch's test of a vector against the
// index, dimensions first.
func (vi *vecIndex) check(x vector, op string) error {
	if x.dims != vi.dims {
		return vecErr("vector index(%s): dimensions are different: %d != %d", op, x.dims, vi.dims)
	}
	if x.typ != vi.typ {
		return vecErr("vector index(%s): vector type differs from column type: %d != %d", op, x.typ, vi.typ)
	}
	return nil
}

// checkVectorIndexRow is the write-time half of a vector index: the row's
// vector, if the index admits the row, must be one the index can hold.
// whereProg is idx's compiled WHERE, nil when it has none.
func checkVectorIndexRow(tbl *tableMeta, idx *indexMeta, whereProg *selfRowExpr, rowid uint64, vals []Value) error {
	if whereProg != nil {
		v, err := whereProg.eval(rowEvalCtx(tbl, rowid, vals, nil, nil, nil))
		if err != nil {
			return err
		}
		if !isTruthy(v) {
			return nil
		}
	}
	_, err := idx.vec.rowVector(indexColumnValue(tbl, rowid, vals, idx.vec.col), "insert")
	return err
}

// ---- vector_top_k ------------------------------------------------------------

func init() {
	RegisterVtabModule("vector_top_k", vectorTopKModule{})
}

// vectorTopKModule is vector_top_k, declared as libSQL declares it:
// CREATE TABLE x(idx hidden, vector hidden, k hidden, id).
type vectorTopKModule struct{}

func (vectorTopKModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) != 0 {
		return nil, nil, fmt.Errorf("vector_top_k takes no module arguments")
	}
	return []VtabColumn{
		{Name: "idx", Hidden: true},
		{Name: "vector", Hidden: true},
		{Name: "k", Hidden: true},
		{Name: "id"},
	}, vectorTopKTable{}, nil
}

// vectorTopKTable answers from the database (vectorTopKRows), so it never opens
// a cursor.
type vectorTopKTable struct{}

// BestIndex takes the three hidden columns' equality constraints, in column
// order, as vectorVtabBestIndex does; IdxNum is how many there were.
func (vectorTopKTable) BestIndex(info *VtabIndexInfo) error {
	argv := 0
	for col := 0; col < 3; col++ {
		for i, c := range info.Constraints {
			if c.Column != col || c.Op != VtabEQ || info.Usage[i].ArgvIndex != 0 {
				continue
			}
			if !c.Usable {
				return fmt.Errorf("%w: vector_top_k(): an argument depends on another row source, which this engine materializes a virtual table before", errVDBEUnsupported)
			}
			argv++
			info.Usage[i].ArgvIndex = argv
			info.Usage[i].Omit = true
			break
		}
	}
	info.IdxNum = argv
	return nil
}

func (vectorTopKTable) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("vector_top_k: no cursor")
}

// vectorTopKRows is vectorIndexSearch: the k rows nearest the query, as one
// column, the rowid, in distance order.
func (p *ReadOnlyPager) vectorTopKRows(argc int, argv []Value) ([][]Value, []int64, error) {
	if argc != 3 {
		return nil, nil, vecErr("vector index(search): got %d parameters, expected 3", argc)
	}
	q, err := parseVector(argv[1], 0)
	if err != nil {
		return nil, nil, err
	}
	var k int64
	switch kv := argv[2]; kv.Typ {
	case Int:
		k = kv.I
	case Float:
		if kv.F != math.Trunc(kv.F) || math.Abs(kv.F) > math.MaxInt32 {
			return nil, nil, vecErr("vector index(search): third parameter (k) must be an integer, but float value were provided")
		}
		k = int64(kv.F)
	default:
		return nil, nil, vecErr("vector index(search): third parameter (k) must be an integer, but unexpected type of value were provided")
	}
	if k < 0 {
		return nil, nil, vecErr("vector index(search): third parameter (k) must be a non-negative integer, but negative value were provided")
	}
	if argv[0].Typ != Text {
		return nil, nil, vecErr("vector index(search): first parameter (index) must be a string")
	}
	name := string(argv[0].S)
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		// Only this database's own catalog is in reach of the pager.
		if !strings.EqualFold(name[:dot], "main") {
			return nil, nil, fmt.Errorf("%w: vector_top_k() over another schema's index", errVDBEUnsupported)
		}
		name = name[dot+1:]
	}
	tm, idx, vi, err := p.vectorIndexNamed(name)
	if err != nil {
		return nil, nil, err
	}
	if err := vi.check(q, "search"); err != nil {
		return nil, nil, err
	}
	if tm.withoutRowid {
		return nil, nil, fmt.Errorf("%w: vector_top_k() over a WITHOUT ROWID table", errVDBEUnsupported)
	}
	if vi.l2 && vi.typ == vec1Bit {
		return nil, nil, fmt.Errorf("%w: vector_top_k() by l2 over float1bit vectors", errVDBEUnsupported)
	}
	if k == 0 {
		return nil, nil, nil
	}

	var ids []int64
	if idx.where == nil && vi.typ == vecF32 {
		ids, err = p.vectorTopKSegments(tm, vi, argv[1], q, k)
	}
	if ids == nil && err == nil {
		ids, err = p.vectorTopKScan(tm, idx, vi, q, k)
	}
	if err != nil {
		return nil, nil, err
	}
	rows := make([][]Value, len(ids))
	for i, id := range ids {
		rows[i] = []Value{{Typ: Null}, {Typ: Null}, {Typ: Null}, {Typ: Int, I: id}}
	}
	return rows, ids, nil
}

// vectorIndexNamed finds a vector index by its exact name, as libSQL's
// metadata lookup does (vectorIndexGetParameters): any other index, or none,
// is "failed to parse vector index parameters".
func (p *ReadOnlyPager) vectorIndexNamed(name string) (*tableMeta, *indexMeta, *vecIndex, error) {
	notFound := vecErr("vector index(search): failed to parse vector index parameters")
	schema, err := p.Schema()
	if err != nil {
		return nil, nil, nil, err
	}
	var ir, tr *SchemaRow
	for i := range schema {
		if schema[i].Type == "index" && !schema[i].Temp && schema[i].Name == name {
			ir = &schema[i]
		}
	}
	if ir == nil || ir.SQL == "" {
		return nil, nil, nil, notFound
	}
	for i := range schema {
		if schema[i].Type == "table" && !schema[i].Temp && equalFoldName(schema[i].Name, ir.TblName) {
			tr = &schema[i]
		}
	}
	if tr == nil {
		return nil, nil, nil, notFound
	}
	withoutRowid := sqlTextTableIsWithoutRowid(tr.SQL)
	cols, _, _, err := parseCreateTableColumnsAndAutoIndexes(tr.SQL, withoutRowid)
	if err != nil {
		return nil, nil, nil, err
	}
	tm := &tableMeta{name: tr.Name, sql: tr.SQL, cols: cols, ipkIndex: -1, rootPage: tr.RootPage}
	for i, c := range cols {
		if c.IsRowidAlias {
			tm.ipkIndex = i
			break
		}
	}
	tm.withoutRowid = withoutRowid
	stmt, err := parseCreateIndexStmt(ir.SQL)
	if err != nil {
		return nil, nil, nil, err
	}
	demoteDoubleQuotedIndexColumns(stmt, tm)
	idx, err := buildExprIndexMeta(ir.Name, tm, stmt, ir.SQL)
	if err != nil || idx.vec == nil {
		return nil, nil, nil, notFound
	}
	return tm, idx, idx.vec, nil
}

// vectorTopKSegments is the JIT search over a float32 column
// (segVectorTopK), or nil, nil when the table is not one it serves.
func (p *ReadOnlyPager) vectorTopKSegments(tm *tableMeta, vi *vecIndex, qv Value, q vector, k int64) ([]int64, error) {
	if qv.Typ != Blob || k > math.MaxInt32 || p.segs == nil {
		return nil, nil
	}
	// The rowid is read as the INTEGER PRIMARY KEY column, or, without one,
	// as a column number no real column has.
	ipk := tm.ipkIndex
	if ipk < 0 {
		ipk = len(tm.cols)
	}
	segs := p.segs.byRoot[tm.rootPage]
	plan := &segVectorPlan{l2: vi.l2, col: vi.col, limit: int(k), outCols: []int{ipk}, nanLast: true,
		rowidOf: func(h vecHit) int64 {
			if h.seg == len(segs) {
				return h.rid
			}
			return int64(segs[h.seg].Rowid(h.row))
		}}
	rows, ok := p.segVectorTopK(tm.rootPage, plan, ipk, Value{Typ: Blob, S: q.blob()})
	if !ok {
		return nil, nil
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		if r[0].Typ != Int {
			return nil, nil
		}
		ids[i] = r[0].I
	}
	return ids, nil
}

// vectorTopKScan ranks every row the index admits, reading the table.
func (p *ReadOnlyPager) vectorTopKScan(tm *tableMeta, idx *indexMeta, vi *vecIndex, q vector, k int64) ([]int64, error) {
	type hit struct {
		d  float32
		id int64
	}
	var hits []hit
	seq, errFn := p.ScanTable(tm.rootPage)
	for rowid, vals := range seq {
		v := indexColumnValue(tm, rowid, vals, vi.col)
		if v.Typ == Null {
			continue
		}
		if idx.where != nil {
			_, in, err := indexEntryOf(tm, idx, rowid, vals)
			if err != nil {
				return nil, err
			}
			if !in {
				continue
			}
		}
		x, err := vi.rowVector(v, "insert")
		if err != nil {
			return nil, err
		}
		var d float32
		if vi.l2 {
			d = distanceL2(*x, q)
		} else {
			d = distanceCos(*x, q)
		}
		hits = append(hits, hit{d, int64(rowid)})
	}
	if err := errFn(); err != nil {
		return nil, err
	}
	slices.SortFunc(hits, func(a, b hit) int {
		an, bn := a.d != a.d, b.d != b.d
		switch {
		case an != bn:
			if an {
				return 1
			}
			return -1
		case !an && a.d != b.d:
			if a.d < b.d {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.id, b.id)
	})
	n := min(int64(len(hits)), k)
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = hits[i].id
	}
	return ids, nil
}
