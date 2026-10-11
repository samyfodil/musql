package driver

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// The format fixture is a database written by an earlier build and committed,
// with what every query below answered on it. Each build must open it and
// answer the same: a file that was readable once stays readable. A change that
// breaks this needs a new format version, and the reader has to go on reading
// the old one (engine/segment_file.go, segFileMinVersion).
//
// It is a segment file and the delta log beside it, so both formats are held:
// the rows written before the last compaction are in the first, the rest in
// the second. MUSQL_WRITE_FORMAT_FIXTURE=1 writes a new fixture -- only when a
// format version is added, into a directory of its own, leaving the old one.

// formatFixtureDir is the fixture the current format writes.
const formatFixtureDir = "testdata/format/v4"

// formatFixtureSetup builds the fixture: every storage class and NULL, a
// rowid alias and a WITHOUT ROWID table, indexes plain, expression and
// partial, a view, a trigger, AUTOINCREMENT, vectors and a vector index, and
// fts4, fts5 and rtree tables. The statements after the VACUUM are what the
// delta log holds.
var formatFixtureSetup = []string{
	"CREATE TABLE kinds(id INTEGER PRIMARY KEY, i INT, r REAL, t TEXT, b BLOB, n NUMERIC, any)",
	"INSERT INTO kinds VALUES (1, 42, 1.5, 'text', x'00ff10', 12.0, NULL)",
	"INSERT INTO kinds VALUES (2, -9223372036854775808, -0.0, '', x'', '7', 3.25)",
	"INSERT INTO kinds VALUES (3, NULL, NULL, NULL, NULL, NULL, 'mixed')",
	"INSERT INTO kinds VALUES (4, 9223372036854775807, 1e308, 'é ünïcode ✓', zeroblob(4), 1e3, x'01')",
	"CREATE INDEX kinds_t ON kinds(t COLLATE NOCASE DESC)",
	"CREATE INDEX kinds_expr ON kinds(i + 1) WHERE i IS NOT NULL",
	"CREATE TABLE wr(a TEXT, b INT, c, PRIMARY KEY (a, b)) WITHOUT ROWID",
	"INSERT INTO wr VALUES ('x', 1, 'one'), ('x', 2, 'two'), ('y', 1, NULL)",
	"CREATE TABLE seq(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT UNIQUE)",
	"INSERT INTO seq(v) VALUES ('a'), ('b'), ('c')",
	"DELETE FROM seq WHERE v = 'c'",
	"CREATE TABLE log(msg TEXT)",
	"CREATE TRIGGER seq_log AFTER INSERT ON seq BEGIN INSERT INTO log VALUES ('added ' || new.v); END",
	"CREATE VIEW kinds_v AS SELECT id, typeof(i) AS ti, length(b) AS lb FROM kinds",
	"CREATE TABLE emb(id INTEGER PRIMARY KEY, e F32_BLOB(4))",
	"INSERT INTO emb VALUES (1, vector('[1,0,0,0]')), (2, vector('[0,1,0,0]')), (3, vector('[1,1,0,0]')), (4, NULL)",
	"CREATE INDEX emb_i ON emb(libsql_vector_idx(e))",
	"CREATE VIRTUAL TABLE docs4 USING fts4(title, body)",
	"INSERT INTO docs4 VALUES ('first', 'the quick brown fox'), ('second', 'lazy dogs sleep')",
	"CREATE VIRTUAL TABLE docs5 USING fts5(title, body)",
	"INSERT INTO docs5 VALUES ('first', 'the quick brown fox'), ('second', 'lazy dogs sleep')",
	"CREATE VIRTUAL TABLE boxes USING rtree(id, x0, x1, y0, y1)",
	"INSERT INTO boxes VALUES (1, 0, 1, 0, 1), (2, 5, 6, 5, 6)",
	"INSERT INTO seq(v) VALUES ('d')",
	"INSERT INTO docs5 VALUES ('third', 'quick thinking')",
	"PRAGMA user_version = 7",
	"VACUUM",
	// The delta log.
	"INSERT INTO kinds VALUES (5, 5, 5.5, 'after', x'05', 5, 5)",
	"UPDATE kinds SET t = 'TEXT' WHERE id = 1",
	"DELETE FROM kinds WHERE id = 2",
	"INSERT INTO emb VALUES (5, vector('[0,0,1,0]'))",
	"INSERT INTO wr VALUES ('z', 9, 'nine')",
}

