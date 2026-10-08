package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestBulkLoadAndVerbatimVacuumKeepEveryRow: a commit large enough that its
// compaction is due rewrites the file directly (compactionDueAfter), and a
// VACUUM right after copies the untouched tables' segments verbatim
// (pristineSegments). Neither may change a row: every answer is checked after
// a reopen, and again after a delta write makes the table no longer pristine.
func TestBulkLoadAndVerbatimVacuumKeepEveryRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const rows = 60000
	for _, q := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`,
		`CREATE TABLE u(x, y)`, // no INTEGER PRIMARY KEY: VACUUM renumbers it
		`CREATE INDEX t_k ON t(k)`,
		fmt.Sprintf(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < %d) INSERT INTO t SELECT i, i %% 97, 'r' || i FROM c`, rows),
		`INSERT INTO u VALUES (1, 'a'), (2, 'b'), (3, 'c')`,
		`DELETE FROM u WHERE x = 2`,
	} {
		if err := n.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if _, err := n.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	n.Close()
	check := func(stage string, extra int) {
		t.Helper()
		got, err := queryDB(t, path, `SELECT count(*), sum(k), max(length(s)), (SELECT count(*) FROM t WHERE k = 5), (SELECT group_concat(rowid || ':' || x) FROM u) FROM t`)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		var sumK int64
		k5 := 0
		for i := 1; i <= rows+extra; i++ {
			sumK += int64(i % 97)
			if i%97 == 5 {
				k5++
			}
		}
		wantU := "1:1,3:3"
		if stage != "before vacuum" {
			wantU = "1:1,2:3" // VACUUM renumbers a table with no INTEGER PRIMARY KEY
		}
		want := fmt.Sprintf("1:%d:0:\"\"|1:%d:0:\"\"|1:%d:0:\"\"|1:%d:0:\"\"|3:0:0:%q|\n",
			rows+extra, sumK, len(fmt.Sprint("r", rows+extra)), k5, wantU)
		if got != want {
			t.Fatalf("%s:\n got  %s want %s", stage, got, want)
		}
		ic, err := queryDB(t, path, `PRAGMA integrity_check`)
		if err != nil || ic != "3:0:0:\"ok\"|\n" {
			t.Fatalf("%s: integrity_check %q %v", stage, ic, err)
		}
	}
	check("before vacuum", 0)
	execDB(t, path, `VACUUM`)
	check("after vacuum", 0)
	execDB(t, path, fmt.Sprintf(`INSERT INTO t VALUES (%d, %d, 'r%d')`, rows+1, (rows+1)%97, rows+1))
	execDB(t, path, `VACUUM`)
	check("after a delta row and another vacuum", 1)
}

// TestFailedBulkInsertUndoesEveryRow: a statement's undo journal folds an
// INSERT of consecutive rowids into one entry (undoDrop's dropMore); a
// statement that fails part way must still take every one of them back.
func TestFailedBulkInsertUndoesEveryRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "u.musq")
	buildDB(t, path, `CREATE TABLE t(id INTEGER PRIMARY KEY, v NOT NULL)`, `INSERT INTO t VALUES (1, 'kept')`)
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	err = n.Exec(`WITH RECURSIVE c(i) AS (SELECT 2 UNION ALL SELECT i+1 FROM c WHERE i < 2000) INSERT INTO t SELECT i, CASE WHEN i = 500 THEN NULL ELSE i END FROM c`)
	if err == nil {
		t.Fatal("the NOT NULL violation at row 500 was accepted")
	}
	_, rows, qerr := n.Query(`SELECT count(*), max(id) FROM t`, nil)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if got := fmt.Sprint(rows[0][0].I, rows[0][1].I); got != "1 1" {
		t.Fatalf("after the failed INSERT: count, max(id) = %s, want 1 1", got)
	}
}
