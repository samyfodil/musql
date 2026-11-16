package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestPlanShapeSharesPrograms: statements that differ only in the literals
// their WHERE and ON compare with a column, list for one or bound it between,
// run one program, each answering as its own compile does; a literal anywhere
// else keeps them apart (#164).
func TestPlanShapeSharesPrograms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path,
		`CREATE TABLE t(a INTEGER, b INTEGER, c TEXT)`,
		`CREATE TABLE u(x INTEGER, y TEXT)`,
		`CREATE INDEX tb ON t(b)`)
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	for i := range 60 {
		if err := n.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, 'c%d')`, i, i%7, i%5)); err != nil {
			t.Fatal(err)
		}
		if err := n.Exec(fmt.Sprintf(`INSERT INTO u VALUES(%d, 'y%d')`, i%9, i%4)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.Commit(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	fresh, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	qs := []string{
		`SELECT a FROM t WHERE b = 5 ORDER BY a`,
		`SELECT a FROM t WHERE b = 3 ORDER BY a`,
		`SELECT a FROM t WHERE b = '3' ORDER BY a`,
		`SELECT a FROM t WHERE b = 3.0 ORDER BY a`,
		`SELECT a FROM t WHERE b = 3.5 ORDER BY a`,
		`SELECT a FROM t WHERE b = X'33' ORDER BY a`,
		`SELECT a FROM t WHERE 4 < b AND c <> 'c1' ORDER BY a`,
		`SELECT a FROM t WHERE 2 < b AND c <> 'c3' ORDER BY a`,
		`SELECT a + 1 FROM t WHERE b = 2 ORDER BY a`,
		`SELECT a + 2 FROM t WHERE b = 2 ORDER BY a`,
		`SELECT a, b = 2 FROM t WHERE b > 4 ORDER BY a`,
		`SELECT a, b = 3 FROM t WHERE b > 4 ORDER BY a`,
		`SELECT a FROM t WHERE c LIKE 'c1%' AND b > 1 ORDER BY a`,
		`SELECT a FROM t WHERE c LIKE 'c2%' AND b > 1 ORDER BY a`,
		`SELECT a FROM t WHERE b > ? AND a < 30 ORDER BY a`,
		`SELECT a FROM t WHERE b > ? AND a < 20 ORDER BY a`,
		`SELECT t.a, u.y FROM t JOIN u ON u.x = t.b AND u.y = 'y1' WHERE t.a < 30 ORDER BY 1, 2`,
		`SELECT t.a, u.y FROM t JOIN u ON u.x = t.b AND u.y = 'y2' WHERE t.a < 40 ORDER BY 1, 2`,
		`SELECT t.a, u.y FROM t LEFT JOIN u ON u.x = t.b AND u.x > 6 WHERE t.a < 12 ORDER BY 1, 2`,
		`SELECT t.a, u.y FROM t LEFT JOIN u ON u.x = t.b AND u.x > 3 WHERE t.a < 12 ORDER BY 1, 2`,
		`SELECT a FROM t WHERE b IN (SELECT x FROM u WHERE x > 5) ORDER BY a`,
		`SELECT a FROM t WHERE b IN (SELECT x FROM u WHERE x > 2) ORDER BY a`,
		`SELECT count(*), sum(a) FROM t WHERE b >= 2 AND b <= 4`,
		`SELECT count(*), sum(a) FROM t WHERE b >= 1 AND b <= 6`,
		`SELECT a FROM t WHERE b = 1 ORDER BY a`,
		`SELECT a FROM t WHERE b = 0 ORDER BY a`,
		`SELECT a FROM t WHERE b > -3 AND a < 9 ORDER BY a`,
		`SELECT a FROM t WHERE b > -1 AND a < 9 ORDER BY a`,
		`SELECT group_concat(c) FROM t WHERE b = 3 OR b = 5`,
		`SELECT group_concat(c) FROM t WHERE b = 2 OR b = 6`,
		`SELECT group_concat(c) FROM t WHERE (b = 2 OR a = 9) AND a > 4`,
		`SELECT group_concat(c) FROM t WHERE (b = 4 OR a = 7) AND a > 3`,
		`SELECT a FROM t WHERE b = 3 LIMIT 2`,
		`SELECT a FROM t WHERE b = 4 LIMIT 3`,
		`SELECT a FROM t WHERE c = 'c1' ORDER BY a`,
		`SELECT a FROM t WHERE c = 'c2' ORDER BY a`,
		`SELECT a FROM t WHERE c > 'c2' AND b < 5 ORDER BY a`,
		`SELECT a FROM t WHERE c > 'c3' AND b < 6 ORDER BY a`,
		`SELECT count(*), sum(a) FROM t WHERE c >= 'c2'`,
		`SELECT count(*), sum(a) FROM t WHERE c >= 'c4'`,
		`SELECT a FROM t WHERE c <> '' ORDER BY a LIMIT 3`,
		`SELECT a FROM t WHERE c = '' ORDER BY a`,
		`SELECT a FROM t WHERE b = '4' ORDER BY a`,
		`SELECT a FROM t WHERE b = '5' ORDER BY a`,
		`SELECT a FROM t WHERE b = '5x' ORDER BY a`,
		`SELECT a FROM t WHERE c = 'C1' COLLATE NOCASE ORDER BY a`,
		`SELECT a FROM t WHERE c = 'C2' COLLATE NOCASE ORDER BY a`,
		`SELECT a FROM t WHERE 'c3' = c ORDER BY a`,
		`SELECT a FROM t WHERE 'c4' = c ORDER BY a`,
		`SELECT t.a FROM t JOIN u ON u.x = t.b WHERE u.y > 'y1' AND t.c < 'c3' ORDER BY 1`,
		`SELECT t.a FROM t JOIN u ON u.x = t.b WHERE u.y > 'y2' AND t.c < 'c4' ORDER BY 1`,
		// A literal inside the operand another is compared with is made a
		// parameter first, though it comes second.
		`SELECT count(*) FROM t WHERE 5 > (b = 3) + 4`,
		`SELECT count(*) FROM t WHERE 6 > (b = 2) + 5`,
		`SELECT a FROM t WHERE 6 = (b = 4) + 5 ORDER BY a`,
		`SELECT a FROM t WHERE 7 = (b = 2) + 6 ORDER BY a`,
		`SELECT a FROM t WHERE 3 < (c = 'c1') + b ORDER BY a`,
		`SELECT a FROM t WHERE 4 < (c = 'c2') + b ORDER BY a`,
		// An IN list's numbers.
		`SELECT a FROM t WHERE b IN (2, 4, 6) ORDER BY a`,
		`SELECT a FROM t WHERE b IN (3, 5, 6) ORDER BY a`,
		`SELECT a FROM t WHERE b NOT IN (2, 4, 6) AND a < 30 ORDER BY a`,
		`SELECT a FROM t WHERE b NOT IN (3, 5, 6) AND a < 40 ORDER BY a`,
		`SELECT count(*), sum(a) FROM t WHERE b IN (2, 4)`,
		`SELECT count(*), sum(a) FROM t WHERE b IN (3, 5)`,
		`SELECT a FROM t WHERE b IN (0, 4, 5) ORDER BY a`,
		`SELECT a FROM t WHERE b IN (1, 3, 5) ORDER BY a`,
		`SELECT a FROM t WHERE b IN ('3', 4, 5) ORDER BY a`,
		`SELECT a FROM t WHERE b IN ('2', 4, 6) ORDER BY a`,
		`SELECT a FROM t WHERE b IN (2.0, 3.5, 4) ORDER BY a`,
		`SELECT a FROM t WHERE b IN (5.0, 2.5, 6) ORDER BY a`,
		`SELECT count(*) FROM t WHERE 5 > (b IN (2, 3, 4)) + 4`,
		`SELECT count(*) FROM t WHERE 6 > (b IN (3, 4, 6)) + 5`,
		`SELECT a FROM t WHERE a IN (3, 9, 27, 81) AND b > 2 ORDER BY a`,
		`SELECT a FROM t WHERE a IN (4, 8, 16, 32) AND b > 3 ORDER BY a`,
		`SELECT t.a, u.y FROM t JOIN u ON u.x = t.b AND u.x IN (2, 3, 4) WHERE t.a < 20 ORDER BY 1, 2`,
		`SELECT t.a, u.y FROM t JOIN u ON u.x = t.b AND u.x IN (3, 5, 6) WHERE t.a < 30 ORDER BY 1, 2`,
		`SELECT group_concat(a) FROM t WHERE b IN (2, 3, 4) OR a = 7`,
		`SELECT group_concat(a) FROM t WHERE b IN (3, 4, 5) OR a = 8`,
		// A BETWEEN's numbers.
		`SELECT a FROM t WHERE b BETWEEN 2 AND 5 ORDER BY a`,
		`SELECT a FROM t WHERE b BETWEEN 3 AND 6 ORDER BY a`,
		`SELECT a FROM t WHERE b NOT BETWEEN 2 AND 5 AND a < 30 ORDER BY a`,
		`SELECT a FROM t WHERE b NOT BETWEEN 3 AND 4 AND a < 40 ORDER BY a`,
		`SELECT count(*), sum(b) FROM t WHERE a BETWEEN 10 AND 40 AND b > 2`,
		`SELECT count(*), sum(b) FROM t WHERE a BETWEEN 5 AND 30 AND b > 3`,
		`SELECT a FROM t WHERE b BETWEEN 0 AND 4 ORDER BY a`,
		`SELECT a FROM t WHERE b BETWEEN 0 AND 6 ORDER BY a`,
		`SELECT a FROM t WHERE c BETWEEN 'c1' AND 'c3' AND b > 2 ORDER BY a`,
		`SELECT a FROM t WHERE c BETWEEN 'c2' AND 'c4' AND b > 3 ORDER BY a`,
		`SELECT count(*) FROM t WHERE 5 > (b BETWEEN 2 AND 4) + 4`,
		`SELECT count(*) FROM t WHERE 6 > (b BETWEEN 3 AND 5) + 5`,
	}
	args := []Value{{Typ: Int, I: 4}}
	planShapeHits.Store(0)
	for round := range 2 {
		for _, q := range qs {
			var a []Value
			if containsParam(q) {
				a = args
			}
			_, got, gerr := p.QueryArgs(q, a)
			_, want, werr := freshQuery(t, fresh, q, a)
			if fmt.Sprint(got, gerr) != fmt.Sprint(want, werr) {
				t.Errorf("round %d %s:\n got  %v %v\n want %v %v", round, q, got, gerr, want, werr)
			}
		}
	}
	// Every statement after a shape's first reused its program: in the first
	// round the second of each pair whose literals are all WHERE or ON
	// comparisons, in the second all of those.
	if n := planShapeHits.Load(); n < 20 {
		t.Errorf("%d statements reused a shape's program", n)
	}
	// The shape's program is the one kept: the second literal of a pair reused it.
	if _, ok := p.planCacheFor()[shapeIndexKey(shapeKey(`SELECT a FROM t WHERE b = 3 ORDER BY a`, literalTokens(mustLex(t, `SELECT a FROM t WHERE b = 3 ORDER BY a`)), ""))]; !ok {
		t.Error("no shape kept for a WHERE literal")
	}
	const text = `SELECT a FROM t WHERE c = 'c1' ORDER BY a`
	if _, ok := p.planCacheFor()[shapeIndexKey(shapeKey(text, literalTokens(mustLex(t, text)), ""))]; !ok {
		t.Error("no shape kept for a WHERE string")
	}
	for _, q := range []string{`SELECT a FROM t WHERE b IN (2, 4, 6) ORDER BY a`, `SELECT a FROM t WHERE b BETWEEN 2 AND 5 ORDER BY a`} {
		if _, ok := p.planCacheFor()[shapeIndexKey(shapeKey(q, literalTokens(mustLex(t, q)), ""))]; !ok {
			t.Errorf("no shape kept for %s", q)
		}
	}
}

func containsParam(q string) bool {
	for i := range q {
		if q[i] == '?' {
			return true
		}
	}
	return false
}

// freshQuery answers q on a pager whose caches are emptied first: its own
// compile, literals and all.
func freshQuery(t *testing.T, p *ReadOnlyPager, q string, args []Value) ([]string, [][]Value, error) {
	t.Helper()
	p.planCache = nil
	if p.sharedPlans != nil {
		p.sharedPlans.m = nil
	}
	planShapesOffForTest = true
	defer func() { planShapesOffForTest = false }()
	return p.QueryArgs(q, args)
}

func mustLex(t *testing.T, q string) []token {
	t.Helper()
	toks, err := lex(q)
	if err != nil {
		t.Fatal(err)
	}
	return toks
}
