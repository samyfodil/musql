// N4 is a concurrent writer/reader differential harness that records
// successful statements in commit order, replays the ledger on a fresh database,
// and verifies the rows match exactly.
package engine_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/internal/filelock"
)

// ---------------------------------------------------------------------
// The ledger (design doc point 3): (seq, connID, sql, args), recorded ONLY
// for statements that reported SUCCESS, in the order they actually
// completed (not the order they were issued) -- this is what makes the
// post-run replay (checkLedgerReplays) meaningful: replaying exactly what
// succeeded, in commit order, against a fresh database must reproduce the
// concurrent run's rows exactly (design doc C6,
// section 8's own "record ONLY the commits that actually reported
// success; replay exactly those, in that order, single-threaded" --
// otherwise this is the "worker double-execution trap" shape, diffing a
// tally instead of a case list).
// ---------------------------------------------------------------------

type n4LedgerEntry struct {
	seq    int
	connID int
	sql    string
	args   []any
}

type n4Ledger struct {
	mu      sync.Mutex
	entries []n4LedgerEntry
}

// recordCommits records every commit in order via CommitOrderHookForTest and returns a stop func.
func (l *n4Ledger) recordCommits() (stop func()) {
	engine.CommitOrderHookForTest = func(sqlText string, args []engine.Value) {
		if sqlText == "" {
			return
		}
		l.record(-1, sqlText, n4AnyArgs(args))
	}
	return func() { engine.CommitOrderHookForTest = nil }
}

// n4AnyArgs converts engine.Value args to driver-level form.
func n4AnyArgs(args []engine.Value) []any {
	out := make([]any, 0, len(args))
	for _, v := range args {
		switch v.Typ {
		case engine.Int:
			out = append(out, v.I)
		case engine.Float:
			out = append(out, v.F)
		case engine.Text:
			out = append(out, string(v.S))
		case engine.Blob:
			out = append(out, append([]byte(nil), v.S...))
		default:
			out = append(out, nil)
		}
	}
	return out
}

func (l *n4Ledger) record(connID int, sqlText string, args []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, n4LedgerEntry{seq: len(l.entries), connID: connID, sql: sqlText, args: args})
}

func (l *n4Ledger) snapshot() []n4LedgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]n4LedgerEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// ---------------------------------------------------------------------
// Outcome classification (design doc point 9, hazard X21): every
// non-success outcome must land in a NAMED bucket, and "other" must stay
// empty. Aggregate atomic counters are sufficient -- the design doc asks for
// a published TALLY, not per-attempt attribution -- so no goroutine-local
// correlation is needed even though multiple writer goroutines call
// commit truly concurrently.
// ---------------------------------------------------------------------

type n4Tally struct {
	writeSuccess         int64
	writeBusyStale       int64 // another connection committed while this transaction was open
	writeBusySegmentLock int64 // segment exclusive lock timeout
	writeBusyOther       int64 // other busy errors (must be empty)
	writeOtherError      int64 // non-busy write failures (must be empty)
	readSuccess          int64
	readBusyShared       int64 // busy acquiring SHARED lock
	readOtherError       int64 // non-busy read failures (must be empty)
	busyRetryAttempts    int64 // hidden retries absorbed by busyRetry
	errMu                sync.Mutex
	firstReadErrMsg      string
	firstWriteErrMsg     string
}

// recordFirstErr keeps the first non-ErrBusy error text seen for a kind.
func (t *n4Tally) recordFirstErr(dst *string, err error) {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	if *dst == "" {
		*dst = err.Error()
	}
}

const staleImageMsgFragment = "another connection committed while this transaction was open"

// classifyWriteErr records an ExecContext outcome.
func (t *n4Tally) classifyWriteErr(err error) {
	if err == nil {
		atomic.AddInt64(&t.writeSuccess, 1)
		return
	}
	if errors.Is(err, engine.ErrBusy) {
		if strings.Contains(err.Error(), staleImageMsgFragment) {
			atomic.AddInt64(&t.writeBusyStale, 1)
			return
		}
		if strings.Contains(err.Error(), engine.SegmentLockBusyFragment) {
			atomic.AddInt64(&t.writeBusySegmentLock, 1)
			return
		}
		atomic.AddInt64(&t.writeBusyOther, 1)
		return
	}
	t.recordFirstErr(&t.firstWriteErrMsg, err)
	atomic.AddInt64(&t.writeOtherError, 1)
}

