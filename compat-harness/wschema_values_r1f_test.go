// Tests direct sqlite_master write VALUES boundary.
// every case below is just "an UPDATE/DELETE/INSERT, against this one table".
//
// Every case runs its catalog edits inside BEGIN ... ROLLBACK on purpose, and
// that is load-bearing twice over. It keeps this file honest about WHERE the
// behavior lives -- a writable_schema edit is only ever observable to the
// connection that made it, and driver's autocommit model opens a fresh
// engine session per statement (see writable_schema_write_test.go's own
// header), so an edit left to commit would be read back by what is, for this
// engine, a REOPEN, and every case would then be measuring the reopen rather
// than the write. Inside an explicit transaction the whole script shares one
// session, which is C SQLite's shape. It also means no case ever leaves a
// deliberately-broken catalog on disk for the next statement to trip over.
package compat

import "testing"

// wsValuesRead is the catalog read every case ends on. rootpage is
// deliberately absent: reading it is a SEPARATE, still-live decline
// (schemaCatalogQueryGuard, engine/query.go -- this writer's page numbering
// legitimately differs from the oracle's), and pulling it in here would make
// these cases fail for a reason that has nothing to do with the boundary they
// exist to gate. typeof() is included because the catalog's declared
// TEXT/TEXT/TEXT/INT/TEXT affinities are applied to whatever the SET list or
// the VALUES tuple produced, and a lost affinity is invisible in the value
// alone.
const wsValuesRead = `SELECT type, name, tbl_name, quote(sql), ` +
	`typeof(type), typeof(name), typeof(tbl_name), typeof(sql) ` +
	`FROM sqlite_master ORDER BY name, type`

// TestWSValuesR1FWhereSelects pins what an UPDATE's/DELETE's WHERE selects --
// the one question writeRowSelected (engine/vtab_write.go) now answers for the
// catalog loops as well as the four writable-vtab ones. Every WHERE here is a
// bare non-boolean expression, so the case measures SQLite's truthiness rule
// and nothing else.
func TestWSValuesR1FWhereSelects(t *testing.T) {
	for _, c := range []struct {
		name  string
		where string
	}{
		{"where a text literal that is not a number", `'abc'`},
		{"where a text literal with a numeric prefix", `'2xyz'`},
		{"where the text zero", `'0'`},
		{"where NULL", `NULL`},
		{"where a nonzero real", `0.5`},
		{"where zero", `0`},
		{"where a column of the row itself", `name > 't1'`},
		{"where a subquery over a real table", `(SELECT count(*) FROM t1) > 0`},
	} {
		t.Run(c.name, func(t *testing.T) {
			differ(t, "writable_schema UPDATE/DELETE WHERE "+c.where, []string{
				`CREATE TABLE t1(a,b)`,
				`CREATE TABLE t2(a)`,
				`CREATE TABLE t3(a)`,
				`INSERT INTO t1 VALUES(1,2)`,
				`PRAGMA writable_schema=ON`,
				`BEGIN`,
				`UPDATE sqlite_master SET tbl_name='hit' WHERE ` + c.where,
				wsValuesRead,
				`ROLLBACK`,
				wsValuesRead,
				`BEGIN`,
				`DELETE FROM sqlite_master WHERE ` + c.where,
				wsValuesRead,
				`SELECT count(*) FROM sqlite_master`,
				`ROLLBACK`,
				wsValuesRead,
			})
		})
	}
}

// TestWSValuesR1FSetListSeesTheOldRow is the load-bearing case for the SET
// half. Every right-hand side is evaluated against the row as it stood BEFORE
// the statement -- update.c:954 codes each changed column inside the scan
// positioned on the current row -- so "SET name='n2', tbl_name=name" must
// leave tbl_name reading the OLD name, never 'n2'. A boundary that seeded its
// value row wrong, or that read an assignment back out of a slot a LATER
// assignment had already overwritten, diverges here on the first case.
func TestWSValuesR1FSetListSeesTheOldRow(t *testing.T) {
	for _, c := range []struct {
		name string
		set  string
	}{
		{"a later target reads an earlier one's OLD value", `name='n2', tbl_name=name, sql=name||'/'||tbl_name`},
		{"the same target assigned twice", `sql='first', sql='second'`},
		{"a target assigned from its own old value", `sql=sql||'!', name=name||'!'`},
		{"an unassigned column keeps what it had", `sql='only sql moves'`},
		{"a subquery right-hand side", `sql=(SELECT count(*) FROM t1)`},
		{"a NULL right-hand side", `sql=NULL, tbl_name=NULL`},
	} {
		t.Run(c.name, func(t *testing.T) {
			differ(t, "writable_schema UPDATE SET "+c.set, []string{
				`CREATE TABLE t1(a,b)`,
				`CREATE INDEX i1 ON t1(a)`,
				`INSERT INTO t1 VALUES(1,2)`,
				`PRAGMA writable_schema=ON`,
				`BEGIN`,
				`UPDATE sqlite_master SET ` + c.set + ` WHERE name='t1'`,
				wsValuesRead,
				`ROLLBACK`,
				wsValuesRead,
			})
		})
	}
}

