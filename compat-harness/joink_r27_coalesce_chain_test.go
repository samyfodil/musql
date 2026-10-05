package compat

// Comprehensive test for USING/NATURAL join coalesce chains at various nesting depths and column layouts.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// k27Schemas: the same chain shapes under four different column layouts.
var k27Schemas = map[string][]string{
	"sharedFirst": {
		`CREATE TABLE t1(a INT, b INT, c INT)`,
		`CREATE TABLE t2(a INT, b INT, d INT)`,
		`CREATE TABLE t4(a INT, b INT, f INT)`,
		`CREATE TABLE t5(a INT, b INT, g INT)`,
		`INSERT INTO t1 VALUES(11,21,31),(12,22,32),(15,25,35),(17,27,37)`,
		`INSERT INTO t2 VALUES(12,22,32),(13,23,33),(15,25,35),(18,28,38)`,
		`INSERT INTO t4 VALUES(11,21,31),(13,23,33),(15,25,35),(19,29,39)`,
		`INSERT INTO t5 SELECT * FROM t4 WHERE a>=15`,
	},
	"sharedMoved": {
		`CREATE TABLE t1(c INT, a INT)`,
		`CREATE TABLE t2(d INT, e INT, a INT)`,
		`CREATE TABLE t4(f INT, a INT, h INT)`,
		`CREATE TABLE t5(a INT, g INT)`,
		`INSERT INTO t1 VALUES(31,11),(32,12),(35,15),(37,17)`,
		`INSERT INTO t2 VALUES(32,1,12),(33,2,13),(35,3,15),(38,4,18)`,
		`INSERT INTO t4 VALUES(31,11,1),(33,13,2),(35,15,3),(39,19,4)`,
		`INSERT INTO t5 VALUES(15,35),(19,39)`,
	},
	"integerPK": {
		`CREATE TABLE t1(id INTEGER PRIMARY KEY, w TEXT)`,
		`CREATE TABLE t2(id INTEGER PRIMARY KEY, x TEXT)`,
		`CREATE TABLE t4(id INTEGER PRIMARY KEY, y TEXT)`,
		`CREATE TABLE t5(id INTEGER PRIMARY KEY, z INT)`,
		`INSERT INTO t1(id,w) VALUES(2,'two'),(3,'three'),(6,'six'),(7,'seven')`,
		`INSERT INTO t2(id,x) VALUES(2,'alice'),(4,'bob'),(6,'cindy'),(8,'dave')`,
		`INSERT INTO t4(id,y) VALUES(1,'red'),(2,'orange'),(3,'yellow'),(4,'green'),(5,'blue')`,
		`INSERT INTO t5(id,z) VALUES(3,333),(4,444),(5,555),(0,1000),(9,999)`,
	},
	"disjoint": {
		`CREATE TABLE t1(p INT, a INT)`,
		`CREATE TABLE t2(a INT, q INT)`,
		`CREATE TABLE t4(a INT, r INT)`,
		`CREATE TABLE t5(s INT, a INT)`,
		`INSERT INTO t1 VALUES(1,1)`,
		`INSERT INTO t2 VALUES(2,2)`,
		`INSERT INTO t4 VALUES(3,3)`,
		`INSERT INTO t5 VALUES(4,4)`,
	},
}

// k27AutoNameRE matches SQLite's ":N" uniquifier on rebuilt groups' duplicated columns, which is order-unspecified.
var k27AutoNameRE = regexp.MustCompile(`:[0-9]+$`)

// k27Compare runs q on both engines and classifies the outcome.
func k27Compare(t *testing.T, setup []string) func(q string) (kind, detail string) {
	pair := r26ObsPair(t, setup)
	return func(q string) (string, string) {
		eCols, eRows, eErr, cCols, cRows, cErr := pair(q)
		if cErr != nil {
			if eErr == nil {
				return "engine-answered-oracle-rejects", cErr.Error()
			}
			return "both-reject", cErr.Error()
		}
		if eErr != nil {
			return "declined", eErr.Error()
		}
		if len(eCols) != len(cCols) {
			return "wrong", fmt.Sprintf("column count: engine=%d cgo=%d", len(eCols), len(cCols))
		}
		for i := range cCols {
			if !k27AutoNameRE.MatchString(cCols[i]) && eCols[i] != cCols[i] {
				return "wrong", fmt.Sprintf("column %d name: engine=%q cgo=%q", i, eCols[i], cCols[i])
			}
		}
		blank := make([]string, len(eCols))
		if ok, why := queryResultsMatch(blank, eRows, blank, cRows,
			strings.Contains(strings.ToUpper(q), "ORDER BY")); !ok {
			return "wrong", why
		}
		return "ok", ""
	}
}

