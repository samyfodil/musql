// Recursive CTE with content-bounded safety net: tests large recursions that
// terminate with internal bounds, over both materialized and streaming consumers.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
)

func rcapPair(t *testing.T) (*engine.Session, *sql.DB) {
	t.Helper()
	return rcteConsumerPair(t)
}

func TestRecursiveCTEPastOldRowCapValues(t *testing.T) {
	edb, cdb := rcapPair(t)
	for _, q := range []string{
		`WITH r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<60000) SELECT count(*), sum(i), min(i), max(i) FROM r`,
		`WITH r(i) AS (SELECT 1 UNION SELECT i+1 FROM r WHERE i<60000) SELECT count(*), sum(i), min(i), max(i) FROM r`,
		`WITH r(i,s) AS (SELECT 1,'a' UNION ALL SELECT i+1, 'b' FROM r WHERE i<60000) SELECT count(*), sum(i), group_concat(DISTINCT s) FROM r`,
		`WITH r(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<60000) SELECT i FROM r LIMIT 4`,
	} {
		flCompareQuery(t, "rcap", edb, cdb, q)
	}
}

func TestRecursiveCTELongRecursionInsert(t *testing.T) {
	flLockstep(t, "temptable2-1.2", []string{
		`CREATE TEMP TABLE t1(a, b)`,
		`WITH x(i) AS ( SELECT 1 UNION ALL SELECT i+1 FROM x WHERE i<100000 ) INSERT INTO t1 SELECT randomblob(100), randomblob(100) FROM X`,
	},
		`SELECT count(*), sum(length(a)), sum(length(b)), typeof(a), typeof(b) FROM t1`,
		`PRAGMA temp.integrity_check`,
	)
	flLockstep(t, "vacuummem-1.0", []string{
		`CREATE TABLE t1(a, b, c)`,
		`WITH r(i) AS ( SELECT 1 UNION ALL SELECT i+1 FROM r WHERE i<100000 ) INSERT INTO t1 SELECT randomblob(100),randomblob(100),randomblob(100) FROM r`,
		`CREATE INDEX t1a ON t1(a)`,
	},
		`SELECT count(*), sum(length(a)), sum(length(b)), sum(length(c)) FROM t1`,
		`PRAGMA integrity_check`,
	)
}

func TestRecursiveCTEWideRowsStreamIntoATable(t *testing.T) {
	flLockstep(t, "bigsort-10k", []string{
		`CREATE TABLE t1(a, b)`,
		`BEGIN`,
		`WITH data(x,y) AS ( SELECT 1, zeroblob(10000) UNION ALL SELECT x+1, y FROM data WHERE x < 10000 ) INSERT INTO t1 SELECT * FROM data`,
		`COMMIT`,
	},
		`SELECT count(*), sum(length(b)), min(a), max(a), sum(a) FROM t1`,
		`SELECT a FROM t1 WHERE rowid IN (1, 777, 10000) ORDER BY a`,
	)
}

func TestRecursiveCTEWideRowsIntoAnIndexedTable(t *testing.T) {
	start := time.Now()
	flLockstep(t, "bigsort-indexed-20k", []string{
		`CREATE TABLE t1(a, b)`,
		`CREATE INDEX t1a ON t1(a)`,
		`BEGIN`,
		`WITH data(x,y) AS ( SELECT 1, zeroblob(10000) UNION ALL SELECT x+1, y FROM data WHERE x < 20000 ) INSERT INTO t1 SELECT * FROM data`,
		`COMMIT`,
	},
		`SELECT count(*), sum(length(b)), min(a), max(a), sum(a) FROM t1`,
		`SELECT a FROM t1 WHERE a IN (1, 777, 20000) ORDER BY a`,
		`SELECT count(*) FROM t1 WHERE a > 19990`,
	)
	if elapsed := time.Since(start); elapsed > 5*time.Minute {
		t.Errorf("took %v for 200 MB through both engines; that is slow enough to be a regression, not a load", elapsed)
	}
}

// TestRecursiveCTERunawayStillDeclines keeps the ORIGINAL job of the safety net
// intact: a recursion with no WHERE bound of its own, feeding a consumer with no
// LIMIT to publish, must still end in a clean error rather than run forever.
// Both bounds are exercised -- the narrow integer recursion ends on the row
// count (measured 1.7s), the growing-TEXT one on the content bound (0.5s).
func TestRecursiveCTERunawayStillDeclines(t *testing.T) {
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	if err := edb.Exec(`CREATE TABLE t(n)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	for _, q := range []string{
		`WITH c(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM c) INSERT INTO t SELECT x FROM c`,
		`WITH c(x) AS (VALUES(1) UNION SELECT x+1 FROM c) INSERT INTO t SELECT x FROM c`,
		`WITH c(x,y) AS (VALUES(1,'a') UNION ALL SELECT x+1, y||'b' FROM c) INSERT INTO t SELECT x FROM c`,
	} {
		start := time.Now()
		err := edb.Exec(q)
		elapsed := time.Since(start)
		if err == nil {
			t.Errorf("non-terminating recursion was ACCEPTED: %s", q)
			continue
		}
		if elapsed > 60*time.Second {
			t.Errorf("%s declined only after %v", q, elapsed)
		}
	}
}
