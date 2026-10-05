package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

// capture is the per-connection change-capture state. At commit time the driver
// hands over row changes; capture diffs them into ops and writes those into
// _repl_oplog + _repl_clock inside the user's transaction.
type capture struct {
	s    *Syncer
	conn *driver.Conn
	buf  []Op // built from this transaction's row changes, at flush time
	// flushed holds ops written in-txn awaiting commit confirmation.
	flushed []Op
	// handoff holds stamped ops between preCommit and handOff.
	handoff []Op
}

// attach wires a capture onto one connection via the driver's capture seam.
func (s *Syncer) attach(conn *driver.Conn) {
	c := &capture{s: s, conn: conn}
	conn.SetRowidRange(s.rowidLo, s.rowidHi)
	conn.SetRowidFloor(s.rowidFloor)
	conn.SetCaptureHooks(&driver.CaptureHooks{
		PreCommit:  c.preCommit,
		PostCommit: c.postCommit,
		Rollback:   c.discard,
		HandOff:    c.handOff,
	})
}

// preCommit turns the transaction's changes into ops and writes them in-txn.
// Schema changes are replayed to verify they are deterministic.
func (c *capture) preCommit(ctx context.Context, conn *driver.Conn, changes []engine.RowChange) error {
	c.buf = c.buf[:0]
	cat, _, err := c.s.catalogIn(ctx, conn)
	if err != nil {
		return err
	}
	if cat.Mode != "" && cat.Mode != c.s.mode {
		// Two nodes first opened the database in different modes while apart,
		// and the other's came first.
		return fmt.Errorf("%w: this database replicates in %s mode; this node was opened in %s", ErrMode, cat.Mode, c.s.mode)
	}
	var w, r *scratch
	verify := false // a statement the scratch could not run: main must still match
	for _, ch := range changes {
		if ch.Kind != engine.RowSchema {
			if err := c.note(cat, ch); err != nil {
				return err
			}
			continue
		}
		if w == nil {
			rows, err := conn.Query(ctx, schemaOpsQuery)
			if err != nil {
				return err
			}
			ops, err := schemaOpsFrom(rows)
			if err != nil {
				return err
			}
			if cat, r, err = replaySchema(ctx, ops); err != nil {
				return err
			}
			defer r.close()
			if _, w, err = replaySchema(ctx, ops); err != nil {
				return err
			}
			defer w.close()
		}
		if words := sqlWords(ch.SQL); words["CREATE"] && words["TRIGGER"] {
			return fmt.Errorf("%w: %s: triggers are not replicated", errSchemaUnsupported, ch.SQL)
		}
		sops, err := cat.classify(ctx, w, ch.SQL)
		if errors.Is(err, errNotOnScratch) {
			verify = true
			continue
		}
		if err != nil {
			return err
		}
		for _, so := range sops {
			if stmt, err := cat.apply(ctx, r, so); err != nil {
				return err
			} else if stmt == "" {
				return fmt.Errorf("%w: %s: it does not replay", errSchemaUnsupported, ch.SQL)
			}
			c.buf = append(c.buf, Op{Kind: OpSchema, Cells: so.cells()})
		}
		ws, err := w.snapshot(ctx)
		if err != nil {
			return err
		}
		rs, err := r.snapshot(ctx)
		if err != nil {
			return err
		}
		if d := sameCatalog(rs, ws); d != "" {
			return fmt.Errorf("%w: %s: its replay differs: %s", errSchemaUnsupported, ch.SQL, d)
		}
	}
	if r != nil && !hasSchemaOp(c.buf) && !verify {
		// Only TEMP objects changed (or nothing: IF NOT EXISTS): main's catalog
		// is as it was, and writing it again would make a temp-only statement a
		// main write.
		r = nil
	}
	if r != nil {
		// The last word: what peers will build is what this database has.
		rows, err := conn.Query(ctx, catalogQuery)
		if err != nil {
			return err
		}
		real := make([]catRow, len(rows))
		for i, row := range rows {
			real[i] = catRow{asString(row[0]), asString(row[1]), asString(row[2]), asString(row[3])}
		}
		want, err := r.snapshot(ctx)
		if err != nil {
			return err
		}
		if d := sameCatalog(real, want); d != "" {
			return fmt.Errorf("%w: %s", errSchemaUnsupported, d)
		}
	}
	// The ops are what a peer would get; a TEMP table's rows and DDL made none.
	if w := c.s.writer; w != nil && len(c.buf) > 0 && !w() {
		return ErrNotWriter
	}
	if c.s.quorum != nil && len(c.buf) > 0 {
		for _, ch := range changes {
			if ch.Temp {
				// The transaction is discarded and only its ops come back.
				return errors.New("replication: a Quorum commit cannot also change TEMP tables")
			}
		}
		c.s.store.seqMu.Lock() // released by handOff, which the driver always calls next
		ops, err := c.fresh(ctx)
		if err == nil {
			ops, err = c.stamp(ctx)
		}
		if err != nil {
			c.s.store.seqMu.Unlock()
			return err
		}
		c.handoff = ops
		return driver.ErrHandOff
	}
	if r != nil && hasSchemaOp(c.buf) {
		ver, err := saveCatalog(ctx, func(ctx context.Context, query string, args ...any) error {
			return conn.Exec(ctx, query, args...)
		}, cat)
		if err != nil {
			return err
		}
		c.s.setCatalog(cat, ver)
	}
	return c.flush(ctx)
}

