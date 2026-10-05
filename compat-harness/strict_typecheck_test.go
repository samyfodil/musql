// Tests STRICT table type checking for INSERT and UPDATE operations.
package compat

import "testing"

// TestStrictInsertTypes covers every STRICT datatype against every storage
// class of inserted value, plus the NULL rule (STRICT says nothing about
// nullability -- that is NOT NULL's job). The typeof() selects are what make
// the ACCEPTED rows load-bearing: a value coerced into the column's type by
// the affinity pass that precedes the check must be stored in that type.
func TestStrictInsertTypes(t *testing.T) {
	differ(t, "strict-insert-int", []string{
		`CREATE TABLE v(i INT) STRICT`,
		`INSERT INTO v VALUES(1)`,
		`INSERT INTO v VALUES(1.0)`,   // integral REAL folds to INTEGER
		`INSERT INTO v VALUES(1.5)`,   // REAL with a fraction: rejected
		`INSERT INTO v VALUES('123')`, // whole-string numeric TEXT converts
		`INSERT INTO v VALUES('1.5')`,
		`INSERT INTO v VALUES('abc')`,
		`INSERT INTO v VALUES('')`,
		`INSERT INTO v VALUES(' 12 ')`,
		`INSERT INTO v VALUES('1e3')`,
		`INSERT INTO v VALUES(x'01')`,
		`INSERT INTO v VALUES(NULL)`,
		`INSERT INTO v VALUES(9223372036854775808)`, // out of int64 range: a REAL literal
		`SELECT i, typeof(i) FROM v ORDER BY rowid`,
	})
	differ(t, "strict-insert-integer", []string{
		`CREATE TABLE v(i INTEGER) STRICT`,
		`INSERT INTO v VALUES(1)`,
		`INSERT INTO v VALUES(2.0)`,
		`INSERT INTO v VALUES(2.5)`,
		`INSERT INTO v VALUES('3')`,
		`INSERT INTO v VALUES(x'01')`,
		`INSERT INTO v VALUES(NULL)`,
		`SELECT i, typeof(i) FROM v ORDER BY rowid`,
	})
	differ(t, "strict-insert-real", []string{
		`CREATE TABLE v(r REAL) STRICT`,
		`INSERT INTO v VALUES(1)`, // REAL affinity forces an integer to float
		`INSERT INTO v VALUES(1.5)`,
		`INSERT INTO v VALUES('2')`,
		`INSERT INTO v VALUES('2.5')`,
		`INSERT INTO v VALUES('abc')`,
		`INSERT INTO v VALUES(x'01')`,
		`INSERT INTO v VALUES(NULL)`,
		`INSERT INTO v VALUES(9007199254740993)`,
		`SELECT r, typeof(r) FROM v ORDER BY rowid`,
	})
	differ(t, "strict-insert-text", []string{
		`CREATE TABLE v(t TEXT) STRICT`,
		`INSERT INTO v VALUES('a')`,
		`INSERT INTO v VALUES(1)`,   // TEXT affinity stringifies a number
		`INSERT INTO v VALUES(1.5)`, // ...including a REAL
		`INSERT INTO v VALUES(x'41')`,
		`INSERT INTO v VALUES(NULL)`,
		`SELECT t, typeof(t) FROM v ORDER BY rowid`,
	})
	differ(t, "strict-insert-blob", []string{
		`CREATE TABLE v(b BLOB) STRICT`,
		`INSERT INTO v VALUES(x'41')`,
		`INSERT INTO v VALUES('a')`, // BLOB has no affinity: nothing converts INTO one
		`INSERT INTO v VALUES(1)`,
		`INSERT INTO v VALUES(1.5)`,
		`INSERT INTO v VALUES(NULL)`,
		`SELECT b, typeof(b) FROM v ORDER BY rowid`,
	})
	differ(t, "strict-insert-any", []string{
		`CREATE TABLE v(y ANY) STRICT`,
		`INSERT INTO v VALUES(1)`,
		`INSERT INTO v VALUES(1.5)`,
		`INSERT INTO v VALUES('1')`, // ANY takes an unconverted TEXT '1', not the INTEGER 1
		`INSERT INTO v VALUES('abc')`,
		`INSERT INTO v VALUES(x'41')`,
		`INSERT INTO v VALUES(NULL)`,
		`SELECT y, typeof(y) FROM v ORDER BY rowid`,
	})
}

