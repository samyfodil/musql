package compat

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2" // driver "duckdb"
)

// DuckDB as a reference arm in the columnar benchmark. It is a different class
// of engine -- analytical, columnar, vectorized, multithreaded by default, and
// not SQLite-compatible -- so it never feeds the headline multiples; it is the
// yardstick for musql's columnar path on the scan workloads.
//
// It is timed twice: with one thread, which is what musql and C SQLite use,
// and with DuckDB's default thread count, which is how it is normally run.
// Its answers are checked against C SQLite by VALUE: DuckDB types count(*) as
// BIGINT and sum() as HUGEINT, so the type-tagged rendering the other arms use
// would differ on every row without either engine being wrong.

// openDuckDB seeds a DuckDB file with the benchmark tables, or returns nil with
// the reason logged.
func openDuckDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	return seedBenchSoft(t, &benchEngine{label: "duckdb", driver: "duckdb", path: path})
}

// setDuckThreads sets the thread count for the next timings; 0 restores the
// default. The arm's pool is a single connection, so the setting sticks.
func setDuckThreads(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	q := "RESET threads"
	if n > 0 {
		q = fmt.Sprintf("SET threads = %d", n)
	}
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("duckdb %s: %v", q, err)
	}
}

// renderValues renders a result as untyped values, for comparing engines that
// agree on numbers but not on integer widths.
func renderValues(db *sql.DB, q string, args []any) (string, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for _, c := range cells {
			fmt.Fprintf(&sb, "%v|", c)
		}
		sb.WriteString(";")
	}
	return sb.String(), rows.Err()
}
