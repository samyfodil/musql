//go:build js && wasm

// Command wasm is musql in a browser: a Web Worker runs this module over an
// in-memory filesystem (memfs.js) with the JIT's host imports (musql.js).
// index.html benchmarks it in three modes; race.html races it against Turso's
// browser build, with every answer cross-checked.
//
//	examples/wasm/setup.sh          # musql.wasm, wasm_exec.js, Turso into vendor/
//	go run ./examples/wasm/serve    # http://localhost:8080/race.html
package main

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"syscall/js"
	"time"
	"unicode/utf8"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/internal/jit"
)

const dbPath = "/bench.musq"

var db *sql.DB

// workloads are compat-harness/bench_columnar_vs_c_test.go's.
var workloads = []struct {
	name, sql string
	args      func(rng *rand.Rand, n int) []any
}{
	{"count(*) WHERE v > ?", "SELECT count(*) FROM t WHERE v > ?",
		func(rng *rand.Rand, n int) []any { return []any{rng.Intn(1_000_000)} }},
	{"count(*) WHERE v > ? AND k <> ?", "SELECT count(*) FROM t WHERE v > ? AND k <> ?",
		func(rng *rand.Rand, n int) []any { return []any{rng.Intn(1_000_000), rng.Intn(10)} }},
	{"rowid point lookup", "SELECT sec FROM t WHERE id = ?",
		func(rng *rand.Rand, n int) []any { return []any{1 + rng.Intn(n)} }},
	{"secondary-index eq", "SELECT count(*) FROM t WHERE sec = ?",
		func(rng *rand.Rand, n int) []any { return []any{rng.Intn(n)} }},
	{"indexed equi-join", "SELECT count(*) FROM t JOIN b ON t.bid = b.id WHERE t.sec = ?",
		func(rng *rand.Rand, n int) []any { return []any{rng.Intn(n)} }},
	{"sum over a filter", "SELECT sum(v) FROM t WHERE v > ?",
		func(rng *rand.Rand, n int) []any { return []any{rng.Intn(1_000_000)} }},
	{"aggregate GROUP BY", "SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k", nil},
	{"ORDER BY v DESC LIMIT 20", "SELECT id, v FROM t ORDER BY v DESC, id DESC LIMIT 20", nil},
	{"count(*) whole table", "SELECT count(*) FROM t", nil},
	// Not in the harness: arithmetic the VM JIT runs, over rows and in a loop.
	{"arithmetic over every row", "SELECT sum(v * 3 + k - id) FROM t", nil},
	{"recursive CTE arithmetic", "WITH RECURSIVE c(i, x) AS (SELECT 1, 0 UNION ALL SELECT i + 1, (x + i * i) % 1000003 FROM c WHERE i < 100000) SELECT max(x) FROM c", nil},
}

func open() error {
	if db != nil {
		db.Close()
	}
	stmts = map[string]*sql.Stmt{}
	var err error
	db, err = sql.Open(driver.DriverName, dbPath)
	return err
}

// seed builds the benchmark tables with n rows, then VACUUMs them into
// segments, as the harness does.
func seed(n int) (string, error) {
	start := time.Now()
	if err := open(); err != nil {
		return "", err
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS t`, `DROP TABLE IF EXISTS b`,
		`CREATE TABLE t (id INTEGER PRIMARY KEY, sec INTEGER, k INTEGER, v INTEGER, bid INTEGER, payload TEXT)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT)`,
		`CREATE INDEX idx_t_sec ON t(sec)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return "", fmt.Errorf("%s: %w", q, err)
		}
	}
	rng := rand.New(rand.NewSource(1))
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	insB, _ := tx.Prepare(`INSERT INTO b(id,label) VALUES(?,?)`)
	insT, _ := tx.Prepare(`INSERT INTO t(id,sec,k,v,bid,payload) VALUES(?,?,?,?,?,?)`)
	for i := range n {
		if _, err := insB.Exec(i, fmt.Sprintf("label-%d", i)); err != nil {
			return "", err
		}
	}
	for i := 1; i <= n; i++ {
		if _, err := insT.Exec(i, rng.Intn(n), rng.Intn(10), rng.Intn(1_000_000), 1+rng.Intn(n),
			fmt.Sprintf("row-%d-payload", i)); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if _, err := db.Exec(`VACUUM`); err != nil {
		return "", err
	}
	return fmt.Sprintf("seeded %d rows in %v", n, time.Since(start).Round(time.Millisecond)), nil
}

