package driver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestDeltaOnlyTableAggregates: a table whose rows exist only in the delta,
// as one Exec of a script leaves them. A delta row stores the INTEGER PRIMARY
// KEY as NULL with the rowid carrying the value, so a predicate on it or a sum
// of it must not be answered from the stored column ("count(*) WHERE id > 0"
// answered 0 and sum(id) NULL), and a FILTER on one aggregate must not hide the
// row from another.
func TestDeltaOnlyTableAggregates(t *testing.T) {
	db, err := sql.Open(DriverName, filepath.Join(t.TempDir(), "d.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE u(id INTEGER PRIMARY KEY, v INTEGER); INSERT INTO u VALUES(1, 5); INSERT INTO u VALUES(2, 6)`); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		`SELECT count(*) FROM u WHERE id > 0`:                                     "2",
		`SELECT count(*) FROM u WHERE id >= 2`:                                    "1",
		`SELECT sum(id) FROM u`:                                                   "3",
		`SELECT sum(v) FROM u WHERE id > 1`:                                       "6",
		`SELECT count(*) FROM u`:                                                  "2",
		`SELECT coalesce(sum(v) FILTER (WHERE 0), -1) FROM u`:                     "-1",
		`SELECT count(*) + 10 * coalesce(sum(abs(v)) FILTER (WHERE 0), 0) FROM u`: "2",
	} {
		var got sql.NullInt64
		if err := db.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if fmt.Sprint(got.Int64) != want || !got.Valid {
			t.Errorf("%s = %v, want %s", q, got, want)
		}
	}
}
