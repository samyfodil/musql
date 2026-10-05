package replication

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
)

// presenceCol is the synthetic per-row cell that carries liveness: value {1} =
// alive, {0} = tombstoned. Modelling presence as a cell lets one last-writer-wins
// rule cover insert / update / delete / resurrection uniformly.
const presenceCol = "__p"

// Store is the CRDT state for one site over a single SQLite database. The
// bookkeeping tables (_repl_oplog, _repl_clock) are the source of truth; the
// user tables are a materialized view maintained by Ingest. A Store is safe for
// concurrent use; all mutations serialize on mu (SQLite is a single writer
// regardless).
type Store struct {
	db    *sql.DB
	site  string
	clock *Clock

	mu sync.Mutex
	vv VersionVector // max contiguous seq observed per site
	// floors is _repl_floors: per site, the seq its ops are pruned up to.
	floors VersionVector
	// acks is the latest ack known of every other site, selfAck this node's own
	// (prune.go).
	acks    Acks
	selfAck Ack
	// seqMu serializes giving out this site's seqs outside a user transaction
	// -- a schema op of its own (localSchemaOp), a Quorum commit from stamping
	// to apply -- so no two take one seq.
	seqMu sync.Mutex
	// q is what Ingest's statements run on: its transaction while one is open,
	// db otherwise.
	q dbtx
	// cat is the schema installed in the user tables (see catalog.go) and ver
	// its version in _repl_meta.
	cat *catalog
	ver string
}

// begin starts a transaction on the store's handle that ctx's cancellation does
// not end. database/sql rolls back a transaction whose context is cancelled
// from a goroutine of its own, which db.Close does not wait for -- so a sync
// loop stopped mid-transaction let the connection roll back and close, and
// touch the file, after the database was closed. Every store transaction ends
// on its own path instead: the statements in it still see ctx, fail once it is
// cancelled, and the error path rolls back before the call returns.
func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(context.WithoutCancel(ctx), nil)
}

