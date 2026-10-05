package engine

import "sync"

// RtreeConnections holds per-connection rtree state.
// holds open, the one piece of rtree.c's per-connection Rtree object
// (rtree.c:283) that changes an answer: the node size. xCreate takes it from
// the page size (getNodeSize, rtree.c:3575-3586) and keeps it for as long as
// the connection's schema stays loaded; any other connection reads it off node
// 1 at xConnect and refuses a node 1 under 448 bytes, or a missing one, as
// "undersize RTree blobs" (rtree.c:3588-3600). The two agree on every sound
// tree. On a damaged %_node they do not: with node 1 gone or short, the
// creating connection's rtreecheck() reports what it finds (rtreecheck.test
// 3.2/3.3, rtreeA.test 1.1.1) while a fresh connection's fails to prepare.
//
// So this records the r-trees CREATEd on the connection, each with the schema
// cookie its CREATE left behind, and a record counts only while the cookie is
// unchanged: every DDL (ALTER's reload included), VACUUM and another
// connection's schema change all move it, and a schema reset they cause
// disconnects the table in C. The resets that leave the cookie where it was --
// a ROLLBACK over a schema change (main.c:1525, vdbe.c:3958) and
// "PRAGMA writable_schema=RESET" (pragma.c:1182) -- clear it outright. An
// r-tree this connection only CONNECTED to is not recorded: whether C still
// holds it depends on every statement since, and the callers decline instead.
type RtreeConnections struct {
	mu      sync.Mutex
	created map[string]uint32 // folded "schema\x00name" -> cookie after the CREATE
}

// NewRtreeConnections returns an empty registry for one connection.
func NewRtreeConnections() *RtreeConnections { return &RtreeConnections{} }

func rtreeConnKey(schema, name string) string {
	return r33sFoldIdent(schema) + "\x00" + r33sFoldIdent(name)
}

func (r *RtreeConnections) noteCreated(schema, name string, cookie uint32) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.created == nil {
		r.created = map[string]uint32{}
	}
	r.created[rtreeConnKey(schema, name)] = cookie
}

// reset is sqlite3ResetAllSchemasOfConnection's effect on the registry.
func (r *RtreeConnections) reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created = nil
}

// createdAt reports whether schema.name was CREATEd on this connection and
// nothing has moved its schema cookie since.
func (r *RtreeConnections) createdAt(schema, name string, cookie uint32) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.created[rtreeConnKey(schema, name)]
	return ok && c == cookie
}

// SetRtreeConnections gives this session its connection's registry. A driver
// that runs each statement on its own session (driver) passes the same one
// every time; without one, a session starts its own at its first CREATE.
func (db *DB) SetRtreeConnections(r *RtreeConnections) { db.rtreeConns = r }

// SetRtreeConnections is the read snapshot's counterpart.
func (p *ReadOnlyPager) SetRtreeConnections(r *RtreeConnections) { p.rtreeConns = r }
