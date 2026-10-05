// This file tests LIMIT/OFFSET expression name context restrictions.
package compat

import "testing"

func upsertScopeBase() []string {
	return []string{
		`CREATE TABLE k(a INTEGER PRIMARY KEY, c INTEGER)`,
		`INSERT INTO k VALUES(1,10)`,
		`CREATE TABLE u(b INTEGER)`,
		`INSERT INTO u VALUES(100),(200),(300)`,
		`CREATE TABLE t(a INTEGER, c INTEGER)`,
	}
}

// TestLimitOffsetRejectsAggregateAndWindow tests that aggregate and window
// functions are rejected in LIMIT/OFFSET clauses.
func TestLimitOffsetRejectsAggregateAndWindow(t *testing.T) {
	for _, q := range []string{
		`SELECT a FROM t ORDER BY a LIMIT row_number() OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT 1 OFFSET row_number() OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT ntile(2) OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT lag(1) OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT first_value(1) OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT nth_value(1,1) OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT rank() OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT sum(1) OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT abs(row_number() OVER ())`,
		`SELECT 1 LIMIT row_number() OVER ()`,
		`SELECT a FROM t ORDER BY a LIMIT count(*)`,
		`SELECT a FROM t ORDER BY a LIMIT 1 OFFSET count(*)`,
		`SELECT a FROM t ORDER BY a LIMIT sum(1)`,
		`SELECT a FROM t ORDER BY a LIMIT max(1)`,
		`SELECT a FROM t ORDER BY a LIMIT avg(1)`,
		`SELECT a FROM t ORDER BY a LIMIT total(1)`,
		`SELECT a FROM t ORDER BY a LIMIT group_concat(1)`,
		`SELECT a FROM t ORDER BY a LIMIT json_group_array(1)`,
		`SELECT a FROM t ORDER BY a LIMIT count(*) FILTER (WHERE 1)`,
		`SELECT a FROM t ORDER BY a LIMIT abs(count(*))`,
		`SELECT a FROM t ORDER BY a LIMIT CASE WHEN 1 THEN count(*) ELSE 2 END`,
		`SELECT 1 LIMIT count(*)`,
	} {
		differ(t, "limit-misuse-"+q, append(limitScopeBase(), q))
	}

	// The counterweight: a call the position ACCEPTS must keep answering, so
	// the leg above cannot be met by refusing every call. A SUBQUERY is its own
	// resolution scope, so an aggregate -- or a whole window query -- inside one
	// is legal here; and 2-argument min/max is SQLite's ordinary SCALAR
	// min/max, not an aggregate at all.
	for _, q := range []string{
		`SELECT a FROM t ORDER BY a LIMIT max(1,2)`,
		`SELECT a FROM t ORDER BY a LIMIT min(2,3)`,
		`SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u)`,
		`SELECT a FROM t ORDER BY a LIMIT 1+(SELECT count(*) FROM u)`,
		`SELECT b FROM u ORDER BY b LIMIT (SELECT row_number() OVER () FROM t LIMIT 1)`,
	} {
		differ(t, "limit-nonagg-"+q, append(limitScopeBase(), q))
	}
}

// TestLimitOffsetRejectsUpsertExcluded pins the ASYMMETRY between lookupName's
// two pseudo-row arms, which is the OTHER half of the regression -- and which an
// earlier revision got backwards in a comment that cited C for it, in code that
// then OVERWROTE COMMITTED ROWS.
//
//	resolve.c:525   if( pParse->pTriggerTab!=0 ){            <- a PARSE field
//	resolve.c:547   if( (pNC->ncFlags & NC_UUpsert)!=0 ...   <- a NAMECONTEXT flag
//
// resolve.c:1903's "memset(&sNC, 0, sizeof(sNC))" cannot reach
// pParse->pTriggerTab, so new./old. still resolve inside a LIMIT -- but it DOES
// clear NC_UUpsert, so excluded. never resolves there and the statement is
// "no such column: excluded.c". Every case below must ERROR and leave k as
// (1,10); the fold wrote (1,7), (1,NULL) and (1,3) instead.
func TestLimitOffsetRejectsUpsertExcluded(t *testing.T) {
	for _, c := range []struct {
		name string
		tail []string
	}{
		{"set-subquery-limit", []string{
			`INSERT INTO k VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT excluded.c)`,
		}},
		{"set-subquery-offset", []string{
			`INSERT INTO k VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT 1 OFFSET excluded.c)`,
		}},
		{"set-subquery-with-from", []string{
			`INSERT INTO k VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 FROM u LIMIT excluded.a)`,
		}},
		{"do-update-where", []string{
			`INSERT INTO k VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=3 WHERE EXISTS(SELECT 1 LIMIT excluded.c)`,
		}},
		{"inside-a-trigger-body", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO k VALUES(1,5) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT excluded.c); END`,
			`INSERT INTO t VALUES(1,1)`,
		}},
	} {
		differ(t, "limit-excluded-"+c.name, append(append(upsertScopeBase(), c.tail...), `SELECT a,c FROM k`))
	}

	// The counterweights. An UPSERT nested in a trigger body may still name
	// new. in its LIMIT (the pTriggerTab arm), and an ordinary excluded.
	// reference OUTSIDE a LIMIT is untouched -- so this gate cannot be met by
	// refusing upserts, or by refusing every pseudo-row reference.
	differ(t, "limit-excluded-trigger-new-still-answers", append(upsertScopeBase(),
		`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO k VALUES(1,5) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT new.a); END`,
		`INSERT INTO t VALUES(1,1)`,
		`SELECT a,c FROM k`))
	differ(t, "limit-excluded-plain-set-still-answers", append(upsertScopeBase(),
		`INSERT INTO k VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=excluded.c`,
		`SELECT a,c FROM k`))

	// And the OTHER half of the whitelist: a REAL TABLE named
	// "new"/"old"/"excluded" is an ordinary scope, whose arm in lookupName never
	// fires outside a trigger/upsert, so a LIMIT naming it is "no such column"
	// on the oracle too.
	for _, name := range []string{"new", "old", "excluded"} {
		differ(t, "limit-realtable-"+name, []string{
			`CREATE TABLE ` + name + `(a INTEGER)`,
			`INSERT INTO ` + name + ` VALUES(2)`,
			`CREATE TABLE u(b INTEGER)`,
			`INSERT INTO u VALUES(100),(200),(300)`,
			`UPDATE ` + name + ` SET a=9 WHERE EXISTS(SELECT 1 FROM u LIMIT ` + name + `.a)`,
			`SELECT a FROM ` + name,
		})
	}
}
