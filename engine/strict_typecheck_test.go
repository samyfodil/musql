// STRICT table type checks: verify exact error text and compilation of OpTypeCheck.
package engine

import (
	"path/filepath"
	"testing"
)

func newStrictTestDB(t *testing.T, setup ...string) *Session {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "t.musq"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	return db
}

// strictWantErr runs sql and requires it to fail with exactly want.
func strictWantErr(t *testing.T, db *Session, sql, want string) {
	t.Helper()
	err := db.Exec(sql)
	if err == nil {
		t.Fatalf("%s: succeeded, want error %q", sql, want)
	}
	if err.Error() != want {
		t.Fatalf("%s:\n got %q\nwant %q", sql, err.Error(), want)
	}
}

// TestStrictTypeErrorText pins one message per (declared type x offending
// storage class) pair, for INSERT. The oracle reports the message without the
// "engine: INSERT into <t>: " prefix this engine's write path prepends to every
// constraint error (the same prefix a NOT NULL or CHECK failure carries here),
// so only the tail after the last ": " is SQLite's own text.
func TestStrictTypeErrorText(t *testing.T) {
	db := newStrictTestDB(t, `CREATE TABLE v(i INT, r REAL, t TEXT, b BLOB, y ANY) STRICT`)
	cases := []struct{ sql, want string }{
		{`INSERT INTO v(i) VALUES(1.5)`, `engine: INSERT into v: cannot store REAL value in INT column v.i`},
		{`INSERT INTO v(i) VALUES('abc')`, `engine: INSERT into v: cannot store TEXT value in INT column v.i`},
		{`INSERT INTO v(i) VALUES(x'01')`, `engine: INSERT into v: cannot store BLOB value in INT column v.i`},
		{`INSERT INTO v(r) VALUES('abc')`, `engine: INSERT into v: cannot store TEXT value in REAL column v.r`},
		{`INSERT INTO v(r) VALUES(x'01')`, `engine: INSERT into v: cannot store BLOB value in REAL column v.r`},
		{`INSERT INTO v(t) VALUES(x'01')`, `engine: INSERT into v: cannot store BLOB value in TEXT column v.t`},
		{`INSERT INTO v(b) VALUES(1)`, `engine: INSERT into v: cannot store INT value in BLOB column v.b`},
		{`INSERT INTO v(b) VALUES(1.5)`, `engine: INSERT into v: cannot store REAL value in BLOB column v.b`},
		{`INSERT INTO v(b) VALUES('a')`, `engine: INSERT into v: cannot store TEXT value in BLOB column v.b`},
	}
	for _, c := range cases {
		strictWantErr(t, db, c.sql, c.want)
	}
	// "y ANY" accepts every one of those values, unconverted, and NULL is
	// always accepted in every column -- STRICT's rule says nothing about
	// nullability. Both are the negative half of the same check.
	for _, s := range []string{
		`INSERT INTO v(y) VALUES(1)`, `INSERT INTO v(y) VALUES(1.5)`,
		`INSERT INTO v(y) VALUES('abc')`, `INSERT INTO v(y) VALUES(x'01')`,
		`INSERT INTO v(i,r,t,b,y) VALUES(NULL,NULL,NULL,NULL,NULL)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// TestStrictTypeErrorTextIntegerSpelling pins the one asymmetry in the message:
// the VALUE's integer class is spelled "INT" (vdbeMemTypeName, vdbe.c:833) but
// a COLUMN declared INTEGER is spelled "INTEGER" (sqlite3StdType, global.c:394),
// so the two words can appear in the same sentence.
func TestStrictTypeErrorTextIntegerSpelling(t *testing.T) {
	db := newStrictTestDB(t, `CREATE TABLE g(a INTEGER, d BLOB) STRICT`)
	strictWantErr(t, db, `INSERT INTO g(a) VALUES(1.5)`,
		`engine: INSERT into g: cannot store REAL value in INTEGER column g.a`)
	strictWantErr(t, db, `INSERT INTO g(d) VALUES(1)`,
		`engine: INSERT into g: cannot store INT value in BLOB column g.d`)
}

// TestStrictTypeErrorTextUpdate is the UPDATE half, including the NOT NULL
// interaction (NOT NULL is reported first when it fires on the same row) and
// the fact that no OR-clause softens a datatype violation.
func TestStrictTypeErrorTextUpdate(t *testing.T) {
	db := newStrictTestDB(t,
		`CREATE TABLE u(i INT, t TEXT, b BLOB, y ANY, n INT NOT NULL) STRICT`,
		`INSERT INTO u VALUES(1,'x',x'01',5,1)`,
	)
	strictWantErr(t, db, `UPDATE u SET i='abc'`,
		`engine: UPDATE u: cannot store TEXT value in INT column u.i`)
	strictWantErr(t, db, `UPDATE u SET t=x'01'`,
		`engine: UPDATE u: cannot store BLOB value in TEXT column u.t`)
	strictWantErr(t, db, `UPDATE u SET b='q'`,
		`engine: UPDATE u: cannot store TEXT value in BLOB column u.b`)
	// NOT NULL first, on the same statement that also violates a type.
	strictWantErr(t, db, `UPDATE u SET n=NULL, i='abc'`,
		`engine: UPDATE u: NOT NULL constraint failed: u.n`)
	// ANY takes anything; the row survives all of the above unchanged.
	if err := db.Exec(`UPDATE u SET y=x'ff'`); err != nil {
		t.Fatalf("UPDATE u SET y=x'ff': %v", err)
	}
}

// TestStrictTypeErrorRowidAlias pins the INTEGER PRIMARY KEY exclusion's OWN
// message: SQLite validates the rowid alias through OP_MustBeInt (insert.c:1534)
// rather than OP_TypeCheck, so it reports the generic "datatype mismatch" --
// byte-identical to the same INSERT into the same table WITHOUT the STRICT
// keyword. An "a INT PRIMARY KEY" column is NOT the rowid alias and gets the
// STRICT message.
func TestStrictTypeErrorRowidAlias(t *testing.T) {
	db := newStrictTestDB(t,
		`CREATE TABLE ip(a INTEGER PRIMARY KEY, b INT) STRICT`,
		`CREATE TABLE ip2(a INT PRIMARY KEY, b TEXT) STRICT`,
	)
	// OP_MustBeInt runs before the NOT NULL loop and the STRICT check that
	// follows it, so its bare message is what a STRICT table reports too.
	strictWantErr(t, db, `INSERT INTO ip VALUES('abc', 1)`,
		`engine: datatype mismatch`)
	strictWantErr(t, db, `INSERT INTO ip VALUES(2.5, 1)`,
		`engine: datatype mismatch`)
	strictWantErr(t, db, `INSERT INTO ip2 VALUES('abc','x')`,
		`engine: INSERT into ip2: cannot store TEXT value in INT column ip2.a`)
	// The rowid alias itself takes a numeric TEXT and an integral REAL.
	for _, s := range []string{`INSERT INTO ip VALUES('5', 1)`, `INSERT INTO ip VALUES(6.0, 1)`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// TestStrictTypeErrorNotSoftened pins that a datatype violation is unsoftenable:
// SQLite raises SQLITE_CONSTRAINT_DATATYPE straight out of the opcode
// (vdbe.c:3391) instead of routing it through the conflict handler, so every
// OR-clause still fails with the same message. On the INSERT side (and for
// "UPDATE OR ABORT") that is OpTypeCheck's own doing: it returns a plain error
// rather than a conflictHalt, so writeCtx.resolveHalt treats it as ABORT no
// matter what the statement asked for. The remaining UPDATE OR-clauses were
// promoted later, by the conflict-handling cluster (updatePlan.conflictAware,
// vdbe_write.go), so those rows pin that every OR spelling reaches the SAME
// message down the same path -- which is the point: the five must not drift.
func TestStrictTypeErrorNotSoftened(t *testing.T) {
	db := newStrictTestDB(t,
		`CREATE TABLE oc(i INT, u INT UNIQUE) STRICT`,
		`INSERT INTO oc VALUES(1,1)`,
	)
	for _, or := range []string{"OR IGNORE", "OR REPLACE", "OR FAIL", "OR ROLLBACK", "OR ABORT"} {
		strictWantErr(t, db, `INSERT `+or+` INTO oc VALUES(x'01',1)`,
			`engine: INSERT into oc: cannot store BLOB value in INT column oc.i`)
	}
	for _, or := range []string{"OR IGNORE", "OR REPLACE", "OR FAIL", "OR ROLLBACK", "OR ABORT"} {
		strictWantErr(t, db, `UPDATE `+or+` oc SET i='abc'`,
			`engine: UPDATE oc: cannot store TEXT value in INT column oc.i`)
	}
}

// TestStrictTypeErrorGeneratedColumn pins the generated-column message, and
// with it the ordering fix: the check runs on the RECOMPUTED value, so an
// UPDATE that pushes a generated column out of its declared type is rejected
// and names that column, not the one the SET assigned. (Before OpTypeCheck the
// check ran on the STALE stored value and accepted the row -- see
// compileUpdateStmt's comment for the oracle evidence.)
func TestStrictTypeErrorGeneratedColumn(t *testing.T) {
	db := newStrictTestDB(t,
		`CREATE TABLE g(a INT, b BLOB AS (a) STORED) STRICT`,
		`INSERT INTO g(a) VALUES(NULL)`,
	)
	strictWantErr(t, db, `UPDATE g SET a=1`,
		`engine: UPDATE g: cannot store INT value in BLOB column g.b`)
	strictWantErr(t, db, `INSERT INTO g(a) VALUES(2)`,
		`engine: INSERT into g: cannot store INT value in BLOB column g.b`)

	dbv := newStrictTestDB(t,
		`CREATE TABLE h(a ANY, b BLOB AS (a) VIRTUAL) STRICT`,
		`INSERT INTO h(a) VALUES(x'01')`,
	)
	strictWantErr(t, dbv, `UPDATE h SET a=1`,
		`engine: UPDATE h: cannot store INT value in BLOB column h.b`)
}

// programUsesTypeCheck reports whether prog emits OpTypeCheck anywhere.
func programUsesTypeCheck(prog *Program) bool {
	for i := range prog.Insns {
		if prog.Insns[i].Op == OpTypeCheck {
			return true
		}
	}
	return false
}

// TestStrictShapesCompileWithTypeCheck is the structural half: these statements
// must compile to real bytecode (RULE #1) AND that bytecode must carry the
// check. Either half alone is insufficient -- a program that compiled without
// OpTypeCheck would store values SQLite rejects, and a shape that stopped
// compiling at all would take the whole statement with it. The non-STRICT twin
// is the control: it must NOT carry the opcode, so a blanket "emit it
// everywhere" could not pass this.
func TestStrictShapesCompileWithTypeCheck(t *testing.T) {
	cases := []struct {
		name   string
		setup  []string
		stmt   string
		strict bool
	}{
		{"insert values", []string{`CREATE TABLE t(a INT, b TEXT) STRICT`}, `INSERT INTO t VALUES(1,'x')`, true},
		{"insert default values", []string{`CREATE TABLE t(a INT DEFAULT 1, b TEXT) STRICT`}, `INSERT INTO t DEFAULT VALUES`, true},
		{"insert select", []string{`CREATE TABLE s(a,b)`, `CREATE TABLE t(a INT, b TEXT) STRICT`}, `INSERT INTO t SELECT a,b FROM s`, true},
		{"insert returning", []string{`CREATE TABLE t(a INT, b TEXT) STRICT`}, `INSERT INTO t VALUES(1,'x') RETURNING a,b`, true},
		{"upsert do update", []string{`CREATE TABLE t(a INT PRIMARY KEY, b TEXT) STRICT`}, `INSERT INTO t VALUES(1,'x') ON CONFLICT(a) DO UPDATE SET b='y'`, true},
		{"update", []string{`CREATE TABLE t(a INT, b TEXT) STRICT`}, `UPDATE t SET b='y'`, true},
		{"update where", []string{`CREATE TABLE t(a INT, b TEXT) STRICT`}, `UPDATE t SET b='y' WHERE a=1`, true},
		{"update returning", []string{`CREATE TABLE t(a INT, b TEXT) STRICT`}, `UPDATE t SET b='y' RETURNING a,b`, true},
		{"non-strict insert", []string{`CREATE TABLE t(a INT, b TEXT)`}, `INSERT INTO t VALUES(1,'x')`, false},
		{"non-strict update", []string{`CREATE TABLE t(a INT, b TEXT)`}, `UPDATE t SET b='y'`, false},
	}
	for _, tc := range cases {
		db := newStrictTestDB(t, tc.setup...)
		prog, err := db.compileWrite(tc.stmt)
		if err != nil {
			t.Errorf("[%s] %s: compile: %v", tc.name, tc.stmt, err)
			continue
		}
		if got := programUsesTypeCheck(prog); got != tc.strict {
			t.Errorf("[%s] %s: OpTypeCheck emitted = %v, want %v\n%s",
				tc.name, tc.stmt, got, tc.strict, prog.Disassemble())
		}
	}
}
