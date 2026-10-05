// Parenthesized join GROUP "*" expansion test for correcness against SQLite.
package compat

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

var pjgSetup = []string{
	`CREATE TABLE t1(a,b)`,
	`CREATE TABLE t2(a,c)`,
	`CREATE TABLE t3(a,d)`,
	`CREATE TABLE t4(k,e)`,
	`INSERT INTO t1 VALUES(1,'b1'),(2,'b2'),(NULL,'bN')`,
	`INSERT INTO t2 VALUES(2,'c2'),(3,'c3'),(NULL,'cN')`,
	`INSERT INTO t3 VALUES(2,'d2'),(4,'d4')`,
	`INSERT INTO t4 VALUES('b2','e1'),('b9','e2')`,
}

func TestParenJoinGroupStar(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM (t1 RIGHT JOIN t2 USING(a)) ORDER BY 1,2,3`,
		`SELECT * FROM (t1 NATURAL RIGHT JOIN t2) ORDER BY 1,2,3`,
		`SELECT * FROM (t1 FULL JOIN t2 USING(a)) ORDER BY 1,2,3`,
		`SELECT * FROM (t1 NATURAL FULL JOIN t2) ORDER BY 1,2,3`,
		`SELECT * FROM (t1 LEFT JOIN t2 USING(a)) ORDER BY 1,2,3`,
		`SELECT * FROM (t1 JOIN t2 USING(a)) ORDER BY 1,2,3`,
		`SELECT * FROM (t1 NATURAL JOIN t2) ORDER BY 1,2,3`,
		`SELECT * FROM t1 RIGHT JOIN t2 USING(a) ORDER BY 1,2,3`,
		`SELECT * FROM t1 FULL JOIN t2 USING(a) ORDER BY 1,2,3`,
		`SELECT a FROM (t1 RIGHT JOIN t2 USING(a)) ORDER BY 1`,
		`SELECT a,b,c FROM (t1 RIGHT JOIN t2 USING(a)) ORDER BY 1,2,3`,
		`SELECT t1.a, t2.a, b, c FROM (t1 RIGHT JOIN t2 USING(a)) ORDER BY 3,4`,
		`SELECT t1.a+0, c FROM (t1 RIGHT JOIN t2 USING(a)) ORDER BY 2`,
		`SELECT t1.a, c FROM t1 RIGHT JOIN t2 USING(a) ORDER BY 2`,
		`SELECT t1.*, t2.* FROM (t1 RIGHT JOIN t2 USING(a)) ORDER BY 1,2,3,4`,
		`SELECT t1.*, c FROM (t1 RIGHT JOIN t2 USING(a)) ORDER BY 3`,
		`SELECT t1.*, c FROM t1 RIGHT JOIN t2 USING(a) ORDER BY 3`,
		`SELECT t1.*, c FROM t1 NATURAL FULL JOIN t2 ORDER BY 3`,
		`SELECT t2.*, b FROM t1 RIGHT JOIN t2 USING(a) ORDER BY 2`,
		`SELECT * FROM (t1 RIGHT JOIN t2 USING(a) RIGHT JOIN t3 USING(a)) ORDER BY 1,2,3,4`,
		`SELECT * FROM (t1 JOIN t2 USING(a) RIGHT JOIN t3 USING(a)) ORDER BY 1,2,3,4`,
		`SELECT * FROM (t1 RIGHT JOIN t2 USING(a) JOIN t3 USING(a)) ORDER BY 1,2,3,4`,
		`SELECT * FROM (t1 RIGHT JOIN t2 USING(a)) JOIN t4 ON t4.k=b ORDER BY 1,2,3,4,5`,
		`SELECT * FROM t4 JOIN (t1 RIGHT JOIN t2 USING(a)) ON t4.k=b ORDER BY 1,2,3,4,5`,
		`SELECT * FROM t4, (t1 RIGHT JOIN t2 USING(a)) ORDER BY 1,2,3,4,5`,
		`SELECT DISTINCT * FROM (t1 RIGHT JOIN t2 USING(a)) ORDER BY 1,2,3`,
		`SELECT * FROM (t1 RIGHT JOIN t2 USING(a)) WHERE a IS NOT NULL ORDER BY 1,2,3`,
		`SELECT * FROM (t1 RIGHT JOIN t2 USING(a)) WHERE a>2 ORDER BY 1,2,3`,
	} {
		differ(t, q, append(append([]string{}, pjgSetup...), q))
	}
}

var pjgKinds = []string{"JOIN", "LEFT JOIN", "RIGHT JOIN", "FULL JOIN"}

func pjgJoin(rng *rand.Rand, tbl string) string {
	kind := pjgKinds[rng.Intn(len(pjgKinds))]
	if rng.Intn(2) == 0 {
		return fmt.Sprintf(" NATURAL %s %s", kind, tbl)
	}
	return fmt.Sprintf(" %s %s USING(a)", kind, tbl)
}

func TestParenJoinGroupStarFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260802))
	for i := 0; i < 80; i++ {
		i := i
		name := fmt.Sprintf("g%02d", i)
		t.Run(name, func(t *testing.T) {
			var stmts []string
			// Four tables. t1/t2/t3 share the join key "a" and carry one
			// private column each; values are drawn from a tiny domain (with
			// NULL in it) so matches, non-matches and NULL keys all occur
			// often. t4's key is named k, deliberately: a second FROM item
			// spelling the column "a" is the separate ambiguity decline
			// TestParenJoinGroupStarAmbiguousDecline pins, not this rule.
			stmts = append(stmts,
				`CREATE TABLE t4(k,e)`,
				`INSERT INTO t4 VALUES('b0','e0'),('b1','e1'),('zz','e2')`)
			for k, tn := range []string{"t1", "t2", "t3"} {
				priv := string(rune('b' + k))
				stmts = append(stmts, fmt.Sprintf(`CREATE TABLE %s(a,%s)`, tn, priv))
				var rows []string
				for r := 0; r < 2+rng.Intn(3); r++ {
					key := "NULL"
					if v := rng.Intn(5); v > 0 {
						key = fmt.Sprintf("%d", v)
					}
					rows = append(rows, fmt.Sprintf(`(%s,'%s%d')`, key, priv, r))
				}
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO %s VALUES%s`, tn, strings.Join(rows, ",")))
			}

			group := "(t1" + pjgJoin(rng, "t2")
			nCols := 3
			if rng.Intn(2) == 0 {
				group += pjgJoin(rng, "t3")
				nCols = 4
			}
			group += ")"

			from := group
			// leading is whether the GROUP is the FROM clause's first element.
			// It decides one thing only, and it is SQLite's rule, not this
			// engine's: a non-leading group is materialized as a subquery whose
			// duplicate columns are renamed "a:1"/"a:2", so a qualified star
			// into one is declined here (see expandSelectList, query.go, and
			// TestParenJoinGroupStarQualifiedStarDecline below). The bare "*"
			// -- this fuzz's actual subject -- is compared either way.
			leading := true
			switch rng.Intn(4) {
			case 0:
				from = group + " JOIN t4 ON t4.k=b"
				nCols += 2
			case 1:
				from = "t4 JOIN " + group + " ON t4.k=b"
				nCols += 2
				leading = false
			case 2:
				from = "t4, " + group
				nCols += 2
				leading = false
			}

			var order []string
			for k := 1; k <= nCols; k++ {
				order = append(order, fmt.Sprintf("%d", k))
			}
			q := fmt.Sprintf(`SELECT * FROM %s ORDER BY %s`, from, strings.Join(order, ","))
			stmts = append(stmts, q)
			if leading {
				stmts = append(stmts, fmt.Sprintf(`SELECT t1.* FROM %s ORDER BY 1,2`, from))
			}
			stmts = append(stmts, fmt.Sprintf(`SELECT a FROM %s ORDER BY 1`, from))
			differ(t, name+" "+q, stmts)
		})
	}
}

