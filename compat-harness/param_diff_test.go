// Differential tests of bound-parameter support against C SQLite.
// Unit tests for parameter-numbering rules live in engine/param_test.go.
package compat

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
	"github.com/samyfodil/musql/engine"
)

func evInt(i int64) engine.Value     { return engine.Value{Typ: engine.Int, I: i} }
func evFloat(f float64) engine.Value { return engine.Value{Typ: engine.Float, F: f} }
func evText(s string) engine.Value   { return engine.Value{Typ: engine.Text, S: []byte(s)} }
func evBlob(b []byte) engine.Value   { return engine.Value{Typ: engine.Blob, S: b} }

var evNull = engine.Value{Typ: engine.Null}

// normalizeAny renders a value scanned from database/sql (mattn's native Go
// types) into the same storage-class-tagged string scheme
// normalizeEngineValue (pureengine_test.go) uses for an engine.Value, so the
// two sides can be compared cell-for-cell via queryResultsMatch. Mirrors
// compat-harness/worker/main.go's normalize function (that one operates on
// the same any-typed scanned values but lives in package main, under
// worker/, and can't be imported directly).
func normalizeAny(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case int64:
		return "I:" + strconv.FormatInt(x, 10)
	case float64:
		return "F:" + strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "T:" + x
	case []byte:
		if utf8.Valid(x) {
			return "T:" + string(x)
		}
		return "X:" + hex.EncodeToString(x)
	default:
		return "?:" + fmt.Sprint(v)
	}
}

// paramDiffDB builds a fresh real-SQLite (mattn) file with one small table
// (a mix of INTEGER/TEXT/NULL rows) and returns its path plus a live *sql.DB
// open on it, for the SELECT-scenario subtests below (none of which mutate
// the database, so they all safely share one build).
//
// The engine reads its OWN format (RULE #3), so the C file is imported once and
// the returned dsn is the import; paramDiffCPath recovers the C file beside it.
func paramDiffDB(t *testing.T) (dsn string, refDB *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	cPath := filepath.Join(dir, "paramdiff.sqlite")
	db, err := sql.Open("sqlite3", cPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, val INTEGER)`,
		`INSERT INTO t VALUES (1,'a',10),(2,'b',20),(3,'c',30),(4,'d',NULL)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	dsn = filepath.Join(dir, "paramdiff.musq")
	if err := sqliteconv.Import(cPath, dsn, sqliteconv.ImportOptions{}); err != nil {
		t.Fatalf("ImportSQLite: %v", err)
	}
	return dsn, db
}

// paramDiffCPath is the C file paramDiffDB imported dsn from.
func paramDiffCPath(dsn string) string {
	return strings.TrimSuffix(dsn, ".musq") + ".sqlite"
}

