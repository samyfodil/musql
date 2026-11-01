package replication

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestPackClockPagesThroughTheCells: an old clock of a row per cell, more
// cells than one page reads, packs into one record per key with every cell,
// and packing again finds nothing to do.
func TestPackClockPagesThroughTheCells(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE main._repl_meta(k TEXT PRIMARY KEY, v BLOB)`,
		`CREATE TABLE main._repl_clock(tbl TEXT, pk BLOB, col TEXT, hlc INTEGER, site TEXT, val BLOB, PRIMARY KEY(tbl, pk, col))`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	const keys, cols = 3000, 3
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		for c := range cols {
			if _, err := tx.ExecContext(ctx, `INSERT INTO main._repl_clock VALUES('t', ?, ?, ?, 's', ?)`,
				[]byte(fmt.Sprintf("k%05d", k)), fmt.Sprintf("c%d", c), k*cols+c+1, []byte{byte(c)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := packClock(ctx, db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM main._repl_clock`).Scan(&n); err != nil || n != keys {
		t.Fatalf("%d records, %v; want %d", n, err, keys)
	}
	cells, err := readRec(ctx, db, "t", []byte("k02999"))
	if err != nil || len(cells) != cols || cells["c2"].hlc != HLC(keys*cols) {
		t.Fatalf("the last key's record: %v, %v", cells, err)
	}
	if err := packClock(ctx, db); err != nil {
		t.Fatalf("packing a packed clock: %v", err)
	}
}
