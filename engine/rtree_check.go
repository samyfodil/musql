// Package engine implements rtreecheck() and rtree's xIntegrity by running SQL
// against shadow tables and checking node blobs with rtree.c's arithmetic.
package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

// rtreeCheckMaxError is the max errors to report before stopping (100).
const rtreeCheckMaxError = 100

// rtreeMaxDepth is the max tree depth (40).
const rtreeMaxDepth = 40

// errRtreeCheckNoMem is the out-of-memory error for zero-length node blobs.
var errRtreeCheckNoMem = errors.New("out of memory")

// rtreeLoadError holds facts from a failed r-tree load.
type rtreeLoadError struct {
	nCol  int
	i32   bool
	node1 int // length of node 1's blob, -1 when it is missing
	err   error
}

// rtreeNode1Len returns the length of node 1's data, or -1 if not found.
func rtreeNode1Len(rp *ReadOnlyPager, name string) int {
	rowids, records, err := rp.Rows(name + "_node")
	if err != nil {
		return -1
	}
	for i, rid := range rowids {
		if rid == 1 && len(records[i]) > 1 {
			return len(rtreeColumnBlob(records[i][1]))
		}
	}
	return -1
}

func (e *rtreeLoadError) Error() string { return e.err.Error() }
func (e *rtreeLoadError) Unwrap() error { return e.err }

// rtreeCheckQuery runs one SQL statement, returning column names and rows.
type rtreeCheckQuery func(sql string, args ...Value) ([]string, [][]Value, error)

// sqliteQuoteQ quotes a string: doubles single quotes, NULL writes "(NULL)".
func sqliteQuoteQ(s *string) string {
	if s == nil {
		return "(NULL)"
	}
	return strings.ReplaceAll(*s, "'", "''")
}

// sqliteQuoteBigQ is %q wrapped in single quotes, NULL writes bare NULL keyword.
func sqliteQuoteBigQ(s *string) string {
	if s == nil {
		return "NULL"
	}
	return "'" + sqliteQuoteQ(s) + "'"
}

// rtreeCheck is struct RtreeCheck (rtree.c:3840).
type rtreeCheck struct {
	q               rtreeCheckQuery
	zDb, zTab       *string
	bInt            bool
	nDim            int
	nLeaf, nNonLeaf int64
	rc              error // check.rc: the first failure; nil is SQLITE_OK
	decline         error // an inner statement this engine cannot run
	report          []string
	nErr            int

	// Not in C: whether every node the walk read had nodeSize bytes, which is
	// what makes C's own step over the same tree succeed (see rtreeCheckTable).
	nodeSize int
	sized    bool
}

// fail records an inner statement's failure as check.rc, or as a decline when
// the statement is one this engine cannot run -- then what C would have done
// with it is unknown, and the whole call declines.
func (c *rtreeCheck) fail(err error) {
	if errors.Is(err, errVDBEUnsupported) {
		if c.decline == nil {
			c.decline = err
		}
		return
	}
	if c.rc == nil {
		c.rc = err
	}
}

func (c *rtreeCheck) ok() bool { return c.rc == nil && c.decline == nil }

// appendMsg is rtreeCheckAppendMsg (rtree.c:3903).
func (c *rtreeCheck) appendMsg(format string, a ...any) {
	if c.ok() && c.nErr < rtreeCheckMaxError {
		c.report = append(c.report, fmt.Sprintf(format, a...))
		c.nErr++
	}
}

// columnBlob is sqlite3_column_bytes followed by sqlite3_column_blob: a BLOB or
// TEXT value's bytes, an INTEGER or REAL rendered as text (sqlite3_value_blob
// falls through to sqlite3_value_text for those), and nothing for NULL.
func rtreeColumnBlob(v Value) []byte {
	switch v.Typ {
	case Blob, Text:
		return v.S
	case Int, Float:
		return []byte(valueToText(v))
	}
	return nil
}

