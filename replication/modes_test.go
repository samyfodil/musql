package replication

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestConstraintsConverge: in CRDT mode a node checks its constraints on its
// own writes, and peers' rows are applied without them -- so nodes converge
// even where a merge breaks a rule each write kept. Two nodes write one UNIQUE
// email while apart: the newer row holds it on both, and the other comes back
// when the value frees up. A two-column CHECK merges into a row breaking it, on
// both. A generated column is recomputed from the merged row.
func TestConstraintsConverge(t *testing.T) {
	c := newCluster(t)
	schema := []string{
		`CREATE TABLE p(id TEXT PRIMARY KEY)`,
		`CREATE TABLE u(id TEXT PRIMARY KEY, email TEXT UNIQUE, lo INT, hi INT,
			total INT AS (lo + hi), prod INT AS (lo * hi) STORED, parent TEXT REFERENCES p(id), CHECK(lo <= hi))`,
		`INSERT INTO p VALUES('p1')`,
		`INSERT INTO u(id, email, lo, hi, parent) VALUES('r0', 'r0@', 1, 5, 'p1')`,
	}
	c.open("a", schema...)
	c.open("b")
	const q = `SELECT id, email, lo, hi, total, prod FROM u ORDER BY id`
	c.converge("r0|r0@|1|5|6|5\n", q)

	c.partition(true)
	c.exec("a", `INSERT INTO u(id, email, lo, hi) VALUES('a1', 'x@', 1, 1)`, `UPDATE u SET lo = 4 WHERE id = 'r0'`)
	c.exec("b", `INSERT INTO u(id, email, lo, hi) VALUES('b1', 'x@', 2, 2)`, `UPDATE u SET hi = 2 WHERE id = 'r0'`)
	c.partition(false)
	// b's row is the newer holder of x@; r0 is lo=4 from a, hi=2 from b.
	c.converge("b1|x@|2|2|4|4\nr0|r0@|4|2|6|8\n", q)
	for _, site := range []string{"a", "b"} {
		held, err := Withheld(t.Context(), c.dbs[site])
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(held); got != "[{u [a1] map[email:x@ hi:1 id:a1 lo:1 parent:<nil>]}]" {
			t.Fatalf("%s: held out %s", site, got)
		}
	}

	// Freeing the value -- here on the node that holds the winner, which is no
	// peer op to it -- brings the held-out row back everywhere.
	c.exec("a", `DELETE FROM u WHERE id = 'b1'`)
	c.converge("a1|x@|1|1|2|1\nr0|r0@|4|2|6|8\n", q)
	if held, err := Withheld(t.Context(), c.dbs["a"]); err != nil || len(held) != 0 {
		t.Fatalf("held out after the value freed up: %v %v", held, err)
	}
}

