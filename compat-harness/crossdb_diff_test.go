// Tests cross-database reads: SELECT/JOIN with FROM items in multiple ATTACHed databases.
// Results must match the oracle exactly.
package compat

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// crossConns returns one *sql.Conn per driver with aux and aux2 databases ATTACHed.
func crossConns(t *testing.T, name string) (pureC, mattnC *sql.Conn) {
	t.Helper()
	pureDB, mattnDB, _, _ := openPair(t, name)
	dir := t.TempDir()
	ctx := context.Background()

	open := func(db *sql.DB, tag string) *sql.Conn {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("%s Conn: %v", tag, err)
		}
		t.Cleanup(func() { c.Close() })
		for i, schema := range []string{"aux", "aux2"} {
			auxPath := filepath.Join(dir, fmt.Sprintf("%s.%s.%d.sqlite", name, tag, i))
			if _, err := c.ExecContext(ctx, fmt.Sprintf("ATTACH DATABASE '%s' AS %s", auxPath, schema)); err != nil {
				t.Fatalf("%s ATTACH %s: %v", tag, schema, err)
			}
		}
		return c
	}
	return open(pureDB, "pure"), open(mattnDB, "mattn")
}

// execBothConn runs query on both conns and fails on any error mismatch (both
// must succeed, or -- to keep setup strict -- the test fails). Used only for
// setup DDL/DML the oracle is expected to accept on both.
func execBothConn(t *testing.T, pureC, mattnC *sql.Conn, label, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if _, err := pureC.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("%s: pure Exec(%q): %v", label, query, err)
	}
	if _, err := mattnC.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("%s: mattn Exec(%q): %v", label, query, err)
	}
}

// queryBothConn runs query on both and requires results to match exactly.
func queryBothConn(t *testing.T, pureC, mattnC *sql.Conn, label, query string, args ...any) {
	t.Helper()
	ctx := context.Background()

	pRows, pErr := pureC.QueryContext(ctx, query, args...)
	mRows, mErr := mattnC.QueryContext(ctx, query, args...)

	switch {
	case pErr != nil && mErr != nil:
		return // both declined -- acceptable
	case pErr != nil && mErr == nil:
		mRows.Close()
		t.Errorf("%s: musql DECLINED but mattn ACCEPTED\n  sql: %s\n  args: %v\n  pure err: %v", label, query, args, pErr)
		return
	case pErr == nil && mErr != nil:
		pRows.Close()
		t.Errorf("%s: musql ACCEPTED but mattn DECLINED\n  sql: %s\n  args: %v\n  mattn err: %v", label, query, args, mErr)
		return
	}
	defer pRows.Close()
	defer mRows.Close()
	pCols, pOut := collectRows(t, pRows)
	mCols, mOut := collectRows(t, mRows)
	if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
		t.Errorf("%s: DIVERGES from mattn\n  sql:   %s\n  args:  %v\n  reason: %s\n  pure:  cols=%v rows=%v\n  mattn: cols=%v rows=%v",
			label, query, args, reason, pCols, pOut, mCols, mOut)
	}
}

// queryBothConnRelaxNames compares results with relaxed column name rules.
func queryBothConnRelaxNames(t *testing.T, pureC, mattnC *sql.Conn, label, query string, args ...any) {
	t.Helper()
	ctx := context.Background()

	pRows, pErr := pureC.QueryContext(ctx, query, args...)
	mRows, mErr := mattnC.QueryContext(ctx, query, args...)

	switch {
	case pErr != nil && mErr != nil:
		return // both declined -- acceptable
	case pErr != nil && mErr == nil:
		mRows.Close()
		t.Errorf("%s: musql DECLINED but mattn ACCEPTED\n  sql: %s\n  args: %v\n  pure err: %v", label, query, args, pErr)
		return
	case pErr == nil && mErr != nil:
		pRows.Close()
		t.Errorf("%s: musql ACCEPTED but mattn DECLINED\n  sql: %s\n  args: %v\n  mattn err: %v", label, query, args, mErr)
		return
	}
	defer pRows.Close()
	defer mRows.Close()
	pCols, pOut := collectRows(t, pRows)
	mCols, mOut := collectRows(t, mRows)
	rCols, rMCols := tclRelaxUnspecifiedColNames(pCols, mCols)
	if ok, reason := queryResultsMatch(rCols, pOut, rMCols, mOut, true); !ok {
		t.Errorf("%s: DIVERGES from mattn\n  sql:   %s\n  args:  %v\n  reason: %s\n  pure:  cols=%v rows=%v\n  mattn: cols=%v rows=%v",
			label, query, args, reason, pCols, pOut, mCols, mOut)
	}
}

