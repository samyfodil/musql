// This file tests PRAGMA introspection and scalar functions (table_info, index_info,
// integrity_check, etc.) against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// formatEngineValue renders one engine.Value into a storage-class-tagged
// string comparable with formatSQLValue's rendering of the equivalent
// database/sql-scanned cgo value -- so e.g. an engine.Text "5" and a cgo
// int64(5) are never conflated even though fmt would print them identically.
func formatEngineValue(v engine.Value) string {
	switch v.Typ {
	case engine.Null:
		return "NULL"
	case engine.Int:
		return fmt.Sprintf("i:%d", v.I)
	case engine.Float:
		return fmt.Sprintf("f:%v", v.F)
	case engine.Text:
		return fmt.Sprintf("t:%s", string(v.S))
	case engine.Blob:
		return fmt.Sprintf("b:%x", v.S)
	default:
		return fmt.Sprintf("?:%v", v)
	}
}

// formatSQLValue is formatEngineValue's counterpart for a database/sql-scanned
// cgo cell (see that function's doc comment).
func formatSQLValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return fmt.Sprintf("i:%d", x)
	case float64:
		return fmt.Sprintf("f:%v", x)
	case string:
		return fmt.Sprintf("t:%s", x)
	case []byte:
		return fmt.Sprintf("b:%x", x)
	default:
		return fmt.Sprintf("?:%v", x)
	}
}

// queryEngineRows runs query against rp (this engine's own read path) and
// renders its columns/rows via formatEngineValue.
func queryEngineRows(t *testing.T, rp *engine.ReadOnlyPager, query string) (cols []string, rows [][]string) {
	t.Helper()
	c, r, err := rp.QueryArgs(query, nil)
	if err != nil {
		t.Fatalf("engine QueryArgs(%s): %v", query, err)
	}
	cols = c
	for _, row := range r {
		var out []string
		for _, v := range row {
			out = append(out, formatEngineValue(v))
		}
		rows = append(rows, out)
	}
	return cols, rows
}

// querySQLRows runs query against sdb (real, live SQLite) and renders its
// columns/rows via formatSQLValue.
func querySQLRows(t *testing.T, sdb *sql.DB, query string) (cols []string, rows [][]string) {
	t.Helper()
	r, err := sdb.Query(query)
	if err != nil {
		t.Fatalf("C SQLite Query(%s): %v", query, err)
	}
	defer r.Close()
	cols, err = r.Columns()
	if err != nil {
		t.Fatalf("C SQLite Columns(%s): %v", query, err)
	}
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatalf("C SQLite Scan(%s): %v", query, err)
		}
		var out []string
		for _, v := range vals {
			out = append(out, formatSQLValue(v))
		}
		rows = append(rows, out)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("C SQLite rows.Err(%s): %v", query, err)
	}
	return cols, rows
}

// assertPragmaEqual runs query against both rp (this engine) and sdb (real,
// live SQLite) and requires identical column names/order and identical,
// order-sensitive row content.
func assertPragmaEqual(t *testing.T, rp *engine.ReadOnlyPager, sdb *sql.DB, query string) {
	t.Helper()
	gotCols, gotRows := queryEngineRows(t, rp, query)
	wantCols, wantRows := querySQLRows(t, sdb, query)

	if len(gotCols) != len(wantCols) {
		t.Fatalf("%s: column count: got %v, want %v", query, gotCols, wantCols)
	}
	for i := range wantCols {
		if gotCols[i] != wantCols[i] {
			t.Fatalf("%s: column %d name: got %q, want %q (full: got=%v want=%v)", query, i, gotCols[i], wantCols[i], gotCols, wantCols)
		}
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("%s: row count: got %d %v, want %d %v", query, len(gotRows), gotRows, len(wantRows), wantRows)
	}
	for i := range wantRows {
		if len(gotRows[i]) != len(wantRows[i]) {
			t.Fatalf("%s: row %d width: got %v, want %v", query, i, gotRows[i], wantRows[i])
		}
		for j := range wantRows[i] {
			if gotRows[i][j] != wantRows[i][j] {
				t.Fatalf("%s: row %d col %d (%s): got %q, want %q\n  full got row:  %v\n  full want row: %v",
					query, i, j, gotCols[j], gotRows[i][j], wantRows[i][j], gotRows[i], wantRows[i])
			}
		}
	}
}