// dbtx is the part of *sql.DB and *sql.Tx the store uses.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// OpenStore initialises the bookkeeping schema and loads the version vector.
// db must be able to write a replicated database without capturing, which a
// plain sql.Open handle cannot (driver.Conn.AllowUncapturedWrites): Open makes the
// one it uses.
func OpenStore(ctx context.Context, db *sql.DB, site string) (*Store, error) {
	if err := InitSchema(ctx, db); err != nil {
		return nil, err
	}
	s := &Store{db: db, q: db, site: site, clock: NewClock(site), vv: VersionVector{}, floors: VersionVector{}, acks: Acks{}, cat: newCatalog()}
	if err := s.claimRanges(ctx); err != nil {
		return nil, err
	}
	if err := s.loadFloors(ctx); err != nil {
		return nil, err
	}
	if err := s.loadVV(ctx); err != nil {
		return nil, err
	}
	if cat, ver, ok, err := loadCatalog(ctx, db); err != nil {
		return nil, err
	} else if ok {
		s.cat, s.ver = cat, ver
	}
	// Seed the clock past the highest HLC already durable, so a fresh local
	// write is causally after everything this database has ever seen -- the op
	// log's, which holds the schema ops, and the clock's, which holds the cells
	// of row ops long pruned from the log.
	var maxHLC sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(h) FROM (SELECT max(hlc) AS h FROM main._repl_oplog UNION ALL SELECT max(hlc) FROM main._repl_clock)`).Scan(&maxHLC); err != nil {
		return nil, err
	}
	if maxHLC.Valid {
		s.clock.Merge(HLC(maxHLC.Int64))
	}
	return s, nil
}

// Site returns the store's site identifier.
func (s *Store) Site() string { return s.site }

// NoteLocal advances the in-memory version vector for a site to seq, used after
// a captured local write has committed its oplog rows out of band (in the user's
// transaction, on a different connection).
func (s *Store) NoteLocal(site string, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.vv[site] {
		s.vv[site] = seq
	}
}

// Clock returns the store's hybrid logical clock.
func (s *Store) Clock() *Clock { return s.clock }

// LocalVV returns a copy of the version vector (max contiguous seq per site).
func (s *Store) LocalVV() VersionVector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vv.Clone()
}

// OpsSince returns ops from the given site with seq > fromSeq, in seq order,
// capped at limit (limit <= 0 means no cap). This is the server side of
// anti-entropy pull.
//
// A fromSeq below the site's floor asks for ops this node has pruned:
// ErrPruned, and the asker needs a snapshot.
func (s *Store) OpsSince(ctx context.Context, site string, fromSeq uint64, limit int) ([]Op, error) {
	var floor uint64
	if err := s.db.QueryRowContext(ctx, `SELECT seq FROM main._repl_floors WHERE site=?`, site).Scan(&floor); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if fromSeq < floor {
		return nil, ErrPruned
	}
	q := `SELECT site, seq, hlc, tbl, pk, op, cells FROM main._repl_oplog WHERE site=? AND seq>? ORDER BY seq`
	args := []any{site, fromSeq}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ops []Op
	for rows.Next() {
		var (
			op    Op
			hlc   int64
			kind  int64
			cells []byte
		)
		if err := rows.Scan(&op.Site, &op.Seq, &hlc, &op.Tbl, &op.PK, &kind, &cells); err != nil {
			return nil, err
		}
		op.HLC = HLC(hlc)
		op.Kind = OpKind(kind)
		if op.Cells, err = decodeCells(cells); err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// Ingest applies op to the store. It is idempotent: an op whose (site, seq) is
// already present is ignored. local reports whether op originated from this
// site's own capture — when true the user table already reflects it, so the row
// is not re-materialized. Remote ops pass local=false.
//
// The whole apply is ONE transaction on the store's connection: the op, the
// clock cells it wins, the rows it rewrites and -- for a schema op -- the tables
// it drops and rebuilds either all land or none do, so a failure part-way leaves
// the op un-recorded and the next delivery redoes it from the start.
func (s *Store) Ingest(ctx context.Context, op Op, local bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if op.Seq <= s.floors[op.Site] && op.Kind != OpSchema {
		// Pruned here: applied long ago, and everywhere. A schema op is never
		// pruned, so one at or below a floor that is not in the log is one this
		// node has not seen -- a snapshot can raise a floor past it.
		return nil
	}

	if same, exists, err := s.sameOp(ctx, op); err != nil {
		return err
	} else if exists {
		if !same {
			// Two nodes are writing under one site id (a copied database file):
			// their ops at one seq differ, and each peer keeps whichever it saw
			// first.
			return fmt.Errorf("replication: site %q wrote two different ops at seq %d; two nodes share that site id", op.Site, op.Seq)
		}
		return nil
	}

	// Advance our clock past the op so the next local write is causally after it.
	s.clock.Merge(op.HLC)

	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	s.q = tx
	cat, ver, err := s.ingestTx(ctx, op, local)
	s.q = s.db
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if cat != nil {
		s.cat, s.ver = cat, ver
	}
	return s.advanceVV(ctx, op.Site)
}

// ingestTx is Ingest's transaction. For a schema op it returns the catalog now
// installed.
func (s *Store) ingestTx(ctx context.Context, op Op, local bool) (*catalog, string, error) {
	if err := s.claimRange(ctx, op.Site); err != nil {
		return nil, "", err
	}
	// A local capture may have changed the schema since this store last looked.
	if err := s.syncCatalog(ctx); err != nil {
		return nil, "", err
	}
	if err := s.appendOplog(ctx, op); err != nil {
		return nil, "", err
	}
	if op.Kind == OpSchema {
		return s.applySchema(ctx, op)
	}
	// Per-cell merge into the clock, presence first. Cells merge even into a
	// currently-tombstoned row (grow-only) so a later resurrection sees them --
	// and into a table or column not known here yet, whose rows are withheld
	// until the schema op that makes it arrives (applySchema materializes them).
	pval := byte(1)
	if op.Kind == OpDelete {
		pval = 0
	}
	if err := s.mergeCell(ctx, op.Tbl, op.PK, presenceCol, op.HLC, op.Site, []byte{pval}); err != nil {
		return nil, "", err
	}
	for _, c := range op.Cells {
		if err := s.mergeCell(ctx, op.Tbl, op.PK, c.Col, op.HLC, op.Site, packVal(c.Type, c.Val)); err != nil {
			return nil, "", err
		}
	}
	if !local {
		if err := s.materializeRow(ctx, s.cat, op.Tbl, op.PK); err != nil {
			return nil, "", err
		}
	}
	return nil, "", nil
}

// mergeCell writes (hlc, site, val) for a cell iff it is a strictly newer writer
// than what the clock holds, by the (hlc, site) total order.
func (s *Store) mergeCell(ctx context.Context, tbl string, pk []byte, col string, hlc HLC, site string, val []byte) error {
	var (
		eHLC  int64
		eSite string
	)
	err := s.q.QueryRowContext(ctx,
		`SELECT hlc, site FROM main._repl_clock WHERE tbl=? AND pk=? AND col=?`, tbl, pk, col).Scan(&eHLC, &eSite)
	switch err {
	case sql.ErrNoRows:
		_, err = s.q.ExecContext(ctx,
			`INSERT INTO main._repl_clock(tbl,pk,col,hlc,site,val) VALUES(?,?,?,?,?,?)`,
			tbl, pk, col, int64(hlc), site, val)
		return err
	case nil:
		if TagLess(HLC(eHLC), eSite, hlc, site) {
			_, err = s.q.ExecContext(ctx,
				`UPDATE main._repl_clock SET hlc=?, site=?, val=? WHERE tbl=? AND pk=? AND col=?`,
				int64(hlc), site, val, tbl, pk, col)
			return err
		}
		return nil // existing writer wins; discard
	default:
		return err
	}
}

// syncCatalog reloads the installed catalog if a capture on another
// connection changed it: its version is one point read.
func (s *Store) syncCatalog(ctx context.Context) error {
	var ver string
	err := s.q.QueryRowContext(ctx, `SELECT v FROM main._repl_meta WHERE k=?`, metaSchemaVer).Scan(&ver)
	if errors.Is(err, sql.ErrNoRows) || err == nil && ver == s.ver {
		return nil
	}
	if err != nil {
		return err
	}
	cat, ver, _, err := loadCatalog(ctx, s.q)
	if err != nil {
		return err
	}
	s.cat, s.ver = cat, ver
	return nil
}

// materializeRow brings one user-table row to what the clock says: gone if
// the row is tombstoned or unknown; otherwise the full row from its live cells
// -- but only once every NOT NULL column without a default has a value (the
// completeness gate), so an update that races ahead of its insert can never
// write NULL into a NOT NULL column or leave a torn row.
//
// A row that would break a UNIQUE index -- two nodes each wrote the value --
// is settled by rebuilding the table (rebuildTable), and so is every change to
// a table that already has a row held out that way: what the table holds is
// then a function of the clock alone, the same on every node.
//
// tblID and the cells' columns are IDs, resolved through cat: a table cat does
// not have (not created here yet, or dropped) is not written, and a cell of a
// column it does not have is not either.
func (s *Store) materializeRow(ctx context.Context, cat *catalog, tblID string, pk []byte) error {
	t := cat.byID(tblID)
	if t == nil || t.Type != "table" {
		return nil
	}
	var held int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM main._repl_withheld WHERE tbl=? LIMIT 1)`, tblID).Scan(&held); err != nil {
		return err
	}
	if held > 0 {
		return s.rebuildTable(ctx, t)
	}
	if conflict, err := s.writeRow(ctx, t, pk, true); err != nil || !conflict {
		return err
	}
	return s.rebuildTable(ctx, t)
}

