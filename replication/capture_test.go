package replication

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// openSynced makes a fresh database with ddl and opens it with crdt.Open (no
// transport: the test wires one, or none), returning the Syncer behind it too.
func openSynced(t *testing.T, site string, ddl ...string) (*Syncer, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), site+".db")
	createSchema(t, path, ddl...)
	s, db, err := openSyncer(context.Background(), path, CRDT(), WithSite(site))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return s, db
}

// wrapUsers is openSynced over a users table.
func wrapUsers(t *testing.T, site string) (*Syncer, *sql.DB) {
	t.Helper()
	return openSynced(t, site, usersDDL)
}

func userDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b []byte
	rows, err := db.Query(`SELECT id, name, age FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var age sql.NullInt64
		if err := rows.Scan(&id, &name, &age); err != nil {
			t.Fatal(err)
		}
		ageStr := "NULL"
		if age.Valid {
			ageStr = itoa(age.Int64)
		}
		b = append(b, (id + "|" + name + "|" + ageStr + "\n")...)
	}
	return string(b)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// replayToFreshStore feeds a syncer's captured ops into a brand-new store and
// returns that store's user-table dump — proving the captured ops are correct
// and self-contained.
func replayToFreshStore(t *testing.T, src *Syncer) string {
	t.Helper()
	ctx := context.Background()
	ops, err := src.Store().OpsSince(ctx, src.Site(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "replay.db")
	db, err := openApplyDB("file:" + path + "?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := OpenStore(ctx, db, "replay") // the schema arrives with the ops
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if err := st.Ingest(ctx, op, false); err != nil {
			t.Fatalf("replay ingest: %v", err)
		}
	}
	return userDump(t, db)
}

// TestCaptureAutocommit covers the majority write path: single statements with
// no explicit transaction, which the driver must wrap so capture is atomic.
func TestCaptureAutocommit(t *testing.T) {
	ctx := context.Background()
	s, db := wrapUsers(t, "auto")

	if _, err := db.Exec(`INSERT INTO users(id,name,age) VALUES('u1','alice',30)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE users SET age=31 WHERE id='u1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,name,age) VALUES('u2','bob',25)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id='u2'`); err != nil {
		t.Fatal(err)
	}

	// Each autocommit write produced exactly one op.
	ops, err := s.Store().OpsSince(ctx, s.Site(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ops = rowOps(ops)
	if len(ops) != 4 {
		t.Fatalf("captured %d ops, want 4", len(ops))
	}

	want := "u1|alice|31\n"
	if got := userDump(t, db); got != want {
		t.Fatalf("user table = %q, want %q", got, want)
	}
	// The captured ops, replayed on a fresh store, reproduce the same state.
	if got := replayToFreshStore(t, s); got != want {
		t.Fatalf("replayed = %q, want %q", got, want)
	}
}

// TestCaptureExplicitTxAtomic checks a multi-statement transaction captures all
// its writes and that a rollback captures none (atomic with the user's data).
func TestCaptureExplicitTxAtomic(t *testing.T) {
	ctx := context.Background()
	s, db := wrapUsers(t, "tx")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Exec(`INSERT INTO users(id,name,age) VALUES('u1','alice',30)`)
	tx.Exec(`INSERT INTO users(id,name,age) VALUES('u2','bob',25)`)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// A rolled-back transaction leaves no data and no ops.
	tx2, _ := db.Begin()
	tx2.Exec(`INSERT INTO users(id,name,age) VALUES('u3','carol',40)`)
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}

	ops, err := s.Store().OpsSince(ctx, s.Site(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ops = rowOps(ops)
	if len(ops) != 2 {
		t.Fatalf("captured %d ops, want 2 (rollback must capture none)", len(ops))
	}
	if got, want := userDump(t, db), "u1|alice|30\nu2|bob|25\n"; got != want {
		t.Fatalf("user table = %q, want %q", got, want)
	}
	if got := replayToFreshStore(t, s); got != userDump(t, db) {
		t.Fatalf("replay %q != origin %q", got, userDump(t, db))
	}
}

// TestCapturePreparedStmt covers prepared-statement writes (which bypass
// conn.exec and go straight to stmt.exec).
func TestCapturePreparedStmt(t *testing.T) {
	ctx := context.Background()
	s, db := wrapUsers(t, "prep")

	stmt, err := db.Prepare(`INSERT INTO users(id,name,age) VALUES(?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for _, r := range []struct {
		id, name string
		age      int
	}{{"u1", "alice", 30}, {"u2", "bob", 25}} {
		if _, err := stmt.Exec(r.id, r.name, r.age); err != nil {
			t.Fatal(err)
		}
	}

	ops, err := s.Store().OpsSince(ctx, s.Site(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ops = rowOps(ops)
	if len(ops) != 2 {
		t.Fatalf("captured %d ops via prepared stmt, want 2", len(ops))
	}
	if got := replayToFreshStore(t, s); got != userDump(t, db) {
		t.Fatalf("replay %q != origin %q", got, userDump(t, db))
	}
}

// TestPKValidation rejects unsupported table shapes at Open.
func TestPKValidation(t *testing.T) {
	cases := []struct {
		name string
		ddl  string
	}{
		{"without-rowid", `CREATE TABLE bad(id TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID`},
		{"composite-pk-nocase", `CREATE TABLE bad(a TEXT, b TEXT, v TEXT, PRIMARY KEY(a, b COLLATE NOCASE))`},
		{"nocase-pk", `CREATE TABLE bad(id TEXT PRIMARY KEY COLLATE NOCASE, v TEXT)`},
		{"nocase-pk-constraint", `CREATE TABLE bad(id TEXT, v TEXT, PRIMARY KEY(id COLLATE NOCASE))`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.db")
			createSchema(t, path, tc.ddl)
			db, err := Open(context.Background(), path, CRDT())
			if err == nil {
				db.Close()
				t.Fatalf("Open accepted %s table, want rejection", tc.name)
			}
		})
	}
}

// rowOps drops the schema ops (the table's create, from genesis) from ops.
func rowOps(ops []Op) []Op {
	var out []Op
	for _, op := range ops {
		if op.Kind != OpSchema {
			out = append(out, op)
		}
	}
	return out
}
