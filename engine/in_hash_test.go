package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestInHashSetMatchesTheLoop: an uncorrelated single-column IN probes a hash
// set built from the subquery's rows (in_hash.go). It must answer exactly as
// inSubMembership's loop does -- the same SQL with the set turned off -- over
// every storage class, each column affinity, NULLs on either side, NOT IN,
// integral and non-integral REALs, integers past 2^53, and blobs.
func TestInHashSetMatchesTheLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	vals := []string{"1", "'1'", "1.0", "1.5", "'1.5'", "'abc'", "x'616263'", "NULL", "-0.0", "0",
		"9007199254740993", "9007199254740992.0", "9223372036854775807", "9.3e18", "'  2'", "2", "'x'"}
	stmts := []string{
		`CREATE TABLE s (id INTEGER PRIMARY KEY, i INTEGER, r REAL, n NUMERIC, txt TEXT, b BLOB, any_)`,
		`CREATE TABLE p (id INTEGER PRIMARY KEY, i INTEGER, r REAL, n NUMERIC, txt TEXT, b BLOB, any_)`,
	}
	for i, v := range vals {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO s VALUES (%d, %s, %s, %s, %s, %s, %s)`, i+1, v, v, v, v, v, v))
	}
	for i := range vals {
		v := vals[(i*7)%len(vals)]
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO p VALUES (%d, %s, %s, %s, %s, %s, %s)`, i+1, v, v, v, v, v, v))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	cols := []string{"i", "r", "n", "txt", "b", "any_"}
	var queries []string
	for _, pc := range cols {
		for _, sc := range cols {
			queries = append(queries,
				fmt.Sprintf(`SELECT id, %s IN (SELECT %s FROM s), %s NOT IN (SELECT %s FROM s) FROM p ORDER BY id`, pc, sc, pc, sc),
				fmt.Sprintf(`SELECT id, %s IN (SELECT %s FROM s WHERE %s IS NOT NULL) FROM p ORDER BY id`, pc, sc, sc))
		}
		queries = append(queries, fmt.Sprintf(`SELECT id, %s IN (SELECT i FROM s WHERE 0) FROM p ORDER BY id`, pc)) // empty set
	}
	for _, q := range queries {
		got, err := queryDB(t, path, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		inHashOffForTest = true
		want, err := queryDB(t, path, q)
		inHashOffForTest = false
		if err != nil {
			t.Fatalf("%s loop: %v", q, err)
		}
		if got != want {
			t.Errorf("%s:\n set  %.400q\n loop %.400q", q, got, want)
		}
	}
}
