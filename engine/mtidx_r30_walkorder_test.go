package engine

// Gate for multi-table indexed cursor walk order, ensuring index-order traversal not rowid order.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// r30Row is one inner-table row of the fixture: k is the join key, v and w are
// observers whose values are deliberately NOT in rowid order, so an index walk
// and a rowid-ordered scan produce visibly different strings. (A fixture whose
// observer ascends with rowid hides exactly this divergence -- the trap
// compat-harness/joinorder_r27_battery_test.go's own comment names.)
type r30Row struct {
	rowid int64
	k     int64
	v     Value // TEXT / INTEGER / REAL / NULL -- one index sorts all of them
	w     int64
}

func r30Text(s string) Value { return Value{Typ: Text, S: []byte(s)} }
func r30Int(i int64) Value   { return Value{Typ: Int, I: i} }

func r30Inner() []r30Row {
	return []r30Row{
		{1, 1, r30Text("D"), 10},
		{2, 1, r30Text("a"), 11},
		{3, 2, r30Text("c"), 12},
		{4, 1, r30Text("b"), 13},
		{5, 2, Value{}, 14}, // NULL sorts first ascending
		{6, 3, r30Text("e"), 15},
		{7, 2, Value{Typ: Float, F: 2.0}, 16},
		{8, 1, r30Text("B"), 9},
		{9, 2, r30Text("A"), 17},
		{10, 1, r30Int(7), 8},
	}
}

// r30Outer is the driving table: its key column repeats (so one inner run is
// visited twice) and includes a value matching nothing.
var r30Outer = []int64{2, 1, 3, 9, 1}