// compareSelectArgs runs sqlText as a SELECT through both engines with their
// respective (but value-equivalent) bound arguments and fails the test if
// the normalized results disagree.
func compareSelectArgs(t *testing.T, dsn string, refDB *sql.DB, sqlText string, engineArgs []engine.Value, mattnArgs []any) {
	t.Helper()

	pager, err := engine.Open(dsn)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer pager.Close()
	gotCols, gotVals, err := pager.QueryArgs(sqlText, engineArgs)
	if err != nil {
		t.Fatalf("engine QueryArgs(%q, %v): %v", sqlText, engineArgs, err)
	}
	gotRows := make([][]string, len(gotVals))
	for r, row := range gotVals {
		cells := make([]string, len(row))
		for c, v := range row {
			cells[c] = normalizeEngineValue(v)
		}
		gotRows[r] = cells
	}

	rows, err := refDB.Query(sqlText, mattnArgs...)
	if err != nil {
		t.Fatalf("mattn Query(%q, %v): %v", sqlText, mattnArgs, err)
	}
	defer rows.Close()
	wantCols, err := rows.Columns()
	if err != nil {
		t.Fatalf("rows.Columns: %v", err)
	}
	var wantRows [][]string
	for rows.Next() {
		cells := make([]any, len(wantCols))
		ptrs := make([]any, len(wantCols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("rows.Scan: %v", err)
		}
		norm := make([]string, len(wantCols))
		for i, c := range cells {
			norm[i] = normalizeAny(c)
		}
		wantRows = append(wantRows, norm)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	ok, reason := queryResultsMatch(gotCols, gotRows, wantCols, wantRows, false)
	if !ok {
		t.Errorf("DIVERGES from C SQLite\n  sql:    %s\n  engine args: %v\n  mattn args:  %v\n  reason: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
			sqlText, engineArgs, mattnArgs, reason, gotCols, gotRows, wantCols, wantRows)
	}
}

func TestParamSelectPositional(t *testing.T) {
	dsn, refDB := paramDiffDB(t)

	t.Run("bare ? in WHERE", func(t *testing.T) {
		compareSelectArgs(t, dsn, refDB, "SELECT name, val FROM t WHERE id = ?",
			[]engine.Value{evInt(2)}, []any{int64(2)})
	})

	t.Run("bare ? in select-list", func(t *testing.T) {
		compareSelectArgs(t, dsn, refDB, "SELECT ?, id FROM t WHERE id = 1",
			[]engine.Value{evText("hello")}, []any{"hello"})
	})

	t.Run("?NNN explicit out of order", func(t *testing.T) {
		compareSelectArgs(t, dsn, refDB, "SELECT ?2, ?1",
			[]engine.Value{evInt(10), evInt(20)}, []any{int64(10), int64(20)})
	})

	t.Run("param in IN list", func(t *testing.T) {
		compareSelectArgs(t, dsn, refDB, "SELECT id FROM t WHERE id IN (?,?) ORDER BY id",
			[]engine.Value{evInt(1), evInt(3)}, []any{int64(1), int64(3)})
	})

	t.Run("param in LIMIT/OFFSET", func(t *testing.T) {
		compareSelectArgs(t, dsn, refDB, "SELECT id FROM t ORDER BY id LIMIT ? OFFSET ?",
			[]engine.Value{evInt(2), evInt(1)}, []any{int64(2), int64(1)})
	})

	t.Run("every value type incl NULL and blob", func(t *testing.T) {
		compareSelectArgs(t, dsn, refDB, "SELECT ?, ?, ?, ?, ?",
			[]engine.Value{evNull, evInt(-7), evFloat(3.5), evText("hi"), evBlob([]byte{0xde, 0xad})},
			[]any{nil, int64(-7), float64(3.5), "hi", []byte{0xde, 0xad}})
	})

	t.Run("unbound trailing param -> NULL", func(t *testing.T) {
		// Engine side: deliberately fewer args than NumParams (2), relying on
		// the documented unbound-parameter-is-NULL fallback. Mattn side:
		// database/sql itself requires the argument COUNT to match
		// NumInput exactly (it errors "not enough args" otherwise, verified
		// directly against mattn), so an explicit nil is passed for ?2 --
		// binding NULL explicitly is exactly what an unbound parameter means
		// at the C API level too, so this is a faithful equivalence, not a
		// different scenario.
		compareSelectArgs(t, dsn, refDB, "SELECT ?1, ?2",
			[]engine.Value{evInt(7)}, []any{int64(7), nil})
	})
}

func TestParamSelectNamed(t *testing.T) {
	dsn, refDB := paramDiffDB(t)

	t.Run("repeated :name used twice, one value both positions", func(t *testing.T) {
		info, err := engine.ParseParamInfo("SELECT :x, :x")
		if err != nil {
			t.Fatalf("ParseParamInfo: %v", err)
		}
		args := engine.BindByName(info, map[string]engine.Value{":x": evInt(42)})
		compareSelectArgs(t, dsn, refDB, "SELECT :x, :x", args, []any{sql.Named("x", int64(42))})
	})

	t.Run("mixed :name, ?, @name, $name forms", func(t *testing.T) {
		sqlText := "SELECT :a, ?, @b, $c"
		info, err := engine.ParseParamInfo(sqlText)
		if err != nil {
			t.Fatalf("ParseParamInfo: %v", err)
		}
		// :a is index 1, bare ? is index 2 (one more than the max assigned
		// so far), @b is index 3, $c is index 4 -- verified by
		// engine/param_test.go's numbering-rule unit tests; build the
		// engine's positional args accordingly.
		args := engine.BindByName(info, map[string]engine.Value{
			":a": evInt(1), "@b": evInt(3), "$c": evInt(4),
		})
		args[1] = evInt(2) // the bare "?" at index 2
		compareSelectArgs(t, dsn, refDB, sqlText, args,
			[]any{sql.Named("a", int64(1)), int64(2), sql.Named("b", int64(3)), sql.Named("c", int64(4))})
	})

	t.Run(":foo and @foo are distinct parameters (not unified)", func(t *testing.T) {
		sqlText := "SELECT :foo, @foo"
		info, err := engine.ParseParamInfo(sqlText)
		if err != nil {
			t.Fatalf("ParseParamInfo: %v", err)
		}
		if info.NumParams != 2 {
			t.Fatalf("NumParams=%d, want 2 (:foo and @foo must be distinct)", info.NumParams)
		}
		// Bound positionally (not via sql.Named) on both sides here: mattn's
		// own name->parameter matching can't disambiguate two sql.Named args
		// that share the same bare name "foo" (stripped of ":"/"@") even
		// though the underlying SQLite parameters are genuinely distinct --
		// a limitation of THIS test's binding mechanism, not of SQLite's
		// parameter model itself (already proven distinct above: passing
		// only one sql.Named("foo", ...) for this 2-parameter statement
		// fails with "not enough args: want 2 got 1", which is what
		// actually establishes distinctness). Positional binding sidesteps
		// that name collision and still exercises the real question: are
		// there two independently-bound slots, read back in the right
		// column order.
		args := []engine.Value{evInt(1), evInt(2)}
		compareSelectArgs(t, dsn, refDB, sqlText, args, []any{int64(1), int64(2)})
	})
}

// copyFile copies src to a new file under t.TempDir() named base, for the
// write-path scenarios below (each engine needs its own private copy of the
// starting database to mutate).
func copyFile(t *testing.T, src, base string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), base)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", dst, err)
	}
	return dst
}

