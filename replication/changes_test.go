package replication

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// changeLog records what a Changes observer was told, per table: how many
// calls, how many keys, and how many calls named the whole table.
type changeLog struct {
	mu    sync.Mutex
	calls map[string]int
	keys  map[string]int
	whole map[string]int
}

func (l *changeLog) Changed(table string, pks [][]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls == nil {
		l.calls, l.keys, l.whole = map[string]int{}, map[string]int{}, map[string]int{}
	}
	l.calls[table]++
	l.keys[table] += len(pks)
	if pks == nil {
		l.whole[table]++
	}
}

// reset forgets what l was told: Open reports the tables it rebuilds.
func (l *changeLog) reset() {
	l.mu.Lock()
	l.calls, l.keys, l.whole = nil, nil, nil
	l.mu.Unlock()
}

func (l *changeLog) get(table string) (calls, keys, whole int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[table], l.keys[table], l.whole[table]
}

// waitCalls waits until l was told of table's rows at least n times, or of
// the whole table when whole is set.
func (l *changeLog) waitCalls(t *testing.T, table string, n int, whole bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		calls, keys, w := l.get(table)
		got := calls - w
		if whole {
			got = w
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: told %d times (%d keys, %d whole), want %d (whole %v)", table, calls, keys, w, n, whole)
		}
	}
}

// TestChangesEveryPath: a node's observer is told of its own commits before
// they return, with the changed rows' keys, and of a peer's once they are
// ingested here; a schema change names every table, the new one included.
func TestChangesEveryPath(t *testing.T) {
	ctx := context.Background()
	net := NewMemNetwork()
	dir := t.TempDir()
	pa, pb := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	createSchema(t, pa, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT NOT NULL)`)
	createSchema(t, pb, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT NOT NULL)`)
	var la, lb changeLog
	a, err := Open(ctx, pa, CRDT(), memSync(net), WithSyncInterval(20*time.Millisecond), WithChanges(&la))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, pb, CRDT(), memSync(net), WithSyncInterval(20*time.Millisecond), WithChanges(&lb))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	la.reset()
	lb.reset()

	exec := func(db *sql.DB, q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Background sync may name a table whole at any time (a peer's schema
	// ops), so the counts below are of the calls naming rows.
	exec(a, `INSERT INTO notes VALUES('n1', 'one'), ('n2', 'two')`)
	if calls, keys, whole := la.get("notes"); calls-whole != 1 || keys != 2 {
		t.Fatalf("a's own commit: told rows %d times with %d keys, want once with 2", calls-whole, keys)
	}
	lb.waitCalls(t, "notes", 1, false)
	// The two rows reach b as two ops, which it may apply one at a time: a
	// delete of n2 before n2 arrives deletes nothing, and n2 then stays.
	for deadline := time.Now().Add(10 * time.Second); dump(t, b, `SELECT count(*) FROM notes`) != "2\n"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a's insert did not reach b")
		}
	}
	exec(a, `UPDATE notes SET body = 'uno' WHERE id = 'n1'`)
	if calls, keys, whole := la.get("notes"); calls-whole != 2 || keys != 3 {
		t.Fatalf("a's update: told rows %d times with %d keys, want twice with 3", calls-whole, keys)
	}
	_, _, wholeBefore := la.get("notes")
	exec(b, `DELETE FROM notes WHERE id = 'n2'`)
	for deadline := time.Now().Add(10 * time.Second); dump(t, a, `SELECT count(*) FROM notes`) != "1\n"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("b's delete did not reach a")
		}
	}
	// a is told of it by the row's key when the op is ingested, or of the
	// whole table when the delete reaches it in a snapshot page.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		calls, _, whole := la.get("notes")
		if calls-whole >= 3 || whole > wholeBefore {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("b's delete reached a untold: %d calls, %d whole", calls, whole)
		}
	}

	exec(a, `CREATE TABLE tags(id TEXT PRIMARY KEY, name TEXT)`)
	if _, _, whole := la.get("tags"); whole == 0 {
		t.Fatal("a's CREATE TABLE: tags not told")
	}
	if _, _, whole := la.get("notes"); whole == 0 {
		t.Fatal("a's CREATE TABLE: notes not told")
	}
	lb.waitCalls(t, "tags", 1, true)
	exec(b, `INSERT INTO tags VALUES('t1', 'red')`)
	la.waitCalls(t, "tags", 1, false)
}

// TestChangesQuorum: in Leader mode with a Quorum, a commit applies on every
// node through the log, an op at a time, and each node's observer is told of
// every row before the commit returns.
func TestChangesQuorum(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	var lg fakeLog
	qa, qb := lg.quorum(), lg.quorum()
	var la, lb changeLog
	open := func(name string, writer func() bool, q *Quorum, l *changeLog) *sql.DB {
		p := filepath.Join(dir, name+".db")
		createSchema(t, p, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`)
		db, err := Open(ctx, p, Leader(writer, q), WithSite(name), WithChanges(l))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	a := open("a", func() bool { return true }, qa, &la)
	open("b", func() bool { return false }, qb, &lb)
	la.reset()
	lb.reset()
	if _, err := a.Exec(`INSERT INTO t(v) VALUES('one'), ('two')`); err != nil {
		t.Fatal(err)
	}
	for _, l := range []*changeLog{&la, &lb} {
		if calls, keys, whole := l.get("t"); keys != 2 || whole != 0 {
			t.Fatalf("told %d times with %d keys (%d whole), want 2 keys", calls, keys, whole)
		}
	}
}

// TestChangesImage: a local delete that frees a UNIQUE value a held-out row
// wants tells the node of the table the row is put back into, and a node
// that joins by taking a peer's image is told of every table.
func TestChangesImage(t *testing.T) {
	lowImageMin(t)
	c := newCluster(t)
	var la, lc changeLog
	c.openWith("a", []Option{WithChanges(&la)}, imageDDL...)
	c.open("b")
	imageHistory(c)
	la.reset()
	c.exec("a", `DELETE FROM u WHERE e = 'dup'`)
	la.waitCalls(t, "u", 1, false)
	la.waitCalls(t, "u", 1, true)
	c.converge("", imageRowsT, imageRowsK, imageRowsU, clockQ, heldQ)

	c.openWith("c", []Option{WithChanges(&lc)})
	waitImage(t, c.dbs["c"])
	for _, tbl := range []string{"t", "k", "u"} {
		lc.waitCalls(t, tbl, 1, true)
	}
}
