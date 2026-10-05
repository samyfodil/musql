package replication

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The installed catalog is kept in _repl_meta with its hash version, written
// together with schema ops and tables so the three never disagree.
const (
	metaCatalog   = "catalog"
	metaSchemaVer = "schema_ver"
)

func loadCatalog(ctx context.Context, q dbtx) (*catalog, string, bool, error) {
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT v FROM main._repl_meta WHERE k=?`, metaCatalog).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return newCatalog(), "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	c, err := decodeCatalog(b)
	return c, catalogVersion(b), err == nil, err
}

func decodeCatalog(b []byte) (*catalog, error) {
	c := newCatalog()
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("replication: the installed catalog does not decode: %w", err)
	}
	return c, nil
}

func catalogVersion(b []byte) string { return contentID("catalog", string(b)) }

// saveCatalog writes c as the installed catalog and returns its version.
func saveCatalog(ctx context.Context, exec func(ctx context.Context, query string, args ...any) error, c *catalog) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	ver := catalogVersion(b)
	for k, v := range map[string]any{metaCatalog: b, metaSchemaVer: ver} {
		if err := exec(ctx, `INSERT INTO main._repl_meta(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v); err != nil {
			return "", err
		}
	}
	return ver, nil
}

func (s *Store) execQ(ctx context.Context, query string, args ...any) error {
	_, err := s.q.ExecContext(ctx, query, args...)
	return err
}