// TestAutoincrementSyncs: AUTOINCREMENT tables sync; each node allocates in
// its own range, so two inserting while apart pick different ids, and a
// deleted id is never handed out again.
func TestAutoincrementSyncs(t *testing.T) {
	c := newCluster(t)
	a := c.open("a", `CREATE TABLE ai(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`)
	c.open("b")
	c.converge("", `SELECT name FROM sqlite_master WHERE name = 'ai'`)
	c.partition(true)
	c.exec("a", `INSERT INTO ai(v) VALUES('a')`)
	c.exec("b", `INSERT INTO ai(v) VALUES('b')`)
	c.partition(false)
	got := c.converge("", `SELECT v FROM ai ORDER BY v`, `SELECT count(DISTINCT id) FROM ai`)
	if got[0] != "a\nb\n" || got[1] != "2\n" {
		t.Fatalf("got %q", got)
	}
	var top int64
	if err := a.QueryRow(`SELECT id FROM ai WHERE v = 'a'`).Scan(&top); err != nil {
		t.Fatal(err)
	}
	c.exec("a", `DELETE FROM ai WHERE v = 'a'`, `INSERT INTO ai(v) VALUES('a2')`)
	var next int64
	if err := a.QueryRow(`SELECT id FROM ai WHERE v = 'a2'`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	// Never reused; not necessarily dense -- a commit retried on SQLITE_BUSY
	// takes a new id each attempt, as C's AUTOINCREMENT may skip one.
	if next <= top {
		t.Fatalf("after deleting %d, the next id is %d, which is not above it", top, next)
	}
}

// TestSingleWriter: in Leader mode, only the node the caller's consensus names
// commits; the other's commit fails with ErrNotWriter and rolls back, its TEMP
// tables stay writable, and it follows. Moving the role moves who writes.
func TestSingleWriter(t *testing.T) {
	c := newCluster(t)
	var aWrites, bWrites atomic.Bool
	aWrites.Store(true)
	a := c.openWith("a", []Option{Leader(aWrites.Load)},
		`CREATE TABLE t(id TEXT PRIMARY KEY, email TEXT UNIQUE)`)
	b := c.openWith("b", []Option{Leader(bWrites.Load)})
	c.converge("", `SELECT name FROM sqlite_master WHERE name = 't'`)

	if _, err := b.Exec(`INSERT INTO t VALUES('b1', 'b@')`); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("a follower's commit: %v, want ErrNotWriter", err)
	}
	if _, err := b.Exec(`CREATE TABLE b2(x)`); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("a follower's DDL: %v, want ErrNotWriter", err)
	}
	conn, err := b.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, s := range []string{`CREATE TEMP TABLE scratch(x)`, `INSERT INTO scratch VALUES(1)`} {
		if _, err := conn.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("a follower's TEMP write %s: %v", s, err)
		}
	}
	c.exec("a", `INSERT INTO t VALUES('a1', 'a@')`)
	c.converge("a1|a@\n", `SELECT * FROM t ORDER BY id`)

	aWrites.Store(false)
	bWrites.Store(true)
	if _, err := a.Exec(`INSERT INTO t VALUES('a2', 'a2@')`); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("the old writer's commit: %v, want ErrNotWriter", err)
	}
	c.exec("b", `INSERT INTO t VALUES('b1', 'b@')`)
	got := c.converge("", `SELECT * FROM t ORDER BY id`)[0]
	if !strings.Contains(got, "b1|b@") || strings.Contains(got, "a2") {
		t.Fatalf("got %q", got)
	}
}