// TestStrictConstraintPrecedence pins where the datatype check sits among the
// other per-row constraints: after NOT NULL, before CHECK.
func TestStrictConstraintPrecedence(t *testing.T) {
	differ(t, "strict-notnull", []string{
		`CREATE TABLE n(a INT NOT NULL, b TEXT) STRICT`,
		`INSERT INTO n VALUES(NULL, x'01')`, // NOT NULL wins over a LATER column's type error
		`INSERT INTO n VALUES(1, x'01')`,
		`INSERT INTO n VALUES(1, 'ok')`,
		`INSERT INTO n VALUES(NULL, 'ok')`,
		`SELECT a,b,typeof(a),typeof(b) FROM n ORDER BY rowid`,
	})
	differ(t, "strict-check", []string{
		`CREATE TABLE pr(a INT NOT NULL CHECK(a<10), b TEXT) STRICT`,
		`INSERT INTO pr VALUES(NULL, x'01')`,
		`INSERT INTO pr VALUES(50, x'01')`, // a type error wins over an EARLIER column's CHECK
		`INSERT INTO pr VALUES(50, 'q')`,
		`INSERT INTO pr VALUES(5, 'q')`,
		`SELECT a,b FROM pr ORDER BY rowid`,
	})
	differ(t, "strict-first-offender", []string{
		`CREATE TABLE fo(a INT, b INT) STRICT`,
		`INSERT INTO fo VALUES(x'01', x'02')`,
		`SELECT a,b FROM fo`,
	})
	differ(t, "strict-defaults", []string{
		// A DEFAULT is affinity-coerced and then type-checked exactly like an
		// explicitly supplied value, so "c BLOB DEFAULT 3" makes the table
		// impossible to insert into at all.
		`CREATE TABLE d(a INT DEFAULT '5', b TEXT DEFAULT 7, c BLOB DEFAULT 3) STRICT`,
		`INSERT INTO d(a) VALUES(1)`,
		`INSERT INTO d DEFAULT VALUES`,
		`SELECT a,b,c,typeof(a),typeof(b),typeof(c) FROM d ORDER BY rowid`,
	})
	// NOT NULL ON CONFLICT REPLACE substitutes the column's DEFAULT for a
	// NULL. The C copies that DEFAULT expression RAW (insert.c:2013-2014) and
	// affinity-coerces the whole row afterwards; this emitter's affinity pass
	// runs before the NOT NULL loop, so the substituted value needs its own
	// (see emitInsertRowBody). The non-strict twins are here because the gap
	// is NOT STRICT-specific: it changed the stored TYPE on an ordinary table
	// too, which was a live wrong answer before this change.
	differ(t, "strict-notnull-replace-default", []string{
		`CREATE TABLE t(a INT NOT NULL ON CONFLICT REPLACE DEFAULT '7') STRICT`,
		`INSERT INTO t VALUES(NULL)`,
		`SELECT a, typeof(a) FROM t`,
	})
	differ(t, "strict-notnull-replace-default-real", []string{
		`CREATE TABLE t(a INT NOT NULL ON CONFLICT REPLACE DEFAULT 7.0) STRICT`,
		`INSERT INTO t VALUES(NULL)`,
		`SELECT a, typeof(a) FROM t`,
	})
	differ(t, "strict-notnull-replace-default-text-col", []string{
		`CREATE TABLE t(a TEXT NOT NULL ON CONFLICT REPLACE DEFAULT 7) STRICT`,
		`INSERT INTO t VALUES(NULL)`,
		`SELECT a, typeof(a) FROM t`,
	})
	differ(t, "strict-notnull-replace-default-blob-col", []string{
		// No affinity can turn 7 into a BLOB, so this one really is rejected.
		`CREATE TABLE t(a BLOB NOT NULL ON CONFLICT REPLACE DEFAULT 7) STRICT`,
		`INSERT INTO t VALUES(NULL)`,
		`SELECT a, typeof(a) FROM t`,
	})
	differ(t, "strict-notnull-replace-default-gen", []string{
		`CREATE TABLE t(a INT NOT NULL ON CONFLICT REPLACE DEFAULT '7', b TEXT AS (a) STORED NOT NULL, c INT NOT NULL ON CONFLICT REPLACE DEFAULT 3) STRICT`,
		`INSERT INTO t(a,c) VALUES(NULL,NULL)`,
		`SELECT a,b,c,typeof(a),typeof(b),typeof(c) FROM t`,
	})
	differ(t, "nonstrict-notnull-replace-default", []string{
		`CREATE TABLE t(a INT NOT NULL ON CONFLICT REPLACE DEFAULT '7')`,
		`INSERT INTO t VALUES(NULL)`,
		`SELECT a, typeof(a) FROM t`,
	})
	differ(t, "nonstrict-notnull-replace-default-text-col", []string{
		`CREATE TABLE t(a TEXT NOT NULL ON CONFLICT REPLACE DEFAULT 7)`,
		`INSERT INTO t VALUES(NULL)`,
		`SELECT a, typeof(a) FROM t`,
	})
	differ(t, "strict-multirow", []string{
		// The offending row aborts the whole statement; the rows before it are
		// rolled back with it.
		`CREATE TABLE ml(a INT, b TEXT) STRICT`,
		`INSERT INTO ml VALUES(1,'a'),(2,x'01'),(3,'c')`,
		`SELECT a,b FROM ml ORDER BY rowid`,
	})
}