func hasSchemaOp(ops []Op) bool {
	for _, op := range ops {
		if op.Kind == OpSchema {
			return true
		}
	}
	return false
}

// note buffers one row change as an op, resolved through cat.
func (c *capture) note(cat *catalog, ch engine.RowChange) error {
	if ch.Temp || isBookkeeping(ch.Table) || strings.HasPrefix(ch.Table, "sqlite_") {
		// A TEMP table is this connection's own, and may share a synced table's
		// name; the bookkeeping is ours, sqlite_ the engine's.
		return nil
	}
	t := cat.byName(ch.Table)
	if t == nil || t.Type != "table" {
		return fmt.Errorf("replication: table %q is not in the replicated schema", ch.Table)
	}
	switch ch.Kind {
	case engine.RowInsert:
		c.s.noteRowid(t.ID, ch.Rowid)
		row := goRow(ch.New)
		key, err := rowKey(t, ch.Cols, ch.Rowid, row)
		if err != nil {
			return err
		}
		cells, err := rowCells(t, ch.Cols, row, nil)
		if err != nil {
			return err
		}
		c.buf = append(c.buf, Op{Tbl: t.ID, PK: key, Kind: OpInsert, Cells: cells})
	case engine.RowDelete:
		key, err := rowKey(t, ch.Cols, ch.Rowid, goRow(ch.Old))
		if err != nil {
			return err
		}
		c.buf = append(c.buf, Op{Tbl: t.ID, PK: key, Kind: OpDelete})
	case engine.RowUpdate:
		oldRow, newRow := goRow(ch.Old), goRow(ch.New)
		oldRowid := ch.Rowid
		if from, moved := ch.MovedFrom(); moved {
			oldRowid = from
		}
		oldKey, err := rowKey(t, ch.Cols, oldRowid, oldRow)
		if err != nil {
			return err
		}
		newKey, err := rowKey(t, ch.Cols, ch.Rowid, newRow)
		if err != nil {
			return err
		}
		if !bytes.Equal(oldKey, newKey) {
			// The key changed: to a peer that is a different row, so the old one
			// goes and the new one arrives whole.
			cells, err := rowCells(t, ch.Cols, newRow, nil)
			if err != nil {
				return err
			}
			c.buf = append(c.buf,
				Op{Tbl: t.ID, PK: oldKey, Kind: OpDelete},
				Op{Tbl: t.ID, PK: newKey, Kind: OpInsert, Cells: cells})
			return nil
		}
		cells, err := rowCells(t, ch.Cols, newRow, oldRow)
		if err != nil {
			return err
		}
		if len(cells) == 0 {
			return nil // no synced column actually changed
		}
		c.buf = append(c.buf, Op{Tbl: t.ID, PK: newKey, Kind: OpUpdate, Cells: cells})
	}
	return nil
}

