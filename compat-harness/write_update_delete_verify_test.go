// This file tests UPDATE and DELETE statements, verifying that the mutated
// database is accepted by C SQLite and read correctly by the pure-Go reader.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// requireIntegrityOK asserts PRAGMA integrity_check reports exactly "ok"
// (and only that one row) against sqlDB's currently-open database.
func requireIntegrityOK(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	rows, err := sqlDB.Query("PRAGMA integrity_check")
	if err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		all = append(all, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0] != "ok" {
		t.Fatalf("PRAGMA integrity_check returned %v, want exactly [\"ok\"]", all)
	}
}

// fetchRowidRows reads tableName through C SQLite, ordered by rowid.
func fetchRowidRows(t *testing.T, sqlDB *sql.DB, tableName, colList string) []wvRow {
	t.Helper()
	q := fmt.Sprintf("SELECT rowid,%s FROM %s ORDER BY rowid", colList, tableName)
	rows, err := sqlDB.Query(q)
	if err != nil {
		t.Fatalf("query %s: %v", tableName, err)
	}
	defer rows.Close()

	nCol := 1
	for _, c := range colList {
		if c == ',' {
			nCol++
		}
	}
	var got []wvRow
	for rows.Next() {
		dest := make([]any, nCol+1)
		ptrs := make([]any, nCol+1)
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", tableName, err)
		}
		rowid, ok := dest[0].(int64)
		if !ok {
			t.Fatalf("%s: rowid column has type %T", tableName, dest[0])
		}
		got = append(got, wvRow{rowid: rowid, cols: dest[1:]})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// requireRowsMatch asserts got exactly matches want, in order.
func requireRowsMatch(t *testing.T, tableName string, got, want []wvRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d rows, want %d: got=%+v want=%+v", tableName, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i].rowid != want[i].rowid {
			t.Errorf("%s: row %d: rowid = %d, want %d", tableName, i, got[i].rowid, want[i].rowid)
		}
		for c := range got[i].cols {
			if !wvValueEqual(got[i].cols[c], want[i].cols[c]) {
				t.Errorf("%s: row %d (rowid %d) col %d: got %#v, want %#v",
					tableName, i, got[i].rowid, c, got[i].cols[c], want[i].cols[c])
			}
		}
	}
}

// requireEngineReaderMatches verifies the pure-Go reader matches expected rows.
func requireEngineReaderMatches(t *testing.T, path, tableName string, ipkIndex int, want []wvRow) {
	t.Helper()
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer p.Close()

	rowids, rows, err := p.Rows(tableName)
	if err != nil {
		t.Fatalf("engine Rows(%s): %v", tableName, err)
	}
	if len(rowids) != len(want) {
		t.Fatalf("engine reader: %s: got %d rows, want %d", tableName, len(rowids), len(want))
	}
	for i := range rowids {
		if rowids[i] != uint64(want[i].rowid) {
			t.Errorf("engine reader: %s: row %d: rowid = %d, want %d", tableName, i, rowids[i], want[i].rowid)
		}
		for c, gv := range rows[i] {
			if c == ipkIndex && gv.Typ == engine.Null {
				gv = engine.Value{Typ: engine.Int, I: int64(rowids[i])}
			}
			if !engineValueEqual(gv, want[i].cols[c]) {
				t.Errorf("engine reader: %s: row %d (rowid %d) col %d: got %+v, want %#v",
					tableName, i, rowids[i], c, gv, want[i].cols[c])
			}
		}
	}
}

// engineValueEqual compares a decoded engine.Value against an expected value.
func engineValueEqual(got engine.Value, want any) bool {
	switch w := want.(type) {
	case nil:
		return got.Typ == engine.Null
	case int64:
		return got.Typ == engine.Int && got.I == w
	case float64:
		return got.Typ == engine.Float && got.F == w
	case string:
		return (got.Typ == engine.Text || got.Typ == engine.Blob) && string(got.S) == w
	case []byte:
		return got.Typ == engine.Blob && string(got.S) == string(w)
	default:
		return false
	}
}

func openCSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB
}

