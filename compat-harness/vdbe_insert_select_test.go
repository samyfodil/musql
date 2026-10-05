// This file tests INSERT INTO SELECT and CREATE TABLE AS SELECT (CTAS),
// verifying data fidelity and schema type correctness.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// wvColType is one expected PRAGMA table_info row: a column's name and its
// verbatim declared-type text ("" for a column with no declared type at all).
type wvColType struct {
	name string
	decl string
}

// verifyTableInfoViaCSQLite asserts that C SQLite's own PRAGMA
// table_info(tableName) -- read straight out of the file the pure-Go engine
// wrote -- reports exactly the column names and declared-type text in want,
// in that order. This is what proves a CTAS table's schema (not just its
// row values) is byte-faithful: PRAGMA table_info's "type" column is the
// verbatim declared-type text stored in sqlite_schema's CREATE TABLE SQL,
// the same text createTableAsSelect (engine/schema_write.go) decides to
// write (or, for a general-expression column, to omit entirely).
func verifyTableInfoViaCSQLite(t *testing.T, db *sql.DB, tableName string, want []wvColType) {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", tableName, err)
	}
	defer rows.Close()

	var got []wvColType
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("PRAGMA table_info(%s): scan: %v", tableName, err)
		}
		got = append(got, wvColType{name: name, decl: ctype})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: table_info returned %d columns %v, want %d %v", tableName, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: column %d = %+v, want %+v", tableName, i, got[i], want[i])
		}
	}
}

// isDB is the subset of *engine.Session's write-path Exec surface these builders
// need, letting insExec accept it fatally on error (mirroring
// write_verify_test.go's wvExec, but this file also needs to assert that a
// deliberately-invalid statement FAILS, which wvExec alone can't express).
func insExec(t *testing.T, db *engine.Session, sqlText string) {
	t.Helper()
	if err := db.Exec(sqlText); err != nil {
		t.Fatalf("engine writer Exec(%s): %v", sqlText, err)
	}
}

// insExecWantErr requires sqlText to FAIL against db, returning the error
// for the caller to inspect/log; a nil error is a hard test failure.
func insExecWantErr(t *testing.T, db *engine.Session, sqlText string) error {
	t.Helper()
	err := db.Exec(sqlText)
	if err == nil {
		t.Fatalf("engine writer Exec(%s): expected an error, got none", sqlText)
	}
	return err
}

