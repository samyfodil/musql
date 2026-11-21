// sqlean's stats family (src/stats): the percentile, variance and standard
// deviation aggregates, each also a window function, and the stats_seq table
// function.
package engine

import (
	"fmt"
	"math"
	"slices"
)

func init() {
	perc := func(name string, p float64) sqleanAggregate {
		return sqleanAggregate{name: name, lo: 1, hi: 1, newAgg: func() udfAggState { return &statsPerc{fixed: true, p: p, hasP: true} }}
	}
	percN := func(name string) sqleanAggregate {
		return sqleanAggregate{name: name, lo: 2, hi: 2, newAgg: func() udfAggState { return &statsPerc{} }}
	}
	variance := func(name string, fn func(statsVariance) (float64, bool)) sqleanAggregate {
		return sqleanAggregate{name: name, lo: 1, hi: 1, newAgg: func() udfAggState { return &statsVarAgg{fn: fn} }}
	}
	sqleanFamilies["stats"] = &sqleanFamily{
		aggs: []sqleanAggregate{
			perc("stats_median", 50), perc("stats_p25", 25), perc("stats_p75", 75),
			perc("stats_p90", 90), perc("stats_p95", 95), perc("stats_p99", 99), percN("stats_perc"),
			variance("stats_stddev", statsVariance.stddevSamp), variance("stats_stddev_samp", statsVariance.stddevSamp),
			variance("stats_stddev_pop", statsVariance.stddevPop), variance("stats_var", statsVariance.varSamp),
			variance("stats_var_samp", statsVariance.varSamp), variance("stats_var_pop", statsVariance.varPop),
			perc("median", 50), perc("percentile_25", 25), perc("percentile_75", 75),
			perc("percentile_90", 90), perc("percentile_95", 95), perc("percentile_99", 99), percN("percentile"),
			variance("stddev", statsVariance.stddevSamp), variance("stddev_samp", statsVariance.stddevSamp),
			variance("stddev_pop", statsVariance.stddevPop), variance("variance", statsVariance.varSamp),
			variance("var_samp", statsVariance.varSamp), variance("var_pop", statsVariance.varPop),
		},
		modules: map[string]VtabModule{"stats_seq": statsSeqModule{}},
	}
}

// statsPerc is fn_perc_step/fn_perc_value: the sample, sorted on demand,
// and the percentile, fixed by the function or set by the first row.
type statsPerc struct {
	values []float64
	sorted bool
	p      float64
	hasP   bool
	fixed  bool
}

// statsPercSameEpsilon is PERC_SAME_EPSILON (stats.h).
const statsPercSameEpsilon = 0.001

func (s *statsPerc) Step(args []Value) error {
	if sqleanAnyNull(args) {
		return nil
	}
	p := s.p
	if !s.fixed {
		var ok bool
		if p, ok = mathNumeric(args[1]); !ok || !(p >= 0 && p <= 100) {
			return fmt.Errorf("percentile should be a number between 0 and 100")
		}
	}
	x, ok := mathNumeric(args[0])
	if !ok {
		return nil
	}
	if !s.hasP {
		s.p, s.hasP = p, true
	} else if math.Abs(s.p-p) > statsPercSameEpsilon {
		return fmt.Errorf("percentile should be the same for all rows")
	}
	s.sorted = len(s.values) == 0 || (s.sorted && x >= s.values[len(s.values)-1])
	s.values = append(s.values, x)
	return nil
}

// Value is sample_perc: linear interpolation between the closest ranks, at
// the zero-based index p/100*(n-1).
func (s *statsPerc) Value() (Value, error) {
	n := len(s.values)
	if n == 0 {
		return sqleanNull, nil
	}
	if !s.sorted {
		slices.Sort(s.values)
		s.sorted = true
	}
	p := s.p
	if !(p >= 0) {
		p = 0
	} else if p > 100 {
		p = 100
	}
	idx := p * float64(n-1) / 100
	lo := int(idx)
	if lo >= n-1 {
		return mathResult(s.values[n-1]), nil
	}
	frac := idx - float64(lo)
	v1, v2 := s.values[lo], s.values[lo+1]
	if frac == 0 {
		return mathResult(v1), nil
	}
	if diff := v2 - v1; !math.IsInf(diff, 0) && !math.IsNaN(diff) {
		return mathResult(v1 + diff*frac), nil
	}
	return mathResult(v1*(1-frac) + v2*frac), nil
}

// statsVariance is Variance (stats.h), Welford's running moments, with the
// infinite values only counted.
type statsVariance struct {
	n         int64
	nonfinite int64
	mean, m2  float64
}

// add is var_add.
func (v *statsVariance) add(x float64) {
	if math.IsInf(x, 0) {
		v.nonfinite++
		return
	}
	v.n++
	delta := x - v.mean
	if !math.IsInf(delta, 0) {
		v.mean += delta / float64(v.n)
	} else {
		v.mean += x/float64(v.n) - v.mean/float64(v.n)
	}
	v.m2 += delta * (x - v.mean)
}

func (v statsVariance) varSamp() (float64, bool) {
	if v.n < 2 || v.nonfinite > 0 {
		return 0, false
	}
	return v.m2 / float64(v.n-1), true
}

func (v statsVariance) varPop() (float64, bool) {
	if v.n < 1 || v.nonfinite > 0 {
		return 0, false
	}
	return v.m2 / float64(v.n), true
}