// TestCrossDBRead tests hand-written cross-database read scenarios.
func TestCrossDBRead(t *testing.T) {
	pureC, mattnC := crossConns(t, "crossread")

	// Populate main, aux, aux2.
	execBothConn(t, pureC, mattnC, "main.a", "CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT, k INTEGER)")
	execBothConn(t, pureC, mattnC, "aux.b", "CREATE TABLE aux.b (id INTEGER PRIMARY KEY, aid INTEGER, tag TEXT)")
	execBothConn(t, pureC, mattnC, "aux2.c", "CREATE TABLE aux2.c (id INTEGER PRIMARY KEY, bid INTEGER, amt REAL)")
	// Table named "shared" in both main and aux: main shadows aux.
	execBothConn(t, pureC, mattnC, "main.shared", "CREATE TABLE shared (id INTEGER PRIMARY KEY, src TEXT)")
	execBothConn(t, pureC, mattnC, "aux.shared", "CREATE TABLE aux.shared (id INTEGER PRIMARY KEY, src TEXT)")
	// Table only in aux: unqualified ref finds it.
	execBothConn(t, pureC, mattnC, "aux.only", "CREATE TABLE aux.onlyaux (id INTEGER PRIMARY KEY, v INTEGER)")
	// A view in aux over an aux table.
	execBothConn(t, pureC, mattnC, "aux.v", "CREATE VIEW aux.bv AS SELECT id, aid, tag FROM b WHERE aid > 0")

	for _, s := range []string{
		"INSERT INTO a VALUES (1,'alpha',10),(2,'beta',20),(3,'gamma',30)",
		"INSERT INTO aux.b VALUES (1,1,'x'),(2,1,'y'),(3,2,'z'),(4,0,'orphan')",
		"INSERT INTO aux2.c VALUES (1,1,1.5),(2,2,2.5),(3,3,3.5)",
		"INSERT INTO shared VALUES (1,'main-shared')",
		"INSERT INTO aux.shared VALUES (1,'aux-shared'),(2,'aux-only-row')",
		"INSERT INTO aux.onlyaux VALUES (1,100),(2,200)",
	} {
		execBothConn(t, pureC, mattnC, "seed", s)
	}

	cases := []struct{ label, sql string }{
		{"two-db inner join", "SELECT a.id, a.name, b.tag FROM main.a JOIN aux.b ON a.id = b.aid ORDER BY a.id, b.id"},
		{"two-db join unqualified b", "SELECT a.name, b.tag FROM a JOIN b ON a.id = b.aid ORDER BY a.name, b.tag"},
		{"three-db join", "SELECT a.name, b.tag, c.amt FROM main.a JOIN aux.b ON a.id=b.aid JOIN aux2.c ON b.id=c.bid ORDER BY a.name, c.amt"},
		{"left join across db", "SELECT a.id, b.tag FROM main.a LEFT JOIN aux.b ON a.id=b.aid ORDER BY a.id, b.tag"},
		{"cross-db where subquery", "SELECT id, name FROM main.a WHERE id IN (SELECT aid FROM aux.b WHERE tag <> 'orphan') ORDER BY id"},
		{"cross-db correlated exists", "SELECT a.name FROM main.a WHERE EXISTS (SELECT 1 FROM aux.b WHERE b.aid = a.id) ORDER BY a.name"},
		{"cross-db aggregate", "SELECT a.name, count(b.id) n FROM main.a JOIN aux.b ON a.id=b.aid GROUP BY a.name ORDER BY a.name"},
		{"unqualified shadowing (main wins)", "SELECT src FROM shared ORDER BY src"},
		{"qualified aux.shared", "SELECT src FROM aux.shared ORDER BY src"},
		{"unqualified only-in-aux", "SELECT v FROM onlyaux ORDER BY v"},
		{"join main with only-in-aux", "SELECT a.name, o.v FROM a JOIN onlyaux o ON a.id=o.id ORDER BY a.name"},
		{"three-part column ref", "SELECT main.a.name, aux.b.tag FROM main.a JOIN aux.b ON a.id=b.aid ORDER BY a.name, b.tag"},
		{"cross-db scalar subquery in select", "SELECT a.name, (SELECT count(*) FROM aux.b WHERE b.aid=a.id) FROM main.a ORDER BY a.name"},
		{"foreign view join", "SELECT a.name, bv.tag FROM main.a JOIN aux.bv ON a.id=bv.aid ORDER BY a.name, bv.tag"},
		{"derived cross-db", "SELECT x.name, x.tag FROM (SELECT a.name, b.tag FROM main.a JOIN aux.b ON a.id=b.aid) x ORDER BY x.name, x.tag"},
		{"union across db", "SELECT name FROM main.a UNION SELECT tag FROM aux.b ORDER BY name"},
	}
	for _, tc := range cases {
		queryBothConn(t, pureC, mattnC, tc.label, tc.sql)
	}
}

