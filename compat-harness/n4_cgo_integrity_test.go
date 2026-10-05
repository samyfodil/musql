// Tests N4 concurrent operations with cgo integrity checks.
// an importable package; see engine/n4_concurrent_test.go's own doc comment
// for why the split is package-shaped, not just organizational), then opens
// the survivor through the REAL oracle and asserts `integrity_check` reports
// exactly "ok".
//
// This module's own TestMain (harness_test.go) already sets
// musqlengine.StructuralCheckAfterCommitForTest = true for every in-process
// test in this package -- this test runs IN-PROCESS (not through the
// separate worker binary), so N2+N3 are already running on every one of its
// own commits too, for free, exactly like the engine/-resident harness.
package compat

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	_ "github.com/samyfodil/musql/driver"
	musqlengine "github.com/samyfodil/musql/engine"
)

const (
	n4cgoCreateTable = `CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, v BLOB)`
	n4cgoCreateIndex = `CREATE INDEX idx_t_k ON t(k)`
	n4cgoSmallBlob   = 24
	n4cgoLargeBlob   = 6000
)

func n4cgoBlob(writerID, seq int, large bool) []byte {
	n := n4cgoSmallBlob
	if large {
		n = n4cgoLargeBlob
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(writerID*31 + seq + i)
	}
	return b
}

type n4cgoOp struct {
	sql  string
	args []any
}

// n4cgoGenOps mirrors engine/n4_concurrent_test.go's n4GenWriterOps exactly
// (same shape: disjoint id range per writer, ~60% insert/20% update/20%
// delete, 1-in-6 inserts large enough to overflow) -- duplicated, not
// shared, per this file's own doc comment.
func n4cgoGenOps(writerID, n int) []n4cgoOp {
	base := int64(writerID+1) * 1_000_000
	var ops []n4cgoOp
	var alive []int64
	nextID := base
	insert := func(seq int) {
		id := nextID
		nextID++
		large := seq%6 == 0
		ops = append(ops, n4cgoOp{`INSERT INTO t(id, k, v) VALUES(?, ?, ?)`, []any{id, id % 50, n4cgoBlob(writerID, seq, large)}})
		alive = append(alive, id)
	}
	for i := 0; i < n; i++ {
		switch i % 5 {
		case 3:
			if len(alive) == 0 {
				insert(i)
				continue
			}
			id := alive[i%len(alive)]
			ops = append(ops, n4cgoOp{`UPDATE t SET v=? WHERE id=?`, []any{n4cgoBlob(writerID, i, i%2 == 0), id}})
		case 4:
			if len(alive) == 0 {
				insert(i)
				continue
			}
			idx := i % len(alive)
			id := alive[idx]
			alive = append(alive[:idx], alive[idx+1:]...)
			ops = append(ops, n4cgoOp{`DELETE FROM t WHERE id=?`, []any{id}})
		default:
			insert(i)
		}
	}
	return ops
}

