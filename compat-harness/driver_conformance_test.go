// Driver conformance measurement: runs database/sql programs through both engines and classifies outcomes.
//
//	  ensureFileExists before its read-only engine.Open.
//	- PANIC        musql panicked. Never acceptable; recovered here
//	  so one bad program can't take down the whole corpus run.
//
// Unlike driver_diff_test.go (which hand-writes step-by-step
// assertions for a handful of deep scenarios), this file is intentionally
// data-driven and wide: many small programs, one per row of the
// PASS/UNSUPPORTED/WRONG/PANIC tally TestDriverConformance prints at the
// end.
package compat

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// ---- outcome classification ----

type outcome int

const (
	outPass outcome = iota
	outUnsupported
	outWrong
	outPanic
)

func (o outcome) String() string {
	switch o {
	case outPass:
		return "PASS"
	case outUnsupported:
		return "UNSUPPORTED"
	case outWrong:
		return "WRONG"
	case outPanic:
		return "PANIC"
	default:
		return "?"
	}
}

// dbProgram is one corpus entry: a small database/sql program (run), tagged
// with a category (the task's broad grouping) and, for programs expected to
// land in outUnsupported, a human-readable feature name used as the
// histogram bucket key.
type dbProgram struct {
	name     string
	category string
	feature  string // only meaningful when the outcome is outUnsupported
	run      func(db *sql.DB) (string, error)
}

// conformanceTally accumulates TestDriverConformance's results across every
// program, run sequentially (no t.Parallel anywhere in this file), so a
// plain struct with no locking is sufficient.
type conformanceTally struct {
	pass, unsupported, wrong, panics int
	unsupportedByFeature             map[string]int
	wrongDetails                     []string
	panicDetails                     []string
}

func (tly *conformanceTally) record(o outcome, p dbProgram, detail string) {
	switch o {
	case outPass:
		tly.pass++
	case outUnsupported:
		tly.unsupported++
		if tly.unsupportedByFeature == nil {
			tly.unsupportedByFeature = map[string]int{}
		}
		tly.unsupportedByFeature[p.feature]++
	case outWrong:
		tly.wrong++
		tly.wrongDetails = append(tly.wrongDetails, fmt.Sprintf("%s [%s]: %s", p.name, p.category, detail))
	case outPanic:
		tly.panics++
		tly.panicDetails = append(tly.panicDetails, fmt.Sprintf("%s [%s]: %s", p.name, p.category, detail))
	}
}

func (tly *conformanceTally) report(t *testing.T) {
	t.Helper()
	total := tly.pass + tly.unsupported + tly.wrong + tly.panics
	t.Logf("==== driver conformance tally ====")
	t.Logf("total=%d  PASS=%d  UNSUPPORTED=%d  WRONG=%d  PANIC=%d", total, tly.pass, tly.unsupported, tly.wrong, tly.panics)

	type kv struct {
		feature string
		count   int
	}
	var ranked []kv
	for f, c := range tly.unsupportedByFeature {
		ranked = append(ranked, kv{f, c})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].feature < ranked[j].feature
	})
	t.Logf("---- unsupported-feature histogram (ranked) ----")
	for _, e := range ranked {
		t.Logf("  %-45s %d", e.feature, e.count)
	}

	if len(tly.wrongDetails) > 0 {
		t.Logf("---- WRONG (must be zero) ----")
		for _, d := range tly.wrongDetails {
			t.Logf("  %s", d)
		}
	}
	if len(tly.panicDetails) > 0 {
		t.Logf("---- PANIC (must be zero) ----")
		for _, d := range tly.panicDetails {
			t.Logf("  %s", d)
		}
	}
}

// ---- running one program safely against one driver ----

// runProgramSafely opens a fresh database at path via driverName, runs p
// against it, and recovers a panic into (out="", err="PANIC: ...",
// panicked=true) instead of letting it escape -- musql must never
// crash its caller, no matter how malformed or unsupported the SQL it's
// asked to run.
func runProgramSafely(driverName, path string, run func(db *sql.DB) (string, error)) (out string, err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			err = fmt.Errorf("PANIC: %v", r)
		}
	}()
	db, operr := sql.Open(driverName, path)
	if operr != nil {
		return "", operr, false
	}
	defer db.Close()
	out, err = run(db)
	return out, err, panicked
}

