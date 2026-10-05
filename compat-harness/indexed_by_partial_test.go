package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// INDEXED BY with a partial index should only use the index if the WHERE
// condition is compatible with the index's partial condition.
// ibpSetup creates partial and full indexes for testing this behavior.
var ibpSetup = []string{
	`CREATE TABLE t(a,b)`,
	`CREATE INDEX px ON t(a) WHERE a>10`,
	`CREATE INDEX fx ON t(b)`,
	`INSERT INTO t VALUES(1,'x'),(20,'y')`,
	`CREATE TABLE u(k)`,
	`INSERT INTO u VALUES(20),(1)`,
	`CREATE TABLE trg(a,b)`,
	`CREATE INDEX trgx ON trg(a) WHERE a>10`,
	`INSERT INTO trg VALUES(1,'x'),(20,'y')`,
	`CREATE TABLE lg(m)`,
	`CREATE TRIGGER trgd AFTER DELETE ON trg BEGIN INSERT INTO lg VALUES(old.a); END`,
}

// TestIndexedByPartialNoQuerySolution verifies that partial index constraints
// are enforced and error messages match when no valid query solution exists.
func TestIndexedByPartialNoQuerySolution(t *testing.T) {
	for _, q := range []string{
		`SELECT a FROM t INDEXED BY px WHERE a=1`,
		// sqlite3ExprImpliesExpr is exact-match plus a few rules: "a>20" does
		// not imply "a>10", nor do IN, BETWEEN or a "+a" operand.
		`SELECT a FROM t INDEXED BY px WHERE a>20`,
		`SELECT a FROM t INDEXED BY px WHERE a IN (20, 30)`,
		`SELECT a FROM t INDEXED BY px WHERE a BETWEEN 11 AND 30`,
		`SELECT a FROM t INDEXED BY px WHERE +a>10`,
		`SELECT a FROM t INDEXED BY px WHERE likely(a>20)`,
		`SELECT a FROM t INDEXED BY px WHERE a>10 OR a>20`,
		`SELECT a FROM t INDEXED BY px WHERE b LIKE 'y%'`,
		`SELECT a FROM t INDEXED BY px WHERE a=1 AND 0`,
		`SELECT a FROM t INDEXED BY px`,
		// No fake rowid index in the chain, and whereShortCut is skipped.
		`SELECT a FROM t INDEXED BY px WHERE rowid=1`,
		// Aggregates that DO reach sqlite3WhereBegin: isSimpleCount wants
		// count(*) alone, no WHERE and no HAVING.
		`SELECT count(*) FROM t INDEXED BY px WHERE a=1`,
		`SELECT count(a) FROM t INDEXED BY px`,
		`SELECT count(*) FROM t INDEXED BY px HAVING count(*)>0`,
		`SELECT count(*) FROM t INDEXED BY px GROUP BY b`,
		`SELECT max(a) FROM t INDEXED BY px`,
		`SELECT a FROM t INDEXED BY px WHERE a=1 GROUP BY a`,
		`SELECT a, row_number() OVER () FROM t INDEXED BY px WHERE a=1`,
		// Joins, whose ON terms join the clause (an OUTER join's only its own).
		`SELECT a, k FROM u, t INDEXED BY px WHERE a=k`,
		`SELECT a, k FROM u LEFT JOIN t INDEXED BY px ON a=k AND a>15`,
		`SELECT a, k FROM u LEFT JOIN t INDEXED BY px ON a=k WHERE a>15`,
		// Expression subqueries are never flattened.
		`SELECT (SELECT a FROM t INDEXED BY px WHERE a=1)`,
		`SELECT (SELECT a FROM t INDEXED BY px WHERE a=u.k AND a>11) FROM u`,
		`SELECT EXISTS (SELECT 1 FROM t INDEXED BY px WHERE a=1)`,
		`SELECT 1 WHERE 20 IN (SELECT a FROM t INDEXED BY px WHERE a=1)`,
		`SELECT a FROM t INDEXED BY px WHERE a=1 UNION SELECT 5`,
		// Name resolution runs first: C reports the column, not the plan.
		`SELECT nosuch FROM t INDEXED BY px WHERE a=1`,
		// Writes: update.c always plans its scan; delete.c truncates instead
		// only with no WHERE, trigger, RETURNING or required foreign key.
		`UPDATE t INDEXED BY px SET b='z'`,
		`UPDATE t INDEXED BY px SET b='w' WHERE a>11`,
		`DELETE FROM t INDEXED BY px WHERE a=1`,
		`DELETE FROM t INDEXED BY px WHERE b LIKE 'y%' RETURNING a`,
		`DELETE FROM t INDEXED BY px RETURNING a`,
		`DELETE FROM trg INDEXED BY trgx`,
		`INSERT INTO u SELECT a FROM t INDEXED BY px`,
	} {
		t.Run(q, func(t *testing.T) {
			var msg, after [2]string
			for i, drv := range []string{"sqlite3", "sqlite"} {
				db, err := sql.Open(drv, filepath.Join(t.TempDir(), "x.db"))
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for _, s := range ibpSetup {
					if _, err := db.Exec(s); err != nil {
						t.Fatalf("%s setup %s: %v", drv, s, err)
					}
				}
				if e := ibpRun(db, q); e != nil {
					msg[i] = e.Error()
				}
				after[i] = ibpDump(t, db)
				db.Close()
			}
			if msg[0] == "" {
				t.Fatalf("the oracle did not reject %q -- the case no longer tests an error", q)
			}
			if msg[0] != msg[1] {
				t.Errorf("%s\n  cgo: %q\n  mus: %q", q, msg[0], msg[1])
			}
			if after[0] != after[1] {
				t.Errorf("%s left different tables\n  cgo: %s\n  mus: %s", q, after[0], after[1])
			}
		})
	}
}

