package driver_test

import (
	"bytes"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestWorkloadWritesOnlyThisFormat: a workload through database/sql -- tables,
// indexes, TEMP tables and triggers, ATTACH, transactions, savepoints,
// rollbacks, reads -- writes this engine's own format and nothing else, its
// TEMP database included.
func TestWorkloadWritesOnlyThisFormat(t *testing.T) {
	dir := t.TempDir()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // anything the engine puts in os.TempDir() lands here
	t.Setenv("TMP", tmp)    // os.TempDir on Windows
	t.Setenv("TEMP", tmp)

	db, err := sql.Open("sqlite", filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // one connection, so TEMP objects persist across statements
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b TEXT)")
	exec("CREATE INDEX t_a ON t(a)")
	for i := 1; i <= 200; i++ {
		exec("INSERT INTO t(a, b) VALUES(?, ?)", i%17, "row")
	}
	exec("CREATE TEMP TABLE tt(x INTEGER, y TEXT)")
	exec("CREATE TEMP TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO tt VALUES(new.a, new.b); END")
	exec("INSERT INTO t(a, b) VALUES(99, 'fired')")
	exec("INSERT INTO tt SELECT a, b FROM t WHERE a < 3")
	exec("BEGIN")
	exec("SAVEPOINT s1")
	exec("INSERT INTO tt VALUES(-1, 'rolled back')")
	exec("ROLLBACK TO s1")
	exec("COMMIT")
	exec("ATTACH '" + filepath.Join(dir, "aux.musq") + "' AS aux")
	exec("CREATE TABLE aux.u(k INTEGER PRIMARY KEY, v INTEGER)")
	exec("INSERT INTO aux.u SELECT id, a FROM t WHERE id <= 10")

	// TEMP is a FILE by default, as C's is (SQLITE_TEMP_STORE=1), and on this
	// engine's own format.
	if got := tempFilesIn(t, tmp); len(got) != 1 {
		t.Fatalf("TEMP should be one file in the temp directory, found %v", got)
	}
	var n, m, k int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM tt").Scan(&m); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM aux.u JOIN t ON t.id = aux.u.k JOIN tt ON tt.x = t.a").Scan(&k); err != nil {
		t.Fatal(err)
	}
	if n != 201 || m == 0 || k == 0 {
		t.Fatalf("workload answered t=%d tt=%d join=%d", n, m, k)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// ...deleted with the connection, as C opens it DELETEONCLOSE.
	if got := tempFilesIn(t, tmp); len(got) != 0 {
		t.Errorf("the TEMP file outlived its connection: %v", got)
	}
	for _, root := range []string{dir, tmp} {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, rerr := os.ReadFile(path)
			if rerr == nil && len(b) > 0 && !bytes.HasPrefix(b, []byte("MQSF")) && !bytes.HasPrefix(b, []byte("MQSD")) {
				t.Errorf("a file of another format on disk: %s (% x)", path, b[:min(16, len(b))])
			}
			return nil
		})
	}
}

// tempFilesIn lists dir's TEMP database files, failing on any that is not the
// engine's own format.
func tempFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	matches, _ := filepath.Glob(filepath.Join(dir, "musql-temp-*.musq"))
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if !bytes.HasPrefix(b, []byte("MQSF")) && len(b) > 0 {
			t.Fatalf("%s is not a segment file: % x", p, b[:min(16, len(b))])
		}
		out = append(out, p)
	}
	return out
}

// TestTempStoreMemoryWritesNoFile: "PRAGMA temp_store=MEMORY" keeps the TEMP
// database in memory, C's sqlite3TempInMemory.
func TestTempStoreMemoryWritesNoFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp) // os.TempDir on Windows
	t.Setenv("TEMP", tmp)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		"PRAGMA temp_store = MEMORY",
		"CREATE TEMP TABLE tt(x)",
		"INSERT INTO tt VALUES(1), (2)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM tt").Scan(&n); err != nil || n != 2 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if got := tempFilesIn(t, tmp); len(got) != 0 {
		t.Fatalf("temp_store=MEMORY wrote %v", got)
	}
}
