// Differential tests of fts3/fts4 MATCH placement widening:
// MATCH constraints are passed to the virtual table when the planner
// can identify them, not just in top-level WHERE AND-conjuncts.
package compat

import "testing"

// TestFts3MatchPlacementWider tests MATCH placements in various positions.
func TestFts3MatchPlacementWider(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE vt USING fts4(x)`,
		`INSERT INTO vt(docid,x) VALUES(1,'abc')`,
		`INSERT INTO vt(docid,x) VALUES(2,'def')`,
		`CREATE TABLE tt(id)`,
		`INSERT INTO tt VALUES(1)`,
		`INSERT INTO tt VALUES(2)`,
	}
	cases := []struct {
		name string
		stmt string
	}{
		{"MATCH in a LEFT JOIN's ON clause", `SELECT id,x FROM tt LEFT JOIN vt ON vt MATCH 'abc' ORDER BY id`},
		{"two aliases of the same table, each own MATCH", `SELECT a.x,b.x FROM vt a, vt b WHERE a.x MATCH 'abc' AND b.x MATCH 'def'`},
		{"OR of two MATCHes against the same source", `SELECT docid FROM vt WHERE vt MATCH 'abc' OR vt MATCH 'def'`},
		{"already worked: MATCH AND-ed with an ordinary conjunct", `SELECT docid FROM vt WHERE vt MATCH 'abc' AND docid>0`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, append(append([]string{}, setup...), c.stmt))
		})
	}
}

// TestFts3MatchPlacementStillDeclines verifies certain MATCH placements still decline.
func TestFts3MatchPlacementStillDeclines(t *testing.T) {
	bad := []struct {
		name  string
		setup []string
		stmt  string
	}{
		// MATCH in select list still errors even if WHERE occurrence is fine.
		{"a duplicate MATCH in the select list still errors", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x)`,
			`INSERT INTO t VALUES('abc')`,
		}, `SELECT docid, t MATCH 'abc' FROM t WHERE t MATCH 'abc'`},
		// Circular non-literal cross-table MATCH dependencies cannot be ordered.
		{"a circular non-literal cross-table MATCH dependency", []string{
			`CREATE VIRTUAL TABLE ft2 USING fts4(x)`,
			`CREATE VIRTUAL TABLE ft3 USING fts4(y)`,
			`INSERT INTO ft2 VALUES('abc')`,
			`INSERT INTO ft3 VALUES('abc')`,
		}, `SELECT * FROM ft3, ft2 WHERE y MATCH x AND x MATCH y`},
	}
	for _, c := range bad {
		c := c
		t.Run(c.name, func(t *testing.T) {
			db := openMusqlFts(t)
			for _, s := range c.setup {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			rows, err := db.Query(c.stmt)
			if err == nil {
				iterErr := rows.Err()
				for rows.Next() {
				}
				if iterErr == nil {
					iterErr = rows.Err()
				}
				rows.Close()
				err = iterErr
			}
			if err == nil {
				t.Fatalf("engine ACCEPTED a MATCH placement the wider rule must still decline: %s", c.stmt)
			}
		})
	}
}

// TestFts3MatchPlacementContentOr checks content= tables correctly handle
// OR of multiple MATCH constraints on a still-indexed but missing-content docid.
func TestFts3MatchPlacementContentOr(t *testing.T) {
	differ(t, "content= table, OR of two MATCHes, a missing content row", []string{
		`CREATE TABLE src(x)`,
		`INSERT INTO src VALUES('abc')`,
		`INSERT INTO src VALUES('def')`,
		`INSERT INTO src VALUES('ghi')`,
		`CREATE VIRTUAL TABLE ct USING fts4(content="src", x)`,
		`INSERT INTO ct(docid,x) SELECT rowid,x FROM src`,
		`DELETE FROM src WHERE x='def'`,
		`SELECT docid FROM ct WHERE ct MATCH 'abc' OR ct MATCH 'def'`,
		`SELECT docid,x FROM ct WHERE ct MATCH 'abc' OR ct MATCH 'def'`,
	})
}
