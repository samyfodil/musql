// Recursive CTE ORDER BY collation and name resolution.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var r25CTESchema = []string{
	`CREATE TABLE tree(id INTEGER PRIMARY KEY, parentid, payload)`,
	`INSERT INTO tree VALUES(1,NULL,'a'),(2,1,'B'),(3,1,'d'),(4,2,'c'),(5,3,'E'),(6,3,'f'),(7,NULL,'G'),(8,7,'h')`,
	`CREATE TABLE tc(id INTEGER PRIMARY KEY, parentid, payload TEXT COLLATE nocase)`,
	`INSERT INTO tc VALUES(1,NULL,'a'),(2,NULL,'B'),(3,1,'c'),(4,2,'d')`,
}

var r25CTEQ = []string{
	// with1.test 10.3-10.6's scan_tree: breadth/depth first, ascending and
	// descending, plus the COLLATE nocase spelling its 10.7 block adds.
	`WITH flat(fid, depth, p) AS (SELECT id, 1, '/' || payload FROM tree WHERE parentid IS NULL UNION ALL SELECT id, depth+1, p||'/'||payload FROM flat, tree WHERE parentid=fid ORDER BY 2, 3) SELECT p FROM flat`,
	`WITH flat(fid, depth, p) AS (SELECT id, 1, '/' || payload FROM tree WHERE parentid IS NULL UNION ALL SELECT id, depth+1, p||'/'||payload FROM flat, tree WHERE parentid=fid ORDER BY 2, 3 DESC) SELECT p FROM flat`,
	`WITH flat(fid, depth, p) AS (SELECT id, 1, '/' || payload FROM tree WHERE parentid IS NULL UNION ALL SELECT id, depth+1, p||'/'||payload FROM flat, tree WHERE parentid=fid ORDER BY 3) SELECT p FROM flat`,
	`WITH flat(fid, depth, p) AS (SELECT id, 1, '/' || payload FROM tree WHERE parentid IS NULL UNION ALL SELECT id, depth+1, p||'/'||payload FROM flat, tree WHERE parentid=fid ORDER BY 2, 3 COLLATE nocase) SELECT p FROM flat`,
	`WITH flat(fid, depth, p) AS (SELECT id, 1, '/' || payload FROM tree WHERE parentid IS NULL UNION ALL SELECT id, depth+1, p||'/'||payload FROM flat, tree WHERE parentid=fid ORDER BY 3 COLLATE nocase DESC) SELECT p FROM flat`,
	// The output column's DECLARED collation governs a term with no COLLATE of
	// its own, and an explicit COLLATE overrides it in either direction.
	`WITH r(fid,p) AS (SELECT id, payload FROM tc WHERE parentid IS NULL UNION ALL SELECT id, payload FROM r, tc WHERE parentid=fid ORDER BY 2) SELECT p FROM r`,
	`WITH r(fid,p) AS (SELECT id, payload FROM tc WHERE parentid IS NULL UNION ALL SELECT id, payload FROM r, tc WHERE parentid=fid ORDER BY 2 COLLATE binary) SELECT p FROM r`,
	`WITH r(fid,p) AS (SELECT id, payload FROM tc WHERE parentid IS NULL UNION ALL SELECT id, payload FROM r, tc WHERE parentid=fid ORDER BY 2 DESC) SELECT p FROM r`,
	// with1.test 10.7.2/10.7.3: the ORDER BY names an arm's ALIAS -- the FIRST
	// arm's, then the SECOND arm's.
	`WITH t(a) AS (SELECT 1 AS b UNION ALL SELECT a+1 AS c FROM t WHERE a<5 ORDER BY b) SELECT * FROM t`,
	`WITH t(a) AS (SELECT 1 AS b UNION ALL SELECT a+1 AS c FROM t WHERE a<5 ORDER BY c) SELECT * FROM t`,
	// A UNION (deduping) recursion with an ORDER BY: the queue collation and
	// the dedup collation are resolved from the same place, so both apply.
	`WITH r(p) AS (SELECT payload FROM tc UNION SELECT p||'x' FROM r WHERE length(p)<3 ORDER BY 1) SELECT p FROM r`,
}

func TestR25RecursiveCTEOrderParity(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	for _, s := range r25CTESchema {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine %s: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo %s: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range r25CTEQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		cc, cr, cerr := cgoSelect(t, cdb, q, nil)
		if cerr != nil {
			t.Errorf("[%s] the oracle rejected the statement this gate is built on: %v", q, cerr)
			continue
		}
		if eerr != nil {
			t.Errorf("[%s] engine declined a recursive CTE C SQLite answers: %v", q, eerr)
			continue
		}
		eRows := engineRowsToStrings(ev)
		cols := make([]string, len(cc))
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		// ordered=false is NOT an option here: the walk order IS the answer,
		// so the comparison is position-sensitive even though the statements
		// carry no outer ORDER BY.
		if len(eRows) != len(cr) {
			t.Errorf("[%s] row count: engine %d, cgo %d\n  engine: %v\n  cgo:    %v", q, len(eRows), len(cr), eRows, cr)
			continue
		}
		for i := range eRows {
			for j := range eRows[i] {
				if eRows[i][j] != cr[i][j] {
					t.Errorf("[%s] DIVERGES at row %d col %d: engine %q, cgo %q\n  engine: %v\n  cgo:    %v",
						q, i, j, eRows[i][j], cr[i][j], eRows, cr)
					break
				}
			}
		}
	}
}

// with1.test 10.7.1: "a" is the CTE's DECLARED column name and no arm's
// select-list name, so C SQLite rejects it ("1st ORDER BY term does not
// match any column in the result set"). Widening the name search across arms
// must not have made it resolve.
func TestR25RecursiveCTEOrderUnresolvedNameDeclines(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	const q = `WITH t(a) AS (SELECT 1 AS b UNION ALL SELECT a+1 AS c FROM t WHERE a<5 ORDER BY a) SELECT * FROM t`
	if _, _, err := p.QueryArgs(q, nil); err == nil {
		t.Errorf("[%s] answered; C SQLite rejects this ORDER BY term", q)
	}
}
