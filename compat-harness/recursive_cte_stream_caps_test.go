package compat

import (
	"encoding/json"
	"testing"
)

// TestRecursiveCTEStreamedPastTheRowCap: recursions of more rows than the
// engine's in-memory row cap, streamed into consumers that keep few of them,
// answer as C does (#70).
func TestRecursiveCTEStreamedPastTheRowCap(t *testing.T) {
	for _, q := range []string{
		"WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x < 400000) SELECT x FROM c WHERE x % 100000 = 0",
		"SELECT * FROM (WITH RECURSIVE c(x) AS (VALUES(1) UNION SELECT x+1 FROM c WHERE x < 350000) SELECT x FROM c WHERE x = 350000)",
		"SELECT * FROM (WITH RECURSIVE d(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM d WHERE n < 1), " +
			"x(v, i) AS (SELECT 0, 0 UNION ALL SELECT v*2+n, i+1 FROM x, d WHERE i < 18) " +
			"SELECT v FROM x WHERE i = 18 AND v % 65536 = 0)",
		"WITH RECURSIVE c(x, s) AS (VALUES(1, 'a') UNION ALL SELECT x+1, substr(s || x, -6) FROM c WHERE x < 320000) " +
			"SELECT x, s FROM c WHERE x > 319998",
		"SELECT * FROM (SELECT 0 AS x UNION ALL SELECT x FROM (WITH RECURSIVE c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c WHERE x < 400000) " +
			"SELECT x FROM c WHERE x % 100000 = 0))",
	} {
		m, c := run(t, "musql", []string{q}), run(t, "cgo", []string{q})
		mb, _ := json.Marshal(m)
		cb, _ := json.Marshal(c)
		if c[0]["kind"] == "error" || string(mb) != string(cb) {
			t.Errorf("%s\n  cgo:   %s\n  musql: %s", q, cb, mb)
		}
	}
}
