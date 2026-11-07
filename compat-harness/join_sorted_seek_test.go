package compat

import "testing"

// joinSeekSetup is two tables with no index on the join columns: duplicate,
// NULL, REAL, TEXT-that-looks-numeric and NOCASE keys, rows inserted out of
// key order so a level's scan order and its key order differ.
var joinSeekSetup = []string{
	"CREATE TABLE a(id INTEGER PRIMARY KEY, k INT, s TEXT COLLATE NOCASE, t TEXT, v)",
	"CREATE TABLE b(id INTEGER PRIMARY KEY, k INT, s TEXT, t TEXT COLLATE NOCASE, v)",
	"INSERT INTO a(k,s,t,v) VALUES(3,'X','b',1),(1,'y','a',2),(NULL,'x','A',3),(3,'Y','c',4),(2,'z',NULL,5),(1,'Z','B',6),(4,'w','10',7),(2.0,'W','2',8)",
	"INSERT INTO b(k,s,t,v) VALUES(1,'x','B',10),(3,'y','a',20),(1,'X','A',30),(NULL,'z','b',40),(5,'Y','10',50),(3,'w','c',60),(2,'x','2',70),(1,'y',NULL,80),(3.0,'Z','C',90)",
}

// TestJoinSortedSeekKeepsOrder pins joins whose inner level has no index: the
// level is narrowed per outer row to the rows sharing its join key (OpSortedSeek),
// and every row, and its ORDER with no ORDER BY, must still be C's.
func TestJoinSortedSeekKeepsOrder(t *testing.T) {
	queries := []string{
		"SELECT a.v, b.v FROM a JOIN b ON a.k=b.k",
		"SELECT a.v, b.v FROM a JOIN b USING(k)",
		"SELECT a.v, b.v FROM a, b WHERE b.k=a.k AND b.v>15",
		"SELECT a.v, b.v FROM a LEFT JOIN b ON a.k=b.k",
		"SELECT a.v, b.v FROM a LEFT JOIN b ON a.k=b.k AND b.v<60",
		"SELECT a.v, b.v FROM a LEFT JOIN b ON a.k=b.k WHERE b.v IS NULL",
		"SELECT a.v, b.v FROM a RIGHT JOIN b ON a.k=b.k",
		"SELECT a.v, b.v FROM a FULL JOIN b ON a.k=b.k",
		"SELECT k, a.v, b.v FROM a FULL JOIN b USING(k)",
		"SELECT a.v, b.v FROM a FULL JOIN b ON a.k=b.k AND a.v>2",
		"SELECT a.v, b.v FROM a JOIN b ON a.s=b.s",
		"SELECT a.v, b.v FROM a JOIN b ON b.s=a.s",
		"SELECT a.v, b.v FROM a JOIN b ON a.t=b.t",
		"SELECT a.v, b.v FROM a JOIN b ON b.t=a.t",
		"SELECT a.v, b.v FROM a JOIN b ON a.t=b.k",
		"SELECT a.v, b.v FROM a JOIN b ON b.k=a.t",
		"SELECT a.v, b.v FROM a JOIN b ON a.k+0=b.k",
		"SELECT a.v, b.v FROM a JOIN b ON a.k=b.k JOIN a a2 ON a2.k=b.k",
		"SELECT a.v, b.v, a2.v FROM a JOIN b ON a.k=b.k LEFT JOIN a a2 ON a2.s=b.s",
		"SELECT a.v, b.v FROM a JOIN b ON a.k IS b.k",
		"SELECT x.v, y.v FROM a x JOIN a y ON x.k=y.k",
		"SELECT sum(a.v*b.v), count(*) FROM a JOIN b ON a.k=b.k",
		"SELECT a.v, (SELECT group_concat(b.v) FROM b WHERE b.k=a.k) FROM a",
		"SELECT a.v, b.v FROM a JOIN b ON a.k=b.k LIMIT 3",
		"SELECT a.k, count(b.v) FROM a LEFT JOIN b ON a.k=b.k GROUP BY a.k",
	}
	for _, q := range queries {
		differ(t, q, append(append([]string(nil), joinSeekSetup...), q))
	}
}

// TestJoinSortedSeekAfterWrites pins a seek on a table the same statement's
// earlier rows changed, and one an UPDATE ... FROM drives.
func TestJoinSortedSeekAfterWrites(t *testing.T) {
	differ(t, "update from", append(append([]string(nil), joinSeekSetup...),
		"UPDATE a SET v = v + b.v FROM b WHERE b.k = a.k AND b.v > 50",
		"SELECT id, v FROM a",
		"INSERT INTO b(k,v) SELECT a.k, a.v FROM a JOIN b ON a.k=b.k",
		"SELECT id, k, v FROM b",
		"DELETE FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.k=a.k AND b.v=20)",
		"SELECT id, v FROM a",
	))
}

// TestCompoundDedupUnderCollations pins UNION, INTERSECT and EXCEPT, which
// dedup through a hashed row set, on keys equal only under a collation or
// across types, and NOCASE text equal up to a NUL.
func TestCompoundDedupUnderCollations(t *testing.T) {
	differ(t, "compound dedup", []string{
		"CREATE TABLE c(x TEXT COLLATE NOCASE, y)",
		"INSERT INTO c VALUES('a',1),('A',1),('b',2.0),('B',2),(NULL,NULL),(NULL,NULL),('a'||char(0)||'b',3),('A'||char(0)||'c',3)",
		"SELECT x, y FROM c UNION SELECT x, y FROM c",
		"SELECT x FROM c UNION SELECT 'B'",
		"SELECT x, y FROM c INTERSECT SELECT upper(x), y FROM c",
		"SELECT x, y FROM c EXCEPT SELECT 'A', 1",
		"SELECT y FROM c UNION SELECT 2 UNION SELECT 2.0",
		"SELECT DISTINCT x FROM c",
		"SELECT x, count(*) FROM c GROUP BY x",
		"SELECT x, row_number() OVER (PARTITION BY x) FROM c",
	})
}