// getNode is rtreeCheckGetNode (rtree.c:3937).
func (c *rtreeCheck) getNode(iNode int64) []byte {
	if !c.ok() {
		return nil
	}
	_, rows, err := c.q(fmt.Sprintf("SELECT data FROM %s.'%s_node' WHERE nodeno=?", sqliteQuoteBigQ(c.zDb), sqliteQuoteQ(c.zTab)), Value{Typ: Int, I: iNode})
	if err != nil {
		c.fail(err)
		return nil
	}
	if len(rows) == 0 {
		c.appendMsg("Node %d missing from database", iNode)
		return nil
	}
	b := rtreeColumnBlob(rows[0][0])
	if len(b) == 0 {
		c.fail(errRtreeCheckNoMem)
		return nil
	}
	if len(b) != c.nodeSize {
		c.sized = false
	}
	return b
}

// checkMapping is rtreeCheckMapping (rtree.c:3981).
func (c *rtreeCheck) checkMapping(bLeaf bool, iKey, iVal int64) {
	sql, tbl := "SELECT parentnode FROM %s.'%s_parent' WHERE nodeno=?1", "%_parent"
	if bLeaf {
		sql, tbl = "SELECT nodeno FROM %s.'%s_rowid' WHERE rowid=?1", "%_rowid"
	}
	if !c.ok() {
		return
	}
	_, rows, err := c.q(fmt.Sprintf(sql, sqliteQuoteBigQ(c.zDb), sqliteQuoteQ(c.zTab)), Value{Typ: Int, I: iKey})
	if err != nil {
		c.fail(err)
		return
	}
	if len(rows) == 0 {
		c.appendMsg("Mapping (%d -> %d) missing from %s table", iKey, iVal, tbl)
		return
	}
	if ii := bitwiseIntOperand(rows[0][0]); ii != iVal { // sqlite3_column_int64
		c.appendMsg("Found (%d -> %d) in %s table, expected (%d -> %d)", iKey, ii, tbl, iKey, iVal)
	}
}

// coordLess reports a<b over two stored coordinates, as an int32 for an
// rtree_i32 table and as a float otherwise -- RtreeCoord's two members
// (rtree.c:300-303), compared exactly as rtreeCheckCellCoord does.
func (c *rtreeCheck) coordLess(a, b uint32) bool {
	if c.bInt {
		return int32(a) < int32(b)
	}
	return math.Float32frombits(a) < math.Float32frombits(b)
}

// checkCellCoord is rtreeCheckCellCoord (rtree.c:4033).
func (c *rtreeCheck) checkCellCoord(iNode int64, iCell int, cell, parent []byte) {
	for i := 0; i < c.nDim; i++ {
		c1 := binary.BigEndian.Uint32(cell[4*2*i:])
		c2 := binary.BigEndian.Uint32(cell[4*(2*i+1):])
		if c.coordLess(c2, c1) {
			c.appendMsg("Dimension %d of cell %d on node %d is corrupt", i, iCell, iNode)
		}
		if parent != nil {
			p1 := binary.BigEndian.Uint32(parent[4*2*i:])
			p2 := binary.BigEndian.Uint32(parent[4*(2*i+1):])
			if c.coordLess(c1, p1) || c.coordLess(p2, c2) {
				c.appendMsg("Dimension %d of cell %d on node %d is corrupt relative to parent", i, iCell, iNode)
			}
		}
	}
}

// checkNode is rtreeCheckNode (rtree.c:4079). The recursion is bounded by the
// depth read off the root, which it refuses past RTREE_MAX_DEPTH.
func (c *rtreeCheck) checkNode(iDepth int, parent []byte, iNode int64) {
	node := c.getNode(iNode)
	if node == nil {
		return
	}
	if len(node) < 4 {
		c.appendMsg("Node %d is too small (%d bytes)", iNode, len(node))
		return
	}
	if parent == nil {
		iDepth = int(binary.BigEndian.Uint16(node))
		if iDepth > rtreeMaxDepth {
			c.appendMsg("Rtree depth out of range (%d)", iDepth)
			return
		}
	}
	nCell := int(binary.BigEndian.Uint16(node[2:]))
	cellSize := 8 + c.nDim*2*4
	if 4+nCell*cellSize > len(node) {
		c.appendMsg("Node %d is too small for cell count of %d (%d bytes)", iNode, nCell, len(node))
		return
	}
	for i := range nCell {
		cell := node[4+i*cellSize:]
		iVal := int64(binary.BigEndian.Uint64(cell))
		c.checkCellCoord(iNode, i, cell[8:], parent)
		if iDepth > 0 {
			c.checkMapping(false, iVal, iNode)
			c.checkNode(iDepth-1, cell[8:], iVal)
			c.nNonLeaf++
		} else {
			c.checkMapping(true, iVal, iNode)
			c.nLeaf++
		}
	}
}