// readBackTable reads "SELECT id, name, val FROM t ORDER BY id" via mattn
// against dsn, normalizing each cell exactly like compareSelectArgs does.
func readBackViaMattn(t *testing.T, dsn string) [][]string {
	t.Helper()
	db, err := sql.Open("sqlite3", exportedForOracle(t, dsn))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT id, name, val FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("mattn readback: %v", err)
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var id int64
		var name sql.NullString
		var val sql.NullInt64
		if err := rows.Scan(&id, &name, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cell := func(ok bool, s string) string {
			if !ok {
				return "N"
			}
			return s
		}
		out = append(out, []string{
			"I:" + strconv.FormatInt(id, 10),
			cell(name.Valid, "T:"+name.String),
			cell(val.Valid, "I:"+strconv.FormatInt(val.Int64, 10)),
		})
	}
	return out
}

func readBackViaEngine(t *testing.T, dsn string) [][]string {
	t.Helper()
	pager, err := engine.Open(dsn)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer pager.Close()
	cols, rows, err := pager.Query("SELECT id, name, val FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("engine readback: %v", err)
	}
	_ = cols
	out := make([][]string, len(rows))
	for r, row := range rows {
		cells := make([]string, len(row))
		for c, v := range row {
			cells[c] = normalizeEngineValue(v)
		}
		out[r] = cells
	}
	return out
}

// TestParamWriteINSERTUPDATEDELETE drives the SAME sequence of parameterized
// INSERT/UPDATE/DELETE statements, with the SAME bound args, through real
// SQLite (mattn, on its own file copy) and through the pure engine
// (InsertArgs/UpdateArgs/DeleteArgs, on its own copy), then requires the
// final table contents to match byte-for-byte.
func TestParamWriteINSERTUPDATEDELETE(t *testing.T) {
	baseDSN, refDB := paramDiffDB(t)
	refDB.Close() // close mattn's handle on the shared base file before copying it

	mattnDSN := copyFile(t, paramDiffCPath(baseDSN), "mattn.sqlite")
	engineDSN := copyFile(t, baseDSN, "engine.musq")

	// 1. INSERT with bound values.
	mdb, err := sql.Open("sqlite3", mattnDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer mdb.Close()
	if _, err := mdb.Exec("INSERT INTO t VALUES (?, ?, ?)", int64(5), "e", int64(50)); err != nil {
		t.Fatalf("mattn INSERT: %v", err)
	}

	edb, err := engine.OpenWrite(engineDSN)
	if err != nil {
		t.Fatalf("engine.OpenWrite: %v", err)
	}
	if _, err := edb.InsertArgs("INSERT INTO t VALUES (?, ?, ?)", []engine.Value{evInt(5), evText("e"), evInt(50)}); err != nil {
		t.Fatalf("engine InsertArgs: %v", err)
	}

	// 2. UPDATE with bound values (positional ? for the new val, named :n for
	// the WHERE match -- exercises a mixed-form write statement too).
	if _, err := mdb.Exec("UPDATE t SET val = ? WHERE name = :n", int64(999), sql.Named("n", "a")); err != nil {
		t.Fatalf("mattn UPDATE: %v", err)
	}
	updInfo, err := engine.ParseParamInfo("UPDATE t SET val = ? WHERE name = :n")
	if err != nil {
		t.Fatalf("ParseParamInfo: %v", err)
	}
	updArgs := engine.BindByName(updInfo, map[string]engine.Value{":n": evText("a")})
	updArgs[0] = evInt(999) // the bare "?" (index 1)
	if _, err := edb.UpdateArgs("UPDATE t SET val = ? WHERE name = :n", updArgs); err != nil {
		t.Fatalf("engine UpdateArgs: %v", err)
	}

	// 3. DELETE with a bound value.
	if _, err := mdb.Exec("DELETE FROM t WHERE id = ?", int64(3)); err != nil {
		t.Fatalf("mattn DELETE: %v", err)
	}
	if _, err := edb.DeleteArgs("DELETE FROM t WHERE id = ?", []engine.Value{evInt(3)}); err != nil {
		t.Fatalf("engine DeleteArgs: %v", err)
	}

	if err := edb.Close(); err != nil {
		t.Fatalf("engine Close: %v", err)
	}
	mdb.Close()

	want := readBackViaMattn(t, mattnDSN)
	got := readBackViaEngine(t, engineDSN)
	if len(got) != len(want) {
		t.Fatalf("row count: engine=%d mattn=%d\n  engine=%v\n  mattn=%v", len(got), len(want), got, want)
	}
	for i := range want {
		for c := range want[i] {
			if got[i][c] != want[i][c] {
				t.Errorf("row %d col %d: engine=%q mattn=%q\n  engine=%v\n  mattn=%v", i, c, got[i][c], want[i][c], got, want)
			}
		}
	}
}