func (v statsVariance) stddevSamp() (float64, bool) {
	r, ok := v.varSamp()
	return math.Sqrt(r), ok
}

func (v statsVariance) stddevPop() (float64, bool) {
	r, ok := v.varPop()
	return math.Sqrt(r), ok
}

// statsVarAgg is fn_var_step/fn_var_value: NULL and non-numeric values are
// skipped.
type statsVarAgg struct {
	v  statsVariance
	fn func(statsVariance) (float64, bool)
}

func (a *statsVarAgg) Step(args []Value) error {
	if x, ok := mathNumeric(args[0]); ok {
		a.v.add(x)
	}
	return nil
}

func (a *statsVarAgg) Value() (Value, error) {
	r, ok := a.fn(a.v)
	if !ok {
		return sqleanNull, nil
	}
	return mathResult(r), nil
}

// statsSeqModule is stats_seq (stats/seq.c): stats_seq(start[, stop[, step]]),
// the integers from start to stop, step apart, in decreasing order for a
// negative step. Unlike generate_series it requires start.
type statsSeqModule struct{}

func (statsSeqModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) != 0 {
		return nil, nil, fmt.Errorf("stats_seq takes no module arguments")
	}
	return []VtabColumn{
		{Name: "value"},
		{Name: "start", Hidden: true, NumericAffinity: true},
		{Name: "stop", Hidden: true, NumericAffinity: true},
		{Name: "step", Hidden: true, NumericAffinity: true},
	}, statsSeqTable{}, nil
}

type statsSeqTable struct{}

// streams: a series is cheap to filter again, and its default stop of
// 4294967295 makes one far too long to materialize under a LIMIT.
func (statsSeqTable) streams() {}

// BestIndex is xbest_index: the start, stop and step equalities, in that
// order, as Filter's arguments; an unusable one rejects the plan.
func (statsSeqTable) BestIndex(info *VtabIndexInfo) error {
	argv, plan := 0, 0
	for col := 1; col <= 3; col++ {
		for i, c := range info.Constraints {
			if c.Usable && c.Op == VtabEQ && c.Column == col && info.Usage[i].ArgvIndex == 0 {
				argv++
				info.Usage[i].ArgvIndex = argv
				plan |= 1 << (col - 1)
				break
			}
		}
	}
	for _, c := range info.Constraints {
		if !c.Usable && c.Op == VtabEQ && c.Column >= 1 && c.Column <= 3 && plan&(1<<(c.Column-1)) == 0 {
			return fmt.Errorf("%w: stats_seq(): an input depends on another row source, which this engine materializes a virtual table before", errVDBEUnsupported)
		}
	}
	info.IdxNum = plan
	return nil
}

func (statsSeqTable) Open() (VtabCursor, error) { return &statsSeqCursor{eof: true}, nil }

// statsSeqCursor is series_make/series_at over the inputs as given.
type statsSeqCursor struct {
	start, stop, step int64
	first             int64
	by, last, index   uint64
	desc, eof         bool
}

// statsSeqInt is arg_int64: an INTEGER, or a REAL that is a whole number in
// int64's range.
func statsSeqInt(v Value) (int64, bool) {
	isInt, i, f, ok := mathNumericType(v)
	switch {
	case !ok:
		return 0, false
	case isInt:
		return i, true
	case !(f >= -9223372036854775808.0 && f < 9223372036854775808.0) || f != math.Trunc(f):
		return 0, false
	}
	return int64(f), true
}

func (c *statsSeqCursor) Filter(idxNum int, _ string, argv []Value) error {
	c.eof, c.index = true, 0
	if idxNum&1 == 0 {
		return fmt.Errorf("table function stats_seq requires an argument")
	}
	in := [3]int64{0, 4294967295, 1}
	k := 0
	for i := range 3 {
		if idxNum&(1<<i) == 0 {
			continue
		}
		if k >= len(argv) {
			return nil
		}
		n, ok := statsSeqInt(argv[k])
		k++
		if !ok {
			return nil
		}
		in[i] = n
	}
	c.start, c.stop, c.step = in[0], in[1], in[2]
	if c.start > c.stop {
		return nil
	}
	switch {
	case c.step == 0:
		c.by = 1
	case c.step > 0:
		c.by = uint64(c.step)
	default:
		c.by = -uint64(c.step)
	}
	c.last = (uint64(c.stop) - uint64(c.start)) / c.by
	c.desc = c.step < 0
	c.first = c.start
	if c.desc {
		c.first = int64(uint64(c.start) + c.last*c.by)
	}
	c.eof = false
	return nil
}

func (c *statsSeqCursor) Next() error {
	if c.index == c.last {
		c.eof = true
	} else {
		c.index++
	}
	return nil
}

func (c *statsSeqCursor) Eof() bool             { return c.eof }
func (c *statsSeqCursor) Rowid() (int64, error) { return int64(c.index + 1), nil }
func (c *statsSeqCursor) Close() error          { return nil }

func (c *statsSeqCursor) Column(i int) (Value, error) {
	switch i {
	case 0:
		off := c.index * c.by
		if c.desc {
			return sqleanInt(int64(uint64(c.first) - off)), nil
		}
		return sqleanInt(int64(uint64(c.first) + off)), nil
	case 1:
		return sqleanInt(c.start), nil
	case 2:
		return sqleanInt(c.stop), nil
	case 3:
		return sqleanInt(c.step), nil
	}
	return Value{}, fmt.Errorf("stats_seq has no column %d", i)
}