// verifyMutatedDB checks that C SQLite and the pure-Go reader agree on the mutated database.
func verifyMutatedDB(t *testing.T, path, tableName, colList string, ipkIndex int, want []wvRow) {
	t.Helper()
	sqlDB := openCSQLite(t, path)
	requireIntegrityOK(t, sqlDB)
	got := fetchRowidRows(t, sqlDB, tableName, colList)
	requireRowsMatch(t, tableName, got, want)
	requireEngineReaderMatches(t, path, tableName, ipkIndex, want)
}

// ---- DELETE scenarios ----

// buildSimpleIntTable creates a simple table with n rows via the pure-Go writer.
func buildSimpleIntTable(t *testing.T, pageSize, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("simple_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	wvExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= n; i++ {
		wvExec(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(%d,%s)", i, wvString(fmt.Sprintf("row-%d", i))))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("engine Close: %v", err)
	}
	return path
}

func TestWriteDeleteScenariosAcceptedByCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			t.Run("partial", func(t *testing.T) {
				path := buildSimpleIntTable(t, pageSize, 10)
				db, err := engine.OpenWrite(path)
				if err != nil {
					t.Fatalf("OpenWrite: %v", err)
				}
				if _, err := db.Delete("DELETE FROM t WHERE id > 5"); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				if err := db.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				var want []wvRow
				for i := int64(1); i <= 5; i++ {
					want = append(want, wvRow{rowid: i, cols: []any{i, fmt.Sprintf("row-%d", i)}})
				}
				verifyMutatedDB(t, path, "t", "id,v", 0, want)
			})

			t.Run("all", func(t *testing.T) {
				path := buildSimpleIntTable(t, pageSize, 10)
				db, err := engine.OpenWrite(path)
				if err != nil {
					t.Fatalf("OpenWrite: %v", err)
				}
				if _, err := db.Delete("DELETE FROM t"); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				if err := db.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				verifyMutatedDB(t, path, "t", "id,v", 0, nil)
			})

			t.Run("none", func(t *testing.T) {
				path := buildSimpleIntTable(t, pageSize, 10)
				db, err := engine.OpenWrite(path)
				if err != nil {
					t.Fatalf("OpenWrite: %v", err)
				}
				if _, err := db.Delete("DELETE FROM t WHERE 1=0"); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				if err := db.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				var want []wvRow
				for i := int64(1); i <= 10; i++ {
					want = append(want, wvRow{rowid: i, cols: []any{i, fmt.Sprintf("row-%d", i)}})
				}
				verifyMutatedDB(t, path, "t", "id,v", 0, want)
			})
		})
	}
}

// TestWriteDeleteLargeMultiPageForcesEmptyPages tests deletion from a large multi-page table.
func TestWriteDeleteLargeMultiPageForcesEmptyPages(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			const n = 3000
			path := buildSimpleIntTable(t, pageSize, n)

			db, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			deleted, err := db.Delete("DELETE FROM t WHERE id % 10 != 0") // keep only every 10th row
			if err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			var want []wvRow
			for i := int64(10); i <= n; i += 10 {
				want = append(want, wvRow{rowid: i, cols: []any{i, fmt.Sprintf("row-%d", i)}})
			}
			if got, expect := n-deleted, len(want); got != expect {
				t.Fatalf("kept %d rows, want %d", got, expect)
			}
			verifyMutatedDB(t, path, "t", "id,v", 0, want)
		})
	}
}

// ---- UPDATE scenarios ----

func TestWriteUpdateGrowShrinkOverflowAcceptedByCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("grow_shrink_%d.sqlite", pageSize))
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			wvExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
			bigVal := stringRepeat("a", pageSize*4) // large enough to force overflow at either page size
			wvExec(t, db, "INSERT INTO t(id,v) VALUES(1,'short')")
			wvExec(t, db, fmt.Sprintf("INSERT INTO t(id,v) VALUES(2,%s)", wvString(bigVal)))
			if err := db.Close(); err != nil {
				t.Fatalf("engine Close: %v", err)
			}

			db2, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			grownVal := stringRepeat("b", pageSize*6)
			if _, err := db2.Update(fmt.Sprintf("UPDATE t SET v=%s WHERE id=1", wvString(grownVal))); err != nil {
				t.Fatalf("Update (grow): %v", err)
			}
			if _, err := db2.Update("UPDATE t SET v='tiny' WHERE id=2"); err != nil {
				t.Fatalf("Update (shrink): %v", err)
			}
			if err := db2.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			want := []wvRow{
				{rowid: 1, cols: []any{int64(1), grownVal}},
				{rowid: 2, cols: []any{int64(2), "tiny"}},
			}
			verifyMutatedDB(t, path, "t", "id,v", 0, want)
		})
	}
}

