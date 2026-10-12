package engine_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// answerRows renders a query's rows as "a|b;c|d", NULL as "~", so a test can
// state the exact result C SQLite gives.
func answerRows(t *testing.T, db snapshotter, sqlText string) (string, error) {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, qerr := p.Query(sqlText)
	if qerr != nil {
		return "", qerr
	}
	var out []string
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, v := range r {
			switch v.Typ {
			case engine.Null:
				cells[i] = "~"
			case engine.Int:
				cells[i] = fmt.Sprint(v.I)
			case engine.Text:
				cells[i] = string(v.S)
			default:
				cells[i] = fmt.Sprint(v.F)
			}
		}
		out = append(out, strings.Join(cells, "|"))
	}
	return strings.Join(out, ";"), nil
}

func openAnswerDB(t *testing.T, setup ...string) snapshotter {
	t.Helper()
	db, err := engine.Create(filepath.Join(t.TempDir(), "a.musq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range setup {
		mustExec(t, db, s)
	}
	return db
}

// TestCountOfViewKeepsOuterAggregate: count(*) over a UNION ALL deletes each
// arm's result list (select.c:7137), but an aggregate written in an arm that
// names the enclosing query's column has already made THAT query an aggregate
// (resolve.c:1355-1361): one row, not one per row of t1. Every expected value
// below was read off C SQLite 3.53.3.
func TestCountOfViewKeepsOuterAggregate(t *testing.T) {
	db := openAnswerDB(t,
		"CREATE TABLE t1(c1)",
		"INSERT INTO t1 VALUES(1),(2),(3)",
	)
	for _, tc := range []struct{ sql, want string }{
		{"SELECT (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT max(c1))) FROM t1", "2"},
		{"SELECT (SELECT count(*) FROM (SELECT max(c1) UNION ALL SELECT 1)) FROM t1", "2"},
		{"SELECT (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT max(c1)) AS d) FROM t1", "2"},
		{"SELECT * FROM (SELECT (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT max(c1))) FROM t1)", "2"},
		{"SELECT (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT 2)) FROM t1", "2;2;2"},
		{"SELECT (SELECT count(*) FROM (SELECT 1 UNION SELECT max(c1))) FROM t1", "2"},
		{"SELECT (SELECT sum(x) FROM (SELECT 1 AS x UNION ALL SELECT max(c1))) FROM t1", "4"},
	} {
		got, err := answerRows(t, db, tc.sql)
		if err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.sql, got, tc.want)
		}
	}
	// INSERT ... SELECT used the wrong row count too.
	mustExec(t, db, "INSERT INTO t1 SELECT (SELECT count(*) FROM (SELECT 1 UNION ALL SELECT max(c1))) FROM t1")
	if got, _ := answerRows(t, db, "SELECT c1 FROM t1 ORDER BY 1"); got != "1;2;2;3" {
		t.Errorf("after INSERT ... SELECT: %q, want 1;2;2;3", got)
	}
}