func (t *n4Tally) classifyReadErr(err error) {
	if err == nil {
		atomic.AddInt64(&t.readSuccess, 1)
		return
	}
	if errors.Is(err, engine.ErrBusy) {
		atomic.AddInt64(&t.readBusyShared, 1)
		return
	}
	t.recordFirstErr(&t.firstReadErrMsg, err)
	atomic.AddInt64(&t.readOtherError, 1)
}

func (t *n4Tally) String() string {
	return fmt.Sprintf(
		"writes: success=%d busy-seglock=%d busy-staleimage=%d busy-OTHER=%d error-OTHER=%d | reads: success=%d busy-shared=%d error-OTHER=%d | busyRetry attempts absorbed silently=%d",
		atomic.LoadInt64(&t.writeSuccess), atomic.LoadInt64(&t.writeBusySegmentLock),
		atomic.LoadInt64(&t.writeBusyStale), atomic.LoadInt64(&t.writeBusyOther), atomic.LoadInt64(&t.writeOtherError),
		atomic.LoadInt64(&t.readSuccess), atomic.LoadInt64(&t.readBusyShared), atomic.LoadInt64(&t.readOtherError),
		atomic.LoadInt64(&t.busyRetryAttempts),
	)
}

// checkTallyHasNoUnclassifiedOutcomes asserts unclassified outcomes are empty.
func (t *n4Tally) checkTallyHasNoUnclassifiedOutcomes(tb testing.TB) {
	tb.Helper()
	if n := atomic.LoadInt64(&t.writeBusyOther); n != 0 {
		tb.Fatalf("N4: %d write ErrBusy outcome(s) matched NEITHER the RESERVED hook, the EXCLUSIVE hook, NOR the stale-image message -- an unclassified busy shape, which is itself a finding (see this file's own doc comment, hazard X21)", n)
	}
	if n := atomic.LoadInt64(&t.writeOtherError); n != 0 {
		tb.Fatalf("N4: %d write failure(s) were not engine.ErrBusy at all -- an unexpected error shape in a workload built to have none (no constraint violations, no schema races: every writer owns a disjoint id range); first was: %s", n, t.firstWriteErrMsg)
	}
	if n := atomic.LoadInt64(&t.readOtherError); n != 0 {
		tb.Fatalf("N4: %d read failure(s) were not engine.ErrBusy at all -- an unexpected error shape (reads should only ever fail via WithSharedLock's own busy timeout); first was: %s", n, t.firstReadErrMsg)
	}
}

// TestN4BusyClassificationIsComplete verifies segment lock busy errors are
// properly classified. It acquires the segment lock file exclusively to trigger
// a segment lock timeout, then verifies it lands in writeBusySegmentLock.
func TestN4BusyClassificationIsComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n4-busyshape.musq")
	if err := engine.CreateFile(path); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	// The lock lives in its OWN file, not the database: a byte-range lock belongs
	// to an inode, and a rewrite publishes by rename, so a lock taken on the
	// database file would be left protecting an unreachable inode (see
	// openSegmentLockFile's own comment). Naming the suffix here rather than
	// exporting it keeps the seam closed; if it ever changes, this test's holder
	// locks a different file, the open succeeds, and the t.Fatalf below says so.
	holder, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("opening the lock-holder fd: %v", err)
	}
	defer holder.Close()
	if ok, err := filelock.LockRange(holder, 0, 0, true); err != nil || !ok {
		t.Fatalf("holder: locking the whole lock file: ok=%v err=%v", ok, err)
	}
	defer filelock.UnlockRange(holder, 0, 0)

	oldBusyTimeout := engine.BusyTimeout
	engine.BusyTimeout = 50 * time.Millisecond
	defer func() { engine.BusyTimeout = oldBusyTimeout }()

	ndb, err := engine.OpenWrite(path)
	if err == nil {
		ndb.Close()
		t.Fatalf("expected engine.OpenWrite to fail with ErrBusy while another fd holds the segment lock, got nil error")
	}
	if !errors.Is(err, engine.ErrBusy) {
		t.Fatalf("expected engine.ErrBusy, got %v", err)
	}
	if !strings.Contains(err.Error(), engine.SegmentLockBusyFragment) {
		t.Fatalf("a segment busy must NAME its source (%q), got %v -- an unnamed busy is what landed in writeBusyOther", engine.SegmentLockBusyFragment, err)
	}
	if strings.Contains(err.Error(), staleImageMsgFragment) {
		t.Fatalf("unexpected stale-image message on a segment-lock timeout: %v", err)
	}

	// Classify it exactly as runN4Scenario does, and confirm it lands in
	// writeBusySegmentLock, NOT writeBusyOther.
	tally := &n4Tally{}
	tally.classifyWriteErr(err)
	if got := atomic.LoadInt64(&tally.writeBusySegmentLock); got != 1 {
		t.Fatalf("expected writeBusySegmentLock=1, got %d (tally: %s)", got, tally.String())
	}
	if got := atomic.LoadInt64(&tally.writeBusyOther); got != 0 {
		t.Fatalf("STOP: a named busy outcome landed in writeBusyOther (got %d) -- the exact false-positive this test exists to prevent is back", got)
	}
	tally.checkTallyHasNoUnclassifiedOutcomes(t) // must NOT fail
}

