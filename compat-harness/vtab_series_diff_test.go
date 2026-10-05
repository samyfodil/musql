// Tests generate_series virtual table against an oracle using recursive CTEs.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// seriesOracleCTE returns a mattn-runnable recursive CTE that yields the same
// integer series generate_series(start, stop, step) does, aliased "s(value)".
func seriesOracleCTE(start, stop, step int64) string {
	return fmt.Sprintf(
		"WITH RECURSIVE s(value) AS ("+
			"SELECT %d WHERE (%d>0 AND %d<=%d) OR (%d<0 AND %d>=%d) "+
			"UNION ALL "+
			"SELECT value+%d FROM s WHERE (%d>0 AND value+%d<=%d) OR (%d<0 AND value+%d>=%d)"+
			")",
		start,
		step, start, stop, step, start, stop,
		step,
		step, step, stop, step, step, stop,
	)
}

// queryStrings runs sql against db and returns normalized column names + rows,
// reusing the harness's collectRows normalization (driver_diff_test.go).
func queryStrings(t *testing.T, db *sql.DB, sqlText string) ([]string, [][]string) {
	t.Helper()
	rows, err := db.Query(sqlText)
	if err != nil {
		t.Fatalf("Query(%q): %v", sqlText, err)
	}
	defer rows.Close()
	return collectRows(t, rows)
}

func openPureOnly(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver.DriverName, filepath.Join(t.TempDir(), "series.pure.sqlite"))
	if err != nil {
		t.Fatalf("open pure: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openMattnMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "series.mattn.sqlite"))
	if err != nil {
		t.Fatalf("open mattn: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// compareSeries runs the pure engine's generate_series and mattn's equivalent
// recursive CTE under an identical projection/predicate suffix and requires the
// normalized result sets to match. suffix begins after the FROM source (e.g.
// " ORDER BY value" or " WHERE value%2=0 ORDER BY value"). proj is the
// select-list (e.g. "value" or "count(*), sum(value)").
func compareSeries(t *testing.T, pureDB, mattnDB *sql.DB, proj string, start, stop, step int64, suffix string) {
	t.Helper()
	pureSQL := fmt.Sprintf("SELECT %s FROM generate_series(%d,%d,%d)%s", proj, start, stop, step, suffix)
	mattnSQL := fmt.Sprintf("%s SELECT %s FROM s%s", seriesOracleCTE(start, stop, step), proj, suffix)

	pCols, pRows := queryStrings(t, pureDB, pureSQL)
	mCols, mRows := queryStrings(t, mattnDB, mattnSQL)

	// Column names differ (pure reports generate_series' "value"; the CTE also
	// reports "value") -- compare row DATA, which is what the vtab pipeline
	// must get right; the aggregate projection column names ("count(*)" etc.)
	// are identical on both sides too, but we only assert row data to stay
	// robust to any incidental naming difference.
	_ = pCols
	_ = mCols
	if !rowsEqual(pRows, mRows) {
		t.Errorf("series mismatch:\n  pure : %s -> %v\n  mattn: %s -> %v", pureSQL, pRows, mattnSQL, mRows)
	}
}

func rowsEqual(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// TestGenerateSeriesDiffFuzz fuzzes random bounded (start, stop, step) triples
// and compares the pure engine's generate_series against mattn's recursive-CTE
// oracle under several projections/predicates.
func TestGenerateSeriesDiffFuzz(t *testing.T) {
	pureDB := openPureOnly(t)
	mattnDB := openMattnMem(t)

	rng := rand.New(rand.NewSource(0x5E21E5)) // deterministic
	const iters = 400
	for i := 0; i < iters; i++ {
		start := int64(rng.Intn(401) - 200) // [-200, 200]
		stop := int64(rng.Intn(401) - 200)
		step := int64(rng.Intn(13) - 6) // [-6, 6]
		if step == 0 {
			step = 1
		}
		// Bare series (native row order), ORDER BY, filtered, and aggregate.
		compareSeries(t, pureDB, mattnDB, "value", start, stop, step, "")
		compareSeries(t, pureDB, mattnDB, "value", start, stop, step, " ORDER BY value")
		compareSeries(t, pureDB, mattnDB, "value", start, stop, step, " ORDER BY value DESC")
		compareSeries(t, pureDB, mattnDB, "value", start, stop, step, " WHERE value%2=0 ORDER BY value")
		compareSeries(t, pureDB, mattnDB, "value", start, stop, step, " WHERE value>=0 ORDER BY value")
		compareSeries(t, pureDB, mattnDB, "count(*), sum(value), min(value), max(value)", start, stop, step, "")
	}
}

// TestGenerateSeriesEponymousWhereForm validates the eponymous
// "FROM generate_series WHERE start=.. AND stop=.. [AND step=..]" hidden-column
// constraint-pushdown form against the recursive-CTE oracle.
func TestGenerateSeriesEponymousWhereForm(t *testing.T) {
	pureDB := openPureOnly(t)
	mattnDB := openMattnMem(t)

	rng := rand.New(rand.NewSource(0xC0FFEE))
	for i := 0; i < 200; i++ {
		start := int64(rng.Intn(101) - 50)
		stop := int64(rng.Intn(101) - 50)
		step := int64(rng.Intn(9) - 4)
		if step == 0 {
			step = 1
		}
		pureSQL := fmt.Sprintf(
			"SELECT value FROM generate_series WHERE start=%d AND stop=%d AND step=%d ORDER BY value",
			start, stop, step)
		mattnSQL := fmt.Sprintf("%s SELECT value FROM s ORDER BY value", seriesOracleCTE(start, stop, step))
		_, pRows := queryStrings(t, pureDB, pureSQL)
		_, mRows := queryStrings(t, mattnDB, mattnSQL)
		if !rowsEqual(pRows, mRows) {
			t.Errorf("eponymous-where mismatch (start=%d stop=%d step=%d):\n  pure  %v\n  mattn %v", start, stop, step, pRows, mRows)
		}
	}
}

// TestGenerateSeriesTwoArgDefaultStep validates the 2-argument call form
// (default step = 1) against the oracle.
func TestGenerateSeriesTwoArgDefaultStep(t *testing.T) {
	pureDB := openPureOnly(t)
	mattnDB := openMattnMem(t)
	for _, tc := range []struct{ start, stop int64 }{
		{1, 10}, {0, 0}, {5, 1}, {-3, 3}, {-10, -1}, {7, 7},
	} {
		pureSQL := fmt.Sprintf("SELECT value FROM generate_series(%d,%d) ORDER BY value", tc.start, tc.stop)
		mattnSQL := fmt.Sprintf("%s SELECT value FROM s ORDER BY value", seriesOracleCTE(tc.start, tc.stop, 1))
		_, pRows := queryStrings(t, pureDB, pureSQL)
		_, mRows := queryStrings(t, mattnDB, mattnSQL)
		if !rowsEqual(pRows, mRows) {
			t.Errorf("2-arg mismatch (%d,%d):\n  pure  %v\n  mattn %v", tc.start, tc.stop, pRows, mRows)
		}
	}
}