// formatFixtureQueries are answered on the fixture and compared.
var formatFixtureQueries = []string{
	"SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY name",
	"SELECT id, i, typeof(i), r, typeof(r), t, quote(b), n, typeof(n), quote(any) FROM kinds ORDER BY id",
	"SELECT id, t FROM kinds ORDER BY t COLLATE NOCASE DESC",
	"SELECT i + 1 FROM kinds WHERE i + 1 > 0 ORDER BY 1",
	"SELECT * FROM wr ORDER BY a, b",
	"SELECT * FROM seq ORDER BY id",
	"SELECT name, seq FROM sqlite_sequence",
	"SELECT * FROM log",
	"SELECT * FROM kinds_v ORDER BY id",
	"SELECT id, vector_extract(e) FROM emb WHERE e IS NOT NULL ORDER BY id",
	"SELECT id FROM vector_top_k('emb_i', vector('[1,0.1,0,0]'), 3)",
	"SELECT title FROM docs4 WHERE docs4 MATCH 'quick'",
	"SELECT title FROM docs5 WHERE docs5 MATCH 'quick' ORDER BY rank",
	"SELECT id FROM boxes WHERE x0 < 2 AND x1 > 0.5",
	"PRAGMA user_version",
	"PRAGMA integrity_check",
}

// formatAnswer renders one query's rows, each value with its type, or its
// error.
func formatAnswer(db *sql.DB, q string) string {
	rows, err := db.Query(q)
	if err != nil {
		return "error: " + err.Error() + "\n"
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
			return "error: " + err.Error() + "\n"
		}
		for i, v := range vals {
			if i > 0 {
				b.WriteString(" | ")
			}
			fmt.Fprintf(&b, "%T(%v)", v, v)
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error() + "\n"
	}
	return b.String()
}

func formatAnswers(db *sql.DB) string {
	var b strings.Builder
	for _, q := range formatFixtureQueries {
		fmt.Fprintf(&b, "%s\n%s\n", q, formatAnswer(db, q))
	}
	return b.String()
}

// TestFormatFixture opens every committed fixture, checks its answers, then
// writes to it and reopens it, so an old file also takes new writes.
func TestFormatFixture(t *testing.T) {
	// fts5 is opt-in (engine.RegisterFTS5); the fixture holds a table of it.
	engine.RegisterFTS5()
	defer engine.UnregisterFTS5()
	if os.Getenv("MUSQL_WRITE_FORMAT_FIXTURE") != "" {
		writeFormatFixture(t)
	}
	dirs, err := filepath.Glob("testdata/format/v*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no format fixtures: %v", err)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(dir, "answers.txt"))
			if err != nil {
				t.Fatal(err)
			}
			// A copy: opening may write, and tests never write into the tree.
			path := filepath.Join(t.TempDir(), "fixture.musq")
			for _, suffix := range []string{"", ".delta"} {
				data, err := os.ReadFile(filepath.Join(dir, "fixture.musq"+suffix))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+suffix, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sql.Open(DriverName, path)
			if err != nil {
				t.Fatal(err)
			}
			if got := formatAnswers(db); got != string(want) {
				t.Errorf("%s answers differently from when it was written:\n%s", dir, lineDiff(string(want), got))
			}
			for _, s := range []string{"INSERT INTO kinds VALUES (6, 6, 6.0, 'new', x'06', 6, 6)", "VACUUM"} {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			db.Close()
			db, err = sql.Open(DriverName, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if got := formatAnswer(db, "SELECT count(*), max(id) FROM kinds"); got != "int64(5) | int64(6)\n" {
				t.Errorf("after writing to %s and reopening: %q", dir, got)
			}
			if got := formatAnswer(db, "PRAGMA integrity_check"); got != "string(ok)\n" {
				t.Errorf("after writing to %s and reopening: %q", dir, got)
			}
		})
	}
}

// TestFormatVersionBounds opens the fixture with its version field set past
// both ends of what this build reads: a newer file and a too-old one are
// errors that say which, never a misread.
func TestFormatVersionBounds(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(formatFixtureDir, "fixture.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		version byte
		want    string
	}{{99, "is from a newer musql"}, {3, "is older than this build reads"}} {
		patched := append([]byte(nil), data...)
		patched[4], patched[5], patched[6], patched[7] = c.version, 0, 0, 0 // the header's version, little-endian
		path := filepath.Join(t.TempDir(), "v.musq")
		if err := os.WriteFile(path, patched, 0o644); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open(DriverName, path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec("SELECT count(*) FROM kinds")
		db.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("format version %d: %v, want an error saying it %s", c.version, err, c.want)
		}
	}
}

// writeFormatFixture writes formatFixtureDir from formatFixtureSetup.
func writeFormatFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.musq")
	db, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range formatFixtureSetup {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	answers := formatAnswers(db)
	db.Close()
	if err := os.MkdirAll(formatFixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".delta"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(formatFixtureDir, "fixture.musq"+suffix), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formatFixtureDir, "answers.txt"), []byte(answers), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lineDiff lists the lines of want and got that differ.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	for i := range max(len(w), len(g)) {
		var a, c string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			c = g[i]
		}
		if a != c {
			fmt.Fprintf(&b, "line %d\n  was: %s\n  now: %s\n", i+1, a, c)
		}
	}
	return b.String()
}
