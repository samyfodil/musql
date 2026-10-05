// FTS virtual tables and anchor row ordering for aggregates; scope narrowed
// to known-order tables (fts3/fts4/fts5) to unblock single-table and LEFT-JOIN cases.
package compat

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// TestFts3SingleTableAnchorUnblockedByUnrelatedIndex pins single-table FTS4 aggregate
// with GROUP BY anchor; unrelated schema indices should not affect it.
func TestFts3SingleTableAnchorUnblockedByUnrelatedIndex(t *testing.T) {
	differ(t, "fts4 single-table anchor, unrelated schema index", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(idx, value)`,
		`INSERT INTO t10 values (1, 'one'),(2, 'two'),(3, 'three')`,
		`CREATE TABLE unrelated(a INTEGER, b INTEGER)`,
		`CREATE INDEX ix_unrelated ON unrelated(a)`,
		`SELECT idx, value FROM t10 WHERE t10 MATCH 'one OR two' GROUP BY idx > 1`,
	})
}

// TestFts3TableFunctionsStayOpaque pins that the narrowing only applies to
// FTS tables, not all virtual tables; other table functions must still decline.
func TestFts3TableFunctionsStayOpaque(t *testing.T) {
	assertStaysDeclined(t,
		[]string{
			`CREATE TABLE unrelated(a INTEGER, b INTEGER)`,
			`CREATE INDEX ix_unrelated ON unrelated(a)`,
		},
		`SELECT value, value > 1 FROM json_each('[10,20,30]') GROUP BY value > 1`,
	)
}

// TestFts3LeftJoinAnchorUnblocked pins FTS4 as the LEFT-JOINED item with
// GROUP BY anchor on a grouped column observable from the FTS table.
func TestFts3LeftJoinAnchorUnblocked(t *testing.T) {
	differ(t, "fts4 LEFT JOIN target anchor, unrelated schema index", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 values ('apple'),('banana'),('cherry')`,
		`CREATE TABLE r(id INTEGER, tag INTEGER)`,
		`INSERT INTO r VALUES (1,10)`,
		`CREATE TABLE unrelated(a INTEGER, b INTEGER)`,
		`CREATE INDEX ix_unrelated ON unrelated(a)`,
		`SELECT r.tag, t10.value FROM r LEFT JOIN t10 ON t10 MATCH 'apple OR banana' GROUP BY r.tag`,
	})
}

// TestFts3CorrelatedMatchJoinForcedOrderAdversarial pins 2-item MATCH-correlated
// JOIN ordering; the two candidate nesting orders produce different anchors.
func TestFts3CorrelatedMatchJoinForcedOrderAdversarial(t *testing.T) {
	differ(t, "fts4 MATCH-correlated 2-item JOIN, adversarial anchor", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 values ('apple'),('banana'),('cherry')`,
		`SELECT x.tag, t10.value FROM t10 JOIN
                (SELECT 'banana' AS term, 1 AS tag UNION ALL SELECT 'apple', 1) AS x
              WHERE t10 MATCH x.term GROUP BY x.tag`,
	})
}

// TestFts3CorrelatedMatchJoinForcedOrderAdversarialFlipped pins the same shape
// with flipped FROM-clause order; forced nesting must not depend on spelling.
func TestFts3CorrelatedMatchJoinForcedOrderAdversarialFlipped(t *testing.T) {
	differ(t, "fts4 MATCH-correlated 2-item JOIN, flipped FROM spelling", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 values ('apple'),('banana'),('cherry')`,
		`SELECT x.tag, t10.value FROM
                (SELECT 'banana' AS term, 1 AS tag UNION ALL SELECT 'apple', 1) AS x
              JOIN t10 ON t10 MATCH x.term GROUP BY x.tag`,
	})
}

// TestFts3CorrelatedMatchExpressionForcedOrder pins forced order with MATCH
// argument as an expression, not a bare column reference.
func TestFts3CorrelatedMatchExpressionForcedOrder(t *testing.T) {
	differ(t, "fts4 MATCH correlated to an expression, not a bare column", []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
		`INSERT INTO t10 values ('apple'),('banana'),('cherry')`,
		`SELECT x.tag, t10.value FROM t10 JOIN
                (SELECT 'ban' AS term, 1 AS tag UNION ALL SELECT 'app', 1) AS x
              WHERE t10 MATCH (x.term || 'ana') GROUP BY x.tag`,
	})
}

// TestFts3CorrelatedMatchThreeItemStillDeclines pins that the narrowing does
// not generalize to 3+ items; those cases still decline.
func TestFts3CorrelatedMatchThreeItemStillDeclines(t *testing.T) {
	assertStaysDeclined(t,
		[]string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 values ('apple'),('banana'),('cherry')`,
			`CREATE TABLE y(id INTEGER, tag INTEGER)`,
			`INSERT INTO y VALUES (1,100)`,
		},
		`SELECT x.tag, y.tag, t10.value FROM t10, y,
                (SELECT 'banana' AS term, 1 AS tag UNION ALL SELECT 'apple', 1) AS x
              WHERE t10 MATCH x.term GROUP BY x.tag`,
	)
}

// TestFts3CorrelatedMatchMixedTermsStillDeclines pins mixed MATCH terms on
// the same VT; mixed correlated and literal terms still decline.
func TestFts3CorrelatedMatchMixedTermsStillDeclines(t *testing.T) {
	assertStaysDeclined(t,
		[]string{
			`CREATE VIRTUAL TABLE t10 USING fts4(value)`,
			`INSERT INTO t10 values ('apple'),('banana'),('cherry')`,
		},
		`SELECT x.tag, t10.value FROM t10 JOIN
                (SELECT 'banana' AS term, 1 AS tag UNION ALL SELECT 'apple', 1) AS x
              WHERE t10 MATCH x.term OR t10 MATCH 'cherry' GROUP BY x.tag`,
	)
}

// TestFts3AnchorOpaqueSchemaScope is a direct smoke check that the narrowing
// reads from the source's own catalog, not global schema.
func TestFts3AnchorOpaqueSchemaScope(t *testing.T) {
	cdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	ddl := []string{
		`CREATE VIRTUAL TABLE t10 USING fts4(idx, value)`,
		`INSERT INTO t10 values (1, 'one'),(2, 'two')`,
		`CREATE TABLE unrelated(a INTEGER)`,
		`CREATE INDEX ix_unrelated ON unrelated(a)`,
	}
	for _, s := range ddl {
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("oracle Exec(%s): %v", s, err)
		}
	}

	path := t.TempDir() + "/anchor_fts_opaque.sqlite"
	edb, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ddl {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine Exec(%s): %v", s, err)
		}
	}
	if err := edb.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	query := `SELECT idx, value FROM t10 WHERE t10 MATCH 'one OR two' GROUP BY idx > 1`
	if _, _, err := p.QueryArgs(query, nil); err != nil {
		t.Errorf("expected the single-table fts4 anchor to compile now, got %v", err)
	}
}