func TestWriteUpdateIPKChangeAcceptedByCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildSimpleIntTable(t, pageSize, 5)
			db, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			if _, err := db.Update("UPDATE t SET id=1000 WHERE id=3"); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			want := []wvRow{
				{rowid: 1, cols: []any{int64(1), "row-1"}},
				{rowid: 2, cols: []any{int64(2), "row-2"}},
				{rowid: 4, cols: []any{int64(4), "row-4"}},
				{rowid: 5, cols: []any{int64(5), "row-5"}},
				{rowid: 1000, cols: []any{int64(1000), "row-3"}},
			}
			verifyMutatedDB(t, path, "t", "id,v", 0, want)
		})
	}
}

func TestWriteUpdateWhereAndCurrentValueAcceptedByCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("where_current_%d.sqlite", pageSize))
			db, err := engine.Create(path)
			if err != nil {
				t.Fatalf("engine.Create: %v", err)
			}
			wvExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, n INTEGER)")
			for i := 1; i <= 5; i++ {
				wvExec(t, db, fmt.Sprintf("INSERT INTO t(id,n) VALUES(%d,%d)", i, i*10))
			}
			if err := db.Close(); err != nil {
				t.Fatalf("engine Close: %v", err)
			}

			db2, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			if _, err := db2.Update("UPDATE t SET n=n+1 WHERE id <= 3"); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if err := db2.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			want := []wvRow{
				{rowid: 1, cols: []any{int64(1), int64(11)}},
				{rowid: 2, cols: []any{int64(2), int64(21)}},
				{rowid: 3, cols: []any{int64(3), int64(31)}},
				{rowid: 4, cols: []any{int64(4), int64(40)}},
				{rowid: 5, cols: []any{int64(5), int64(50)}},
			}
			verifyMutatedDB(t, path, "t", "id,n", 0, want)
		})
	}
}

// ---- mixed sequence ----

// TestMixedInsertUpdateDeleteInsertAcceptedByCSQLite runs the task's
// "mixed sequence" scenario: INSERT, UPDATE, DELETE, INSERT (each against a
// database reopened via engine.OpenWrite), with integrity_check after.
func TestWriteMixedInsertUpdateDeleteInsertAcceptedByCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildSimpleIntTable(t, pageSize, 5) // rows 1..5, v="row-<id>"

			db1, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite (insert): %v", err)
			}
			wvExec(t, db1, "INSERT INTO t(id,v) VALUES(6,'row-6')")
			if err := db1.Close(); err != nil {
				t.Fatalf("Close (insert): %v", err)
			}

			db2, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite (update): %v", err)
			}
			if _, err := db2.Update("UPDATE t SET v=v||'-upd' WHERE id IN (2,4,6)"); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if err := db2.Close(); err != nil {
				t.Fatalf("Close (update): %v", err)
			}

			db3, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite (delete): %v", err)
			}
			if _, err := db3.Delete("DELETE FROM t WHERE id=3"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if err := db3.Close(); err != nil {
				t.Fatalf("Close (delete): %v", err)
			}

			db4, err := engine.OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite (insert 2): %v", err)
			}
			wvExec(t, db4, "INSERT INTO t(id,v) VALUES(7,'row-7')")
			if err := db4.Close(); err != nil {
				t.Fatalf("Close (insert 2): %v", err)
			}

			want := []wvRow{
				{rowid: 1, cols: []any{int64(1), "row-1"}},
				{rowid: 2, cols: []any{int64(2), "row-2-upd"}},
				{rowid: 4, cols: []any{int64(4), "row-4-upd"}},
				{rowid: 5, cols: []any{int64(5), "row-5"}},
				{rowid: 6, cols: []any{int64(6), "row-6-upd"}},
				{rowid: 7, cols: []any{int64(7), "row-7"}},
			}
			verifyMutatedDB(t, path, "t", "id,v", 0, want)
		})
	}
}

func stringRepeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}
