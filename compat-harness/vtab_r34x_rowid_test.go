// Virtual-table INSERT with named rowid: tests how different modules handle
// rowid as both a pseudo-column and an actual column.
package compat

import "testing"

// r34xRowidValues are rowid values that test integer validation.
var r34xRowidValues = []struct{ name, val string }{
	{"integer", `5`},
	{"negative", `-5`},
	{"numeric text", `'5'`},
	{"integral real text", `'5.0'`},
	{"integral real", `5.0`},
	{"non-numeric text", `'x'`},
	{"empty text", `''`},
	{"fractional real", `5.5`},
	{"blob", `x'0102'`},
	{"null", `NULL`},
	{"expression", `2+3`},
	{"text expression", `'a'||'b'`},
}

// TestR34XRtreeNamedRowid tests rtree's handling of explicit rowid.
func TestR34XRtreeNamedRowid(t *testing.T) {
	for _, v := range r34xRowidValues {
		for _, spelling := range []string{"rowid", "oid", "_rowid_"} {
			differ(t, "r34x rtree "+spelling+" "+v.name, []string{
				`CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)`,
				`INSERT INTO t(` + spelling + `, minX, maxX) VALUES(` + v.val + `, 1, 2)`,
				`SELECT rowid, id, minX, maxX FROM t ORDER BY rowid`,
			})
		}
	}
	// id column is silently coerced by rtree.
	for _, v := range r34xRowidValues {
		differ(t, "r34x rtree id column "+v.name, []string{
			`CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)`,
			`INSERT INTO t(id, minX, maxX) VALUES(` + v.val + `, 1, 2)`,
			`SELECT rowid, id, minX, maxX FROM t ORDER BY rowid`,
		})
		differ(t, "r34x rtree positional "+v.name, []string{
			`CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)`,
			`INSERT INTO t VALUES(` + v.val + `, 1, 2)`,
			`SELECT rowid, id, minX, maxX FROM t ORDER BY rowid`,
		})
	}
	// Both rowid and id named: rowid is checked, id decides.
	differ(t, "r34x rtree rowid and id", []string{
		`CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)`,
		`INSERT INTO t(rowid, id, minX, maxX) VALUES(5, 9, 1, 2)`,
		`SELECT rowid, id, minX, maxX FROM t ORDER BY rowid`,
	})
	differ(t, "r34x rtree bad rowid with good id", []string{
		`CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)`,
		`INSERT INTO t(rowid, id, minX, maxX) VALUES('x', 9, 1, 2)`,
		`SELECT rowid, id, minX, maxX FROM t ORDER BY rowid`,
	})
	// Multi-row VALUES with bad rowid in second row.
	differ(t, "r34x rtree multi row second bad", []string{
		`CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)`,
		`INSERT INTO t(rowid, id, minX, maxX) VALUES(1, 1, 1, 2),('x', 2, 3, 4)`,
		`SELECT rowid, id, minX, maxX FROM t ORDER BY rowid`,
	})
	// SELECT-sourced INSERT with bad rowid.
	differ(t, "r34x rtree select source bad rowid", []string{
		`CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)`,
		`CREATE TABLE s(r, i, a, b)`,
		`INSERT INTO s VALUES('x', 7, 1, 2)`,
		`INSERT INTO t(rowid, id, minX, maxX) SELECT r, i, a, b FROM s`,
		`SELECT rowid, id, minX, maxX FROM t ORDER BY rowid`,
	})
	// Unknown column name.
	differ(t, "r34x rtree unknown column", []string{
		`CREATE VIRTUAL TABLE t USING rtree(id, minX, maxX)`,
		`INSERT INTO t(nosuch, minX, maxX) VALUES(1, 1, 2)`,
		`SELECT rowid, id, minX, maxX FROM t ORDER BY rowid`,
	})
}