// r30Build creates a two-table database with the given inner-table index DDL
// applied and returns a read pager over it.
func r30Build(t *testing.T, name string, idx []string) *ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := append([]string{
		"CREATE TABLE t1(x,y)",
		"CREATE TABLE t2(k,v,w)",
	}, idx...)
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for i, x := range r30Outer {
		if _, _, err := db.ExecArgs("INSERT INTO t1(x,y) VALUES(?,?)",
			[]Value{r30Int(x), r30Text(fmt.Sprintf("o%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range r30Inner() {
		if _, _, err := db.ExecArgs("INSERT INTO t2(rowid,k,v,w) VALUES(?,?,?,?)",
			[]Value{r30Int(r.rowid), r30Int(r.k), r.v, r30Int(r.w)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// r30KeyCol names one column of an expected walk order: which inner-row field
// it reads, under which collating sequence, and whether the index declared it
// DESC. "rowid" is the trailing key column every secondary index over a rowid
// table ends with.
type r30KeyCol struct {
	name string
	coll string
	desc bool
}

func r30Field(r r30Row, name string) Value {
	switch name {
	case "k":
		return r30Int(r.k)
	case "v":
		return r.v
	case "w":
		return r30Int(r.w)
	default:
		return r30Int(r.rowid)
	}
}

// r30Expect computes the rows "SELECT t2.v, t2.w FROM t1, t2 WHERE t2.k = t1.x"
// must produce when t1 drives the loop in rowid order and each of its keys'
// matching t2 rows arrive in the walk order key describes. Derived from the
// fixture and the CREATE INDEX text alone -- nothing here calls the engine.
func r30Expect(key []r30KeyCol) [][2]int64 {
	inner := r30Inner()
	var out [][2]int64
	for _, x := range r30Outer {
		var run []r30Row
		for _, r := range inner {
			if r.k == x {
				run = append(run, r)
			}
		}
		sort.SliceStable(run, func(i, j int) bool {
			for _, kc := range key {
				c := compareValuesCollatedEnc(r30Field(run[i], kc.name), r30Field(run[j], kc.name), kc.coll, UTF8)
				if c != 0 {
					if kc.desc {
						return c > 0
					}
					return c < 0
				}
			}
			return false
		})
		for _, r := range run {
			out = append(out, [2]int64{r.rowid, r.w})
		}
	}
	return out
}

// r30Query runs the join and returns (rowid, w) per output row. It selects
// t2.rowid rather than t2.v so the comparison is exact for the NULL and REAL
// values in the fixture, and t2.w so no single-column index can COVER it (a
// covering index is priced differently -- see indexWalkOrder).
func r30Query(t *testing.T, p *ReadOnlyPager, sql string) [][2]int64 {
	t.Helper()
	_, rows, err := p.Query(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	out := make([][2]int64, len(rows))
	for i, r := range rows {
		if len(r) != 2 || r[0].Typ != Int || r[1].Typ != Int {
			t.Fatalf("%s: row %d is %v, want two integers", sql, i, r)
		}
		out[i] = [2]int64{r[0].I, r[1].I}
	}
	return out
}

const r30SQL = "SELECT t2.rowid, t2.w FROM t1, t2 WHERE t2.k = t1.x"

func r30Fmt(rows [][2]int64) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("(%d,%d)", r[0], r[1])
	}
	return strings.Join(parts, " ")
}

func r30Multiset(rows [][2]int64) string {
	c := append([][2]int64(nil), rows...)
	sort.Slice(c, func(i, j int) bool {
		if c[i][0] != c[j][0] {
			return c[i][0] < c[j][0]
		}
		return c[i][1] < c[j][1]
	})
	return r30Fmt(c)
}

func TestR30JoinSeekWalksIndexOrder(t *testing.T) {
	rowidOnly := []r30KeyCol{{name: "rowid"}}

	cases := []struct {
		name string
		ddl  []string
		key  []r30KeyCol
		why  string
	}{
		{
			name: "single-col",
			ddl:  []string{"CREATE INDEX i2 ON t2(k)"},
			key:  []r30KeyCol{{name: "k", coll: "BINARY"}, {name: "rowid"}},
			why:  "k is pinned by the equality, so the rowid is the only tiebreak -- the one shape where a rowid sort was already the walk order",
		},
		{
			name: "two-col",
			ddl:  []string{"CREATE INDEX i2 ON t2(k,v)"},
			key:  []r30KeyCol{{name: "k", coll: "BINARY"}, {name: "v", coll: "BINARY"}, {name: "rowid"}},
			why:  "within the k run the walk is ordered by v, which the rowid sort destroyed",
		},
		{
			name: "two-col-desc",
			ddl:  []string{"CREATE INDEX i2 ON t2(k,v DESC)"},
			key:  []r30KeyCol{{name: "k", coll: "BINARY"}, {name: "v", coll: "BINARY", desc: true}, {name: "rowid"}},
			why:  "a DESC key column is stored reversed, so a FORWARD walk yields it largest-first (NULLs last)",
		},
		{
			name: "two-col-nocase",
			ddl:  []string{"CREATE INDEX i2 ON t2(k,v COLLATE NOCASE)"},
			key:  []r30KeyCol{{name: "k", coll: "BINARY"}, {name: "v", coll: "NOCASE"}, {name: "rowid"}},
			why:  "the index's own collating sequence orders the b-tree, so 'a' and 'A' tie and fall through to the rowid",
		},
		{
			name: "three-col",
			ddl:  []string{"CREATE INDEX i2 ON t2(k,w,v)"},
			key:  []r30KeyCol{{name: "k", coll: "BINARY"}, {name: "w", coll: "BINARY"}, {name: "v", coll: "BINARY"}, {name: "rowid"}},
			why:  "every key column after the equality prefix participates, in declaration order",
		},
		{
			name: "unique",
			ddl:  []string{"CREATE UNIQUE INDEX i2 ON t2(k,w)"},
			key:  []r30KeyCol{{name: "k", coll: "BINARY"}, {name: "w", coll: "BINARY"}, {name: "rowid"}},
			why:  "a UNIQUE index over a rowid table still stores the rowid as its final key column",
		},
		{
			name: "other-index-elsewhere",
			ddl:  []string{"CREATE INDEX i2 ON t2(k,v)", "CREATE INDEX i3 ON t2(w)"},
			key:  []r30KeyCol{{name: "k", coll: "BINARY"}, {name: "v", coll: "BINARY"}, {name: "rowid"}},
			why:  "an index leading with a DIFFERENT column cannot serve this equality, so it is not a rival for the walk order",
		},
		{
			// NOT an upgrade waiting to happen in the way a decline-assertion
			// is: this states the ORDER, and if a later stream teaches the seek
			// which index the cost model picks (see indexWalkOrder's comment on
			// colUsed) the answer here changes and this case names it.
			name: "rival-leading-index-declines",
			ddl:  []string{"CREATE INDEX i2a ON t2(k,v)", "CREATE INDEX i2b ON t2(k)"},
			key:  rowidOnly,
			why:  "two indexes lead with k; which one SQLite walks depends on szIdxRow and on covering, so the order is left alone",
		},
		{
			// UPGRADE, round 41: wherePlanMultiTableOrder (where_plan_gate.go)
			// now hands wherePlanIndexList the real stmt instead of nil, so a
			// PURE expression index like i3 is represented (XN_EXPR) rather
			// than blanket-declining the whole table -- see
			// where_plan_index.go's own doc comment. The ported cost solver
			// then correctly finds i3 is NOT a rival at all for THIS query:
			// i3's only key column is the expression "k+0", and
			// sqlite3ExprCompare (which this port's own term-matching mirrors)
			// requires the SAME EXPRESSION TEXT to bind a term to a key column
			// -- "t2.k = t1.x" names the bare column "k", never "k+0", so i3
			// can never anchor that equality and always loses to i2 on cost.
			// Verified directly against the real oracle with this exact
			// fixture and DDL: it also picks "SEARCH t2 USING INDEX i2 (k=?)"
			// (EXPLAIN QUERY PLAN) and produces this exact row order.
			name: "expression-index-rival-loses-on-cost",
			ddl:  []string{"CREATE INDEX i2 ON t2(k,v)", "CREATE INDEX i3 ON t2(k+0)"},
			key:  []r30KeyCol{{name: "k", coll: "BINARY"}, {name: "v", coll: "BINARY"}, {name: "rowid"}},
			why:  "i3's key is an expression that never matches the equality term's own text, so i2 is the only real rival and wins exactly as it does with no i3 present at all",
		},
	}

	// The oracle-free control: the same query over the same data with no index
	// at all. Its row multiset is what every case's must equal.
	noIdx := r30Query(t, r30Build(t, "noidx", nil), r30SQL)
	if len(noIdx) == 0 {
		t.Fatal("fixture produced no rows -- it cannot observe an order")
	}
	wantSet := r30Multiset(noIdx)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r30Query(t, r30Build(t, tc.name, tc.ddl), r30SQL)
			if gotSet := r30Multiset(got); gotSet != wantSet {
				t.Fatalf("WRONG ROW SET with %v (%s):\n got %s\nwant %s\nthe seek changed which rows match, which it must never do",
					tc.ddl, tc.why, gotSet, wantSet)
			}
			want := r30Expect(tc.key)
			if r30Fmt(got) != r30Fmt(want) {
				t.Fatalf("ORDER with %v (%s):\n got %s\nwant %s\nthe row set is correct, so this is an ORDER divergence: either the "+
					"correlated seek stopped walking the index, or the join LOOP ORDER changed (this expectation assumes t1 drives)",
					tc.ddl, tc.why, r30Fmt(got), r30Fmt(want))
			}
		})
	}
}

// TestR30SeekOrderNeverPanicsOnPagerlessCursor covers the invariant
// cursorHasNoBtree exists for: a pre-materialized row source (derived table,
// CTE, virtual table) has no pager, and reaching a b-tree branch of rewind()
// with one dereferences nil. indexWalkOrder is on that path -- it reads
// cur.pager.Schema() -- so it gets the same explicit guard, and these shapes
// pin it.
func TestR30SeekOrderNeverPanicsOnPagerlessCursor(t *testing.T) {
	p := r30Build(t, "pagerless", []string{"CREATE INDEX i2 ON t2(k,v)"})
	for _, sql := range []string{
		"SELECT d.x, t2.rowid FROM (SELECT x FROM t1) AS d, t2 WHERE t2.k = d.x",
		"WITH c(x) AS (SELECT x FROM t1) SELECT c.x, t2.rowid FROM c, t2 WHERE t2.k = c.x",
		"SELECT t1.x, d.k FROM t1, (SELECT k FROM t2) AS d WHERE d.k = t1.x",
		"SELECT t1.x, j.value FROM t1, json_each('[1,2,3]') AS j WHERE j.value = t1.x",
	} {
		if _, _, err := p.Query(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}
