// Pseudo-row lifetime, shadowing, and column identity tests verify that
// references to pseudo-rows (excluded, new, old, returned values) behave
// correctly when used in subqueries and with proper caching and collation rules.
package compat

import "testing"

// TestUpsertExcludedSetSubqueryIsPerConflictingRow checks that subqueries
// referencing the excluded row are re-evaluated for each conflicting row.
func TestUpsertExcludedSetSubqueryIsPerConflictingRow(t *testing.T) {
	differ(t, "excluded per conflicting row, INSERT ... SELECT", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE src(k INTEGER, v INTEGER)`,
		`INSERT INTO src VALUES(1,100),(2,200),(3,300)`,
		`INSERT INTO u VALUES(1,10),(2,10),(3,10)`,
		`INSERT INTO u SELECT k,v FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)`,
		`SELECT k,v FROM u ORDER BY k`,
	})
	// Test excluded.rowid and compound queries too.
	differ(t, "excluded.rowid per conflicting row", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE src(k INTEGER, v INTEGER)`,
		`INSERT INTO src VALUES(1,100),(2,200),(3,300)`,
		`INSERT INTO u VALUES(1,10),(2,10),(3,10)`,
		`INSERT INTO u SELECT k,v FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.rowid*1000)`,
		`SELECT k,v FROM u ORDER BY k`,
	})
	differ(t, "excluded per conflicting row through a compound", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE src(k INTEGER, v INTEGER)`,
		`INSERT INTO src VALUES(1,100),(2,200),(3,300)`,
		`INSERT INTO u VALUES(1,10),(2,10),(3,10)`,
		`INSERT INTO u SELECT k,v FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v UNION SELECT 0 ORDER BY 1 DESC LIMIT 1)`,
		`SELECT k,v FROM u ORDER BY k`,
	})
	// Control: uncorrelated subqueries freeze at the first conflicting row.
	differ(t, "an uncorrelated SET subquery freezes at the first conflicting row", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
		`CREATE TABLE src(k INTEGER, v INTEGER)`,
		`INSERT INTO src VALUES(1,100),(2,200),(3,300)`,
		`INSERT INTO u VALUES(1,10),(2,10),(3,10)`,
		`INSERT INTO u SELECT k,v FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT abs(random()))`,
		`SELECT count(DISTINCT v) FROM u`,
	})
}

// TestUpsertSetMixedLifetimeMatchesC checks that SET lists can mix correlated
// and uncorrelated subqueries with different lifetimes.
func TestUpsertSetMixedLifetimeMatchesC(t *testing.T) {
	differ(t, "a mixed-lifetime upsert SET", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v, w)`,
		`CREATE TABLE src(k INTEGER, v INTEGER)`,
		`INSERT INTO src VALUES(1,100),(2,200),(3,300)`,
		`INSERT INTO u VALUES(1,10,0),(2,10,0),(3,10,0)`,
		`INSERT INTO u SELECT k,v,0 FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1), w=(SELECT abs(random()))`,
		`SELECT k,v FROM u ORDER BY k`,
		`SELECT count(DISTINCT w) FROM u`,
	})
}

