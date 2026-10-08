package engine

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestServiceBlocksMatchTheLoop: a compiled loop body with instructions the
// program JIT has no native form for runs them in service blocks -- the VDBE's
// own opcode code on a Value window -- and must answer exactly as the loop
// does, NULLs, text, collations, errors and all.
func TestServiceBlocksMatchTheLoop(t *testing.T) {
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER, s TEXT, n TEXT, m, c TEXT COLLATE NOCASE, r REAL)`}
	rng := rand.New(rand.NewSource(29))
	words := []string{"apple", "Banana", "cherry", "date", "x1", "X12", "x123", "é", "", "50%", "a_b"}
	for i := 1; i <= 2500; i++ {
		s := words[rng.Intn(len(words))] + fmt.Sprint(rng.Intn(30))
		n := fmt.Sprintf("'%s'", words[rng.Intn(len(words))])
		if i%9 == 0 {
			n = "NULL"
		}
		var m string
		switch i % 4 {
		case 0:
			m = fmt.Sprint(rng.Intn(100))
		case 1:
			m = fmt.Sprintf("'%d'", rng.Intn(100))
		case 2:
			m = fmt.Sprintf("%d.5", rng.Intn(100))
		default:
			m = "NULL"
		}
		c := []string{"Alpha", "alpha", "BETA", "beta", "gamma"}[i%5]
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %d, '%s', %s, %s, '%s', %d.25)`,
			i, rng.Intn(1000)-500, s, n, m, c, rng.Intn(50)))
	}
	p := newSegPair(t, stmts...)
	tv := func(s string) Value { return Value{Typ: Text, S: []byte(s)} }
	cases := []struct {
		sql  string
		args []Value
		// served: 1 a kernel answers it, -1 nothing may, 0 another fast path
		// (an index seek, a filtered count) may take it first.
		served int
	}{
		{`SELECT max(length(s)) FROM t`, nil, 1},
		{`SELECT min(length(s)) FROM t`, nil, 1},
		{`SELECT count(*) FROM t WHERE length(s) > 5`, nil, 1},
		{`SELECT sum(k) FROM t WHERE length(s) BETWEEN 3 AND 6 AND k > 0`, nil, 1},
		{`SELECT count(*) FROM t WHERE s LIKE 'x1%'`, nil, 1},
		{`SELECT count(*) FROM t WHERE s LIKE ? OR k > 400`, []Value{tv("%a%")}, 0},
		{`SELECT count(*) FROM t WHERE s NOT LIKE '%e%' AND k < 0`, nil, 1},
		{`SELECT count(*) FROM t WHERE s GLOB '[a-c]*'`, nil, 1},
		{`SELECT sum(k) FROM t WHERE upper(s) LIKE 'X1%'`, nil, 1},
		{`SELECT count(*) FROM t WHERE n LIKE '%a%'`, nil, 0}, // NULLs: LIKE is NULL, the row is skipped
		{`SELECT count(*) FROM t WHERE n IS NULL AND k > 0`, nil, 1},
		{`SELECT count(*) FROM t WHERE length(n) > 3`, nil, 0}, // length(NULL) is NULL
		{`SELECT count(length(n)) FROM t`, nil, -1},            // count of a NULL-bearing value: declined
		{`SELECT sum(length(n)) FROM t`, nil, -1},
		{`SELECT count(*) FROM t WHERE c = 'alpha'`, nil, 0}, // NOCASE comparison
		{`SELECT count(*) FROM t WHERE c > 'alpha'`, nil, 1},
		{`SELECT count(*) FROM t WHERE s = 'x123'`, nil, 0},
		{`SELECT count(*) FROM t WHERE m = 3`, nil, 0}, // affinity: '3' and 3 and NULL
		{`SELECT count(*) FROM t WHERE m > 50`, nil, 0},
		{`SELECT count(*) FROM t WHERE r > 20.5`, nil, 1},
		{`SELECT count(*) FROM t WHERE substr(s, 1, 1) = 'x'`, nil, 0},
		{`SELECT count(*) FROM t WHERE s || 'z' LIKE '%9z'`, nil, 1},
		{`SELECT count(*) FROM t WHERE instr(s, 'a') > 0 AND abs(k) < 100`, nil, 1},
		{`SELECT count(*) FROM t WHERE coalesce(n, s) LIKE 'a%'`, nil, 1},
		{`SELECT count(*) FROM t WHERE k IN (1, 2, 3) OR length(s) = 4`, nil, 1},
		{`SELECT max(k) FROM t WHERE trim(s) <> s OR s = ''`, nil, 0},
		{`SELECT count(*) FROM t WHERE random() > 0`, nil, -1}, // non-deterministic: never a service
		{`SELECT count(*) FROM t WHERE abs(s) > 0`, nil, -1},   // abs of TEXT is a native read of a text column
	}
	for _, c := range cases {
		_, want, werr := p.plain(c.sql, c.args...)
		ResetSegFilterCountersForTest()
		_, got, gerr := p.fast(c.sql, c.args...)
		served, _ := SegFilterCountersForTest()
		if (werr == nil) != (gerr == nil) {
			t.Errorf("%s: kernel error %v, loop error %v", c.sql, gerr, werr)
			continue
		}
		if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w && !strings.Contains(c.sql, "random()") {
			t.Errorf("%s %v:\n kernel %.200s\n loop   %.200s", c.sql, c.args, g, w)
		}
		if JITEnabled() && c.served == 1 && served == 0 {
			t.Errorf("%s: not served", c.sql)
		}
		if JITEnabled() && c.served == -1 && served != 0 {
			t.Errorf("%s: served, but should decline", c.sql)
		}
	}
}

