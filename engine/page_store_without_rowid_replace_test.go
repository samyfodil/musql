package engine

import (
	"path/filepath"
	"testing"
)

// TestWithoutRowidPutReplacesExistingKey: direct row put on WITHOUT ROWID tables.
func TestWithoutRowidPutReplacesExistingKey(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "wr.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE t(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
		`INSERT INTO t VALUES('x', 1)`,
		`INSERT INTO t VALUES('y', 7)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	tbl := db.findTableMeta("t")
	if tbl == nil {
		t.Fatal("no tableMeta for t")
	}
	var id uint64
	for rid, rec := range tbl.rows.all() {
		if len(rec) > 0 && rec[0].Typ == Text && string(rec[0].S) == "x" {
			id = rid
		}
	}
	if id == 0 {
		t.Fatal("no row found for primary key 'x'")
	}

	// Same id, same primary key, different value -- the shape that duplicated.
	tbl.putRow(id, []Value{{Typ: Text, S: []byte("x")}, {Typ: Int, I: 2}})

	pg, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := pg.QueryArgs(`SELECT a, b FROM t ORDER BY a`, nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("table has %d rows after replacing one, want 2: %v", len(rows), rows)
	}
	if got := string(rows[0][0].S); got != "x" || rows[0][1].Typ != Int || rows[0][1].I != 2 {
		t.Fatalf("row 'x' = %v, want x|2", rows[0])
	}
	// The untouched row must be intact: a delete-then-insert that deleted too
	// much would take this with it.
	if got := string(rows[1][0].S); got != "y" || rows[1][1].Typ != Int || rows[1][1].I != 7 {
		t.Fatalf("row 'y' = %v, want y|7", rows[1])
	}
}
