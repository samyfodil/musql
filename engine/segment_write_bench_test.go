package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchWriteFixture tests segment write performance with a single open session.
func benchWriteFixture(b *testing.B, rows int) string {
	b.Helper()
	segPath := filepath.Join(b.TempDir(), "w.musq")
	db, err := Create(segPath)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT)`); err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= rows; i++ {
		if eerr := db.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d,%d,'r%d')`, i, i*7, i)); eerr != nil {
			b.Fatal(eerr)
		}
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	return segPath
}

// BenchmarkSegmentCommitUpdate is one autocommit-equivalent UPDATE per iteration
// on this format: execute, append the batch, fsync.
func BenchmarkSegmentCommitUpdate(b *testing.B) {
	const rows = 10000
	segPath := benchWriteFixture(b, rows)
	nw, err := OpenWrite(segPath)
	if err != nil {
		b.Fatal(err)
	}
	defer nw.Close()
	// Touch the table once so the lazy load is not inside the timed loop, exactly
	// as the SQLite arm's pager cache is warm after its first statement.
	if eerr := nw.Exec(`UPDATE t SET k = k WHERE id = 1`); eerr != nil {
		b.Fatal(eerr)
	}
	if _, cerr := nw.Commit(); cerr != nil {
		b.Fatal(cerr)
	}
	// BOUND PARAMETERS, not interpolated text: the write plan cache is keyed on
	// the statement's text, so interpolating the values recompiles every iteration
	// and measures the compiler instead of the commit (17% of the first run).
	const stmt = `UPDATE t SET k = ? WHERE id = ?`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, eerr := nw.ExecArgs(stmt, []Value{
			{Typ: Int, I: int64(i)}, {Typ: Int, I: int64((i % rows) + 1)}}); eerr != nil {
			b.Fatal(eerr)
		}
		if _, cerr := nw.Commit(); cerr != nil {
			b.Fatal(cerr)
		}
	}
	b.StopTimer()
	if st, serr := os.Stat(segDeltaPath(segPath)); serr == nil {
		b.ReportMetric(float64(st.Size())/float64(b.N), "deltaB/op")
	}
}