// TestNativeTextLengthMatchesTheLoop: length() over a clean TEXT column runs as
// the kernel's POpTextLen -- SIMD over ASCII, the service block for any cell
// with a byte >= 0x80 -- and a column with NULLs takes the service-only
// program. Both must answer as the loop does.
func TestNativeTextLengthMatchesTheLoop(t *testing.T) {
	rng := rand.New(rand.NewSource(37))
	text := func(i int) string {
		switch i % 11 {
		case 0:
			return ""
		case 1:
			return "é" + strings.Repeat("a", rng.Intn(20))
		case 2:
			return strings.Repeat("b", 15+rng.Intn(3)) // around one SIMD chunk
		case 3:
			return strings.Repeat("c", 40+rng.Intn(10))
		case 4:
			return "x" + strings.Repeat("d", rng.Intn(30)) + "ü"
		default:
			return strings.Repeat("e", rng.Intn(25))
		}
	}
	for _, withNull := range []bool{false, true} {
		stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT, k INTEGER)`}
		for i := 1; i <= 3000; i++ {
			v := "'" + text(i) + "'"
			switch {
			case withNull && i%13 == 0:
				v = "NULL"
			case i%17 == 0:
				// An embedded NUL: length() stops at it.
				v = "'ab' || char(0) || 'cdé'"
			case i%19 == 0:
				v = "'" + strings.Repeat("f", 20) + "' || char(0) || 'é'"
			}
			stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %s, %d)`, i, v, i%7))
		}
		p := newSegPair(t, stmts...)
		for _, q := range []string{
			`SELECT max(length(s)) FROM t`,
			`SELECT min(length(s)) FROM t`,
			`SELECT sum(length(s)) FROM t`,
			`SELECT count(*) FROM t WHERE length(s) > 16`,
			`SELECT count(*) FROM t WHERE length(s) = 0 OR k = 3`,
			`SELECT sum(k) FROM t WHERE length(s) BETWEEN 15 AND 17`,
		} {
			_, want, werr := p.plain(q)
			ResetSegFilterCountersForTest()
			_, got, gerr := p.fast(q)
			served, _ := SegFilterCountersForTest()
			if (werr == nil) != (gerr == nil) {
				t.Fatalf("null=%v %s: kernel error %v, loop error %v", withNull, q, gerr, werr)
			}
			if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
				t.Errorf("null=%v %s:\n kernel %s\n loop   %s", withNull, q, g, w)
			}
			// With NULLs, length(s) is NULL on some rows, and a NULL written
			// back to a native register declines the statement: the loop
			// answers, which the comparison above already checked.
			if JITEnabled() && served == 0 && !withNull {
				t.Errorf("null=%v %s: not served", withNull, q)
			}
		}
	}
}

// TestNativeTextMatchMatchesTheLoop: "col [NOT] LIKE 'literal'" over a clean
// TEXT column runs as the kernel's POpTextMatch (equality, prefix, contains,
// ASCII-folded), and must answer as the loop does -- including under
// case_sensitive_like, which the folded program was not compiled for and so
// must not serve.
func TestNativeTextMatchMatchesTheLoop(t *testing.T) {
	rng := rand.New(rand.NewSource(43))
	alpha := []string{"x", "X", "1", "2", "a", "B", "%", "_", "é"}
	stmts := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT, k INTEGER)`}
	for i := 1; i <= 3000; i++ {
		var b strings.Builder
		for range rng.Intn(24) {
			b.WriteString(alpha[rng.Intn(len(alpha))])
		}
		v := "'" + b.String() + "'"
		if i%23 == 0 {
			v = "'x1' || char(0) || 'zz'"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d, %s, %d)`, i, v, i%9))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT max(k) FROM t WHERE s LIKE 'x1%'`,
		`SELECT max(id) FROM t WHERE s LIKE '%1x%'`,
		`SELECT min(id) FROM t WHERE s LIKE 'X1'`,
		`SELECT sum(k) FROM t WHERE s NOT LIKE '%a%'`,
		`SELECT min(id) FROM t WHERE s NOT LIKE 'x%' AND k % 3 = 0`,
		`SELECT sum(length(s)) FROM t WHERE s LIKE '%b%'`,
		`SELECT max(k) FROM t WHERE s LIKE 'zz%'`,
		`SELECT max(k) FROM t WHERE s LIKE '%xxxxxxxxxxxxxxxxx%'`, // a 17-byte piece: not native
		`SELECT max(k) FROM t WHERE s LIKE '%1'`,                  // a suffix
		`SELECT count(*) FROM t WHERE s NOT LIKE '%X2' OR k = 0`,
		`SELECT count(*) FROM t WHERE s LIKE 'x1%' OR k > 5`, // the three-valued OR epilogue
		`SELECT count(*) FROM t WHERE NOT (s LIKE 'a%') OR k = 2`,
		`SELECT count(*) FROM t WHERE s LIKE '%B%' AND (k = 1 OR s LIKE '%x%')`,
	}
	run := func(cs bool) {
		for _, q := range queries {
			ask := func(fast bool) string {
				segPeepholesOffForTest = !fast
				defer func() { segPeepholesOffForTest = false }()
				n, err := OpenWrite(p.path)
				if err != nil {
					t.Fatal(err)
				}
				defer n.Discard()
				if cs {
					if err := n.Exec(`PRAGMA case_sensitive_like = ON`); err != nil {
						t.Fatal(err)
					}
				}
				_, rows, err := n.Query(q, nil)
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return fmt.Sprint(typedRows(rows))
			}
			want := ask(false)
			if got := ask(true); got != want {
				t.Errorf("case_sensitive_like=%v %s:\n kernel %s\n loop   %s", cs, q, got, want)
			}
		}
	}
	run(false)
	run(true)
}
