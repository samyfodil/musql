package replication

import (
	"context"
	"database/sql"
	"maps"
	"path/filepath"
	"strings"
	"testing"
)

// TestClockRecordRoundTrips: a record gives back every cell it packed -- each
// tag, shared or its own, and each value, empty ones included.
func TestClockRecordRoundTrips(t *testing.T) {
	for _, cells := range []map[string]clockCell{
		{presenceCol: {7, "a", []byte{1}}},
		{presenceCol: {7, "a", []byte{1}}, "c1": {7, "a", []byte{TypeText, 'x'}}, "c2": {7, "a", nil}},
		{presenceCol: {9, "b", []byte{0}}, "c1": {7, "a", []byte{TypeInt, 1}}, "c2": {8, "a", []byte{}}, "c3": {7, "b", []byte{TypeNull}}},
		{"c1": {1 << 60, strings.Repeat("s", 300), []byte(strings.Repeat("v", 1000))}},
	} {
		rec, top := encodeRec(cells)
		got, err := decodeRec(rec)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(cells) {
			t.Fatalf("%d cells back, want %d", len(got), len(cells))
		}
		var want HLC
		for col, c := range cells {
			g := got[col]
			if g.hlc != c.hlc || g.site != c.site || string(g.val) != string(c.val) {
				t.Fatalf("cell %q came back %+v, want %+v", col, g, c)
			}
			want = max(want, c.hlc)
		}
		if top != want {
			t.Fatalf("top %d, want %d", top, want)
		}
		for i := range rec {
			if _, err := decodeRec(rec[:i]); err == nil {
				t.Fatalf("a record cut to %d of %d bytes decoded", i, len(rec))
			}
		}
	}
}

// TestInitSchemaPacksACellClock: a database whose clock holds a row per cell
// -- what builds before records wrote -- opens with the same cells packed a
// record per row, and a build before records can no longer open it.
func TestInitSchemaPacksACellClock(t *testing.T) {
	ctx := context.Background()
	db, err := openApplyDB(filepath.Join(t.TempDir(), "cells.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE _repl_meta(k TEXT PRIMARY KEY, v BLOB)`,
		`CREATE TABLE _repl_clock(tbl TEXT, pk BLOB, col TEXT, hlc INTEGER, site TEXT, val BLOB, PRIMARY KEY(tbl,pk,col))`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]clockCell{}
	for i := range 20000 {
		tbl, pk := []string{"t1", "t2"}[i%2], EncodePK(int64(i/6))
		col := []string{presenceCol, "a", "b"}[i/2%3]
		c := clockCell{HLC(100 + i%5), []string{"x", "y"}[i%3%2], []byte{TypeInt, byte(i)}}
		if _, err := tx.Exec(`INSERT INTO _repl_clock(tbl,pk,col,hlc,site,val) VALUES(?,?,?,?,?,?)`, tbl, pk, col, int64(c.hlc), c.site, c.val); err != nil {
			t.Fatal(err)
		}
		k := tbl + "|" + string(pk)
		if want[k] == nil {
			want[k] = map[string]clockCell{}
		}
		want[k][col] = c
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := InitSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT tbl, pk, top, rec FROM main._repl_clock`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]clockCell{}
	for rows.Next() {
		var tbl string
		var pk, rec []byte
		var top int64
		if err := rows.Scan(&tbl, &pk, &top, &rec); err != nil {
			t.Fatal(err)
		}
		cells, err := decodeRec(rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, wantTop := encodeRec(cells); int64(wantTop) != top {
			t.Fatalf("top %d, want %d", top, wantTop)
		}
		got[tbl+"|"+string(pk)] = cells
	}
	rows.Close()
	if !maps.EqualFunc(got, want, func(a, b map[string]clockCell) bool {
		return maps.EqualFunc(a, b, func(x, y clockCell) bool { return x.hlc == y.hlc && x.site == y.site && string(x.val) == string(y.val) })
	}) {
		t.Fatalf("%d rows packed, want %d, or a cell changed", len(got), len(want))
	}
	if err := InitSchema(ctx, db); err != nil {
		t.Fatalf("InitSchema again: %v", err)
	}
	if _, err := db.Exec(`SELECT max(hlc) FROM main._repl_clock`); err == nil {
		t.Fatal("the query a build before records opens with still runs")
	}
	if _, err := db.Exec(`UPDATE _repl_meta SET v='3' WHERE k=?`, metaLayout); err != nil {
		t.Fatal(err)
	}
	if err := InitSchema(ctx, db); err == nil {
		t.Fatal("InitSchema took a layout newer than this build's")
	}
}

func TestInitSchemaRefusesAForeignMeta(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE _repl_meta(k TEXT PRIMARY KEY, x BLOB)`); err != nil {
		t.Fatal(err)
	}
	if err := InitSchema(context.Background(), db); err == nil {
		t.Fatal("InitSchema took a _repl_meta it cannot read")
	}
}
