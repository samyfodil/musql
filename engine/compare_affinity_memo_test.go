package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestCompareTextAffinityMemo: a number compared with a TEXT column takes
// TEXT affinity, so it answers as the number's text would. The VDBE keeps the
// last numbers it rendered: a BETWEEN's two constants, alternating per row,
// more constants than it keeps, and an INTEGER whose value is a REAL's bit
// pattern, must each still render as themselves.
func TestCompareTextAffinityMemo(t *testing.T) {
	old := vmJITEnabled
	vmJITEnabled = false
	defer func() { vmJITEnabled = old }()
	setup := []string{`CREATE TABLE t(id INTEGER PRIMARY KEY, x TEXT)`}
	vals := []string{"0.99", "1.0", "1.2", "1.49", "1.5", "12", "4607182418800017408", "1", "-3", "1.0e+300", "abc", ""}
	for i := range 300 {
		setup = append(setup, fmt.Sprintf(`INSERT INTO t VALUES(%d, '%s')`, i, vals[i%len(vals)]))
	}
	path := filepath.Join(t.TempDir(), "t.musq")
	buildDB(t, path, setup...)
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, c := range [][2]string{
		{`x BETWEEN 0.99 AND 1.49`, `x BETWEEN '0.99' AND '1.49'`},
		{`x = 1.0 OR x = 4607182418800017408`, `x = '1.0' OR x = '4607182418800017408'`},
		{`x = 4607182418800017408 OR x = 1.0 OR x = 12`, `x = '4607182418800017408' OR x = '1.0' OR x = '12'`},
		{`x < 1e300 AND x > 12`, `x < '1.0e+300' AND x > '12'`},
		{`x >= -3 AND x <> 1.5 AND x < 1.2`, `x >= '-3' AND x <> '1.5' AND x < '1.2'`},
		{`x = id - 299 OR x = id * 0 + 1.0`, `x = CAST(id - 299 AS TEXT) OR x = '1.0'`},
		{`x = 0.99 OR x = 1.0 OR x = 1.2 OR x = 1.49 OR x = 1.5 OR x = 12 OR x = 1 OR x = -3 OR x = 1e300 OR x = 4607182418800017408 OR x BETWEEN 1.25 AND 1.3`,
			`x = '0.99' OR x = '1.0' OR x = '1.2' OR x = '1.49' OR x = '1.5' OR x = '12' OR x = '1' OR x = '-3' OR x = '1.0e+300' OR x = '4607182418800017408' OR x BETWEEN '1.25' AND '1.3'`},
	} {
		got, want := "", ""
		for i, w := range c {
			_, rows, err := p.QueryArgs(`SELECT group_concat(id) FROM t WHERE `+w, nil)
			if err != nil {
				t.Fatalf("%s: %v", w, err)
			}
			s := fmt.Sprint(typedRows(rows))
			if i == 0 {
				got = s
			} else {
				want = s
			}
		}
		if got != want {
			t.Errorf("%s:\n got  %.200s\n want %.200s", c[0], got, want)
		}
	}
}
