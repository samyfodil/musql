package compat

// This file tests result-set-alias rules in WHERE/ON/GROUP BY/HAVING clauses
// against C SQLite.

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// aliasSchema is the shared fixture for all tests.
var aliasSchema = []string{
	"CREATE TABLE t1(a INTEGER, b INTEGER, c TEXT)",
	"INSERT INTO t1 VALUES(1,10,'x'),(2,20,'y'),(3,30,'z'),(4,20,'X'),(5,NULL,NULL)",
	"CREATE TABLE t2(a INTEGER, d INTEGER)",
	"INSERT INTO t2 VALUES(1,100),(2,200),(9,900)",
}

// aliasPair opens one fresh Go engine DB and one fresh cgo connection, applies
// aliasSchema to both, and returns them.
func aliasPair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	godb, err := engine.Create(filepath.Join(t.TempDir(), "go.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { godb.Discard() })
	cgodb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "cgo.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	t.Cleanup(func() { cgodb.Close() })
	for _, s := range aliasSchema {
		if execErr, panicked, panicVal := tclSafeExecArgs(godb, s); panicked {
			t.Fatalf("PANIC on fixture %q: %v", s, panicVal)
		} else if execErr != nil {
			t.Fatalf("engine fixture %q: %v", s, execErr)
		}
		if _, err := cgodb.Exec(s); err != nil {
			t.Fatalf("cgo fixture %q: %v", s, err)
		}
	}
	return godb, cgodb
}

// aliasCompare runs stmt against both engines and reports whether the Go engine
// answered it, checking order-sensitivity only when ORDER BY is present.
func aliasCompare(t *testing.T, godb *engine.Session, cgodb *sql.DB, stmt string) bool {
	t.Helper()
	gotCols, gotRows, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
	if panicked {
		t.Fatalf("PANIC evaluating %q: %v", stmt, panicVal)
	}
	wantCols, wantRows, werr := tclRunCGOQuery(cgodb, stmt)
	if qerr != nil {
		return false
	}
	if werr != nil {
		t.Errorf("WRONG (engine accepted a SELECT C SQLite rejects)\n  stmt: %s\n  cgo error: %v\n  engine: cols=%v rows=%v", stmt, werr, gotCols, gotRows)
		return true
	}
	ordered := strings.Contains(strings.ToUpper(stmt), "ORDER BY")
	if ok, reason := queryResultsMatch(gotCols, gotRows, wantCols, wantRows, ordered); !ok {
		t.Errorf("MISMATCH: %s\n  stmt: %s\n  engine: cols=%v rows=%v\n  cgo:    cols=%v rows=%v",
			reason, stmt, gotCols, gotRows, wantCols, wantRows)
	}
	return true
}

// TestResultAliasMatchesCSQLite tests the alias rule across WHERE, ON, GROUP BY,
// and HAVING clauses.
func TestResultAliasMatchesCSQLite(t *testing.T) {
	godb, cgodb := aliasPair(t)
	for _, stmt := range []string{
		// WHERE, with the alias buried under every operator the substitution
		// pass walks.
		"SELECT a AS q FROM t1 WHERE q>1",
		"SELECT a+b AS s FROM t1 WHERE s>15",
		"SELECT a AS q FROM t1 WHERE Q>1", // alias match is case-insensitive
		`SELECT a AS "my col" FROM t1 WHERE "my col">1`,
		"SELECT a AS q, b AS r FROM t1 WHERE r=q*10",
		"SELECT a AS q FROM t1 WHERE -q < -1",
		"SELECT a AS q FROM t1 WHERE +q>1",
		"SELECT a AS q FROM t1 WHERE NOT q>1",
		"SELECT a AS q FROM t1 WHERE q BETWEEN 2 AND 3",
		"SELECT a AS q FROM t1 WHERE q IN (1,2)",
		"SELECT a AS q FROM t1 WHERE q IS NULL",
		"SELECT a AS q FROM t1 WHERE q IS NOT 1",
		"SELECT b AS q FROM t1 WHERE q NOT NULL",
		"SELECT c AS q FROM t1 WHERE q LIKE 'y'",
		"SELECT c AS q FROM t1 WHERE q GLOB 'x*'",
		"SELECT c AS q FROM t1 WHERE q='y' COLLATE NOCASE",
		"SELECT a AS q FROM t1 WHERE CAST(q AS TEXT)='2'",
		"SELECT abs(-a) AS q FROM t1 WHERE q>1 AND q<3",
		"SELECT a AS q FROM t1 WHERE CASE WHEN q>1 THEN 1 ELSE 0 END",
		"SELECT a AS q FROM t1 WHERE q IN (SELECT a FROM t2)",
		"SELECT a AS q FROM t1 WHERE q COLLATE NOCASE > 1",
		"SELECT t1.a AS q FROM t1 JOIN t2 ON q=t2.a",
		"SELECT t1.a AS q FROM t1 LEFT JOIN t2 ON q=t2.a",
		"SELECT t1.b AS q FROM t1 LEFT JOIN t2 ON t2.a=t1.a WHERE q>10",
		"SELECT a AS q FROM t1 GROUP BY q",
		"SELECT a+b AS s, count(*) FROM t1 GROUP BY s",
		"SELECT a AS q FROM t1 GROUP BY q, b HAVING q>1",
		"SELECT count(*) AS n FROM t1 GROUP BY b HAVING n>0",
		"SELECT sum(b) AS s FROM t1 GROUP BY a HAVING s>15",
		"SELECT b AS q, sum(a) AS s FROM t1 GROUP BY q HAVING s>1 AND q<25",
		"SELECT sum(a) AS s FROM t1 GROUP BY b HAVING s IN (1,2,3)",
		"SELECT a AS q FROM t1 GROUP BY 1 HAVING q>1",
		"SELECT a AS q FROM t1 WHERE q>1 GROUP BY q HAVING q<4 ORDER BY q",
		"SELECT a+0 AS a FROM t1 WHERE a>1",
		"SELECT b AS a FROM t1 WHERE a>2",
		"SELECT t1.a AS d FROM t1, t2 WHERE d>100",
		"SELECT a AS rowid FROM t1 WHERE rowid>1",
		"SELECT b AS oid FROM t1 WHERE oid>15",
		"SELECT (SELECT b+100 AS q FROM t1 AS inner1 WHERE q>101 LIMIT 1) FROM t2 AS q",
		"SELECT 0 AS true FROM t1 WHERE true",
		"SELECT a AS q, b AS q FROM t1 WHERE q>1",
		"SELECT random() AS r FROM t1 WHERE r=r",
		"SELECT 1 AS one, 2 AS two WHERE one<two",
		"SELECT 1 AS x, 2 AS x WHERE x=2",
	} {
		if !aliasCompare(t, godb, cgodb, stmt) {
			t.Errorf("engine DECLINED a statement this gate requires it to answer: %s", stmt)
		}
	}
}

// TestResultAliasStaysRejected verifies that invalid alias references
// (aggregates outside HAVING, qualified aliases) are properly rejected.
func TestResultAliasStaysRejected(t *testing.T) {
	godb, cgodb := aliasPair(t)
	for _, stmt := range []string{
		"SELECT count(*) AS n FROM t1 WHERE n>0",       // misuse of aggregate
		"SELECT sum(b) AS s FROM t1 GROUP BY s",        // aggregate in GROUP BY
		"SELECT sum(b) OVER () AS w FROM t1 WHERE w>0", // window in WHERE
		"SELECT max(b) OVER () AS w FROM t1 GROUP BY a HAVING w>0",
		"SELECT a AS q FROM t1 WHERE t1.q>1",      // a qualified alias is not a name
		"SELECT a AS q, q AS r FROM t1 WHERE r>1", // an alias may not reference an alias
		"SELECT nosuch AS q FROM t1 WHERE q>1",    // the alias's own expression must resolve
	} {
		if _, _, werr := tclRunCGOQuery(cgodb, stmt); werr == nil {
			t.Fatalf("fixture drift: C SQLite ACCEPTS %q -- this case belongs in the matching test", stmt)
		}
		_, _, qerr, panicked, panicVal := tclSafeGoQuery(godb, stmt)
		if panicked {
			t.Fatalf("PANIC evaluating %q: %v", stmt, panicVal)
		}
		if qerr == nil {
			t.Errorf("engine ACCEPTED a statement C SQLite rejects: %s", stmt)
		}
	}
}

// TestResultAliasFuzzDifferential generates random alias-referencing SELECTs
// and verifies the engine matches C SQLite behavior.
func TestResultAliasFuzzDifferential(t *testing.T) {
	godb, cgodb := aliasPair(t)
	rng := rand.New(rand.NewSource(20260728))

	names := []string{"q", "s", "a", "b", "c", "d", "rowid", "x"}
	exprs := []string{"a", "b", "a+b", "a*2", "abs(-a)", "b-a", "c", "a+0", "length(c)"}
	preds := []string{"%s>1", "%s>=2", "%s<>2", "%s IS NULL", "%s NOT NULL", "%s IN (1,2,20)",
		"%s BETWEEN 1 AND 20", "-%s<0", "NOT %s>2", "CAST(%s AS TEXT)='2'", "%s COLLATE NOCASE>1"}
	joinFroms := []string{"t1", "t1, t2", "t1 JOIN t2 ON t1.a=t2.a", "t1 LEFT JOIN t2 ON t1.a=t2.a"}

	answered, oracleOK := 0, 0
	const n = 400
	for i := 0; i < n; i++ {
		name := names[rng.Intn(len(names))]
		expr := exprs[rng.Intn(len(exprs))]
		pred := fmt.Sprintf(preds[rng.Intn(len(preds))], name)
		from := joinFroms[rng.Intn(len(joinFroms))]

		var stmt string
		switch rng.Intn(4) {
		case 0:
			stmt = fmt.Sprintf("SELECT %s AS %s FROM %s WHERE %s", expr, name, from, pred)
		case 1:
			stmt = fmt.Sprintf("SELECT %s AS %s FROM t1 GROUP BY %s", expr, name, name)
		case 2:
			stmt = fmt.Sprintf("SELECT %s AS %s, count(*) AS cnt FROM t1 GROUP BY %s HAVING cnt>0 AND %s",
				expr, name, name, pred)
		case 3:
			stmt = fmt.Sprintf("SELECT %s AS %s FROM %s WHERE %s ORDER BY 1", expr, name, from, pred)
		}
		if _, _, werr := tclRunCGOQuery(cgodb, stmt); werr != nil {
			continue
		}
		oracleOK++
		if aliasCompare(t, godb, cgodb, stmt) {
			answered++
		}
	}
	if oracleOK < n/2 || answered*10 < oracleOK*8 {
		t.Fatalf("engine answered only %d of the %d generated statements C SQLite accepts (of %d generated): this gate is not exercising the rule", answered, oracleOK, n)
	}
	t.Logf("engine answered %d of the %d generated alias statements C SQLite accepts (of %d generated)", answered, oracleOK, n)
}