// rowKey is a row's identity: its primary key's values (encodeKey), or its
// rowid when it declares none (an INTEGER PRIMARY KEY is the rowid already;
// capture fills it in). A NULL in a key column -- which a rowid table's PRIMARY
// KEY allows any number of times -- cannot be an identity and is refused.
func rowKey(t *object, cols []string, rowid int64, row []any) ([]byte, error) {
	kcs := t.keyCols()
	if kcs == nil {
		return EncodePK(rowid), nil
	}
	vals := make([]any, len(kcs))
	for k, kc := range kcs {
		i := slices.IndexFunc(cols, func(name string) bool { return fold(name) == fold(kc.Name) })
		if i < 0 {
			return nil, fmt.Errorf("replication: table %q: its key column %q is not in the row", t.Name, kc.Name)
		}
		if row[i] == nil {
			return nil, fmt.Errorf("replication: table %q: a row with a NULL primary key cannot sync", t.Name)
		}
		vals[k] = row[i]
	}
	return encodeKey(vals), nil
}

// rowCells is a row's non-key cells by column ID -- only those that differ from
// old, when old is given.
func rowCells(t *object, cols []string, row, old []any) ([]Cell, error) {
	var out []Cell
	for i, name := range cols {
		col := t.colByName(name)
		if col == nil {
			return nil, fmt.Errorf("replication: table %q: column %q is not in the replicated schema", t.Name, name)
		}
		if col.PK || col.Gen || (old != nil && valueEqual(old[i], row[i])) {
			continue
		}
		out = append(out, CellFromValue(col.ID, row[i]))
	}
	return out, nil
}

// goRow converts one engine row into plain Go values (nil/int64/float64/
// string/[]byte), the domain encodeValue (store.go) understands.
func goRow(vals []engine.Value) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = driver.GoValue(v)
	}
	return out
}

// flush writes every buffered op into _repl_oplog + _repl_clock on this same
// connection, inside the open transaction, so they commit atomically with the
// user's writes. A local op's HLC comes from Now(), which is strictly greater
// than every HLC in the clock, so its cells always win — the clock upsert is
// unconditional, no read-compare needed.
//
// Seqs continue from the log as THIS transaction reads it, not from a counter:
// a peer's version vector only moves over contiguous seqs, so one handed to a
// commit that then failed was a hole no peer could ever get past -- and after a
// restart the counter resumed below it and reused seqs the log already held.
// The log insert is a plain INSERT for the same reason: two transactions that
// read the same maximum must not both commit, and OR IGNORE let the second's op
// vanish.
func (c *capture) flush(ctx context.Context) error {
	if len(c.buf) == 0 {
		return nil
	}
	ops, err := c.stamp(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if err := c.writeOp(ctx, op); err != nil {
			return err
		}
		c.flushed = append(c.flushed, op)
	}
	c.buf = c.buf[:0]
	return nil
}

// fresh refuses a Quorum commit whose transaction began before another commit
// landed. Its statements ran on what the database held then, and its seqs
// would repeat the other's: an ordinary commit fails for the same reason when
// it finds the file moved (engine.ErrBusy), but this transaction is discarded
// and never gets there. Every change to a Quorum database is an op, so the log
// as the transaction sees it, against the log as it is now, says whether
// anything landed.
func (c *capture) fresh(ctx context.Context) ([]Op, error) {
	const q = `SELECT site, max(seq) FROM main._repl_oplog GROUP BY site ORDER BY site`
	rows, err := c.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	var seen strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&seen, "%s=%v;", asString(r[0]), r[1])
	}
	now, err := c.s.store.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer now.Close()
	var cur strings.Builder
	for now.Next() {
		var site string
		var seq int64
		if err := now.Scan(&site, &seq); err != nil {
			return nil, err
		}
		fmt.Fprintf(&cur, "%s=%d;", site, seq)
	}
	if err := now.Err(); err != nil {
		return nil, err
	}
	if seen.String() != cur.String() {
		return nil, fmt.Errorf("%w: another connection committed while this transaction was open", engine.ErrBusy)
	}
	return nil, nil
}

// stamp is the buffered ops with this site, their seqs and their HLCs.
func (c *capture) stamp(ctx context.Context) ([]Op, error) {
	rows, err := c.conn.Query(ctx, maxSeqQuery, c.s.site, c.s.site)
	if err != nil {
		return nil, err
	}
	var seq uint64
	if n, ok := rows[0][0].(int64); ok {
		seq = uint64(n)
	}
	ops := make([]Op, len(c.buf))
	for i, op := range c.buf {
		seq++
		op.Site, op.Seq, op.HLC = c.s.site, seq, c.s.clock.Now()
		ops[i] = op
	}
	c.buf = c.buf[:0]
	return ops, nil
}