// TestK27CoalesceChain tests groups on both sides of coalescing joins, nested at depth 2-3, over four column layouts.
// Every combination must be answered and must match the oracle.
func TestK27CoalesceChain(t *testing.T) {
	for sname, setup := range k27Schemas {
		col := "a"
		if sname == "integerPK" {
			col = "id"
		}
		cmp := k27Compare(t, setup)
		// mustAnswer is false for a family whose shapes a DIFFERENT guard
		// legitimately declines -- checkRebuiltJoinGroups' front-move and
		// aliased-repeat rules, and the aggregate/GROUP BY-over-a-group
		// declines. Those still have to be never-WRONG here; they just do not
		// have to be served.
		sweep := func(family string, mustAnswer bool, qs []string) {
			for _, q := range qs {
				kind, detail := cmp(q)
				switch kind {
				case "ok":
				case "wrong":
					t.Errorf("[%s/%s] WRONG: %s\n  %s", sname, family, q, detail)
				case "declined":
					if mustAnswer {
						t.Errorf("[%s/%s] DECLINED a shape the ported chain serves: %s\n  %s", sname, family, q, detail)
					}
				case "engine-answered-oracle-rejects":
					t.Errorf("[%s/%s] the engine ANSWERED what the oracle rejects (%s): %s", sname, family, detail, q)
				case "both-reject":
					t.Errorf("[%s/%s] the ORACLE now rejects a shape this gate assumes it answers (%s): %s", sname, family, detail, q)
				}
			}
		}
		for _, sel := range []string{col, "*"} {
			// Group on the LEFT of the coalescing join.
			var left []string
			for _, o1 := range r26JoinOps {
				for _, o2 := range r26JoinOps {
					left = append(left, fmt.Sprintf(
						"SELECT %s FROM (t4 %s t5 USING(%s)) %s t1 USING(%s) ORDER BY 1", sel, o1, col, o2, col))
				}
			}
			sweep("left."+sel, true, left)

			// Group on the LEFT, nested twice.
			var leftNested []string
			for _, o1 := range r26JoinOps {
				for _, o2 := range r26JoinOps {
					for _, o3 := range r26JoinOps {
						leftNested = append(leftNested, fmt.Sprintf(
							"SELECT %s FROM ((t4 %s t5 USING(%s)) %s t1 USING(%s)) %s t2 USING(%s) ORDER BY 1",
							sel, o1, col, o2, col, o3, col))
					}
				}
			}
			sweep("leftNested."+sel, true, leftNested)

			// A group joined to ANOTHER group.
			var groupPair []string
			for _, o1 := range r26JoinOps {
				for _, o2 := range r26JoinOps {
					for _, o3 := range r26JoinOps {
						groupPair = append(groupPair, fmt.Sprintf(
							"SELECT %s FROM (t4 %s t5 USING(%s)) %s (t1 %s t2 USING(%s)) USING(%s) ORDER BY 1",
							sel, o1, col, o2, o3, col, col))
					}
				}
			}
			// A "*" over the SECOND group is a separate decline rule.
			sweep("groupPair."+sel, sel != "*", groupPair)
		}

		// Group on the RIGHT, nested twice. A bare "*" over a non-leading group is tested separately.
		var rightNested []string
		for _, o1 := range r26JoinOps {
			for _, o2 := range r26JoinOps {
				for _, o3 := range r26JoinOps {
					rightNested = append(rightNested, fmt.Sprintf(
						"SELECT %s FROM t2 %s (t1 %s (t4 %s t5 USING(%s)) USING(%s)) USING(%s) ORDER BY 1",
						col, o1, o2, o3, col, col, col))
				}
			}
		}
		sweep("rightNested", true, rightNested)

		// NATURAL spellings of the same chains.
		var natural []string
		for _, o1 := range r26JoinOps {
			for _, o2 := range r26JoinOps {
				n1, n2 := "NATURAL "+o1, "NATURAL "+o2
				natural = append(natural,
					fmt.Sprintf("SELECT %s FROM (t4 %s t5) %s t1 ORDER BY 1", col, n1, n2),
					fmt.Sprintf("SELECT * FROM (t4 %s t5) %s t1 ORDER BY 1", n1, n2),
					fmt.Sprintf("SELECT %s FROM ((t4 %s t5) %s t1) %s t2 ORDER BY 1", col, n1, n2, n1))
			}
		}
		sweep("natural", true, natural)

		// Other channels the coalesced value reaches: member-qualified reads, group-alias reads, aggregation, WHERE and GROUP BY.
		var channels []string
		for _, o1 := range r26JoinOps {
			for _, o2 := range r26JoinOps {
				g := fmt.Sprintf("(t4 %s t5 USING(%s)) %s t1 USING(%s)", o1, col, o2, col)
				channels = append(channels,
					fmt.Sprintf("SELECT t4.%s, t5.%s, t1.%s, %s FROM %s ORDER BY 4,1,2,3", col, col, col, col, g),
					fmt.Sprintf("SELECT count(*), count(%s), sum(%s) FROM %s", col, col, g),
					fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL ORDER BY 1", col, g, col),
					fmt.Sprintf("SELECT %s FROM %s GROUP BY %s ORDER BY 1", col, g, col),
					fmt.Sprintf("SELECT j.%s FROM (t4 %s t5 USING(%s)) AS j %s t1 USING(%s) ORDER BY 1", col, o1, col, o2, col),
				)
			}
		}
		// Aliased groups, aggregates over groups, and GROUP BY over groups are separate decline rules.
		sweep("channels", false, channels)
	}
}

