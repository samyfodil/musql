package engine

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// segFileCounter is the change counter the segment file at path was written
// at, which only a rewrite moves.
func segFileCounter(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := OpenSegmentFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return f.srcChangeCounter
}

// segAnswer is sql's rows as a fresh read-only open of path answers them.
func segAnswer(t *testing.T, path, sql string) string {
	t.Helper()
	p, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(sql, nil)
	if err != nil {
		return "error: " + err.Error()
	}
	return rowsText(rows)
}

// rowsText is rows as text, a row per line and its values space-separated.
func rowsText(rows [][]Value) string {
	var b strings.Builder
	for _, r := range rows {
		for i, v := range r {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(valueToText(v))
		}
		b.WriteByte(';')
	}
	return b.String()
}

func execCommitEach(t *testing.T, s *Session, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if err := s.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if _, err := s.Commit(); err != nil {
			t.Fatalf("commit %s: %v", q, err)
		}
	}
}

// TestCatalogRecordKeepsTheFile: a view, a trigger or a header word is a
// catalog record in the delta, not a rewrite of the file, and every later open
// reads the catalog it brings -- until a compaction folds it into the file.
func TestCatalogRecordKeepsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.musq")
	s, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	execCommitEach(t, s,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, x INTEGER)`,
		`CREATE TABLE log(id INTEGER PRIMARY KEY, what TEXT)`,
		`INSERT INTO t VALUES(1, 10), (2, 20), (3, 30)`)
	base := segFileCounter(t, path)
	steps := []struct {
		stmt, query, want string
	}{
		{`CREATE VIEW v AS SELECT x * 2 AS y FROM t`, `SELECT y FROM v ORDER BY y`, "20;40;60;"},
		{`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log(what) VALUES('ins ' || new.x); END`, `SELECT name FROM sqlite_master WHERE type = 'trigger'`, ""},
		{`PRAGMA user_version = 7`, `PRAGMA user_version`, "7;"},
		{`DROP VIEW v`, `SELECT count(*) FROM sqlite_master WHERE name = 'v'`, "0;"},
	}
	for _, st := range steps {
		execCommitEach(t, s, st.stmt)
		if got := segFileCounter(t, path); got != base {
			t.Fatalf("%s rewrote the file (counter %d, was %d)", st.stmt, got, base)
		}
		if st.want != "" {
			if got := segAnswer(t, path, st.query); got != st.want {
				t.Fatalf("after %s: %s answered %s, want %s", st.stmt, st.query, got, st.want)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The trigger the delta carries fires in a session opened afterwards.
	s2, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	execCommitEach(t, s2, `INSERT INTO t VALUES(4, 40)`)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	want := "ins 40;"
	if got := segAnswer(t, path, `SELECT what FROM log`); got != want {
		t.Fatalf("the trigger answered %s, want %s", got, want)
	}
	// A compaction folds the catalog into the file.
	if _, err := CompactSegmentFile(path); err != nil {
		t.Fatal(err)
	}
	if got := segFileCounter(t, path); got == base {
		t.Fatalf("the compaction did not rewrite the file")
	}
	for q, want := range map[string]string{
		`PRAGMA user_version`: "7;",
		`SELECT name FROM sqlite_master WHERE type = 'trigger'`: "tr;",
		`SELECT count(*) FROM sqlite_master WHERE name = 'v'`:   "0;",
	} {
		if got := segAnswer(t, path, q); got != want {
			t.Fatalf("after compaction %s answered %s, want %s", q, got, want)
		}
	}
}

// TestCatalogRecordKeepsTheOrder: sqlite_master lists objects in creation
// order, which a DROP renumbers; the catalog record carries every table's
// place in it too.
func TestCatalogRecordKeepsTheOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.musq")
	s, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	execCommitEach(t, s,
		`CREATE VIEW a AS SELECT 1`,
		`CREATE TABLE b(x)`,
		`CREATE VIEW c AS SELECT 2`,
		`DROP VIEW a`,
		`CREATE VIEW d AS SELECT 3`)
	want := "b table;c view;d view;"
	if got := segAnswer(t, path, `SELECT name, type FROM sqlite_master`); got != want {
		t.Fatalf("sqlite_master answered %s, want %s", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CompactSegmentFile(path); err != nil {
		t.Fatal(err)
	}
	if got := segAnswer(t, path, `SELECT name, type FROM sqlite_master`); got != want {
		t.Fatalf("after compaction sqlite_master answered %s, want %s", got, want)
	}
}

// TestCatalogRecordReachesAnotherSession: a session that loaded the file
// before another one's CREATE VIEW sees the view at its next statement.
func TestCatalogRecordReachesAnotherSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	a, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	execCommitEach(t, a, `CREATE TABLE t(x)`, `INSERT INTO t VALUES(5)`)
	b, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	execCommitEach(t, a, `CREATE VIEW v AS SELECT x + 1 AS y FROM t`)
	if _, err := b.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	_, rows, err := b.Query(`SELECT y FROM v`, nil)
	if err != nil || rowsText(rows) != "6;" {
		t.Fatalf("the other session answered %v, %v", rows, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCatalogRecordNotForTablesOrIndexes: a change to the table directory, or
// to an index, still rewrites the file.
func TestCatalogRecordNotForTablesOrIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.musq")
	s, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	execCommitEach(t, s, `CREATE TABLE t(x)`)
	for _, stmt := range []string{`CREATE INDEX tx ON t(x)`, `CREATE TABLE u(y)`, `ALTER TABLE u ADD COLUMN z`, `DROP INDEX tx`} {
		before := segFileCounter(t, path)
		execCommitEach(t, s, stmt)
		if segFileCounter(t, path) == before {
			t.Fatalf("%s did not rewrite the file", stmt)
		}
	}
}

// TestCatalogRecordInAnOlderDelta: a delta older than version 7 holds no
// catalog record, so the commit rewrites the file instead.
func TestCatalogRecordInAnOlderDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.musq")
	s, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	execCommitEach(t, s, `CREATE TABLE t(x)`, `INSERT INTO t VALUES(1)`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CompactSegmentFile(path); err != nil {
		t.Fatal(err)
	}
	base := segFileCounter(t, path)
	f, err := OpenSegmentFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hdr := segDeltaHeader(f.srcChangeCounter, f.srcPageCount)
	f.Close()
	binary.LittleEndian.PutUint32(hdr[4:], 6)
	binary.LittleEndian.PutUint64(hdr[24:], segDeltaChecksum(6, hdr[:24], 0))
	if err := os.WriteFile(segDeltaPath(path), hdr, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	execCommitEach(t, s, `CREATE VIEW v AS SELECT x FROM t`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if segFileCounter(t, path) == base {
		t.Fatalf("a catalog change over a version-6 delta did not rewrite the file")
	}
	if got := segAnswer(t, path, `SELECT x FROM v`); got != "1;" {
		t.Fatalf("the view answered %s", got)
	}
}

// TestCatalogRecordTornAppend: a catalog batch cut short is not history; the
// file opens with the catalog before it.
func TestCatalogRecordTornAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.musq")
	s, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	execCommitEach(t, s, `CREATE TABLE t(x)`, `INSERT INTO t VALUES(1)`, `CREATE VIEW v1 AS SELECT x FROM t`)
	// Where the log ends, as a replay finds it: before and after the batch
	// that carries v2.
	ends := func() (int64, []byte) {
		data, err := os.ReadFile(segDeltaPath(path))
		if err != nil {
			t.Fatal(err)
		}
		f, err := OpenSegmentFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		st, err := replaySegDelta(data, f.srcChangeCounter, f.srcPageCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		return st.bytes, data
	}
	was, _ := ends()
	execCommitEach(t, s, `CREATE VIEW v2 AS SELECT x FROM t`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	end, data := ends()
	if end <= was {
		t.Fatalf("the CREATE VIEW v2 batch ends at %d, the log before it at %d", end, was)
	}
	if err := os.WriteFile(segDeltaPath(path), data[:end-3], 0o644); err != nil {
		t.Fatal(err)
	}
	want := "1;0;"
	if got := segAnswer(t, path, `SELECT count(*) FROM sqlite_master WHERE name = 'v1' UNION ALL SELECT count(*) FROM sqlite_master WHERE name = 'v2'`); got != want {
		t.Fatalf("a torn catalog batch answered %s, want %s", got, want)
	}
}

// TestCatalogRecordRefusesAStaleWriter: a session whose rows were read before
// another one's catalog record cannot commit over it -- its INSERT ran without
// the trigger, its CREATE TRIGGER would write a catalog without the other's
// DROP VIEW -- so it is busy, and the work done again on the new catalog keeps
// both.
func TestCatalogRecordRefusesAStaleWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale.musq")
	a, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	execCommitEach(t, a, `CREATE TABLE t(x)`, `CREATE TABLE log(what)`, `CREATE VIEW v AS SELECT x FROM t`)
	for _, c := range []struct{ other, stale, check, want string }{
		{`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log VALUES('fired'); END`, `INSERT INTO t VALUES(1)`,
			`SELECT what FROM log`, "fired;"},
		{`DROP VIEW v`, `CREATE TRIGGER tr2 AFTER DELETE ON t BEGIN INSERT INTO log VALUES('gone'); END`,
			`SELECT name FROM sqlite_master WHERE name IN ('v', 'tr2') ORDER BY name`, "tr2;"},
	} {
		b, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Exec(c.stale); err != nil {
			t.Fatalf("%s: %v", c.stale, err)
		}
		if _, err := a.RefreshIfStale(); err != nil {
			t.Fatal(err)
		}
		execCommitEach(t, a, c.other)
		if _, err := b.Commit(); !errors.Is(err, ErrBusy) {
			t.Fatalf("%s committed over %s: %v", c.stale, c.other, err)
		}
		if err := b.Discard(); err != nil {
			t.Fatal(err)
		}
		again, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		execCommitEach(t, again, c.stale)
		if err := again.Close(); err != nil {
			t.Fatal(err)
		}
		if got := segAnswer(t, path, c.check); got != c.want {
			t.Fatalf("after %s and %s: %s answered %s, want %s", c.other, c.stale, c.check, got, c.want)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}
