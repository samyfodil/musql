// Tests for schema-qualified write targets.
package compat

import "testing"

func TestSchemaQualifiedWriteTarget(t *testing.T) {
	// has to land in exactly one of them, and the final dump proves which.
	shadow := []string{
		`CREATE TABLE t(a,b)`,
		`CREATE TEMP TABLE t(a,b)`,
		`INSERT INTO main.t VALUES(1,'main')`,
		`INSERT INTO temp.t VALUES(1,'temp')`,
		`INSERT INTO t VALUES(2,'unqualified')`,
	}

	differ(t, "qualified INSERT lands in the named catalog", append(append([]string{}, shadow...),
		`SELECT a,b FROM main.t ORDER BY a`,
		`SELECT a,b FROM temp.t ORDER BY a`,
	))

	differ(t, "qualified UPDATE updates only the named catalog", append(append([]string{}, shadow...),
		`UPDATE main.t SET b='main-updated' WHERE a=1`,
		`SELECT a,b FROM main.t ORDER BY a`,
		`SELECT a,b FROM temp.t ORDER BY a`,
		`UPDATE temp.t SET b='temp-updated' WHERE a=1`,
		`SELECT a,b FROM main.t ORDER BY a`,
		`SELECT a,b FROM temp.t ORDER BY a`,
	))

	differ(t, "qualified DELETE deletes only from the named catalog", append(append([]string{}, shadow...),
		`DELETE FROM main.t WHERE a=1`,
		`SELECT a,b FROM main.t ORDER BY a`,
		`SELECT a,b FROM temp.t ORDER BY a`,
		`DELETE FROM temp.t`,
		`SELECT a,b FROM main.t ORDER BY a`,
		`SELECT a,b FROM temp.t ORDER BY a`,
	))

	// The OTHER direction of the ladder: a "main."-qualified write while ONLY
	// a temp table of that name exists is "no such table", not a silent write
	// into the temp one -- and vice versa.
	differ(t, "main. does not fall through to a temp-only table", []string{
		`CREATE TEMP TABLE only_temp(a)`,
		`INSERT INTO main.only_temp VALUES(1)`,
		`UPDATE main.only_temp SET a=2`,
		`DELETE FROM main.only_temp`,
		`INSERT INTO only_temp VALUES(9)`,
		`SELECT a FROM only_temp`,
	})
	differ(t, "temp. does not fall through to a main-only table", []string{
		`CREATE TABLE only_main(a)`,
		`INSERT INTO temp.only_main VALUES(1)`,
		`UPDATE temp.only_main SET a=2`,
		`DELETE FROM temp.only_main`,
		`INSERT INTO only_main VALUES(9)`,
		`SELECT a FROM only_main`,
	})
}

func TestSchemaQualifiedWriteRejections(t *testing.T) {
	// An unknown database name, and a missing table inside a database that
	// DOES resolve: both engines reject, and changes() must be untouched by
	// the rejection (a statement that never runs publishes nothing).
	differ(t, "unknown database name", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1),(2)`,
		`SELECT changes() AS c`,
		`INSERT INTO nosuchdb.t VALUES(3)`,
		`SELECT changes() AS c`,
		`UPDATE nosuchdb.t SET a=4`,
		`SELECT changes() AS c`,
		`DELETE FROM nosuchdb.t`,
		`SELECT changes() AS c`,
		`SELECT a FROM t ORDER BY a`,
	})
	differ(t, "missing table under a resolving qualifier", []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO main.nosuchtable VALUES(1)`,
		`UPDATE main.nosuchtable SET a=1`,
		`DELETE FROM main.nosuchtable`,
		`INSERT INTO temp.nosuchtable VALUES(1)`,
		`SELECT count(*) AS n FROM t`,
	})
}

func TestSchemaQualifiedWriteUnresolvableForeignKey(t *testing.T) {
	// An unresolvable foreign key is an error for ANY DML on the table, rows or
	// no rows, and the COMPILED path raises it from Program.FKTargets -- whose
	// target name comes from fkDMLTargetName (fk.go). That token walk used to
	// hand back the QUALIFIER ("main") as the table name, which resolved to
	// nothing, so a qualified write carried no FKTargets at all. It was
	// harmless only while these statements were declined outright; they
	// compile now, so FKTargets is the thing that has to be right.
	differ(t, "qualified DML against an unresolvable FK child", []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE c(x REFERENCES nosuchparent(y))`,
		`INSERT INTO main.c VALUES(1)`,
		`UPDATE main.c SET x=2`,
		`DELETE FROM main.c`,
		`SELECT count(*) AS n FROM c`,
	})
}

func TestSchemaQualifiedSqliteSequenceWrite(t *testing.T) {
	// sqlite_sequence is C SQLite's per-database AUTOINCREMENT bookkeeping
	// (build.c:2925's pSeqTab), and the write compiler's own synthesized
	// fallback for it (findTableMetaOrSqliteSequenceIn) is MAIN's -- so a
	// "temp."-qualified target must not reach it while a "main."-qualified one
	// must.
	differ(t, "qualified sqlite_sequence write", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`INSERT INTO t(b) VALUES('one'),('two')`,
		`SELECT name,seq FROM sqlite_sequence ORDER BY name`,
		`UPDATE main.sqlite_sequence SET seq=41 WHERE name='t'`,
		`SELECT name,seq FROM sqlite_sequence ORDER BY name`,
		`INSERT INTO t(b) VALUES('three')`,
		`SELECT a,b FROM t ORDER BY a`,
		`DELETE FROM main.sqlite_sequence WHERE name='t'`,
		`SELECT count(*) AS n FROM sqlite_sequence`,
	})
	differ(t, "temp.sqlite_sequence is not main's", []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`,
		`INSERT INTO t(b) VALUES('one')`,
		`UPDATE temp.sqlite_sequence SET seq=41 WHERE name='t'`,
		`DELETE FROM temp.sqlite_sequence`,
		`SELECT name,seq FROM sqlite_sequence ORDER BY name`,
	})
}
