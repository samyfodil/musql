// This file implements the rtree and rtree_i32 virtual-table modules.
//
// Schema: CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX [, minY, maxY, ...]).
// The first column is an INTEGER PRIMARY KEY alias; the rest are 1-5 dimensions
// of min/max coordinate pairs (3, 5, 7, 9, or 11 columns total). "rtree" stores
// 32-bit floats; "rtree_i32" stores 32-bit signed integers.
//
// The engine re-applies the WHERE clause over whatever rows a vtab produces,
// so a correct rtree module can store all rows and let WHERE do filtering,
// matching SQLite's result exactly.
//
// Coordinate storage matches rtree.c: min rounds down, max rounds up to the
// nearest float32, preserving spatial semantics.
package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

func init() {
	RegisterVtabModule("rtree", rtreeModule{i32: false})
	RegisterVtabModule("rtree_i32", rtreeModule{i32: true})
}

const rtreeMaxDimensions = 5

// rtreeMaxAuxColumn is RTREE_MAX_AUX_COLUMN (rtree.c:139), which rtreeInit's
// argc check applies to the whole argument list (rtree.c:3651).
const rtreeMaxAuxColumn = 100

// rtreeModule is the rtree / rtree_i32 VtabModule. It is writable (see
// newWritableStore below, the writableVtabModule contract in vtab_write.go).
type rtreeModule struct{ i32 bool }

// Connect satisfies VtabModule. rtree is never eponymous (it needs a CREATE
// VIRTUAL TABLE naming its columns), so Connect is used only to validate the
// module arguments at CREATE time; the actual live table is built by
// newWritableStore. It returns the declared columns and a throwaway store.
func (m rtreeModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	st, err := m.buildStore("", args)
	if err != nil {
		return nil, nil, err
	}
	return st.columns, st, nil
}

// newWritableStore satisfies the writableVtabModule contract (vtab_write.go):
// it builds a fresh, empty backing store for a CREATE VIRTUAL TABLE, using the
// table name (for error messages) and the raw module arguments (column names).
func (m rtreeModule) newWritableStore(tableName string, args []string) (vtabStore, error) {
	return m.buildStore(tableName, args)
}

func (m rtreeModule) buildStore(tableName string, args []string) (*rtreeStore, error) {
	// rtreeInit's argc check (rtree.c:3651-3654): 3 to RTREE_MAX_AUX_COLUMN
	// module arguments, the id included.
	if len(args) < 3 {
		return nil, fmt.Errorf("Too few columns for an rtree table")
	}
	if len(args) > rtreeMaxAuxColumn {
		return nil, fmt.Errorf("Too many columns for an rtree table")
	}
	// rtreeInit's own declaration (rtree.c:3684-3708): each argument's FIRST
	// TOKEN (rtreeTokenLength, :3610-3613) as a column of "CREATE TABLE x(...)",
	// handed to sqlite3_declare_vtab -- the id and coordinates typed, an
	// AUXILIARY "+name ..." column untyped, whatever follows its name. It is
	// that parse -- ahead of the dimension checks below, as in C -- that
	// decides whether the arguments are column names at all:
	// "rtree(index, x1, y1, x2, y2)" is 'near "index": syntax error' in
	// 3.53.3, and a quoted name is dequoted. So the names are taken from the
	// parse, not from the argument text.
	var decl strings.Builder
	decl.WriteString("CREATE TABLE x(")
	nDim2, nAux := 0, 0
	for i, a := range args {
		aux := i > 0 && strings.HasPrefix(a, "+")
		if aux {
			a = a[1:]
			if a == "" || strings.ContainsRune(" \t\n\f\r", rune(a[0])) {
				// rtreeTokenLength would measure the whitespace itself.
				return nil, fmt.Errorf("%w: rtree auxiliary column with whitespace after its +", errVDBEUnsupported)
			}
		} else if nAux > 0 {
			return nil, fmt.Errorf("Auxiliary rtree columns must be last")
		}
		toks, lerr := lex(a)
		if lerr != nil || len(toks) == 0 || toks[0].kind == tkEOF {
			return nil, fmt.Errorf("rtree: empty column name in argument %q", a)
		}
		if i > 0 {
			decl.WriteByte(',')
		}
		decl.WriteString(a[toks[0].Start:toks[0].End])
		switch {
		case aux:
			nAux++
		case i == 0 || m.i32:
			decl.WriteString(" INT") // the id column; rtree_i32 coordinates
		default:
			decl.WriteString(" REAL")
		}
		if i > 0 && !aux {
			nDim2++
		}
	}
	decl.WriteString(");")
	declared, _, _, derr := parseCreateTableColumnsAndAutoIndexes(decl.String(), false)
	if derr != nil {
		return nil, fmt.Errorf("%s", strings.TrimPrefix(derr.Error(), "engine: "))
	}
	if len(declared) != len(args) {
		return nil, fmt.Errorf("%w: rtree declaration parsed to %d columns for %d arguments", errVDBEUnsupported, len(declared), len(args))
	}
	// rtree.c:3712-3721, over the COORDINATE count alone.
	switch {
	case nDim2/2 < 1:
		return nil, fmt.Errorf("Too few columns for an rtree table")
	case nDim2 > rtreeMaxDimensions*2:
		return nil, fmt.Errorf("Too many columns for an rtree table")
	case nDim2%2 != 0:
		return nil, fmt.Errorf("Wrong number of columns for an rtree table")
	}
	names := make([]string, len(args))
	cols := make([]VtabColumn, len(args))
	for i, c := range declared {
		names[i] = c.Name
		typ := "REAL"
		switch {
		case i > nDim2:
			typ = "" // auxiliary: declared without a type
		case i == 0 || m.i32:
			typ = "INT"
		}
		cols[i] = VtabColumn{Name: c.Name, Type: typ}
	}
	return &rtreeStore{
		name:     tableName,
		i32:      m.i32,
		colNames: names,
		columns:  cols,
		dims:     nDim2 / 2,
		nAux:     nAux,
	}, nil
}

