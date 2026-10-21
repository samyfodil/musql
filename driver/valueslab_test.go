package driver

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"
)

// Values scanned into any are kept across rows and later queries, so the
// chunks they live in must never be written again.
func TestValueSlabValuesStayValid(t *testing.T) {
	db, err := sql.Open("musql", filepath.Join(t.TempDir(), "s.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE t(i INTEGER, r REAL, s TEXT, b BLOB, n)`); err != nil {
		t.Fatal(err)
	}
	const n = 40
	for k := range n {
		if _, err := db.Exec(`INSERT INTO t VALUES(?, ?, ?, ?, NULL)`, int64(k)*1e9, float64(k)+0.5, fmt.Sprintf("text-%d", k), []byte{byte(k), 0}); err != nil {
			t.Fatal(err)
		}
	}
	var kept [][]any
	for range 3 {
		rows, err := db.Query(`SELECT i, r, s, b, n, '' FROM t ORDER BY i`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			row := make([]any, 6)
			ptrs := make([]any, len(row))
			for i := range row {
				ptrs[i] = &row[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			kept = append(kept, row)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if len(kept) != 3*n {
		t.Fatalf("got %d rows, want %d", len(kept), 3*n)
	}
	for j, row := range kept {
		k := j % n
		if row[0] != int64(k)*1e9 || row[1] != float64(k)+0.5 || row[2] != fmt.Sprintf("text-%d", k) ||
			fmt.Sprint(row[3]) != fmt.Sprint([]byte{byte(k), 0}) || row[4] != nil || row[5] != "" {
			t.Fatalf("row %d: got %#v", j, row)
		}
	}
	var f any
	if err := db.QueryRow(`SELECT ?`, math.Inf(-1)).Scan(&f); err != nil || f != math.Inf(-1) {
		t.Fatalf("got %v, %v", f, err)
	}
}