// TestStrictRowidAlias pins the INTEGER PRIMARY KEY exclusion: SQLite leaves
// the rowid alias out of OP_TypeCheck (OP_MustBeInt at insert.c:1534 already
// guards it) and reports the generic "datatype mismatch" for it, which is NOT
// a STRICT-specific error -- an "a INT PRIMARY KEY" column is a real column
// and IS type-checked.
func TestStrictRowidAlias(t *testing.T) {
	differ(t, "strict-ipk", []string{
		`CREATE TABLE ip(a INTEGER PRIMARY KEY, b INT) STRICT`,
		`INSERT INTO ip VALUES('abc', 1)`,
		`INSERT INTO ip VALUES(2.5, 1)`,
		`INSERT INTO ip VALUES('5', 1)`, // numeric TEXT is a fine rowid
		`INSERT INTO ip VALUES(NULL, 2)`,
		`INSERT INTO ip VALUES(7, 'zz')`,
		`SELECT a,b,typeof(a) FROM ip ORDER BY rowid`,
	})
	differ(t, "strict-int-pk-not-alias", []string{
		`CREATE TABLE ip2(a INT PRIMARY KEY, b TEXT) STRICT`,
		`INSERT INTO ip2 VALUES('abc','x')`,
		`INSERT INTO ip2 VALUES(1,'x')`,
		`SELECT a,b,typeof(a) FROM ip2 ORDER BY rowid`,
	})
	differ(t, "strict-explicit-rowid", []string{
		`CREATE TABLE t(a INT, b TEXT) STRICT`,
		`INSERT INTO t(rowid, a, b) VALUES(1, 1, 'x')`,
		`INSERT INTO t(rowid, a, b) VALUES('zz', 1, 'x')`,
		`INSERT INTO t(rowid, a, b) VALUES(2, x'01', 'x')`,
		`SELECT rowid,a,b FROM t ORDER BY rowid`,
	})
	differ(t, "strict-update-rowid-target", []string{
		`CREATE TABLE ur(i INT, t TEXT) STRICT`,
		`INSERT INTO ur VALUES(1,'a')`,
		`UPDATE ur SET rowid='abc'`,
		`UPDATE ur SET rowid=5`,
		`SELECT rowid,i,t FROM ur`,
	})
	differ(t, "strict-autoincrement", []string{
		`CREATE TABLE ai(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT) STRICT`,
		`INSERT INTO ai(b) VALUES('x')`,
		`INSERT INTO ai(b) VALUES(1)`,
		`SELECT a,b,typeof(b) FROM ai ORDER BY rowid`,
	})
	differ(t, "strict-without-rowid", []string{
		`CREATE TABLE wr(k TEXT PRIMARY KEY, v INT) STRICT, WITHOUT ROWID`,
		`INSERT INTO wr VALUES('a',1)`,
		`INSERT INTO wr VALUES('b','zz')`,
		`INSERT INTO wr VALUES(1,'zz')`,
		`UPDATE wr SET v='qq'`,
		`SELECT k,v FROM wr`,
	})
}