// stmts holds each query's prepared statement, so a repeated query -- every
// timed one -- is not prepared again per call, as a real application would
// not. Reset with the *sql.DB they belong to (open); the worker is single
// threaded.
var stmts = map[string]*sql.Stmt{}

// queryRows runs q and reads every row, which is what a timing must include.
func queryRows(q string, args ...any) ([][]any, []string, error) {
	st := stmts[q]
	if st == nil {
		var err error
		if st, err = db.Prepare(q); err != nil {
			return nil, nil, err
		}
		stmts[q] = st
	}
	rows, err := st.Query(args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
	}
	return out, cols, rows.Err()
}

// bench times every workload for about 300ms after a warm-up. The mode is
// fixed for the life of the instance (see main): the plan cache keeps the
// plan its first run compiled, so the JIT cannot be toggled per query.
func bench() ([]any, error) {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		return nil, err
	}
	var res []any
	for _, w := range workloads {
		rng := rand.New(rand.NewSource(7))
		args := func() []any {
			if w.args == nil {
				return nil
			}
			return w.args(rng, n)
		}
		for range 3 {
			if _, _, err := queryRows(w.sql, args()...); err != nil {
				return nil, fmt.Errorf("%s: %w", w.sql, err)
			}
		}
		iters, start := 0, time.Now()
		for time.Since(start) < 300*time.Millisecond {
			if _, _, err := queryRows(w.sql, args()...); err != nil {
				return nil, err
			}
			iters++
		}
		res = append(res, map[string]any{
			"name": w.name, "sql": w.sql, "iters": iters,
			"us": float64(time.Since(start).Microseconds()) / float64(iters),
		})
	}
	return res, nil
}

// jsonBuf is querySync's output buffer, kept between calls: the worker is
// single-threaded, and a buffer that has grown to the largest answer seen
// stops allocating.
var jsonBuf []byte

// appendJSONValue encodes one value queryRows produces: int64, float64,
// string, bool or nil. encoding/json's generic path was a quarter of a point
// query here.
func appendJSONValue(b []byte, v any) []byte {
	switch x := v.(type) {
	case int64:
		return strconv.AppendInt(b, x, 10)
	case float64:
		return strconv.AppendFloat(b, x, 'g', -1, 64)
	case string:
		return appendJSONString(b, x)
	case bool:
		return strconv.AppendBool(b, x)
	case nil:
		return append(b, "null"...)
	}
	return appendJSONString(b, fmt.Sprint(v))
}

// appendJSONString quotes s by JSON's rules, which are not Go's: control
// characters as \u00XX, invalid UTF-8 as U+FFFD.
func appendJSONString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < utf8.RuneSelf {
			b = append(b, c)
			i++
			continue
		}
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\t':
				b = append(b, '\\', 't')
			case '\r':
				b = append(b, '\\', 'r')
			default:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			b = append(b, "\\ufffd"...)
		} else {
			b = append(b, s[i:i+n]...)
		}
		i += n
	}
	return append(b, '"')
}

// jsArg converts one JS argument: a whole number binds as INTEGER.
func jsArg(v js.Value) any {
	switch v.Type() {
	case js.TypeNumber:
		if f := v.Float(); f == float64(int64(f)) {
			return int64(f)
		} else {
			return f
		}
	case js.TypeString:
		return v.String()
	case js.TypeBoolean:
		return v.Bool()
	}
	return nil
}