// TestModeIsRequiredAndRecorded: Open needs a mode; the first one a database is
// opened in is recorded and replicates, and Open in another is refused.
func TestModeIsRequiredAndRecorded(t *testing.T) {
	ctx := t.Context()
	p := filepath.Join(t.TempDir(), "m.db")
	createSchema(t, p, `CREATE TABLE t(id TEXT PRIMARY KEY)`)
	if db, err := Open(ctx, p); err == nil {
		db.Close()
		t.Fatal("Open without a mode was accepted")
	}
	db, err := Open(ctx, p, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err := Open(ctx, p, Leader(func() bool { return true })); !errors.Is(err, ErrMode) {
		if err == nil {
			db.Close()
		}
		t.Fatalf("reopening a CRDT database as Leader: %v, want ErrMode", err)
	}
	db, err = Open(ctx, p, CRDT())
	if err != nil {
		t.Fatalf("reopening in its own mode: %v", err)
	}
	db.Close()
}

// TestModeConflictWhileApart: two nodes first opened in different modes while
// apart each record theirs; once they meet, the earlier one is the database's
// mode, and the other node's commits are refused.
func TestModeConflictWhileApart(t *testing.T) {
	c := newCluster(t)
	c.partition(true)
	c.open("a", `CREATE TABLE t(id TEXT PRIMARY KEY)`)
	b := c.openWith("b", []Option{Leader(func() bool { return true })}, `CREATE TABLE t(id TEXT PRIMARY KEY)`)
	c.partition(false)
	// Each node's create of t and its mode, on both.
	c.converge("4\n", `SELECT count(*) FROM _repl_oplog WHERE op = 3`)
	if _, err := b.Exec(`INSERT INTO t VALUES('x')`); !errors.Is(err, ErrMode) {
		t.Fatalf("a commit on the node whose mode lost: %v, want ErrMode", err)
	}
	c.exec("a", `INSERT INTO t VALUES('y')`)
}

// TestOldBookkeepingIsRenamed: a database last opened when this package was
// named crdt keeps its op log, clock and site under the new names.
func TestOldBookkeepingIsRenamed(t *testing.T) {
	ctx := t.Context()
	p := filepath.Join(t.TempDir(), "old.db")
	createSchema(t, p, `CREATE TABLE t(id TEXT PRIMARY KEY, v TEXT)`, `INSERT INTO t VALUES('r', 'x')`)
	db, err := Open(ctx, p, CRDT(), WithSite("old"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	plain, err := openApplyDB(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bookkeeping {
		if _, err := plain.Exec(`ALTER TABLE ` + b + ` RENAME TO _crdt_` + strings.TrimPrefix(b, "_repl_")); err != nil {
			t.Fatal(err)
		}
	}
	plain.Close()
	db, err = Open(ctx, p, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := dump(t, db, `SELECT v FROM _repl_meta WHERE k='site'`); got != "old\n" {
		t.Fatalf("site %q, want old", got)
	}
	if got := dump(t, db, `SELECT count(*) FROM sqlite_master WHERE name LIKE '\_crdt\_%' ESCAPE '\'`); got != "0\n" {
		t.Fatalf("old tables left: %q", got)
	}
	if _, err := db.Exec(`UPDATE t SET v = 'y'`); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, db, `SELECT max(seq) FROM _repl_oplog`); got == "1\n" || got == "\n" {
		t.Fatalf("the log did not carry over: max seq %q", got)
	}
}

// fakeLog is a consensus log in one process: propose appends the entry and
// applies it on every node, in order, before returning -- what a raft
// ApplyFuture's Error gives the writer.
type fakeLog struct {
	mu      sync.Mutex
	entries [][]byte
	nodes   []*Quorum
	fail    error
	skip    *Quorum // a node propose does not apply on
}

func (l *fakeLog) quorum() *Quorum {
	var q *Quorum
	q = NewQuorum(func(ctx context.Context, entry []byte) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.fail != nil {
			return l.fail
		}
		l.entries = append(l.entries, entry)
		for _, n := range l.nodes {
			if n == l.skip {
				continue
			}
			if err := n.Apply(ctx, entry); err != nil {
				return err
			}
		}
		return nil
	})
	l.nodes = append(l.nodes, q)
	return q
}

// TestQuorumCommitsThroughTheLog: in Leader mode with a Quorum, a commit
// returns once the caller's log has it and every node applied it -- with no
// transport at all, the follower has the row the moment Exec returns. A
// proposal that fails leaves the row nowhere, a transaction's statements and
// DDL are one entry, concurrent commits on the writer each get their own seqs,
// and the recorded mode keeps a writer without a Quorum out.
func TestQuorumCommitsThroughTheLog(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	var lg fakeLog
	qa, qb := lg.quorum(), lg.quorum()
	yes, no := func() bool { return true }, func() bool { return false }
	open := func(name string, writer func() bool, q *Quorum) *sql.DB {
		p := filepath.Join(dir, name+".db")
		createSchema(t, p, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT UNIQUE)`)
		db, err := Open(ctx, p, Leader(writer, q), WithSite(name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	a, b := open("a", yes, qa), open("b", no, qb)

	res, err := a.Exec(`INSERT INTO t(v) VALUES('one')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dump(t, b, `SELECT id, v FROM t`), fmt.Sprintf("%d|one\n", id); got != want {
		t.Fatalf("follower right after the commit: %q, want %q", got, want)
	}
	if got := dump(t, a, `SELECT v FROM t`); got != "one\n" {
		t.Fatalf("writer: %q", got)
	}

	lg.fail = errors.New("no quorum")
	if _, err := a.Exec(`INSERT INTO t(v) VALUES('lost')`); err == nil || !strings.Contains(err.Error(), "no quorum") {
		t.Fatalf("a commit whose proposal failed: %v", err)
	}
	lg.fail = nil
	for _, db := range []*sql.DB{a, b} {
		if got := dump(t, db, `SELECT v FROM t ORDER BY v`); got != "one\n" {
			t.Fatalf("after a failed proposal: %q", got)
		}
	}

	tx, err := a.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`INSERT INTO t(v) VALUES('two')`, `ALTER TABLE t ADD COLUMN w TEXT`, `UPDATE t SET w = 'x'`} {
		if _, err := tx.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	before := len(lg.entries)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(lg.entries) != before+1 {
		t.Fatalf("a transaction made %d entries, want 1", len(lg.entries)-before)
	}
	if got := dump(t, b, `SELECT v, w FROM t ORDER BY v`); got != "one|x\ntwo|x\n" {
		t.Fatalf("follower after the transaction: %q", got)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10 {
				// Contending commits on the writer are refused SQLITE_BUSY past
				// the driver's own retries, as on any database; the caller
				// retries.
				for {
					_, err := a.Exec(`INSERT INTO t(v) VALUES(?)`, fmt.Sprintf("g%d-%d", g, i))
					if errors.Is(err, engine.ErrBusy) {
						continue
					}
					if err != nil {
						errs <- err
					}
					break
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := dump(t, b, `SELECT count(*) FROM t`); got != "42\n" {
		t.Fatalf("follower after concurrent commits: %q rows", got)
	}
	// No gap and no seq handed out twice: a commit whose transaction began
	// before another landed is refused (and retried), not stamped on top of it.
	if got := dump(t, a, `SELECT count(*) = max(seq) FROM _repl_oplog WHERE site = 'a'`); got != "1\n" {
		t.Fatalf("the writer's seqs are not contiguous")
	}

	// A transaction computed on what the database held when it began does not
	// commit over a commit that landed since: no lost update.
	if _, err := a.Exec(`INSERT INTO t(id, v, w) VALUES(1000, 'counter', '0')`); err != nil {
		t.Fatal(err)
	}
	slow, err := a.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slow.Exec(`UPDATE t SET w = w + 1 WHERE id = 1000`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(`UPDATE t SET w = w + 1 WHERE id = 1000`); err != nil {
		t.Fatal(err)
	}
	if err := slow.Commit(); !errors.Is(err, engine.ErrBusy) {
		t.Fatalf("a transaction begun before another commit landed: %v, want ErrBusy", err)
	}
	if got := dump(t, b, `SELECT w FROM t WHERE id = 1000`); got != "1\n" {
		t.Fatalf("counter on the follower: %q, want 1", got)
	}

	conn, err := a.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE scratch(x)`); err != nil {
		t.Fatal(err)
	}
	tx2, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(`INSERT INTO scratch VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(`INSERT INTO t(v) VALUES('with temp')`); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err == nil || !strings.Contains(err.Error(), "TEMP") {
		t.Fatalf("a quorum commit that also changes TEMP: %v", err)
	}

	// Retire is a write too: through the log, on the writer only.
	before = len(lg.entries)
	if err := Retire(ctx, a, "gone"); err != nil {
		t.Fatal(err)
	}
	if len(lg.entries) != before+1 {
		t.Fatal("Retire did not go through the log")
	}
	if err := Retire(ctx, b, "gone2"); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("Retire on a follower: %v, want ErrNotWriter", err)
	}

	lg.skip = qa
	if _, err := a.Exec(`INSERT INTO t(v) VALUES('unapplied')`); err == nil || !strings.Contains(err.Error(), "not applied here") {
		t.Fatalf("a proposal not applied on the writer: %v", err)
	}
	lg.skip = nil

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(ctx, filepath.Join(dir, "a.db"), Leader(yes), WithSite("a")); !errors.Is(err, ErrMode) {
		if err == nil {
			db.Close()
		}
		t.Fatalf("a quorum database reopened without a Quorum: %v, want ErrMode", err)
	}
}
