package compat

import "testing"

// init appends ~20 more curated cases to harness_test.go's corpus, covering
// CTEs, window functions, views, triggers, indexes, LIKE/GLOB, COLLATE
// NOCASE, UPSERT, RETURNING, generated columns, misc functions, rowid
// aliasing, WITHOUT ROWID, multi-column ORDER BY, HAVING, EXISTS, set
// operations, hex/unhex, and nested subqueries.
func init() {
	corpus = append(corpus, []struct {
		name  string
		stmts []string
	}{
		{"cte-recursive", []string{
			"WITH RECURSIVE nums(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM nums WHERE x<5) SELECT x FROM nums",
			"WITH RECURSIVE nums(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM nums WHERE x<10) SELECT sum(x), count(*) FROM nums",
		}},
		{"cte-nonrecursive", []string{
			"WITH a AS (SELECT 1 AS v), b AS (SELECT 2 AS v) SELECT a.v+b.v FROM a,b",
		}},
		{"window-functions", []string{
			"CREATE TABLE w(g TEXT, v INTEGER)",
			"INSERT INTO w VALUES('a',1),('a',2),('a',3),('b',10),('b',20)",
			"SELECT g,v,row_number() OVER (PARTITION BY g ORDER BY v), rank() OVER (PARTITION BY g ORDER BY v), sum(v) OVER (PARTITION BY g ORDER BY v) FROM w ORDER BY g,v",
		}},
		{"views", []string{
			"CREATE TABLE vt(id INTEGER, v INTEGER)",
			"INSERT INTO vt VALUES(1,10),(2,-5),(3,20)",
			"CREATE VIEW vv AS SELECT id, v*2 AS v2 FROM vt WHERE v>0",
			"SELECT * FROM vv ORDER BY id",
		}},
		{"triggers", []string{
			"CREATE TABLE tr(id INTEGER PRIMARY KEY, v INTEGER)",
			"CREATE TABLE tr_log(id INTEGER, old_v INTEGER, new_v INTEGER)",
			"CREATE TRIGGER trg AFTER UPDATE ON tr BEGIN INSERT INTO tr_log VALUES(old.id, old.v, new.v); END",
			"INSERT INTO tr VALUES(1,100)",
			"UPDATE tr SET v=200 WHERE id=1",
			"SELECT * FROM tr_log",
		}},
		{"index-query", []string{
			"CREATE TABLE ix(a INTEGER, b TEXT)",
			"CREATE INDEX idx_ix_a ON ix(a)",
			"INSERT INTO ix VALUES(1,'x'),(2,'y'),(2,'z'),(3,'w')",
			"SELECT b FROM ix WHERE a=2 ORDER BY b",
		}},
		{"like-glob-escape", []string{
			"SELECT 'abc' LIKE 'a%', 'ABC' LIKE 'a%', 'a_c' LIKE 'a\\_c' ESCAPE '\\', 'a_c' LIKE 'a_c', 'abc' GLOB 'a?c', 'abc' GLOB 'a*', 'abc' GLOB 'A*'",
		}},
		{"collate-nocase", []string{
			"CREATE TABLE cn(s TEXT COLLATE NOCASE)",
			"INSERT INTO cn VALUES('B'),('a'),('C')",
			"SELECT s FROM cn ORDER BY s",
			"SELECT 'ABC'='abc' COLLATE NOCASE, 'ABC'='abc'",
		}},
		{"upsert-on-conflict", []string{
			"CREATE TABLE up(id INTEGER PRIMARY KEY, v INTEGER)",
			"INSERT INTO up VALUES(1,10)",
			"INSERT INTO up VALUES(1,20) ON CONFLICT(id) DO UPDATE SET v=up.v+excluded.v",
			"INSERT INTO up VALUES(2,5) ON CONFLICT(id) DO NOTHING",
			"SELECT * FROM up ORDER BY id",
		}},
		{"returning-clause", []string{
			"CREATE TABLE ret(id INTEGER PRIMARY KEY, v INTEGER)",
			"INSERT INTO ret VALUES(1,5) RETURNING id, v*2",
			"UPDATE ret SET v=v+1 WHERE id=1 RETURNING id, v",
			"DELETE FROM ret WHERE id=1 RETURNING id",
		}},
		{"generated-columns", []string{
			"CREATE TABLE gc(a INTEGER, b INTEGER GENERATED ALWAYS AS (a*2) STORED, c INTEGER GENERATED ALWAYS AS (a+1) VIRTUAL)",
			"INSERT INTO gc(a) VALUES(5),(10)",
			"SELECT a,b,c FROM gc ORDER BY a",
		}},
		{"printf-format", []string{
			"SELECT printf('%d-%s-%.2f', 5, 'x', 3.14159), printf('%05d', 42), printf('%x', 255)",
		}},
		{"quote-fn", []string{
			"SELECT quote('abc'), quote(NULL), quote(5), quote(3.5), quote(x'ab'), quote('a''b')",
		}},
		{"randomblob-length", []string{
			"SELECT length(randomblob(16)), typeof(randomblob(16)), length(zeroblob(8)), typeof(zeroblob(8))",
		}},
		{"rowid-alias", []string{
			"CREATE TABLE ra(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO ra VALUES(1,'a'),(2,'b')",
			"SELECT rowid,id,v FROM ra ORDER BY id",
			"UPDATE ra SET id=100 WHERE rowid=1",
			"SELECT rowid,id,v FROM ra ORDER BY rowid",
		}},
		{"without-rowid", []string{
			"CREATE TABLE wr(k TEXT PRIMARY KEY, v INTEGER) WITHOUT ROWID",
			"INSERT INTO wr VALUES('b',2),('a',1),('c',3)",
			"SELECT k,v FROM wr ORDER BY k",
		}},
		{"multi-col-order", []string{
			"CREATE TABLE mc(a INTEGER, b INTEGER)",
			"INSERT INTO mc VALUES(1,2),(1,1),(2,5),(2,1),(1,3)",
			"SELECT a,b FROM mc ORDER BY a ASC, b DESC",
		}},
		{"having", []string{
			"CREATE TABLE hv(g TEXT, v INTEGER)",
			"INSERT INTO hv VALUES('a',1),('a',20),('b',2),('b',3),('c',100)",
			"SELECT g, sum(v) FROM hv GROUP BY g HAVING sum(v)>10 ORDER BY g",
		}},
		{"exists-subquery", []string{
			"CREATE TABLE e1(id INTEGER)",
			"CREATE TABLE e2(id INTEGER)",
			"INSERT INTO e1 VALUES(1),(2),(3)",
			"INSERT INTO e2 VALUES(2),(3)",
			"SELECT id FROM e1 WHERE EXISTS (SELECT 1 FROM e2 WHERE e2.id=e1.id) ORDER BY id",
			"SELECT id FROM e1 WHERE NOT EXISTS (SELECT 1 FROM e2 WHERE e2.id=e1.id) ORDER BY id",
		}},
		{"union-intersect-except", []string{
			"SELECT 1 UNION SELECT 2 UNION SELECT 1 ORDER BY 1",
			"SELECT 1 UNION ALL SELECT 1 UNION ALL SELECT 2 ORDER BY 1",
			"SELECT v FROM (SELECT 1 AS v UNION SELECT 2 UNION SELECT 3) INTERSECT SELECT v FROM (SELECT 2 AS v UNION SELECT 3 UNION SELECT 4) ORDER BY v",
			"SELECT v FROM (SELECT 1 AS v UNION SELECT 2 UNION SELECT 3) EXCEPT SELECT v FROM (SELECT 2 AS v UNION SELECT 4) ORDER BY v",
		}},
		{"hex-unhex", []string{
			"SELECT hex('AB'), hex(x'0a0b'), unhex('4142'), unhex('41-42','-')",
		}},
		{"nested-subquery", []string{
			"CREATE TABLE ns(a INTEGER)",
			"INSERT INTO ns VALUES(1),(5),(10),(15),(20)",
			"SELECT a FROM ns WHERE a IN (SELECT a FROM ns WHERE a > (SELECT avg(a) FROM ns)) ORDER BY a",
		}},
	}...)
}