// runConformance is the per-program subtest body: run p against both
// drivers (each against its own fresh, empty temp file) and classify.
func runConformance(t *testing.T, p dbProgram, tly *conformanceTally) {
	t.Helper()
	dir := t.TempDir()
	purePath := filepath.Join(dir, "pure.db")
	mattnPath := filepath.Join(dir, "mattn.db")

	pureOut, pureErr, purePanic := runProgramSafely(driver.DriverName, purePath, p.run)
	mattnOut, mattnErr, mattnPanic := runProgramSafely("sqlite3", mattnPath, p.run)

	if mattnPanic {
		t.Fatalf("harness bug: mattn (the oracle) itself panicked for %q: %v", p.name, mattnErr)
	}

	switch {
	case purePanic:
		tly.record(outPanic, p, pureErr.Error())
		t.Errorf("PANIC in musql: %v", pureErr)

	case pureErr == nil && mattnErr == nil:
		if pureOut == mattnOut {
			tly.record(outPass, p, "")
		} else {
			tly.record(outWrong, p, fmt.Sprintf("both succeeded but disagree: pure=%q mattn=%q", pureOut, mattnOut))
			t.Errorf("WRONG: both succeeded but results disagree:\n  pure:  %s\n  mattn: %s", pureOut, mattnOut)
		}

	case pureErr != nil && mattnErr == nil:
		tly.record(outUnsupported, p, pureErr.Error())
		t.Logf("UNSUPPORTED (feature=%s): musql errored where mattn succeeded (%q): %v", p.feature, mattnOut, pureErr)

	case pureErr == nil && mattnErr != nil:
		tly.record(outWrong, p, fmt.Sprintf("musql succeeded (%q) where mattn errored: %v", pureOut, mattnErr))
		t.Errorf("WRONG (over-permissive): musql succeeded (%q) where mattn errored: %v", pureOut, mattnErr)

	default: // both errored: same "error category" (an error), good enough here.
		tly.record(outPass, p, "")
	}
}

// ---- shared per-program helpers ----

