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
		// Under an OR, operands that are equal, or not, decide how it is
		// planned: an IN, x>=A from "x=A OR x>A", or neither.
		`SELECT a FROM t WHERE b = 2 OR b = 2 ORDER BY a`,
		`SELECT a FROM t WHERE b = 3 OR b = 4 ORDER BY a`,
		`SELECT a FROM t WHERE b = 5 OR b = 5 ORDER BY a`,
		`SELECT a FROM t WHERE b = 4 OR b > 4`,
		`SELECT a FROM t WHERE b = 4 OR b > 5`,
		`SELECT a FROM t WHERE b = 5 OR b > 5`,
		`SELECT a FROM t WHERE b < 3 OR b = 3`,
		`SELECT a FROM t WHERE b < 2 OR b = 4`,
		`SELECT a FROM t WHERE b = 2 OR b = 3 OR b = 2`,
		`SELECT a FROM t WHERE b = 2 OR b = 3 OR b = 6`,
		`SELECT a FROM t WHERE c = 'c1' OR c = 'C1'`,
		`SELECT a FROM t WHERE c = 'c2' OR c = 'c2'`,
		`SELECT a FROM t WHERE c = 'c3' OR c = 'c4'`,
		`SELECT a FROM t WHERE c = 'c1' OR c > 'c1'`,
		`SELECT a FROM t WHERE c = 'c2' OR c > 'C2'`,
		`SELECT a FROM t WHERE c = 'c2' OR c > 'c3'`,
		`SELECT a FROM t WHERE b = 3 OR a = 3`,
		`SELECT a FROM t WHERE b = 4 OR a = 5`,
		`SELECT a FROM t WHERE (b = 2 AND a > 10) OR (b = 2 AND a < 5)`,
		`SELECT a FROM t WHERE (b = 3 AND a > 10) OR (b = 4 AND a < 5)`,
		`SELECT a FROM t WHERE b = 3000000000 OR b = 3000000000`,
		`SELECT a FROM t WHERE b = 3000000000 OR b > 3000000000`,
		`SELECT a FROM t WHERE b = 2.5 OR b > 2.5`,
		`SELECT a FROM t WHERE b = 3.5 OR b > 4.5`,
		`SELECT a FROM t WHERE b = '3' OR b = 3`,
		`SELECT a FROM t WHERE b = '4' OR b = 4`,
		`SELECT a FROM t WHERE b = (5) OR b > 5`,
		`SELECT a FROM t WHERE b = (5) OR b > 6`,
		`SELECT a FROM t WHERE b = 2 OR b IN (2, 3)`,
		`SELECT a FROM t WHERE b = 2 OR b IN (4, 3)`,
		`SELECT t.a, u.y FROM t JOIN u ON u.x = t.b AND (u.x = 2 OR u.x = 2) ORDER BY 1, 2`,
		`SELECT t.a, u.y FROM t JOIN u ON u.x = t.b AND (u.x = 3 OR u.x = 4) ORDER BY 1, 2`,
		`SELECT a + 5, b FROM t WHERE b = 6 OR a = 6`,
		`SELECT a + 6, b FROM t WHERE b = 5 OR a = 7`,
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
	if _, ok := p.planCacheFor()[shapeIndexKey(shapeKey(`SELECT a FROM t WHERE b = 3 ORDER BY a`, literalTokens(mustLex(t, `SELECT a FROM t WHERE b = 3 ORDER BY a`)), "", false))]; !ok {
		t.Error("no shape kept for a WHERE literal")
	}
	const text = `SELECT a FROM t WHERE c = 'c1' ORDER BY a`
	if _, ok := p.planCacheFor()[shapeIndexKey(shapeKey(text, literalTokens(mustLex(t, text)), "", false))]; !ok {
		t.Error("no shape kept for a WHERE string")
	}
	for _, q := range []string{`SELECT a FROM t WHERE b IN (2, 4, 6) ORDER BY a`, `SELECT a FROM t WHERE b BETWEEN 2 AND 5 ORDER BY a`} {
		if _, ok := p.planCacheFor()[shapeIndexKey(shapeKey(q, literalTokens(mustLex(t, q)), "", false))]; !ok {
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

// TestPlanShapeResultColumns: literals that are a whole operand of
// arithmetic or a sign in the result list are a shape's parameters too, and
// each statement of the shape names its columns by its own spelling.
func TestPlanShapeResultColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path, `CREATE TABLE t(a INTEGER, b INTEGER, c TEXT COLLATE NOCASE)`)
	n, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	for i := range 40 {
		c := []string{"'x'", "'X'", "'y'", "NULL"}[i%4]
		if err := n.Exec(fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, %s)`, i, i%7, c)); err != nil {
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
	// Pairs: the second of each reuses the first's program when shared.
	pairs := []struct {
		a, b   string
		shared bool
	}{
		{`SELECT a * 3, - 7 + b FROM t WHERE b > 2`, `SELECT a * 4, - 18 + b FROM t WHERE b > 3`, true},
		{`SELECT a * 3 AS x, b FROM t WHERE b > 2`, `SELECT a * 5 AS x, b FROM t WHERE b > 2`, true},
		{`SELECT a * 3 /* three */, b FROM t WHERE b > 4`, `SELECT a * 12 /* three */, b FROM t WHERE b > 4`, true},
		{`SELECT b, a + 5   FROM t WHERE b > 3`, `SELECT b, a + 600   FROM t WHERE b > 3`, true},
		{`SELECT + 7 * a, a % 3 FROM t`, `SELECT + 8 * a, a % 4 FROM t`, true},
		{`SELECT a / 2 FROM t`, `SELECT a / 4 FROM t`, true},
		{`SELECT a / 2.0 FROM t`, `SELECT a / 4.0 FROM t`, true},
		{`SELECT - 54 * count(*), sum(a) + 3 FROM t WHERE b > 2`, `SELECT - 55 * count(*), sum(a) + 9 FROM t WHERE b > 3`, true},
		{`SELECT -9223372036854775808 + a FROM t WHERE b > 5`, `SELECT -9223372036854775807 + a FROM t WHERE b > 5`, false},
		{`SELECT 9223372036854775807 + a FROM t WHERE b > 5`, `SELECT 9223372036854775806 + a FROM t WHERE b > 5`, true},
		{`SELECT -1.5 + a FROM t WHERE b >= 5`, `SELECT -9223372036854775808 + a FROM t WHERE b >= 5`, false},
		{`SELECT -0x10 + a FROM t WHERE b > 5`, `SELECT -0x8000000000000000 + a FROM t WHERE b > 5`, false},
		{`SELECT -0.5, abs(-0.5) FROM t WHERE b > 5`, `SELECT -0.0, abs(-0.0) FROM t WHERE b > 5`, false},
		{`SELECT -0.5 * a FROM t WHERE b > 5`, `SELECT -1e-400 * a FROM t WHERE b > 5`, false},
		{`SELECT CASE WHEN a > 5 THEN a * 2 ELSE 3 END FROM t`, `SELECT CASE WHEN a > 9 THEN a * 7 ELSE 3 END FROM t`, true},
		{`SELECT a > 5, b = 2 FROM t`, `SELECT a > 7, b = 3 FROM t`, true},
		{`SELECT likelihood(a > 5, 0.5), a FROM t WHERE b > 2`, `SELECT likelihood(a > 6, 0.5), a FROM t WHERE b > 2`, true},
		{`SELECT c || 5, a * 2 FROM t WHERE b > 2`, `SELECT c || 5, a * 3 FROM t WHERE b > 2`, true},
		// Kept as written: a GROUP BY, HAVING or ORDER BY may match a result
		// column, and a "*" moves the columns.
		{`SELECT c || 5, count(*) FROM t GROUP BY c || 5`, `SELECT c || 6, count(*) FROM t GROUP BY c || 6`, false},
		{`SELECT b + 5, count(*) FROM t GROUP BY b + 5`, `SELECT b + 6, count(*) FROM t GROUP BY b + 6`, false},
		{`SELECT a + 5 FROM t WHERE b > 2 ORDER BY a + 5`, `SELECT a + 6 FROM t WHERE b > 2 ORDER BY a + 6`, false},
		{`SELECT *, a + 5 FROM t WHERE b = 3`, `SELECT *, a + 6 FROM t WHERE b = 3`, false},
		{`SELECT a > 5 OR b < 3 FROM t WHERE b > 2`, `SELECT a > 6 OR b < 4 FROM t WHERE b > 2`, true},
		// Under an OR, which literals are equal is part of the shape.
		{`SELECT a > 5 OR b < 3 FROM t WHERE b > 2`, `SELECT a > 5 OR b < 3 FROM t WHERE b > 3`, false},
		{`SELECT a > 5 OR b < 3 FROM t WHERE b > 2`, `SELECT a > 7 OR b < 4 FROM t WHERE b > 9`, true},
		{`SELECT sum(a) OVER (ORDER BY a ROWS 2 PRECEDING) * 3 FROM t`, `SELECT sum(a) OVER (ORDER BY a ROWS 2 PRECEDING) * 4 FROM t`, false},
	}
	for round := range 2 {
		for _, pr := range pairs {
			for _, q := range []string{pr.a, pr.b} {
				before := planShapeHits.Load()
				cols, got, gerr := p.QueryArgs(q, nil)
				wcols, want, werr := freshQuery(t, fresh, q, nil)
				if fmt.Sprint(cols, got, gerr) != fmt.Sprint(wcols, want, werr) {
					t.Errorf("round %d %s:\n got  %q %v %v\n want %q %v %v", round, q, cols, got, gerr, wcols, want, werr)
				}
				if hit := planShapeHits.Load() > before; q == pr.b && round == 0 && hit != pr.shared {
					t.Errorf("%s: reused the shape's program %v, want %v", q, hit, pr.shared)
				}
			}
		}
	}
}

// TestPlanCacheAdmitsWhenFull: past planCacheMax distinct statements, a new
// statement's program and a new shape are still kept, each replacing an
// older one, and the cache stays bounded.
func TestPlanCacheAdmitsWhenFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.musq")
	buildDB(t, path, `CREATE TABLE t(a INTEGER, b INTEGER)`, `INSERT INTO t VALUES(1, 5), (2, 6), (3, 5)`)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i := range planCacheMax + 50 {
		if _, _, err := p.QueryArgs(fmt.Sprintf(`SELECT a, %d FROM t`, i+2), nil); err != nil {
			t.Fatal(err)
		}
	}
	const q = `SELECT b FROM t ORDER BY a`
	for range 2 {
		if _, _, err := p.QueryArgs(q, nil); err != nil {
			t.Fatal(err)
		}
	}
	if p.cachedPlan(q) == nil {
		t.Error("a statement after the cache filled was not kept")
	}
	before := planShapeHits.Load()
	for _, s := range []string{`SELECT a FROM t WHERE b = 5`, `SELECT a FROM t WHERE b = 6`} {
		if _, _, err := p.QueryArgs(s, nil); err != nil {
			t.Fatal(err)
		}
	}
	if planShapeHits.Load() == before {
		t.Error("a shape made after the cache filled was not reused")
	}
	if n := len(p.planCacheFor()); n > planCacheMax {
		t.Errorf("the cache holds %d programs, past %d", n, planCacheMax)
	}
}