func TestParenJoinGroupStarNonLeadingGroupReadsRaw(t *testing.T) {
	for _, q := range []string{
		`SELECT t1.* FROM t4, (t1 RIGHT JOIN t2 USING(a)) ORDER BY 1,2`,
		`SELECT t2.* FROM t4, (t1 RIGHT JOIN t2 USING(a)) ORDER BY 1,2`,
		`SELECT t1.* FROM t4 JOIN (t1 RIGHT JOIN t2 USING(a)) ON t4.k=b ORDER BY 1,2`,
		`SELECT t1.* FROM t4 CROSS JOIN (t1 NATURAL FULL JOIN t2) ORDER BY 1,2`,
		`SELECT t1.* FROM t4 JOIN (t1 JOIN t2 ON t1.a=t2.a) ON t4.k=b ORDER BY 1,2`,
		`SELECT t2.* FROM t4 JOIN (t1 JOIN t2 ON t1.a=t2.a) ON t4.k=b ORDER BY 1,2`,
		`SELECT t1.* FROM (t1 RIGHT JOIN t2 USING(a)) JOIN t4 ON t4.k=b ORDER BY 1,2`,
	} {
		stmts := append(append([]string{}, pjgSetup...), q)
		want := run(t, "cgo", stmts)
		got := run(t, "musql", stmts)
		w, g := want[len(want)-1], got[len(got)-1]
		if fmt.Sprint(w["rows"]) != fmt.Sprint(g["rows"]) {
			t.Errorf("[%s] ROWS diverge\n  cgo:    %v\n  musql: %v", q, w["rows"], g["rows"])
			continue
		}
		wc, gc := pjgStripSuffix(w["cols"]), pjgStripSuffix(g["cols"])
		if fmt.Sprint(wc) != fmt.Sprint(gc) {
			t.Errorf("[%s] COLUMN NAMES diverge beyond the unspecified \":N\" suffix\n  cgo:    %v\n  musql: %v", q, w["cols"], g["cols"])
		}
	}
}

func pjgStripSuffix(cols any) []string {
	list, _ := cols.([]any)
	out := make([]string, 0, len(list))
	for _, c := range list {
		s, _ := c.(string)
		if i := strings.LastIndex(s, ":"); i > 0 {
			if _, err := strconv.Atoi(s[i+1:]); err == nil {
				s = s[:i]
			}
		}
		out = append(out, s)
	}
	return out
}

func TestParenJoinGroupStarAmbiguousDecline(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM t3, (t1 RIGHT JOIN t2 USING(a))`,
		`SELECT * FROM t3 JOIN (t1 RIGHT JOIN t2 USING(a)) ON t3.d=b`,
	} {
		stmts := append(append([]string{}, pjgSetup...), q)
		if res := run(t, "cgo", stmts); res[len(res)-1]["kind"] != "rows" {
			t.Errorf("oracle no longer answers %q: %v -- this decline may now be correct", q, res[len(res)-1])
		}
		if res := run(t, "musql", stmts); res[len(res)-1]["kind"] != "error" {
			t.Errorf("%q now ANSWERS in musql (%v) -- it must be right, not merely answered; "+
				"check the coalesced column against the oracle before deleting this case", q, res[len(res)-1])
		}
	}
}