// TestStrictNotSoftenedByConflictClause pins that SQLITE_CONSTRAINT_DATATYPE
// comes straight out of the opcode (vdbe.c:3391) rather than through the
// conflict handler, so no OR-clause turns a type violation into a skip or a
// replace -- unlike NOT NULL, whose OR IGNORE really does skip the row (the
// first case here shows both behaviours in one table).
func TestStrictNotSoftenedByConflictClause(t *testing.T) {
	differ(t, "strict-or-ignore-vs-notnull", []string{
		`CREATE TABLE t(i INT NOT NULL, b TEXT) STRICT`,
		`INSERT OR IGNORE INTO t VALUES(NULL, x'01')`, // NOT NULL: skipped, no error
		`INSERT OR IGNORE INTO t VALUES(1, x'01')`,    // type: hard error anyway
		`SELECT count(*) FROM t`,
	})
	differ(t, "strict-insert-or-clauses", []string{
		`CREATE TABLE oc(i INT, u INT UNIQUE) STRICT`,
		`INSERT INTO oc VALUES(1,1)`,
		`INSERT OR IGNORE INTO oc VALUES(x'01',1)`,
		`INSERT OR REPLACE INTO oc VALUES(x'01',1)`,
		`INSERT OR ROLLBACK INTO oc VALUES(x'01',4)`,
		`INSERT OR FAIL INTO oc VALUES(x'01',5)`,
		`SELECT i,u FROM oc ORDER BY rowid`,
	})
	differ(t, "strict-update-or-clauses", []string{
		`CREATE TABLE ui(i INT, u INT UNIQUE) STRICT`,
		`INSERT INTO ui VALUES(1,1)`,
		`UPDATE ui SET i='abc'`,
		`UPDATE OR IGNORE ui SET i='abc'`,
		`UPDATE OR REPLACE ui SET i='abc'`,
		`SELECT i,u FROM ui`,
	})
}

// TestStrictUpdate covers the UPDATE half: every declared type, the NOT NULL
// interaction, a WHERE that selects only some rows, and the arithmetic that
// silently changes a value's storage class (int64 overflow promotes to REAL,
// which an INT column then refuses).
func TestStrictUpdate(t *testing.T) {
	differ(t, "strict-update-types", []string{
		`CREATE TABLE u(i INT, t TEXT, b BLOB, y ANY) STRICT`,
		`INSERT INTO u VALUES(1,'x',x'01',5)`,
		`UPDATE u SET t='y'`,
		`UPDATE u SET i='abc'`,
		`UPDATE u SET i='7'`,
		`UPDATE u SET i=2.0`,
		`UPDATE u SET i=2.5`,
		`UPDATE u SET t=x'01'`,
		`UPDATE u SET b='q'`,
		`UPDATE u SET y=x'ff'`,
		`SELECT i,t,b,y,typeof(i),typeof(t),typeof(b),typeof(y) FROM u`,
	})
	differ(t, "strict-update-real", []string{
		`CREATE TABLE t(a REAL) STRICT`,
		`INSERT INTO t VALUES(1)`,
		`UPDATE t SET a=2`,
		`UPDATE t SET a='3'`,
		`UPDATE t SET a=x'01'`,
		`SELECT a, typeof(a) FROM t`,
	})
	differ(t, "strict-update-overflow", []string{
		// a+1 overflows int64 and becomes a REAL, which the INT column refuses.
		`CREATE TABLE t(a INT) STRICT`,
		`INSERT INTO t VALUES(9223372036854775807)`,
		`UPDATE t SET a=a+1`,
		`UPDATE t SET a=a||'x'`,
		`SELECT a, typeof(a) FROM t`,
	})
	differ(t, "strict-update-notnull", []string{
		`CREATE TABLE un(a INT NOT NULL, b TEXT) STRICT`,
		`INSERT INTO un VALUES(1,'x')`,
		`UPDATE un SET a=NULL`,
		`UPDATE un SET a=NULL, b=x'01'`,
		`UPDATE un SET a='q', b=x'01'`,
		`SELECT a,b FROM un`,
	})
	differ(t, "strict-update-where", []string{
		`CREATE TABLE uw(i INT, t TEXT) STRICT`,
		`INSERT INTO uw VALUES(1,'a'),(2,'b')`,
		`UPDATE uw SET i='zz' WHERE t='b'`, // the offending row aborts the statement
		`UPDATE uw SET t='c' WHERE i=1`,
		`SELECT i,t FROM uw ORDER BY rowid`,
	})
	differ(t, "strict-update-ipk", []string{
		`CREATE TABLE up(a INTEGER PRIMARY KEY, b INT) STRICT`,
		`INSERT INTO up VALUES(1,1)`,
		`UPDATE up SET a='abc'`,
		`UPDATE up SET a='5'`,
		`UPDATE up SET b=x'01'`,
		`SELECT a,b,typeof(a),typeof(b) FROM up`,
	})
}