// TestWSValuesR1FSetAffinity pins the OTHER thing the SET boundary owes: the
// catalog's declared TEXT/TEXT/TEXT/INT/TEXT affinities are applied to what
// each right-hand side produced, exactly as they are for any other table.
func TestWSValuesR1FSetAffinity(t *testing.T) {
	differ(t, "writable_schema UPDATE applies the catalog's affinities", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA writable_schema=ON`,
		`BEGIN`,
		`UPDATE sqlite_master SET type=1, name=2.5, tbl_name='3', sql=x'6162' WHERE name='t1'`,
		wsValuesRead,
		`ROLLBACK`,
		`BEGIN`,
		`UPDATE sqlite_master SET type=2.0, name=x'00', sql=9 WHERE name='t1'`,
		wsValuesRead,
		`ROLLBACK`,
		wsValuesRead,
	})
}

// TestWSValuesR1FInsertRowValues pins the INSERT tuple: which slot each
// expression lands in with and without a column list, what the columns the
// statement did not name hold, and the affinities applied on the way in.
func TestWSValuesR1FInsertRowValues(t *testing.T) {
	for _, c := range []struct {
		name string
		ins  string
	}{
		{"positional, five values, mixed types", `INSERT INTO sqlite_master VALUES(1, 2.5, '3', 4.0, x'6162')`},
		{"a column list in declaration order", `INSERT INTO sqlite_master(type,name) VALUES('x','y')`},
		{"a column list OUT of declaration order", `INSERT INTO sqlite_master(sql,name,type) VALUES('s','n','t')`},
		{"expressions, not literals", `INSERT INTO sqlite_master(type,name,sql) VALUES(upper('t'), 'a'||'b', length('abcd'))`},
		{"two tuples at once", `INSERT INTO sqlite_master(type,name) VALUES('p','q'),('r','s')`},
		{"a subquery cell", `INSERT INTO sqlite_master(type,name) VALUES('c', (SELECT count(*) FROM t1))`},
	} {
		t.Run(c.name, func(t *testing.T) {
			differ(t, "writable_schema "+c.ins, []string{
				`CREATE TABLE t1(a)`,
				`INSERT INTO t1 VALUES(7)`,
				`PRAGMA writable_schema=ON`,
				`BEGIN`,
				c.ins,
				wsValuesRead,
				`SELECT count(*) FROM sqlite_master`,
				`ROLLBACK`,
				wsValuesRead,
			})
		})
	}
}

// TestWSValuesR1FInsertArityPrecedesEvaluation pins that the arity check is a
// PREPARE-time property, decided over EVERY tuple before any of them is
// turned into values (insert.c:1249-1253, reached before the coding loop at
// insert.c:1431). A two-tuple VALUES whose second tuple is short inserts
// neither.
func TestWSValuesR1FInsertArityPrecedesEvaluation(t *testing.T) {
	differ(t, "writable_schema INSERT arity is checked before evaluation", []string{
		`CREATE TABLE t1(a)`,
		`PRAGMA writable_schema=ON`,
		`BEGIN`,
		`INSERT INTO sqlite_master VALUES('table','ok','ok',0,'x'),(1,2,3)`,
		`SELECT count(*) FROM sqlite_master`,
		wsValuesRead,
		`INSERT INTO sqlite_master VALUES(1,2,3,4)`,
		`SELECT count(*) FROM sqlite_master`,
		`ROLLBACK`,
		wsValuesRead,
	})
}
