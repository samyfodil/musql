// This file gates JSON nesting in the aggregate builders: json_group_array()
// and json_group_object() embed an already-JSON argument instead of quoting it,
// like json_array()/json_object() do. A plain string like '[1,2]' must still be
// quoted.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

var jsonQ = []string{
	`SELECT json_group_array(x) FROM t1`,
	`SELECT json_group_array(json_object('x',x)) FROM t1`,
	`SELECT json_group_array(json_array(x)) FROM t1`,
	`SELECT json_group_array(json('[1,2]')) FROM t1`,
	`SELECT json_group_array('[1,2]') FROM t1`,
	`SELECT json_object('a', json_object('b',1))`,
	`SELECT json_array(json_object('b',1))`,
	`SELECT json_group_object('k', json_object('x',x)) FROM t1`,
	`SELECT json_group_array(json_quote(x)) FROM t1`,
}

func TestJSONAggregateNestingParity(t *testing.T) {
	edb, _ := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	defer edb.Close()
	cdb, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
	defer cdb.Close()
	for _, s := range []string{`CREATE TABLE t1(x)`, `INSERT INTO t1 VALUES(1),('abc')`} {
		edb.Exec(s)
		cdb.Exec(s)
	}
	p, _ := edb.SnapshotPager()
	for _, q := range jsonQ {
		_, ev, eerr := p.QueryArgs(q, nil)
		_, cr, cerr := cgoSelect(t, cdb, q, nil)
		if eerr != nil || cerr != nil {
			t.Fatalf("[%s] engine=%v cgo=%v", q, eerr, cerr)
		}
		eRows := engineRowsToStrings(ev)
		if ok, reason := queryResultsMatch([]string{"j"}, eRows, []string{"j"}, cr, true); !ok {
			t.Errorf("[%s] DIVERGES: %s\n  engine: %v\n  cgo:    %v", q, reason, eRows, cr)
		}
	}
}