// TestUnusedSubqueryColumnsAreNotComputed: disableUnusedSubqueryResultColumns
// (select.c:5305) turns a subquery column the enclosing query never names into
// NULL, so the error abs() raises on the smallest integer is never raised.
func TestUnusedSubqueryColumnsAreNotComputed(t *testing.T) {
	db := openAnswerDB(t,
		"CREATE TABLE t(k INT, v TEXT)",
		"INSERT INTO t VALUES(1,'a'),(2,'b')",
		"CREATE VIEW vw AS SELECT k, abs(-9223372036854775807-1) AS x FROM t",
	)
	const bad = "abs(-9223372036854775807-1)"
	for _, tc := range []struct{ sql, want string }{
		{"SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT " + bad + " FROM t)", "4"},
		{"SELECT count(*) FROM (SELECT " + bad + " AS x FROM t)", "2"},
		{"SELECT k FROM (SELECT k, " + bad + " AS x FROM t)", "1;2"},
		{"SELECT k FROM (SELECT k, " + bad + " AS x FROM t ORDER BY 1 LIMIT 5)", "1;2"},
		{"SELECT k FROM (SELECT k, " + bad + " AS x FROM t ORDER BY k)", "1;2"},
		{"SELECT k FROM (SELECT k, " + bad + " FROM t UNION ALL SELECT 3, 4)", "1;2;3"},
		{"SELECT 1 FROM (SELECT " + bad + " AS x FROM t)", "1;1"},
		{"SELECT (SELECT count(*) FROM (SELECT k FROM t UNION ALL SELECT " + bad + " FROM t))", "4"},
		{"SELECT count(*) FROM vw", "2"},
		{"SELECT k FROM vw", "1;2"},
		{"SELECT 1 WHERE EXISTS (SELECT " + bad + " FROM t)", "1"},
		{"SELECT d.k FROM (SELECT k, " + bad + " AS x FROM t) AS d JOIN t USING(k)", "1;2"},
	} {
		got, err := answerRows(t, db, tc.sql)
		if err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.sql, got, tc.want)
		}
	}
	// A column the query does read, or that C computes anyway, still raises.
	for _, sqlText := range []string{
		"SELECT x FROM (SELECT k, " + bad + " AS x FROM t)",
		"SELECT * FROM (SELECT k, " + bad + " AS x FROM t)",
		"SELECT count(*) FROM (SELECT DISTINCT k, " + bad + " AS x FROM t)",
		"SELECT count(*) FROM (SELECT k, " + bad + " AS x FROM t UNION SELECT 1, 2)",
		"SELECT count(*) FROM (SELECT max(k) AS k, " + bad + " AS x FROM t)",
		"SELECT d.k FROM (SELECT k, " + bad + " AS x FROM t) AS d WHERE x > 0",
		"SELECT k FROM (SELECT k, " + bad + " AS x FROM t ORDER BY x)",
		"SELECT x FROM vw",
	} {
		if got, err := answerRows(t, db, sqlText); err == nil {
			t.Errorf("%s = %q, want the integer overflow C raises", sqlText, got)
		}
	}
}

// TestPragmaTableInfoOfViewOverView: naming a view's columns resolves its body,
// whatever its FROM holds; an expression-named column survives being selected
// through another view.
func TestPragmaTableInfoOfViewOverView(t *testing.T) {
	db := openAnswerDB(t,
		"CREATE TABLE t1(a INT, b TEXT, e NUMERIC)",
		"CREATE VIEW v1 AS SELECT count(*) FROM t1",
		"CREATE VIEW v2 AS SELECT * FROM v1",
		"CREATE VIEW v3 AS SELECT a, b, e, a+1 FROM t1",
		"CREATE VIEW v4 AS SELECT * FROM (SELECT * FROM v3)",
		"CREATE VIEW v5 AS WITH c(q) AS (SELECT e FROM t1) SELECT q FROM c",
		"CREATE VIEW v6(p, q, r) AS SELECT count(*) FROM t1",
	)
	for _, tc := range []struct{ sql, want string }{
		{"PRAGMA table_info(v2)", "0|count(*)||0|~|0"},
		{"PRAGMA table_xinfo(v2)", "0|count(*)||0|~|0|0"},
		{"SELECT * FROM pragma_table_info('v2')", "0|count(*)||0|~|0"},
		{"PRAGMA table_info(v4)", "0|a|INT|0|~|0;1|b|TEXT|0|~|0;2|e|NUMERIC|0|~|0;3|a+1||0|~|0"},
		{"PRAGMA table_info(v5)", "0|q|NUMERIC|0|~|0"},
		{"PRAGMA table_info(v6)", "0|p||0|~|0;1|q||0|~|0;2|r||0|~|0"},
	} {
		got, err := answerRows(t, db, tc.sql)
		if err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.sql, got, tc.want)
		}
	}
}
