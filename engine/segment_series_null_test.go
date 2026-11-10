package engine_test

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestSeriesProgramNullTest: a compiled program over generate_series that
// only null-tests a column reads its NULL indicator, which for a series is
// never set. The block it read was the column's values, so every nonzero
// value tested NULL.
func TestSeriesProgramNullTest(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "s.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, tc := range []struct {
		q    string
		want int64
	}{
		{`SELECT count(*) FROM generate_series(0,9) WHERE value IS NULL`, 0},
		{`SELECT count(*) FROM generate_series(0,9) WHERE value IS NOT NULL`, 10},
		{`SELECT count(*) FROM generate_series(0,9) WHERE NOT (value IS NOT NULL)`, 0},
		{`SELECT sum(value IS NULL) FROM generate_series(0,9)`, 0},
		{`SELECT count(*) FROM generate_series(0,9) WHERE value IS NULL OR value = 3`, 1},
		{`SELECT count(*) FROM generate_series(0,9) WHERE value IS NOT NULL AND value > 3`, 6},
		{`SELECT count(*) FROM generate_series(0,9) WHERE start IS NULL`, 0},
		{`SELECT count(*) FROM generate_series(0,9) a, generate_series(0,2) b WHERE b.value IS NULL`, 0},
		{`SELECT count(*) FROM generate_series(0,9) a, generate_series(0,2) b WHERE a.value IS NOT NULL`, 30},
	} {
		var got int64
		if err := db.QueryRow(tc.q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		if got != tc.want {
			t.Errorf("%s = %d, want %d", tc.q, got, tc.want)
		}
	}
	for _, tc := range []struct {
		q    string
		want int64
	}{
		{`CREATE TABLE n AS SELECT value v FROM generate_series(0,9) WHERE value IS NULL`, 0},
		{`CREATE TABLE nn AS SELECT value v FROM generate_series(0,9) WHERE value IS NOT NULL`, 10},
	} {
		if _, err := db.Exec(tc.q); err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
	}
	for tbl, want := range map[string]int64{"n": 0, "nn": 10} {
		var got int64
		if err := db.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("CREATE TABLE %s AS ... holds %d rows, want %d", tbl, got, want)
		}
	}
}
