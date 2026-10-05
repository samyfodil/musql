package compat

// Gate for truly nested parenthesized joins. Compares row values against C SQLite
// across different page sizes. Column names are left uncompared due to nondeterministic naming.

import (
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// nestedParenValuesCase is one test case with a SQL statement.
type nestedParenValuesCase struct {
	name string
	sql  string
}

var nestedParenValuesCorpus = []nestedParenValuesCase{
	{
		name: "inner-only, 3 levels deep",
		sql: `SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4
		      FROM rf1 JOIN (rf2 JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k
		      ORDER BY rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	},
	{
		name: "outer LEFT wrapping a 2-deep nested group",
		sql: `SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4
		      FROM rf1 LEFT JOIN (rf2 JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k
		      ORDER BY rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	},
	{
		// Duplicate column names in nested join.
		name: "duplicate bare column name (k) across a nested group",
		sql: `SELECT rf1.k, rf2.k, rf3.k, rf4.k
		      FROM rf1 JOIN (rf2 JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k
		      ORDER BY rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	},
	{
		// Single outer RIGHT wrapping nested inner joins.
		name: "single outer RIGHT wrapping a 2-deep nested (all-INNER) group",
		sql: `SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4
		      FROM rf1 RIGHT JOIN (rf2 JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k
		      ORDER BY coalesce(rf1.k, rf2.k, rf3.k, rf4.k), rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	},
	{
		// Single outer FULL with nested left join.
		name: "single outer FULL wrapping a 2-deep nested group (LEFT internal)",
		sql: `SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4
		      FROM rf1 FULL JOIN (rf2 LEFT JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k
		      ORDER BY coalesce(rf1.k, rf2.k, rf3.k, rf4.k), rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	},
	{
		// Outer INNER with right-joined deepest member.
		name: "outer INNER wrapping a 2-deep nested group with a RIGHT innermost connector",
		sql: `SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4
		      FROM rf1 JOIN (rf2 JOIN (rf3 RIGHT JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k
		      ORDER BY rf1.id1, rf2.id2, coalesce(rf3.k, rf4.k), rf3.id3, rf4.id4`,
	},
	{
		// Stacked RIGHT over RIGHT nested group.
		name: "stacked RIGHT over RIGHT nested group",
		sql: `SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4
		      FROM rf1 RIGHT JOIN (rf2 RIGHT JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k
		      ORDER BY coalesce(rf1.k, rf2.k, rf3.k, rf4.k), rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	},
	{
		name: "stacked FULL over RIGHT nested group",
		sql: `SELECT rf1.v1, rf2.v2, rf3.v3, rf4.v4
		      FROM rf1 FULL JOIN (rf2 RIGHT JOIN (rf3 JOIN rf4 ON rf3.k = rf4.k) ON rf2.k = rf3.k) ON rf1.k = rf2.k
		      ORDER BY coalesce(rf1.k, rf2.k, rf3.k, rf4.k), rf1.id1, rf2.id2, rf3.id3, rf4.id4`,
	},
}

// TestNestedParenJoinValuesParity checks row values against C SQLite at different page sizes.
func TestNestedParenJoinValuesParity(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			path := buildRightFullJoinDB(t, pageSize)

			cdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()

			p, err := engine.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			for _, c := range nestedParenValuesCorpus {
				gotCols, gotVals, gotErr := p.Query(c.sql)
				cgoCols, cgoRows, cgoErr := cgoSelect(t, cdb, c.sql, nil)
				if gotErr != nil || cgoErr != nil {
					t.Errorf("[%s] unexpected error\n  sql: %s\n  engine=%v\n  cgo=%v", c.name, c.sql, gotErr, cgoErr)
					continue
				}
				if len(gotCols) != len(cgoCols) {
					t.Errorf("[%s] column COUNT mismatch: engine=%d cgo=%d\n  sql: %s", c.name, len(gotCols), len(cgoCols), c.sql)
					continue
				}
				gotRows := engineRowsToStrings(gotVals)
				// Column names are deliberately NOT compared here (see this
				// file's package doc comment) -- only row count, and then
				// values, order-sensitively (the ORDER BY above is a full
				// tiebreak via each table's own primary key, so row order is
				// part of the contract).
				if len(gotRows) != len(cgoRows) {
					t.Errorf("[%s] row COUNT mismatch: engine=%d cgo=%d\n  sql: %s\n  engine: %v\n  cgo: %v", c.name, len(gotRows), len(cgoRows), c.sql, gotRows, cgoRows)
					continue
				}
				for r := range gotRows {
					if len(gotRows[r]) != len(cgoRows[r]) {
						t.Errorf("[%s] row %d column count mismatch: engine=%d cgo=%d\n  sql: %s", c.name, r, len(gotRows[r]), len(cgoRows[r]), c.sql)
						continue
					}
					for col := range gotRows[r] {
						if !cellsEqual(gotRows[r][col], cgoRows[r][col]) {
							t.Errorf("[%s] row %d col %d VALUE mismatch: engine=%q cgo=%q\n  sql: %s\n  engine row: %v\n  cgo row:    %v",
								c.name, r, col, gotRows[r][col], cgoRows[r][col], c.sql, gotRows[r], cgoRows[r])
						}
					}
				}
			}
		})
	}
}