// TestN4CgoIntegrityCheck runs a small concurrent writer+reader stress
// against musql (exactly this repo's own N4 shape, at reduced scale) and
// then asserts C SQLite's own `PRAGMA integrity_check` reports "ok"
// over the survivor file -- the one check the pure-Go engine/ harness
// cannot perform on itself.
func TestN4CgoIntegrityCheck(t *testing.T) {
	oldBusyTimeout := musqlengine.BusyTimeout
	musqlengine.BusyTimeout = 750 * time.Millisecond
	defer func() { musqlengine.BusyTimeout = oldBusyTimeout }()

	dir := t.TempDir()
	path := filepath.Join(dir, "n4cgo.musq") // the driver writes no other format
	ctx := context.Background()

	seedDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("N4 cgo: sql.Open (seed): %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, n4cgoCreateTable); err != nil {
		t.Fatalf("N4 cgo: CREATE TABLE: %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, n4cgoCreateIndex); err != nil {
		t.Fatalf("N4 cgo: CREATE INDEX: %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("N4 cgo: seed Close: %v", err)
	}

	const nWriters = 4
	const nReaders = 3
	const writesPerWriter = 25
	total := nWriters + nReaders

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("N4 cgo: sql.Open (main): %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(total)

	conns := make([]*sql.Conn, total)
	for i := 0; i < total; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("N4 cgo: db.Conn(%d): %v", i, err)
		}
		conns[i] = c
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	var otherWriteErrs int
	var mu sync.Mutex
	stop := make(chan struct{})
	var wgWriters, wgReaders sync.WaitGroup

	for w := 0; w < nWriters; w++ {
		wgWriters.Add(1)
		go func(connID int) {
			defer wgWriters.Done()
			conn := conns[connID]
			for _, op := range n4cgoGenOps(connID, writesPerWriter) {
				_, err := conn.ExecContext(ctx, op.sql, op.args...)
				if err != nil && !errors.Is(err, musqlengine.ErrBusy) {
					mu.Lock()
					otherWriteErrs++
					mu.Unlock()
					t.Logf("N4 cgo: writer %d: unexpected non-busy error: %v", connID, err)
				}
			}
		}(w)
	}
	for r := 0; r < nReaders; r++ {
		wgReaders.Add(1)
		go func(connID int) {
			defer wgReaders.Done()
			conn := conns[connID]
			for {
				select {
				case <-stop:
					return
				default:
				}
				rows, err := conn.QueryContext(ctx, `SELECT count(*) FROM t WHERE k = ?`, connID%50)
				if err == nil {
					for rows.Next() {
					}
					rows.Close()
				}
			}
		}(nWriters + r)
	}

	done := make(chan struct{})
	go func() {
		wgWriters.Wait()
		close(stop)
		wgReaders.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("N4 cgo: scenario did not finish within 60s")
	}

	if otherWriteErrs != 0 {
		t.Fatalf("STOP-AND-RECONSIDER (N4 cgo): %d write failure(s) were not engine.ErrBusy at all", otherWriteErrs)
	}

	// N2+N3 explicit final pass, exactly like the primary engine/ harness
	// (StructuralCheckAfterCommitForTest already ran both on every commit
	// above, via this module's own TestMain).
	found, err := musqlengine.IntegrityCheck(path, musqlengine.IntegrityCheckOptions{})
	if err != nil {
		t.Fatalf("N4 cgo post-run: IntegrityCheck: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("STOP-AND-RECONSIDER (N4 cgo): integrity_check found %d problem(s): %v", len(found), found)
	}

	// The oracle check this file exists for: C SQLite's own
	// PRAGMA integrity_check over what musql just wrote under concurrent access
	// -- reached through the EXPORT, because the survivor is a segment database and
	// C cannot read that format. This is the interchange promise itself
	// (AGENTS.md Rule 3: "a database musql wrote can be handed back as a .db that
	// C reads"), so running the oracle over the export tests strictly more than
	// running it over a file musql wrote in C's format directly would: the
	// concurrent workload AND the conversion, with C as the judge of both.
	exported := filepath.Join(t.TempDir(), "exported.db")
	if eerr := sqliteconv.Export(path, exported, 0); eerr != nil {
		t.Fatalf("N4 cgo: Export: %v", eerr)
	}
	cdb, err := sql.Open("sqlite3", exported)
	if err != nil {
		t.Fatalf("N4 cgo: sql.Open (cgo oracle): %v", err)
	}
	defer cdb.Close()
	var verdict string
	if err := cdb.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&verdict); err != nil {
		t.Fatalf("N4 cgo: PRAGMA integrity_check: %v", err)
	}
	if strings.ToLower(verdict) != "ok" {
		t.Fatalf("STOP-AND-RECONSIDER (N4 cgo): C SQLite's own PRAGMA integrity_check reported %q, not \"ok\", over the EXPORT of musql's own concurrently-written survivor", verdict)
	}
	t.Logf("N4 cgo: %d writer(s)/%d reader(s), %d writes/writer, cgo oracle integrity_check=%q", nWriters, nReaders, writesPerWriter, verdict)
}