// catalogRows is a real database's catalog in scratch.snapshot's form.
func catalogRows(ctx context.Context, q dbtx) ([]catRow, error) {
	rows, err := q.QueryContext(ctx, catalogQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []catRow
	for rows.Next() {
		var r catRow
		var sqlText sql.NullString
		if err := rows.Scan(&r.Type, &r.Name, &r.Tbl, &sqlText); err != nil {
			return nil, err
		}
		r.SQL = sqlText.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// sameCatalog reports the first difference between two catalogs, or "".
func sameCatalog(got, want []catRow) string {
	m := map[string]catRow{}
	for _, r := range want {
		m[fold(r.Name)] = r
	}
	for _, r := range got {
		w, ok := m[fold(r.Name)]
		if !ok {
			return fmt.Sprintf("%s %q is not in the replicated schema", r.Type, r.Name)
		}
		if w != r {
			return fmt.Sprintf("%s %q is %q here and %q replicated", r.Type, r.Name, r.SQL, w.SQL)
		}
		delete(m, fold(r.Name))
	}
	for _, r := range m {
		return fmt.Sprintf("%s %q is missing here", r.Type, r.Name)
	}
	return ""
}

// schemaOpsFrom reads every schema op in the log, in canonical order.
func schemaOpsFrom(rows [][]any) ([]Op, error) {
	ops := make([]Op, 0, len(rows))
	for _, r := range rows {
		op := Op{Site: asString(r[0]), Seq: uint64(r[1].(int64)), HLC: HLC(r[2].(int64)), Kind: OpSchema}
		cells, err := decodeCells(asBytes(r[3]))
		if err != nil {
			return nil, err
		}
		op.Cells = cells
		ops = append(ops, op)
	}
	sortSchemaOps(ops)
	return ops, nil
}

const schemaOpsQuery = `SELECT site, seq, hlc, cells FROM main._repl_oplog WHERE op=3`

func asString(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	s, _ := v.(string)
	return s
}

func asBytes(v any) []byte {
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	b, _ := v.([]byte)
	return b
}

func (s *Store) schemaOps(ctx context.Context) ([]Op, error) {
	rows, err := s.q.QueryContext(ctx, schemaOpsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var raw [][]any
	for rows.Next() {
		var site string
		var seq, hlc int64
		var cells []byte
		if err := rows.Scan(&site, &seq, &hlc, &cells); err != nil {
			return nil, err
		}
		raw = append(raw, []any{site, seq, hlc, cells})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return schemaOpsFrom(raw)
}

// applySchema installs the schema the log now reduces to, op being the schema
// op just appended to it. Returns the new catalog and its version.
//
// When op is the newest schema op and the installed schema is exactly what the
// log reduced to before it -- nearly always -- the statement the reducer ran
// for it runs here too, and only the rows it unlocks are materialized. When it
// arrived LATE, an earlier op landed after later ones, and the tables whose
// identity or definition that changed are rebuilt from the clock (reconcile).
// Either way the result is checked against the reducer's catalog text.
func (s *Store) applySchema(ctx context.Context, op Op) (*catalog, string, error) {
	ops, err := s.schemaOps(ctx)
	if err != nil {
		return nil, "", err
	}
	last := len(ops) - 1
	isLast := ops[last].Site == op.Site && ops[last].Seq == op.Seq
	target, sc, err := replaySchema(ctx, ops[:last])
	if err != nil {
		return nil, "", err
	}
	defer sc.close()
	before := target.clone()
	so, err := decodeSchemaOp(ops[last])
	if err != nil {
		return nil, "", err
	}
	stmt, err := target.apply(ctx, sc, so)
	if err != nil {
		return nil, "", err
	}
	if isLast && s.cat.liveEqual(before) {
		if stmt != "" {
			if err := s.execQ(ctx, stmt); err != nil {
				return nil, "", fmt.Errorf("replication: applying %s: %w", stmt, err)
			}
			switch so.Act {
			case actCreate:
				if so.Type == "table" {
					err = s.rebuildTable(ctx, target.byID(so.ID))
				}
			case actAddCol:
				err = s.rebuildTable(ctx, target.byID(so.ID))
			}
			if err != nil {
				return nil, "", err
			}
		}
	} else if err := s.reconcile(ctx, s.cat, target); err != nil {
		return nil, "", err
	}
	if err := s.checkInstalled(ctx, sc); err != nil {
		return nil, "", err
	}
	ver, err := saveCatalog(ctx, s.execQ, target)
	return target, ver, err
}

// checkInstalled compares the real catalog with the reducer's scratch.
func (s *Store) checkInstalled(ctx context.Context, sc *scratch) error {
	got, err := catalogRows(ctx, s.q)
	if err != nil {
		return err
	}
	want, err := sc.snapshot(ctx)
	if err != nil {
		return err
	}
	if d := sameCatalog(got, want); d != "" {
		return fmt.Errorf("replication: the installed schema does not match the replicated one: %s", d)
	}
	return nil
}

func sameObject(a, b *object) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// reconcile turns the user tables from schema from into schema to: every
// object that is not the same in both -- identity, name, text, columns -- is
// dropped, and everything to has that is not left standing is created, a
// table's rows rebuilt from the clock (never from the rows it had: those may
// belong to an incarnation that just lost its name).
func (s *Store) reconcile(ctx context.Context, from, to *catalog) error {
	rebuilt := map[string]bool{}
	keep := map[string]bool{}
	for _, o := range from.Objs {
		if t := to.byID(o.ID); t != nil && sameObject(o, t) {
			keep[o.ID] = true
		} else if o.Type == "table" {
			rebuilt[o.ID] = true
		}
	}
	for _, o := range from.Objs {
		if o.Type == "index" && rebuilt[o.Tbl] {
			delete(keep, o.ID) // it goes with its table
		}
	}
	// Views and indexes first, then tables; IF EXISTS since a table's DROP
	// already took its indexes.
	for _, pass := range []func(*object) bool{
		func(o *object) bool { return o.Type != "table" },
		func(o *object) bool { return o.Type == "table" },
	} {
		for _, o := range from.Objs {
			if keep[o.ID] || !pass(o) {
				continue
			}
			if err := s.execQ(ctx, fmt.Sprintf("DROP %s IF EXISTS main.%s", strings.ToUpper(o.Type), q(o.Name))); err != nil {
				return err
			}
		}
	}
	for _, o := range to.Objs {
		if keep[o.ID] {
			continue
		}
		if err := s.execQ(ctx, o.SQL); err != nil {
			return fmt.Errorf("replication: recreating %s %q: %w", o.Type, o.Name, err)
		}
		if o.Type == "table" {
			if err := s.rebuildTable(ctx, o); err != nil {
				return err
			}
		}
	}
	return nil
}

// resync brings the user tables to what the log reduces to, if they are not
// there -- the check Open runs, for a process that stopped between a schema
// op's durability and anything that depends on it.
func (s *Store) resync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops, err := s.schemaOps(ctx)
	if err != nil || len(ops) == 0 {
		return err
	}
	target, sc, err := replaySchema(ctx, ops)
	if err != nil {
		return err
	}
	defer sc.close()
	if target.liveEqual(s.cat) {
		return nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	s.q = tx
	ver, err := func() (string, error) {
		if err := s.reconcile(ctx, s.cat, target); err != nil {
			return "", err
		}
		if err := s.checkInstalled(ctx, sc); err != nil {
			return "", err
		}
		return saveCatalog(ctx, s.execQ, target)
	}()
	s.q = s.db
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.cat, s.ver = target, ver
	return nil
}

// recordMode puts mode in the replicated catalog if it has none yet -- a schema
// op of this site, so every node gets it -- and refuses a database that already
// replicates in another one.
func (s *Store) recordMode(ctx context.Context, mode string) error {
	s.mu.Lock()
	have := s.cat.Mode
	s.mu.Unlock()
	if have == "" {
		if err := s.localSchemaOp(ctx, schemaOp{Act: actMode, Name: mode}); err != nil {
			return err
		}
		s.mu.Lock()
		have = s.cat.Mode
		s.mu.Unlock()
	}
	if have != mode {
		return fmt.Errorf("%w: this database replicates in %s mode, not %s", ErrMode, have, mode)
	}
	return nil
}

// localSchemaOp makes so a schema op of this site -- one no SQL statement
// expresses (a mode, a retirement) -- and applies it.
func (s *Store) localSchemaOp(ctx context.Context, so schemaOp) error {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, maxSeqQuery, s.site, s.site).Scan(&seq); err != nil {
		return err
	}
	return s.Ingest(ctx, Op{Site: s.site, Seq: uint64(seq.Int64) + 1, HLC: s.clock.Now(), Kind: OpSchema,
		Cells: so.cells()}, true)
}

// ErrMode is a database opened, or written, in a mode other than the one its
// nodes replicate in.
var ErrMode = errors.New("replication: wrong mode")

// genesis makes a database's schema and rows, as they are before its first
// sync, into ops of this site: every table, index and view becomes a create
// op, and every row an insert. Their IDs depend only on their text, so two
// nodes whose schemas were created alike -- the same migration run on each --
// agree on every one of them, and their rows merge.
//
// A table without a declared key has its rows renumbered first, into this
// site's rowid range (lo): its rowids are its identity, and two nodes' rowid-1
// rows are different rows. C SQLite promises no stable rowid for such a table
// either (VACUUM may renumber it). A table with an INTEGER PRIMARY KEY keeps its
// ids, which are the application's: two nodes' row 1 is the same row.
//
// It runs once, in one transaction, and only on a database with no op log.
func (s *Store) genesis(ctx context.Context, lo uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, ok, err := loadCatalog(ctx, s.db); err != nil || ok {
		return err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM main._repl_oplog`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("replication: this database's op log was written by an older crdt, keyed by name; it cannot be carried over")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	s.q = tx
	cat, ver, err := s.genesisTx(ctx, lo)
	s.q = s.db
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.cat, s.ver = cat, ver
	return s.advanceVV(ctx, s.site)
}

func (s *Store) genesisTx(ctx context.Context, lo uint64) (*catalog, string, error) {
	real, err := catalogRows(ctx, s.q)
	if err != nil {
		return nil, "", err
	}
	// Tables before what depends on them.
	rank := map[string]int{"table": 0, "index": 1, "view": 2}
	sort.SliceStable(real, func(i, j int) bool { return rank[real[i].Type] < rank[real[j].Type] })
	cat, sc := newCatalog(), (*scratch)(nil)
	if sc, err = newScratch(); err != nil {
		return nil, "", err
	}
	defer sc.close()
	var ops []Op
	for _, r := range real {
		if r.Type == "index" && r.SQL == "" {
			continue // an automatic index is its table's
		}
		if err := supportedObject(ctx, s.q, r); err != nil {
			return nil, "", fmt.Errorf("replication: %w", err)
		}
		so := schemaOp{Act: actCreate, ID: objectID(r.Type, r.Name, r.SQL, 0), Type: r.Type, Name: r.Name, SQL: r.SQL}
		if r.Type == "index" {
			so.Tbl = cat.byName(r.Tbl).ID
		}
		if stmt, err := cat.apply(ctx, sc, so); err != nil {
			return nil, "", err
		} else if stmt == "" {
			return nil, "", fmt.Errorf("replication: %s %q does not replay", r.Type, r.Name)
		}
		ops = append(ops, Op{Kind: OpSchema, Cells: so.cells()})
	}
	if err := s.checkInstalled(ctx, sc); err != nil {
		return nil, "", err
	}
	for _, t := range cat.Objs {
		if t.Type != "table" {
			continue
		}
		if t.keyCols() == nil {
			if err := s.renumber(ctx, t, lo); err != nil {
				return nil, "", err
			}
		}
		rows, err := s.tableRows(ctx, t)
		if err != nil {
			return nil, "", err
		}
		ops = append(ops, rows...)
	}
	for i := range ops {
		ops[i].Site, ops[i].Seq, ops[i].HLC = s.site, uint64(i+1), s.clock.Now()
		if err := s.appendOplog(ctx, ops[i]); err != nil {
			return nil, "", err
		}
		if ops[i].Kind == OpSchema {
			continue
		}
		if err := s.mergeCell(ctx, ops[i].Tbl, ops[i].PK, presenceCol, ops[i].HLC, s.site, []byte{1}); err != nil {
			return nil, "", err
		}
		for _, c := range ops[i].Cells {
			if err := s.mergeCell(ctx, ops[i].Tbl, ops[i].PK, c.Col, ops[i].HLC, s.site, packVal(c.Type, c.Val)); err != nil {
				return nil, "", err
			}
		}
	}
	ver, err := saveCatalog(ctx, s.execQ, cat)
	return cat, ver, err
}

// renumber moves a keyless table's rows to rowids lo, lo+1, ... in their
// current order: all out, then all back, so no new rowid can collide with an
// old one on the way.
func (s *Store) renumber(ctx context.Context, t *object, lo uint64) error {
	rid := rowidName(t.Cols)
	if rid == "" {
		return fmt.Errorf("replication: table %q: columns shadow every name for the rowid", t.Name)
	}
	names := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		names[i] = q(c.Name)
	}
	list := strings.Join(names, ",")
	rows, err := s.q.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM main.%s ORDER BY %s", list, q(t.Name), rid))
	if err != nil {
		return err
	}
	var all [][]any
	for rows.Next() {
		vals := make([]any, len(t.Cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return err
		}
		all = append(all, vals)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(all) == 0 {
		return err
	}
	if err := s.execQ(ctx, "DELETE FROM main."+q(t.Name)); err != nil {
		return err
	}
	ins := fmt.Sprintf("INSERT INTO main.%s(%s,%s) VALUES(?%s)", q(t.Name), rid, list, strings.Repeat(",?", len(names)))
	for i, vals := range all {
		if err := s.execQ(ctx, ins, append([]any{int64(lo) + int64(i)}, vals...)...); err != nil {
			return err
		}
	}
	return nil
}

// tableRows is an insert op (unstamped) for every row of t.
func (s *Store) tableRows(ctx context.Context, t *object) ([]Op, error) {
	names := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		names[i] = q(c.Name)
	}
	rid := rowidName(t.Cols)
	if rid == "" {
		rid = "NULL" // shadowed names: the table has a key, which is its identity
	}
	rows, err := s.q.QueryContext(ctx, fmt.Sprintf("SELECT %s,%s FROM main.%s", rid, strings.Join(names, ","), q(t.Name)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Op
	for rows.Next() {
		vals := make([]any, len(t.Cols)+1)
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		cols := make([]string, len(t.Cols))
		for i, c := range t.Cols {
			cols[i] = c.Name
		}
		row := vals[1:]
		for i, v := range row {
			if b, ok := v.([]byte); ok {
				row[i] = append([]byte(nil), b...)
			}
		}
		rowid, _ := vals[0].(int64)
		key, err := rowKey(t, cols, rowid, row)
		if err != nil {
			return nil, err
		}
		cells, err := rowCells(t, cols, row, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, Op{Tbl: t.ID, PK: key, Kind: OpInsert, Cells: cells})
	}
	return out, rows.Err()
}