// writeRow writes pk's row as the clock has it, first deleting the one there
// when del is set. conflict reports an alive, complete row the table refused:
// one of its UNIQUE values is another row's.
func (s *Store) writeRow(ctx context.Context, t *object, pk []byte, del bool) (conflict bool, err error) {
	var keyNames []string
	for _, kc := range t.keyCols() {
		keyNames = append(keyNames, kc.Name)
	}
	if keyNames == nil {
		rid := rowidName(t.Cols)
		if rid == "" {
			return false, fmt.Errorf("replication: table %q: columns shadow every name for the rowid", t.Name)
		}
		keyNames = []string{rid}
	}
	keyVals, err := decodeKey(pk, len(keyNames))
	if err != nil {
		return false, fmt.Errorf("replication: table %q: %w", t.Name, err)
	}
	if del {
		where := make([]string, len(keyNames))
		for i, k := range keyNames {
			where[i] = quoteIdent(k) + "=?"
		}
		if _, err := s.q.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM main.%s WHERE %s`, quoteIdent(t.Name), strings.Join(where, " AND ")), keyVals...); err != nil {
			return false, err
		}
	}

	alive, known, err := s.presence(ctx, t.ID, pk)
	if err != nil || !known || !alive {
		return false, err
	}
	cells, err := s.liveCells(ctx, t.ID, pk)
	if err != nil {
		return false, err
	}
	for _, c := range t.Cols {
		if c.PK || c.Gen || !c.NotNull || c.Default {
			continue
		}
		if _, ok := cells[c.ID]; !ok {
			return false, nil // completeness gate: withhold until the covering insert arrives
		}
	}

	colNames := slices.Clone(keyNames)
	args := keyVals
	for _, c := range t.Cols { // schema order, deterministic; a generated column has no cell
		if c.PK {
			continue
		}
		if v, ok := cells[c.ID]; ok {
			colNames = append(colNames, c.Name)
			args = append(args, driverValue(v.typ, v.raw))
		}
	}
	quoted := make([]string, len(colNames))
	ph := make([]string, len(colNames))
	for i, c := range colNames {
		quoted[i] = quoteIdent(c)
		ph[i] = "?"
	}
	// OR IGNORE: the key's own row is gone (deleted above, or the table is
	// being rebuilt), so the only conflict left is a UNIQUE one.
	res, err := s.q.ExecContext(ctx, fmt.Sprintf(`INSERT OR IGNORE INTO main.%s(%s) VALUES(%s)`,
		quoteIdent(t.Name), strings.Join(quoted, ","), strings.Join(ph, ",")), args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 0, err
}

// rebuildTable writes table t afresh from the clock: every alive row, newest
// first -- by the latest (hlc, site) of any of its cells -- each skipped when a
// UNIQUE value it holds is already taken, which can only be by a newer row. The
// skipped ones go in _repl_withheld, and stay in the clock: when the value
// frees up, the next rebuild takes them. The result depends on nothing but the
// clock, so every node that has the same ops holds the same rows.
func (s *Store) rebuildTable(ctx context.Context, t *object) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM main.`+quoteIdent(t.Name)); err != nil {
		return err
	}
	if _, err := s.q.ExecContext(ctx, `DELETE FROM main._repl_withheld WHERE tbl=?`, t.ID); err != nil {
		return err
	}
	type stamp struct {
		pk   []byte
		hlc  int64
		site string
	}
	rows, err := s.q.QueryContext(ctx, `SELECT pk, max(hlc) FROM main._repl_clock WHERE tbl=? GROUP BY pk`, t.ID)
	if err != nil {
		return err
	}
	var all []stamp
	for rows.Next() {
		var st stamp
		if err := rows.Scan(&st.pk, &st.hlc); err != nil {
			rows.Close()
			return err
		}
		all = append(all, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range all {
		// The site of the row's latest cell breaks an HLC tie, as it does
		// everywhere else (TagLess).
		if err := s.q.QueryRowContext(ctx, `SELECT max(site) FROM main._repl_clock WHERE tbl=? AND pk=? AND hlc=?`,
			t.ID, all[i].pk, all[i].hlc).Scan(&all[i].site); err != nil {
			return err
		}
	}
	slices.SortFunc(all, func(a, b stamp) int {
		if TagLess(HLC(a.hlc), a.site, HLC(b.hlc), b.site) {
			return 1
		}
		if TagLess(HLC(b.hlc), b.site, HLC(a.hlc), a.site) {
			return -1
		}
		return bytes.Compare(a.pk, b.pk)
	})
	for _, st := range all {
		conflict, err := s.writeRow(ctx, t, st.pk, false)
		if err != nil {
			return err
		}
		if conflict {
			if _, err := s.q.ExecContext(ctx, `INSERT INTO main._repl_withheld(tbl, pk) VALUES(?, ?)`, t.ID, st.pk); err != nil {
				return err
			}
		}
	}
	return nil
}

// rebuildHeld rebuilds every table among tblIDs that has a row held out by a
// UNIQUE conflict: a local commit may have freed the value, and the next remote
// op to the table may be a long way off.
func (s *Store) rebuildHeld(ctx context.Context, tblIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	s.q = tx
	err = func() error {
		if err := s.syncCatalog(ctx); err != nil {
			return err
		}
		for _, id := range tblIDs {
			var held int
			if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM main._repl_withheld WHERE tbl=? LIMIT 1)`, id).Scan(&held); err != nil {
				return err
			}
			if t := s.cat.byID(id); held > 0 && t != nil {
				if err := s.rebuildTable(ctx, t); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	s.q = s.db
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// --- clock reads ---

func (s *Store) presence(ctx context.Context, tbl string, pk []byte) (alive, known bool, err error) {
	var val []byte
	err = s.q.QueryRowContext(ctx,
		`SELECT val FROM main._repl_clock WHERE tbl=? AND pk=? AND col=?`, tbl, pk, presenceCol).Scan(&val)
	switch err {
	case sql.ErrNoRows:
		return false, false, nil
	case nil:
		return len(val) > 0 && val[0] == 1, true, nil
	default:
		return false, false, err
	}
}

type typedVal struct {
	typ byte
	raw []byte
}

func (s *Store) liveCells(ctx context.Context, tbl string, pk []byte) (map[string]typedVal, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT col, val FROM main._repl_clock WHERE tbl=? AND pk=? AND col<>?`, tbl, pk, presenceCol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]typedVal{}
	for rows.Next() {
		var col string
		var val []byte
		if err := rows.Scan(&col, &val); err != nil {
			return nil, err
		}
		typ, raw := unpackVal(val)
		out[col] = typedVal{typ, raw}
	}
	return out, rows.Err()
}

// --- rowid ranges ---

// rowidHighs is, per table, the largest key in [lo, hi) the clock holds a cell
// of -- a deleted row's presence cell included. An integer key encodes as its
// type byte and 8 big-endian bytes (EncodePK), so for keys in one positive range
// byte order is numeric order, and nothing else falls between the bounds.
func (s *Store) rowidHighs(ctx context.Context, lo, hi uint64) (map[string]uint64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tbl, max(pk) FROM main._repl_clock WHERE pk >= ? AND pk < ? GROUP BY tbl`,
		EncodePK(int64(lo)), EncodePK(int64(hi)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var tbl string
		var pk []byte
		if err := rows.Scan(&tbl, &pk); err != nil {
			return nil, err
		}
		if v, ok := driverValue(unpackVal(pk)).(int64); ok {
			out[tbl] = uint64(v)
		}
	}
	return out, rows.Err()
}

// claimRange admits site to its rowid range (RowidRange), or refuses it when
// another site already holds that range here. Two sites that hash to one range
// hand out the same rowids, and their different rows would merge as one -- on
// this node, and on every other node that admits both. So each node admits the
// first site it saw per range and refuses the other's ops, whatever key they
// touch.
//
// ponytail: containment, not convergence -- two nodes that saw the two sites in
// different orders admit different ones, and the refused site's ops never land
// there. The odds are ~n^2/2^31 for n sites; route ranges through the op log if
// that is ever hit.
func (s *Store) claimRange(ctx context.Context, site string) error {
	lo, _ := RowidRange(site)
	var owner string
	err := s.q.QueryRowContext(ctx, `SELECT site FROM main._repl_ranges WHERE lo=?`, int64(lo)).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.q.ExecContext(ctx, `INSERT INTO main._repl_ranges(lo, site) VALUES(?, ?)`, int64(lo), site)
		return err
	case err != nil:
		return err
	case owner != site:
		return fmt.Errorf("replication: site %q shares a rowid range with site %q, which this node admitted first; its ops are refused", site, owner)
	}
	return nil
}

// claimRanges claims this site's range, then every site's already in the log
// (a database from before the registry). A log that holds two sites of one
// range may already hold their rows merged, which nothing here can undo.
func (s *Store) claimRanges(ctx context.Context) error {
	if err := s.claimRange(ctx, s.site); err != nil {
		return fmt.Errorf("replication: this database's site %q: %w", s.site, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT site FROM main._repl_oplog`)
	if err != nil {
		return err
	}
	var sites []string
	for rows.Next() {
		var site string
		if err := rows.Scan(&site); err != nil {
			rows.Close()
			return err
		}
		sites = append(sites, site)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, site := range sites {
		if err := s.claimRange(ctx, site); err != nil {
			return fmt.Errorf("replication: the op log already holds ops of both sites: %w", err)
		}
	}
	return nil
}

// --- oplog + version vector ---

// holds is sameOp for a caller outside Ingest -- and an op at or below its
// site's floor was applied here, though pruned since.
func (s *Store) holds(ctx context.Context, op Op) (same, exists bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op.Seq <= s.floors[op.Site] && op.Kind != OpSchema {
		return true, true, nil
	}
	return s.sameOp(ctx, op)
}

// sameOp reports whether the log holds op's (site, seq), and if so whether it is
// the same op.
func (s *Store) sameOp(ctx context.Context, op Op) (same, exists bool, err error) {
	var hlc, kind int64
	var tbl string
	var pk, cells []byte
	err = s.q.QueryRowContext(ctx,
		`SELECT hlc, tbl, pk, op, cells FROM main._repl_oplog WHERE site=? AND seq=?`, op.Site, op.Seq).Scan(&hlc, &tbl, &pk, &kind, &cells)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, err
	}
	same = HLC(hlc) == op.HLC && tbl == op.Tbl && bytes.Equal(pk, op.PK) &&
		OpKind(kind) == op.Kind && bytes.Equal(cells, encodeCells(op.Cells))
	return same, true, nil
}

func (s *Store) hasOp(ctx context.Context, site string, seq uint64) (bool, error) {
	var one int
	err := s.q.QueryRowContext(ctx,
		`SELECT 1 FROM main._repl_oplog WHERE site=? AND seq=?`, site, seq).Scan(&one)
	switch err {
	case sql.ErrNoRows:
		return false, nil
	case nil:
		return true, nil
	default:
		return false, err
	}
}

func (s *Store) appendOplog(ctx context.Context, op Op) error {
	_, err := s.q.ExecContext(ctx,
		`INSERT OR IGNORE INTO main._repl_oplog(site,seq,hlc,tbl,pk,op,cells) VALUES(?,?,?,?,?,?,?)`,
		op.Site, op.Seq, int64(op.HLC), op.Tbl, op.PK, int64(op.Kind), encodeCells(op.Cells))
	return err
}

func (s *Store) advanceVV(ctx context.Context, site string) error {
	next := s.vv[site] + 1
	for {
		ok, err := s.hasOp(ctx, site, next)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		s.vv[site] = next
		next++
	}
	return nil
}

// ErrPruned is a request for ops a node has pruned: it holds their effect, not
// the ops, and the asker has to take a snapshot instead.
var ErrPruned = errors.New("replication: history pruned; a snapshot is required")

func (s *Store) loadFloors(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT site, seq FROM main._repl_floors`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var site string
		var seq uint64
		if err := rows.Scan(&site, &seq); err != nil {
			return err
		}
		s.floors[site] = seq
	}
	return rows.Err()
}

// maxSeqQuery is a site's highest seq: in the log, or pruned below its floor.
// A new op takes the next one -- after a prune, the log alone can hand out a
// seq again.
const maxSeqQuery = `SELECT max(m) FROM (SELECT max(seq) AS m FROM main._repl_oplog WHERE site=? UNION ALL SELECT seq FROM main._repl_floors WHERE site=?)`

func (s *Store) loadVV(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT site, seq FROM main._repl_oplog ORDER BY site, seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	bySite := map[string][]uint64{}
	for rows.Next() {
		var site string
		var seq uint64
		if err := rows.Scan(&site, &seq); err != nil {
			return err
		}
		bySite[site] = append(bySite[site], seq)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for site, f := range s.floors {
		s.vv[site] = f
	}
	for site, seqs := range bySite {
		c := s.floors[site]       // everything up to the floor is applied
		for _, sq := range seqs { // ascending
			if sq <= c {
				continue
			}
			if sq == c+1 {
				c++
			} else if sq > c+1 {
				break
			}
		}
		s.vv[site] = c
	}
	return nil
}

// --- table introspection ---

// rowidName is the first of _rowid_, rowid, oid that no column shadows: the
// name that reaches a keyless table's rowid (C's sqlite3RowidAlias,
// expr.c:3054), or "" when every one is shadowed.
func rowidName(cols []column) string {
next:
	for _, n := range []string{"_rowid_", "rowid", "oid"} {
		for _, c := range cols {
			if fold(c.Name) == n {
				continue next
			}
		}
		return n
	}
	return ""
}

// --- value + cell codecs ---

// packVal prepends the type tag to a cell's raw bytes for storage in the clock.
func packVal(typ byte, raw []byte) []byte {
	return append([]byte{typ}, raw...)
}

func unpackVal(b []byte) (typ byte, raw []byte) {
	if len(b) == 0 {
		return TypeNull, nil
	}
	return b[0], b[1:]
}

// driverValue turns a (type, raw) pair into a database/sql bind value.
func driverValue(typ byte, raw []byte) any {
	switch typ {
	case TypeInt:
		return int64(binary.BigEndian.Uint64(pad8(raw)))
	case TypeFloat:
		return math.Float64frombits(binary.BigEndian.Uint64(pad8(raw)))
	case TypeText:
		return string(raw)
	case TypeBlob:
		return raw
	default:
		return nil
	}
}

func pad8(b []byte) []byte {
	if len(b) >= 8 {
		return b
	}
	p := make([]byte, 8)
	copy(p[8-len(b):], b)
	return p
}

// CellFromValue builds a Cell from a Go value produced by database/sql.
func CellFromValue(col string, v any) Cell {
	typ, raw := encodeValue(v)
	return Cell{Col: col, Type: typ, Val: raw}
}

// encodeKey is a key's identity: EncodePK of its one value, or for a composite
// key a tuple -- tupleKey, then each value's EncodePK length-prefixed -- so no
// two different tuples encode alike.
func encodeKey(vals []any) []byte {
	if len(vals) == 1 {
		return EncodePK(vals[0])
	}
	out := []byte{tupleKey}
	for _, v := range vals {
		e := EncodePK(v)
		out = binary.AppendUvarint(out, uint64(len(e)))
		out = append(out, e...)
	}
	return out
}

// tupleKey starts a composite key's identity; it is no Type*, which is what a
// single key's starts with.
const tupleKey = 0x80

// decodeKey is encodeKey's values, as bind values, for a key of n columns.
func decodeKey(pk []byte, n int) ([]any, error) {
	if n == 1 {
		return []any{driverValue(unpackVal(pk))}, nil
	}
	if len(pk) == 0 || pk[0] != tupleKey {
		return nil, fmt.Errorf("a %d-column key's identity is not a tuple", n)
	}
	r := bytes.NewReader(pk[1:])
	out := make([]any, 0, n)
	for r.Len() > 0 {
		l, err := binary.ReadUvarint(r)
		if err != nil || l > uint64(r.Len()) {
			return nil, fmt.Errorf("a key tuple is truncated")
		}
		e := make([]byte, l)
		r.Read(e)
		out = append(out, driverValue(unpackVal(e)))
	}
	if len(out) != n {
		return nil, fmt.Errorf("a key tuple has %d values, not %d", len(out), n)
	}
	return out, nil
}

// EncodePK encodes a primary-key value into the opaque, type-tagged identity
// used as the row key in the oplog and clock.
//
// An integral REAL key is encoded as the INTEGER it equals: SQLite's key
// comparison is numeric across the two (sqlite3MemCompare, vdbeaux.c:4579), so 1 and
// 1.0 are one row to a PRIMARY KEY of no affinity, and must be one identity.
func EncodePK(v any) []byte {
	if f, ok := v.(float64); ok && f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63 {
		v = int64(f)
	}
	typ, raw := encodeValue(v)
	return packVal(typ, raw)
}

func encodeValue(v any) (typ byte, raw []byte) {
	switch x := v.(type) {
	case nil:
		return TypeNull, nil
	case int64:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(x))
		return TypeInt, b
	case int:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(int64(x)))
		return TypeInt, b
	case float64:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, math.Float64bits(x))
		return TypeFloat, b
	case string:
		return TypeText, []byte(x)
	case []byte:
		return TypeBlob, append([]byte(nil), x...)
	default:
		return TypeText, []byte(fmt.Sprint(x))
	}
}

func encodeCells(cells []Cell) []byte {
	var buf bytes.Buffer
	var tmp [binary.MaxVarintLen64]byte
	putUvarint := func(n uint64) { buf.Write(tmp[:binary.PutUvarint(tmp[:], n)]) }
	putBytes := func(b []byte) {
		putUvarint(uint64(len(b)))
		buf.Write(b)
	}
	putUvarint(uint64(len(cells)))
	for _, c := range cells {
		putBytes([]byte(c.Col))
		buf.WriteByte(c.Type)
		putBytes(c.Val)
	}
	return buf.Bytes()
}

func decodeCells(b []byte) ([]Cell, error) {
	if len(b) == 0 {
		return nil, nil
	}
	r := bytes.NewReader(b)
	readBytes := func() ([]byte, error) {
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, err
		}
		out := make([]byte, n)
		if _, err := readFull(r, out); err != nil {
			return nil, err
		}
		return out, nil
	}
	count, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	cells := make([]Cell, 0, count)
	for i := uint64(0); i < count; i++ {
		col, err := readBytes()
		if err != nil {
			return nil, err
		}
		typ, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		val, err := readBytes()
		if err != nil {
			return nil, err
		}
		cells = append(cells, Cell{Col: string(col), Type: typ, Val: val})
	}
	return cells, nil
}

func readFull(r *bytes.Reader, p []byte) (int, error) {
	for n := 0; n < len(p); {
		m, err := r.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return len(p), nil
}

// quoteIdent quotes a SQL identifier, escaping embedded double quotes.
func quoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// WithheldRow is a row CRDT mode holds out of its table: it lost a UNIQUE value
// to a newer row (README.md, "Two modes"), and comes back when the value frees
// up.
type WithheldRow struct {
	Table string         // the table's current name
	Key   []any          // its primary key's values in column order, or its rowid
	Row   map[string]any // its columns as the clock has them, by name
}

// Withheld lists the rows held out of db's tables, db being the *sql.DB Open
// returned (or any connection to the file: it only reads).
func Withheld(ctx context.Context, db *sql.DB) ([]WithheldRow, error) {
	cat, _, _, err := loadCatalog(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT tbl, pk FROM main._repl_withheld ORDER BY tbl, pk`)
	if err != nil {
		return nil, err
	}
	type held struct {
		tbl string
		pk  []byte
	}
	var all []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.tbl, &h.pk); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []WithheldRow
	for _, h := range all {
		t := cat.byID(h.tbl)
		if t == nil {
			continue // dropped since; the next rebuild of nothing clears it
		}
		n := max(len(t.keyCols()), 1)
		key, err := decodeKey(h.pk, n)
		if err != nil {
			return nil, err
		}
		w := WithheldRow{Table: t.Name, Key: key, Row: map[string]any{}}
		cells, err := db.QueryContext(ctx, `SELECT col, val FROM main._repl_clock WHERE tbl=? AND pk=? AND col<>?`, h.tbl, h.pk, presenceCol)
		if err != nil {
			return nil, err
		}
		for cells.Next() {
			var col string
			var val []byte
			if err := cells.Scan(&col, &val); err != nil {
				cells.Close()
				return nil, err
			}
			if c := t.col(col); c != nil {
				w.Row[c.Name] = driverValue(unpackVal(val))
			}
		}
		cells.Close()
		if err := cells.Err(); err != nil {
			return nil, err
		}
		for i, kc := range t.keyCols() {
			w.Row[kc.Name] = key[i]
		}
		out = append(out, w)
	}
	return out, nil
}
