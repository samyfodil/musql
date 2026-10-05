package replication

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/samyfodil/musql/engine"
)

// Package replication manages replicated schema. Schema changes travel as ops
// (OpSchema) in the same log as row changes. IDs are content-addressed (hash of
// name, CREATE text, and retirement count), so two nodes running the same
// migration create the same table and their rows merge.

// OpSchema is a schema change. Its one cell ("schema") is a schemaOp in JSON.
const OpSchema OpKind = 3

const schemaCell = "schema"

// The schema actions a schemaOp carries.
const (
	actCreate    = "create"
	actDrop      = "drop"
	actRename    = "rename"
	actAddCol    = "addcol"
	actRenameCol = "renamecol"
	actDropCol   = "dropcol"
	actMode      = "mode"
	actRetire    = "retire"
)

// schemaOp is one resolved schema change, identified by ID, not name.
type schemaOp struct {
	Act  string `json:"act"`
	ID   string `json:"id"`
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
	SQL  string `json:"sql,omitempty"`
	Tbl  string `json:"tbl,omitempty"`
	Col  string `json:"col,omitempty"`
	// Quote is a renamecol's quoting of its new name.
	Quote bool `json:"quote,omitempty"`
}

func (o schemaOp) cells() []Cell {
	b, _ := json.Marshal(o) // a struct of strings cannot fail to marshal
	return []Cell{{Col: schemaCell, Type: TypeBlob, Val: b}}
}

func decodeSchemaOp(op Op) (schemaOp, error) {
	var so schemaOp
	for _, c := range op.Cells {
		if c.Col == schemaCell {
			return so, json.Unmarshal(c.Val, &so)
		}
	}
	return so, fmt.Errorf("replication: schema op %s/%d has no %q cell", op.Site, op.Seq, schemaCell)
}

type column struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	PK      bool   `json:"pk,omitempty"`
	NotNull bool   `json:"nn,omitempty"`
	Default bool   `json:"df,omitempty"`
	// Gen marks a generated column: never a cell, recomputed on every node
	// from the merged row (C requires the expression to be deterministic,
	// resolve.c:1219-1228).
	Gen bool `json:"gen,omitempty"`
}

type object struct {
	ID   string   `json:"id"`
	Type string   `json:"type"` // table, index, view
	Name string   `json:"name"`
	Tbl  string   `json:"tbl,omitempty"` // an index's table
	SQL  string   `json:"sql"`
	Cols []column `json:"cols,omitempty"`
}

// catalog is the schema a set of schema ops reduces to.
type catalog struct {
	Objs    []*object       `json:"objs"` // live, in creation order
	Retired map[string]bool `json:"retired,omitempty"`
	// Gen counts how often a name was retired (dropped or renamed away): part of
	// the ID of whatever takes the name next. Keyed by genKey.
	Gen map[string]int `json:"gen,omitempty"`
	// Mode is the database's replication mode: the first mode op in canonical
	// order sets it, and later ones are no-ops.
	Mode string `json:"mode,omitempty"`
	// Gone is the sites Retire took out of the members pruning waits for.
	Gone map[string]bool `json:"gone,omitempty"`
}

func newCatalog() *catalog { return &catalog{Retired: map[string]bool{}, Gen: map[string]int{}} }

