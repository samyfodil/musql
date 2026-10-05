package compat

import (
	"fmt"
	"testing"
)

// TestCollationOrSharedColumnMatches gates the OR-to-IN transform when every
// disjunct is a "col = col" equality against one shared column with an index
// under its declared collation, ensuring all branches compare under that collation.
func TestCollationOrSharedColumnMatches(t *testing.T) {
	// The exact mined where2.test t614/t615/t616 families: outer-declared
	// NOCASE vs inner BINARY (all four orders answer 0 rows: c's BINARY
	// governs every branch once the index is probed), outer plain vs inner
	// NOCASE (all four orders answer both rows), both NOCASE.
	setup := func(ac, cc string) []string {
		return []string{
			"CREATE TABLE ta(a TEXT" + ac + ", b TEXT" + ac + ")",
			"INSERT INTO ta VALUES('AAA','BBB')",
			"CREATE TABLE tb(x,y,c TEXT" + cc + ")",
			"INSERT INTO tb(c) VALUES('aaa'),('bbb')",
		}
	}
	orders := []string{
		"SELECT c FROM ta, tb WHERE a=c OR b=c",
		"SELECT c FROM ta, tb WHERE c=a OR c=b",
		"SELECT c FROM ta, tb WHERE c=a OR b=c",
		"SELECT c FROM ta, tb WHERE a=c OR c=b",
	}
	families := []struct {
		name   string
		ac, cc string
	}{
		{"outer-nocase-inner-binary", " COLLATE NOCASE", ""},
		{"outer-binary-inner-nocase", "", " COLLATE NOCASE"},
		{"both-nocase", " COLLATE NOCASE", " COLLATE NOCASE"},
	}
	for _, f := range families {
		f := f
		t.Run("indexed-"+f.name, func(t *testing.T) {
			stmts := append(setup(f.ac, f.cc), "CREATE INDEX tb_c ON tb(c)")
			stmts = append(stmts, orders...)
			differ(t, "indexed-"+f.name, stmts)
		})
		// Without the index, per-term evaluation applies.
		t.Run("unindexed-"+f.name, func(t *testing.T) {
			differ(t, "unindexed-"+f.name, append(setup(f.ac, f.cc), orders...))
		})
	}

	base := append(setup(" COLLATE NOCASE", ""), "CREATE INDEX tb_c ON tb(c)")
	single := func(name string, extra ...string) {
		t.Run(name, func(t *testing.T) {
			differ(t, name, append(append([]string{}, base...), extra...))
		})
	}
	// FROM order flipped: planner probes the indexed side either way.
	single("from-order-flipped", "SELECT c FROM tb, ta WHERE a=c OR b=c")
	// Three disjuncts, one shared column.
	single("three-disjuncts", "SELECT c FROM ta, tb WHERE a=c OR b=c OR a=c")
	// Transform under GROUP BY, aggregate, DISTINCT, and ORDER BY.
	single("group-by", "SELECT c, count(*) FROM ta, tb WHERE a=c OR b=c GROUP BY c")
	single("count-star", "SELECT count(*) FROM ta, tb WHERE a=c OR b=c")
	single("distinct", "SELECT DISTINCT c FROM ta, tb WHERE a=c OR b=c")
	single("order-by", "SELECT c FROM ta, tb WHERE a=c OR b=c ORDER BY c")
	// JOIN ON spelling variants.
	single("inner-join-on", "SELECT c FROM ta JOIN tb ON (a=c OR b=c)")
	single("cross-join-on", "SELECT c FROM ta CROSS JOIN tb ON (a=c OR b=c)")
	single("left-join-on", "SELECT c FROM ta LEFT JOIN tb ON (a=c OR b=c)")
	// LEFT JOIN with shared column on left: no probe, per-term rule applies.
	single("left-join-on-shared-left",
		"CREATE TABLE tt(x TEXT, y TEXT)",
		"INSERT INTO tt VALUES('aaa','bbb')",
		"CREATE INDEX ta_a ON ta(a)",
		"SELECT x FROM ta LEFT JOIN tt ON (x=a OR y=a)")
	// UPDATE ... FROM applies same transform through scan compiler.
	single("update-from",
		"UPDATE tb SET y='hit' FROM ta WHERE a=c OR b=c",
		"SELECT c, y FROM tb ORDER BY c")

	// Boundary shapes that serve as-written: non-leading index column,
	// index under different collation, or no shared column.
	single("non-leading-index-column",
		"CREATE TABLE tc(x,y,c2 TEXT)",
		"INSERT INTO tc(c2) VALUES('aaa'),('bbb')",
		"CREATE INDEX tc_xc ON tc(x,c2)",
		"SELECT c2 FROM ta, tc WHERE a=c2 OR b=c2")
	single("mismatched-index-collation",
		"CREATE TABLE tf(x,y,c2 TEXT)",
		"INSERT INTO tf(c2) VALUES('aaa'),('bbb')",
		"CREATE INDEX tf_c ON tf(c2 COLLATE NOCASE)",
		// ORDER BY pins row order for consistent comparison.
		"SELECT c2 FROM ta, tf WHERE a=c2 OR b=c2 ORDER BY c2",
		"SELECT c2 FROM ta, tf WHERE c2=a OR c2=b ORDER BY c2")
	single("no-shared-column",
		"CREATE TABLE tj(x,d TEXT,c2 TEXT)",
		"INSERT INTO tj(c2,d) VALUES('aaa','aaa'),('bbb','bbb')",
		"CREATE INDEX tj_c ON tj(c2)",
		"CREATE INDEX tj_d ON tj(d)",
		"SELECT c2 FROM ta, tj WHERE a=c2 OR b=d")

	// Transform variants over index/table kind.
	for i, mk := range []string{
		"CREATE INDEX v_c ON tv(c2,x)",
		"CREATE UNIQUE INDEX v_c ON tv(c2)",
		"CREATE INDEX v_c ON tv(c2 DESC)",
	} {
		single(fmt.Sprintf("index-variant-%d", i),
			"CREATE TABLE tv(x,y,c2 TEXT)",
			"INSERT INTO tv(c2) VALUES('aaa'),('bbb')",
			mk,
			"SELECT c2 FROM ta, tv WHERE a=c2 OR b=c2")
	}
	single("without-rowid-secondary-index",
		"CREATE TABLE tn(k TEXT PRIMARY KEY, c2 TEXT) WITHOUT ROWID",
		"INSERT INTO tn VALUES('k1','aaa'),('k2','bbb')",
		"CREATE INDEX tn_c ON tn(c2)",
		"SELECT c2 FROM ta, tn WHERE a=c2 OR b=c2")
	// Both sides indexed: transform applies.
	single("outer-side-also-indexed",
		"CREATE INDEX ta_a2 ON ta(a)",
		"CREATE INDEX ta_b2 ON ta(b)",
		"SELECT c FROM ta, tb WHERE a=c OR b=c")
}
