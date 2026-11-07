package engine

import "testing"

// TestSeriesValueConstraintsInSQL runs the narrowing through statements: a
// BETWEEN, a series with no stop that a value bound ends (past C's default
// stop, which an upper bound on value replaces), and a correlated
// EXISTS whose equality on value reaches the inner series per outer row.
func TestSeriesValueConstraintsInSQL(t *testing.T) {
	p := newEmptyPager(t)
	cases := []struct {
		q    string
		want []int64
	}{
		{"SELECT value FROM generate_series(1,100,7) WHERE value BETWEEN 20 AND 50", []int64{22, 29, 36, 43, 50}},
		{"SELECT value FROM generate_series(1) WHERE value BETWEEN 4294967290 AND 4294967300", []int64{4294967290, 4294967291, 4294967292, 4294967293, 4294967294, 4294967295, 4294967296, 4294967297, 4294967298, 4294967299, 4294967300}},
		{"SELECT value FROM generate_series(10,1,-3) WHERE value < 9 AND value > 1", []int64{7, 4}},
		{"SELECT value FROM generate_series(1,20) WHERE value = 5.0", []int64{5}},
		{"SELECT value FROM generate_series(1,20) WHERE value = 5.5", nil},
		{"SELECT value FROM generate_series(1,20) WHERE value > 17.5", []int64{18, 19, 20}},
		{"SELECT count(*) FROM generate_series(1,2000) t WHERE EXISTS (SELECT 1 FROM generate_series(1,2000) u WHERE u.value = t.value*3)", []int64{666}},
		{"SELECT t.value FROM generate_series(1,6) t WHERE (SELECT count(*) FROM generate_series(1,6) u WHERE u.value < t.value) = 2", []int64{3}},
	}
	for _, c := range cases {
		_, rows, err := p.Query(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		if got := vtabRowsInt(t, rows, 0); !eqInts(got, c.want) {
			t.Errorf("%s = %v, want %v", c.q, got, c.want)
		}
	}
}
