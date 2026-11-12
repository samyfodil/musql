package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestPlanShapeSharesPrograms: statements that differ only in the literals
// their WHERE and ON compare with a column run one program, each answering as
// its own compile does; a literal anywhere else keeps them apart (#164).
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
		`SELECT a FROM t WHERE b = 3 LIMIT 2`,
		`SELECT a FROM t WHERE b = 4 LIMIT 3`,
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