// scanAllRows renders every column/row of an already-executed *sql.Rows
// into one order-sensitive, storage-class-tagged string (via normalizeAny,
// param_diff_test.go) so two drivers' outputs can be compared with a plain
// == -- exactly the tagging scheme this package already uses elsewhere
// (collectRows/queryResultsMatch, driver_diff_test.go) so a TEXT '3'
// and an INTEGER 3 are never conflated even though fmt would print them
// identically.
func scanAllRows(rows *sql.Rows) (string, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(strings.Join(cols, "|"))
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		sb.WriteByte('\n')
		for i, v := range vals {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(normalizeAny(v))
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// runSetup executes each statement in setup in order, stopping (and
// returning) at the first error.
func runSetup(db *sql.DB, setup []string) error {
	for _, s := range setup {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("setup %q: %w", s, err)
		}
	}
	return nil
}

// queryProgram builds a dbProgram whose run runs setup then one query and
// renders its result set via scanAllRows -- the workhorse for the many
// programs below that are "create/seed a table, then check one SELECT".
func queryProgram(name, category, feature string, setup []string, query string, args ...any) dbProgram {
	return dbProgram{name: name, category: category, feature: feature, run: func(db *sql.DB) (string, error) {
		if err := runSetup(db, setup); err != nil {
			return "", err
		}
		rows, err := db.Query(query, args...)
		if err != nil {
			return "", err
		}
		return scanAllRows(rows)
	}}
}

// execProgram builds a dbProgram whose run runs setup then one Exec,
// rendering RowsAffected/LastInsertId -- the workhorse for programs that
// are "create/seed a table, then check one INSERT/UPDATE/DELETE/DDL
// statement's outcome" (including ones expected to error, e.g. a
// constraint violation or an unsupported construct).
func execProgram(name, category, feature string, setup []string, stmt string, args ...any) dbProgram {
	return dbProgram{name: name, category: category, feature: feature, run: func(db *sql.DB) (string, error) {
		if err := runSetup(db, setup); err != nil {
			return "", err
		}
		res, err := db.Exec(stmt, args...)
		if err != nil {
			return "", err
		}
		ra, _ := res.RowsAffected()
		li, _ := res.LastInsertId()
		return fmt.Sprintf("RA=%d LI=%d", ra, li), nil
	}}
}

// ---- the corpus ----

var conformancePrograms = buildConformancePrograms()

func buildConformancePrograms() []dbProgram {
	var ps []dbProgram
	add := func(p dbProgram) { ps = append(ps, p) }

	// ---- Basic ----
	add(queryProgram("create-table-basic", "basic", "",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)"},
		"SELECT * FROM t"))
	add(queryProgram("create-table-multiple-types", "basic", "",
		[]string{"CREATE TABLE t (a INTEGER, b TEXT, c REAL, d BLOB)"},
		"SELECT * FROM t"))
	add(queryProgram("insert-select", "basic", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)",
			"INSERT INTO t (name) VALUES ('a'), ('b'), ('c')",
		},
		"SELECT id, name FROM t ORDER BY id"))
	add(execProgram("update-where", "basic", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)",
			"INSERT INTO t (v) VALUES (1), (2), (3)",
		},
		"UPDATE t SET v = v * 10 WHERE v >= 2"))
	add(execProgram("delete-where", "basic", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)",
			"INSERT INTO t (v) VALUES (1), (2), (3)",
		},
		"DELETE FROM t WHERE v = 2"))
	add(dbProgram{name: "prepared-insert-reuse", category: "basic", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)"); err != nil {
			return "", err
		}
		stmt, err := db.Prepare("INSERT INTO t (v) VALUES (?)")
		if err != nil {
			return "", err
		}
		defer stmt.Close()
		var sb strings.Builder
		for _, v := range []int{10, 20, 30} {
			res, err := stmt.Exec(v)
			if err != nil {
				return "", err
			}
			ra, _ := res.RowsAffected()
			li, _ := res.LastInsertId()
			fmt.Fprintf(&sb, "RA=%d,LI=%d;", ra, li)
		}
		rows, err := db.Query("SELECT id, v FROM t ORDER BY id")
		if err != nil {
			return "", err
		}
		out, err := scanAllRows(rows)
		if err != nil {
			return "", err
		}
		sb.WriteString(out)
		return sb.String(), nil
	}})
	add(queryProgram("positional-params", "basic", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)",
			"INSERT INTO t (v) VALUES (1), (2), (3)",
		},
		"SELECT v FROM t WHERE v > ? ORDER BY v", 1))
	add(execProgram("named-params", "basic", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, val REAL)",
			"INSERT INTO t (name, val) VALUES ('a', 1.0)",
		},
		"UPDATE t SET val = :v WHERE name = :n", sql.Named("v", 9.5), sql.Named("n", "a")))
	add(queryProgram("multi-row-scan-iterate", "basic", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)",
			"INSERT INTO t (v) VALUES (1), (2), (3), (4), (5)",
		},
		"SELECT v FROM t ORDER BY id"))
	add(execProgram("rows-affected-multi-update", "basic", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)",
			"INSERT INTO t (v) VALUES (1), (2), (3), (4)",
		},
		"UPDATE t SET v = 0 WHERE v > 1"))
	add(execProgram("last-insert-id-after-multi-insert", "basic", "",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)"},
		"INSERT INTO t (v) VALUES (1), (2), (3)"))
	add(execProgram("create-table-if-not-exists-rerun", "basic", "CREATE TABLE IF NOT EXISTS re-run (existing table)",
		[]string{"CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY)"},
		"CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY)"))

	// ---- Types ----
	add(queryProgram("integer-roundtrip", "types", "",
		[]string{"CREATE TABLE t (v INTEGER)", "INSERT INTO t VALUES (42)"}, "SELECT v FROM t"))
	add(queryProgram("text-roundtrip", "types", "",
		[]string{"CREATE TABLE t (v TEXT)", "INSERT INTO t VALUES ('hello world')"}, "SELECT v FROM t"))
	add(queryProgram("real-roundtrip", "types", "",
		[]string{"CREATE TABLE t (v REAL)", "INSERT INTO t VALUES (3.14159)"}, "SELECT v FROM t"))
	add(queryProgram("blob-roundtrip", "types", "",
		[]string{"CREATE TABLE t (v BLOB)"}, "SELECT ? AS v", []byte{0xDE, 0xAD, 0xBE, 0xEF}))
	add(queryProgram("null-roundtrip", "types", "",
		[]string{"CREATE TABLE t (v INTEGER)", "INSERT INTO t VALUES (NULL)"}, "SELECT v FROM t"))
	add(dbProgram{name: "sql-nullstring-valid", category: "types", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (v TEXT)"); err != nil {
			return "", err
		}
		if _, err := db.Exec("INSERT INTO t VALUES ('present')"); err != nil {
			return "", err
		}
		var n sql.NullString
		if err := db.QueryRow("SELECT v FROM t").Scan(&n); err != nil {
			return "", err
		}
		return fmt.Sprintf("valid=%v value=%q", n.Valid, n.String), nil
	}})
	add(dbProgram{name: "sql-nullstring-null", category: "types", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (v TEXT)"); err != nil {
			return "", err
		}
		if _, err := db.Exec("INSERT INTO t VALUES (NULL)"); err != nil {
			return "", err
		}
		var n sql.NullString
		if err := db.QueryRow("SELECT v FROM t").Scan(&n); err != nil {
			return "", err
		}
		return fmt.Sprintf("valid=%v", n.Valid), nil
	}})
	add(dbProgram{name: "sql-nullint64", category: "types", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (v INTEGER)"); err != nil {
			return "", err
		}
		if _, err := db.Exec("INSERT INTO t VALUES (7)"); err != nil {
			return "", err
		}
		var n sql.NullInt64
		if err := db.QueryRow("SELECT v FROM t").Scan(&n); err != nil {
			return "", err
		}
		return fmt.Sprintf("valid=%v value=%d", n.Valid, n.Int64), nil
	}})
	add(dbProgram{name: "sql-nullfloat64", category: "types", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (v REAL)"); err != nil {
			return "", err
		}
		if _, err := db.Exec("INSERT INTO t VALUES (NULL)"); err != nil {
			return "", err
		}
		var n sql.NullFloat64
		if err := db.QueryRow("SELECT v FROM t").Scan(&n); err != nil {
			return "", err
		}
		return fmt.Sprintf("valid=%v", n.Valid), nil
	}})
	add(queryProgram("big-int-maxint64", "types", "",
		nil, "SELECT ?", int64(9223372036854775807)))
	add(queryProgram("big-int-minint64", "types", "",
		nil, "SELECT ?", int64(-9223372036854775808)))
	add(queryProgram("unicode-text", "types", "",
		[]string{"CREATE TABLE t (v TEXT)"}, "SELECT ?", "héllo wörld 日本語 🎉"))
	add(queryProgram("empty-string-vs-null", "types", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t (v) VALUES (''), (NULL)",
		},
		"SELECT id, v, v IS NULL, v = '' FROM t ORDER BY id"))
	add(queryProgram("negative-numbers", "types", "",
		nil, "SELECT ?, ?", -42, -3.5))

	// ---- Query features ----
	joinSetup := []string{
		"CREATE TABLE u (id INTEGER PRIMARY KEY, name TEXT)",
		"CREATE TABLE o (uid INTEGER, amt INTEGER)",
		"INSERT INTO u VALUES (1,'a'),(2,'b'),(3,'c')",
		"INSERT INTO o VALUES (1,10),(1,20),(2,5)",
	}
	add(queryProgram("inner-join", "query-features", "",
		joinSetup, "SELECT u.name, o.amt FROM u JOIN o ON o.uid = u.id ORDER BY u.name, o.amt"))
	add(queryProgram("left-outer-join", "query-features", "",
		joinSetup, "SELECT u.name, o.amt FROM u LEFT JOIN o ON o.uid = u.id ORDER BY u.name, o.amt"))
	add(queryProgram("cross-join-comma", "query-features", "",
		joinSetup, "SELECT u.name, o.amt FROM u, o WHERE o.uid = u.id ORDER BY u.name, o.amt"))
	add(queryProgram("natural-join", "query-features", "NATURAL JOIN",
		joinSetup, "SELECT * FROM u NATURAL JOIN o"))
	add(queryProgram("using-clause", "query-features", "JOIN ... USING(...)",
		[]string{
			"CREATE TABLE u2 (id INTEGER PRIMARY KEY, name TEXT)",
			"CREATE TABLE o2 (id INTEGER, amt INTEGER)",
			"INSERT INTO u2 VALUES (1,'a')",
			"INSERT INTO o2 VALUES (1,10)",
		},
		"SELECT * FROM u2 JOIN o2 USING(id)"))
	add(queryProgram("right-join", "query-features", "RIGHT JOIN",
		joinSetup, "SELECT u.name, o.amt FROM u RIGHT JOIN o ON o.uid = u.id"))
	add(queryProgram("full-outer-join", "query-features", "FULL OUTER JOIN",
		joinSetup, "SELECT u.name, o.amt FROM u FULL OUTER JOIN o ON o.uid = u.id"))

	groupSetup := []string{
		"CREATE TABLE s (g TEXT, v INTEGER)",
		"INSERT INTO s VALUES ('a',1),('a',2),('b',10),('b',NULL),('c',5)",
	}
	add(queryProgram("group-by-basic", "query-features", "",
		groupSetup, "SELECT g, count(*) FROM s GROUP BY g ORDER BY g"))
	add(queryProgram("group-by-having", "query-features", "",
		groupSetup, "SELECT g, sum(v) FROM s GROUP BY g HAVING sum(v) > 3 ORDER BY g"))
	add(queryProgram("aggregate-count-sum-avg-min-max", "query-features", "",
		groupSetup, "SELECT count(*), count(v), sum(v), avg(v), min(v), max(v) FROM s"))
	add(queryProgram("aggregate-group-concat", "query-features", "",
		groupSetup, "SELECT g, group_concat(v, '-') FROM s GROUP BY g ORDER BY g"))
	add(queryProgram("distinct", "query-features", "",
		[]string{"CREATE TABLE p (v INTEGER)", "INSERT INTO p VALUES (3),(1),(2),(3),(1)"},
		"SELECT DISTINCT v FROM p ORDER BY v"))
	add(queryProgram("order-by-limit-offset", "query-features", "",
		[]string{"CREATE TABLE p (v INTEGER)", "INSERT INTO p VALUES (5),(3),(1),(4),(2)"},
		"SELECT v FROM p ORDER BY v DESC LIMIT 2 OFFSET 1"))

	compoundSetup := []string{
		"CREATE TABLE c1 (v INTEGER)", "CREATE TABLE c2 (v INTEGER)",
		"INSERT INTO c1 VALUES (1),(2),(3)",
		"INSERT INTO c2 VALUES (2),(3),(4)",
	}
	add(queryProgram("union", "query-features", "",
		compoundSetup, "SELECT v FROM c1 UNION SELECT v FROM c2 ORDER BY v"))
	add(queryProgram("union-all", "query-features", "",
		compoundSetup, "SELECT v FROM c1 UNION ALL SELECT v FROM c2 ORDER BY v"))
	add(queryProgram("intersect", "query-features", "",
		compoundSetup, "SELECT v FROM c1 INTERSECT SELECT v FROM c2 ORDER BY v"))
	add(queryProgram("except", "query-features", "",
		compoundSetup, "SELECT v FROM c1 EXCEPT SELECT v FROM c2 ORDER BY v"))

	add(queryProgram("subquery-scalar", "query-features", "",
		joinSetup, "SELECT name, (SELECT sum(amt) FROM o WHERE o.uid = u.id) FROM u ORDER BY name"))
	add(queryProgram("subquery-in", "query-features", "",
		joinSetup, "SELECT name FROM u WHERE id IN (SELECT uid FROM o WHERE amt > 8) ORDER BY name"))
	add(queryProgram("subquery-exists", "query-features", "",
		joinSetup, "SELECT name FROM u WHERE EXISTS (SELECT 1 FROM o WHERE o.uid = u.id) ORDER BY name"))
	add(queryProgram("case-when", "query-features", "",
		[]string{"CREATE TABLE p (v INTEGER)", "INSERT INTO p VALUES (1),(5),(10)"},
		"SELECT v, CASE WHEN v < 3 THEN 'lo' WHEN v < 8 THEN 'mid' ELSE 'hi' END FROM p ORDER BY v"))
	add(queryProgram("in-list", "query-features", "",
		[]string{"CREATE TABLE p (v INTEGER)", "INSERT INTO p VALUES (1),(2),(3),(4)"},
		"SELECT v FROM p WHERE v IN (2,4) ORDER BY v"))
	add(queryProgram("like-operator", "query-features", "",
		[]string{"CREATE TABLE p (v TEXT)", "INSERT INTO p VALUES ('apple'),('banana'),('avocado')"},
		"SELECT v FROM p WHERE v LIKE 'a%' ORDER BY v"))
	add(queryProgram("glob-operator", "query-features", "GLOB operator",
		[]string{"CREATE TABLE p (v TEXT)", "INSERT INTO p VALUES ('apple'),('banana')"},
		"SELECT v FROM p WHERE v GLOB 'a*'"))
	add(queryProgram("between", "query-features", "",
		[]string{"CREATE TABLE p (v INTEGER)", "INSERT INTO p VALUES (1),(5),(10)"},
		"SELECT v FROM p WHERE v BETWEEN 2 AND 9"))
	add(queryProgram("cast", "query-features", "",
		nil, "SELECT CAST('123abc' AS INTEGER), CAST(3.99 AS INTEGER), CAST(65 AS TEXT)"))

	funcOK := func(name, expr string) {
		add(queryProgram("func-"+name, "query-features", "", nil, "SELECT "+expr))
	}
	funcOK("length", "length('hello')")
	funcOK("substr", "substr('hello',2,3)")
	funcOK("abs", "abs(-5)")
	funcOK("coalesce", "coalesce(NULL, NULL, 3)")
	funcOK("upper-lower", "upper('AbC'), lower('AbC')")
	funcOK("typeof", "typeof(1), typeof(1.0), typeof('x'), typeof(NULL)")
	funcOK("hex", "hex(x'deadbeef')")

	funcUnsupported := func(name, feature, expr string) {
		add(queryProgram("func-"+name, "query-features", feature, nil, "SELECT "+expr))
	}
	funcUnsupported("round", "round() scalar function", "round(2.5)")
	funcUnsupported("trim", "trim() scalar function", "trim('  x  ')")
	funcUnsupported("replace", "replace() scalar function", "replace('aaa','a','b')")
	funcUnsupported("instr", "instr() scalar function", "instr('abcabc','bc')")
	funcUnsupported("printf", "printf() scalar function", "printf('%d-%s', 1, 'x')")
	funcUnsupported("char", "char() scalar function", "char(65,66,67)")
	funcUnsupported("quote", "quote() scalar function", "quote('a''b')")
	// random() is nondeterministic -- comparing its value against the oracle
	// can never match. Check it is callable and yields an integer instead.
	funcUnsupported("random", "random() scalar function", "typeof(random())")
	funcUnsupported("zeroblob", "zeroblob() scalar function", "zeroblob(4)")
	funcUnsupported("date", "date()/datetime() functions", "date('2024-02-29')")
	funcUnsupported("datetime", "date()/datetime() functions", "datetime('2024-01-01 12:00:00','+1 day')")
	funcUnsupported("strftime", "strftime() function", "strftime('%Y-%m','2024-06-15')")
	funcUnsupported("julianday", "julianday() function", "julianday('2000-01-01')")
	funcUnsupported("json-extract", "json1 (json_extract)", "json_extract('{\"a\":1}', '$.a')")

	// ---- Transactions ----
	add(dbProgram{name: "tx-commit", category: "transactions", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (v INTEGER)"); err != nil {
			return "", err
		}
		tx, err := db.Begin()
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		rows, err := db.Query("SELECT v FROM t")
		if err != nil {
			return "", err
		}
		return scanAllRows(rows)
	}})
	add(dbProgram{name: "tx-rollback", category: "transactions", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (v INTEGER)"); err != nil {
			return "", err
		}
		if _, err := db.Exec("INSERT INTO t VALUES (1)"); err != nil {
			return "", err
		}
		tx, err := db.Begin()
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec("INSERT INTO t VALUES (2)"); err != nil {
			return "", err
		}
		if err := tx.Rollback(); err != nil {
			return "", err
		}
		rows, err := db.Query("SELECT v FROM t ORDER BY v")
		if err != nil {
			return "", err
		}
		return scanAllRows(rows)
	}})
	add(dbProgram{name: "tx-read-your-writes", category: "transactions", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (v INTEGER)"); err != nil {
			return "", err
		}
		tx, err := db.Begin()
		if err != nil {
			return "", err
		}
		defer tx.Rollback()
		if _, err := tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
			return "", err
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
			return "", err
		}
		return fmt.Sprintf("in-tx-count=%d", count), nil
	}})
	add(dbProgram{name: "tx-error-then-rollback", category: "transactions", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)"); err != nil {
			return "", err
		}
		if _, err := db.Exec("INSERT INTO t VALUES (1, 100)"); err != nil {
			return "", err
		}
		tx, err := db.Begin()
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec("INSERT INTO t VALUES (2, 200)"); err != nil {
			return "", err
		}
		// A duplicate PK conflict: the application's usual pattern is to
		// roll back on any error from within the transaction.
		_, insErr := tx.Exec("INSERT INTO t VALUES (1, 999)")
		if insErr == nil {
			tx.Rollback()
			return "", fmt.Errorf("expected a PK conflict error, got none")
		}
		if err := tx.Rollback(); err != nil {
			return "", err
		}
		rows, err := db.Query("SELECT id, v FROM t ORDER BY id")
		if err != nil {
			return "", err
		}
		return scanAllRows(rows)
	}})
	add(execProgram("savepoint", "transactions", "SAVEPOINT / nested transactions",
		[]string{"CREATE TABLE t (v INTEGER)", "BEGIN"},
		"SAVEPOINT sp1"))
	add(dbProgram{name: "nested-begin-in-tx", category: "transactions", feature: "nested transactions (BEGIN while a transaction is open)", run: func(db *sql.DB) (string, error) {
		if _, err := db.Exec("CREATE TABLE t (v INTEGER)"); err != nil {
			return "", err
		}
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		tx1, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		defer tx1.Rollback()
		_, err = conn.BeginTx(ctx, nil) // a second BEGIN on the SAME conn while one is open
		if err != nil {
			return "", err
		}
		return "nested BEGIN succeeded", nil
	}})

	// ---- Schema ----
	add(execProgram("create-index-basic", "schema", "",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)"},
		"CREATE INDEX idx_v ON t(v)"))
	add(execProgram("create-unique-index", "schema", "",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)"},
		"CREATE UNIQUE INDEX idx_v ON t(v)"))
	add(execProgram("drop-index", "schema", "",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)", "CREATE INDEX idx_v ON t(v)"},
		"DROP INDEX idx_v"))
	add(execProgram("pk-duplicate-rejected", "schema", "",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES (1,'a')"},
		"INSERT INTO t VALUES (1,'b')"))
	add(execProgram("unique-duplicate-rejected", "schema", "",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, u TEXT UNIQUE)",
			"INSERT INTO t VALUES (1,'a')",
		},
		"INSERT INTO t VALUES (2,'a')"))
	add(execProgram("expression-index", "schema", "expression indexes",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"},
		"CREATE INDEX idx_upper ON t(upper(v))"))
	add(execProgram("partial-index-where", "schema", "partial indexes (WHERE)",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)"},
		"CREATE INDEX idx_v ON t(v) WHERE v > 0"))
	add(execProgram("alter-table-add-column", "schema", "ALTER TABLE ADD COLUMN",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY)"},
		"ALTER TABLE t ADD COLUMN v TEXT"))
	add(execProgram("alter-table-rename", "schema", "ALTER TABLE RENAME",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY)"},
		"ALTER TABLE t RENAME TO t2"))

	// ---- Common unsupported / constrained constructs ----
	add(dbProgram{name: "multi-statement-exec", category: "unsupported-common", feature: "multi-statement Exec (\";\"-separated script)", run: func(db *sql.DB) (string, error) {
		res, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY); INSERT INTO t VALUES (1);")
		if err != nil {
			return "", err
		}
		ra, _ := res.RowsAffected()
		return fmt.Sprintf("RA=%d", ra), nil
	}})
	add(queryProgram("cte-with", "unsupported-common", "CTE (WITH)",
		[]string{"CREATE TABLE t (v INTEGER)", "INSERT INTO t VALUES (1),(2),(3)"},
		"WITH c AS (SELECT v FROM t WHERE v > 1) SELECT * FROM c"))
	add(execProgram("view-create", "unsupported-common", "VIEW",
		[]string{"CREATE TABLE t (v INTEGER)"},
		"CREATE VIEW v1 AS SELECT v FROM t"))
	add(execProgram("trigger-create", "unsupported-common", "TRIGGER",
		[]string{"CREATE TABLE t (v INTEGER)", "CREATE TABLE log (msg TEXT)"},
		"CREATE TRIGGER trg AFTER INSERT ON t BEGIN INSERT INTO log VALUES ('fired'); END"))
	add(queryProgram("window-function-over", "unsupported-common", "window functions (OVER)",
		[]string{"CREATE TABLE t (v INTEGER)", "INSERT INTO t VALUES (1),(2),(3)"},
		"SELECT v, row_number() OVER (ORDER BY v) FROM t"))
	add(execProgram("upsert-on-conflict", "unsupported-common", "UPSERT (ON CONFLICT)",
		[]string{
			"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)",
			"INSERT INTO t VALUES (1, 10)",
		},
		"INSERT INTO t VALUES (1, 20) ON CONFLICT(id) DO UPDATE SET v = 20"))
	add(execProgram("returning-clause", "unsupported-common", "RETURNING",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)"},
		"INSERT INTO t (v) VALUES (5) RETURNING id"))
	add(execProgram("pragma-foreign-keys", "unsupported-common", "PRAGMA (foreign_keys)",
		nil, "PRAGMA foreign_keys = ON"))
	add(queryProgram("pragma-user-version", "unsupported-common", "PRAGMA (user_version)",
		nil, "PRAGMA user_version"))
	add(queryProgram("pragma-table-info", "unsupported-common", "PRAGMA (table_info)",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"},
		"PRAGMA table_info(t)"))
	add(execProgram("autoincrement-keyword", "unsupported-common", "AUTOINCREMENT keyword",
		nil, "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)"))
	add(execProgram("check-constraint", "unsupported-common", "CHECK column constraint",
		nil, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER CHECK(v > 0))"))
	add(execProgram("default-value-omitted", "unsupported-common", "DEFAULT column values",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER DEFAULT 7)"},
		"INSERT INTO t (id) VALUES (1)"))
	add(execProgram("not-null-violation", "unsupported-common", "",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, nn TEXT NOT NULL)"},
		"INSERT INTO t (id) VALUES (1)"))
	add(queryProgram("collate-nocase", "unsupported-common", "COLLATE (expression-level)",
		[]string{"CREATE TABLE t (v TEXT)", "INSERT INTO t VALUES ('ABC'),('abc')"},
		"SELECT v FROM t ORDER BY v COLLATE NOCASE"))
	add(execProgram("foreign-key-unenforced-by-default", "unsupported-common", "",
		[]string{
			"CREATE TABLE parent (id INTEGER PRIMARY KEY)",
			"CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id))",
		},
		"INSERT INTO child VALUES (1, 999)"))
	add(execProgram("replace-into", "unsupported-common", "REPLACE INTO",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES (1,'a')"},
		"REPLACE INTO t VALUES (1,'b')"))
	add(execProgram("insert-or-ignore", "unsupported-common", "INSERT OR IGNORE",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES (1,'a')"},
		"INSERT OR IGNORE INTO t VALUES (1,'b')"))
	add(execProgram("insert-or-replace", "unsupported-common", "INSERT OR REPLACE",
		[]string{"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)", "INSERT INTO t VALUES (1,'a')"},
		"INSERT OR REPLACE INTO t VALUES (1,'b')"))
	add(execProgram("insert-into-select", "unsupported-common", "INSERT INTO ... SELECT",
		[]string{
			"CREATE TABLE src (v INTEGER)", "INSERT INTO src VALUES (1),(2)",
			"CREATE TABLE dst (v INTEGER)",
		},
		"INSERT INTO dst SELECT v FROM src"))
	add(execProgram("without-rowid", "unsupported-common", "WITHOUT ROWID tables",
		nil, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT) WITHOUT ROWID"))
	add(execProgram("strict-table", "unsupported-common", "STRICT tables",
		nil, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT) STRICT"))

	return ps
}

func TestDriverConformance(t *testing.T) {
	tly := &conformanceTally{}
	for _, p := range conformancePrograms {
		p := p
		t.Run(p.category+"/"+p.name, func(t *testing.T) {
			runConformance(t, p, tly)
		})
	}
	tly.report(t)
	if tly.wrong != 0 || tly.panics != 0 {
		t.Errorf("driver conformance corpus found %d WRONG and %d PANIC outcome(s) -- see log above; every one must be fixed or downgraded to a clean UNSUPPORTED rejection", tly.wrong, tly.panics)
	}
}
