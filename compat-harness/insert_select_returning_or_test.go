package compat

// INSERT OR ... SELECT ... RETURNING correctness tests against SQLite.
//
// Verifies that RETURNING works correctly with OR clauses in SELECT-sourced INSERTs.
import "testing"

var insertSelectReturningOrCases = []struct {
	name   string
	nSetup int
	stmts  []string
}{
	// ---- OR IGNORE: the skipped row must not appear in the result set ----
	{"or-ignore-dup-within-source", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y'),(1,'z'),(3,'w')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"or-ignore-conflict-with-existing", 5, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`INSERT INTO dst VALUES(1,'seed')`,
		`SELECT count(*) FROM dst`,
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"or-ignore-ipk-conflict", 5, []string{
		`CREATE TABLE src(k,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO dst VALUES(2,'seed')`,
		`SELECT count(*) FROM dst`,
		`INSERT OR IGNORE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`,
		`SELECT k,b FROM dst ORDER BY k`,
		`SELECT changes()`,
	}},
	{"or-ignore-notnull", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(NULL,'y'),(3,'z')`,
		`CREATE TABLE dst(a NOT NULL, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"or-ignore-check", 4, []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(20),(3)`,
		`CREATE TABLE dst(a, b, CHECK(a<10))`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT a,'k' FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},

	{"or-replace-ipk", 5, []string{
		`CREATE TABLE src(k,b)`,
		`INSERT INTO src VALUES(1,'x'),(3,'z')`,
		`CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`,
		`INSERT INTO dst VALUES(1,'seed'),(2,'seed2')`,
		`SELECT count(*) FROM dst`,
		`INSERT OR REPLACE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`,
		`SELECT k,b FROM dst ORDER BY k`,
		`SELECT changes()`,
	}},
	{"or-replace-unique", 5, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`INSERT INTO dst VALUES(1,'seed')`,
		`SELECT count(*) FROM dst`,
		`INSERT OR REPLACE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b,rowid`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"or-replace-notnull-default", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,NULL)`,
		`CREATE TABLE dst(a, b NOT NULL DEFAULT 'dd')`,
		`SELECT count(*) FROM src`,
		`INSERT OR REPLACE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},

	{"or-fail-unique-survivors", 5, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`INSERT INTO dst VALUES(2,'seed')`,
		`SELECT count(*) FROM dst`,
		`INSERT OR FAIL INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
	}},
	{"or-fail-notnull-survivors", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(NULL,'y'),(3,'z')`,
		`CREATE TABLE dst(a NOT NULL, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR FAIL INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
	}},

	{"or-abort-unique", 5, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`INSERT INTO dst VALUES(2,'seed')`,
		`SELECT count(*) FROM dst`,
		`INSERT OR ABORT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
	}},
	{"or-rollback-unique", 5, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y'),(3,'z')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`INSERT INTO dst VALUES(2,'seed')`,
		`SELECT count(*) FROM dst`,
		`INSERT OR ROLLBACK INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
	}},
	{"or-abort-no-conflict", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR ABORT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},

	{"mustbeint-or-ignore", 4, []string{
		`CREATE TABLE src(k,b)`,
		`INSERT INTO src VALUES('abc','y'),(2,'z')`,
		`CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`,
		`SELECT k,b FROM dst ORDER BY k`,
		`SELECT count(*) FROM dst`,
	}},
	{"mustbeint-or-fail", 4, []string{
		`CREATE TABLE src(k,b)`,
		`INSERT INTO src VALUES(1,'x'),('abc','y')`,
		`CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR FAIL INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`,
		`SELECT k,b FROM dst ORDER BY k`,
		`SELECT count(*) FROM dst`,
	}},
	{"mustbeint-or-replace", 4, []string{
		`CREATE TABLE src(k,b)`,
		`INSERT INTO src VALUES(1,'x'),(2.5,'y')`,
		`CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR REPLACE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`,
		`SELECT k,b FROM dst ORDER BY k`,
		`SELECT count(*) FROM dst`,
	}},
	{"mustbeint-coercible", 4, []string{
		`CREATE TABLE src(k,b)`,
		`INSERT INTO src VALUES('12','x'),(2.0,'y')`,
		`CREATE TABLE dst(k INTEGER PRIMARY KEY, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT k,b FROM src ORDER BY rowid RETURNING k,b`,
		`SELECT k,b,typeof(k) FROM dst ORDER BY k`,
		`SELECT changes()`,
	}},

	{"declared-ignore-default", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(1,'y'),(2,'z')`,
		`CREATE TABLE dst(a UNIQUE ON CONFLICT IGNORE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"declared-replace-default", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(1,'y'),(2,'z')`,
		`CREATE TABLE dst(a UNIQUE ON CONFLICT REPLACE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"declared-ignore-overridden-by-or-replace", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(1,'y'),(2,'z')`,
		`CREATE TABLE dst(a UNIQUE ON CONFLICT IGNORE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR REPLACE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"declared-notnull-ignore", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(NULL,'y'),(3,'z')`,
		`CREATE TABLE dst(a NOT NULL ON CONFLICT IGNORE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},

	{"strict-or-ignore-typeviolation", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),('notanint','y'),(3,'z')`,
		`CREATE TABLE dst(a INT, b TEXT) STRICT`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
	}},
	{"strict-or-replace-ok", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE dst(a INT PRIMARY KEY, b TEXT) STRICT`,
		`SELECT count(*) FROM src`,
		`INSERT OR REPLACE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING a,b`,
		`SELECT a,b FROM dst ORDER BY a`,
		`SELECT changes()`,
	}},

	// ---- Generated columns x SELECT source x RETURNING ----
	// A generated column derived from the auto-assigned INTEGER PRIMARY KEY:
	// see rederiveGeneratedFromRowid (engine/vdbe_write.go) and insert.c:1545-1548.
	{"generated-from-ipk", 4, []string{
		`CREATE TABLE src(v)`,
		`INSERT INTO src VALUES('x'),('y')`,
		`CREATE TABLE dst(id INTEGER PRIMARY KEY, v, g AS (id*2))`,
		`SELECT count(*) FROM src`,
		`INSERT INTO dst(v) SELECT v FROM src ORDER BY rowid RETURNING id,v,g`,
		`SELECT id,v,g FROM dst ORDER BY id`,
		`SELECT changes()`,
	}},
	{"generated-from-ipk-stored", 4, []string{
		`CREATE TABLE src(v)`,
		`INSERT INTO src VALUES('x'),('y')`,
		`CREATE TABLE dst(id INTEGER PRIMARY KEY, v, g AS (id*2) STORED)`,
		`SELECT count(*) FROM src`,
		`INSERT INTO dst(v) SELECT v FROM src ORDER BY rowid RETURNING id,v,g`,
		`SELECT id,v,g FROM dst ORDER BY id`,
		`SELECT changes()`,
	}},
	{"generated-or-ignore", 4, []string{
		`CREATE TABLE src(v)`,
		`INSERT INTO src VALUES(1),(1),(2)`,
		`CREATE TABLE dst(id INTEGER PRIMARY KEY, v UNIQUE, g AS (id*100+v))`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst(v) SELECT v FROM src ORDER BY rowid RETURNING id,v,g`,
		`SELECT id,v,g FROM dst ORDER BY id`,
		`SELECT changes()`,
	}},

	// ---- Column DEFAULTs and a narrower column list, under an OR-clause ----
	{"or-ignore-with-defaults", 4, []string{
		`CREATE TABLE src(a)`,
		`INSERT INTO src VALUES(1),(1),(2)`,
		`CREATE TABLE dst(a UNIQUE, d DEFAULT 'dflt', e DEFAULT 7)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst(a) SELECT a FROM src ORDER BY rowid RETURNING a,d,e`,
		`SELECT a,d,e FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// RETURNING * and rowid over the OR-clause path.
	{"or-ignore-returning-star", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(1,'y'),(2,'z')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src ORDER BY rowid RETURNING *, rowid, a||b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	// An empty source under an OR-clause: zero rows out, statement succeeds.
	{"or-ignore-empty-source", 3, []string{
		`CREATE TABLE src(a,b)`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src RETURNING a,b`,
		`SELECT count(*) FROM dst`,
		`SELECT changes()`,
	}},
	// A WHERE-filtered / compound / LIMITed source under an OR-clause.
	{"or-ignore-filtered-source", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'k'),(1,'k'),(2,'j'),(3,'k')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src WHERE b='k' ORDER BY rowid RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
	{"or-ignore-compound-source", 4, []string{
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(1,'x'),(2,'y')`,
		`CREATE TABLE dst(a UNIQUE, b)`,
		`SELECT count(*) FROM src`,
		`INSERT OR IGNORE INTO dst SELECT a,b FROM src UNION ALL SELECT a,b FROM src RETURNING a,b`,
		`SELECT a,b,rowid FROM dst ORDER BY rowid`,
		`SELECT changes()`,
	}},
}

func TestInsertSelectReturningOrClause(t *testing.T) {
	for _, c := range insertSelectReturningOrCases {
		differExecOnce(t, c.name, c.nSetup, c.stmts)
	}
}