// knownDivergences documents SQL sequences that legitimately do NOT compare
// identically across engines and/or runs -- not because musql or
// modernc.org/sqlite are wrong, but because of real, documented SQLite
// semantics (intentional non-determinism, or a feature compiled into some
// builds and not others). Entries are skipped rather than deleted so the
// SQL and the reasoning both stay attached and runnable/inspectable.
var knownDivergences = []struct {
	name   string
	stmts  []string
	reason string
}{
	{
		name: "rowid-random-on-int64-overflow",
		stmts: []string{
			"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t VALUES(9223372036854775807, 'a')",
			"INSERT INTO t VALUES(NULL, 'b')",
			"SELECT id, v FROM t ORDER BY v",
		},
		reason: "Minimal repro for a real finding from TestFuzzDifferential " +
			"(programs #125, #167, #358, seed 424242, before the fuzzer's " +
			"INTEGER PRIMARY KEY columns were switched to AUTOINCREMENT). " +
			"Once a rowid-alias column's max value hits math.MaxInt64 " +
			"(9223372036854775807), SQLite's documented rowid-assignment " +
			"algorithm falls back to picking a RANDOM unused rowid for the " +
			"next NULL (auto-assign) insert -- see the SQLite docs section " +
			"on rowid selection ('If ... the largest possible integer, then " +
			"the database engine will pick new ROWIDs at random'). This is " +
			"genuine, intentional non-determinism baked into SQLite itself: " +
			"all three engines (musql, modernc.org/sqlite, C SQLite " +
			"via mattn/go-sqlite3) correctly implement the *same rule*, they " +
			"just each land on a different actual id because it is driven by " +
			"each build's own PRNG state at that moment. It is not a driver- " +
			"marshaling nuance and not an engine incompatibility -- it is the " +
			"one place the SQL standard-adjacent behavior of SQLite itself is " +
			"defined to be unpredictable. Not run through differ() because " +
			"asserting equality on a value SQLite documents as random would " +
			"be flaky by design, not a meaningful compatibility check.",
	},
}

func TestKnownDivergences(t *testing.T) {
	for _, c := range knownDivergences {
		t.Run(c.name, func(t *testing.T) {
			t.Skip(c.reason)
		})
	}
}