// rtreeStore is a writable rtree table's live, in-session state: the r-tree
// itself (rtreeTree, rtree_tree.go), maintained exactly as rtree.c maintains
// it, so its shadow tables, its scan order and the rowids it hands out are C's.
// tree is nil only on a store no CREATE or load has initialized yet.
type rtreeStore struct {
	name     string // vtab name, for constraint-error text
	i32      bool
	colNames []string     // all declared column names, [id, minX, maxX, ...]
	columns  []VtabColumn // parallel VtabColumn schema
	dims     int
	tree     *rtreeTree

	// nAux is the number of auxiliary columns, whose values live in
	// %_rowid's a0, a1, ... (rtree.c:3440-3444) and so outside the tree:
	// aux holds them by rowid, for the rows that have any.
	nAux int
	aux  map[int64][]Value
}

// ---- vtabStore (read/materialize/persist/clone) ----

func (s *rtreeStore) vtabColumns() []VtabColumn { return s.columns }

// vtabRows returns every stored row as a FULL declared row [id, coord0, ...],
// in the order rtree.c's own cursor returns them (rtreeTree.walk) -- which is
// what a SELECT without ORDER BY shows, what "INSERT ... SELECT" assigns
// rowids from, and the order an UPDATE or DELETE visits its rows in.
func (s *rtreeStore) vtabRows() (rowids []int64, rows [][]Value) {
	if s.tree == nil {
		return nil, nil
	}
	nDim2 := s.dims * 2
	// A walk over a tree read from a file cannot fail here: rtreeLoadStore
	// already walked it once and refused anything it could not.
	_ = s.tree.walk(func(c rtreeTCell) {
		row := make([]Value, len(s.columns))
		row[0] = Value{Typ: Int, I: c.rowid}
		for d := 0; d < nDim2 && d+1 < len(row); d++ {
			row[d+1] = rtreeStoredCoord(rtreeCoordValue(c.c[d], s.i32), s.i32)
		}
		// rtreeColumn reads an auxiliary column out of %_rowid (rtree.c:1795-1816);
		// a row that never had one reads NULL, as a0.. do.
		copy(row[1+nDim2:], s.aux[c.rowid])
		rowids = append(rowids, c.rowid)
		rows = append(rows, row)
	})
	return rowids, rows
}

// vtabClone deep-copies the store for a transaction snapshot (txn.go).
func (s *rtreeStore) vtabClone() vtabStore {
	cp := *s
	if s.tree != nil {
		cp.tree = s.tree.clone()
	}
	cp.aux = maps.Clone(s.aux)
	return &cp
}

// vtabLoadRow is part of the vtabStore contract for modules whose rows live
// under a rootpage of their own; an r-tree's live in %_node and are loaded by
// rtreeLoadStore, so nothing reaches this.
// vtabDetach is the interface's own contract (vtab_write.go). The tree holds
// coordinates, which are numbers; the AUXILIARY columns are the only values
// here that can carry bytes read out of a segment file's mapping.
func (s *rtreeStore) vtabDetach() {
	for rid, vals := range s.aux {
		s.aux[rid] = ownValues(vals)
	}
}

func (s *rtreeStore) vtabLoadRow(rowid int64, record []Value) {}

// ---- VirtualTable (read cursor, so Connect's throwaway store is drivable) ----

func (s *rtreeStore) BestIndex(info *VtabIndexInfo) error { return nil } // full scan; engine re-applies WHERE
func (s *rtreeStore) Open() (VtabCursor, error) {
	rowids, rows := s.vtabRows()
	return &rtreeCursor{rowids: rowids, rows: rows}, nil
}

