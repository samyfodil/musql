package engine

import "testing"

// TestTempTableShadowDoesNotEmptyMainIndex verifies that creating a TEMP table
// with the same name as a main table does not corrupt the main table's index.
func TestTempTableShadowDoesNotEmptyMainIndex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts []string
		query string
		want  int64
	}{
		// Temp table created after the main table and index.
		{"temp table created last", []string{
			"CREATE TABLE t0(c0, c1)",
			"CREATE INDEX i0 ON t0(c0)",
			"INSERT INTO t0 VALUES(1,2)",
			"CREATE TEMP TABLE t0(c0, c1)",
		}, "SELECT c0 FROM main.t0 WHERE c0=1", 1},
		// Temp table created first, then main table with qualified spelling.
		{"temp table created first", []string{
			"CREATE TEMP TABLE tbl(a,b,c)",
			"INSERT INTO tbl VALUES(1,2,3)",
			"CREATE TABLE main.tbl(a,b,c)",
			"INSERT INTO main.tbl VALUES(9,9,9)",
			"CREATE INDEX main.tbli ON tbl(a,b,c)",
		}, "SELECT a FROM main.tbl WHERE a=9", 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer n.Discard()
			for _, s := range tc.stmts {
				if err := n.Exec(s); err != nil {
					t.Fatalf("%q: %v", s, err)
				}
			}
			_, rows, qerr := n.Query(tc.query, nil)
			if qerr != nil {
				t.Fatalf("%q: %v", tc.query, qerr)
			}
			if len(rows) != 1 || rows[0][0].I != tc.want {
				t.Errorf("%q = %v, want one row holding %d -- the main table's "+
					"index was rebuilt from the shadowing TEMP table's rows",
					tc.query, rows, tc.want)
			}
		})
	}
}
