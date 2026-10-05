package compat

// This file tests aliased join groups with bare star expansion.
// It verifies correct behavior when repeated column names appear in
// chained NATURAL joins with aliased parenthesized groups.

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// TestR42JoinHAliasedNaturalChainBareStar tests aliased natural join chains with bare star expansion.
func TestR42JoinHAliasedNaturalChainBareStar(t *testing.T) {
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

	setup := []string{
		`CREATE TABLE t1(c0 INT)`,
		`CREATE TABLE t2(c0 BLOB)`,
		`CREATE TABLE t3(c0 BLOB)`,
		`CREATE TABLE t4(c4 BLOB)`,
		`INSERT INTO t1(c0) VALUES(0)`,
		`INSERT INTO t3(c0) VALUES('0')`,
	}
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("engine setup %q: %v", s, err)
		}
		if _, err := cdb.Exec(s); err != nil {
			t.Fatalf("cgo setup %q: %v", s, err)
		}
	}
	p, err := edb.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		id string
		q  string
	}{
		// 14.1.*: t1 leads.
		{"14.1.1", `SELECT * FROM t1 NATURAL LEFT JOIN t2 NATURAL JOIN t3`},
		{"14.1.2", `SELECT * FROM t1 NATURAL LEFT JOIN t2 NATURAL JOIN t3 FULL JOIN t4 ON true`},
		{"14.1.3", `SELECT * FROM (t1 NATURAL LEFT JOIN t2 NATURAL JOIN t3) FULL JOIN t4 ON true`},
		{"14.1.4", `SELECT * FROM (t1 NATURAL LEFT JOIN t2 NATURAL JOIN t3) AS qq FULL JOIN t4 ON true`},
		// 14.2.*: t3 leads (symmetric, exercises the front-move condition from
		// the OTHER member).
		{"14.2.1", `SELECT * FROM t3 NATURAL LEFT JOIN t2 NATURAL JOIN t1`},
		{"14.2.2", `SELECT * FROM t3 NATURAL LEFT JOIN t2 NATURAL JOIN t1 FULL JOIN t4 ON true`},
		{"14.2.3", `SELECT * FROM (t3 NATURAL LEFT JOIN t2 NATURAL JOIN t1) FULL JOIN t4 ON true`},
		{"14.2.4", `SELECT * FROM (t3 NATURAL LEFT JOIN t2 NATURAL JOIN t1) AS qq FULL JOIN t4 ON true`},
	} {
		cCols, cRows, cErr := cgoSelect(t, cdb, tc.q, nil)
		eCols, eVals, eErr := p.QueryArgs(tc.q, nil)
		if cErr != nil {
			if eErr == nil {
				t.Errorf("[%s %s] the engine ANSWERED what the oracle rejects: %v", tc.id, tc.q, cErr)
			}
			continue
		}
		if eErr != nil {
			t.Errorf("[%s %s] the engine DECLINED a joinH.test statement the oracle answers: %v", tc.id, tc.q, eErr)
			continue
		}
		if len(eCols) != len(cCols) {
			t.Errorf("[%s %s] column count: engine=%v cgo=%v", tc.id, tc.q, eCols, cCols)
			continue
		}
		for i := range eCols {
			if eCols[i] != cCols[i] {
				t.Errorf("[%s %s] column %d name: engine=%q cgo=%q", tc.id, tc.q, i, eCols[i], cCols[i])
			}
		}
		if ok, why := queryResultsMatch(eCols, engineRowsToStrings(eVals), cCols, cRows, false); !ok {
			t.Errorf("[%s %s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", tc.id, tc.q, why, eVals, cRows)
		}
	}
}