// TestIndexedByPartialAnswered is the other side of the rule, run through the
// differ: a hint the WHERE implies, a full index, and every statement C codes
// without sqlite3WhereBegin.
func TestIndexedByPartialAnswered(t *testing.T) {
	for i, q := range [][]string{
		{`SELECT a FROM t INDEXED BY px WHERE a>10`},
		{`SELECT a FROM t INDEXED BY px WHERE 10<a`},
		{`SELECT a FROM t INDEXED BY px WHERE a>10 AND a=20`},
		{`SELECT a FROM t INDEXED BY px WHERE a>10 AND b IS NOT NULL`},
		{`SELECT a FROM t AS x INDEXED BY px WHERE x.a>10`},
		{`SELECT a FROM t INDEXED BY px WHERE a>10 ORDER BY b`},
		{`SELECT count(*) FROM t INDEXED BY px WHERE a>10`},
		{`SELECT a, k FROM u LEFT JOIN t INDEXED BY px ON a=k AND a>10`},
		{`SELECT a, k FROM u, t INDEXED BY px WHERE a=k AND a>10`},
		{`SELECT (SELECT a FROM t INDEXED BY px WHERE a=u.k AND a>10) FROM u`},
		// isSimpleCount (select.c:5441): OP_Count, no sqlite3WhereBegin.
		{`SELECT count(*) FROM t INDEXED BY px`},
		{`SELECT DISTINCT count(*) FROM t INDEXED BY px`},
		{`SELECT count(*) FROM t INDEXED BY px LIMIT 1`},
		{`SELECT a FROM t INDEXED BY px WHERE a>10 UNION ALL SELECT count(*) FROM t INDEXED BY px`},
		// A full index, and a subquery body whose own WHERE implies.
		{`SELECT b FROM t INDEXED BY fx WHERE a=1`},
		{`SELECT b FROM t INDEXED BY fx`},
		{`SELECT * FROM (SELECT a FROM t INDEXED BY px WHERE a>10) WHERE a=20`},
		// A CTE nothing reads is never coded.
		{`WITH c AS (SELECT a FROM t INDEXED BY px WHERE a=1) SELECT 1`},
		// Writes the WHERE implies, and the truncate (delete.c:471): the
		// where-less DELETE never calls sqlite3WhereBegin.
		{`UPDATE t INDEXED BY px SET b='w' WHERE a>10`, `SELECT a,b FROM t ORDER BY a`},
		{`DELETE FROM t INDEXED BY px WHERE a>10`, `SELECT a,b FROM t ORDER BY a`},
		{`DELETE FROM t INDEXED BY px`, `SELECT a,b FROM t ORDER BY a`},
		{`INSERT INTO u SELECT a FROM t INDEXED BY px WHERE a>10`, `SELECT k FROM u ORDER BY k`},
		// ...and with foreign keys on, over a table no foreign key names.
		{`PRAGMA foreign_keys=ON`, `DELETE FROM t INDEXED BY px`, `SELECT count(*) FROM t`},
	} {
		differ(t, fmt.Sprintf("answered %d", i), append(append([]string{}, ibpSetup...), q...))
	}
	// A child table under foreign_keys=ON: sqlite3FkRequired, so no truncate.
	fk := []string{
		`CREATE TABLE p(id INTEGER PRIMARY KEY)`, `CREATE TABLE c(x REFERENCES p(id), y)`,
		`CREATE INDEX cx ON c(x) WHERE x>5`, `INSERT INTO p VALUES(1),(9)`, `INSERT INTO c VALUES(1,1),(9,2)`,
		`PRAGMA foreign_keys=ON`,
	}
	differ(t, "fk child delete", append(append([]string{}, fk...), `DELETE FROM c INDEXED BY cx`, `SELECT count(*) FROM c`))
	differ(t, "fk child delete implied", append(append([]string{}, fk...), `DELETE FROM c INDEXED BY cx WHERE x>5`, `SELECT x FROM c`))
}