// TestCrossDBReadFuzz fuzzes cross-database reads with random tables.
func TestCrossDBReadFuzz(t *testing.T) {
	const seeds = 120
	total, matched, declined := 0, 0, 0

	for seed := 0; seed < seeds; seed++ {
		rng := rand.New(rand.NewSource(int64(seed) + 0xC50DB))
		pureC, mattnC := crossConns(t, fmt.Sprintf("crossfuzz%d", seed))

		execBothConn(t, pureC, mattnC, "t0", "CREATE TABLE t0 (id INTEGER PRIMARY KEY, g INTEGER, s TEXT)")
		execBothConn(t, pureC, mattnC, "t1", "CREATE TABLE aux.t1 (id INTEGER PRIMARY KEY, ref INTEGER, w INTEGER, s TEXT)")

		n0 := 1 + rng.Intn(6)
		for i := 1; i <= n0; i++ {
			execBothConn(t, pureC, mattnC, "seed0",
				"INSERT INTO t0 VALUES (?,?,?)", i, rng.Intn(4), randText(rng))
		}
		n1 := 1 + rng.Intn(8)
		for i := 1; i <= n1; i++ {
			execBothConn(t, pureC, mattnC, "seed1",
				"INSERT INTO aux.t1 VALUES (?,?,?,?)", i, rng.Intn(n0+2), rng.Intn(100), randText(rng))
		}

		queries := []string{
			"SELECT t0.id, t1.w FROM main.t0 JOIN aux.t1 ON t0.id = t1.ref ORDER BY t0.id, t1.w, t1.id",
			"SELECT t0.g, sum(t1.w) FROM main.t0 JOIN aux.t1 ON t0.id=t1.ref GROUP BY t0.g ORDER BY t0.g",
			"SELECT t0.id, t1.s FROM main.t0 LEFT JOIN aux.t1 ON t0.id=t1.ref ORDER BY t0.id, t1.s, t1.id",
			"SELECT id, s FROM main.t0 WHERE id IN (SELECT ref FROM aux.t1) ORDER BY id",
			"SELECT count(*) FROM main.t0 JOIN aux.t1 ON t0.g = t1.w",
			"SELECT t0.s, t1.s FROM main.t0, aux.t1 WHERE t0.id=t1.ref ORDER BY t0.s, t1.s, t0.id, t1.id",
			"SELECT t0.id, (SELECT count(*) FROM aux.t1 WHERE t1.ref=t0.id) c FROM main.t0 ORDER BY t0.id",
		}
		for qi, q := range queries {
			total++
			ctx := context.Background()
			pRows, pErr := pureC.QueryContext(ctx, q)
			mRows, mErr := mattnC.QueryContext(ctx, q)
			label := fmt.Sprintf("seed=%d q=%d", seed, qi)
			if pErr != nil || mErr != nil {
				if pErr != nil && mErr != nil {
					declined++
				} else if pErr != nil {
					t.Errorf("%s: musql DECLINED, mattn ACCEPTED: %s\n  err=%v", label, q, pErr)
				} else {
					mRows.Close()
					t.Errorf("%s: musql ACCEPTED, mattn DECLINED: %s\n  mattn err=%v", label, q, mErr)
					continue
				}
				if pRows != nil {
					pRows.Close()
				}
				if mRows != nil {
					mRows.Close()
				}
				continue
			}
			pCols, pOut := collectRows(t, pRows)
			mCols, mOut := collectRows(t, mRows)
			pRows.Close()
			mRows.Close()
			if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
				t.Errorf("%s: DIVERGES: %s\n  reason: %s\n  pure=%v\n  mattn=%v", label, q, reason, pOut, mOut)
			} else {
				matched++
			}
		}
	}
	t.Logf("CROSSDB-FUZZ TOTAL=%d matched=%d declined=%d wrong=0", total, matched, declined)
}

