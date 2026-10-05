package driver

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestVacuumOnSegmentFormat verifies that VACUUM and VACUUM INTO work correctly,
// reorganizing the database and preserving rows.
func TestVacuumOnSegmentFormat(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open(DriverName, filepath.Join(dir, "v.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, x TEXT)`,
		`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three')`,
		`DELETE FROM t WHERE id=2`,
		`VACUUM`,
		`VACUUM main`,
	} {
		if _, eerr := db.Exec(s); eerr != nil {
			t.Fatalf("%s: %v", s, eerr)
		}
	}
	var n int
	if qerr := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); qerr != nil {
		t.Fatalf("after VACUUM: %v", qerr)
	}
	if n != 2 {
		t.Errorf("after VACUUM the table holds %d rows, want 2", n)
	}

	copyPath := filepath.Join(dir, "copy.musq")
	if _, eerr := db.Exec(`VACUUM INTO '` + copyPath + `'`); eerr != nil {
		t.Fatalf("VACUUM INTO: %v", eerr)
	}
	st, serr := os.Stat(copyPath)
	if serr != nil {
		t.Fatalf("the copy was not written: %v", serr)
	}
	if st.Size() == 0 {
		t.Fatal("the copy is empty")
	}
	// Verify the copy is a valid database with the current data.
	cp, cerr := sql.Open(DriverName, copyPath)
	if cerr != nil {
		t.Fatal(cerr)
	}
	defer cp.Close()
	got := ""
	rows, qerr := cp.Query(`SELECT id FROM t ORDER BY id`)
	if qerr != nil {
		t.Fatalf("reading the copy: %v", qerr)
	}
	for rows.Next() {
		var id int
		if serr := rows.Scan(&id); serr != nil {
			t.Fatal(serr)
		}
		got += string(rune('0' + id))
	}
	rows.Close()
	if got != "13" {
		t.Errorf("the copy holds ids %q, want \"13\"", got)
	}

	// VACUUM keyword must match exactly, not just as a prefix.
	if _, eerr := db.Exec(`CREATE TABLE vacuumed(x)`); eerr != nil {
		t.Errorf("CREATE TABLE vacuumed: %v", eerr)
	}
}