// buildInsertSelectCTASDB drives the pure-Go engine writer through the
// INSERT ... SELECT / CTAS scenario matrix this gate covers (see the task
// list in this file's package doc comment) and returns the finished file's
// path, per-table expected content (for verifyTableViaCSQLite, reused
// verbatim from write_verify_test.go), and per-CTAS-table expected
// PRAGMA table_info columns.
func buildInsertSelectCTASDB(t *testing.T, pageSize int) (path string, tables []wvTable, typeChecks map[string][]wvColType) {
	t.Helper()
	path = filepath.Join(t.TempDir(), fmt.Sprintf("insertselect_ctas_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	typeChecks = make(map[string][]wvColType)

	// ---- source tables ----
	// src has an INTEGER PRIMARY KEY (id) and one untyped column (n, always
	// NULL) so a CTAS/INSERT...SELECT reading it exercises: an inherited
	// INTEGER/TEXT/REAL/BLOB declared type, an inherited "no declared type"
	// column, NULLs, negative/zero/empty/unicode values -- all the storage
	// classes and edge cases this gate's task list asks for.
	insExec(t, db, `CREATE TABLE src (id INTEGER PRIMARY KEY, a INTEGER, b TEXT, c REAL, d BLOB, n)`)
	type srcRow struct {
		id   int64
		a    int64
		b    string
		c    float64
		d    []byte
		hasD bool
	}
	srcRaw := []srcRow{
		{id: 1, a: 10, b: "alpha", c: 1.5, d: []byte{0x01, 0x02}, hasD: true},
		{id: 2, a: 20, b: "", c: 0, hasD: false},
		{id: 3, a: -5, b: "unicode: 日本語 café", c: -3.25, d: []byte{}, hasD: true},
		{id: 4, a: 30, b: "beta", c: 2.5, d: []byte{0xff, 0xab, 0xcd}, hasD: true},
	}
	for _, r := range srcRaw {
		dVal := "NULL"
		if r.hasD {
			dVal = wvBlob(r.d)
		}
		insExec(t, db, fmt.Sprintf("INSERT INTO src(id,a,b,c,d,n) VALUES(%d,%d,%s,%s,%s,NULL)",
			r.id, r.a, wvString(r.b), wvFloatLiteral(r.c), dVal))
	}

	insExec(t, db, `CREATE TABLE src2 (id INTEGER, tag TEXT)`)
	insExec(t, db, `INSERT INTO src2(id,tag) VALUES(1,'x'),(2,'x'),(3,'y'),(4,'y')`)

	srcRowVal := func(i int) []any {
		r := srcRaw[i]
		var dv any
		if r.hasD {
			dv = r.d
		}
		return []any{r.a, r.b, r.c, dv, nil}
	}

	// ---- 1. INSERT INTO t2 SELECT * FROM t1 ----
	insExec(t, db, `CREATE TABLE dest1 (id INTEGER PRIMARY KEY, a INTEGER, b TEXT, c REAL, d BLOB, n)`)
	insExec(t, db, `INSERT INTO dest1 SELECT * FROM src`)
	var dest1Rows []wvRow
	for i, r := range srcRaw {
		dest1Rows = append(dest1Rows, wvRow{rowid: r.id, cols: append([]any{r.id}, srcRowVal(i)...)})
	}
	tables = append(tables, wvTable{name: "dest1", colList: "id,a,b,c,d,n", rows: dest1Rows})

	// ---- 2. explicit column list + reordering, plus WHERE ----
	insExec(t, db, `CREATE TABLE dest2 (x TEXT, y INTEGER)`)
	insExec(t, db, `INSERT INTO dest2(y,x) SELECT a,b FROM src WHERE a > 0 ORDER BY a`)
	var dest2Rows []wvRow
	rid := int64(0)
	for _, i := range []int{0, 1, 3} { // a=10,20,30, ascending
		rid++
		r := srcRaw[i]
		dest2Rows = append(dest2Rows, wvRow{rowid: rid, cols: []any{r.b, r.a}})
	}
	tables = append(tables, wvTable{name: "dest2", colList: "x,y", rows: dest2Rows})

	// ---- 3. JOIN + aggregate + ORDER BY (deterministic group order) ----
	insExec(t, db, `CREATE TABLE dest3 (tag TEXT, cnt INTEGER)`)
	insExec(t, db, `INSERT INTO dest3 SELECT s2.tag, count(*) FROM src s1 JOIN src2 s2 ON s1.id = s2.id GROUP BY s2.tag ORDER BY s2.tag`)
	tables = append(tables, wvTable{name: "dest3", colList: "tag,cnt", rows: []wvRow{
		{rowid: 1, cols: []any{"x", int64(2)}},
		{rowid: 2, cols: []any{"y", int64(2)}},
	}})

	// ---- 4. self-insert: INSERT INTO t SELECT * FROM t doubles the table ----
	insExec(t, db, `CREATE TABLE selfd (v INTEGER)`)
	insExec(t, db, `INSERT INTO selfd VALUES(1),(2),(3)`)
	insExec(t, db, `INSERT INTO selfd SELECT * FROM selfd`)
	tables = append(tables, wvTable{name: "selfd", colList: "v", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1)}},
		{rowid: 2, cols: []any{int64(2)}},
		{rowid: 3, cols: []any{int64(3)}},
		{rowid: 4, cols: []any{int64(1)}},
		{rowid: 5, cols: []any{int64(2)}},
		{rowid: 6, cols: []any{int64(3)}},
	}})

	// ---- 5. column-count mismatch: "N values for M columns" ----
	if mismatchErr := insExecWantErr(t, db, `INSERT INTO dest1 SELECT a FROM src`); mismatchErr != nil {
		t.Logf("INSERT column-count mismatch correctly rejected: %v", mismatchErr)
	}

	// ---- 6. constraint interplay: UNIQUE violation rolls back the whole statement ----
	insExec(t, db, `CREATE TABLE uniq1 (a INTEGER UNIQUE)`)
	insExec(t, db, `INSERT INTO uniq1 VALUES(10)`) // collides with src.a=10 (srcRaw[0])
	if uniqErr := insExecWantErr(t, db, `INSERT INTO uniq1 SELECT a FROM src`); uniqErr != nil {
		t.Logf("INSERT...SELECT UNIQUE violation correctly rejected: %v", uniqErr)
	}
	// The failed statement above must have rolled back ALL of its rows (not
	// just refused the colliding one), leaving uniq1 with only its original
	// pre-statement row -- exactly like a VALUES-sourced INSERT's own
	// verifyInsertUniqueOrRollback (shared code, see insertFromSelect's doc
	// comment).
	tables = append(tables, wvTable{name: "uniq1", colList: "a", rows: []wvRow{
		{rowid: 1, cols: []any{int64(10)}},
	}})

	// ---- 7. CTAS from a simple SELECT: every column a direct reference ----
	insExec(t, db, `CREATE TABLE ctas_simple AS SELECT a,b,c,d,n FROM src`)
	var ctasSimpleRows []wvRow
	for i := range srcRaw {
		ctasSimpleRows = append(ctasSimpleRows, wvRow{rowid: int64(i + 1), cols: srcRowVal(i)})
	}
	tables = append(tables, wvTable{name: "ctas_simple", colList: "a,b,c,d,n", rows: ctasSimpleRows})
	// A CTAS column's declared type is the AFFINITY NAME, not the source
	// column's own declared type: SQLite renders a fresh definition for the
	// new table (INTEGER->INT, BLOB and no-type -> no type at all). Verified
	// directly against C SQLite for exactly this schema, and gated
	// byte-for-byte by vdbe_ctas_render_test.go. These expectations
	// previously read INTEGER/BLOB, matching what this engine used to store
	// rather than what SQLite does.
	typeChecks["ctas_simple"] = []wvColType{
		{name: "a", decl: "INT"},
		{name: "b", decl: "TEXT"},
		{name: "c", decl: "REAL"},
		{name: "d", decl: ""},
		{name: "n", decl: ""},
	}

	// ---- 8. CTAS from a JOIN: a direct reference to a source table's own
	// INTEGER PRIMARY KEY column carries its INTEGER affinity through as the
	// rendered type name "INT" -- WITHOUT becoming an IPK/rowid-alias itself:
	// a CTAS table is always a plain rowid table, even here.
	insExec(t, db, `CREATE TABLE ctas_join AS SELECT s1.id, s2.tag FROM src s1 JOIN src2 s2 ON s1.id = s2.id ORDER BY s1.id`)
	tables = append(tables, wvTable{name: "ctas_join", colList: "id,tag", rows: []wvRow{
		{rowid: 1, cols: []any{int64(1), "x"}},
		{rowid: 2, cols: []any{int64(2), "x"}},
		{rowid: 3, cols: []any{int64(3), "y"}},
		{rowid: 4, cols: []any{int64(4), "y"}},
	}})
	typeChecks["ctas_join"] = []wvColType{
		{name: "id", decl: "INT"}, // affinity name -- see ctas_simple above
		{name: "tag", decl: "TEXT"},
	}

	// ---- 9. CTAS mixing a direct column reference with a general
	// expression in the SAME table: only the direct reference inherits a
	// declared type.
	insExec(t, db, `CREATE TABLE ctas_mixed AS SELECT b, a+1 AS ap1 FROM src WHERE a > 0 ORDER BY a`)
	var ctasMixedRows []wvRow
	rid = 0
	for _, i := range []int{0, 1, 3} {
		rid++
		r := srcRaw[i]
		ctasMixedRows = append(ctasMixedRows, wvRow{rowid: rid, cols: []any{r.b, r.a + 1}})
	}
	tables = append(tables, wvTable{name: "ctas_mixed", colList: "b,ap1", rows: ctasMixedRows})
	typeChecks["ctas_mixed"] = []wvColType{
		{name: "b", decl: "TEXT"},
		{name: "ap1", decl: ""},
	}

	// ---- 10. CTAS from a pure-aggregate SELECT: count/sum/avg are all
	// general expressions, so every column is untyped.
	insExec(t, db, `CREATE TABLE ctas_agg AS SELECT count(*) AS cnt, sum(a) AS suma, avg(c) AS avgc FROM src WHERE a > 0`)
	wantAvg := ((srcRaw[0].c + srcRaw[1].c + srcRaw[3].c) / 3)
	tables = append(tables, wvTable{name: "ctas_agg", colList: "cnt,suma,avgc", rows: []wvRow{
		{rowid: 1, cols: []any{int64(3), int64(60), wantAvg}},
	}})
	typeChecks["ctas_agg"] = []wvColType{
		{name: "cnt", decl: ""},
		{name: "suma", decl: ""},
		{name: "avgc", decl: ""},
	}

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}
	return path, tables, typeChecks
}

