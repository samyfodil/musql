package engine

import (
	"strings"
	"testing"
)

// TestNotIndexedSuppressesTheIndexSeek verifies that the NOT INDEXED clause
// suppresses index seeks while preserving PRIMARY KEY seeks.
func TestNotIndexedSuppressesTheIndexSeek(t *testing.T) {
	for _, tc := range []struct {
		name, ddl, query string
		wantIndexHint    bool
		wantRowidHint    bool
	}{
		{"plain scan seeks the index", "CREATE TABLE t(c0, c1)",
			"SELECT c0 FROM t WHERE c0=1", true, false},
		{"NOT INDEXED does not", "CREATE TABLE t(c0, c1)",
			"SELECT c0 FROM t NOT INDEXED WHERE c0=1", false, false},
		// NOT INDEXED preserves rowid seeks.
		{"NOT INDEXED keeps the rowid seek", "CREATE TABLE t(c0 INTEGER PRIMARY KEY, c1)",
			"SELECT c1 FROM t NOT INDEXED WHERE c0=1", false, true},
		{"rowid seek without the hint", "CREATE TABLE t(c0 INTEGER PRIMARY KEY, c1)",
			"SELECT c1 FROM t WHERE c0=1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range []string{tc.ddl, "CREATE INDEX ix ON t(c0)",
				"INSERT INTO t VALUES(1,2),(3,4)"} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("%q: %v", s, err)
				}
			}
			pg, perr := db.SnapshotPager()
			if perr != nil {
				t.Fatal(perr)
			}
			d, derr := DisassembleScan(pg, tc.query)
			if derr != nil {
				t.Fatalf("disassemble %q: %v", tc.query, derr)
			}
			if got := strings.Contains(d, "SeekIndexHint"); got != tc.wantIndexHint {
				t.Errorf("%q emits SeekIndexHint=%v, want %v\n%s", tc.query, got, tc.wantIndexHint, d)
			}
			if got := strings.Contains(d, "SeekRowidHint"); got != tc.wantRowidHint {
				t.Errorf("%q emits SeekRowidHint=%v, want %v\n%s", tc.query, got, tc.wantRowidHint, d)
			}
			// Answer must match either way.
			_, rows, qerr := pg.QueryArgs(tc.query, nil)
			if qerr != nil || len(rows) != 1 {
				t.Errorf("%q = %v (err %v), want exactly one row", tc.query, rows, qerr)
			}
		})
	}
}
