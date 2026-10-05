package compat

// TestCompoundRecursiveCTEArm tests RECURSIVE CTEs in COMPOUND SELECT arms
// with various operations (UNION, EXCEPT, INTERSECT).

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

var recursiveArmSchema = []string{
	"CREATE TABLE tx(x)",
	"INSERT INTO tx VALUES(2),(4),(6),(9)",
	"CREATE TABLE link(aa, bb)",
	"INSERT INTO link VALUES(1,2),(2,3),(3,4),(4,5)",
}

func TestCompoundRecursiveCTEArm(t *testing.T) {
	for _, q := range []string{
		"WITH RECURSIVE r1(x) AS (VALUES(2) UNION ALL SELECT x+2 FROM r1 WHERE x<20), r2(y) AS (VALUES(3) UNION ALL SELECT y+3 FROM r2 WHERE y<20) SELECT x FROM r1 EXCEPT SELECT y FROM r2 ORDER BY 1",
		"WITH RECURSIVE r1(x) AS (VALUES(2) UNION ALL SELECT x+2 FROM r1 WHERE x<20) SELECT x FROM r1 UNION ALL SELECT x FROM tx ORDER BY 1",
		"WITH RECURSIVE r1(x) AS (VALUES(2) UNION ALL SELECT x+2 FROM r1 WHERE x<20) SELECT x FROM r1 UNION SELECT x FROM tx ORDER BY 1",
		"WITH RECURSIVE r1(x) AS (VALUES(2) UNION ALL SELECT x+2 FROM r1 WHERE x<20) SELECT x FROM r1 INTERSECT SELECT x FROM tx",
		"WITH RECURSIVE r1(x) AS (VALUES(2) UNION ALL SELECT x+2 FROM r1 WHERE x<20) SELECT x FROM tx EXCEPT SELECT x FROM r1",
		"WITH RECURSIVE r1(x) AS (VALUES(2) UNION ALL SELECT x+2 FROM r1 WHERE x<20) SELECT * FROM (SELECT x FROM r1 WHERE x<9) UNION ALL SELECT x FROM tx ORDER BY 1",
		"WITH RECURSIVE cl(x) AS (VALUES(1) UNION SELECT bb FROM link JOIN cl ON aa=x ORDER BY x DESC) SELECT x FROM cl UNION SELECT x FROM tx ORDER BY 1",
		"WITH c(x) AS (SELECT x FROM tx WHERE x>3) SELECT x FROM c UNION ALL SELECT x FROM tx ORDER BY 1",
	} {
		if !differ(t, "recursivearm", append(append([]string(nil), recursiveArmSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}

// TestCompoundRecursiveCTEArmLimit tests LIMIT/OFFSET on recursive CTE bodies.
func TestCompoundRecursiveCTEArmLimit(t *testing.T) {
	for _, q := range []string{
		"WITH RECURSIVE r1(x) AS (VALUES(2) UNION ALL SELECT x+2 FROM r1 WHERE x<20 LIMIT 3) SELECT x FROM r1 UNION SELECT x FROM tx",
		"WITH RECURSIVE cl(x) AS (VALUES(1) UNION SELECT bb FROM link JOIN cl ON aa=x ORDER BY x LIMIT 3) SELECT x FROM cl UNION ALL SELECT x FROM tx",
	} {
		if !differ(t, "recursivearmlimit", append(append([]string(nil), recursiveArmSchema...), q)) {
			t.Errorf("diverged on: %s", q)
		}
	}
}