// TestK27CoalesceChainDepth4 tests the 512-combination depth-4 sweep in both nesting directions.
// All combinations must be served and must match.
func TestK27CoalesceChainDepth4(t *testing.T) {
	cmp := k27Compare(t, []string{
		`CREATE TABLE t1(a INT, b INT)`,
		`CREATE TABLE t2(a INT, c INT)`,
		`CREATE TABLE t3(d INT, a INT)`,
		`CREATE TABLE t4(a INT, e INT)`,
		`CREATE TABLE t5(f INT, a INT, g INT)`,
		`INSERT INTO t1 VALUES(1,10),(2,20),(5,50)`,
		`INSERT INTO t2 VALUES(2,200),(3,300),(5,500)`,
		`INSERT INTO t3 VALUES(300,3),(400,4),(500,5)`,
		`INSERT INTO t4 VALUES(4,4000),(5,5000),(6,6000)`,
		`INSERT INTO t5 VALUES(1,5,2),(2,7,3)`,
	})
	for _, o1 := range r26JoinOps {
		for _, o2 := range r26JoinOps {
			for _, o3 := range r26JoinOps {
				for _, o4 := range r26JoinOps {
					for _, q := range []string{
						fmt.Sprintf("SELECT a FROM t1 %s (t2 %s (t3 %s (t4 %s t5 USING(a)) USING(a)) USING(a)) USING(a) ORDER BY 1", o1, o2, o3, o4),
						fmt.Sprintf("SELECT a FROM (((t1 %s t2 USING(a)) %s t3 USING(a)) %s t4 USING(a)) %s t5 USING(a) ORDER BY 1", o1, o2, o3, o4),
					} {
						kind, detail := cmp(q)
						switch kind {
						case "ok":
						case "declined":
							t.Errorf("DECLINED a depth-4 chain the ported rule serves: %s\n  %s", q, detail)
						default:
							t.Errorf("%s: %s\n  %s", kind, q, detail)
						}
					}
				}
			}
		}
	}
}