// TestReturningCompoundSubqueryLifetime checks that compound subqueries in
// RETURNING clauses follow the same lifetime rules as simple subqueries.
func TestReturningCompoundSubqueryLifetime(t *testing.T) {
	differ(t, "compound RETURNING subquery correlated to the affected row", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q')`,
		`UPDATE t SET b='z' RETURNING a,(SELECT count(*) FROM s WHERE x=a UNION SELECT 99 ORDER BY 1 LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "compound RETURNING subquery correlated in its SECOND arm", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q')`,
		`UPDATE t SET b='z' RETURNING a,(SELECT 0 UNION ALL SELECT a ORDER BY 1 DESC LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Control: uncorrelated compounds stay once.
	differ(t, "an uncorrelated compound RETURNING subquery stays once", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q')`,
		`UPDATE t SET b='z' RETURNING a,(SELECT count(*) FROM s UNION SELECT 99 ORDER BY 1 LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}

// TestReturningAffectedRowThroughACorrelatedSubProgram checks that affected
// rows are correctly passed to correlated subprograms.
func TestReturningAffectedRowThroughACorrelatedSubProgram(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x,y)`,
		`INSERT INTO s VALUES(1,'one'),(2,'two'),(3,'three')`,
	}
	differ(t, "affected row read from inside a correlated sub-program", append(append([]string(nil), setup...),
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT (SELECT count(*) FROM s WHERE s.x=q.x AND q.x=a) FROM s AS q LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a`))
	// Test ROWID spelling too.
	differ(t, "affected ROWID read from inside a correlated sub-program", append(append([]string(nil), setup...),
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING rowid,(SELECT (SELECT count(*) FROM s WHERE s.x=q.x AND q.x=t.rowid) FROM s AS q LIMIT 1)`,
		`SELECT a,b FROM t ORDER BY a`))
	differ(t, "affected row read from inside a correlated IN sub-program", append(append([]string(nil), setup...),
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s AS q WHERE q.x IN (SELECT q.x WHERE q.x=a))`,
		`SELECT a,b FROM t ORDER BY a`))
}

// TestPseudoRowsRejectASchemaQualifier checks that schema-qualified references
// to pseudo-rows are rejected.
func TestPseudoRowsRejectASchemaQualifier(t *testing.T) {
	differ(t, "main.excluded.<col> in an upsert SET subquery", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
		`INSERT INTO u VALUES(1,10)`,
		`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT main.excluded.v+1)`,
		`SELECT k,v FROM u`,
	})
	differ(t, "main.excluded.<col> under an aggregate in an upsert SET subquery", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
		`INSERT INTO u VALUES(1,10)`,
		`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT sum(main.excluded.v))`,
		`SELECT k,v FROM u`,
	})
	differ(t, "main.<target>.<col> in a RETURNING subquery", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s WHERE x=main.t.a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	// Test schema-qualified NEW./OLD. in trigger bodies.
	for _, body := range []string{
		`INSERT INTO dst SELECT 5 LIMIT main.new.a`,
		`INSERT INTO dst SELECT main.new.a`,
		`INSERT INTO dst VALUES(main.new.a)`,
	} {
		differ(t, "main.new.<col> in a trigger body: "+body, []string{
			`CREATE TABLE t(a INTEGER)`,
			`CREATE TABLE dst(b INTEGER)`,
			`CREATE TRIGGER tr AFTER INSERT ON t BEGIN ` + body + `; END`,
			`INSERT INTO t VALUES(2)`,
			`SELECT count(*),coalesce(sum(b),-1) FROM dst`,
		})
	}
	differ(t, "main.new.<col> in a trigger body DELETE ... WHERE", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE dst(b INTEGER)`,
		`INSERT INTO dst VALUES(2),(3)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN DELETE FROM dst WHERE b=main.new.a; END`,
		`INSERT INTO t VALUES(2)`,
		`SELECT count(*) FROM dst`,
	})
	differ(t, "main.old.<col> in a DELETE trigger body", []string{
		`CREATE TABLE t(a INTEGER)`,
		`CREATE TABLE dst(b INTEGER)`,
		`INSERT INTO t VALUES(2)`,
		`CREATE TRIGGER tr AFTER DELETE ON t BEGIN INSERT INTO dst VALUES(main.old.a); END`,
		`DELETE FROM t`,
		`SELECT count(*),coalesce(sum(b),-1) FROM dst`,
	})
}