type rtreeCursor struct {
	rowids []int64
	rows   [][]Value
	pos    int
}

func (c *rtreeCursor) Filter(int, string, []Value) error { c.pos = 0; return nil }
func (c *rtreeCursor) Next() error                       { c.pos++; return nil }
func (c *rtreeCursor) Eof() bool                         { return c.pos >= len(c.rows) }
func (c *rtreeCursor) Column(i int) (Value, error)       { return c.rows[c.pos][i], nil }
func (c *rtreeCursor) Rowid() (int64, error)             { return c.rowids[c.pos], nil }
func (c *rtreeCursor) Close() error                      { return nil }

// ---- VtabUpdater (writes) ----

// rtreeCellOf builds the cell rtreeUpdate inserts (rtree.c:3148-3170).
func (s *rtreeStore) rtreeCellOf(rowid int64, coords []Value) rtreeTCell {
	c := rtreeTCell{rowid: rowid}
	for d := range coords {
		c.c[d] = rtreeCoordBits(valueAsFloat(coords[d]), s.i32)
	}
	return c
}

func (s *rtreeStore) needTree() error {
	if s.tree == nil {
		return fmt.Errorf("engine: internal: rtree %s has no tree", s.name)
	}
	return nil
}

// InsertRow is rtreeUpdate's INSERT (rtree.c:3141-3228): the coordinates are
// rounded and checked first, then a given rowid is checked against %_rowid, and
// a NULL one is allocated after that. It returns the rowid used.
func (s *rtreeStore) InsertRow(vals []Value) (int64, error) {
	if len(vals) != len(s.columns) {
		return 0, fmt.Errorf("rtree: internal: %d values for %d columns", len(vals), len(s.columns))
	}
	if err := s.needTree(); err != nil {
		return 0, err
	}
	coords, err := s.coerceCoords(vals[1 : 1+s.dims*2])
	if err != nil {
		return 0, err
	}
	var rowid int64
	if vals[0].Typ != Null {
		rowid = rtreeInt(vals[0])
		if _, exists := s.tree.rowid[rowid]; exists {
			// rtree.c:3194 (rtreeConstraintError(pRtree, 0), message at :3089): a
			// genuine SQLITE_CONSTRAINT return, eligible for OP_VUpdate's
			// OE_Ignore (vtab_write.go's vtabConstraint).
			return 0, vtabConstraint(fmt.Errorf("UNIQUE constraint failed: %s.%s", s.name, s.colNames[0]))
		}
	} else {
		rowid = s.tree.newRowid()
	}
	s.tree.resetCache()
	cell := s.rtreeCellOf(rowid, coords)
	if err := s.tree.insert(&cell); err != nil {
		return 0, err
	}
	s.setAux(rowid, vals[1+s.dims*2:])
	return rowid, nil
}

// setAux is pWriteAux, "UPDATE %_rowid SET a0=?2, ... WHERE rowid=?1"
// (rtree.c:3236-3245), run after the row's cell is in the tree: each value
// bound as it is, into a column declared without a type.
func (s *rtreeStore) setAux(rowid int64, aux []Value) {
	if s.nAux == 0 {
		return
	}
	if s.aux == nil {
		s.aux = map[int64][]Value{}
	}
	s.aux[rowid] = slices.Clone(aux)
}

// UpdateRow is rtreeUpdate's UPDATE: the row is DELETED and the new one
// INSERTED (rtree.c:3200-3228), so even an update that keeps its rowid moves
// the cell to wherever ChooseLeaf puts it now. A NULL id takes a fresh rowid,
// allocated after the delete (bHaveRowid==0, :3213-3215). It returns the new
// rowid.
func (s *rtreeStore) UpdateRow(oldRowid int64, vals []Value) (int64, error) {
	if len(vals) != len(s.columns) {
		return 0, fmt.Errorf("rtree: internal: %d values for %d columns", len(vals), len(s.columns))
	}
	if err := s.needTree(); err != nil {
		return 0, err
	}
	coords, err := s.coerceCoords(vals[1 : 1+s.dims*2])
	if err != nil {
		return 0, err
	}
	haveRowid := vals[0].Typ != Null
	newRowid := oldRowid
	if haveRowid {
		newRowid = rtreeInt(vals[0])
		if newRowid != oldRowid {
			if _, exists := s.tree.rowid[newRowid]; exists {
				// rtree.c:3194, same constraint as InsertRow's above.
				return 0, vtabConstraint(fmt.Errorf("UNIQUE constraint failed: %s.%s", s.name, s.colNames[0]))
			}
		}
	}
	s.tree.resetCache()
	if err := s.tree.deleteRowid(oldRowid); err != nil {
		return 0, err
	}
	delete(s.aux, oldRowid) // the %_rowid row goes with it
	if !haveRowid {
		newRowid = s.tree.newRowid()
	}
	s.tree.resetCache()
	cell := s.rtreeCellOf(newRowid, coords)
	if err := s.tree.insert(&cell); err != nil {
		return 0, err
	}
	s.setAux(newRowid, vals[1+s.dims*2:])
	return newRowid, nil
}

