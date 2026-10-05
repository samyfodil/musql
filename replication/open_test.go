package replication

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/musql/driver"
)

// memSync is the WithTransport option for an in-memory network: each opened
// database joins net under its own site id.
func memSync(net *MemNetwork) Option {
	return WithTransport(func(ctx context.Context, s *Syncer) (Transport, error) {
		return net.AddNode(PeerID(s.Site()), s.OpSource()), nil
	})
}

// createSchema makes path's schema with a plain connection, as an application
// would before (or without) sync.
func createSchema(t *testing.T, path string, ddl ...string) {
	t.Helper()
	db, err := openApplyDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range ddl {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// dump renders q's rows, one line each, cells joined by "|".
func dump(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			if i > 0 {
				b.WriteByte('|')
			}
			if x, ok := v.([]byte); ok {
				v = string(x)
			}
			fmt.Fprint(&b, v)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TestOpenSyncsEveryTable: two databases opened with crdt.Open and nothing else
// -- no site id, no table list, no Attach, no Run -- converge on every table,
// both directions, including a table the caller never named.
func TestOpenSyncsEveryTable(t *testing.T) {
	ctx := context.Background()
	net := NewMemNetwork()
	dir := t.TempDir()
	schema := []string{
		`CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT NOT NULL)`,
		`CREATE TABLE tags(id TEXT PRIMARY KEY, name TEXT)`,
	}
	pa, pb := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	createSchema(t, pa, schema...)
	createSchema(t, pb, schema...)

	a, err := Open(ctx, pa, CRDT(), memSync(net), WithSyncInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, pb, CRDT(), memSync(net), WithSyncInterval(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	for _, s := range []string{`INSERT INTO notes VALUES('n1','hello')`, `INSERT INTO tags VALUES('t1','red')`} {
		if _, err := a.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Exec(`INSERT INTO notes VALUES('n2','world')`); err != nil {
		t.Fatal(err)
	}

	const q = `SELECT id, body FROM notes ORDER BY id`
	const qt = `SELECT id, name FROM tags ORDER BY id`
	want, wantT := "n1|hello\nn2|world\n", "t1|red\n"
	deadline := time.Now().Add(10 * time.Second)
	for {
		ga, gb, gt := dump(t, a, q), dump(t, b, q), dump(t, b, qt)
		if ga == want && gb == want && gt == wantT {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not converge: A=%q B=%q B.tags=%q", ga, gb, gt)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestOpenKeepsItsSiteID: the site id Open generates is kept in the database,
// so a reopen continues the same op log; a conflicting WithSite is refused.
func TestOpenKeepsItsSiteID(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "s.db")
	createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
	site := func() string {
		db, err := openApplyDB(p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var v []byte
		if err := db.QueryRow(`SELECT v FROM _repl_meta WHERE k='site'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return string(v)
	}
	db, err := Open(ctx, p, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notes VALUES('n1','x')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	first := site()
	if len(first) != 32 {
		t.Fatalf("generated site id %q, want 32 hex chars", first)
	}
	db, err = Open(ctx, p, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notes VALUES('n2','y')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if got := site(); got != first {
		t.Fatalf("site id changed across reopen: %q -> %q", first, got)
	}
	if _, err := Open(ctx, p, CRDT(), WithSite("someone-else")); err == nil {
		t.Fatal("a WithSite that differs from the kept site id was accepted")
	}
}

// TestOpenCloseStopsSync: db.Close stops the background sync and releases the
// site, so the same database can be opened again in the same process -- and
// while it is open, a second Open of it is refused and leaves the first working.
func TestOpenCloseStopsSync(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "c.db")
	createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
	for i := 0; i < 3; i++ {
		db, err := Open(ctx, p, CRDT(), memSync(NewMemNetwork()), WithSyncInterval(5*time.Millisecond))
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if again, err := Open(ctx, p, CRDT()); err == nil {
			again.Close()
			t.Fatalf("open %d: a second Open of an open database was accepted", i)
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO notes VALUES('n','v')`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

// TestOpenRowidTables: an INTEGER PRIMARY KEY table and a table with no declared
// key both sync. Two nodes inserting with auto-assigned ids while partitioned
// draw from their own ranges, so no insert overwrites the other's; an explicit
// id is the same row on both; and moving a row to a new key moves it on the peer.
func TestOpenRowidTables(t *testing.T) {
	ctx := context.Background()
	net := NewMemNetwork()
	dir := t.TempDir()
	schema := []string{
		`CREATE TABLE items(id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE log(msg TEXT)`,
	}
	syncers := map[string]*Syncer{}
	open := func(name string) *sql.DB {
		p := filepath.Join(dir, name+".db")
		createSchema(t, p, schema...)
		sy, db, err := openSyncer(ctx, p, CRDT(), WithSite(name), memSync(net), WithSyncInterval(20*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		syncers[name] = sy
		return db
	}
	a, b := open("a"), open("b")
	net.Partition("a", "b", true)
	exec := func(db *sql.DB, ss ...string) {
		t.Helper()
		for _, s := range ss {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	exec(a, `INSERT INTO items(v) VALUES('a1'),('a2')`, `INSERT INTO log VALUES('a')`,
		`INSERT INTO items VALUES(7, 'from a')`)
	exec(b, `INSERT INTO items(v) VALUES('b1'),('b2')`, `INSERT INTO log VALUES('b')`,
		`INSERT INTO items VALUES(7, 'from b')`)
	exec(a, `UPDATE items SET id = 5 WHERE v = 'a1'`)
	net.Partition("a", "b", false)

	const q = `SELECT id, v FROM items ORDER BY id`
	const ql = `SELECT rowid, msg FROM log ORDER BY rowid`
	deadline := time.Now().Add(10 * time.Second)
	for {
		ia, ib, la, lb := dump(t, a, q), dump(t, b, q), dump(t, a, ql), dump(t, b, ql)
		if ia == ib && la == lb && strings.Count(ia, "\n") == 5 && strings.Count(la, "\n") == 2 {
			// a1 moved to 5, ONE row 7 (last writer wins), a2, b1, b2.
			if !strings.HasPrefix(ia, "5|a1\n7|from ") {
				t.Fatalf("items: %q", ia)
			}
			return
		}
		if time.Now().After(deadline) {
			clk := `SELECT col, hlc, site, hex(val) FROM _repl_clock WHERE pk = X'` + fmt.Sprintf("%X", EncodePK(int64(7))) + `' ORDER BY col`
			ops := `SELECT site, seq, hlc, op FROM _repl_oplog WHERE pk = X'` + fmt.Sprintf("%X", EncodePK(int64(7))) + `' ORDER BY site, seq`
			var st strings.Builder
			for _, site := range []string{"a", "b"} {
				sy := syncers[site]
				fmt.Fprintf(&st, "\n%s vv=%v", site, sy.store.LocalVV())
				fresh, _ := openApplyDB(filepath.Join(dir, site+".db"))
				fmt.Fprintf(&st, " fresh items=%q", dump(t, fresh, q))
				fresh.Close()
				var viaApply string
				rows, _ := sy.store.db.Query(`SELECT group_concat(v) FROM items`)
				for rows.Next() {
					rows.Scan(&viaApply)
				}
				rows.Close()
				fmt.Fprintf(&st, " viaApply=%q", viaApply)
				for _, from := range []string{"a", "b"} {
					got, err := sy.store.OpsSince(ctx, from, 0, 0)
					fmt.Fprintf(&st, " opsSince(%s)=%d %v", from, len(got), err)
				}
			}
			t.Fatalf("did not converge:\nA items=%q log=%q\nB items=%q log=%q\nA clock(7)=%q ops=%q\nB clock(7)=%q ops=%q%s",
				ia, la, ib, lb, dump(t, a, clk), dump(t, a, ops), dump(t, b, clk), dump(t, b, ops), st.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestOpenSyncsMainOnly: rows written to a TEMP table, or to an ATTACHed
// database's table, that share a synced table's name stay on this node -- a
// peer's main table never receives them.
func TestOpenSyncsMainOnly(t *testing.T) {
	ctx := context.Background()
	net := NewMemNetwork()
	dir := t.TempDir()
	open := func(name string) *sql.DB {
		p := filepath.Join(dir, name+".db")
		createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
		db, err := Open(ctx, p, CRDT(), WithSite(name), memSync(net), WithSyncInterval(20*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	a, b := open("a"), open("b")
	aux := filepath.Join(dir, "aux.db")
	createSchema(t, aux, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
	conn, err := a.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, s := range []string{
		`CREATE TEMP TABLE notes(id TEXT PRIMARY KEY, body TEXT)`,
		`INSERT INTO temp.notes VALUES('temp', 'x')`,
		`ATTACH '` + aux + `' AS aux`,
		`INSERT INTO aux.notes VALUES('aux', 'x')`,
		`INSERT INTO main.notes VALUES('main', 'x')`,
	} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for dump(t, b, `SELECT id FROM notes`) != "main\n" {
		if time.Now().After(deadline) {
			t.Fatalf("B: %q, want only main's row", dump(t, b, `SELECT id FROM notes ORDER BY id`))
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // a few more rounds: nothing else may arrive
	if got := dump(t, b, `SELECT id FROM notes ORDER BY id`); got != "main\n" {
		t.Fatalf("B: %q, want only main's row", got)
	}
}

// TestOpenFailedCommitLeavesNoSeqHole: a commit that fails after capture wrote
// its ops (here: another connection committed while the transaction was open)
// leaves no gap in the site's seqs, which peers could never get past.
func TestOpenFailedCommitLeavesNoSeqHole(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "h.db")
	createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`, `CREATE TABLE other(a)`)
	db, err := Open(ctx, p, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO notes VALUES('n1', 'x')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO notes VALUES('n2', 'x')`); err != nil {
		t.Fatal(err)
	}
	createSchema(t, p, `INSERT INTO other VALUES(1)`) // commits in between
	if err := tx.Commit(); err == nil {
		t.Fatal("a transaction committed over another connection's commit")
	}
	if _, err := db.Exec(`INSERT INTO notes VALUES('n3', 'x')`); err != nil {
		t.Fatal(err)
	}
	// 1 and 2 are genesis's creates of the two tables, 3 the mode; n1 is 4
	// and n3 is 5.
	if got := dump(t, db, `SELECT group_concat(seq) FROM (SELECT seq FROM _repl_oplog ORDER BY seq)`); got != "1,2,3,4,5\n" {
		t.Fatalf("seqs %q, want 1,2,3,4,5", got)
	}
}

// TestOpenKeyIdentity: a NULL key is refused rather than synced as one identity
// for many rows, and an integral REAL key is the INTEGER it equals.
func TestOpenKeyIdentity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.db")
	createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
	db, err := Open(context.Background(), p, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO notes VALUES(NULL, 'x')`); err == nil {
		t.Fatal("a NULL primary key was accepted")
	}
	if got := dump(t, db, `SELECT count(*) FROM notes`); got != "0\n" {
		t.Fatalf("the refused row was kept: count %q", got)
	}
	if !bytes.Equal(EncodePK(1.0), EncodePK(int64(1))) || bytes.Equal(EncodePK(1.5), EncodePK(int64(1))) {
		t.Fatal("an integral REAL key is not the INTEGER key it equals")
	}
}

// TestOpenRefusesALogWithSharedRange: a database whose log already holds ops of
// two sites sharing a rowid range (written before nodes kept a range registry)
// may hold their rows merged as one, and Open says so instead of syncing on.
func TestOpenRefusesALogWithSharedRange(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "r.db")
	createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
	db, err := Open(ctx, p, CRDT(), WithSite("s94558"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	plain, err := openApplyDB(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO _repl_oplog(site,seq,hlc,tbl,pk,op,cells) VALUES('s113311',1,1,'t',x'01',0,NULL)`,
		`DELETE FROM _repl_ranges`,
	} {
		if _, err := plain.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	plain.Close()
	if db, err := Open(ctx, p, CRDT()); err == nil {
		db.Close()
		t.Fatal("Open accepted a log holding two sites of one rowid range")
	} else if !strings.Contains(err.Error(), "shares a rowid range") {
		t.Fatalf("got %v, want the shared-range refusal", err)
	}
}

// TestOpenNeverReusesADeletedRowid: a new row never gets the id of a deleted
// one, across a reopen too -- to a peer the id is the row, and its late update
// to the deleted row would land on the new one.
func TestOpenNeverReusesADeletedRowid(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "r.db")
	createSchema(t, p, `CREATE TABLE log(msg TEXT)`, `CREATE TABLE items(id INTEGER PRIMARY KEY, v TEXT)`)
	rowidOf := func(db *sql.DB, q string) int64 {
		t.Helper()
		res, err := db.Exec(q)
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var last [2]int64
	for round := range 2 {
		db, err := Open(ctx, p, CRDT(), WithSite("reuse"))
		if err != nil {
			t.Fatal(err)
		}
		for i, tbl := range []string{"log(msg)", "items(v)"} {
			name := strings.SplitN(tbl, "(", 2)[0]
			if round == 0 {
				rowidOf(db, `INSERT INTO `+tbl+` VALUES('a')`)
			}
			top := rowidOf(db, `INSERT INTO `+tbl+` VALUES('b')`)
			if round == 1 && top != last[i]+1 {
				t.Fatalf("%s after reopen: rowid %d, want %d (one past the deleted %d)", name, top, last[i]+1, last[i])
			}
			if _, err := db.Exec(`DELETE FROM `+name+` WHERE rowid = ?`, top); err != nil {
				t.Fatal(err)
			}
			if got := rowidOf(db, `INSERT INTO `+tbl+` VALUES('c')`); got != top+1 {
				t.Fatalf("%s: rowid %d after deleting %d, want %d", name, got, top, top+1)
			}
			if _, err := db.Exec(`DELETE FROM `+name+` WHERE rowid = ?`, top+1); err != nil {
				t.Fatal(err)
			}
			last[i] = top + 1
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOpenConnStateIsTheUsers: changes(), total_changes() and
// last_insert_rowid() -- and Result's LastInsertId and RowsAffected -- report the
// user's statements, as on a plain database: the op-log and clock rows capture
// writes at commit are not the user's. LastInsertId once came back as the
// rowid of crdt's own op-log row.
func TestOpenConnStateIsTheUsers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	schema := `CREATE TABLE items(id INTEGER PRIMARY KEY, v TEXT)`
	plainPath, syncedPath := filepath.Join(dir, "plain.db"), filepath.Join(dir, "synced.db")
	createSchema(t, plainPath, schema)
	createSchema(t, syncedPath, schema)
	plain, err := openApplyDB(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	synced, err := Open(ctx, syncedPath, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer synced.Close()
	trace := func(db *sql.DB) string {
		t.Helper()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var b strings.Builder
		for _, q := range []string{
			`INSERT INTO items VALUES(7, 'a')`,
			`INSERT INTO items VALUES(9, 'b'), (11, 'c')`,
			`UPDATE items SET v = 'x' WHERE id > 8`,
			`DELETE FROM items WHERE id = 7`,
			`BEGIN`,
			`INSERT INTO items VALUES(20, 'd')`,
			`UPDATE items SET v = 'y'`,
			`COMMIT`,
		} {
			res, err := conn.ExecContext(ctx, q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			last, _ := res.LastInsertId()
			n, _ := res.RowsAffected()
			var ch, tot, lr int64
			if err := conn.QueryRowContext(ctx, `SELECT changes(), total_changes(), last_insert_rowid()`).Scan(&ch, &tot, &lr); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s: last=%d n=%d changes=%d total=%d lastrowid=%d\n", q, last, n, ch, tot, lr)
		}
		return b.String()
	}
	if p, s := trace(plain), trace(synced); p != s {
		t.Fatalf("synced database's connection state differs:\nplain:\n%s\nsynced:\n%s", p, s)
	}
}

// TestPlainConnectionCannotWrite: a write to a replicated database through any
// connection but replication's would never reach a peer, so it is refused,
// loudly, and reads stay open. The pragma that lifts query_only does not lift
// it, and a database that is not replicated is untouched.
func TestPlainConnectionCannotWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "r.db")
	createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`, `INSERT INTO notes VALUES('n1', 'x')`)
	db, err := Open(ctx, p, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plain, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	plain.SetMaxOpenConns(1)
	if got := dump(t, plain, `SELECT body FROM notes`); got != "x\n" {
		t.Fatalf("a plain read: %q", got)
	}
	for _, s := range []string{
		`INSERT INTO notes VALUES('n2', 'y')`,
		`UPDATE notes SET body = 'z'`,
		`CREATE TABLE other(a)`,
		`DROP TABLE _repl_meta`,
		`PRAGMA query_only = 0`,
		`DELETE FROM notes`,
	} {
		_, err := plain.Exec(s)
		if s == `PRAGMA query_only = 0` {
			continue // accepted, and lifts nothing
		}
		if err == nil || !strings.Contains(err.Error(), "replicated") {
			t.Errorf("%s through a plain connection: %v, want the refusal", s, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO notes VALUES('n3', 'w')`); err != nil {
		t.Fatalf("the replicated handle: %v", err)
	}
	if got := dump(t, plain, `SELECT id FROM notes ORDER BY id`); got != "n1\nn3\n" {
		t.Fatalf("after the refusals: %q", got)
	}

	// A replicated database ATTACHed to another one's capturing connection is
	// still locked: capture covers that connection's own main file only.
	p2 := filepath.Join(dir, "r2.db")
	createSchema(t, p2, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
	db2, err := Open(ctx, p2, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH '`+p2+`' AS other`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO other.notes VALUES('sneak', 'x')`); err == nil || !strings.Contains(err.Error(), "replicated") {
		t.Fatalf("a write to an ATTACHed replicated database: %v, want the refusal", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO main.notes VALUES('n4', 'v')`); err != nil {
		t.Fatalf("the connection's own database: %v", err)
	}

	other := filepath.Join(dir, "plain.db")
	odb, err := sql.Open("sqlite", other)
	if err != nil {
		t.Fatal(err)
	}
	defer odb.Close()
	if _, err := odb.Exec(`CREATE TABLE t(a)`); err != nil {
		t.Fatalf("a database that is not replicated: %v", err)
	}
}

// TestGuardOnAnUnguardedReplicatedFile: a database replicated before the guard
// lived in the file has the bookkeeping, a mode, and no guard -- so the Open
// after it writes nothing of its own. Open records the guard anyway, and it
// outlives the replicated handle.
func TestGuardOnAnUnguardedReplicatedFile(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "r.db")
	createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
	db, err := Open(ctx, p, CRDT())
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	plainWrite := func() error {
		plain, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		defer plain.Close()
		_, err = plain.Exec(`INSERT OR REPLACE INTO notes VALUES('n', 'x')`)
		return err
	}
	// Lift the guard, as a file from before it would be.
	apply, err := openApplyDB(p)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := apply.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(dc any) error { return dc.(*driver.Conn).SetCaptureGuard("") }); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	apply.Close()
	if err := plainWrite(); err != nil {
		t.Fatalf("a plain write with the guard lifted: %v", err)
	}
	if db, err = Open(ctx, p, CRDT()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := plainWrite(); err == nil || !strings.Contains(err.Error(), "replicated") {
		t.Fatalf("a plain write after Open closed: %v, want the refusal", err)
	}
}

// TestAssignedRowidRanges: two sites whose hashed ranges collide (s94558 and
// s113311) given ranges of their own with WithRowidRange insert while apart and
// both rows sync -- each id from its own range. The range is part of the site
// id, so reopening with another one is refused.
func TestAssignedRowidRanges(t *testing.T) {
	c := newCluster(t)
	a := c.openWith("a", []Option{WithSite("s94558"), WithRowidRange(1)}, `CREATE TABLE log(msg TEXT)`)
	b := c.openWith("b", []Option{WithSite("s113311"), WithRowidRange(2)}, `CREATE TABLE log(msg TEXT)`)
	c.partition(true)
	c.exec("a", `INSERT INTO log VALUES('from a')`)
	c.exec("b", `INSERT INTO log VALUES('from b')`)
	c.partition(false)
	c.converge("from a\nfrom b\n", `SELECT msg FROM log ORDER BY msg`)
	for db, want := range map[*sql.DB]string{a: "from a", b: "from b"} {
		var id int64
		if err := db.QueryRow(`SELECT rowid FROM log WHERE msg = ?`, want).Scan(&id); err != nil {
			t.Fatal(err)
		}
		lo, hi := RowidRange(map[string]string{"from a": "s94558#r1", "from b": "s113311#r2"}[want])
		if uint64(id) < lo || uint64(id) >= hi {
			t.Fatalf("%s got rowid %d, outside its range [%d, %d)", want, id, lo, hi)
		}
	}
	if lo, _ := RowidRange("x#r7"); lo != 7<<32 {
		t.Fatalf("RowidRange(x#r7) = %d", lo)
	}

	p := filepath.Join(t.TempDir(), "r.db")
	createSchema(t, p, `CREATE TABLE t(a)`)
	db, err := Open(context.Background(), p, CRDT(), WithSite("n"), WithRowidRange(5))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err := Open(context.Background(), p, CRDT(), WithRowidRange(6)); err == nil {
		db.Close()
		t.Fatal("reopening with another rowid range was accepted")
	}
	db, err = Open(context.Background(), p, CRDT(), WithSite("n"), WithRowidRange(5))
	if err != nil {
		t.Fatalf("reopening as it was: %v", err)
	}
	db.Close()
}

// TestWithMaxSizeRefusesLocalWritesOnly: a node's own writes stop at its
// limit, and a peer's rows are applied past it -- an op refused here would fail
// on this node forever, and it would never converge.
func TestWithMaxSizeRefusesLocalWritesOnly(t *testing.T) {
	ctx := context.Background()
	net := NewMemNetwork()
	dir := t.TempDir()
	const limit = 256 << 10
	open := func(name string, opts ...Option) *sql.DB {
		p := filepath.Join(dir, name+".db")
		createSchema(t, p, `CREATE TABLE notes(id TEXT PRIMARY KEY, body TEXT)`)
		db, err := Open(ctx, p, append([]Option{CRDT(), WithSite(name), memSync(net), WithSyncInterval(20 * time.Millisecond)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	a := open("a", WithMaxSize(limit))
	b := open("b")
	body := strings.Repeat("x", 2000)
	const rows = 300 // ~600 KiB of bodies, well past a's limit
	for i := range rows {
		if _, err := b.Exec(`INSERT INTO notes VALUES(?, ?)`, fmt.Sprint("b", i), body); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for dump(t, a, `SELECT count(*) FROM notes`) != fmt.Sprint(rows, "\n") {
		if time.Now().After(deadline) {
			t.Fatalf("a holds %q rows, want %d: a peer's rows were refused", dump(t, a, `SELECT count(*) FROM notes`), rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := a.Exec(`INSERT INTO notes VALUES('a1', ?)`, body); err == nil || !strings.Contains(err.Error(), "database or disk is full") {
		t.Fatalf("a's own write past its limit: %v, want full", err)
	}
	if got := dump(t, a, `PRAGMA max_size`); got != fmt.Sprint(limit, "\n") {
		t.Fatalf("a's max_size = %q", got)
	}
}