// TestUpsertSetSubqueryOnceIsPerStatementNotPerTuple checks that uncorrelated
// SET subqueries are evaluated once per statement, not once per tuple.
func TestUpsertSetSubqueryOnceIsPerStatementNotPerTuple(t *testing.T) {
	differ(t, "unrolled VALUES upsert, INTEGER PRIMARY KEY target", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
		`INSERT INTO u VALUES(1,10),(2,10),(3,10)`,
		`INSERT INTO u VALUES(1,100),(2,200),(3,300) ON CONFLICT(k) DO UPDATE SET v=(SELECT abs(random()))`,
		`SELECT count(DISTINCT v) FROM u`,
	})
	// Test UNIQUE index target too.
	differ(t, "unrolled VALUES upsert, UNIQUE index target", []string{
		`CREATE TABLE u(k INTEGER, v)`,
		`CREATE UNIQUE INDEX ui ON u(k)`,
		`INSERT INTO u VALUES(1,10),(2,10),(3,10)`,
		`INSERT INTO u VALUES(1,100),(2,200),(3,300) ON CONFLICT(k) DO UPDATE SET v=(SELECT abs(random()))`,
		`SELECT count(DISTINCT v) FROM u`,
	})
	// Test inside a trigger body too.
	differ(t, "unrolled VALUES upsert inside a trigger body", []string{
		`CREATE TABLE fire(z)`,
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
		`INSERT INTO u VALUES(1,10),(2,10),(3,10)`,
		`CREATE TRIGGER tg AFTER INSERT ON fire BEGIN
		   INSERT INTO u VALUES(1,100),(2,200),(3,300) ON CONFLICT(k) DO UPDATE SET v=(SELECT abs(random()));
		 END`,
		`INSERT INTO fire VALUES(1)`,
		`SELECT count(DISTINCT v) FROM u`,
	})
	// Test per-row lifetime in unrolled VALUES.
	differ(t, "unrolled VALUES upsert, per-row lifetime still per row", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u VALUES(1,10),(2,10),(3,10)`,
		`INSERT INTO u VALUES(1,100),(2,200),(3,300) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)`,
		`SELECT k,v FROM u ORDER BY k`,
	})
}

// TestReturningAffectedRowKeepsItsColumnIdentity checks that affected rows keep
// their column identity (collation and affinity) in RETURNING subqueries.
func TestReturningAffectedRowKeepsItsColumnIdentity(t *testing.T) {
	for _, tc := range []struct{ name, ret string }{
		{"subquery spelling, declared COLLATE", `(SELECT CASE WHEN v='abc' THEN 'hit' ELSE 'miss' END FROM s)`},
		{"direct spelling, declared COLLATE", `CASE WHEN v='abc' THEN 'hit' ELSE 'miss' END`},
		{"subquery spelling, declared affinity", `(SELECT CASE WHEN n='5' THEN 'hit' ELSE 'miss' END FROM s)`},
		{"direct spelling, declared affinity", `CASE WHEN n='5' THEN 'hit' ELSE 'miss' END`},
		// Qualified spelling.
		{"subquery spelling, qualified by the target", `(SELECT CASE WHEN t.v='abc' THEN 'hit' ELSE 'miss' END FROM s)`},
	} {
		differ(t, "RETURNING affected row keeps its column identity: "+tc.name, []string{
			`CREATE TABLE t(a INTEGER, v TEXT COLLATE NOCASE, n INTEGER)`,
			`CREATE TABLE s(x INTEGER)`,
			`INSERT INTO s VALUES(1)`,
			`INSERT INTO t VALUES(1,'ABC',5) RETURNING ` + tc.ret,
		})
	}
}

// TestExcludedCarriesNoColumnIdentity checks that excluded rows do NOT keep
// their column identity (opposite rule from RETURNING).
func TestExcludedCarriesNoColumnIdentity(t *testing.T) {
	for _, tc := range []struct{ name, set string }{
		{"direct, declared COLLATE", `CASE WHEN excluded.v='abc' THEN 'hit' ELSE 'miss' END`},
		{"subquery, declared COLLATE", `(SELECT CASE WHEN excluded.v='abc' THEN 'hit' ELSE 'miss' END)`},
		{"direct, declared affinity", `CASE WHEN excluded.n='5' THEN 'hit' ELSE 'miss' END`},
		{"subquery, declared affinity", `(SELECT CASE WHEN excluded.n='5' THEN 'hit' ELSE 'miss' END)`},
	} {
		differ(t, "excluded carries no column identity: "+tc.name, []string{
			`CREATE TABLE u(k INTEGER PRIMARY KEY, v TEXT COLLATE NOCASE, n INTEGER)`,
			`CREATE TABLE src(k INTEGER, v TEXT, n INTEGER)`,
			`INSERT INTO src VALUES(1,'ABC',5),(2,'def',9)`,
			`INSERT INTO u VALUES(1,'x',0),(2,'y',0)`,
			`INSERT INTO u SELECT k,v,n FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v=` + tc.set,
			`SELECT k,v FROM u ORDER BY k`,
		})
	}
	// Test DO UPDATE WHERE too.
	differ(t, "excluded carries no column identity: DO UPDATE ... WHERE", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v TEXT COLLATE NOCASE)`,
		`CREATE TABLE src(k INTEGER, v TEXT)`,
		`INSERT INTO src VALUES(1,'ABC')`,
		`INSERT INTO u VALUES(1,'x')`,
		`INSERT INTO u SELECT k,v FROM src WHERE 1 ON CONFLICT(k) DO UPDATE SET v='hit' WHERE excluded.v='abc'`,
		`SELECT k,v FROM u`,
	})
}

