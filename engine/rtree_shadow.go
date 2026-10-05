package engine

// R-tree shadow tables: the three real tables an r-tree creates
// and syncing logic to keep them equal.
// file owns the schema and the sync, mirroring fts5_shadow.go exactly.
//
// Before this existed, rtree was this engine's last PRIVATE-STORE module: its
// rows lived in a b-tree under the vtab's own rootpage and nowhere C SQLite
// would look. The cost was not subtle -- a database this engine wrote answered
//
//	SELECT id,x0,x1 FROM r   ->   no such table: main.r_node
//
// in C SQLite, which made rtree the remaining exception to this project's
// central premise, and it is why sqlite_master and PRAGMA table_list declined
// outright whenever an r-tree was in the schema (query.go's
// vtabPrivateStoreModules): C's catalog shows four rows there and this engine
// could only show one.

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// createShadowTables creates the three tables rtree.c:3440-3453 creates, with
// its own column spellings, and seeds node 1 -- which must exist even for an
// empty tree (rtree.c:37, "The root node of an r-tree always exists").
//
// C writes the seed as zeroblob(iNodeSize); this writes the encoded empty root
// instead, which is the same bytes (depth 0, zero entries, zero padding) and
// says so structurally rather than by coincidence.
func (m rtreeModule) createShadowTables(db *DB, name string, st *rtreeStore, isTemp bool) error {
	tempKw := ""
	if isTemp {
		tempKw = "TEMP "
	}
	// "_rowid" carries one "a<i>" column per AUXILIARY column (rtree.c:3440-3444).
	// DOUBLE quotes, not fts3's single ones: rtree.c:3440-3450 writes each
	// shadow name with "%w" ("CREATE TABLE \"%w\".\"%w_node\"(...)"), and
	// the CREATE text is what lands in sqlite_master for a reader to compare.
	var auxCols strings.Builder
	for i := range st.nAux {
		fmt.Fprintf(&auxCols, ",a%d", i)
	}
	stmts := []struct{ suffix, sql string }{
		{"_rowid", fmt.Sprintf("CREATE %sTABLE %s(rowid INTEGER PRIMARY KEY,nodeno%s)", tempKw, quoteIdent(name+"_rowid"), auxCols.String())},
		{"_node", fmt.Sprintf("CREATE %sTABLE %s(nodeno INTEGER PRIMARY KEY,data)", tempKw, quoteIdent(name+"_node"))},
		{"_parent", fmt.Sprintf("CREATE %sTABLE %s(nodeno INTEGER PRIMARY KEY,parentnode)", tempKw, quoteIdent(name+"_parent"))},
	}
	scope := createScope(isTemp)
	created := make([]string, 0, len(stmts))
	for _, s := range stmts {
		if err := db.CreateTable(s.sql); err != nil {
			// A shadow name already taken leaves nothing behind, exactly as
			// fts3/fts5's own createShadowTables does.
			for _, n := range created {
				if tbl := db.findTableMetaIn(scope, n); tbl != nil {
					db.removeTableAndIndexes(tbl)
				}
			}
			return err
		}
		created = append(created, name+s.suffix)
	}
	// getNodeSize's xCreate arm (rtreeNodeSize): the size comes from the page
	// size the file uses, and xConnect later reads it back as length(data) of
	// node 1.
	st.tree = newRtreeTree(rtreeNodeSize(int(db.pageSize), st.dims*2), st.dims*2, st.i32)
	return db.rtreeSyncShadowsIn(scope, name, st)
}

// rtreeSyncIfNeeded re-encodes vm's shadow tables when vm is an r-tree, and
// does nothing otherwise -- fts5SyncIfNeeded's twin, called from the same
// places so the file image and the live store can never disagree.
func (db *DB) rtreeSyncIfNeeded(vm *vtabMeta) error {
	st, isRtree := vm.store.(*rtreeStore)
	if !isRtree {
		return nil
	}
	return db.rtreeSyncShadowsIn(createScope(vm.isTemp), vm.name, st)
}

// rtreeSyncShadowsIn writes %_node, %_rowid and %_parent from the live tree.
// It is the ONLY place r-tree bytes reach the file, and what it writes is the
// tree's own state -- every node's full zData, stale tail bytes included, and
// the two mapping tables -- because the tree IS rtree.c's (rtree_tree.go).
func (db *DB) rtreeSyncShadowsIn(scope schemaScope, name string, st *rtreeStore) error {
	nodeTbl := db.findTableMetaIn(scope, name+"_node")
	rowidTbl := db.findTableMetaIn(scope, name+"_rowid")
	parentTbl := db.findTableMetaIn(scope, name+"_parent")
	if nodeTbl == nil || rowidTbl == nil || parentTbl == nil {
		return fmt.Errorf("engine: rtree table %s is missing a shadow table (%%_node/%%_rowid/%%_parent)", name)
	}
	if st.tree == nil {
		return fmt.Errorf("engine: internal: rtree %s has no tree to write", name)
	}
	for _, t := range []*tableMeta{nodeTbl, rowidTbl, parentTbl} {
		fts3ClearTable(t)
	}
	for no, n := range st.tree.nodes {
		nodeTbl.putRow(uint64(no), []Value{{Typ: Null}, {Typ: Blob, S: append([]byte(nil), n.data...)}})
	}
	for rid, leaf := range st.tree.rowid {
		rec := make([]Value, 2+st.nAux)
		rec[1] = Value{Typ: Int, I: leaf}
		copy(rec[2:], st.aux[rid])
		rowidTbl.putRow(uint64(rid), rec)
	}
	for child, p := range st.tree.parent {
		parentTbl.putRow(uint64(child), []Value{{Typ: Null}, {Typ: Int, I: p}})
	}
	return nil
}