// handOff lands a Quorum commit: the transaction is discarded, and its ops
// go through the caller's consensus, which applies them on every node, this
// one included. The commit succeeded when they are applied here.
func (c *capture) handOff() error {
	defer c.s.store.seqMu.Unlock()
	ops := c.handoff
	c.handoff = nil
	return c.s.propose(context.Background(), ops)
}

// propose sends ops, stamped under seqMu which the caller holds, through the
// caller's consensus log as one entry, and reports whether they were applied
// here.
func (s *Syncer) propose(ctx context.Context, ops []Op) error {
	entry, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	if err := s.quorum.propose(ctx, entry); err != nil {
		return err
	}
	for _, op := range ops {
		same, exists, err := s.store.holds(ctx, op)
		if err != nil {
			return err
		}
		if !exists || !same {
			return fmt.Errorf("replication: the quorum committed op %s/%d but it was not applied here; propose must return after Apply has run on this node", op.Site, op.Seq)
		}
	}
	return nil
}

func (c *capture) writeOp(ctx context.Context, op Op) error {
	if err := c.conn.Exec(ctx,
		`INSERT INTO main._repl_oplog(site,seq,hlc,tbl,pk,op,cells) VALUES(?,?,?,?,?,?,?)`,
		op.Site, int64(op.Seq), int64(op.HLC), op.Tbl, op.PK, int64(op.Kind), encodeCells(op.Cells)); err != nil {
		return err
	}
	if op.Kind == OpSchema {
		return nil // a schema op has no cells of any row
	}
	pval := byte(1)
	if op.Kind == OpDelete {
		pval = 0
	}
	if err := c.upsertClock(ctx, op.Tbl, op.PK, presenceCol, op.HLC, op.Site, []byte{pval}); err != nil {
		return err
	}
	for _, cell := range op.Cells {
		if err := c.upsertClock(ctx, op.Tbl, op.PK, cell.Col, op.HLC, op.Site, packVal(cell.Type, cell.Val)); err != nil {
			return err
		}
	}
	return nil
}

func (c *capture) upsertClock(ctx context.Context, tbl string, pk []byte, col string, hlc HLC, site string, val []byte) error {
	return c.conn.Exec(ctx,
		`INSERT INTO main._repl_clock(tbl,pk,col,hlc,site,val) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(tbl,pk,col) DO UPDATE SET hlc=excluded.hlc, site=excluded.site, val=excluded.val`,
		tbl, pk, col, int64(hlc), site, val)
}

// postCommit runs after the user's transaction commits. The ops are durable, so
// it advances the version vector for this site and hands the ops to the syncer
// for propagation.
func (c *capture) postCommit() {
	if len(c.flushed) == 0 {
		return
	}
	var maxSeq uint64
	for _, op := range c.flushed {
		if op.Seq > maxSeq {
			maxSeq = op.Seq
		}
	}
	c.s.store.NoteLocal(c.s.site, maxSeq)
	c.s.propagate(c.flushed)
	var tbls []string
	for _, op := range c.flushed {
		if op.Kind != OpSchema && !slices.Contains(tbls, op.Tbl) {
			tbls = append(tbls, op.Tbl)
		}
	}
	c.flushed = c.flushed[:0]
	// A local delete or update can free a UNIQUE value a held-out row wants;
	// peers rebuild when the op reaches them, and so must this node.
	if err := c.s.store.rebuildHeld(context.Background(), tbls); err != nil {
		log.Printf("replication: rebuilding tables after a local commit: %v", err)
	}
}

// discard drops all buffered and flushed-but-uncommitted ops on rollback.
func (c *capture) discard() {
	c.buf = c.buf[:0]
	c.flushed = c.flushed[:0]
}

// valueEqual compares two column values as delivered by the change log
// (int64, float64, string, []byte, or nil).
func valueEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ab, aok := a.([]byte)
	bb, bok := b.([]byte)
	if aok || bok {
		return aok && bok && bytes.Equal(ab, bb)
	}
	return a == b
}
