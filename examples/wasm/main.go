//go:build js && wasm

// Command wasm is musql in a browser: a Web Worker runs this module over an
// in-memory filesystem (memfs.js) with the JIT's host imports (musql.js),
// and index.html drives it.
//
//	GOOS=js GOARCH=wasm go build -o examples/wasm/musql.wasm ./examples/wasm
//	cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" examples/wasm/
//	python3 -m http.server -d examples/wasm 8080
package main

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"syscall/js"
	"time"

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

// queryRows runs q and reads every row, which is what a timing must include.
func queryRows(q string, args ...any) ([][]any, []string, error) {
	rows, err := db.Query(q, args...)
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

// async exposes f to JS as a function returning a Promise: Go must not block
// inside a JS callback, and every file operation here does.
func async(f func(args []js.Value) (any, error)) js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.Global().Get("Promise").New(js.FuncOf(func(_ js.Value, pr []js.Value) any {
			go func() {
				v, err := f(args)
				if err != nil {
					pr[1].Invoke(js.Global().Get("Error").New(err.Error()))
					return
				}
				pr[0].Invoke(js.ValueOf(v))
			}()
			return nil
		}))
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
	api.Set("query", async(func(a []js.Value) (any, error) {
		start := time.Now()
		rows, cols, err := queryRows(a[0].String())
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
	js.Global().Set("musql", api)
	js.Global().Call("musqlReady")
	select {}
}