// rtreeLoadStore reads st's tree back out of the three shadow tables,
// verbatim -- the reverse of rtreeSyncShadowsIn, and the path that recovers a
// table REAL SQLITE wrote, since the bytes and the mappings are its own. Every
// node must be the size node 1 is, which is how xConnect derives iNodeSize
// (rtree.c:3588-3597) and what nodeAcquire checks each blob against
// (rtree.c:752-760); the tree is walked once here so a malformed one is
// refused now rather than half-read later.
func rtreeLoadStore(rp *ReadOnlyPager, st *rtreeStore, name string) error {
	rowids, records, err := rp.Rows(name + "_node")
	if err != nil {
		return err
	}
	nDim2 := st.dims * 2
	var t *rtreeTree
	for i, rid := range rowids {
		if len(records[i]) < 2 || records[i][1].Typ != Blob {
			return fmt.Errorf("engine: rtree %s: node %d is not a blob", name, rid)
		}
		if int64(rid) == 1 {
			t = &rtreeTree{nodeSize: len(records[i][1].S), nDim2: nDim2, nBytesPerCell: rtreeBytesPerCell(nDim2), i32: st.i32,
				nodes: map[int64]*rtreeTNode{}, parent: map[int64]int64{}, rowid: map[int64]int64{}}
		}
	}
	// A missing node 1, or one under 448 bytes, is unreadable to C whether or
	// not its connection holds the table: connected, nodeAcquire's blob_open
	// fails or the size differs from the page-derived one (SQLITE_CORRUPT_VTAB,
	// rtree.c:757-766); connecting, getNodeSize refuses it as "undersize RTree
	// blobs" (rtree.c:3595). createShadowTables writes node 1 with the table,
	// as xCreate does, so neither is a table this engine is still building.
	if t == nil || t.nodeSize < rtreeMinNodeSize {
		return fmt.Errorf("engine: rtree %s: %w", name, errRtreeCorrupt)
	}
	for i, rid := range rowids {
		blob := records[i][1].S
		if len(blob) != t.nodeSize {
			return fmt.Errorf("engine: rtree %s: node %d is %d bytes where node 1 is %d: %w", name, rid, len(blob), t.nodeSize, errRtreeCorrupt)
		}
		t.nodes[int64(rid)] = &rtreeTNode{no: int64(rid), data: append([]byte(nil), blob...)}
	}
	for _, tbl := range []struct {
		suffix string
		into   map[int64]int64
	}{{"_rowid", t.rowid}, {"_parent", t.parent}} {
		rids, recs, rerr := rp.Rows(name + tbl.suffix)
		if rerr != nil {
			return rerr
		}
		for i, rid := range rids {
			if len(recs[i]) > 1 && recs[i][1].Typ == Int {
				tbl.into[int64(rid)] = recs[i][1].I
			}
			if tbl.suffix == "_rowid" && st.nAux > 0 && len(recs[i]) > 2 {
				if st.aux == nil {
					st.aux = map[int64][]Value{}
				}
				a := make([]Value, st.nAux)
				copy(a, recs[i][2:])
				st.aux[int64(rid)] = a
			}
		}
	}
	t.depth = int(binary.BigEndian.Uint16(t.nodes[1].data[0:2]))
	if werr := t.walk(func(rtreeTCell) {}); werr != nil {
		return fmt.Errorf("engine: rtree %s: %w", name, werr)
	}
	st.tree = t
	return nil
}

// rtreeStoredCoord is how a decoded coordinate is presented as a column
// value: an rtree_i32 table's coordinates are INTEGERs and an ordinary
// rtree's are REALs, which is what the module's own coerceCoords stores and
// what "SELECT typeof(x0)" reports.
func rtreeStoredCoord(v float64, i32 bool) Value {
	if i32 {
		return Value{Typ: Int, I: int64(v)}
	}
	return Value{Typ: Float, F: v}
}

// materializeRtree is the READ path's entry: it rebuilds a throwaway store
// from the shadow tables and presents its rows, the same shape
// materializeFts5 takes. A writable vtab's rows have to come from the FILE
// (not from any pager-local state) so they are visible across the driver's
// per-statement engine sessions.
func (p *ReadOnlyPager) materializeRtree(m rtreeModule, args []string, name string, cols []columnInfo) ([]columnInfo, [][]Value, []int64, error) {
	_, vt, err := m.Connect(args)
	if err != nil {
		return nil, nil, nil, err
	}
	st, ok := vt.(*rtreeStore)
	if !ok {
		return nil, nil, nil, fmt.Errorf("engine: rtree table %s has no backing store", name)
	}
	if lerr := rtreeLoadStore(p, st, name); lerr != nil {
		nCol := 0
		for _, c := range cols {
			if !c.Hidden {
				nCol++
			}
		}
		return nil, nil, nil, &rtreeLoadError{nCol: nCol, i32: st.i32, node1: rtreeNode1Len(p, name), err: lerr}
	}
	rowids, rows := st.vtabRows()
	return cols, rows, rowids, nil
}
