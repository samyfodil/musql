package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorrelatedRowidSeekMatchesTheScan: "b.id = t.bid" inside a correlated
// subquery seeks b's rowid with the outer value instead of scanning b for
// every outer row. Each query is compared with the same comparison wrapped by
// noSeek: the same truth value, NULL included, and the same affinity, in a form
// no seek planner reads. ("b.id + 0" would not do: it drops the column's
// affinity and changes the answer; "OR 0" would not either, since the planners
// look through an always-false arm -- seekConjuncts.) The
// keys cover every storage class: a TEXT or REAL key falls back to the scan and
// must still find the rows its affinity matches.
func TestCorrelatedRowidSeekMatchesTheScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	stmts := []string{
		`CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT)`,
		`CREATE TABLE t (id INTEGER PRIMARY KEY, bid, n INTEGER)`,
	}
	for i := 0; i < 300; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO b VALUES (%d, 'b%d')`, i*2, i))
	}
	keys := []string{"0", "2", "3", "598", "600", "-2", "'4'", "'x'", "6.0", "6.5", "NULL", "x'08'", "9223372036854775807"}
	for i := 1; i <= 400; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %d)`, i, keys[i%len(keys)], i%7))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)

	queries := []string{
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid)`,
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE t.bid = b.id AND b.id < 300)`,
		`SELECT count(*) FROM t WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.id = t.bid)`,
		`SELECT t.id, (SELECT label FROM b WHERE b.id = t.bid) FROM t ORDER BY t.id`,
		`SELECT count(*) FROM t WHERE t.n IN (SELECT b.id FROM b WHERE b.id = t.bid)`,
		`SELECT t.id FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.rowid = t.bid) ORDER BY t.id`,
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE EXISTS (SELECT 1 FROM b AS c WHERE c.id = t.bid AND c.id = b.id))`,
		// seekConjuncts: an always-false OR arm and an always-true AND arm.
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE (b.id = t.bid OR 0) AND b.id < 400)`,
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE (1 AND (0 OR b.id = t.bid)))`,
	}
	run := func(stage string) {
		for _, q := range queries {
			got, err := queryDB(t, path, q)
			if err != nil {
				t.Fatalf("%s %s: %v", stage, q, err)
			}
			ref := strings.ReplaceAll(q, "b.id = t.bid", noSeek("b.id = t.bid"))
			ref = strings.ReplaceAll(ref, "c.id = t.bid", noSeek("c.id = t.bid"))
			ref = strings.ReplaceAll(ref, "t.bid = b.id", noSeek("t.bid = b.id"))
			ref = strings.ReplaceAll(ref, "b.rowid = t.bid", noSeek("b.rowid = t.bid"))
			if ref == q {
				t.Fatalf("no unseekable reference for %s", q)
			}
			want, err := queryDB(t, path, ref)
			if err != nil {
				t.Fatalf("%s %s: %v", stage, ref, err)
			}
			if got != want {
				t.Errorf("%s %s:\n seek %s\n scan %s", stage, q, got, want)
			}
		}
	}
	run("segments")
	execDB(t, path, `INSERT INTO b VALUES (3, 'three')`, `DELETE FROM b WHERE id = 2`, `UPDATE t SET bid = 3 WHERE id = 1`)
	run("with log")
}

// noSeek wraps a comparison so it keeps its truth value -- NULL included,
// since (NULL AND 0) is 0 -- but offers no seek: the extra OR arm is not a
// literal, so seekConjuncts leaves it in place.
func noSeek(cmp string) string { return "(" + cmp + " OR (b.id IS NULL AND 0))" }