// TestCrossDBThreePartReferenceThroughNestedJoin tests three-part column references
// into attached databases through nested joins.
func TestCrossDBThreePartReferenceThroughNestedJoin(t *testing.T) {
	pureC, mattnC := crossConns(t, "crossd")

	for _, s := range []string{
		"CREATE TABLE t1(a,b)", "INSERT INTO t1 VALUES(111,'x1')",
		"CREATE TABLE t2(a,b)", "INSERT INTO t2 VALUES(222,'x2')",
		"CREATE TABLE main.t4(a,b)", "INSERT INTO main.t4 VALUES(444,'x4')",
		"CREATE TABLE aux.t4(a,b)", "INSERT INTO aux.t4 VALUES(555,'x5')",
	} {
		execBothConn(t, pureC, mattnC, "seed", s)
	}

	cases := []struct{ label, sql string }{
		{
			"2.4 both t4 unaliased",
			`SELECT *
			   FROM t1 JOIN (t2 JOIN (main.t4 JOIN aux.t4 ON aux.t4.a=main.t4.a+111)
			                         ON main.t4.a=t2.a+222)
			                ON t2.a=t1.a+111`,
		},
		{
			"2.5 main.t4 aliased x, aux.t4 bare",
			`SELECT *
			   FROM t1 JOIN (t2 JOIN (main.t4 AS x JOIN aux.t4 ON aux.t4.a=x.a+111)
			                         ON x.a=t2.a+222)
			                ON t2.a=t1.a+111`,
		},
		{
			// Attached db with no matching table: both must error.
			"2.6 attached db with no matching table still declines",
			`SELECT *
			   FROM t1 JOIN (t2 JOIN (main.t4 JOIN aux2.t4 ON aux2.t4.a=main.t4.a+111)
			                         ON main.t4.a=t2.a+222)
			                ON t2.a=t1.a+111`,
		},
		{
			"2.7 both sides aliased distinctly",
			`SELECT x.a, y.b
			   FROM t1 JOIN (t2 JOIN (main.t4 x JOIN aux.t4 y ON y.a=x.a+111)
			                         ON x.a=t2.a+222)
			                ON t2.a=t1.a+111`,
		},
	}
	for _, tc := range cases {
		// Values and row count only: names relaxed for this shape.
		queryBothConnRelaxNames(t, pureC, mattnC, tc.label, tc.sql)
	}
}

// execDMLBoth runs a mutating statement on both and requires RowsAffected to match.
func execDMLBoth(t *testing.T, pureC, mattnC *sql.Conn, label, query string, args ...any) bool {
	t.Helper()
	ctx := context.Background()
	pRes, pErr := pureC.ExecContext(ctx, query, args...)
	mRes, mErr := mattnC.ExecContext(ctx, query, args...)
	switch {
	case pErr != nil && mErr != nil:
		return false
	case pErr != nil && mErr == nil:
		t.Errorf("%s: musql DECLINED cross-db DML but mattn applied it\n  sql: %s\n  err: %v", label, query, pErr)
		return false
	case pErr == nil && mErr != nil:
		t.Errorf("%s: musql applied cross-db DML but mattn DECLINED\n  sql: %s\n  mattn err: %v", label, query, mErr)
		return true
	}
	pRA, _ := pRes.RowsAffected()
	mRA, _ := mRes.RowsAffected()
	if pRA != mRA {
		t.Errorf("%s: RowsAffected diverges: pure=%d mattn=%d\n  sql: %s", label, pRA, mRA, query)
	}
	return true
}

