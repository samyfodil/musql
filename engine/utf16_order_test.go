package engine_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestUTF16TextOrder: in a UTF-16 database, TEXT compares as memcmp over its
// encoded units, so 😀 and 😁, which share their high surrogate, differ, and a
// hash GROUP BY emits its groups in that order (compat-harness
// TestUTF16SurrogatePairsCompare checks the same against C SQLite).
func TestUTF16TextOrder(t *testing.T) {
	for _, c := range []struct{ enc, want string }{
		{"UTF-16le", "😀:2 😀a:1 😁:1 z:1 é:1"},
		{"UTF-16be", "z:1 é:1 😀:2 😀a:1 😁:1"},
	} {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "u.musq"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range []string{"PRAGMA encoding='" + c.enc + "'", "CREATE TABLE e(x)", "INSERT INTO e VALUES('😀'),('😁'),('😀'),('😀a'),('z'),('é')"} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		var eq int
		if err := db.QueryRow("SELECT '😀' = '😁'").Scan(&eq); err != nil || eq != 0 {
			t.Errorf("%s: '😀' = '😁' is %d, %v", c.enc, eq, err)
		}
		rows, err := db.Query("SELECT x || ':' || count(*) FROM e GROUP BY x ORDER BY x")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		rows.Close()
		db.Close()
		if s := strings.Join(got, " "); s != c.want {
			t.Errorf("%s: %s, want %s", c.enc, s, c.want)
		}
	}
}