// ---------------------------------------------------------------------
// Configuration and the mixed-DML workload (design doc: "a schema/workload
// that touches all three [overflow pages, index writes, freelist
// pressure], not just trivial single-column inserts").
// ---------------------------------------------------------------------

type n4Config struct {
	name           string
	nWriters       int
	nReaders       int // MUST be bounded (hazard X24) -- see this file's own doc comment
	writesPerConn  int
	busyTimeout    time.Duration
	overallTimeout time.Duration
}

// n4CreateTable and n4CreateIndex are reused by ledger replay and other tests.
// Large blobs trigger overflow chains, the index forces multiple b-tree writes,
// and deletes exercise freelist management.
const (
	n4CreateTable  = `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v BLOB)`
	n4CreateIndex  = `CREATE INDEX idx_t_k ON t(k)`
	n4SmallBlobLen = 24
	n4LargeBlobLen = 6000
)

func n4MakeBlob(writerID, seq int, large bool) []byte {
	n := n4SmallBlobLen
	if large {
		n = n4LargeBlobLen
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(writerID*31 + seq + i)
	}
	return b
}

// n4WriterOp is one autocommit statement a writer worker issues -- exactly
// what gets recorded into the ledger on success.
type n4WriterOp struct {
	sql  string
	args []any
}

// n4GenWriterOps builds a mixed DML sequence for one writer: 60% insert
// (some with large blobs), 20% update, 20% delete. Each writer owns a disjoint
// id range to prevent constraint violations.
func n4GenWriterOps(writerID, n int) []n4WriterOp {
	base := int64(writerID+1) * 1_000_000
	var ops []n4WriterOp
	var alive []int64
	nextID := base
	insert := func(seq int) {
		id := nextID
		nextID++
		large := seq%6 == 0
		ops = append(ops, n4WriterOp{n4CreateInsertSQL, []any{id, id % 50, n4MakeBlob(writerID, seq, large)}})
		alive = append(alive, id)
	}
	for i := 0; i < n; i++ {
		switch i % 5 {
		case 3: // update
			if len(alive) == 0 {
				insert(i)
				continue
			}
			id := alive[i%len(alive)]
			large := i%2 == 0
			ops = append(ops, n4WriterOp{`UPDATE t SET v=? WHERE id=?`, []any{n4MakeBlob(writerID, i, large), id}})
		case 4: // delete
			if len(alive) == 0 {
				insert(i)
				continue
			}
			idx := i % len(alive)
			id := alive[idx]
			alive = append(alive[:idx], alive[idx+1:]...)
			ops = append(ops, n4WriterOp{`DELETE FROM t WHERE id=?`, []any{id}})
		default:
			insert(i)
		}
	}
	return ops
}

const n4CreateInsertSQL = `INSERT INTO t(id, k, v) VALUES(?, ?, ?)`

// ---------------------------------------------------------------------
// The harness proper.
// ---------------------------------------------------------------------

