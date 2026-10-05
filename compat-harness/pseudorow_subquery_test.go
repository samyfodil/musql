// The three pseudo-rows a subquery can name from inside a write statement
// (NEW./OLD. in triggers, excluded in upserts, the affected row in RETURNING),
// and the LIMIT/OFFSET clause that can name one. This engine compiles
// subqueries into separate programs, so the row must be snapshotted and
// passed via an opcode.
package compat

import "testing"

// TestUpsertExcludedInSetSubquery checks "excluded" reached from inside a
// subquery in an ON CONFLICT DO UPDATE's SET list.
func TestUpsertExcludedInSetSubquery(t *testing.T) {
	differ(t, "excluded in a scalar SET subquery", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u VALUES(1,10)`,
		`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)`,
		`SELECT k,v FROM u`,
	})
	differ(t, "excluded under an aggregate in a SET subquery", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u VALUES(1,10)`,
		`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT sum(excluded.v))`,
		`SELECT k,v FROM u`,
	})
	differ(t, "excluded in a compound SET subquery", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u VALUES(1,10)`,
		`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v UNION SELECT 99 ORDER BY 1 LIMIT 1)`,
		`SELECT k,v FROM u`,
	})
	// INTEGER PRIMARY KEY and rowid both map to the rowid register.
	differ(t, "excluded names the INTEGER PRIMARY KEY", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u VALUES(1,10)`,
		`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.k*7)`,
		`SELECT k,v FROM u`,
	})
	differ(t, "excluded names the rowid pseudo-column", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u VALUES(1,10)`,
		`INSERT INTO u VALUES(1,20) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.rowid*7)`,
		`SELECT k,v FROM u`,
	})
	// Each conflicting row must see its own proposed row.
	differ(t, "excluded is per candidate row across a VALUES list", []string{
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
		`INSERT INTO u VALUES(1,10),(2,10)`,
		`INSERT INTO u VALUES(1,100),(2,200) ON CONFLICT(k) DO UPDATE SET v=(SELECT excluded.v+1)`,
		`SELECT k,v FROM u ORDER BY k`,
	})
	// Row-value SET from a subquery names no pseudo-row.
	differ(t, "row-value SET from a subquery", []string{
		`CREATE TABLE up1(k PRIMARY KEY,m,n)`,
		`INSERT INTO up1 VALUES(1,1,1)`,
		`INSERT INTO up1 VALUES(1,9,9) ON CONFLICT(k) DO UPDATE SET (m,n)=(SELECT 7,8)`,
		`SELECT k,m,n FROM up1`,
	})
	// Upsert inside a trigger body: NEW and EXCLUDED are distinct.
	differ(t, "excluded beside NEW inside a trigger body", []string{
		`CREATE TABLE k(a INTEGER PRIMARY KEY, c INTEGER)`,
		`INSERT INTO k VALUES(1,10)`,
		`CREATE TABLE t(a INTEGER, c INTEGER)`,
		`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO k VALUES(1,5) ON CONFLICT(a) DO UPDATE SET c=(SELECT excluded.c*100+new.c); END`,
		`INSERT INTO t VALUES(1,3)`,
		`SELECT a,c FROM k`,
	})
	// Target aliased: excluded refers to the pseudo-table, not the alias.
	differ(t, "target aliased, so excluded is the pseudo-table", []string{
		`CREATE TABLE excluded(a INTEGER PRIMARY KEY, c INT DEFAULT 7)`,
		`INSERT INTO excluded VALUES(1,5)`,
		`INSERT INTO excluded AS base(a) VALUES(1) ON CONFLICT(a) DO UPDATE SET c=(SELECT excluded.c+1)`,
		`SELECT a,c FROM excluded`,
	})
}

// TestUpsertSetSubqueryReadsTheLiveTable checks SET subqueries over the
// target table and ones naming the conflicting row.
func TestUpsertSetSubqueryReadsTheLiveTable(t *testing.T) {
	for _, tc := range []struct{ name, stmt string }{
		{"SET subquery over a table", `INSERT INTO u VALUES(1,20),(5,50),(2,30) ON CONFLICT(k) DO UPDATE SET v=(SELECT max(v) FROM u)`},
		{"SET subquery naming the conflicting row", `INSERT INTO u VALUES(1,20),(2,30) ON CONFLICT(k) DO UPDATE SET v=(SELECT u.v+1)`},
	} {
		differ(t, tc.name, []string{
			`CREATE TABLE u(k INTEGER PRIMARY KEY, v INTEGER)`,
			`INSERT INTO u VALUES(1,10),(2,11)`,
			tc.stmt,
			`SELECT k,v FROM u ORDER BY k`,
		})
	}
}

// TestTriggerBodyLimitNamesFiringRow checks LIMIT/OFFSET expressions
// naming the firing row in triggers. LIMIT and OFFSET are coded at runtime
// when they reference pseudo-rows.
func TestTriggerBodyLimitNamesFiringRow(t *testing.T) {
	base := []string{
		`CREATE TABLE t(a INTEGER, c INTEGER)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`CREATE TABLE u(b INTEGER)`,
		`INSERT INTO u VALUES(100),(200),(300)`,
		`CREATE TABLE dst(v INTEGER)`,
	}
	for _, c := range []struct {
		name string
		tail []string
	}{
		{"scan LIMIT new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.a; END`,
			`INSERT INTO t VALUES(2,0)`}},
		{"scan OFFSET new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT 1 OFFSET new.a; END`,
			`INSERT INTO t VALUES(1,0)`}},
		{"scan LIMIT old", []string{
			`CREATE TRIGGER tg AFTER DELETE ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT old.a; END`,
			`DELETE FROM t WHERE a = 2`}},
		{"scan LIMIT old OFFSET new", []string{
			`CREATE TRIGGER tg AFTER UPDATE ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT old.a OFFSET new.c; END`,
			`UPDATE t SET c = 1 WHERE a = 2`}},
		{"scan LIMIT arithmetic over new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.a + 1; END`,
			`INSERT INTO t VALUES(1,0)`}},
		// The counter takes the clause's ordinary INTEGER coercion.
		{"a TEXT firing value that is numeric", []string{
			`CREATE TABLE ts(a TEXT, c INTEGER)`,
			`CREATE TRIGGER tg AFTER INSERT ON ts BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.a; END`,
			`INSERT INTO ts VALUES('2',0)`}},
		// A NULL in LIMIT is a datatype mismatch that aborts.
		{"a NULL firing value is a datatype mismatch", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.c; END`,
			`INSERT INTO t VALUES(1,NULL)`}},
		// LIMIT of 0 yields no rows; negative is unlimited.
		{"a zero firing value yields no rows", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.c; END`,
			`INSERT INTO t VALUES(9,0)`}},
		{"a negative firing value is unlimited", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT new.c; END`,
			`INSERT INTO t VALUES(9,-1)`}},
		// A negative OFFSET skips nothing.
		{"a negative firing OFFSET skips nothing", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u LIMIT 2 OFFSET new.c; END`,
			`INSERT INTO t VALUES(9,-5)`}},
		// DISTINCT scan uses the same counter register.
		{"DISTINCT scan LIMIT new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT DISTINCT b FROM u LIMIT new.a; END`,
			`INSERT INTO t VALUES(2,0)`}},
		{"DISTINCT scan OFFSET new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT DISTINCT b FROM u LIMIT 1 OFFSET new.a; END`,
			`INSERT INTO t VALUES(1,0)`}},
		// FROM-less LIMIT in an upsert SET subquery.
		{"FROM-less LIMIT new inside an upsert SET subquery", []string{
			`CREATE TABLE k(a INTEGER PRIMARY KEY, c INTEGER)`,
			`INSERT INTO k VALUES(1,10)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO k VALUES(1,5) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT new.a); END`,
			`INSERT INTO t VALUES(1,1)`}},
		// Sorted body: the sorter bound must go unbounded when OFFSET is coded
		// at runtime.
		{"sorted LIMIT new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u ORDER BY b LIMIT new.a; END`,
			`INSERT INTO t VALUES(2,0)`}},
		{"sorted DESC LIMIT new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u ORDER BY b DESC LIMIT new.a; END`,
			`INSERT INTO t VALUES(2,0)`}},
		{"sorted LIMIT new OFFSET new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u ORDER BY b LIMIT new.a OFFSET new.c; END`,
			`INSERT INTO t VALUES(2,1)`}},
		{"sorted literal LIMIT with a coded OFFSET", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u ORDER BY b LIMIT 2 OFFSET new.c; END`,
			`INSERT INTO t VALUES(9,1)`}},
		{"sorted LIMIT new is a datatype mismatch on NULL", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT b FROM u ORDER BY b LIMIT new.c; END`,
			`INSERT INTO t VALUES(1,NULL)`}},
		{"sorted DISTINCT LIMIT new", []string{
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO dst SELECT DISTINCT b FROM u ORDER BY b LIMIT new.a; END`,
			`INSERT INTO t VALUES(2,0)`}},
		{"FROM-less LIMIT 0 from the firing row", []string{
			`CREATE TABLE k(a INTEGER PRIMARY KEY, c INTEGER)`,
			`INSERT INTO k VALUES(1,10)`,
			`CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO k VALUES(1,5) ON CONFLICT(a) DO UPDATE SET c=(SELECT 7 LIMIT new.c); END`,
			`INSERT INTO t VALUES(1,0)`}},
	} {
		script := append(append([]string(nil), base...), c.tail...)
		flLockstep(t, "trigger-limit-"+c.name, script,
			`SELECT v FROM dst ORDER BY v`,
			`SELECT a,c FROM t ORDER BY a,c`,
			`SELECT count(*) FROM dst`)
	}
}

// TestReturningSubqueryNamesAffectedRow checks RETURNING subqueries that
// name the affected row, which are evaluated once per returned row.
func TestReturningSubqueryNamesAffectedRow(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1),(2),(2)`,
	}
	differ(t, "returning subquery correlated to the affected row", append(append([]string(nil), setup...),
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s WHERE x=a)`,
		`SELECT a,b FROM t ORDER BY a`))
	// Qualified by the target table's own name.
	differ(t, "returning subquery qualified by the target table", append(append([]string(nil), setup...),
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s WHERE x=t.a)`,
		`SELECT a,b FROM t ORDER BY a`))
	// Control: uncorrelated subquery freezes at the first row.
	differ(t, "returning subquery naming nothing stays once", append(append([]string(nil), setup...),
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT count(*) FROM s)`,
		`SELECT a,b FROM t ORDER BY a`))
	// Nested subqueries reach through all nesting levels.
	differ(t, "returning subquery correlated one level deeper", append(append([]string(nil), setup...),
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT (SELECT count(*) FROM s WHERE x=a))`,
		`SELECT a,b FROM t ORDER BY a`))
	differ(t, "returning subquery correlated on UPDATE", []string{
		`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,10),(2,20)`,
		`CREATE TABLE s(x)`, `INSERT INTO s VALUES(1),(2),(2)`,
		`UPDATE t SET b=b+1 RETURNING a,(SELECT count(*) FROM s WHERE x=a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "returning subquery correlated on DELETE", []string{
		`CREATE TABLE t(a,b)`, `INSERT INTO t VALUES(1,10),(2,20)`,
		`CREATE TABLE s(x)`, `INSERT INTO s VALUES(1),(2),(2)`,
		`DELETE FROM t RETURNING a,(SELECT count(*) FROM s WHERE x=a)`,
		`SELECT count(*) FROM t`,
	})
	// The subquery's FROM wins the name over the pseudo-row.
	differ(t, "the subquery's own FROM shadows the affected row", []string{
		`CREATE TABLE t(a,b)`, `CREATE TABLE s(a)`, `INSERT INTO s VALUES(7),(8)`,
		`INSERT INTO t VALUES(1,'p'),(2,'q') RETURNING a,(SELECT sum(a) FROM s)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
}

// TestReturningSubqueryPastTheCorrelationSpan checks RETURNING subqueries
// after the write path's correlation span ends, including those reading
// the table being modified.
func TestReturningSubqueryPastTheCorrelationSpan(t *testing.T) {
	differ(t, "correlated SET and correlated RETURNING on one UPDATE", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x,y)`,
		`INSERT INTO s VALUES(1,'one'),(2,'two'),(2,'TWO'),(3,'three')`,
		`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
		`UPDATE t SET b=(SELECT y FROM s WHERE x=a) RETURNING a,(SELECT count(*) FROM s WHERE s.x=t.a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "correlated RETURNING on a DELETE", []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TABLE s(x,y)`,
		`INSERT INTO s VALUES(1,'one'),(2,'two'),(2,'TWO'),(3,'three')`,
		`INSERT INTO t VALUES(1,'p'),(2,'q'),(3,'r')`,
		`DELETE FROM t WHERE a=1 RETURNING a,(SELECT count(*) FROM s WHERE s.x=t.a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "correlated RETURNING reading the table the UPDATE is rewriting", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`UPDATE t SET b=b+1 RETURNING a,(SELECT count(*) FROM t WHERE t.b>a)`,
		`SELECT a,b FROM t ORDER BY a`,
	})
	differ(t, "correlated RETURNING reading the table the DELETE is emptying", []string{
		`CREATE TABLE t(a,b)`,
		`INSERT INTO t VALUES(1,10),(2,20),(3,30)`,
		`DELETE FROM t RETURNING a,(SELECT count(*) FROM t WHERE t.b>a)`,
		`SELECT count(*) FROM t`,
	})
}