// TestStrictGeneratedColumns is the group that was WRONG before this change:
// every "UPDATE ... SET" below stored a row C SQLite rejects, because the
// path that ran then type-checked the generated column's stale value. STORED and
// VIRTUAL both, and the ANY-source cases show it is the generated column's own
// declared type that is violated, not the assigned column's.
func TestStrictGeneratedColumns(t *testing.T) {
	differ(t, "strict-gencol-insert", []string{
		`CREATE TABLE g(a INT, b TEXT AS (a) STORED, c TEXT AS (a) VIRTUAL) STRICT`,
		`INSERT INTO g(a) VALUES(1)`,
		`SELECT a,b,c,typeof(a),typeof(b),typeof(c) FROM g`,
	})
	differ(t, "strict-gencol-insert-blob-stored", []string{
		`CREATE TABLE g(a INT, b BLOB AS (a) STORED) STRICT`,
		`INSERT INTO g(a) VALUES(1)`,
		`SELECT a,b FROM g`,
	})
	differ(t, "strict-gencol-insert-blob-virtual", []string{
		`CREATE TABLE g(a INT, b BLOB AS (a) VIRTUAL) STRICT`,
		`INSERT INTO g(a) VALUES(1)`,
		`SELECT a,b FROM g`,
	})
	differ(t, "strict-gencolUpdate-stored", []string{
		`CREATE TABLE g(a INT, b BLOB AS (a) STORED) STRICT`,
		`INSERT INTO g(a) VALUES(NULL)`,
		`UPDATE g SET a=1`,
		`SELECT a, typeof(a) FROM g`,
	})
	differ(t, "strict-gencolUpdate-virtual", []string{
		`CREATE TABLE g(a INT, b BLOB AS (a) VIRTUAL) STRICT`,
		`INSERT INTO g(a) VALUES(NULL)`,
		`UPDATE g SET a=1`,
		`SELECT a, typeof(a) FROM g`,
	})
	differ(t, "strict-gencolUpdate-any-stored", []string{
		`CREATE TABLE h(a ANY, b BLOB AS (a) STORED) STRICT`,
		`INSERT INTO h(a) VALUES(x'01')`,
		`UPDATE h SET a=1`,
		`SELECT a, b, typeof(a), typeof(b) FROM h`,
	})
	differ(t, "strict-gencolUpdate-any-virtual", []string{
		`CREATE TABLE h(a ANY, b BLOB AS (a) VIRTUAL) STRICT`,
		`INSERT INTO h(a) VALUES(x'01')`,
		`UPDATE h SET a=1`,
		`SELECT a, b, typeof(a), typeof(b) FROM h`,
	})
	differ(t, "strict-gencolUpdate-ok", []string{
		// The same shape that STAYS legal: the recomputed value fits.
		`CREATE TABLE ug(a INT, b TEXT AS (a) STORED) STRICT`,
		`INSERT INTO ug(a) VALUES(1)`,
		`UPDATE ug SET a=2`,
		`SELECT a,b,typeof(a),typeof(b) FROM ug`,
	})
}

