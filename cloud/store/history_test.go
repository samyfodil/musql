package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
)

func TestMain(m *testing.M) {
	engine.Configure(engine.WithCommitHook(OnCommit))
	engine.RegisterFTS5()
	os.Exit(m.Run())
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// dump renders a database whole: its schema rows, then every table's rows
// with their rowids (key order for a WITHOUT ROWID table), shadow tables
// included, and an FTS5 query when there is one.
func dump(t *testing.T, path string) string {
	t.Helper()
	db := openDB(t, path)
	defer db.Close()
	var b strings.Builder
	q := func(s string) {
		rows, err := db.Query(s)
		if err != nil && strings.Contains(err.Error(), "unsupported") {
			fmt.Fprintf(&b, "-- %s: declined\n", s) // an fts5 segment layout the engine will not show
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "-- %s\n", s)
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if bs, ok := v.([]byte); ok {
					v = fmt.Sprintf("x'%x'", bs)
				}
				fmt.Fprintf(&b, "%v|", v)
			}
			b.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	q(`SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY rowid`)
	rows, err := db.Query(`SELECT name, wr FROM pragma_table_list WHERE schema = 'main' AND type IN ('table', 'shadow') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	type tbl struct {
		name string
		wr   bool
	}
	var tables []tbl
	for rows.Next() {
		var tb tbl
		if err := rows.Scan(&tb.name, &tb.wr); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, tb)
	}
	rows.Close()
	for _, tb := range tables {
		if tb.wr {
			q(`SELECT * FROM "` + tb.name + `" ORDER BY 1`)
		} else {
			q(`SELECT rowid, * FROM "` + tb.name + `" ORDER BY rowid`)
		}
	}
	var fts int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'ft'`).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	if fts > 0 {
		q(`SELECT rowid, body FROM ft WHERE ft MATCH 'apple' ORDER BY rowid`)
	}
	return b.String()
}

type point struct {
	at   time.Time
	dump string
}

// TestRestoreIsExact records the database after every commit across row
// writes, DDL, a trigger, an FTS5 table, a WITHOUT ROWID table and enough
// writes to compact, then restores every one of those times and compares.
func TestRestoreIsExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	db := openDB(t, path)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c)`)
	h, err := Open(path, filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var points []point
	mark := func() {
		points = append(points, point{time.Now(), dump(t, path)})
		time.Sleep(time.Millisecond)
	}
	mark()
	steps := []string{
		`INSERT INTO t(b, c) VALUES ('one', 1), ('two', 2), ('three', 3)`,
		`DELETE FROM t WHERE b = 'two'`,
		`INSERT INTO t(b, c) VALUES ('four', 4)`, // rowid 4 after a delete: not reused
		`UPDATE t SET c = c * 10 WHERE a > 1`,
		`CREATE INDEX tb ON t(b)`,
		`ALTER TABLE t ADD COLUMN d DEFAULT 'dflt'`,
		`CREATE TABLE log(id INTEGER PRIMARY KEY AUTOINCREMENT, what)`,
		`CREATE TRIGGER t_ins AFTER INSERT ON t BEGIN INSERT INTO log(what) VALUES ('ins ' || new.b); END`,
		`INSERT INTO t(b, c) VALUES ('five', 5)`,
		`CREATE VIRTUAL TABLE ft USING fts5(body)`,
		`INSERT INTO ft(body) VALUES ('apple pie'), ('banana'), ('apple tart')`,
		`DELETE FROM ft WHERE rowid = 1`,
		`CREATE TABLE wr(k TEXT PRIMARY KEY, v) WITHOUT ROWID`,
		`INSERT INTO wr VALUES ('b', 2), ('a', 1), ('c', 3)`,
		`UPDATE wr SET v = v + 100 WHERE k = 'b'`,
		`DELETE FROM t WHERE a = 1`,
	}
	for _, s := range steps {
		exec(t, db, s)
		mark()
	}
	for i := range 150 {
		exec(t, db, `INSERT INTO t(b, c) VALUES (?, ?)`, fmt.Sprintf("row %d %s", i, strings.Repeat("x", 200)), i)
		if i%25 == 0 {
			mark()
		}
	}
	exec(t, db, `DELETE FROM t WHERE c % 3 = 0`)
	mark()
	exec(t, db, `VACUUM`)
	mark()
	exec(t, db, `INSERT INTO ft(body) VALUES ('apple crumble')`)
	mark()
	if bases := len(h.Snapshots()); bases < 3 {
		t.Errorf("only %d bases: the run never rewrote the file", bases)
	}
	for i, p := range points {
		dst := filepath.Join(dir, fmt.Sprintf("r%d.musq", i))
		if err := h.Restore(p.at, dst); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		if got := dump(t, dst); got != p.dump {
			t.Fatalf("restore %d differs\nwant:\n%s\ngot:\n%s", i, p.dump, got)
		}
	}
	// A restored database is a working one: it takes writes and keeps its
	// rowid sequence.
	dst := filepath.Join(dir, "r-last.musq")
	if err := h.Restore(time.Now(), dst); err != nil {
		t.Fatal(err)
	}
	r := openDB(t, dst)
	exec(t, r, `INSERT INTO t(b) VALUES ('after')`)
	var a, logs int64
	if err := r.QueryRow(`SELECT max(a) FROM t`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	var want int64
	if err := db.QueryRow(`SELECT max(a) FROM t`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if err := r.QueryRow(`SELECT count(*) FROM log WHERE what = 'ins after'`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if a != want+1 || logs != 1 {
		t.Errorf("restored copy: max(a) %d want %d, trigger rows %d", a, want+1, logs)
	}
}

func TestRestoreDeclines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	db := openDB(t, path)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(a)`)
	before := time.Now()
	time.Sleep(time.Millisecond)
	h, err := Open(path, filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.Restore(before, filepath.Join(dir, "x.musq")); !errors.Is(err, ErrBeforeHistory) {
		t.Errorf("before the oldest snapshot: %v", err)
	}
	if err := h.Restore(time.Now().Add(time.Hour), filepath.Join(dir, "x.musq")); !errors.Is(err, ErrFuture) {
		t.Errorf("in the future: %v", err)
	}
	if err := h.Restore(time.Now(), path); err == nil {
		t.Errorf("restore over an existing file was accepted")
	}
}