// TestTriggerBodyLimitWithheldWhenTheFromShadowsIt checks that LIMIT promotion
// is withheld when a FROM clause has a table shadowing the pseudo-row name.
func TestTriggerBodyLimitWithheldWhenTheFromShadowsIt(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"FROM a table named new", `INSERT INTO dst SELECT new.b FROM new LIMIT new.a`},
		{"FROM aliased new", `INSERT INTO dst SELECT x.b FROM u AS new, u AS x LIMIT new.a`},
		{"FROM a table named old", `INSERT INTO dst SELECT b FROM old LIMIT new.a`},
	} {
		res := run(t, "musql", []string{
			`CREATE TABLE t(a INTEGER, b INTEGER)`,
			`CREATE TABLE new(a INTEGER, b INTEGER)`,
			`CREATE TABLE old(a INTEGER, b INTEGER)`,
			`CREATE TABLE u(a INTEGER, b INTEGER)`,
			`CREATE TABLE dst(v INTEGER)`,
			`INSERT INTO new VALUES(7,70),(8,80)`,
			`INSERT INTO old VALUES(7,70),(8,80)`,
			`INSERT INTO u VALUES(7,70),(8,80)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN ` + tc.body + `; END`,
			`INSERT INTO t VALUES(2,20)`,
			`SELECT count(*) FROM dst`,
		})
		if res[9]["kind"] != "error" {
			t.Errorf("[%s] %s: the firing statement was answered, not declined: %v\n"+
				"  a FROM item named new/old WINS the name in 3.53.3 (resolve.c:521's cnt==0), which this\n"+
				"  compiler's arm order does not reproduce -- so serving the clause here answers the FIRING\n"+
				"  row where the oracle answers the table. If compileColumn's order is fixed, delete this\n"+
				"  withholding and turn these into differ() cases.", tc.name, tc.body, res[9])
		}
		if rows, _ := res[10]["rows"].([]any); len(rows) == 1 {
			if cells, _ := rows[0].([]any); len(cells) == 1 && cells[0] != "I:0" {
				t.Errorf("[%s] the declined trigger body still wrote %v rows into dst", tc.name, cells[0])
			}
		}
	}
	// Control: no shadowing, so promotion applies.
	differ(t, "LIMIT new.a with nothing shadowing it", []string{
		`CREATE TABLE t(a INTEGER, b INTEGER)`,
		`CREATE TABLE u(a INTEGER, b INTEGER)`,
		`CREATE TABLE dst(v INTEGER)`,
		`INSERT INTO u VALUES(7,70),(8,80)`,
		`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.a; END`,
		`INSERT INTO t VALUES(1,20)`,
		`SELECT v FROM dst ORDER BY v`,
	})
}