// TestIndexedByPartialFromBodyAnswered: a view, CTE or FROM-subquery body whose
// own WHERE leaves the hint no loop, which C answers because it flattens the
// body into (or pushes the outer WHERE down into) the query that reads it.
// These declined until the flattening was ported (flatten_projection.go,
// pushdown_where.go); now the body is planned as C plans it, and the answer is
// compared.
func TestIndexedByPartialFromBodyAnswered(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM (SELECT a FROM t INDEXED BY px) WHERE a>10`,
		`SELECT count(*) FROM (SELECT a FROM t INDEXED BY px)`,
		`WITH c AS (SELECT a FROM t INDEXED BY px) SELECT * FROM c WHERE a>10`,
		`CREATE VIEW v AS SELECT a FROM t INDEXED BY px; SELECT * FROM v WHERE a>10`,
	} {
		differ(t, q, append(append([]string{}, ibpSetup...), strings.Split(q, "; ")...))
	}
}

// TestIndexedByXferCopyAnswered: "INSERT INTO t2 SELECT * FROM t" that C
// answers by xferOptimization (insert.c:3012) reads t's b-tree in ROWID order
// and never plans, so an INDEXED BY hint neither refuses it -- a partial index
// that leaves no loop, an index that does not exist -- nor orders it. The hint
// used to decline the first two and, over an ordinary index, hand the new rows
// their rowids in INDEX order: a wrong answer.
func TestIndexedByXferCopyAnswered(t *testing.T) {
	base := []string{
		"CREATE TABLE t(a, b)",
		"CREATE INDEX tb ON t(b)",
		"CREATE INDEX px ON t(a) WHERE a > 100",
		"INSERT INTO t VALUES(1,'z'),(2,'y'),(3,'x'),(4,'w')",
		"CREATE TABLE t2(a, b)",
	}
	for _, q := range []string{
		"INSERT INTO t2 SELECT * FROM t INDEXED BY tb",
		"INSERT INTO t2 SELECT * FROM t INDEXED BY px",
		"INSERT INTO t2 SELECT * FROM t INDEXED BY nosuch",
		"INSERT INTO t2 SELECT * FROM t",
		// No copy: a WITH on the INSERT (insert.c:3037), so the hint plans.
		"WITH c AS (SELECT 1) INSERT INTO t2 SELECT * FROM t INDEXED BY px",
		"WITH c AS (SELECT 1) INSERT INTO t2 SELECT * FROM t INDEXED BY tb",
	} {
		differ(t, q, append(append([]string{}, base...), q, "SELECT rowid, a, b FROM t2 ORDER BY rowid"))
	}
}

// TestIndexedByXferCopyUnprovenDeclines pins the shapes C decides at RUN time:
// with an index on the destination or an OR IGNORE, the copy happens only if the
// destination is empty (emptyDestTest, insert.c:3248-3271), and its rowid order
// differs from the hinted scan's. The narrowed eligibility gate cannot prove
// either way, so the statement declines rather than guess. The premise is
// checked against the oracle, so a change in C's answer is re-measured.
func TestIndexedByXferCopyUnprovenDeclines(t *testing.T) {
	for _, q := range []string{
		"CREATE INDEX t2a ON t2(a); INSERT INTO t2 SELECT * FROM t INDEXED BY tb",
		"INSERT OR IGNORE INTO t2 SELECT * FROM t INDEXED BY tb",
	} {
		stmts := append([]string{
			"CREATE TABLE t(a, b)", "CREATE INDEX tb ON t(b)",
			"INSERT INTO t VALUES(1,'z'),(2,'y'),(3,'x'),(4,'w')", "CREATE TABLE t2(a, b)",
		}, strings.Split(q, "; ")...)
		if got := run(t, "cgo", stmts)[len(stmts)-1]; got["kind"] != "rows" {
			t.Fatalf("%q: premise gone: oracle answered %v", q, got)
		}
		if got := run(t, "musql", stmts)[len(stmts)-1]["kind"]; got != "error" {
			t.Errorf("%q: expected a decline, got kind=%v", q, got)
		}
	}
}

// ibpRun executes one statement the way the harness worker does -- Query
// first, Exec for a statement that returns no rows.
func ibpRun(db *sql.DB, stmt string) error {
	rs, err := db.Query(stmt)
	if err != nil {
		_, eerr := db.Exec(stmt)
		return eerr
	}
	for rs.Next() {
	}
	rs.Close()
	return rs.Err()
}

// ibpDump renders every table the setup creates, in rowid order.
func ibpDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var sb strings.Builder
	for _, tbl := range []string{"t", "u", "trg", "lg"} {
		rs, err := db.Query(`SELECT * FROM ` + tbl + ` ORDER BY rowid`)
		if err != nil {
			t.Fatalf("read back %s: %v", tbl, err)
		}
		cols, cerr := rs.Columns()
		if cerr != nil {
			t.Fatal(cerr)
		}
		sb.WriteString(tbl + ":")
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b) // the two drivers differ only in how TEXT arrives
				}
			}
			fmt.Fprintf(&sb, "%v;", vals)
		}
		rs.Close()
	}
	return sb.String()
}