// fold is SQLite's identifier comparison: ASCII case folding only
// (sqlite3StrICmp, util.c).
func fold(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// contentID hashes a versioned, length-prefixed tuple.
func contentID(parts ...string) string {
	h := sha256.New()
	h.Write([]byte("crdt-id-v1"))
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func objGenKey(name string) string        { return "o\x00" + fold(name) }
func colGenKey(tblID, name string) string { return tblID + "\x00" + fold(name) }
func objectID(typ, name, sql string, gen int) string {
	return contentID("obj", typ, fold(name), sql, strconv.Itoa(gen))
}

// A table's first columns are part of its CREATE text, which its ID already
// hashes; a column added later hashes its definition and generation too, so two
// different definitions of one name never merge.
func initialColID(tblID, name string) string { return contentID("col", tblID, fold(name)) }
func addedColID(tblID, name, def string, gen int) string {
	return contentID("addcol", tblID, fold(name), def, strconv.Itoa(gen))
}

func (c *catalog) byName(name string) *object {
	f := fold(name)
	for _, o := range c.Objs {
		if fold(o.Name) == f {
			return o
		}
	}
	return nil
}

func (c *catalog) byID(id string) *object {
	for _, o := range c.Objs {
		if o.ID == id {
			return o
		}
	}
	return nil
}

func (o *object) col(id string) *column {
	for i := range o.Cols {
		if o.Cols[i].ID == id {
			return &o.Cols[i]
		}
	}
	return nil
}

func (o *object) colByName(name string) *column {
	f := fold(name)
	for i := range o.Cols {
		if fold(o.Cols[i].Name) == f {
			return &o.Cols[i]
		}
	}
	return nil
}

// keyCols is the table's primary key columns, or nil when its rowid is its
// identity. They come in column order, not key order: either is the same on
// every node (Cols is read off the reducer's scratch), and an identity only
// needs one.
func (o *object) keyCols() []*column {
	var out []*column
	for i := range o.Cols {
		if o.Cols[i].PK {
			out = append(out, &o.Cols[i])
		}
	}
	return out
}

func (c *catalog) clone() *catalog {
	b, _ := json.Marshal(c)
	out := newCatalog()
	json.Unmarshal(b, out) // round-trips its own encoding
	return out
}

func (c *catalog) retire(id string) {
	c.Retired[id] = true
	for i, o := range c.Objs {
		if o.ID == id {
			c.Objs = append(c.Objs[:i], c.Objs[i+1:]...)
			return
		}
	}
}

// liveEqual reports whether two catalogs hold the same live objects: IDs,
// names, catalog text and column identities. Order is compared too, since it
// is the order a rebuild recreates them in.
func (c *catalog) liveEqual(d *catalog) bool {
	a, _ := json.Marshal(c.Objs)
	b, _ := json.Marshal(d.Objs)
	g, _ := json.Marshal(c.Gone)
	h, _ := json.Marshal(d.Gone)
	return string(a) == string(b) && c.Mode == d.Mode && string(g) == string(h)
}

// --- the scratch database the reducer runs on ---

// scratch is an empty in-memory database holding only a schema. One
// connection: every connection to ":memory:" is a database of its own.
type scratch struct{ db *sql.DB }

func newScratch() (*scratch, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &scratch{db: db}, nil
}

func (sc *scratch) close() { sc.db.Close() }

// catRow is one row of a catalog: sqlite_master's.
type catRow struct{ Type, Name, Tbl, SQL string }

// snapshot is main's catalog, user objects only, sorted by name -- the form two
// catalogs are compared in (a real database's through catalogRows).
func (sc *scratch) snapshot(ctx context.Context) ([]catRow, error) {
	rows, err := sc.db.QueryContext(ctx, catalogQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []catRow
	for rows.Next() {
		var r catRow
		var q sql.NullString
		if err := rows.Scan(&r.Type, &r.Name, &r.Tbl, &q); err != nil {
			return nil, err
		}
		r.SQL = q.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// catalogQuery lists main's user objects: not the engine's own, not this
// package's, and an automatic index goes with its table.
const catalogQuery = `SELECT type, name, tbl_name, sql FROM main.sqlite_master
	WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'
	AND name NOT IN ('_repl_meta', '_repl_oplog', '_repl_clock', '_repl_ranges', '_repl_withheld', '_repl_floors')
	ORDER BY name`

// tableCols reads a table's columns off the scratch -- table_xinfo, since
// table_info leaves generated columns out.
func (sc *scratch) tableCols(ctx context.Context, name string) ([]column, error) {
	rows, err := sc.db.QueryContext(ctx, `SELECT name, "notnull", dflt_value IS NOT NULL, pk, hidden FROM pragma_table_xinfo(?)`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []column
	for rows.Next() {
		var c column
		var pk, hidden int
		if err := rows.Scan(&c.Name, &c.NotNull, &c.Default, &pk, &hidden); err != nil {
			return nil, err
		}
		c.PK = pk > 0
		c.Gen = hidden == 2 || hidden == 3
		out = append(out, c)
	}
	return out, rows.Err()
}

func q(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// --- the reducer ---

// replaySchema reduces schema ops, already in canonical order, to a catalog,
// and returns the scratch database holding it (the caller closes it).
func replaySchema(ctx context.Context, ops []Op) (*catalog, *scratch, error) {
	sc, err := newScratch()
	if err != nil {
		return nil, nil, err
	}
	c := newCatalog()
	for _, op := range ops {
		so, derr := decodeSchemaOp(op)
		if derr != nil {
			sc.close()
			return nil, nil, derr
		}
		if _, aerr := c.apply(ctx, sc, so); aerr != nil {
			sc.close()
			return nil, nil, aerr
		}
	}
	return c, sc, nil
}

// sortSchemaOps puts schema ops in canonical order: (HLC, site, seq).
func sortSchemaOps(ops []Op) {
	sort.Slice(ops, func(i, j int) bool {
		a, b := ops[i], ops[j]
		if a.HLC != b.HLC {
			return a.HLC < b.HLC
		}
		if a.Site != b.Site {
			return a.Site < b.Site
		}
		return a.Seq < b.Seq
	})
}

// apply reduces one op, running on sc the SQL it means there, and returns that
// SQL -- empty when the op is a no-op here, which is a deterministic outcome
// (its target is gone, its name is taken, the scratch refused it) and the same
// on every node. An error is an infrastructure failure, never an SQL refusal.
func (c *catalog) apply(ctx context.Context, sc *scratch, o schemaOp) (string, error) {
	var stmt string
	var tbl *object
	switch o.Act {
	case actMode:
		if c.Mode == "" {
			c.Mode = o.Name
		}
		return "", nil // nothing to run: a mode is not a schema object
	case actRetire:
		if c.Gone == nil {
			c.Gone = map[string]bool{}
		}
		c.Gone[o.Name] = true
		return "", nil
	case actCreate:
		if c.Retired[o.ID] || c.byID(o.ID) != nil || c.byName(o.Name) != nil {
			return "", nil // retired, already here (the same migration twice), or the name is taken
		}
		if o.Type == "index" && c.byID(o.Tbl) == nil {
			return "", nil
		}
		stmt = o.SQL
	case actDrop:
		obj := c.byID(o.ID)
		if obj == nil {
			return "", nil
		}
		stmt = fmt.Sprintf("DROP %s main.%s", strings.ToUpper(obj.Type), q(obj.Name))
	case actRename:
		if tbl = c.byID(o.ID); tbl == nil || tbl.Type != "table" || c.byName(o.Name) != nil {
			return "", nil
		}
		stmt = fmt.Sprintf("ALTER TABLE main.%s RENAME TO %s", q(tbl.Name), q(o.Name))
	case actAddCol, actRenameCol, actDropCol:
		if tbl = c.byID(o.ID); tbl == nil || tbl.Type != "table" {
			return "", nil
		}
		col := tbl.col(o.Col)
		switch o.Act {
		case actAddCol:
			if col != nil || c.Retired[o.Col] {
				return "", nil
			}
			stmt = fmt.Sprintf("ALTER TABLE main.%s ADD COLUMN %s", q(tbl.Name), o.SQL)
		case actRenameCol:
			if col == nil || tbl.colByName(o.Name) != nil {
				return "", nil
			}
			to := o.Name
			if o.Quote {
				to = q(o.Name)
			}
			stmt = fmt.Sprintf("ALTER TABLE main.%s RENAME COLUMN %s TO %s", q(tbl.Name), q(col.Name), to)
		default:
			if col == nil {
				return "", nil
			}
			stmt = fmt.Sprintf("ALTER TABLE main.%s DROP COLUMN %s", q(tbl.Name), q(col.Name))
		}
	default:
		return "", fmt.Errorf("replication: unknown schema action %q (a newer crdt wrote it)", o.Act)
	}
	if _, err := sc.db.ExecContext(ctx, stmt); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		return "", nil // refused here, so refused everywhere
	}
	switch o.Act {
	case actCreate:
		if o.Type == "index" {
			// It must index the table the origin meant: its text names the table,
			// and that name may belong to another table here.
			var on string
			if err := sc.db.QueryRowContext(ctx, `SELECT tbl_name FROM main.sqlite_master WHERE name=?`, o.Name).Scan(&on); err != nil {
				return "", err
			}
			if fold(on) != fold(c.byID(o.Tbl).Name) {
				if _, err := sc.db.ExecContext(ctx, "DROP INDEX main."+q(o.Name)); err != nil {
					return "", err
				}
				return "", nil
			}
		}
		obj := &object{ID: o.ID, Type: o.Type, Name: o.Name, Tbl: o.Tbl}
		if o.Type == "table" {
			cols, err := sc.tableCols(ctx, o.Name)
			if err != nil {
				return "", err
			}
			for i := range cols {
				cols[i].ID = initialColID(o.ID, cols[i].Name)
			}
			obj.Cols = cols
		}
		c.Objs = append(c.Objs, obj)
	case actDrop:
		c.Gen[objGenKey(c.byID(o.ID).Name)]++
		c.retire(o.ID)
	case actRename:
		c.Gen[objGenKey(tbl.Name)]++
		tbl.Name = o.Name
	case actAddCol:
		cols, err := sc.tableCols(ctx, tbl.Name)
		if err != nil {
			return "", err
		}
		nc := cols[len(cols)-1]
		nc.ID = o.Col
		tbl.Cols = append(tbl.Cols, nc)
	case actRenameCol:
		col := tbl.col(o.Col)
		c.Gen[colGenKey(tbl.ID, col.Name)]++
		col.Name = o.Name
	case actDropCol:
		col := tbl.col(o.Col)
		c.Gen[colGenKey(tbl.ID, col.Name)]++
		c.Retired[o.Col] = true
		for i := range tbl.Cols {
			if tbl.Cols[i].ID == o.Col {
				tbl.Cols = append(tbl.Cols[:i], tbl.Cols[i+1:]...)
				break
			}
		}
	}
	return stmt, c.refresh(ctx, sc)
}

// refresh re-reads every live object's catalog text: a rename rewrites the
// indexes and views that name it, and a DROP TABLE takes its indexes along,
// which retires them.
func (c *catalog) refresh(ctx context.Context, sc *scratch) error {
	snap, err := sc.snapshot(ctx)
	if err != nil {
		return err
	}
	byName := map[string]catRow{}
	for _, r := range snap {
		byName[fold(r.Name)] = r
	}
	for _, o := range append([]*object(nil), c.Objs...) {
		r, ok := byName[fold(o.Name)]
		if !ok {
			c.Gen[objGenKey(o.Name)]++
			c.retire(o.ID)
			continue
		}
		o.SQL = r.SQL
	}
	return nil
}

// --- turning a local DDL statement into ops ---

// errSchemaUnsupported marks a schema change this package does not replicate.
var errSchemaUnsupported = errors.New("replication: schema change not replicated")

// errNotOnScratch is classify's "the statement does not run on the scratch".
var errNotOnScratch = errors.New("replication: statement does not run on the replicated schema")

// classify runs a DDL statement that just succeeded on the local database
// against w -- a scratch holding c's schema -- and resolves what it did into
// schema ops, reading the change off w's catalog before and after. One DDL
// statement makes one change, so the before/after difference names it: an
// object appeared, objects vanished, a table's name changed, or a table's column
// list gained, lost or renamed one. Nothing changed (IF NOT EXISTS, a TEMP
// object) is no op at all. c is only read.
func (c *catalog) classify(ctx context.Context, w *scratch, stmt string) ([]schemaOp, error) {
	before, err := w.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := w.db.ExecContext(ctx, stmt); err != nil {
		// It names something the scratch does not have -- a TEMP object from an
		// earlier transaction, most likely. Not an op; the caller checks main's
		// real catalog against the replicated one, which is what decides.
		return nil, errNotOnScratch
	}
	after, err := w.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	prev := map[string]catRow{}
	for _, r := range before {
		prev[fold(r.Name)] = r
	}
	next := map[string]catRow{}
	for _, r := range after {
		next[fold(r.Name)] = r
	}
	var added, removed []catRow
	for k, r := range next {
		if _, ok := prev[k]; !ok {
			added = append(added, r)
		}
	}
	for k, r := range prev {
		if _, ok := next[k]; !ok {
			removed = append(removed, r)
		}
	}
	refuse := func(why string) ([]schemaOp, error) {
		return nil, fmt.Errorf("%w: %s: %s", errSchemaUnsupported, stmt, why)
	}
	switch {
	case len(added) == 1 && len(removed) == 0:
		r := added[0]
		if err := supportedObject(ctx, w.db, r); err != nil {
			return refuse(err.Error())
		}
		o := schemaOp{Act: actCreate, Type: r.Type, Name: r.Name, SQL: r.SQL}
		o.ID = objectID(r.Type, r.Name, r.SQL, c.Gen[objGenKey(r.Name)])
		if r.Type == "index" {
			t := c.byName(r.Tbl)
			if t == nil {
				return refuse("its table is not synced")
			}
			o.Tbl = t.ID
		}
		return []schemaOp{o}, nil
	case len(added) == 0 && len(removed) > 0:
		// One statement drops one object -- and a table's indexes with it.
		primary := removed[0]
		for _, r := range removed {
			if r.Type == "table" {
				primary = r
			}
		}
		obj := c.byName(primary.Name)
		if obj == nil {
			return refuse("it drops an object this package does not know")
		}
		return []schemaOp{{Act: actDrop, ID: obj.ID}}, nil
	case len(added) == 1 && len(removed) == 1 && added[0].Type == "table" && removed[0].Type == "table":
		obj := c.byName(removed[0].Name)
		if obj == nil {
			return refuse("it renames a table this package does not know")
		}
		return []schemaOp{{Act: actRename, ID: obj.ID, Name: added[0].Name}}, nil
	case len(added) != 0 || len(removed) != 0:
		return refuse("it changes several objects at once")
	}
	// The same objects: a column change, or nothing a replica could see.
	for _, r := range after {
		if r.Type != "table" || prev[fold(r.Name)].SQL == r.SQL {
			continue
		}
		obj := c.byName(r.Name)
		if obj == nil {
			return refuse("it alters a table this package does not know")
		}
		cols, err := w.tableCols(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		was := obj.Cols
		switch {
		case len(cols) == len(was)+1:
			def, ok := engine.AlterAddColumnDef(stmt)
			if !ok {
				return refuse("a column appeared that no ADD COLUMN added")
			}
			if err := supportedObject(ctx, w.db, r); err != nil {
				return refuse(err.Error())
			}
			name := cols[len(cols)-1].Name
			id := addedColID(obj.ID, name, def, c.Gen[colGenKey(obj.ID, name)])
			return []schemaOp{{Act: actAddCol, ID: obj.ID, Col: id, SQL: def}}, nil
		case len(cols) == len(was)-1:
			for _, old := range was {
				if colIndex(cols, old.Name) < 0 {
					return []schemaOp{{Act: actDropCol, ID: obj.ID, Col: old.ID}}, nil
				}
			}
		case len(cols) == len(was):
			var ops []schemaOp
			quote, isRename := engine.AlterRenameColumnQuoted(stmt)
			for i := range cols {
				if cols[i].Name != was[i].Name {
					ops = append(ops, schemaOp{Act: actRenameCol, ID: obj.ID, Col: was[i].ID, Name: cols[i].Name, Quote: quote})
				}
			}
			if len(ops) == 1 && isRename {
				return ops, nil
			}
		}
		return refuse("it changes a table's definition in a way that is not replicated")
	}
	for _, r := range after {
		if prev[fold(r.Name)].SQL != r.SQL {
			return nil, nil // a rename's rewrite of an index or view: no op of its own
		}
	}
	return nil, nil
}

func colIndex(cols []column, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// supportedObject refuses what cannot be replicated at all, naming the reason.
// Constraints are not on the list: a node checks them on its own writes, and
// applies a peer's rows without them (CHECK skipped, foreign keys off, a UNIQUE
// conflict settled by Store.rebuildTable) -- so nodes converge, and a merged
// row may break a rule each write kept, which is the eventual consistency the
// caller chose. What is left:
//
//   - a trigger: its effects are not captured (engine/rowhook.go records only
//     the firing statement's rows) and it would fire again on every peer;
//   - a virtual table: its storage is a module's, not rows this package sees;
//   - WITHOUT ROWID, a key column not collated BINARY: the key is the row's
//     identity, and these make it not one.
func supportedObject(ctx context.Context, db dbtx, r catRow) error {
	switch {
	case r.Type == "trigger":
		return errors.New("triggers are not replicated")
	case r.Type == "table" && len(r.SQL) >= 20 && strings.EqualFold(r.SQL[:20], "CREATE VIRTUAL TABLE"):
		return errors.New("virtual tables are not replicated")
	case r.Type != "table":
		return nil
	}
	if sqlWords(r.SQL)["WITHOUT"] {
		return fmt.Errorf("table %q: WITHOUT ROWID is not replicated", r.Name)
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_index_list(?) WHERE origin = 'pk'`, r.Name)
	if err != nil {
		return err
	}
	var pkIndex string
	for rows.Next() {
		if err := rows.Scan(&pkIndex); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if pkIndex == "" {
		return nil
	}
	// Every key column: a composite key's second column collated NOCASE makes
	// 'a' and 'A' one row.
	rows, err = db.QueryContext(ctx, `SELECT coll FROM pragma_index_xinfo(?) WHERE key=1`, pkIndex)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var coll string
		if err := rows.Scan(&coll); err != nil {
			return err
		}
		if !strings.EqualFold(coll, "BINARY") {
			return fmt.Errorf("table %q: a primary key collated %s is not replicated (only BINARY)", r.Name, coll)
		}
	}
	return rows.Err()
}

// sqlWords is the set of bare words in a statement, upper-cased, skipping
// string literals and quoted identifiers -- enough to find a keyword without
// being fooled by a column named "check_x" or a default of 'UNIQUE'.
func sqlWords(stmt string) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < len(stmt); {
		ch := stmt[i]
		switch {
		case ch == '\'' || ch == '"' || ch == '`' || ch == '[':
			end := ch
			if ch == '[' {
				end = ']'
			}
			i++
			for i < len(stmt) {
				if stmt[i] == end {
					if end != ']' && i+1 < len(stmt) && stmt[i+1] == end {
						i += 2 // a doubled quote is the quote itself
						continue
					}
					break
				}
				i++
			}
			i++
		case ch == '_' || ch >= 0x80 || 'a' <= ch|0x20 && ch|0x20 <= 'z':
			j := i
			for j < len(stmt) && (stmt[j] == '_' || stmt[j] >= 0x80 || 'a' <= stmt[j]|0x20 && stmt[j]|0x20 <= 'z' || '0' <= stmt[j] && stmt[j] <= '9') {
				j++
			}
			out[strings.ToUpper(stmt[i:j])] = true
			i = j
		default:
			i++
		}
	}
	return out
}
