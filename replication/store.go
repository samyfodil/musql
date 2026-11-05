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
	"sync/atomic"
	"time"

	"github.com/samyfodil/musql/engine"
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
	// txHeld caches, inside an ingest's transaction, whether a table has
	// withheld rows.
	txHeld map[string]bool
	// mayHold is every table that may have withheld rows: those with some at
	// open and those rebuildTable held a row of since (it alone writes
	// _repl_withheld, under mu).
	mayHold map[string]bool
	// seenRowid, when set, is told each applied op's integer key: the
	// Syncer's rowid floor, where every site assigns rowids alike.
	seenRowid func(tbl string, rowid int64)
	// localRetries counts local commits being retried after losing to an
	// apply, localUntil (unix ns) is when waitLocal stops waiting for them,
	// lastLocal is when a local commit last landed, and applied counts ingest
	// commits.
	localRetries atomic.Int32
	localUntil   atomic.Int64
	lastLocal    atomic.Int64
	applied      atomic.Uint64
	// localWaiting and localGot count the local writes waiting for the write
	// lock and done waiting; yieldTo is the localGot a yielding apply waits for
	// (waitLocal). ingestWaiting and ingestGot are the same for ingests
	// (waitIngest). localFree closes when localGot or ingestGot rises or
	// localRetries falls.
	localWaiting  atomic.Int32
	localGot      atomic.Uint64
	yieldTo       atomic.Uint64
	ingestWaiting atomic.Int32
	ingestGot     atomic.Uint64
	localMu       sync.Mutex
	localFree     chan struct{}
	// ownSeq is the highest seq of this site's that a local commit or the log
	// at open holds. NoteLocal raises it without mu and foldOwn takes it into
	// vv under mu, so a local writer need not wait for an apply transaction.
	ownSeq atomic.Uint64
	// anyHeld is whether mayHold has a table at all, read without mu.
	anyHeld atomic.Bool
	// group gathers concurrent Ingest calls into one transaction (Ingest).
	group ingestGroup
	// pins hold prune at the cuts of the images being sent (image.go).
	pins []*imagePin
	// intern is the column and site names the clock records an ingest reads
	// hold, one string each (decodeRecIn); read under mu.
	intern recIntern
}

// begin starts a store transaction with BEGIN IMMEDIATE, so no other
// connection commits while it is open: a deferred one lost to every local
// write that committed after it began. It ignores ctx's cancellation, since
// database/sql rolls a cancelled transaction back from a goroutine of its own
// that db.Close does not wait for; the statements inside still see ctx, and
// the error path rolls back before returning.
func (s *Store) begin(ctx context.Context) (storeTx, error) {
	conn, err := s.db.Conn(context.WithoutCancel(ctx))
	if err != nil {
		return storeTx{}, err
	}
	deadline := time.Now().Add(engine.BusyTimeout)
	delay := time.Millisecond
	for {
		_, err := conn.ExecContext(context.WithoutCancel(ctx), `BEGIN IMMEDIATE`)
		if err == nil {
			return storeTx{conn}, nil
		}
		if !errors.Is(err, engine.ErrBusy) || time.Now().After(deadline) || ctx.Err() != nil {
			conn.Close()
			return storeTx{}, err
		}
		time.Sleep(delay)
		delay = min(2*delay, 5*time.Millisecond)
	}
}

// storeTx is a transaction begin started.
type storeTx struct{ *sql.Conn }

func (t storeTx) Commit() error {
	defer t.Close()
	_, err := t.ExecContext(context.Background(), `COMMIT`)
	if err != nil {
		// A failed COMMIT leaves the transaction open; the connection goes
		// back to the pool, so it must not.
		t.ExecContext(context.Background(), `ROLLBACK`)
	}
	return err
}

func (t storeTx) Rollback() error {
	defer t.Close()
	_, err := t.ExecContext(context.Background(), `ROLLBACK`)
	return err
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
	s := &Store{db: db, q: db, site: site, clock: NewClock(site), vv: VersionVector{}, floors: VersionVector{}, acks: Acks{}, cat: newCatalog(), mayHold: map[string]bool{}, intern: recIntern{}}
	if err := s.claimRanges(ctx); err != nil {
		return nil, err
	}
	if err := s.loadHeld(ctx); err != nil {
		return nil, err
	}
	if err := s.loadFloors(ctx); err != nil {
		return nil, err
	}
	if err := s.loadVV(ctx); err != nil {
		return nil, err
	}
	s.ownSeq.Store(s.vv[site])
	if cat, ver, ok, err := loadCatalog(ctx, db); err != nil {
		return nil, err
	} else if ok {
		s.cat, s.ver = cat, ver
	}
	// Seed the clock past the highest HLC already durable, so a fresh local
	// write is causally after everything this database has ever seen -- the op
	// log's, which holds the schema ops, and the clock's, which holds the cells
	// of row ops long pruned from the log.
	// Two plain max()es: under a subquery, the clock's every row is loaded
	// to find one.
	for _, q := range []string{`SELECT max(hlc) FROM main._repl_oplog`, `SELECT max(top) FROM main._repl_clock`} {
		var maxHLC sql.NullInt64
		if err := db.QueryRowContext(ctx, q).Scan(&maxHLC); err != nil {
			return nil, err
		}
		if maxHLC.Valid {
			s.clock.Merge(HLC(maxHLC.Int64))
		}
	}
	return s, nil
}

// Site returns the store's site identifier.
func (s *Store) Site() string { return s.site }

