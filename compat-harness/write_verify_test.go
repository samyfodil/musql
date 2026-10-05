// This file verifies the write path: the pure-Go engine writer produces
// databases that pass PRAGMA integrity_check and whose rows read back
// byte-identical through C SQLite.
package compat

import (
	"bytes"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// wvRow is one expected row: its rowid and column values, using database/sql
// scan types directly (int64, float64, string, []byte, nil) since that's
// what mattn hands back and what this file compares against.
type wvRow struct {
	rowid int64
	cols  []any
}

// wvTable is one table's expected content plus the "SELECT ..." column list
// (in table-declaration order) to read it back with.
type wvTable struct {
	name    string
	colList string
	rows    []wvRow
}

func wvFloatLiteral(f float64) string {
	if math.Signbit(f) && f == 0 {
		return "-0.0"
	}
	s := fmt.Sprintf("%.17g", f)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

func wvString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func wvBlob(b []byte) string   { return "x'" + fmt.Sprintf("%x", b) + "'" }

func wvExec(t *testing.T, db *engine.Session, sqlText string) {
	t.Helper()
	if err := db.Exec(sqlText); err != nil {
		t.Fatalf("engine writer Exec(%s): %v", sqlText, err)
	}
}

// buildWriteVerifyDB drives the pure-Go engine writer through the same
// scenario matrix engine/write_test.go exercises against its own read side
// -- several tables, every column type/serial-type family (including exact
// MinInt64/MaxInt64 integers, signed zero, unicode/empty text, populated/
// empty blobs), an INTEGER PRIMARY KEY table with deliberately shuffled
// (non-ascending) explicit rowids, a plain-rowid table with no IPK, a large
// (thousands of rows) table forcing leaf splits and multi-level interior
// b-tree growth, and a table with tens-of-KB blobs/text forcing multi-page
// overflow chains -- and returns the finished file's path plus, per table,
// what a correct reader must see.
func buildWriteVerifyDB(t *testing.T, pageSize int) (path string, tables []wvTable) {
	t.Helper()
	path = filepath.Join(t.TempDir(), fmt.Sprintf("writeverify_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	// types_ipk: INTEGER PRIMARY KEY, shuffled explicit rowids, one row per
	// interesting value family.
	wvExec(t, db, `CREATE TABLE types_ipk (id INTEGER PRIMARY KEY, i INTEGER, r REAL, t TEXT, b BLOB, n)`)
	type tRow struct {
		id int64
		i  int64
		r  float64
		t  string
		b  []byte
	}
	raw := []tRow{
		{id: 50, i: 0, r: 3.14159, t: "hello, world", b: []byte{0x00, 0x01, 0x02, 0xff, 0xfe}},
		{id: 10, i: 1, r: math.Copysign(0, -1), t: "", b: []byte{}},
		{id: 30, i: math.MaxInt64, r: 1.0, t: "unicode: 日本語 \U0001F600 café", b: []byte{0xde, 0xad, 0xbe, 0xef}},
		{id: 5, i: math.MinInt64, r: 3.14159265358979, t: "y", b: []byte{0xab}},
		{id: 9999, i: -1, r: 0.0, t: "padding-" + strings.Repeat("z", 100), b: bytes.Repeat([]byte{0xab, 0xcd}, 50)},
	}
	for _, r := range raw {
		wvExec(t, db, fmt.Sprintf("INSERT INTO types_ipk(id,i,r,t,b,n) VALUES(%d,%d,%s,%s,%s,NULL)",
			r.id, r.i, wvFloatLiteral(r.r), wvString(r.t), wvBlob(r.b)))
	}
	sorted := append([]tRow(nil), raw...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].id < sorted[j].id })
	var typesRows []wvRow
	for _, r := range sorted {
		var bl any = r.b
		if len(r.b) == 0 {
			bl = []byte{}
		}
		// r's column is REAL affinity: C SQLite's own "IntReal" storage
		// folding (see engine/sql_eval.go's foldAlreadyNumeric/intFoldWide,
		// and engine/write_test.go's matching fix) normalizes an exact-zero
		// REAL's sign away on storage, so the row inserted with -0.0
		// (id=10, above -- deliberately chosen to exercise this) must read
		// back through C SQLite as +0.0, not -0.0.
		wantR := r.r
		if math.Signbit(wantR) && wantR == 0 {
			wantR = 0
		}
		typesRows = append(typesRows, wvRow{rowid: r.id, cols: []any{r.id, r.i, wantR, r.t, bl, nil}})
	}
	tables = append(tables, wvTable{name: "types_ipk", colList: "id,i,r,t,b,n", rows: typesRows})

	// plain_rowid: no INTEGER PRIMARY KEY -- rowids auto-assigned 1,2,3,...
	wvExec(t, db, `CREATE TABLE plain_rowid (name TEXT, val REAL)`)
	var plainRows []wvRow
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("plain-%d", i)
		val := float64(i) * 0.25
		wvExec(t, db, fmt.Sprintf("INSERT INTO plain_rowid(name,val) VALUES(%s,%s)", wvString(name), wvFloatLiteral(val)))
		plainRows = append(plainRows, wvRow{rowid: int64(i + 1), cols: []any{name, val}})
	}
	tables = append(tables, wvTable{name: "plain_rowid", colList: "name,val", rows: plainRows})

	// many_rows: thousands of rows to force leaf splits + interior b-tree
	// growth (multi-page, and at the smaller page size, multi-level).
	wvExec(t, db, `CREATE TABLE many_rows (id INTEGER PRIMARY KEY, name TEXT, val REAL)`)
	const nMany = 3000
	var manyRows []wvRow
	for i := 1; i <= nMany; i++ {
		name := fmt.Sprintf("row-%d-with-some-padding-text-to-bulk-up-the-cell", i)
		val := float64(i) * 1.5
		wvExec(t, db, fmt.Sprintf("INSERT INTO many_rows(id,name,val) VALUES(%d,%s,%s)", i, wvString(name), wvFloatLiteral(val)))
		manyRows = append(manyRows, wvRow{rowid: int64(i), cols: []any{int64(i), name, val}})
	}
	tables = append(tables, wvTable{name: "many_rows", colList: "id,name,val", rows: manyRows})

	// big_blobs: tens-of-KB blobs/text forcing multi-page overflow chains.
	wvExec(t, db, `CREATE TABLE big_blobs (id INTEGER PRIMARY KEY, data BLOB, note TEXT)`)
	var blobRows []wvRow
	for i, kb := range []int{1, 8, 40} {
		blob := make([]byte, kb*1024)
		for j := range blob {
			blob[j] = byte((j*31 + i) % 256)
		}
		note := strings.Repeat(fmt.Sprintf("note-%d-中文-", i), 400)
		id := int64(i + 1)
		wvExec(t, db, fmt.Sprintf("INSERT INTO big_blobs(id,data,note) VALUES(%d,%s,%s)", id, wvBlob(blob), wvString(note)))
		blobRows = append(blobRows, wvRow{rowid: id, cols: []any{id, blob, note}})
	}
	tables = append(tables, wvTable{name: "big_blobs", colList: "id,data,note", rows: blobRows})

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}
	return path, tables
}

