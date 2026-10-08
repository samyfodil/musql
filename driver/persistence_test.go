package driver

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDataPersistsAcrossProcesses: musql is a file database, not an in-memory
// store. A child process writes and commits rows of every storage class and
// exits WITHOUT closing the database -- so nothing can be flushed at shutdown --
// a second child changes them and rewrites the file with VACUUM, and this
// process, which never held the data, opens the file and reads it all back.
func TestDataPersistsAcrossProcesses(t *testing.T) {
	if mode := os.Getenv("MUSQL_PERSIST_CHILD"); mode != "" {
		persistChild(t, mode, os.Getenv("MUSQL_PERSIST_PATH"))
		return
	}
	path := filepath.Join(t.TempDir(), "persist.musq")
	for _, mode := range []string{"write", "change"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDataPersistsAcrossProcesses$", "-test.count=1")
		cmd.Env = append(os.Environ(), "MUSQL_PERSIST_CHILD="+mode, "MUSQL_PERSIST_PATH="+path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child %s: %v\n%s", mode, err, out)
		}
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		t.Fatalf("the database file is not on disk: %v", err)
	}
	db, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, i, r, s, b, n FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int64
		var i, r, s, b, n any
		if err := rows.Scan(&id, &i, &r, &s, &b, &n); err != nil {
			t.Fatal(err)
		}
		if bs, ok := s.([]byte); ok {
			s = string(bs)
		}
		got = append(got, fmt.Sprintf("%d|%v|%v|%v|%x|%v", id, i, r, s, b, n))
	}
	want := []string{
		"1|42|1.5|hello|0001ff|<nil>",
		"2|-9223372036854775808|-0.25|wörld 🙂|deadbeef|<nil>",
		"4|7|2.5|changed|00|<nil>",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("read back from a fresh process:\n got  %v\n want %v", got, want)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM big`).Scan(&count); err != nil || count != 50000 {
		t.Fatalf("big table: %d rows, %v; want 50000", count, err)
	}
}

// persistChild is one writer process. It exits with os.Exit and never closes
// the database, so what the parent reads is what the commits made durable.
func persistChild(t *testing.T, mode, path string) {
	db, err := sql.Open(DriverName, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var stmts []string
	switch mode {
	case "write":
		stmts = []string{
			`CREATE TABLE t (id INTEGER PRIMARY KEY, i INTEGER, r REAL, s TEXT, b BLOB, n)`,
			`INSERT INTO t VALUES (1, 42, 1.5, 'hello', x'0001ff', NULL)`,
			`INSERT INTO t VALUES (2, -9223372036854775808, -0.25, 'wörld 🙂', x'deadbeef', NULL)`,
			`INSERT INTO t VALUES (3, 0, 0.0, 'gone', x'', NULL)`,
			`CREATE TABLE big (id INTEGER PRIMARY KEY, v INTEGER)`,
			`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 50000) INSERT INTO big SELECT x, x * 3 FROM c`,
		}
	case "change":
		stmts = []string{
			`INSERT INTO t VALUES (4, 7, 2.5, 'changed', x'00', NULL)`,
			`DELETE FROM t WHERE id = 3`,
			`VACUUM`,
		}
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", q, err)
			os.Exit(1)
		}
	}
	os.Exit(0) // deliberately no db.Close()
}