// checkCount is rtreeCheckCount (rtree.c:4142).
func (c *rtreeCheck) checkCount(zTbl string, nExpect int64) {
	if !c.ok() {
		return
	}
	_, rows, err := c.q(fmt.Sprintf("SELECT count(*) FROM %s.'%s%s'", sqliteQuoteBigQ(c.zDb), sqliteQuoteQ(c.zTab), zTbl))
	if err != nil {
		c.fail(err)
		return
	}
	if len(rows) > 0 {
		if nActual := bitwiseIntOperand(rows[0][0]); nActual != nExpect {
			c.appendMsg("Wrong number of entries in %%%s table - expected %d, actual %d", zTbl, nExpect, nActual)
		}
	}
}

// rtreeCheckTable is rtreeCheckTable (rtree.c:4166): the report ("" when the
// tree is sound) and check.rc. A non-nil error that is errVDBEUnsupported means
// the answer is not knowable here and the caller must decline.
//
// One input is decided differently from C, and only where the difference could
// show. C's bInt is "column 1 of the FIRST ROW of SELECT * FROM <table> is an
// INTEGER" (rtree.c:4204), which for a healthy rtree_i32 table with any row is
// true, and false once its own lazy walk to that first row hits a node it finds
// corrupt -- a verdict that depends on the node size its connection cached at
// xCreate or xConnect (getNodeSize, rtree.c:3567), which this engine does not
// model. bInt changes nothing but how coordinates compare, so the check runs
// with each value; when the two reports agree, that is C's report whatever its
// step did. When they differ, the int reading is answered only for a tree this
// engine loaded whole with every node the size C writes (then C's step reaches
// the first row too), and otherwise the call declines.
func rtreeCheckTable(q rtreeCheckQuery, conns *RtreeConnections, zDb, zTab *string) (string, error) {
	base := rtreeCheck{q: q, zDb: zDb, zTab: zTab}

	nAux := 0
	if cols, _, err := q(fmt.Sprintf("SELECT * FROM %s.'%s_rowid'", sqliteQuoteBigQ(zDb), sqliteQuoteQ(zTab))); err == nil {
		nAux = len(cols) - 2
	} else if errors.Is(err, errVDBEUnsupported) {
		return "", err
	} else if !rtreeCheckPrepareError(err) {
		// C only PREPARES this statement (rtree.c:4180), so a failure while
		// running it is not one C can meet.
		return "", fmt.Errorf("%w: rtreecheck(): %v", errVDBEUnsupported, err)
	}

	var (
		i32       bool // an rtree_i32 table whose bInt this engine must decide
		loaded    bool // the r-tree loaded whole
		ambiguous bool // C's first step may or may not have reached a row
	)
	cols, rows, err := q(fmt.Sprintf("SELECT * FROM %s.%s", sqliteQuoteBigQ(zDb), sqliteQuoteBigQ(zTab)))
	var le *rtreeLoadError
	switch {
	case err == nil:
		base.nDim = (len(cols) - 1 - nAux) / 2
		if base.nDim >= 1 && len(rows) > 0 && len(rows[0]) > 1 {
			base.bInt = rows[0][1].Typ == Int
			i32, loaded = base.bInt, true
		}
	case errors.Is(err, errVDBEUnsupported):
		return "", err
	case errors.As(err, &le):
		// C's step returned SQLITE_CORRUPT, which rtree.c:4209 forgives; the
		// prepare had already sized the result. Every node C cannot read
		// becomes SQLITE_CORRUPT_VTAB there (nodeAcquire, rtree.c:757-763),
		// so every failed load here is that case -- provided the prepare got
		// that far. With node 1 missing or short it does only on a connection
		// that already holds the table; any other connects, and getNodeSize
		// refuses it (rtree.c:3595), failing the whole call. So the answer is
		// known only for a table this connection created (rtree_conn.go).
		if le.node1 < rtreeMinNodeSize && !rtreeHeldByConnection(q, conns, zDb, zTab) {
			return "", fmt.Errorf("%w: rtreecheck() over a damaged root node, whose answer depends on whether this connection already holds the table", errVDBEUnsupported)
		}
		base.nDim = (le.nCol - 1 - nAux) / 2
		i32, ambiguous = le.i32, le.i32
	default:
		base.rc = err
	}
	if base.nDim < 1 {
		if base.rc == nil {
			base.appendMsg("Schema corrupt or not an rtree")
		}
		return rtreeCheckResult(&base)
	}
	if i32 {
		if _, prow, perr := q(fmt.Sprintf("PRAGMA %s.page_size", sqliteQuoteBigQ(zDb))); perr == nil && len(prow) > 0 {
			base.nodeSize = rtreeNodeSize(int(bitwiseIntOperand(prow[0][0])), base.nDim*2)
		}
	}

	run := func(bInt bool) (rtreeCheck, string, error) {
		c := base
		c.bInt, c.sized = bInt, true
		if c.ok() {
			c.checkNode(0, nil, 1)
		}
		c.checkCount("_rowid", c.nLeaf)
		c.checkCount("_parent", c.nNonLeaf)
		rep, rerr := rtreeCheckResult(&c)
		return c, rep, rerr
	}
	c, rep, rerr := run(base.bInt)
	// bInt steers only how coordinates compare, never which statements run,
	// so a failed check fails the same way under either value.
	if !i32 || rerr != nil {
		return rep, rerr
	}
	if _, other, _ := run(!base.bInt); rep == other {
		return rep, nil
	}
	if loaded && !ambiguous && c.sized && c.nodeSize > 0 {
		return rep, nil
	}
	return "", fmt.Errorf("%w: rtreecheck() over an rtree_i32 tree whose first row C's own step may not reach", errVDBEUnsupported)
}