// TestPragmaMatchesCSQLite is the PRAGMA conformance gate: for a schema
// with several tables (a rowid-alias PRIMARY KEY, a UNIQUE column producing
// an automatic index, a table-level foreign key, and one with no foreign
// keys at all) plus one explicit, non-unique index, it drives the pure-Go
// engine writer AND a live real-SQLite oracle connection through the
// identical DDL script, then asserts every implemented PRAGMA answers
// byte-for-byte identically -- at both a 512- and a 4096-byte page size.
func TestPragmaMatchesCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testPragmaScenario(t, pageSize)
		})
	}
}

func testPragmaScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("pragma_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1) // a single logical connection, so every statement below lands on the SAME in-memory schema (see vdbe_drop_test.go's identical note)

	// PRAGMA page_size only takes effect on a still-empty database, so this
	// must run before any CREATE TABLE below, on BOTH sides. engine.Create used
	// to take the page size as an argument; it does not any more, and this
	// setup kept asking only the oracle.
	if _, err := sdb.Exec(fmt.Sprintf("PRAGMA page_size=%d", pageSize)); err != nil {
		t.Fatalf("C SQLite PRAGMA page_size=%d: %v", pageSize, err)
	}
	if err := db.Exec(fmt.Sprintf("PRAGMA page_size=%d", pageSize)); err != nil {
		t.Fatalf("engine PRAGMA page_size=%d: %v", pageSize, err)
	}

	ddl := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT 'x', v REAL)`,
		`CREATE TABLE u(a INTEGER UNIQUE, b INTEGER)`,
		`CREATE INDEX idx1 ON t(name)`,
		`CREATE TABLE parent(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE child(id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id))`,
		// PRAGMA index_xinfo's "coll" column: an index key's EFFECTIVE collating
		// sequence, reported with the LETTER CASE the source text used. The
		// spellings below are deliberately mixed -- "collate nocase" lower-case
		// on the column, "RtRiM" mixed on another, an explicit "COLLATE rtrim" on
		// one key and an explicit "COLLATE BINARY" overriding a NOCASE column on
		// another -- because each is reported back verbatim, and this engine used
		// to hardcode "BINARY" for every one of them. See pragmaIndexInfo's collOf.
		`CREATE TABLE coll(a, b TEXT collate nocase, c TEXT COLLATE RtRiM, d TEXT COLLATE NOCASE)`,
		`CREATE INDEX collFromCol ON coll(b)`,                          // the column's own, lower-case
		`CREATE INDEX collFromColMixed ON coll(c)`,                     // the column's own, mixed-case
		`CREATE INDEX collExplicit ON coll(b COLLATE rtrim)`,           // the key's own overrides the column's
		`CREATE INDEX collBinaryOverride ON coll(d COLLATE BINARY, a)`, // ... including back to BINARY
		`CREATE INDEX collExpr ON coll(a || 'x')`,                      // an expression key is always BINARY
		`CREATE TABLE collPK(k TEXT COLLATE NOCASE PRIMARY KEY, v)`,
		// PRAGMA table_info's declared-type and generated-column reporting.
		// Three rules, each verified against the oracle and each previously wrong
		// here -- and all three invisible, because a bare PRAGMA's rows are never
		// compared by the mined-TCL corpus:
		//
		//  1. A STANDARD type name canonicalizes to UPPER CASE -- exactly
		//     ANY/BLOB/INT/INTEGER/REAL/TEXT -- while every OTHER declared type is
		//     verbatim. "integer" -> INTEGER, but "DoUbLe" stays "DoUbLe" and
		//     "numeric" stays "numeric": NOT a blanket upper-casing.
		//  2. table_info OMITS generated columns and numbers cid over the SURVIVORS
		//     (contiguous cids); table_xinfo reports all of them and encodes the
		//     kind in "hidden": 0 ordinary, 2 VIRTUAL, 3 STORED.
		//  3. A run of whitespace inside a NON-standard declared type is preserved
		//     exactly, as is a /*comment*/ between the words.
		`CREATE TABLE tylc(a integer primary key, b int, c text, d any, e blob, f real)`,
		`CREATE TABLE tyverb(a numeric, b decimal, c boolean, d date, e DoUbLe, f varchar(10))`,
		`CREATE TABLE tyws(a  int   unsigned , b character    varying(20), c int Always, d int/*x*/unsigned)`,
		//  4. A declared type whose last six bytes are "always" (case-insensitively)
		//     AND which is at least 16 bytes long -- exactly len("GENERATED ALWAYS")
		//     -- has that suffix and the whitespace before it TRIMMED, and only
		//     then canonicalized (stripTypeTokenAlways, engine/pragma_parse.go).
		//     Each pair below differs only in its final word or by one byte of
		//     padding, which is what makes it a keyword rule rather than the
		//     length threshold it first looks like.
		`CREATE TABLE alw15(a int      Always, b text     Always, c foo      Always)`,
		`CREATE TABLE alw16(a int       Always, b text      Always, c foo       Always)`,
		`CREATE TABLE alwctl(a int       Zlways, b int       Alway, c int       Alwayss)`,
		`CREATE TABLE alwcase(a int       ALWAYS, b int       always, c int       AlWaYs)`,
		`CREATE TABLE alwmisc(a intXXXXXXXAlways, b alwaysalwaysalways, c varchar   Always, d integer   Always)`,
		`CREATE TABLE gencols(a INTEGER PRIMARY KEY, v int GENERATED ALWAYS AS (a+1), c text, s AS (a*2) STORED, d int)`,
		`CREATE TABLE genlead(v1 AS (1), a int, v2 AS (2) VIRTUAL, b int)`,
		`CREATE TABLE genonly(a int, b AS (a) STORED NOT NULL, c AS (a))`, // an automatic index inherits the column's
		// table_info over a VIEW: the view's own result columns. The TYPE rule is
		// the subtle part -- a bare column reference reports the underlying
		// column's declared type, but a column declared with NO type reports
		// "BLOB" where its own table reports ""; a CAST reports its target's
		// AFFINITY name (NUMERIC -> NUM, and note INTEGER's is "INT"); every
		// other expression is ""; and a parenthesized reference is transparent
		// while a unary "+" is not. All verified against the oracle.
		`CREATE TABLE vsrc(a integer, b TEXT, c REAL, d BLOB, e, f numeric, g varchar(9), h DoUbLe)`,
		`CREATE VIEW vw1 AS SELECT a,b,c,d,e,f,g,h FROM vsrc`,
		`CREATE VIEW vw2(p,q) AS SELECT a,b FROM vsrc`,
		`CREATE VIEW vw3 AS SELECT a+1 x, 'l' y, count(*) w FROM vsrc`,
		`CREATE VIEW vw4 AS SELECT * FROM vsrc`,
		`CREATE VIEW vw5 AS SELECT CAST(a AS NUMERIC) n1, CAST(a AS TEXT) n2, CAST(a AS REAL) n3, CAST(a AS BLOB) n4 FROM vsrc`,
		`CREATE VIEW vw6 AS SELECT (a) par, +a pa FROM vsrc`,
		`CREATE VIEW vw7 AS SELECT a AS a1, a AS a2 FROM vsrc`,
	}
	for _, s := range ddl {
		if err := db.Exec(s); err != nil {
			t.Fatalf("engine Exec(%s): %v", s, err)
		}
		if _, err := sdb.Exec(s); err != nil {
			t.Fatalf("C SQLite Exec(%s): %v", s, err)
		}
	}

	// PRAGMA user_version's setter, run on the still-open write session
	// (before Close/commit) -- see execPragma's own doc comment for why this
	// is one of the few pragma ASSIGNMENTS this engine actually applies.
	const setStmt = "PRAGMA user_version = 42"
	if err := db.Exec(setStmt); err != nil {
		t.Fatalf("engine Exec(%s): %v", setStmt, err)
	}
	if _, err := sdb.Exec(setStmt); err != nil {
		t.Fatalf("C SQLite Exec(%s): %v", setStmt, err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("engine writer Close: %v", err)
	}

	rp, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}

	for _, q := range []string{
		"PRAGMA integrity_check",
		"PRAGMA quick_check",
		"PRAGMA page_size",
		"PRAGMA user_version",
		"PRAGMA schema_version",
		"PRAGMA table_info(t)",
		"PRAGMA table_info(u)",
		"PRAGMA table_info(parent)",
		"PRAGMA table_info(child)",
		"PRAGMA table_info(nope)", // no such table: zero rows, not an error, on both sides
		"PRAGMA table_xinfo(t)",
		"PRAGMA table_xinfo(child)",
		"PRAGMA index_list(t)",
		"PRAGMA index_list(u)",
		"PRAGMA index_list(parent)", // no indexes at all: zero rows on both sides
		"PRAGMA index_info(idx1)",
		"PRAGMA index_xinfo(idx1)",
		"PRAGMA index_info(sqlite_autoindex_u_1)",
		"PRAGMA index_xinfo(sqlite_autoindex_u_1)",
		"PRAGMA foreign_key_list(t)",     // no FKs at all: zero rows on both sides
		"PRAGMA foreign_key_list(u)",     // no FKs at all: zero rows on both sides
		"PRAGMA foreign_key_list(child)", // one FK, referencing parent(id)
		"PRAGMA index_list(coll)",
		"PRAGMA index_info(collFromCol)", // no "coll" column at all: unchanged by the above
		"PRAGMA index_xinfo(collFromCol)",
		"PRAGMA index_xinfo(collFromColMixed)",
		"PRAGMA index_xinfo(collExplicit)",
		"PRAGMA index_xinfo(collBinaryOverride)",
		"PRAGMA index_xinfo(collExpr)",
		"PRAGMA index_xinfo(sqlite_autoindex_collPK_1)",
		"PRAGMA table_info(tylc)",   // standard names canonicalize to upper case
		"PRAGMA table_info(tyverb)", // non-standard names stay verbatim
		"PRAGMA table_info(tyws)",   // interior whitespace/comments preserved
		// The trailing-ALWAYS trim, at and around its 16-byte gate.
		"PRAGMA table_info(alw15)",
		"PRAGMA table_info(alw16)",
		"PRAGMA table_xinfo(alw16)",
		"PRAGMA table_info(alwctl)",
		"PRAGMA table_info(alwcase)",
		"PRAGMA table_info(alwmisc)",
		// The SCHEMA CATALOG is a legitimate target with a fixed five-column
		// shape, under every spelling; it has no schema row describing itself,
		// so this engine used to answer zero rows for all four.
		"PRAGMA table_info(sqlite_master)",
		"PRAGMA table_xinfo(sqlite_master)",
		"PRAGMA table_info(sqlite_schema)",
		"PRAGMA table_info(sqlite_temp_master)",
		"PRAGMA table_info(sqlite_temp_schema)",
		"PRAGMA table_info(gencols)",
		"PRAGMA table_xinfo(gencols)",
		"PRAGMA table_info(genlead)",
		"PRAGMA table_xinfo(genlead)",
		"PRAGMA table_info(genonly)",
		"PRAGMA table_xinfo(genonly)",
		"PRAGMA index_list(gencols)",
		"PRAGMA table_info(vw1)",
		"PRAGMA table_xinfo(vw1)",
		"PRAGMA table_info(vw2)",
		"PRAGMA table_info(vw3)",
		"PRAGMA table_info(vw4)",
		"PRAGMA table_info(vw5)",
		"PRAGMA table_info(vw6)",
		"PRAGMA table_info(vw7)",
		"PRAGMA table_info(vsrc)", // the SOURCE table: its untyped column reports "", not BLOB
		"PRAGMA index_list(vw1)",  // zero rows for a view, on both engines
		"PRAGMA foreign_key_list(vw1)",
	} {
		assertPragmaEqual(t, rp, sdb, q)
	}

	if err := rp.Close(); err != nil {
		t.Fatalf("engine ReadOnlyPager Close: %v", err)
	}

	// Reopen (a fresh *ReadOnlyPager, from scratch) and prove user_version
	// persisted across the Close+reopen round trip -- not merely visible
	// within the same session that set it.
	rp2, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open (reopen): %v", err)
	}
	defer rp2.Close()
	assertPragmaEqual(t, rp2, sdb, "PRAGMA user_version")
}