// TestWritePathAcceptedByCSQLite is the write path's hard gate: for a
// database built ENTIRELY by the pure-Go engine writer, at both a small
// (512, so leaf splits happen quickly) and default-ish (4096) page size,
// C SQLite must report integrity_check='ok' and read back every row
// exactly as inserted.
func TestWritePathAcceptedByCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path, tables := buildWriteVerifyDB(t, pageSize)

			db, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer db.Close()

			var integrity string
			if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
				t.Fatalf("PRAGMA integrity_check: %v", err)
			}
			if integrity != "ok" {
				t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
			}

			// integrity_check only returns one row when everything is fine;
			// make sure there isn't a second row hiding a further problem.
			rows, err := db.Query("PRAGMA integrity_check")
			if err != nil {
				t.Fatalf("PRAGMA integrity_check (2nd pass): %v", err)
			}
			var all []string
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatal(err)
				}
				all = append(all, s)
			}
			rows.Close()
			if len(all) != 1 || all[0] != "ok" {
				t.Fatalf("PRAGMA integrity_check returned %v, want exactly [\"ok\"]", all)
			}

			for _, tbl := range tables {
				t.Run(tbl.name, func(t *testing.T) {
					verifyTableViaCSQLite(t, db, tbl)
				})
			}
		})
	}
}

func verifyTableViaCSQLite(t *testing.T, db *sql.DB, tbl wvTable) {
	t.Helper()
	q := fmt.Sprintf("SELECT rowid,%s FROM %s ORDER BY rowid", tbl.colList, tbl.name)
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("query %s: %v", tbl.name, err)
	}
	defer rows.Close()

	nCol := len(strings.Split(tbl.colList, ","))
	var got []wvRow
	for rows.Next() {
		dest := make([]any, nCol+1)
		ptrs := make([]any, nCol+1)
		for i := range dest {
			ptrs[i] = &dest[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", tbl.name, err)
		}
		rowid, ok := dest[0].(int64)
		if !ok {
			t.Fatalf("%s: rowid column has type %T", tbl.name, dest[0])
		}
		got = append(got, wvRow{rowid: rowid, cols: dest[1:]})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) != len(tbl.rows) {
		t.Fatalf("%s: got %d rows, want %d", tbl.name, len(got), len(tbl.rows))
	}
	for i := range got {
		if got[i].rowid != tbl.rows[i].rowid {
			t.Errorf("%s: row %d: rowid = %d, want %d", tbl.name, i, got[i].rowid, tbl.rows[i].rowid)
		}
		for c := range got[i].cols {
			if !wvValueEqual(got[i].cols[c], tbl.rows[i].cols[c]) {
				t.Errorf("%s: row %d (rowid %d) col %d: got %#v, want %#v",
					tbl.name, i, got[i].rowid, c, got[i].cols[c], tbl.rows[i].cols[c])
			}
		}
	}
}

// wvValueEqual compares one scanned column value (from C SQLite via
// mattn) against the expected Go value, normalizing int64-vs-int and
// bit-exact float comparison (so e.g. -0.0 and NaN-free values compare
// correctly).
func wvValueEqual(got, want any) bool {
	switch w := want.(type) {
	case nil:
		return got == nil
	case int64:
		g, ok := got.(int64)
		return ok && g == w
	case float64:
		g, ok := got.(float64)
		return ok && math.Float64bits(g) == math.Float64bits(w)
	case string:
		switch g := got.(type) {
		case string:
			return g == w
		case []byte:
			return string(g) == w
		default:
			return false
		}
	case []byte:
		g, ok := got.([]byte)
		return ok && bytes.Equal(g, w)
	default:
		return false
	}
}