// rtreeHeldByConnection reports whether this connection created zDb.zTab and
// nothing has moved zDb's schema cookie since (RtreeConnections).
func rtreeHeldByConnection(q rtreeCheckQuery, conns *RtreeConnections, zDb, zTab *string) bool {
	if conns == nil || zDb == nil || zTab == nil {
		return false
	}
	_, rows, err := q(fmt.Sprintf("PRAGMA %s.schema_version", sqliteQuoteBigQ(zDb)))
	if err != nil || len(rows) == 0 || len(rows[0]) == 0 || rows[0][0].Typ != Int {
		return false
	}
	return conns.createdAt(*zDb, *zTab, uint32(rows[0][0].I))
}

// rtreeCheckResult turns a finished check into rtreeCheckTable's result.
func rtreeCheckResult(c *rtreeCheck) (string, error) {
	if c.decline != nil {
		return "", c.decline
	}
	if c.rc != nil {
		return "", c.rc
	}
	return strings.Join(c.report, "\n"), nil
}

// rtreeCheckPrepareError reports whether err is one C meets at PREPARE time --
// an object or schema that does not exist, or text that does not parse -- as
// opposed to one only running the statement could produce.
func rtreeCheckPrepareError(err error) bool {
	msg := err.Error()
	for _, s := range []string{"no such table", "unknown database", "syntax error", "incomplete input", "unrecognized token"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// rtreecheckValue is the SQL function rtreecheck() (rtree.c:4282) over q: "ok"
// for a sound tree, the report otherwise, and an error carrying C's result
// code when the check itself failed (sqlite3_result_error_code, whose message
// is sqlite3ErrStr's).
func rtreecheckValue(q rtreeCheckQuery, conns *RtreeConnections, args []Value) (Value, error) {
	if len(args) != 1 && len(args) != 2 {
		return Value{}, fmt.Errorf("engine: wrong number of arguments to function rtreecheck()")
	}
	text := func(v Value) *string { // sqlite3_value_text
		if v.Typ == Null {
			return nil
		}
		s := valueToText(v)
		return &s
	}
	zDb, zTab := text(args[0]), (*string)(nil)
	if len(args) == 1 {
		main := "main"
		zDb, zTab = &main, zDb
	} else {
		zTab = text(args[1])
	}
	rep, err := rtreeCheckTable(q, conns, zDb, zTab)
	switch {
	case errors.Is(err, errVDBEUnsupported):
		return Value{}, err
	case errors.Is(err, errRtreeCheckNoMem):
		return Value{}, fmt.Errorf("engine: out of memory")
	case err != nil:
		return Value{}, fmt.Errorf("engine: SQL logic error")
	}
	if rep == "" {
		rep = "ok"
	}
	return Value{Typ: Text, S: []byte(rep)}, nil
}

// rtreecheck is OpFunction's arm for rtreecheck(): the check reads the database
// as this statement sees it -- the write session's own image inside a write
// program, the program's pager otherwise.
func (m *vdbe) rtreecheck(args []Value) (Value, error) {
	var pager *ReadOnlyPager
	switch {
	case m.wctx != nil && m.wctx.db != nil:
		p, err := m.wctx.db.SnapshotPager()
		if err != nil {
			return Value{}, err
		}
		pager = p
	case m.pager != nil:
		pager = m.pager
	default:
		for c := m.outer; c != nil && pager == nil; c = c.outer {
			pager = c.pager
		}
	}
	if pager == nil {
		return Value{}, fmt.Errorf("%w: rtreecheck() with no database to read", errVDBEUnsupported)
	}
	return rtreecheckValue(func(sql string, a ...Value) ([]string, [][]Value, error) {
		return pager.QueryArgs(sql, a)
	}, pager.rtreeConns, args)
}

// integrityCheckRtrees is PRAGMA integrity_check's second pass over the
// virtual tables in scope (pragma.c:2163-2191) for the one module whose
// xIntegrity this engine ports, rtree's (rtreeIntegrity, rtree.c:4214-4230):
// rtreeCheckTable's report, prefixed "In RTree <schema>.<name>:", one row per
// r-tree that has one. quick_check runs it too -- rtreeIntegrity ignores
// isQuick. The pass visits the schema's tables in HASH order, which decides
// the order of two or more reports; that order is not reproduced here, so the
// pragma declines when more than one r-tree has something to say.
func (p *ReadOnlyPager) integrityCheckRtrees(scope schemaScope, only string) ([]Value, error) {
	rows, err := p.Schema()
	if err != nil {
		return nil, err
	}
	zDb := localSchemaOr(p.localSchema)
	if scope == scopeTemp {
		zDb = "temp"
	}
	q := func(sql string, a ...Value) ([]string, [][]Value, error) { return p.QueryArgs(sql, a) }
	var out []Value
	for i := range rows {
		tr := &rows[i]
		if tr.Type != "table" || !scope.accepts(tr.Temp) || !isCreateVirtualTableSQL(tr.SQL) {
			continue
		}
		if only != "" && !equalFoldName(tr.Name, only) {
			continue
		}
		_, module, _, _, perr := parseCreateVirtualTableStmt(tr.SQL)
		if perr != nil {
			continue
		}
		if m := r33sFoldIdent(module); m != "rtree" && m != "rtree_i32" {
			continue
		}
		name := tr.Name
		// sqlite3ViewGetColumnNames connects a table the connection does not
		// hold yet, and a damaged root fails that connect -- the whole pragma
		// with it (pragma.c:2178). See rtreeCheckTable for the same split.
		if rtreeNode1Len(p, name) < rtreeMinNodeSize && !rtreeHeldByConnection(q, p.rtreeConns, &zDb, &name) {
			return nil, fmt.Errorf("%w: PRAGMA integrity_check over r-tree %s, whose damaged root node fails xConnect on a connection that does not already hold it", errVDBEUnsupported, name)
		}
		rep, cerr := rtreeCheckTable(q, p.rtreeConns, &zDb, &name)
		if cerr != nil {
			if errors.Is(cerr, errVDBEUnsupported) {
				return nil, cerr
			}
			// OP_VCheck aborts the statement on a non-OK xIntegrity
			// (vdbe.c:8434-8437).
			return nil, fmt.Errorf("engine: SQL logic error")
		}
		if rep != "" {
			out = append(out, Value{Typ: Text, S: []byte("In RTree " + zDb + "." + name + ":\n" + rep)})
		}
	}
	if len(out) > 1 {
		return nil, fmt.Errorf("%w: PRAGMA integrity_check with %d damaged r-trees, reported in schema hash order", errVDBEUnsupported, len(out))
	}
	return out, nil
}