// DeleteRow is rtreeUpdate's DELETE, rtreeDeleteRowid.
func (s *rtreeStore) DeleteRow(rowid int64) error {
	if err := s.needTree(); err != nil {
		return err
	}
	s.tree.resetCache()
	delete(s.aux, rowid)
	return s.tree.deleteRowid(rowid)
}

// coerceCoords coerces the 2*dims coordinate input values to their stored
// representation and enforces the per-dimension min<=max constraint, in the
// same order and with the same wording SQLite's rtree.c uses.
func (s *rtreeStore) coerceCoords(in []Value) ([]Value, error) {
	if len(in) != s.dims*2 {
		return nil, fmt.Errorf("rtree: internal: %d coordinate values for %d dimensions", len(in), s.dims)
	}
	out := make([]Value, len(in))
	for i := 0; i < len(in); i++ {
		isMin := i%2 == 0
		if s.i32 {
			out[i] = Value{Typ: Int, I: int64(int32(rtreeInt(in[i])))}
		} else {
			d := rtreeDouble(in[i])
			if isMin {
				out[i] = Value{Typ: Float, F: rtreeValueDown(d)}
			} else {
				out[i] = Value{Typ: Float, F: rtreeValueUp(d)}
			}
		}
	}
	// min<=max per dimension, reporting the first violated pair.
	for k := 0; k < s.dims; k++ {
		lo := out[2*k]
		hi := out[2*k+1]
		var violated bool
		if s.i32 {
			violated = lo.I > hi.I
		} else {
			violated = lo.F > hi.F
		}
		if violated {
			// rtree.c:3162/3173 (rtreeConstraintError(pRtree, ii+1), message
			// at :3095) -- the x1<=x2 case: also a genuine SQLITE_CONSTRAINT,
			// also OE_Ignore-eligible.
			return nil, vtabConstraint(fmt.Errorf("rtree constraint failed: %s.(%s<=%s)", s.name, s.colNames[2*k+1], s.colNames[2*k+2]))
		}
	}
	return out, nil
}

// rtree.c's exact multiplicative rounding constants (2^23 = 8388608): a "min"
// coordinate that a plain (float) cast rounded UP is nudged toward zero (or
// away, if negative) so the stored float32 is <= the input; a "max" coordinate
// is nudged the other way so it is >= the input. This keeps the stored box a
// conservative cover of the true point and -- crucially -- reproduces rtree's
// stored coordinate byte-for-byte.
const (
	rtreeRndTowards = 1.0 - 1.0/8388608.0 // round towards zero
	rtreeRndAway    = 1.0 + 1.0/8388608.0 // round away from zero
)

// rtreeValueDown rounds d down to a float32 <= d, byte-matching rtree.c's
// rtreeValueDown. Returned as the exact float64 of that float32.
func rtreeValueDown(d float64) float64 {
	f := float32(d)
	if float64(f) > d {
		m := rtreeRndTowards
		if d < 0 {
			m = rtreeRndAway
		}
		f = float32(d * m)
	}
	return float64(f)
}

// rtreeValueUp rounds d up to a float32 >= d, byte-matching rtree.c's
// rtreeValueUp.
func rtreeValueUp(d float64) float64 {
	f := float32(d)
	if float64(f) < d {
		m := rtreeRndAway
		if d < 0 {
			m = rtreeRndTowards
		}
		f = float32(d * m)
	}
	return float64(f)
}

// rtreeDouble coerces a coordinate input to a double, matching
// sqlite3_value_double: integers/reals directly, a leading numeric text
// prefix parsed, everything else (including NULL) 0.
func rtreeDouble(v Value) float64 {
	switch v.Typ {
	case Int:
		return float64(v.I)
	case Float:
		return v.F
	case Text, Blob:
		if isF, i, f, ok := parseNumericPrefix(string(v.S)); ok {
			if isF {
				return f
			}
			return float64(i)
		}
	}
	return 0
}

// rtreeInt coerces an id/i32-coordinate input to an int64, matching
// sqlite3_value_int64: an integer directly, a real truncated toward zero, a
// leading numeric text prefix parsed (a float prefix truncated), NULL/other 0.
func rtreeInt(v Value) int64 {
	switch v.Typ {
	case Int:
		return v.I
	case Float:
		return int64(v.F)
	case Text, Blob:
		if isF, i, f, ok := parseNumericPrefix(string(v.S)); ok {
			if isF {
				return int64(f)
			}
			return i
		}
	}
	return 0
}