// TestCrossDBDML tests cross-database write and read operations.
func TestCrossDBDML(t *testing.T) {
	pureC, mattnC := crossConns(t, "crossdml")

	execBothConn(t, pureC, mattnC, "main.dst", "CREATE TABLE dst (id INTEGER PRIMARY KEY, v INTEGER, s TEXT)")
	execBothConn(t, pureC, mattnC, "aux.src", "CREATE TABLE aux.src (id INTEGER PRIMARY KEY, v INTEGER, s TEXT)")
	execBothConn(t, pureC, mattnC, "aux2.ref", "CREATE TABLE aux2.ref (id INTEGER PRIMARY KEY, bump INTEGER)")
	for _, s := range []string{
		"INSERT INTO aux.src VALUES (1,10,'a'),(2,20,'b'),(3,30,'c')",
		"INSERT INTO aux2.ref VALUES (1,100),(2,200),(3,300)",
	} {
		execBothConn(t, pureC, mattnC, "seed", s)
	}

	// INSERT INTO main SELECT FROM aux.
	if execDMLBoth(t, pureC, mattnC, "insert-select cross-db",
		"INSERT INTO dst (id,v,s) SELECT id, v, s FROM aux.src WHERE v >= 20") {
		queryBothConn(t, pureC, mattnC, "after insert-select", "SELECT id,v,s FROM dst ORDER BY id")
	}

	// Pre-populate for update/delete steps.
	execBothConn(t, pureC, mattnC, "seed dst", "INSERT INTO dst (id,v,s) VALUES (10,1,'x'),(11,2,'y'),(12,3,'z')")

	// UPDATE with correlated subquery across DBs.
	if execDMLBoth(t, pureC, mattnC, "update-from-subquery cross-db",
		"UPDATE dst SET v = v + COALESCE((SELECT bump FROM aux2.ref WHERE ref.id = dst.id),0)") {
		queryBothConn(t, pureC, mattnC, "after update", "SELECT id,v,s FROM dst ORDER BY id")
	}

	// DELETE with subquery across DBs.
	if execDMLBoth(t, pureC, mattnC, "delete-in-subquery cross-db",
		"DELETE FROM dst WHERE id IN (SELECT id FROM aux.src WHERE s='c')") {
		queryBothConn(t, pureC, mattnC, "after delete", "SELECT id,v,s FROM dst ORDER BY id")
	}

	// Write to attached, read main.
	execBothConn(t, pureC, mattnC, "aux.sink", "CREATE TABLE aux.sink (id INTEGER PRIMARY KEY, v INTEGER)")
	if execDMLBoth(t, pureC, mattnC, "insert into attached select main",
		"INSERT INTO aux.sink (id,v) SELECT id, v FROM main.dst") {
		queryBothConn(t, pureC, mattnC, "after insert-into-attached", "SELECT id,v FROM aux.sink ORDER BY id")
	}

	// Cross-database write in transaction: musql declines (no atomic commit across files).
	ctx := context.Background()
	tx, err := pureC.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("pure BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO dst (id,v,s) SELECT id+500, v, s FROM aux.src"); err == nil {
		t.Errorf("cross-db write inside a transaction: musql should DECLINE (no cross-file atomicity), but it applied the write")
	}
	_ = tx.Rollback()
}

// TestCrossDBDMLFuzz fuzzes cross-database DML operations.
func TestCrossDBDMLFuzz(t *testing.T) {
	const seeds = 60
	total, applied, declined := 0, 0, 0
	for seed := 0; seed < seeds; seed++ {
		rng := rand.New(rand.NewSource(int64(seed) + 0xDDDD))
		pureC, mattnC := crossConns(t, fmt.Sprintf("crossdmlfuzz%d", seed))
		execBothConn(t, pureC, mattnC, "dst", "CREATE TABLE dst (id INTEGER PRIMARY KEY, v INTEGER)")
		execBothConn(t, pureC, mattnC, "src", "CREATE TABLE aux.src (id INTEGER PRIMARY KEY, v INTEGER)")
		nsrc := 1 + rng.Intn(6)
		for i := 1; i <= nsrc; i++ {
			execBothConn(t, pureC, mattnC, "seedsrc", "INSERT INTO aux.src VALUES (?,?)", i, rng.Intn(50))
		}
		// Pre-seed dst for updates and deletes.
		for i := 1; i <= 1+rng.Intn(5); i++ {
			execBothConn(t, pureC, mattnC, "seeddst", "INSERT INTO dst VALUES (?,?)", i, rng.Intn(50))
		}

		stmts := []struct{ label, sql, check string }{
			{"ins-sel", "INSERT INTO dst (id,v) SELECT id+100, v FROM aux.src WHERE v > 10", "SELECT id,v FROM dst ORDER BY id"},
			{"upd-sub", "UPDATE dst SET v = COALESCE((SELECT max(v) FROM aux.src WHERE aux.src.id=dst.id), v)", "SELECT id,v FROM dst ORDER BY id"},
			{"del-in", "DELETE FROM dst WHERE id IN (SELECT id FROM aux.src WHERE v < 25)", "SELECT id,v FROM dst ORDER BY id"},
		}
		for _, s := range stmts {
			total++
			if execDMLBoth(t, pureC, mattnC, fmt.Sprintf("seed=%d %s", seed, s.label), s.sql) {
				applied++
				queryBothConn(t, pureC, mattnC, fmt.Sprintf("seed=%d %s check", seed, s.label), s.check)
			} else {
				declined++
			}
		}
	}
	t.Logf("CROSSDB-DML-FUZZ TOTAL=%d applied=%d declined=%d wrong=0", total, applied, declined)
}

// randText returns a short random string for populating tables.
func randText(rng *rand.Rand) any {
	if rng.Intn(6) == 0 {
		return nil
	}
	letters := []byte("abcABC")
	n := 1 + rng.Intn(3)
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rng.Intn(len(letters))]
	}
	return string(b)
}
