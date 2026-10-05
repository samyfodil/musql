package compat

import "testing"

// TestWindowConstantValueAndViewUpdateFrom covers window1.test 73.3-73.4. A
// first_value/last_value/nth_value whose argument is constant reads the same
// value at every position, so a tied peer order cannot change it. And an
// UPDATE ... FROM against a view takes RETURNING -- the NEW row, as without
// FROM -- and fires once per join row when the INSTEAD OF programs cannot
// tell the order they fire in (update.c:952-985).
func TestWindowConstantValueAndViewUpdateFrom(t *testing.T) {
	differ(t, "window1 73", []string{
		"CREATE TABLE t1(a INT)",
		"INSERT INTO t1(a) VALUES(1),(2),(4)",
		"CREATE VIEW t2(b,c) AS SELECT * FROM t1 JOIN t1 A ORDER BY sum(0) OVER(PARTITION BY 0)",
		"CREATE TRIGGER x1 INSTEAD OF UPDATE ON t2 BEGIN SELECT true; END",
		"SELECT *, nth_value(15,2) OVER() FROM t2, t1 WHERE b=4",
		"SELECT a, nth_value(15,4) OVER(), first_value('x') OVER(ORDER BY a%2), last_value(7) OVER(ORDER BY a%2) FROM t1",
		"UPDATE t2 SET c=nth_value(15,2) OVER() FROM (SELECT * FROM t1) WHERE b=4 RETURNING *",
	})
	differ(t, "view UPDATE FROM", []string{
		"CREATE TABLE t1(a INT)",
		"INSERT INTO t1(a) VALUES(1),(2),(4)",
		"CREATE TABLE log(x)",
		"CREATE VIEW v(b,c) AS SELECT a, a*10 FROM t1",
		"CREATE TRIGGER tv INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(old.b||'>'||new.c); END",
		"UPDATE v SET c=m.a FROM (SELECT a FROM t1) AS m WHERE v.b=m.a RETURNING b, c",
		"SELECT * FROM log ORDER BY rowid",
		"DROP TRIGGER tv",
		"CREATE TRIGGER tv2 INSTEAD OF UPDATE ON v WHEN new.c > 1 BEGIN SELECT new.b + new.c; END",
		"UPDATE v SET c=m.a FROM (SELECT a FROM t1) AS m WHERE v.b=4 RETURNING b, c, typeof(c)",
		"DROP TRIGGER tv2",
		"CREATE TRIGGER tv3 INSTEAD OF UPDATE ON v BEGIN SELECT RAISE(IGNORE) WHERE new.c=2; END",
		"UPDATE v SET c=m.a FROM (SELECT a FROM t1) AS m WHERE v.b=4 RETURNING b, c",
	})
	differ(t, "view UPDATE FROM RETURNING, writing trigger", []string{
		"CREATE TABLE b(k,v)",
		"CREATE VIEW v1 AS SELECT k,v FROM b",
		"CREATE TABLE m(k,nv)",
		"INSERT INTO b VALUES(1,'a'),(2,'b'),(3,'c')",
		"INSERT INTO m VALUES(1,'x'),(3,'z')",
		"CREATE TRIGGER vu INSTEAD OF UPDATE ON v1 BEGIN UPDATE b SET v=new.v WHERE k=old.k; END",
		"UPDATE v1 SET v=m.nv FROM m WHERE m.k=v1.k RETURNING v",
		"SELECT * FROM b ORDER BY k",
	})
}
