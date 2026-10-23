package store

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
)

// ship applies to r everything h has that r lacks.
func ship(t *testing.T, h, r *History) {
	t.Helper()
	for {
		out, wait, err := h.Ship(r.Seq())
		if err != nil {
			t.Fatal(err)
		}
		if wait != nil {
			return
		}
		for i := range out {
			sh := &out[i]
			var delta io.Reader
			if sh.Delta != nil {
				delta = sh.Delta
			}
			err := r.Apply(sh.Record, sh.Segment, delta)
			sh.Close()
			if err != nil {
				t.Fatalf("apply %d: %v", sh.Seq, err)
			}
		}
	}
}

// sameFiles checks that the replica's pair is the primary's committed pair.
func sameFiles(t *testing.T, primary, replica string) {
	t.Helper()
	a, b := readAll(t, primary), readAll(t, replica)
	if !bytes.Equal(a, b) {
		t.Fatalf("segment files differ: %d and %d bytes", len(a), len(b))
	}
	var end int64
	if err := engine.CommitState(primary, func(c engine.CommitInfo) error { end = c.DeltaEnd; return nil }); err != nil {
		t.Fatal(err)
	}
	var pd []byte
	if end > 0 {
		pd = readAll(t, primary+".delta")[:end]
	}
	rd, err := os.ReadFile(replica + ".delta")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if !bytes.Equal(pd, rd) {
		t.Fatalf("deltas differ: %d and %d bytes", len(pd), len(rd))
	}
}

func readAll(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReplicaFollowsByteExactly(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, "p.musq")
	db := openDB(t, pp)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`)
	h, err := Open(pp, filepath.Join(dir, "ph"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rp := filepath.Join(dir, "r.musq")
	rh := filepath.Join(dir, "rh")
	r, err := OpenReplica(rp, rh)
	if err != nil {
		t.Fatal(err)
	}
	ship(t, h, r)
	sameFiles(t, pp, rp)
	// A reader held open on the replica sees each applied record.
	rdb := openDB(t, rp+"?_pragma=query_only(1)")
	defer rdb.Close()
	rdb.SetMaxOpenConns(1)
	steps := []string{
		`INSERT INTO t VALUES(1, 'a')`,
		`INSERT INTO t VALUES(2, 'b')`,
		`CREATE INDEX tb ON t(b)`,
		`UPDATE t SET b = 'c' WHERE a = 1`,
		`VACUUM`,
		`DELETE FROM t WHERE a = 2`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET b = upper(b) WHERE a = new.a; END`,
		`INSERT INTO t VALUES(3, 'x')`,
	}
	for i, q := range steps {
		exec(t, db, q)
		ship(t, h, r)
		sameFiles(t, pp, rp)
		if got, want := dump(t, rp), dump(t, pp); got != want {
			t.Fatalf("step %d %s: replica\n%s\nprimary\n%s", i, q, got, want)
		}
		var n int
		if err := rdb.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		var pn int
		if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&pn); err != nil {
			t.Fatal(err)
		}
		if n != pn {
			t.Fatalf("step %d: held reader counts %d rows, primary %d", i, n, pn)
		}
	}

	// Restart: the replica goes on from its own log.
	r.Close()
	if r, err = OpenReplica(rp, rh); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO t VALUES(4, 'y')`)
	ship(t, h, r)
	sameFiles(t, pp, rp)

	// A crash after logging a record and before applying it: the file is
	// behind the log, and opening rolls it forward.
	exec(t, db, `INSERT INTO t VALUES(5, 'z')`)
	out, _, err := h.Ship(r.Seq())
	if err != nil || len(out) != 1 || out[0].Base {
		t.Fatalf("ship: %v %d", err, len(out))
	}
	r.mu.Lock()
	if err := r.writeRecord(out[0].Record); err != nil {
		t.Fatal(err)
	}
	r.mu.Unlock()
	r.Close()
	if r, err = OpenReplica(rp, rh); err != nil {
		t.Fatal(err)
	}
	sameFiles(t, pp, rp)
	exec(t, db, `INSERT INTO t VALUES(6, 'w')`)
	ship(t, h, r)
	sameFiles(t, pp, rp)

	// A new replica after pruning starts from the newest base.
	if _, err := h.Snapshot(false); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO t VALUES(7, 'v')`)
	if err := h.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	ship(t, h, r)
	sameFiles(t, pp, rp)
	r2p := filepath.Join(dir, "r2.musq")
	r2, err := OpenReplica(r2p, filepath.Join(dir, "r2h"))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	ship(t, h, r2)
	sameFiles(t, pp, r2p)
	r.Close()
}

// TestReplicaStartsOverOnAnOlderBase: a primary whose log started over
// (lower seqs) sends a base the replica takes from scratch.
func TestReplicaStartsOverOnAnOlderBase(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, "p.musq")
	db := openDB(t, pp)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(a)`)
	h, err := Open(pp, filepath.Join(dir, "ph"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		exec(t, db, `INSERT INTO t VALUES(?)`, i)
	}
	rp := filepath.Join(dir, "r.musq")
	r, err := OpenReplica(rp, filepath.Join(dir, "rh"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ship(t, h, r)
	h.Close()
	exec(t, db, `INSERT INTO t VALUES(99)`)
	h2, err := Open(pp, filepath.Join(dir, "ph2"))
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	if h2.Seq() >= r.Seq() {
		t.Fatalf("seqs %d, %d", h2.Seq(), r.Seq())
	}
	ship(t, h2, r)
	sameFiles(t, pp, rp)
}

// TestReplicaReadsAreNeverTorn: a reader on the replica while records are
// applied sees whole commits only: rows 1..n for some n, never a gap.
func TestReplicaReadsAreNeverTorn(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, "p.musq")
	db := openDB(t, pp)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, pad)`)
	h, err := Open(pp, filepath.Join(dir, "ph"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rp := filepath.Join(dir, "r.musq")
	r, err := OpenReplica(rp, filepath.Join(dir, "rh"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ship(t, h, r)
	rdb := openDB(t, rp+"?_pragma=query_only(1)")
	defer rdb.Close()
	var stop atomic.Bool
	var wg sync.WaitGroup
	var bad atomic.Value
	for range 3 {
		wg.Go(func() {
			last := 0
			for !stop.Load() {
				var n, mx int
				if err := rdb.QueryRow(`SELECT count(*), coalesce(max(a), 0) FROM t`).Scan(&n, &mx); err != nil {
					bad.Store(err.Error())
					return
				}
				if n != mx || n < last {
					bad.Store("torn or backwards read")
					return
				}
				last = n
			}
		})
	}
	for i := 1; i <= 60; i++ {
		exec(t, db, `INSERT INTO t VALUES(?, randomblob(200))`, i)
		if i%20 == 0 {
			exec(t, db, `VACUUM`)
		}
		ship(t, h, r)
	}
	stop.Store(true)
	wg.Wait()
	if v := bad.Load(); v != nil {
		t.Fatal(v)
	}
	sameFiles(t, pp, rp)
}