// async exposes f to JS as a function returning a Promise: Go must not block
// inside a JS callback, and every file operation here does.
func async(f func(args []js.Value) (any, error)) js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		var exec js.Func
		exec = js.FuncOf(func(_ js.Value, pr []js.Value) any {
			exec.Release() // the Promise calls its executor once
			go func() {
				v, err := f(args)
				if err != nil {
					pr[1].Invoke(js.Global().Get("Error").New(err.Error()))
					return
				}
				pr[0].Invoke(js.ValueOf(v))
			}()
			return nil
		})
		return js.Global().Get("Promise").New(exec)
	})
}

func main() {
	// MUSQL_JIT=0 is the plain VDBE: no kernels and none of the columnar
	// paths the JIT is reached through.
	if os.Getenv("MUSQL_JIT") == "0" {
		engine.Configure(engine.WithoutJIT())
	}
	api := js.Global().Get("Object").New()
	api.Set("seed", async(func(a []js.Value) (any, error) { return seed(a[0].Int()) }))
	api.Set("open", async(func(a []js.Value) (any, error) { return nil, open() }))
	api.Set("loop", async(func(a []js.Value) (any, error) {
		start := time.Now()
		for range a[1].Int() {
			if _, _, err := queryRows(a[0].String()); err != nil {
				return nil, err
			}
		}
		return float64(time.Since(start).Microseconds()) / float64(a[1].Int()), nil
	}))
	api.Set("bench", async(func(a []js.Value) (any, error) { return bench() }))
	api.Set("vector", js.ValueOf(jit.HasVector()))
	api.Set("exec", async(func(a []js.Value) (any, error) {
		if db == nil {
			if err := open(); err != nil {
				return nil, err
			}
		}
		_, err := db.Exec(a[0].String())
		return nil, err
	}))
	// query(sql, args?) runs one statement with optional bound arguments, a JS
	// array of numbers, strings or nulls.
	api.Set("query", async(func(a []js.Value) (any, error) {
		var args []any
		if len(a) > 1 && a[1].Type() == js.TypeObject {
			for i := range a[1].Length() {
				args = append(args, jsArg(a[1].Index(i)))
			}
		}
		start := time.Now()
		rows, cols, err := queryRows(a[0].String(), args...)
		if err != nil {
			return nil, err
		}
		ms := float64(time.Since(start).Microseconds()) / 1000
		jr := make([]any, len(rows))
		for i, r := range rows {
			jr[i] = r
		}
		jc := make([]any, len(cols))
		for i, c := range cols {
			jc[i] = c
		}
		return map[string]any{"columns": jc, "rows": jr, "ms": ms}, nil
	}))
	// querySync(sql, args?) answers on the calling event's own goroutine and
	// returns one JSON string, {"columns", "rows"} or {"error"}. The async API
	// above pays a new goroutine (whose stack then grows through the whole
	// engine) and a finalizer per JS value it builds, ~80us a call against a
	// ~20us point lookup. Nothing here blocks: memfs answers the fs calls
	// synchronously and the file stamp is a direct import.
	api.Set("querySync", js.FuncOf(func(_ js.Value, a []js.Value) any {
		var args []any
		if len(a) > 1 && a[1].Type() == js.TypeObject {
			for i := range a[1].Length() {
				args = append(args, jsArg(a[1].Index(i)))
			}
		}
		rows, cols, err := queryRows(a[0].String(), args...)
		jsonBuf = appendQueryJSON(jsonBuf[:0], rows, cols, err)
		return string(jsonBuf)
	}))
	js.Global().Set("musql", api)
	js.Global().Call("musqlReady")
	select {}
}

// appendQueryJSON encodes a query's answer as querySync returns it:
// {"columns", "rows"} or {"error"}.
func appendQueryJSON(b []byte, rows [][]any, cols []string, err error) []byte {
	if err != nil {
		b = appendJSONString(append(b, `{"error":`...), err.Error())
		return append(b, '}')
	}
	b = append(b, `{"columns":[`...)
	for i, c := range cols {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, c)
	}
	b = append(b, `],"rows":[`...)
	for i, r := range rows {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		for j, v := range r {
			if j > 0 {
				b = append(b, ',')
			}
			b = appendJSONValue(b, v)
		}
		b = append(b, ']')
	}
	return append(b, ']', '}')
}