// TestRepairFileAheadOfLog: commits made while nothing logged them (a crash
// between the commit and its record), and a torn record, are caught up at
// open.
func TestRepairFileAheadOfLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	hdir := filepath.Join(dir, "history")
	db := openDB(t, path)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`)
	h, err := Open(path, hdir)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO t(b) VALUES (1)`)
	mid := time.Now()
	midDump := dump(t, path)
	h.Close()
	exec(t, db, `INSERT INTO t(b) VALUES (2)`)
	exec(t, db, `CREATE TABLE u(x)`) // a rewrite, also unlogged
	exec(t, db, `INSERT INTO u VALUES (3)`)
	f, err := os.OpenFile(filepath.Join(hdir, "log"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{40, 0, 0, 0, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	h, err = Open(path, hdir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, c := range []struct {
		at   time.Time
		want string
	}{{mid, midDump}, {time.Now(), dump(t, path)}} {
		dst := filepath.Join(dir, fmt.Sprintf("r%d.musq", c.at.UnixNano()))
		if err := h.Restore(c.at, dst); err != nil {
			t.Fatal(err)
		}
		if got := dump(t, dst); got != c.want {
			t.Errorf("want:\n%s\ngot:\n%s", c.want, got)
		}
	}
	// And logging goes on from there.
	exec(t, db, `INSERT INTO u VALUES (4)`)
	dst := filepath.Join(dir, "after.musq")
	if err := h.Restore(time.Now(), dst); err != nil {
		t.Fatal(err)
	}
	if got, want := dump(t, dst), dump(t, path); got != want {
		t.Errorf("after repair: want:\n%s\ngot:\n%s", want, got)
	}
}

// TestRepairLogAheadOfFile: the file lost commits its log holds (here, an
// older copy put back in its place); open rolls it forward to the log's end.
func TestRepairLogAheadOfFile(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprint("rewrite=", rewrite), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "db.musq")
			hdir := filepath.Join(dir, "history")
			db := openDB(t, path)
			exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`)
			h, err := Open(path, hdir)
			if err != nil {
				t.Fatal(err)
			}
			exec(t, db, `INSERT INTO t(b) VALUES (1)`)
			saved := filepath.Join(dir, "saved")
			if err := os.Mkdir(saved, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{"", ".delta"} {
				if err := copyFile(path+s, filepath.Join(saved, "db.musq"+s), -1); err != nil {
					t.Fatal(err)
				}
			}
			exec(t, db, `INSERT INTO t(b) VALUES (2)`)
			if rewrite {
				exec(t, db, `CREATE INDEX tb ON t(b)`)
				exec(t, db, `INSERT INTO t(b) VALUES (3)`)
			}
			want := dump(t, path)
			db.Close()
			h.Close()
			engine.RemoveDatabaseFiles(path)
			for _, s := range []string{"", ".delta"} {
				if err := copyFile(filepath.Join(saved, "db.musq"+s), path+s, -1); err != nil {
					t.Fatal(err)
				}
			}
			h, err = Open(path, hdir)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			if got := dump(t, path); got != want {
				t.Errorf("rolled forward: want:\n%s\ngot:\n%s", want, got)
			}
			db = openDB(t, path)
			defer db.Close()
			exec(t, db, `INSERT INTO t(b) VALUES (9)`)
			dst := filepath.Join(dir, "r.musq")
			if err := h.Restore(time.Now(), dst); err != nil {
				t.Fatal(err)
			}
			if got, want := dump(t, dst), dump(t, path); got != want {
				t.Errorf("after roll-forward: want:\n%s\ngot:\n%s", want, got)
			}
			if n := len(h.Snapshots()); n != 1 && !rewrite || rewrite && n != 2 {
				t.Errorf("%d bases: the roll-forward took one it did not need", n)
			}
		})
	}
}

// TestConcurrentWriters: several connections commit at once; every logged
// point is a state some serial order of whole commits produced.
func TestConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	db := openDB(t, path)
	defer db.Close()
	db.SetMaxOpenConns(4)
	exec(t, db, `CREATE TABLE t(g, i, PRIMARY KEY (g, i))`)
	h, err := Open(path, filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	const writers, each = 4, 40
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	var mu sync.Mutex
	var times []time.Time
	for g := range writers {
		wg.Go(func() {
			for i := range each {
				for {
					_, err := db.Exec(`INSERT INTO t VALUES (?, ?)`, g, i)
					if err == nil {
						break
					}
					if !strings.Contains(err.Error(), "busy") && !strings.Contains(err.Error(), "locked") {
						errs <- err
						return
					}
				}
				mu.Lock()
				times = append(times, time.Now())
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for k, at := range times {
		if k%7 != 0 && k != len(times)-1 {
			continue
		}
		dst := filepath.Join(dir, fmt.Sprintf("r%d.musq", k))
		if err := h.Restore(at, dst); err != nil {
			t.Fatal(err)
		}
		r := openDB(t, dst)
		rows, err := r.Query(`SELECT g, count(*), max(i) FROM t GROUP BY g`)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for rows.Next() {
			var g, n, mx int
			if err := rows.Scan(&g, &n, &mx); err != nil {
				t.Fatal(err)
			}
			if mx != n-1 {
				t.Errorf("restore at %d: writer %d has %d rows up to %d: not a prefix", k, g, n, mx)
			}
			total += n
		}
		rows.Close()
		r.Close()
		if total < k+1 {
			t.Errorf("restore at write %d holds only %d rows", k, total)
		}
		if k == len(times)-1 && total != writers*each {
			t.Errorf("last restore holds %d rows, want %d", total, writers*each)
		}
	}
}

// TestBranchAndSchemaOnly: a branch is the parent at a time; a schema-only
// branch has every object and no rows, and works.
func TestBranchAndSchemaOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	db := openDB(t, path)
	defer db.Close()
	exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`)
	h, err := Open(path, filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, s := range []string{
		`CREATE TABLE log(what)`,
		`CREATE TRIGGER ti AFTER INSERT ON t BEGIN INSERT INTO log VALUES (new.b); END`,
		`CREATE VIRTUAL TABLE ft USING fts5(body)`,
		`CREATE TABLE wr(k PRIMARY KEY, v) WITHOUT ROWID`,
		`CREATE VIEW v AS SELECT b FROM t`,
		`CREATE INDEX tb ON t(b)`,
		`INSERT INTO t(b) VALUES ('x'), ('y')`,
		`INSERT INTO ft VALUES ('apple')`,
		`INSERT INTO wr VALUES (1, 2)`,
	} {
		exec(t, db, s)
	}
	branch := filepath.Join(dir, "branch.musq")
	if err := h.Restore(time.Now(), branch); err != nil {
		t.Fatal(err)
	}
	if got, want := dump(t, branch), dump(t, path); got != want {
		t.Errorf("branch: want:\n%s\ngot:\n%s", want, got)
	}
	so := filepath.Join(dir, "schema.musq")
	if err := SchemaOnly(context.Background(), path, so); err != nil {
		t.Fatal(err)
	}
	s := openDB(t, so)
	defer s.Close()
	schema := func(d *sql.DB) string {
		var out string
		if err := d.QueryRow(`SELECT group_concat(type || ' ' || name || ' ' || coalesce(sql, ''), char(10)) FROM (SELECT * FROM sqlite_schema ORDER BY name)`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got, want := schema(s), schema(db); got != want {
		t.Errorf("schema-only schema: want:\n%s\ngot:\n%s", want, got)
	}
	for _, tb := range []string{"t", "log", "ft", "wr"} {
		var n int
		if err := s.QueryRow(`SELECT count(*) FROM ` + tb).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows, %v", tb, n, err)
		}
	}
	exec(t, s, `INSERT INTO t(b) VALUES ('z')`)
	var w string
	if err := s.QueryRow(`SELECT what FROM log`).Scan(&w); err != nil || w != "z" {
		t.Errorf("trigger in the schema-only copy: %q %v", w, err)
	}
}

// TestPruneAndInPlace: pruning keeps every point after the cutoff restorable;
// an in-place restore continues the same history.
func TestPruneAndInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.musq")
	db := openDB(t, path)
	exec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`)
	h, err := Open(path, filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var points []point
	for i := range 6 {
		exec(t, db, `INSERT INTO t(b) VALUES (?)`, i)
		if i == 2 {
			if _, err := h.Snapshot(true); err != nil {
				t.Fatal(err)
			}
		}
		points = append(points, point{time.Now(), dump(t, path)})
		time.Sleep(time.Millisecond)
	}
	if err := h.Prune(points[3].at); err != nil {
		t.Fatal(err)
	}
	if got := h.Oldest(); got.After(points[2].at) {
		t.Errorf("pruned past the base a restore at the cutoff needs")
	}
	if err := h.Restore(points[0].at, filepath.Join(dir, "old.musq")); !errors.Is(err, ErrBeforeHistory) {
		t.Errorf("a pruned point restored: %v", err)
	}
	for i := 2; i < len(points); i++ {
		dst := filepath.Join(dir, fmt.Sprintf("p%d.musq", i))
		if err := h.Restore(points[i].at, dst); err != nil {
			t.Fatal(err)
		}
		if got := dump(t, dst); got != points[i].dump {
			t.Errorf("after prune, point %d: want:\n%s\ngot:\n%s", i, points[i].dump, got)
		}
	}
	db.Close()
	// In place, as the server does it: restore beside, retarget, then the
	// new file is the database.
	inplace := filepath.Join(dir, "db2.musq")
	if err := h.Restore(points[3].at, inplace); err != nil {
		t.Fatal(err)
	}
	if err := h.Retarget(inplace); err != nil {
		t.Fatal(err)
	}
	engine.RemoveDatabaseFiles(path)
	db = openDB(t, inplace)
	defer db.Close()
	exec(t, db, `INSERT INTO t(b) VALUES ('new')`)
	newest := point{time.Now(), dump(t, inplace)}
	for _, p := range []point{points[4], newest} {
		dst := filepath.Join(dir, fmt.Sprintf("q%d.musq", p.at.UnixNano()))
		if err := h.Restore(p.at, dst); err != nil {
			t.Fatal(err)
		}
		if got := dump(t, dst); got != p.dump {
			t.Errorf("after an in-place restore: want:\n%s\ngot:\n%s", p.dump, got)
		}
	}
}
