package compat

import "testing"

// TestAlterR30SignedTermDefault pins "ALTER TABLE ... ADD COLUMN x <type>
// DEFAULT -<term>" over a term that is not a number.
//
// SQLite allows "DEFAULT -'hello'" and folds it to 0, which TEXT affinity
// stores as "0". The CREATE TABLE and ADD COLUMN paths must fold these
// identically, since ADD COLUMN backfills from its own parse.
func TestAlterR30SignedTermDefault(t *testing.T) {
	differ(t, "alter-r30-default-neg-string", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1)`,
		`ALTER TABLE t1 ADD COLUMN d TEXT DEFAULT -'hello'`,
		`SELECT d, typeof(d) FROM t1`,
		`SELECT sql FROM sqlite_master`,
		`INSERT INTO t1(a) VALUES(2)`,
		`SELECT a, d, typeof(d) FROM t1 ORDER BY a`,
	})
	differ(t, "alter-r30-default-neg-numeric-string", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1)`,
		`ALTER TABLE t1 ADD COLUMN d DEFAULT -'3.5'`,
		`ALTER TABLE t1 ADD COLUMN e DEFAULT -'12abc'`,
		`ALTER TABLE t1 ADD COLUMN f TEXT DEFAULT +'hello'`,
		`SELECT d, typeof(d), e, typeof(e), f, typeof(f) FROM t1`,
		`SELECT sql FROM sqlite_master`,
	})
	differ(t, "alter-r30-default-neg-blob", []string{
		`CREATE TABLE t1(a)`,
		`INSERT INTO t1 VALUES(1)`,
		`ALTER TABLE t1 ADD COLUMN d DEFAULT -x'6869'`,
		`SELECT d, typeof(d) FROM t1`,
		`SELECT sql FROM sqlite_master`,
	})
	// CREATE TABLE path must fold to the identical values.
	differ(t, "alter-r30-default-signed-term-create", []string{
		`CREATE TABLE t1(a, d TEXT DEFAULT -'hello', e DEFAULT -'hello', f TEXT DEFAULT +'hello', g DEFAULT -'12abc', h DEFAULT -'3.5', i DEFAULT -x'6869')`,
		`INSERT INTO t1(a) VALUES(1)`,
		`SELECT d, typeof(d), e, typeof(e), f, typeof(f), g, typeof(g), h, typeof(h), i, typeof(i) FROM t1`,
	})
}
