package engine

import (
	"fmt"
	"testing"
)

// TestZoneMinMaxMatchesTheLoop: a predicate-free min/max comes from the zone
// maps; a filtered one, or one over the rowid alias, still runs the kernel.
func TestZoneMinMaxMatchesTheLoop(t *testing.T) {
	p := newSegPair(t, `CREATE TABLE t(id INTEGER PRIMARY KEY, v INTEGER, w INTEGER)`,
		`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < 140000)
		 INSERT INTO t SELECT i, (i * 7919) % 100003 - 50000, -i FROM c`)
	cases := []struct {
		sql  string
		zone bool
	}{
		{`SELECT min(v) FROM t`, true},
		{`SELECT max(v) FROM t`, true},
		{`SELECT min(v), max(v) FROM t`, true},
		{`SELECT max(w), min(w) FROM t`, true},
		{`SELECT min(v) FROM t WHERE v > 100`, false},
		{`SELECT max(id) FROM t`, false},
	}
	for _, c := range cases {
		want := fmt.Sprint(typedRows(p.mustPlain(c.sql)))
		before := segZoneExtremeHits.Load()
		got := fmt.Sprint(typedRows(p.mustFast(c.sql)))
		if got != want {
			t.Errorf("%s: fast %s plain %s", c.sql, got, want)
		}
		if JITEnabled() && (segZoneExtremeHits.Load() > before) != c.zone {
			t.Errorf("%s: zone used=%v", c.sql, segZoneExtremeHits.Load() > before)
		}
	}
}