// NoteLocal advances the in-memory version vector for a site to seq, used after
// a captured local write has committed its oplog rows out of band (in the user's
// transaction, on a different connection).
func (s *Store) NoteLocal(site string, seq uint64) {
	if site != s.site {
		s.mu.Lock()
		defer s.mu.Unlock()
		if seq > s.vv[site] {
			s.vv[site] = seq
		}
		return
	}
	for {
		own := s.ownSeq.Load()
		if seq <= own || s.ownSeq.CompareAndSwap(own, seq) {
			return
		}
	}
}

// foldOwn takes the local commits NoteLocal noted into vv. Called with mu held.
func (s *Store) foldOwn() {
	if own := s.ownSeq.Load(); own > s.vv[s.site] {
		s.vv[s.site] = own
	}
}

// Clock returns the store's hybrid logical clock.
func (s *Store) Clock() *Clock { return s.clock }

// LocalVV returns a copy of the version vector (max contiguous seq per site).
func (s *Store) LocalVV() VersionVector {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.foldOwn()
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
	if ops, err := s.opsUpTo(ctx, site, fromSeq, limit); err != nil || len(ops) > 0 {
		return ops, err
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
		op, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

// opsUpTo is OpsSince's ops up to the site's contiguous seq, by a point read
// each where a range of seqs would scan the whole log. It stops at the first
// seq missing (pruned meanwhile).
func (s *Store) opsUpTo(ctx context.Context, site string, fromSeq uint64, limit int) ([]Op, error) {
	s.mu.Lock()
	s.foldOwn()
	top := s.vv[site]
	s.mu.Unlock()
	if top <= fromSeq {
		return nil, nil
	}
	n := top - fromSeq
	if limit > 0 && uint64(limit) < n {
		n = uint64(limit)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ops := make([]Op, 0, n)
	for seq := fromSeq + 1; seq <= fromSeq+n; seq++ {
		op, err := scanOp(conn.QueryRowContext(ctx, `SELECT site, seq, hlc, tbl, pk, op, cells FROM main._repl_oplog WHERE site=? AND seq=?`, site, int64(seq)))
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// scanOp reads an op from a row of "SELECT site, seq, hlc, tbl, pk, op, cells".
func scanOp(r interface{ Scan(...any) error }) (Op, error) {
	var op Op
	var hlc, kind int64
	var cells []byte
	if err := r.Scan(&op.Site, &op.Seq, &hlc, &op.Tbl, &op.PK, &kind, &cells); err != nil {
		return op, err
	}
	op.HLC, op.Kind = HLC(hlc), OpKind(kind)
	var err error
	op.Cells, err = decodeCells(cells)
	return op, err
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
//
// GROUP COMMIT: an Ingest arriving while another applies waits, and the next
// transaction applies every op queued meanwhile, one commit and sync for all.
// A call that finds nothing applying leads at once. Each returns once its own
// op is durable or refused.
func (s *Store) Ingest(ctx context.Context, op Op, local bool) error {
	g := &s.group
	c := &ingestCall{op: op, local: local, done: make(chan error, 1)}
	g.mu.Lock()
	g.queue = append(g.queue, c)
	lead := !g.running
	g.running = true
	g.mu.Unlock()
	if !lead {
		if err := <-c.done; err != errIngestLead {
			return err
		}
	}
	g.mu.Lock()
	calls := g.queue
	g.queue = nil
	g.mu.Unlock()
	s.applyCalls(ctx, calls)
	// The oldest call that queued meanwhile leads the next transaction, so
	// none waits on a leader that has gone back to its caller.
	g.mu.Lock()
	if len(g.queue) > 0 {
		g.queue[0].done <- errIngestLead
	} else {
		g.running = false
	}
	g.mu.Unlock()
	return <-c.done
}

// ingestGroup is the Ingest calls waiting for the next transaction, and
// whether one is leading it (Ingest).
type ingestGroup struct {
	mu      sync.Mutex
	running bool
	queue   []*ingestCall
}

// ingestCall is one Ingest waiting for its op's transaction: done gets its
// result, or errIngestLead when it is to lead the next one.
type ingestCall struct {
	op    Op
	local bool
	done  chan error
}

var errIngestLead = errors.New("replication: lead the next ingest")

// applyCalls applies the queued ops in order, a run of one local flag to a
// transaction, and hands each call its result; a refused op does not stop the
// rest. The leader's cancellation does not end the others' transaction.
func (s *Store) applyCalls(ctx context.Context, calls []*ingestCall) {
	ctx = context.WithoutCancel(ctx)
	for len(calls) > 0 {
		n := 1
		for n < len(calls) && calls[n].local == calls[0].local {
			n++
		}
		run := calls[:n]
		calls = calls[n:]
		ops := make([]Op, len(run))
		for i, c := range run {
			ops[i] = c.op
		}
		for len(run) > 0 {
			k, err := s.ingestAllAs(ctx, ops, run[0].local)
			if err == nil || k < 0 {
				for _, c := range run {
					c.done <- err
				}
				break
			}
			for _, c := range run[:k] {
				c.done <- nil
			}
			run[k].done <- err
			run, ops = run[k+1:], ops[k+1:]
		}
	}
}

// ingestAll applies ops in order, as Ingest one at a time would, a few
// hundred to a transaction instead of one each. When ops[k] is refused it
// returns k and that error, with ops[:k] applied and the rest not tried; on an
// error no op caused, -1, and the ops before it may have applied.
func (s *Store) ingestAll(ctx context.Context, ops []Op) (int, error) {
	return s.ingestAllAs(ctx, ops, false)
}

// ingestAllAs is ingestAll for ops that are all local, or all not (Ingest).
func (s *Store) ingestAllAs(ctx context.Context, ops []Op, local bool) (int, error) {
	done := 0
	for done < len(ops) {
		n, err := s.ingest(ctx, ops[done:], local)
		switch {
		case err == nil:
			done += n
			continue
		case n < 0:
			return -1, err
		case n > 0:
			// The refused op rolled back the ones before it, which apply alone.
			if _, perr := s.ingestAllAs(ctx, ops[done:done+n], local); perr != nil {
				return -1, perr
			}
		}
		return done + n, err
	}
	return len(ops), nil
}

// maxHold is how long an ingest holds the write lock before it commits and
// lets a local writer in, and holdAlone how long when no local commit landed
// for localQuiet: a commit costs much the same however many ops it carries.
const (
	maxHold    = 50 * time.Millisecond
	holdAlone  = 500 * time.Millisecond
	localQuiet = time.Second
	// yieldHold is the least an ingest holds the lock once a local write waits:
	// each turn costs it more than its commit. Beside two writers, 10 ms gave
	// the ingest 1,700 ops/s and 30 ms gave it 4,000.
	yieldHold = 30 * time.Millisecond
)

// hold is how long an ingest may hold the write lock now.
func (s *Store) hold() time.Duration {
	if time.Since(time.Unix(0, s.lastLocal.Load())) > localQuiet {
		return holdAlone
	}
	return maxHold
}

// localCommitted notes a local commit, for hold.
func (s *Store) localCommitted() { s.lastLocal.Store(time.Now().UnixNano()) }

// waitLocal holds an apply off while a local commit that failed after its
// capture is being retried: the driver re-runs an autocommit statement whole,
// and a long one would never fit between applies. After an apply yielded the
// lock to waiting local writes, it also waits for each of them to take it.
// Both waits end engine.BusyTimeout after they began.
func (s *Store) waitLocal() {
	until := time.Now().Add(engine.BusyTimeout)
	for {
		free := s.localChange()
		left := time.Until(time.Unix(0, s.localUntil.Load()))
		if s.localRetries.Load() <= 0 || left <= 0 {
			if left = time.Until(until); s.localGot.Load() >= s.yieldTo.Load() || left <= 0 {
				return
			}
		}
		t := time.NewTimer(left)
		select {
		case <-free:
		case <-t.C:
		}
		t.Stop()
	}
}

// localFailed and localDone bracket a local commit's retries (capture).
func (s *Store) localFailed() {
	s.localUntil.Store(time.Now().Add(engine.BusyTimeout).UnixNano())
	s.localRetries.Add(1)
}

func (s *Store) localDone() {
	s.localRetries.Add(-1)
	s.wakeLocal()
}

// waitIngest lets an ingest waiting for the write lock take it ahead of a
// local write, waiting at most holdAlone: the lock is not handed over in turn.
func (s *Store) waitIngest() {
	got, until := s.ingestGot.Load(), time.Now().Add(holdAlone)
	for {
		free := s.localChange()
		left := time.Until(until)
		if s.ingestWaiting.Load() <= 0 || s.ingestGot.Load() != got || left <= 0 {
			return
		}
		t := time.NewTimer(left)
		select {
		case <-free:
		case <-t.C:
		}
		t.Stop()
	}
}

// ingestWaits reports whether an ingest waits for the write lock (capture).
func (s *Store) ingestWaits() bool { return s.ingestWaiting.Load() > 0 }

// localLockWait brackets a local write's wait for the write lock (capture).
func (s *Store) localLockWait(waiting bool) {
	if waiting {
		s.localWaiting.Add(1)
		s.waitIngest()
		return
	}
	s.localGot.Add(1)
	s.localWaiting.Add(-1)
	s.wakeLocal()
}

// localChange is the channel wakeLocal closes next.
func (s *Store) localChange() chan struct{} {
	s.localMu.Lock()
	defer s.localMu.Unlock()
	if s.localFree == nil {
		s.localFree = make(chan struct{})
	}
	return s.localFree
}

// wakeLocal wakes waitLocal and waitIngest to look again.
func (s *Store) wakeLocal() {
	s.localMu.Lock()
	if s.localFree != nil {
		close(s.localFree)
		s.localFree = nil
	}
	s.localMu.Unlock()
}

// ingest applies ops, from the first, in one transaction, until they are done
// or it has held the write lock for maxHold, and reports how many it took. On
// an error it applies none of them, and reports the index of the op refused,
// or -1 when no op was.
func (s *Store) ingest(ctx context.Context, ops []Op, local bool) (int, error) {
	s.waitLocal()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.foldOwn()

	if s.held(ctx, ops) {
		return len(ops), nil
	}
	n, err := s.ingestOnce(ctx, ops, local, true)
	if errors.Is(err, errRunRefused) {
		// Which op the run failed on is not known: they apply again one at a
		// time, which finds it and applies the ones before it.
		n, err = s.ingestOnce(ctx, ops, local, false)
	}
	return n, err
}

// errRunRefused is ingestOnce's answer for a batch transaction that failed and
// rolled back.
var errRunRefused = errors.New("replication: a run of ops was refused")

// ingestOnce is ingest's transaction, with ingestTx's batch.
func (s *Store) ingestOnce(ctx context.Context, ops []Op, local, batch bool) (int, error) {
	s.ingestWaiting.Add(1)
	tx, err := s.begin(ctx)
	s.ingestGot.Add(1)
	s.ingestWaiting.Add(-1)
	s.wakeLocal()
	if err != nil {
		return -1, err
	}
	s.q, s.txHeld = tx, map[string]bool{}
	cat, ver := s.cat, s.ver
	added, n, err := s.ingestTx(ctx, ops, local, batch)
	s.q, s.txHeld = s.db, nil
	if err != nil {
		tx.Rollback()
		s.cat, s.ver = cat, ver
		if batch {
			return 0, errRunRefused
		}
		return n, err
	}
	if err := tx.Commit(); err != nil {
		s.cat, s.ver = cat, ver
		return -1, err
	}
	s.applied.Add(1)
	for site, seqs := range added {
		if err := s.advanceVV(ctx, site, seqs); err != nil {
			return -1, err
		}
	}
	return n, nil
}

// held reports whether every op is one the log holds already (or pruned), as
// a peer's push and an anti-entropy pull of the same op make common: those
// need no transaction. An op past its site's contiguous seq may be new.
func (s *Store) held(ctx context.Context, ops []Op) bool {
	for _, op := range ops {
		if op.Seq > s.vv[op.Site] {
			return false
		}
	}
	for _, op := range ops {
		if op.Seq <= s.floors[op.Site] && op.Kind != OpSchema {
			continue
		}
		if same, exists, err := s.sameOp(ctx, op); err != nil || !exists || !same {
			return false
		}
	}
	return true
}

// ingestTx is ingest's transaction: the seqs it added to the log, by site, and
// how many ops it took, or the index of the op refused. With batch, the data
// ops between two schema ops merge a run at a time (applyOps), and an error
// names no op.
func (s *Store) ingestTx(ctx context.Context, ops []Op, local, batch bool) (map[string]map[uint64]bool, int, error) {
	// A local capture may have changed the schema since this store last looked;
	// none can while the transaction holds the write lock.
	if err := s.syncCatalog(ctx); err != nil {
		return nil, -1, err
	}
	added := map[string]map[uint64]bool{}
	var run []Op
	flush := func() error {
		err := s.applyOps(ctx, run, local)
		run = run[:0]
		return err
	}
	start, hold := time.Now(), s.hold()
	for i, op := range ops {
		if i > 0 && time.Since(start) > hold {
			return added, i, flush()
		}
		if w := s.localWaiting.Load(); i > 0 && w > 0 && time.Since(start) > yieldHold {
			s.yieldTo.Store(s.localGot.Load() + uint64(w))
			return added, i, flush()
		}
		if op.Seq <= s.floors[op.Site] && op.Kind != OpSchema {
			// Pruned here: applied long ago, and everywhere. A schema op is never
			// pruned, so one at or below a floor that is not in the log is one this
			// node has not seen -- a snapshot can raise a floor past it.
			continue
		}
		if fresh, err := s.appendOplog(ctx, op); err != nil {
			return nil, i, err
		} else if !fresh {
			same, _, err := s.sameOp(ctx, op)
			if err != nil {
				return nil, i, err
			}
			if !same {
				// Two nodes are writing under one site id (a copied database file):
				// their ops at one seq differ, and each peer keeps whichever it saw
				// first.
				return nil, i, fmt.Errorf("replication: site %q wrote two different ops at seq %d; two nodes share that site id", op.Site, op.Seq)
			}
			continue
		}
		// Advance our clock past the op so the next local write is causally after it.
		s.clock.Merge(op.HLC)
		if added[op.Site] == nil {
			if err := s.claimRange(ctx, op.Site); err != nil {
				return nil, i, err
			}
			added[op.Site] = map[uint64]bool{}
		}
		added[op.Site][op.Seq] = true
		if s.seenRowid != nil && op.Kind != OpSchema {
			if v, ok := driverValue(unpackVal(op.PK)).(int64); ok {
				s.seenRowid(op.Tbl, v)
			}
		}
		if batch && op.Kind != OpSchema {
			if run = append(run, op); len(run) < applyRunMax {
				continue
			}
			if err := flush(); err != nil {
				return nil, i, err
			}
			continue
		}
		if err := flush(); err != nil { // a schema op applies after every op before it
			return nil, i, err
		}
		if err := s.applyOp(ctx, op, local); err != nil {
			return nil, i, err
		}
	}
	return added, len(ops), flush()
}

// applyOp applies op, already in the log, to the clock and the user tables.
func (s *Store) applyOp(ctx context.Context, op Op, local bool) error {
	if op.Kind == OpSchema {
		clear(s.txHeld)
		cat, ver, err := s.applySchema(ctx, op)
		if err == nil && cat != nil {
			s.cat, s.ver = cat, ver
		}
		return err
	}
	// Per-cell merge into the clock, presence first. Cells merge even into a
	// currently-tombstoned row (grow-only) so a later resurrection sees them --
	// and into a table or column not known here yet, whose rows are withheld
	// until the schema op that makes it arrives (applySchema materializes them).
	have, err := s.mergeOp(ctx, op)
	if err != nil || local {
		return err
	}
	return s.materializeRowFrom(ctx, s.cat, op.Tbl, op.PK, have)
}

// clockCell is one cell of a row's clock record.
type clockCell struct {
	hlc  HLC
	site string
	val  []byte
}

// mergeOp merges op's presence and cells into the clock, each iff op is a
// strictly newer writer than the cell's, by the (hlc, site) total order: one
// read of the row's record and, when op won a cell of it, one write. It
// returns the row's cells after.
func (s *Store) mergeOp(ctx context.Context, op Op) (map[string]clockCell, error) {
	have, err := s.clockCells(ctx, op.Tbl, op.PK)
	if err != nil {
		return nil, err
	}
	had := len(have) > 0
	if !mergeInto(have, op) {
		return have, nil
	}
	fresh, won := recArgs(nil, nil, op.Tbl, op.PK, have, had)
	return have, s.writeRecs(ctx, fresh, won)
}

// clockCells is the clock's cells of tbl's row pk, by column.
func (s *Store) clockCells(ctx context.Context, tbl string, pk []byte) (map[string]clockCell, error) {
	return readRec(ctx, s.q, tbl, pk)
}

// mergeInto merges op's presence and cells into have, each iff op is a
// strictly newer writer than the cell's, and reports whether it took any.
func mergeInto(have map[string]clockCell, op Op) bool {
	took := false
	merge := func(col string, val []byte) {
		if c, ok := have[col]; ok && !TagLess(c.hlc, c.site, op.HLC, op.Site) {
			return // the existing writer wins
		}
		have[col] = clockCell{op.HLC, op.Site, val}
		took = true
	}
	alive := 1
	if op.Kind == OpDelete {
		alive = 0
	}
	merge(presenceCol, presenceVals[alive])
	// Every cell of the op packs into one buffer, each its own capped slice.
	n := 0
	for _, c := range op.Cells {
		n += 1 + len(c.Val)
	}
	buf := make([]byte, 0, n)
	for _, c := range op.Cells {
		at := len(buf)
		buf = append(append(buf, c.Type), c.Val...)
		merge(c.Col, buf[at:len(buf):len(buf)])
	}
	return took
}

// presenceVals is a presence cell's value, dead and alive: shared, and never
// written to.
var presenceVals = [2][]byte{{0}, {1}}

// recsPerStmt is the most clock records one statement of writeRecs writes.
const recsPerStmt = 128

// writeRecs writes clock records (recArgs): a plain INSERT of new ones, since
// an upsert costs the engine several times an insert, and an upsert of the
// rest.
func (s *Store) writeRecs(ctx context.Context, fresh, won []any) error {
	const insert = `INSERT INTO main._repl_clock(tbl,pk,top,rec) VALUES `
	const update = ` ON CONFLICT(tbl,pk) DO UPDATE SET top=excluded.top, rec=excluded.rec`
	for _, w := range []struct {
		args []any
		tail string
	}{{fresh, ""}, {won, update}} {
		for args := w.args; len(args) > 0; {
			n := min(len(args)/4, recsPerStmt)
			if _, err := s.q.ExecContext(ctx, insert+strings.TrimSuffix(strings.Repeat(`(?,?,?,?),`, n), ",")+w.tail, args[:4*n]...); err != nil {
				return err
			}
			args = args[4*n:]
		}
	}
	return nil
}

// applyRunMax is the most data ops ingestTx merges in one run (applyOps).
const applyRunMax = 256

// applyOps is applyOp for a run of data ops already in the log: each row's ops
// merge into its clock cells in memory, the changed cells and rows are written
// once for the run, many to a statement, and a table that holds rows out or
// refuses a UNIQUE value is rebuilt from the clock afterwards.
func (s *Store) applyOps(ctx context.Context, ops []Op, local bool) error {
	if len(ops) == 0 {
		return nil
	}
	type key struct{ tbl, pk string }
	var keys []key
	var tables []string
	byKey := map[key][]Op{}
	pksOf := map[string][][]byte{}
	for _, op := range ops {
		k := key{op.Tbl, string(op.PK)}
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
			if _, ok := pksOf[op.Tbl]; !ok {
				tables = append(tables, op.Tbl)
			}
			pksOf[op.Tbl] = append(pksOf[op.Tbl], op.PK)
		}
		byKey[k] = append(byKey[k], op)
	}
	clock := map[key]map[string]clockCell{}
	for _, tbl := range tables {
		if err := s.readRecs(ctx, tbl, pksOf[tbl], func(pk []byte, cells map[string]clockCell) {
			clock[key{tbl, string(pk)}] = cells
		}); err != nil {
			return err
		}
	}
	var fresh, won []any
	changed := map[key]map[string]clockCell{}
	for _, k := range keys {
		have, had := clock[k]
		if !had {
			have = map[string]clockCell{}
		}
		took := false
		for _, op := range byKey[k] {
			took = mergeInto(have, op) || took
		}
		if took {
			fresh, won = recArgs(fresh, won, k.tbl, []byte(k.pk), have, had)
			changed[k] = have
		}
	}
	if err := s.writeRecs(ctx, fresh, won); err != nil || local {
		return err
	}
	live := map[string]typedVal{}
	for _, tbl := range tables {
		t := s.cat.byID(tbl)
		if t == nil || t.Type != "table" {
			continue
		}
		conflict, err := s.heldInTx(ctx, tbl)
		if err == nil && !conflict {
			conflict, err = s.writeRows(ctx, t, pksOf[tbl], func(pk []byte) map[string]clockCell {
				return changed[key{tbl, string(pk)}]
			}, live)
		}
		if err != nil {
			return err
		}
		if conflict {
			if err := s.rebuildTable(ctx, t); err != nil {
				return err
			}
			delete(s.txHeld, tbl) // the rebuild may have held rows out
		}
	}
	return nil
}

// readRecs hands got the clock's cells of each of tbl's rows pks it holds a
// record of, by a point read each.
func (s *Store) readRecs(ctx context.Context, tbl string, pks [][]byte, got func(pk []byte, cells map[string]clockCell)) error {
	for _, pk := range pks {
		var rec []byte
		err := s.q.QueryRowContext(ctx, `SELECT rec FROM main._repl_clock WHERE tbl=? AND pk=?`, tbl, pk).Scan(&rec)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		cells, err := decodeRecIn(rec, s.intern)
		if err != nil {
			return err
		}
		got(pk, cells)
	}
	return nil
}

// writeRows is writeRow for the rows of t among pks whose cells changed (an
// unchanged one has none): each one's row deleted, then every alive, complete
// one inserted, many to a statement. conflict reports a row the table refused.
func (s *Store) writeRows(ctx context.Context, t *object, pks [][]byte, cellsOf func(pk []byte) map[string]clockCell, live map[string]typedVal) (conflict bool, err error) {
	keyNames, err := keyNamesOf(t)
	if err != nil {
		return false, err
	}
	where := make([]string, len(keyNames))
	for i, k := range keyNames {
		where[i] = quoteIdent(k) + "=?"
	}
	del := fmt.Sprintf(`DELETE FROM main.%s WHERE %s`, quoteIdent(t.Name), strings.Join(where, " AND "))
	ins := rowInserter{s: s, t: t}
	for _, pk := range pks {
		have := cellsOf(pk)
		if have == nil {
			continue
		}
		keyVals, err := decodeKey(pk, len(keyNames))
		if err != nil {
			return false, fmt.Errorf("replication: table %q: %w", t.Name, err)
		}
		if _, err := s.q.ExecContext(ctx, del, keyVals...); err != nil {
			return false, err
		}
		if !liveCellsInto(have, live) {
			continue
		}
		cols, args, ok := rowValues(t, keyNames, keyVals, live)
		if !ok {
			continue // completeness gate: withhold until the covering insert arrives
		}
		if err := ins.add(ctx, cols, args); err != nil {
			return false, err
		}
	}
	if err := ins.flush(ctx); err != nil {
		return false, err
	}
	return ins.refused, nil
}

// rowInserter inserts rows of one table many to a statement, rows of one
// column list together, as writeRow inserts one: OR IGNORE, the rows being
// gone already, so that refused is whether a UNIQUE value refused any.
type rowInserter struct {
	s       *Store
	t       *object
	cols    []string
	args    []any
	n       int
	refused bool
}

func (b *rowInserter) add(ctx context.Context, cols []string, args []any) error {
	if b.n > 0 && (!slices.Equal(cols, b.cols) || b.n == rowsPerInsert || len(b.args)+len(args) > sqliteMaxVariables) {
		if err := b.flush(ctx); err != nil {
			return err
		}
	}
	b.cols = cols
	b.args = append(b.args, args...)
	b.n++
	return nil
}

func (b *rowInserter) flush(ctx context.Context) error {
	if b.n == 0 {
		return nil
	}
	inserted, err := b.s.insertRows(ctx, b.t, b.cols, b.n, b.args)
	if err != nil {
		return err
	}
	b.refused = b.refused || inserted < b.n
	b.args, b.n = b.args[:0], 0
	return nil
}

// insertRows inserts n rows of t, OR IGNORE, each of cols from args in turn,
// and says how many went in.
func (s *Store) insertRows(ctx context.Context, t *object, cols []string, n int, args []any) (int, error) {
	one := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"
	res, err := s.q.ExecContext(ctx, fmt.Sprintf(`INSERT OR IGNORE INTO main.%s(%s) VALUES %s`,
		quoteIdent(t.Name), quotedList(cols), strings.TrimSuffix(strings.Repeat(one+",", n), ",")), args...)
	if err != nil {
		return 0, err
	}
	inserted, err := res.RowsAffected()
	return int(inserted), err
}

// mergeCell writes (hlc, site, val) for a cell iff it is a strictly newer writer
// than what the clock holds, by the (hlc, site) total order.
func (s *Store) mergeCell(ctx context.Context, tbl string, pk []byte, col string, hlc HLC, site string, val []byte) error {
	have, err := s.clockCells(ctx, tbl, pk)
	if err != nil {
		return err
	}
	if c, ok := have[col]; ok && !TagLess(c.hlc, c.site, hlc, site) {
		return nil // existing writer wins; discard
	}
	had := len(have) > 0
	have[col] = clockCell{hlc, site, val}
	fresh, won := recArgs(nil, nil, tbl, pk, have, had)
	return s.writeRecs(ctx, fresh, won)
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

// materializeRowFrom brings one user-table row to what the clock says, from
// the row's clock cells when the caller has them (mergeOp): gone if the row is
// tombstoned or unknown, otherwise the full row from its live cells, but only
// once every NOT NULL column without a default has a value (the completeness
// gate), so an update that races its insert never writes a NULL there or a
// torn row. A row that would break a UNIQUE index is settled by rebuilding the
// table (rebuildTable), as is every change to a table already holding a row
// out, so what the table holds is a function of the clock alone. tblID and the
// cells' columns are IDs resolved through cat; what cat lacks is not written.
func (s *Store) materializeRowFrom(ctx context.Context, cat *catalog, tblID string, pk []byte, have map[string]clockCell) error {
	t := cat.byID(tblID)
	if t == nil || t.Type != "table" {
		return nil
	}
	held, err := s.heldInTx(ctx, tblID)
	if err != nil {
		return err
	}
	if held {
		return s.rebuildTable(ctx, t)
	}
	if conflict, err := s.writeRow(ctx, t, pk, true, have); err != nil || !conflict {
		return err
	}
	return s.rebuildTable(ctx, t)
}

// writeRow writes pk's row as the clock has it, first deleting the one there
// when del is set. conflict reports an alive, complete row the table refused:
// one of its UNIQUE values is another row's.
func (s *Store) writeRow(ctx context.Context, t *object, pk []byte, del bool, have map[string]clockCell) (conflict bool, err error) {
	keyNames, err := keyNamesOf(t)
	if err != nil {
		return false, err
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

	if have == nil {
		if have, err = s.clockCells(ctx, t.ID, pk); err != nil {
			return false, err
		}
	}
	cells, alive := liveCells(have)
	if !alive {
		return false, nil
	}
	colNames, args, ok := rowValues(t, keyNames, keyVals, cells)
	if !ok {
		return false, nil // completeness gate: withhold until the covering insert arrives
	}
	// OR IGNORE: the key's own row is gone (deleted above, or the table is
	// being rebuilt), so the only conflict left is a UNIQUE one.
	res, err := s.q.ExecContext(ctx, fmt.Sprintf(`INSERT OR IGNORE INTO main.%s(%s) VALUES(%s)`,
		quoteIdent(t.Name), quotedList(colNames), strings.TrimSuffix(strings.Repeat("?,", len(colNames)), ",")), args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 0, err
}

// keyNamesOf is the columns t's rows are keyed by: its key's, or the name
// that reaches its rowid.
func keyNamesOf(t *object) ([]string, error) {
	var keyNames []string
	for _, kc := range t.keyCols() {
		keyNames = append(keyNames, kc.Name)
	}
	if keyNames == nil {
		rid := rowidName(t.Cols)
		if rid == "" {
			return nil, fmt.Errorf("replication: table %q: columns shadow every name for the rowid", t.Name)
		}
		keyNames = []string{rid}
	}
	return keyNames, nil
}

// rowValues is what an INSERT of a row with these cells names: its key, then
// every column that has a cell, in schema order (a generated column has none),
// with their values. ok is false while a NOT NULL column without a default has
// no cell yet: the completeness gate.
func rowValues(t *object, keyNames []string, keyVals []any, cells map[string]typedVal) (cols []string, args []any, ok bool) {
	for _, c := range t.Cols {
		if c.PK || c.Gen || !c.NotNull || c.Default {
			continue
		}
		if _, ok := cells[c.ID]; !ok {
			return nil, nil, false
		}
	}
	cols = append(make([]string, 0, len(keyNames)+len(cells)), keyNames...)
	args = append(make([]any, 0, len(keyVals)+len(cells)), keyVals...)
	for _, c := range t.Cols {
		if c.PK {
			continue
		}
		if v, ok := cells[c.ID]; ok {
			cols = append(cols, c.Name)
			args = append(args, driverValue(v.typ, v.raw))
		}
	}
	return cols, args, true
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = quoteIdent(n)
	}
	return strings.Join(quoted, ",")
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
	var unique int
	if err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM pragma_index_list(?, 'main') WHERE "unique"`, t.Name).Scan(&unique); err != nil {
		return err
	}
	if unique == 0 {
		return s.rewriteRows(ctx, t)
	}
	type stamp struct {
		pk   []byte
		hlc  int64
		site string
	}
	// Each row's latest cell; the site breaks an HLC tie, as it does everywhere
	// else (TagLess).
	rows, err := s.q.QueryContext(ctx, `SELECT pk, rec FROM main._repl_clock WHERE tbl=?`, t.ID)
	if err != nil {
		return err
	}
	var all []stamp
	for rows.Next() {
		var st stamp
		var rec []byte
		if err := rows.Scan(&st.pk, &rec); err != nil {
			rows.Close()
			return err
		}
		cells, err := decodeRec(rec)
		if err != nil {
			rows.Close()
			return err
		}
		first := true
		for _, c := range cells {
			if first || TagLess(HLC(st.hlc), st.site, c.hlc, c.site) {
				st.hlc, st.site, first = int64(c.hlc), c.site, false
			}
		}
		all = append(all, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
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
		conflict, err := s.writeRow(ctx, t, st.pk, false, nil)
		if err != nil {
			return err
		}
		if conflict {
			if err := s.holdOut(ctx, t.ID, st.pk); err != nil {
				return err
			}
		}
	}
	return nil
}

// holdOut records pk's row of tbl as held out.
func (s *Store) holdOut(ctx context.Context, tbl string, pk []byte) error {
	s.mayHold[tbl] = true
	if s.txHeld != nil {
		s.txHeld[tbl] = true
	}
	s.anyHeld.Store(true)
	_, err := s.q.ExecContext(ctx, `INSERT INTO main._repl_withheld(tbl, pk) VALUES(?, ?)`, tbl, pk)
	return err
}

// heldInTx is holdsOut, asked once per table in an ingest's transaction.
func (s *Store) heldInTx(ctx context.Context, tbl string) (bool, error) {
	held, ok := s.txHeld[tbl]
	if !ok {
		var err error
		if held, err = s.holdsOut(ctx, tbl); err != nil {
			return false, err
		}
		if s.txHeld != nil {
			s.txHeld[tbl] = held
		}
	}
	return held, nil
}

// holdsOut reports whether tbl has a row held out.
func (s *Store) holdsOut(ctx context.Context, tbl string) (bool, error) {
	if !s.mayHold[tbl] {
		return false, nil
	}
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM main._repl_withheld WHERE tbl=? LIMIT 1)`, tbl).Scan(&n)
	return n > 0, err
}

// loadHeld fills mayHold from _repl_withheld.
func (s *Store) loadHeld(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT tbl FROM main._repl_withheld`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tbl string
		if err := rows.Scan(&tbl); err != nil {
			return err
		}
		s.mayHold[tbl] = true
		s.anyHeld.Store(true)
	}
	return rows.Err()
}

// rewriteRows is rebuildTable for a table with no UNIQUE index: the order rows
// go in does not matter, so they go in key order, many to a statement.
func (s *Store) rewriteRows(ctx context.Context, t *object) error {
	keyNames, err := keyNamesOf(t)
	if err != nil {
		return err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT pk, rec FROM main._repl_clock WHERE tbl=? ORDER BY pk`, t.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	b := rowBatch{s: s, t: t, keyNames: keyNames}
	for rows.Next() {
		var pk, rec []byte
		if err := rows.Scan(&pk, &rec); err != nil {
			return err
		}
		have, err := decodeRec(rec)
		if err != nil {
			return err
		}
		cells, alive := liveCells(have)
		if !alive {
			continue
		}
		keyVals, err := decodeKey(pk, len(keyNames))
		if err != nil {
			return fmt.Errorf("replication: table %q: %w", t.Name, err)
		}
		cols, args, ok := rowValues(t, keyNames, keyVals, cells)
		if !ok {
			continue
		}
		if err := b.add(ctx, pk, cols, args); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return b.flush(ctx)
}

// rowsPerInsert caps the rows of one rowBatch INSERT, and sqliteMaxVariables
// its parameters: SQLITE_LIMIT_VARIABLE_NUMBER's default, which the engine
// keeps.
const (
	rowsPerInsert      = 64
	sqliteMaxVariables = 32766
)

// rowBatch writes rows of one table, many to an INSERT while they name the
// same columns.
type rowBatch struct {
	s        *Store
	t        *object
	keyNames []string
	cols     []string
	pks      [][]byte
	args     []any
}

func (b *rowBatch) add(ctx context.Context, pk []byte, cols []string, args []any) error {
	if len(b.pks) > 0 && (!slices.Equal(cols, b.cols) || len(b.pks) == rowsPerInsert || len(b.args)+len(args) > sqliteMaxVariables) {
		if err := b.flush(ctx); err != nil {
			return err
		}
	}
	b.cols = cols
	b.pks = append(b.pks, pk)
	b.args = append(b.args, args...)
	return nil
}

// flush writes the batch. A row the table refused -- it has no UNIQUE index,
// so a NULL in a NOT NULL column -- is held out, as rebuildTable holds out a
// row it refuses.
func (b *rowBatch) flush(ctx context.Context) error {
	if len(b.pks) == 0 {
		return nil
	}
	n, err := b.s.insertRows(ctx, b.t, b.cols, len(b.pks), b.args)
	if err != nil {
		return err
	}
	if n < len(b.pks) {
		where := make([]string, len(b.keyNames))
		for i, k := range b.keyNames {
			where[i] = quoteIdent(k) + "=?"
		}
		have := fmt.Sprintf(`SELECT count(*) FROM main.%s WHERE %s`, quoteIdent(b.t.Name), strings.Join(where, " AND "))
		for _, pk := range b.pks {
			keyVals, err := decodeKey(pk, len(b.keyNames))
			if err != nil {
				return err
			}
			var k int
			if err := b.s.q.QueryRowContext(ctx, have, keyVals...).Scan(&k); err != nil {
				return err
			}
			if k == 0 {
				if err := b.s.holdOut(ctx, b.t.ID, pk); err != nil {
					return err
				}
			}
		}
	}
	b.pks, b.args = b.pks[:0], b.args[:0]
	return nil
}

// rebuildHeld rebuilds every table among tblIDs that has a row held out by a
// UNIQUE conflict: a local commit may have freed the value, and the next remote
// op to the table may be a long way off.
func (s *Store) rebuildHeld(ctx context.Context, tblIDs []string) error {
	if !s.anyHeld.Load() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A table with no row held out needs no transaction, which every local
	// commit would otherwise pay; and one never held out, not even a query.
	heldOut := false
	for _, id := range tblIDs {
		held, err := s.holdsOut(ctx, id)
		if err != nil {
			return err
		}
		heldOut = heldOut || held
	}
	if !heldOut {
		return nil
	}
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
			held, err := s.holdsOut(ctx, id)
			if err != nil {
				return err
			}
			if t := s.cat.byID(id); held && t != nil {
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

type typedVal struct {
	typ byte
	raw []byte
}

// liveCells is a row's cells but its presence, and whether that says the row
// is there: false for a deleted row and for one the clock does not know.
func liveCells(have map[string]clockCell) (map[string]typedVal, bool) {
	cells := make(map[string]typedVal, len(have))
	if !liveCellsInto(have, cells) {
		return nil, false
	}
	return cells, true
}

// liveCellsInto is liveCells into cells, which it clears first.
func liveCellsInto(have map[string]clockCell, cells map[string]typedVal) bool {
	clear(cells)
	if p, ok := have[presenceCol]; !ok || len(p.val) == 0 || p.val[0] != 1 {
		return false
	}
	for col, c := range have {
		if col != presenceCol {
			typ, raw := unpackVal(c.val)
			cells[col] = typedVal{typ, raw}
		}
	}
	return true
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
// A deliberate simplification: containment, not convergence -- two nodes that saw the two sites in
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

// appendOplog adds op to the log, reporting false when the log held its
// (site, seq) already.
func (s *Store) appendOplog(ctx context.Context, op Op) (bool, error) {
	res, err := s.q.ExecContext(ctx,
		`INSERT OR IGNORE INTO main._repl_oplog(site,seq,hlc,tbl,pk,op,cells) VALUES(?,?,?,?,?,?,?)`,
		op.Site, op.Seq, int64(op.HLC), op.Tbl, op.PK, int64(op.Kind), encodeCells(op.Cells))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// advanceVV moves site's contiguous seq over every seq the log holds past it:
// added are seqs just committed, known without a read.
func (s *Store) advanceVV(ctx context.Context, site string, added map[uint64]bool) error {
	next := s.vv[site] + 1
	for {
		if !added[next] {
			ok, err := s.hasOp(ctx, site, next)
			if err != nil {
				return err
			}
			if !ok {
				break
			}
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
	b := make([]byte, 1+len(raw))
	b[0] = typ
	copy(b[1:], raw)
	return b
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
		cells, err := readRec(ctx, db, h.tbl, h.pk)
		if err != nil {
			return nil, err
		}
		for col, cell := range cells {
			if c := t.col(col); c != nil && col != presenceCol {
				w.Row[c.Name] = driverValue(unpackVal(cell.val))
			}
		}
		for i, kc := range t.keyCols() {
			w.Row[kc.Name] = key[i]
		}
		out = append(out, w)
	}
	return out, nil
}