// dumpGoroutinesOnTimeout runs fn and fails with a goroutine dump if timeout is exceeded.
func dumpGoroutinesOnTimeout(t *testing.T, timeout time.Duration, fn func(done chan<- struct{})) {
	t.Helper()
	done := make(chan struct{})
	go fn(done)
	select {
	case <-done:
	case <-time.After(timeout):
		var buf bytes.Buffer
		pprof.Lookup("goroutine").WriteTo(&buf, 2)
		t.Fatalf("N4: scenario did not finish within %s (%d goroutines currently alive) -- this may be reader-starvation of a writer (a KNOWN, already-measured, accepted characteristic of musql's lock ladder, hazard X6/D5 in lock.go's own package doc comment: musql's readers never take the PENDING rung, so a writer's own PENDING is inert against them) rather than a genuine hang; full goroutine dump follows so the two are distinguishable:\n%s", timeout, runtime.NumGoroutine(), buf.String())
	}
}

// runN4Scenario is the shared driver for concurrent writer/reader tests.
// It returns the final db path, the success ledger, and the outcome tally.
func runN4Scenario(t *testing.T, cfg n4Config) (path string, ledger *n4Ledger, tally *n4Tally) {
	t.Helper()

	// Shrink BusyTimeout for the run's duration to increase contention.
	oldBusyTimeout := engine.BusyTimeout
	engine.BusyTimeout = cfg.busyTimeout
	defer func() { engine.BusyTimeout = oldBusyTimeout }()

	tally = &n4Tally{}
	oldRetryHook := driver.BusyRetryAttemptHookForTest
	driver.BusyRetryAttemptHookForTest = func() { atomic.AddInt64(&tally.busyRetryAttempts, 1) }
	defer func() { driver.BusyRetryAttemptHookForTest = oldRetryHook }()

	dir := t.TempDir()
	path = filepath.Join(dir, "n4.musq")
	ctx := context.Background()

	seedDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("N4: sql.Open (seed): %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, n4CreateTable); err != nil {
		t.Fatalf("N4: CREATE TABLE: %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, n4CreateIndex); err != nil {
		t.Fatalf("N4: CREATE INDEX: %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("N4: seed Close: %v", err)
	}

	// Acquire all Conn objects upfront and hold them for the run's duration.
	// This ensures each goroutine has its own fresh engine.Session.
	total := cfg.nWriters + cfg.nReaders
	var connsOpenedForThisPath int64
	driver.RegisterConnectionHook(func(c *driver.Conn, dsn string) error {
		if strings.TrimPrefix(dsn, "file:") == path {
			atomic.AddInt64(&connsOpenedForThisPath, 1)
		}
		return nil
	})

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("N4: sql.Open (main): %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(total)

	conns := make([]*sql.Conn, total)
	for i := 0; i < total; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("N4: db.Conn(%d): %v", i, err)
		}
		conns[i] = c
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	if got := atomic.LoadInt64(&connsOpenedForThisPath); got != int64(total) {
		t.Fatalf("N4: expected exactly %d genuinely new underlying connections against %s, RegisterConnectionHook counted %d -- the 'own connection, not pool-multiplexed' claim (design doc point 1) is not actually true for this run", total, path, got)
	}

	ledger = &n4Ledger{}
	stopLedger := ledger.recordCommits()
	defer stopLedger()
	stopReaders := make(chan struct{})
	var wgWriters, wgReaders sync.WaitGroup

	// Writers: each on its own pinned connection with autocommit statements.
	for w := 0; w < cfg.nWriters; w++ {
		wgWriters.Add(1)
		go func(connID int) {
			defer wgWriters.Done()
			conn := conns[connID]
			for _, op := range n4GenWriterOps(connID, cfg.writesPerConn) {
				_, err := conn.ExecContext(ctx, op.sql, op.args...)
				tally.classifyWriteErr(err) // the commit itself is recorded by the commit-order hook (ledger.recordCommits)
			}
		}(w)
	}

	// Readers: bounded count, each on its own pinned connection, looping SELECTs
	// until writers finish. This tests reader-vs-writer interleaving.
	readerQueries := []string{
		`SELECT count(*), coalesce(sum(length(v)), 0) FROM t`,
		`SELECT id, k FROM t WHERE k = ? ORDER BY id`,
		`SELECT id FROM t NOT INDEXED WHERE k = ? ORDER BY id`,
	}
	for r := 0; r < cfg.nReaders; r++ {
		wgReaders.Add(1)
		go func(connID, seed int) {
			defer wgReaders.Done()
			conn := conns[connID]
			i := 0
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				q := readerQueries[i%len(readerQueries)]
				var rows *sql.Rows
				var err error
				if strings.Contains(q, "?") {
					rows, err = conn.QueryContext(ctx, q, (seed+i)%50)
				} else {
					rows, err = conn.QueryContext(ctx, q)
				}
				if err == nil {
					for rows.Next() {
					}
					err = rows.Err()
					rows.Close()
				}
				tally.classifyReadErr(err)
				i++
			}
		}(cfg.nWriters+r, r*7+1)
	}

	dumpGoroutinesOnTimeout(t, cfg.overallTimeout, func(done chan<- struct{}) {
		// Wait for writers first, then stop and wait for readers.
		wgWriters.Wait()
		close(stopReaders)
		wgReaders.Wait()
		close(done)
	})

	t.Logf("N4[%s]: %d conn(s) (%d writer, %d reader), %d writes/conn, busyTimeout=%s -- tally: %s",
		cfg.name, total, cfg.nWriters, cfg.nReaders, cfg.writesPerConn, cfg.busyTimeout, tally.String())

	tally.checkTallyHasNoUnclassifiedOutcomes(t)
	return path, ledger, tally
}

// ---------------------------------------------------------------------
// The mandatory post-run check sequence (design doc point 8). Every one of
// these is a HARD gate: any finding here is the STOP/GO condition (this
// file's own top-of-package doc comment references musql-stage4-design-
// and-live-bugs.md's explicit rule) -- a redesign signal, not a "patch this
// one thing" signal, and must be reported as such rather than silently
// fixed.
// ---------------------------------------------------------------------

// checkStructuralAndN3 runs PRAGMA integrity_check on the final database.
func checkStructuralAndN3(t *testing.T, path string) {
	t.Helper()
	found, err := engine.IntegrityCheck(path, engine.IntegrityCheckOptions{})
	if err != nil {
		t.Fatalf("N4 post-run: IntegrityCheck(%s): %v", path, err)
	}
	if len(found) > 0 {
		t.Fatalf("STOP-AND-RECONSIDER (N4 post-run): integrity_check found %d problem(s) in the survivor file %s that StructuralCheckAfterCommitForTest did NOT catch mid-run: %v", len(found), path, found)
	}
}

// checkLedgerReplays replays successful statements in commit order on a fresh
// database single-threaded and verifies the result rows match the concurrent run.
func checkLedgerReplays(t *testing.T, concurrentPath string, ledger *n4Ledger, wrote int64) {
	t.Helper()
	entries := ledger.snapshot()
	if wrote > 0 && len(entries) == 0 {
		t.Fatalf("STOP-AND-RECONSIDER (N4 ledger replay): %d writes succeeded and the ledger recorded none -- the commit-order hook is not firing, so a replay would prove nothing", wrote)
	}

	replayPath := filepath.Join(t.TempDir(), "n4-replay.musq")
	ctx := context.Background()
	db, err := sql.Open("sqlite", replayPath)
	if err != nil {
		t.Fatalf("N4 ledger replay: sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1) // single-threaded replay, deliberately: one commit sequence
	defer db.Close()
	for _, s := range []string{n4CreateTable, n4CreateIndex} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("N4 ledger replay: %s: %v", s, err)
		}
	}
	for _, e := range entries {
		if _, err := db.ExecContext(ctx, e.sql, e.args...); err != nil {
			t.Fatalf("STOP-AND-RECONSIDER (N4 ledger replay): replaying ledger entry seq=%d (originally conn %d, %q, args=%v), which the CONCURRENT run itself recorded as SUCCESSFUL, FAILED when replayed single-threaded in recorded commit order: %v -- the concurrent run's own success/ordering is not reproducible sequentially", e.seq, e.connID, e.sql, e.args, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("N4 ledger replay: Close: %v", err)
	}
	if want, got := n4DumpRows(t, concurrentPath), n4DumpRows(t, replayPath); got != want {
		t.Fatalf("STOP-AND-RECONSIDER (N4 ledger replay): the single-threaded replay of the run's own %d recorded commits does not hold the rows the concurrent run's database holds\n concurrent: %.400s\n replay:     %.400s", len(entries), want, got)
	}
}

// n4DumpRows renders all rows of t in rowid order with their types.
func n4DumpRows(t *testing.T, path string) string {
	t.Helper()
	rp, err := engine.Open(path)
	if err != nil {
		t.Fatalf("N4: Open(%s): %v", path, err)
	}
	defer rp.Close()
	_, rows, err := rp.Query(`SELECT id, k, v FROM t ORDER BY id`)
	if err != nil {
		t.Fatalf("N4: dumping %s: %v", path, err)
	}
	var b strings.Builder
	for _, r := range rows {
		for _, c := range r {
			fmt.Fprintf(&b, "%d:%d:%q|", c.Typ, c.I, c.S)
		}
		b.WriteByte(';')
	}
	return b.String()
}

// checkIndexVsTableScan verifies index queries and full table scans return the same rows.
func checkIndexVsTableScan(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("N4 index-vs-tablescan: sql.Open: %v", err)
	}
	defer db.Close()

	queryIDs := func(q string, k int) []int64 {
		rows, err := db.QueryContext(ctx, q, k)
		if err != nil {
			t.Fatalf("N4 index-vs-tablescan: query %q (k=%d): %v", q, k, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("N4 index-vs-tablescan: scan: %v", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("N4 index-vs-tablescan: rows.Err: %v", err)
		}
		return out
	}

	for k := 0; k < 50; k++ {
		viaIndex := queryIDs(`SELECT id FROM t WHERE k = ? ORDER BY id`, k)
		viaScan := queryIDs(`SELECT id FROM t NOT INDEXED WHERE k = ? ORDER BY id`, k)
		if len(viaIndex) != len(viaScan) {
			t.Fatalf("STOP-AND-RECONSIDER (N4 index-vs-tablescan): k=%d: index path returned %d row(s) %v, table-scan path returned %d row(s) %v", k, len(viaIndex), viaIndex, len(viaScan), viaScan)
		}
		for i := range viaIndex {
			if viaIndex[i] != viaScan[i] {
				t.Fatalf("STOP-AND-RECONSIDER (N4 index-vs-tablescan): k=%d: index path %v and table-scan path %v disagree at position %d", k, viaIndex, viaScan, i)
			}
		}
	}
}

// runN4PostRunChecks runs integrity, ledger replay, and index checks.
func runN4PostRunChecks(t *testing.T, path string, ledger *n4Ledger, tally *n4Tally) {
	t.Helper()
	checkStructuralAndN3(t, path)
	t.Log("N4 post-run: CheckStructuralIntegrity + N3 index cross-check both clean")
	checkLedgerReplays(t, path, ledger, atomic.LoadInt64(&tally.writeSuccess))
	t.Logf("N4 post-run: single-threaded replay of %d recorded commit(s) holds exactly the concurrent run's rows", len(ledger.snapshot()))
	checkIndexVsTableScan(t, path)
	t.Log("N4 post-run: index-scan and table-scan agree for every k in [0,50)")
}

// TestN4ConcurrentWritersOnly tests many writers without readers.
func TestN4ConcurrentWritersOnly(t *testing.T) {
	path, ledger, tally := runN4Scenario(t, n4Config{
		name:           "writers-only",
		nWriters:       6,
		nReaders:       0,
		writesPerConn:  40,
		busyTimeout:    750 * time.Millisecond,
		overallTimeout: 60 * time.Second,
	})
	runN4PostRunChecks(t, path, ledger, tally)
}

// TestN4ConcurrentReadersAndWriters tests reader-vs-writer interleaving,
// the critical case that finds correctness issues.
func TestN4ConcurrentReadersAndWriters(t *testing.T) {
	if runtime.GOOS == "js" {
		// One thread and no preemption: some goroutine of this scenario
		// never blocks, the others never run, and the run hangs until the
		// test binary is killed. A browser worker runs one connection's
		// statements in sequence, which is not this scenario; finding the
		// non-yielding loop is open work.
		t.Skip("js/wasm: the concurrent reader/writer scenario hangs under the cooperative scheduler")
	}
	path, ledger, tally := runN4Scenario(t, n4Config{
		name:           "readers-and-writers",
		nWriters:       6,
		nReaders:       4,
		writesPerConn:  40,
		busyTimeout:    750 * time.Millisecond,
		overallTimeout: 60 * time.Second,
	})
	runN4PostRunChecks(t, path, ledger, tally)
}