// TestStrictOtherWritePaths covers the remaining emitters the STRICT decline
// used to cover wholesale: INSERT ... SELECT, the upsert DO UPDATE tail,
// RETURNING, triggers, and a foreign-key child.
func TestStrictOtherWritePaths(t *testing.T) {
	differ(t, "strict-insert-select", []string{
		`CREATE TABLE src(a, b)`,
		`INSERT INTO src VALUES(1,'x'),(x'01','y')`,
		`CREATE TABLE dst(i INT, t TEXT) STRICT`,
		`INSERT INTO dst SELECT a,b FROM src`,
		`SELECT i,t FROM dst ORDER BY rowid`,
		`INSERT INTO dst SELECT a,b FROM src WHERE a=1`,
		`SELECT i,t FROM dst ORDER BY rowid`,
	})
	differ(t, "strict-insert-select-no-from", []string{
		`CREATE TABLE t(a INT, b TEXT) STRICT`,
		`INSERT INTO t SELECT 1,'x'`,
		`INSERT INTO t SELECT x'01','x'`,
		`INSERT INTO t SELECT 2,'y' UNION ALL SELECT x'02','z'`,
		`SELECT a,b FROM t ORDER BY rowid`,
	})
	differ(t, "strict-deferred-default", []string{
		// CURRENT_TIMESTAMP is TEXT; an INT column refuses it, and the
		// DEFAULT is evaluated per INSERT rather than folded, so the check
		// sees the clock reading and not a literal.
		`CREATE TABLE t(a INT, ts INT DEFAULT CURRENT_TIMESTAMP) STRICT`,
		`INSERT INTO t(a) VALUES(1)`,
		`SELECT a FROM t`,
	})
	differ(t, "strict-where-subquery", []string{
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(1)`,
		`CREATE TABLE t(a INT, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`UPDATE t SET a=x'01' WHERE a IN (SELECT x FROM s)`,
		`SELECT a,b FROM t`,
	})
	differ(t, "strict-insert-select-strict-source", []string{
		`CREATE TABLE p(i INT) STRICT`,
		`INSERT INTO p VALUES(1)`,
		`CREATE TABLE q(j INT) STRICT`,
		`INSERT INTO q SELECT i FROM p`,
		`SELECT j FROM q`,
	})
	differ(t, "strict-upsert", []string{
		`CREATE TABLE us(k INT PRIMARY KEY, v TEXT) STRICT`,
		`INSERT INTO us VALUES(1,'a')`,
		`INSERT INTO us VALUES(1,'b') ON CONFLICT(k) DO UPDATE SET v='c'`,
		`INSERT INTO us VALUES(1,'b') ON CONFLICT(k) DO UPDATE SET v=x'01'`,
		`INSERT INTO us VALUES(1,'b') ON CONFLICT(k) DO UPDATE SET v=5`,
		`INSERT INTO us VALUES(2,x'01') ON CONFLICT(k) DO NOTHING`,
		`SELECT k,v,typeof(v) FROM us ORDER BY rowid`,
	})
	differ(t, "strict-returning", []string{
		`CREATE TABLE rt(i INT, t TEXT) STRICT`,
		`INSERT INTO rt VALUES(1,'a') RETURNING i,t,rowid`,
		`INSERT INTO rt VALUES(x'01','a') RETURNING i,t`,
		`UPDATE rt SET t='b' RETURNING i,t`,
		`UPDATE rt SET i=x'01' RETURNING i,t`,
		`SELECT i,t FROM rt`,
	})
	differ(t, "strict-after-insert-trigger", []string{
		`CREATE TABLE tr(i INT, t TEXT) STRICT`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER ai AFTER INSERT ON tr BEGIN INSERT INTO log VALUES(new.i); END`,
		`INSERT INTO tr VALUES(1,'a')`,
		`INSERT INTO tr VALUES(x'01','a')`,
		`SELECT x FROM log`,
		`SELECT i,t FROM tr`,
	})
	differ(t, "strict-before-insert-trigger-raise-ignore", []string{
		// The datatype check runs BEFORE the BEFORE trigger (insert.c:1491
		// then :1495), so RAISE(IGNORE) does not rescue the offending row.
		`CREATE TABLE t(i INT, b TEXT) STRICT`,
		`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN SELECT RAISE(IGNORE); END`,
		`INSERT INTO t VALUES(1,'a')`,
		`INSERT INTO t VALUES(x'01','a')`,
		`SELECT count(*) FROM t`,
	})
	differ(t, "strict-before-update-trigger-raise-ignore", []string{
		`CREATE TABLE t(i INT, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'a')`,
		`CREATE TRIGGER bu BEFORE UPDATE ON t BEGIN SELECT RAISE(IGNORE); END`,
		`UPDATE t SET i=x'01'`,
		`SELECT i,b FROM t`,
	})
	differ(t, "strict-after-update-trigger", []string{
		`CREATE TABLE tu(i INT, t TEXT) STRICT`,
		`CREATE TABLE log(x)`,
		`CREATE TRIGGER au AFTER UPDATE ON tu BEGIN INSERT INTO log VALUES(new.i); END`,
		`INSERT INTO tu VALUES(1,'a')`,
		`UPDATE tu SET i=2`,
		`UPDATE tu SET i=x'01'`,
		`SELECT x FROM log`,
		`SELECT i,t FROM tu`,
	})
	differ(t, "strict-indexed", []string{
		`CREATE TABLE t(a INT, b TEXT) STRICT`,
		`CREATE INDEX ia ON t(a)`,
		`CREATE UNIQUE INDEX ib ON t(b)`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(2,'x')`,
		`INSERT INTO t VALUES(x'01','y')`,
		`UPDATE t SET a=x'01'`,
		`UPDATE t SET b='z'`,
		`SELECT a,b FROM t ORDER BY rowid`,
	})
	differ(t, "strict-temp-catalog", []string{
		`CREATE TEMP TABLE t(a INT, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(x'01','x')`,
		`UPDATE t SET a='zz'`,
		`SELECT a,b FROM t`,
	})
	differ(t, "strict-schema-qualified", []string{
		`CREATE TABLE t(a INT, b TEXT) STRICT`,
		`INSERT INTO main.t VALUES(1,'x')`,
		`INSERT INTO main.t VALUES(x'01','x')`,
		`UPDATE main.t SET a='zz'`,
		`SELECT a,b FROM t`,
	})
	differ(t, "strict-aliased-update", []string{
		`CREATE TABLE t(a INT, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`UPDATE t AS z SET a=x'01'`,
		`UPDATE t AS z SET a=2`,
		`SELECT a,b FROM t`,
	})
	differ(t, "strict-set-subquery", []string{
		`CREATE TABLE s(x)`,
		`INSERT INTO s VALUES(x'01')`,
		`CREATE TABLE t(a INT, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`UPDATE t SET a=(SELECT x FROM s)`,
		`SELECT a,b FROM t`,
	})
	differ(t, "strict-declared-conflict-insert", []string{
		`CREATE TABLE t(a INT UNIQUE ON CONFLICT IGNORE, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`INSERT INTO t VALUES(1,'y')`,
		`INSERT INTO t VALUES(2,x'01')`,
		`SELECT a,b FROM t ORDER BY rowid`,
	})
	differ(t, "strict-replace-into", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT) STRICT`,
		`INSERT INTO t VALUES(1,'x')`,
		`REPLACE INTO t VALUES(1,'y')`,
		`REPLACE INTO t VALUES(1,x'01')`,
		`SELECT a,b FROM t`,
	})
	differ(t, "strict-ignore-check-constraints", []string{
		// PRAGMA ignore_check_constraints=ON suppresses the CHECK block; the
		// datatype check must survive it. The C emits OP_TypeCheck from
		// sqlite3TableAffinity, reachable from three places in
		// sqlite3GenerateConstraintChecks, not only the CHECK block.
		`CREATE TABLE t(a INT CHECK(a<10), b TEXT) STRICT`,
		`PRAGMA ignore_check_constraints=ON`,
		`INSERT INTO t VALUES(50,'x')`,
		`INSERT INTO t VALUES(x'01','x')`,
		`UPDATE t SET a=x'01'`,
		`SELECT a,b FROM t`,
	})
	differ(t, "strict-column-subset-gen", []string{
		`CREATE TABLE t(a INT, b INT AS (a*2) STORED, c TEXT) STRICT`,
		`INSERT INTO t(a,c) VALUES(1,'x')`,
		`INSERT INTO t(c,a) VALUES('y',2)`,
		`INSERT INTO t(a,c) VALUES(x'01','z')`,
		`UPDATE t SET a=3 WHERE c='x'`,
		`SELECT a,b,c FROM t ORDER BY rowid`,
	})
	differ(t, "strict-foreign-key-child", []string{
		`CREATE TABLE fk_p(a INT PRIMARY KEY) STRICT`,
		`CREATE TABLE fk_c(b INT REFERENCES fk_p(a)) STRICT`,
		`PRAGMA foreign_keys=ON`,
		`INSERT INTO fk_p VALUES(1)`,
		`INSERT INTO fk_c VALUES(1)`,
		`INSERT INTO fk_c VALUES('x')`,
		`SELECT b FROM fk_c`,
	})
}
