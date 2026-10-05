package compat

import "testing"

// TestCollationOrViewSourceMatchesC: the declared-collation OR probe over a
// VIEW source. It declined while a view might flatten into a shape the probe
// could not see; flattenSubquery is ported now, so the view is its base table
// before planning, and "ta, vb" answers exactly as "ta, tb" does.
func TestCollationOrViewSourceMatchesC(t *testing.T) {
	base := []string{"CREATE TABLE ta(a TEXT COLLATE NOCASE, b TEXT COLLATE NOCASE)", "INSERT INTO ta VALUES('AAA','BBB')",
		"CREATE TABLE tb(x,y,c TEXT)", "INSERT INTO tb(c) VALUES('aaa'),('bbb')", "CREATE INDEX tb_c ON tb(c)",
		"CREATE VIEW vb AS SELECT x,y,c FROM tb"}
	for _, q := range []string{"SELECT c FROM ta, vb WHERE a=c OR b=c", "SELECT c FROM ta, tb WHERE a=c OR b=c", "SELECT c FROM vb, ta WHERE a=c OR b=c"} {
		differ(t, q, append(append([]string{}, base...), q))
	}
}
