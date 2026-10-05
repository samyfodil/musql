package driver

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

var udfCounter atomic.Int64

// Registered before any statement runs, as RegisterFunction requires.
func init() {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(RegisterFunction("udf_twice", 1, true, func(args []any) (any, error) {
		if args[0] == nil {
			return nil, nil
		}
		return args[0].(int64) * 2, nil
	}))
	must(RegisterFunction("udf_sum", -1, true, func(args []any) (any, error) {
		var s int64
		for _, a := range args {
			s += a.(int64)
		}
		return s, nil
	}))
	must(RegisterFunction("udf_counter", 0, false, func([]any) (any, error) {
		return udfCounter.Add(1), nil
	}))
	must(RegisterFunction("udf_boom", 0, true, func([]any) (any, error) {
		return nil, errors.New("boom from Go")
	}))
	must(RegisterFunction("udf_echo", 1, true, func(args []any) (any, error) { return args[0], nil }))
}

func udfDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "udf.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// TestUserFunctions: a registered function is called from SQL like a built-in
// -- arguments, NULL, any arity, every value type, its error -- and a name that
// is already a function cannot be registered again.
func TestUserFunctions(t *testing.T) {
	db := udfDB(t)
	for q, want := range map[string]string{
		`SELECT udf_twice(21)`:                              "42",
		`SELECT udf_twice(NULL) IS NULL`:                    "1",
		`SELECT udf_sum()`:                                  "0",
		`SELECT udf_sum(1, 2, 3, 4)`:                        "10",
		`SELECT typeof(udf_echo(1.5))`:                      "real",
		`SELECT udf_echo('txt') || typeof(udf_echo('txt'))`: "txttext",
		`SELECT hex(udf_echo(x'00ff'))`:                     "00FF",
		`SELECT UDF_TWICE(2)`:                               "4",
	} {
		var got string
		if err := db.QueryRow(q).Scan(&got); err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	for q, want := range map[string]string{
		`SELECT udf_twice(1, 2)`: "wrong number of arguments",
		`SELECT udf_boom()`:      "boom from Go",
	} {
		if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
	if err := RegisterFunction("abs", 1, true, func([]any) (any, error) { return nil, nil }); err == nil {
		t.Error("a built-in's name was registered")
	}
	if err := RegisterFunction("udf_twice", 1, true, func([]any) (any, error) { return nil, nil }); err == nil {
		t.Error("a registered name was registered again")
	}
}

// TestUserFunctionDeterminism: a function registered without deterministic is
// called once per row, never folded into a constant, and may not be used where
// C SQLite requires SQLITE_DETERMINISTIC (resolve.c:1219-1228); a deterministic
// one may.
func TestUserFunctionDeterminism(t *testing.T) {
	db := udfDB(t)
	rows, err := db.Query(`WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x < 3) SELECT udf_counter() FROM c`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		seen[n] = true
	}
	rows.Close()
	if len(seen) != 3 {
		t.Fatalf("a non-deterministic function gave %d distinct values over 3 rows", len(seen))
	}
	for _, s := range []string{`CREATE TABLE t(a)`, `CREATE INDEX ti ON t(udf_twice(a))`, `INSERT INTO t VALUES(5)`} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var a int64
	if err := db.QueryRow(`SELECT a FROM t WHERE udf_twice(a) = 10`).Scan(&a); err != nil || a != 5 {
		t.Fatalf("a deterministic function's index: %d %v", a, err)
	}
	if _, err := db.Exec(`CREATE INDEX tn ON t(udf_counter())`); err == nil {
		t.Fatal("a non-deterministic function was accepted in an index")
	}
	if _, err := db.Exec(`CREATE TABLE g(a, b AS (udf_counter()))`); err == nil {
		t.Fatal("a non-deterministic function was accepted in a generated column")
	}
}
