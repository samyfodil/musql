// Tests anchor-row optimization for recursive CTEs with aggregates.
package compat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestAggIdxRecCTEAnchorCorpus tests aggregate over recursive CTE with index.
func TestAggIdxRecCTEAnchorCorpus(t *testing.T) {
	flLockstep(t, "mallocA-7", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE INDEX i1 ON t1(a, b)`,
	},
		`WITH r(x,y) AS (
      SELECT 1, randomblob(100)
      UNION ALL
      SELECT x+1, randomblob(100) FROM r
      LIMIT 1000
    )
    SELECT count(x), length(y) FROM r GROUP BY (x%5)`)
}

// recCTEAnchorQueries all read the GROUP's (or the whole-table aggregate's)
// ANCHOR ROW -- a bare column beside an aggregate, or a correlated select-list
// subquery -- which is exactly what the guard protects.
var recCTEAnchorQueries = []string{
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT count(x), y FROM r GROUP BY (x%3)`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT count(x), min(y), y FROM r GROUP BY (x%3)`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT y FROM r`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT count(*), y FROM r`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT max(x), y FROM r`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT min(x), y FROM r`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT group_concat(y) FROM r`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT group_concat(y) FROM r GROUP BY (x%3)`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r LIMIT 10) SELECT x%3, group_concat(y) FROM r GROUP BY (x%3) ORDER BY 1`,
	`WITH r(x) AS (SELECT 1 UNION SELECT x+1 FROM r WHERE x<9) SELECT count(*), x FROM r GROUP BY (x%4)`,
	`WITH r(x,y) AS (SELECT 1, randomblob(4) UNION ALL SELECT x+1, randomblob(4) FROM r LIMIT 20) SELECT count(x), length(y) FROM r GROUP BY (x%5)`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r WHERE x<8) SELECT count(x), y, (SELECT count(*) FROM u WHERE u.a<x) FROM r GROUP BY (x%3)`,
	`WITH r(x,y) AS (SELECT 1, 'a' UNION ALL SELECT x+1, 'b'||x FROM r WHERE x<8) SELECT total(x), y FROM r GROUP BY (x%2)`,
	`WITH RECURSIVE r(x,y) AS (VALUES(1,'a') UNION ALL SELECT x+1, 'b'||x FROM r WHERE x<8) SELECT count(x), y FROM r GROUP BY (x%3)`,
}

func TestAggIdxRecCTEAnchorNoIndex(t *testing.T) {
	flLockstep(t, "recursive-CTE anchor, no index", []string{
		`CREATE TABLE u(a,b)`, `INSERT INTO u VALUES(3,'x'),(1,'y'),(2,'z')`,
	}, recCTEAnchorQueries...)
}

func TestAggIdxRecCTEAnchorPlainIndex(t *testing.T) {
	flLockstep(t, "recursive-CTE anchor, unrelated index", []string{
		`CREATE TABLE u(a,b)`, `INSERT INTO u VALUES(3,'x'),(1,'y'),(2,'z')`,
		`CREATE INDEX iu ON u(a)`,
	}, recCTEAnchorQueries...)
}

func TestAggIdxRecCTEAnchorUniqueIndex(t *testing.T) {
	flLockstep(t, "recursive-CTE anchor, unrelated UNIQUE index", []string{
		`CREATE TABLE u(a,b)`, `INSERT INTO u VALUES(3,'x'),(1,'y'),(2,'z')`,
		`CREATE UNIQUE INDEX iu ON u(a,b)`,
	}, recCTEAnchorQueries...)
}

// TestAggIdxRecCTEAnchorNotSelfContainedDeclines is the BOUNDARY of the
// narrowing, pinned rather than left to be rediscovered: a recursive CTE whose
// body reads a real table is NOT proven, because an index on that table really
// can change which row enters the recursion's queue first, and so which row of
// a group is its anchor. 3.53.3 answers both statements; this engine declines
// them, and that is the honest answer until the where-planner port can say
// which access path SQLite picked (anchorNoIndexInPlay's own doc comment).
//
// The second case is stricter than the order argument alone requires -- a
// scalar subquery in the body is a VALUE, not a row source, so it cannot
// reorder the queue -- and is refused anyway; see
// recursiveCTEArmSelfContained (engine/anchor_recursive_cte.go).
func TestAggIdxRecCTEAnchorNotSelfContainedDeclines(t *testing.T) {
	setup := []string{
		`CREATE TABLE u(a,b)`,
		`INSERT INTO u VALUES(3,'x'),(1,'y'),(2,'z')`,
		`CREATE INDEX iu ON u(a)`,
	}
	declines := []string{
		`WITH r(x,y) AS (SELECT a, b FROM u UNION ALL SELECT x+1, y FROM r WHERE x<9) SELECT count(x), y FROM r GROUP BY (x%3)`,
		`WITH r(x,y) AS (SELECT 1, (SELECT b FROM u LIMIT 1) UNION ALL SELECT x+1, y FROM r WHERE x<9) SELECT count(x), y FROM r GROUP BY (x%3)`,
	}
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("setup %s: %v", s, err)
		}
	}
	for _, q := range declines {
		p, perr := edb.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager: %v", perr)
		}
		_, _, qerr := p.QueryArgs(q, nil)
		p.Close()
		switch {
		case qerr == nil:
			t.Errorf("%s: answered -- if the anchor guard can now prove this shape, move it into "+
				"recCTEAnchorQueries and pin the ANSWER against the oracle instead", q)
		case !strings.Contains(qerr.Error(), "whose anchor row C SQLite"):
			t.Errorf("%s: %v, want the anchor guard's own decline -- a different error means a different gap", q, qerr)
		}
	}
}