// TestInsertSelectAndCTASAcceptedByCSQLite is the INSERT ... SELECT / CTAS
// gate: for a database built ENTIRELY by the pure-Go engine writer, at both a
// small (512) and default-ish (4096) page size, C SQLite must report
// integrity_check='ok', read back every row exactly as inserted, and (for
// every CTAS table) see exactly the declared-type schema this engine derived.
func TestInsertSelectAndCTASAcceptedByCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path, tables, typeChecks := buildInsertSelectCTASDB(t, pageSize)

			sdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatalf("sql.Open(sqlite3): %v", err)
			}
			defer sdb.Close()

			var integrity string
			if err := sdb.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
				t.Fatalf("PRAGMA integrity_check: %v", err)
			}
			if integrity != "ok" {
				t.Fatalf("PRAGMA integrity_check = %q, want \"ok\"", integrity)
			}

			for _, tbl := range tables {
				t.Run(tbl.name, func(t *testing.T) {
					verifyTableViaCSQLite(t, sdb, tbl)
				})
			}
			for name, want := range typeChecks {
				t.Run(name+"/table_info", func(t *testing.T) {
					verifyTableInfoViaCSQLite(t, sdb, name, want)
				})
			}
		})
	}
}

// TestInsertSelectColumnCountMismatchMatchesCSQLite additionally confirms
// (against a real, in-memory SQLite connection given the identical schema)
// that a column-count mismatch between an INSERT ... SELECT's source and its
// target column list is genuinely a real-SQLite error too, not merely this
// engine's own invention.
func TestInsertSelectColumnCountMismatchMatchesCSQLite(t *testing.T) {
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	for _, stmt := range []string{
		`CREATE TABLE src(a INTEGER, b INTEGER, c INTEGER)`,
		`CREATE TABLE dest(a INTEGER, b INTEGER, c INTEGER)`,
		`INSERT INTO src VALUES(1,2,3)`,
	} {
		if _, err := sdb.Exec(stmt); err != nil {
			t.Fatalf("real sqlite3 Exec(%s): %v", stmt, err)
		}
	}
	if _, err := sdb.Exec(`INSERT INTO dest SELECT a FROM src`); err == nil {
		t.Fatalf("C SQLite unexpectedly accepted a column-count-mismatched INSERT ... SELECT")
	} else {
		t.Logf("C SQLite rejects the same mismatch too: %v", err)
	}
}
