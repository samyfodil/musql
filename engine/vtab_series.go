// This file implements generate_series as a built-in virtual-table module.
//
// Schema: CREATE TABLE x(value, start HIDDEN, stop HIDDEN, step HIDDEN).
// value runs from start to stop, inclusive, incrementing by step (default 1).
// step=0 is treated as 1; negative step produces descending. start defaults to 0.
// NULL bounds produce an empty result.
//
// A series longer than maxRows is declined with an error: a scan that cannot
// stream it (vtab_stream.go) materializes it. Without a stop it runs to
// 4294967295, as in C, unless a constraint on value bounds it.
package engine

import (
	"fmt"
	"math"
)

func init() {
	RegisterVtabModule("generate_series", seriesModule{})
}

// seriesModule is the generate_series VtabModule.
type seriesModule struct{}

func (seriesModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) != 0 {
		// The eponymous generate_series takes no CREATE-time module arguments;
		// its inputs arrive as call arguments / WHERE constraints instead.
		return nil, nil, fmt.Errorf("generate_series takes no module arguments")
	}
	cols := []VtabColumn{
		{Name: "value"},
		// NUMERIC, as C declares them: "start hidden" is a column whose type
		// is the word HIDDEN, which sqlite3AffinityType reads as NUMERIC, and
		// vtab.c strips the word from the type without recomputing it. So
		// "start = '1'" is true (#138).
		{Name: "start", Hidden: true, NumericAffinity: true},
		{Name: "stop", Hidden: true, NumericAffinity: true},
		{Name: "step", Hidden: true, NumericAffinity: true},
	}
	return cols, seriesTable{}, nil
}

// Column indices of the declared schema.
const (
	seriesColValue = 0
	seriesColStart = 1
	seriesColStop  = 2
	seriesColStep  = 3
)

// idxNum bits, series.c's: which inputs Filter's argv carries, in the order
// start, stop, step, then the value constraint bounding it from below (=, >=
// or >) and the one bounding it from above (<= or <).
const (
	seriesHasStart  = 0x0001
	seriesHasStop   = 0x0002
	seriesHasStep   = 0x0004
	seriesValueEq   = 0x0080
	seriesValueGE   = 0x0100
	seriesValueGT   = 0x0200
	seriesValueLE   = 0x1000
	seriesValueLT   = 0x2000
	seriesValueLow  = seriesValueEq | seriesValueGE | seriesValueGT
	seriesValueHigh = seriesValueLE | seriesValueLT
)

type seriesTable struct{}

// streams makes generate_series a vtabStreamer: filtering it again is a few
// integer operations.
func (seriesTable) streams() {}

// BestIndex consumes at most one EQ constraint per hidden bound column
// (start/stop/step), assigning argv slots in the fixed order start, stop, step
// and recording which are present in IdxNum, and, as seriesBestIndex does
// (series.c:771-824), the usable constraints on value that bound the series:
// an equality, or the last lower and the last upper bound. The engine still
// applies every constraint as WHERE, so these only narrow what is generated.
func (seriesTable) BestIndex(info *VtabIndexInfo) error {
	argv := 0
	idxNum := 0
	// Fixed order so Filter can read argv positionally: start, then stop, then step.
	inputs := []struct {
		col int
		bit int
	}{
		{seriesColStart, seriesHasStart},
		{seriesColStop, seriesHasStop},
		{seriesColStep, seriesHasStep},
	}
	for _, target := range inputs {
		for i, c := range info.Constraints {
			if c.Usable && c.Op == VtabEQ && c.Column == target.col && info.Usage[i].ArgvIndex == 0 {
				argv++
				info.Usage[i].ArgvIndex = argv
				info.Usage[i].Omit = false // engine re-applies WHERE; do not omit
				idxNum |= target.bit
				break
			}
		}
	}
	low, high := -1, -1
	for i, c := range info.Constraints {
		if !c.Usable || c.Column != seriesColValue {
			continue
		}
		switch c.Op {
		case VtabEQ:
			idxNum = idxNum&^(seriesValueLow|seriesValueHigh) | seriesValueEq
			low, high = i, -1
		case VtabGE, VtabGT:
			if idxNum&seriesValueEq == 0 {
				bit := seriesValueGE
				if c.Op == VtabGT {
					bit = seriesValueGT
				}
				idxNum = idxNum&^(seriesValueGE|seriesValueGT) | bit
				low = i
			}
		case VtabLE, VtabLT:
			if idxNum&seriesValueEq == 0 {
				bit := seriesValueLE
				if c.Op == VtabLT {
					bit = seriesValueLT
				}
				idxNum = idxNum&^seriesValueHigh | bit
				high = i
			}
		}
	}
	for _, i := range []int{low, high} {
		if i >= 0 {
			argv++
			info.Usage[i].ArgvIndex = argv
		}
	}
	// An UNUSABLE constraint on start/stop/step rejects the whole plan,
	// verbatim from seriesBestIndex:
	//
	//	series.c:867  if( (unusableMask & ~idxNum)!=0 ){
	//	series.c:868    /* The start, stop, and step columns are inputs.  Therefore if there
	//	series.c:869    ** are unusable constraints on any of start, stop, or step then
	//	series.c:870    ** this plan is unusable */
	//	series.c:871    return SQLITE_CONSTRAINT;
	//
	// See buildVtabConstraints (vtab.go) for why an unresolvable right-hand
	// side is offered unusable rather than dropped, and why the rejection is
	// final here: this engine materializes a virtual table once, standalone,
	// so there is no other loop order to cost.
	for _, c := range info.Constraints {
		if c.Usable {
			continue
		}
		for _, target := range inputs {
			if c.Column == target.col && idxNum&target.bit == 0 {
				return fmt.Errorf("%w: generate_series(): its %s input depends on another row source, which this engine materializes a virtual table before",
					errVDBEUnsupported, seriesInputName(c.Column))
			}
		}
	}
	info.IdxNum = idxNum
	return nil
}

// seriesInputName spells the hidden input column col for an error message.
func seriesInputName(col int) string {
	switch col {
	case seriesColStop:
		return "stop"
	case seriesColStep:
		return "step"
	}
	return "start"
}

func (seriesTable) Open() (VtabCursor, error) { return &seriesCursor{}, nil }

// seriesCursor walks the series: count values from base, by apart. start,
// stop and step are the inputs as given, which the hidden columns report.
type seriesCursor struct {
	start, stop, step int64
	base, by          int64
	value             int64
	count, k          uint64
}

// Filter is seriesFilter (series.c:442-662) without LIMIT, OFFSET and the
// ORDER BY flags, which this engine does not offer a module: the inputs, then
// the range the value constraints leave, then the exact last value.
func (c *seriesCursor) Filter(idxNum int, _ string, argv []Value) error {
	c.count, c.k = 0, 0
	// Any NULL input or bound -> empty result (SQLite's documented behavior).
	for _, v := range argv {
		if v.Typ == Null {
			return nil
		}
	}
	i := 0
	next := func() Value { v := argv[i]; i++; return v }
	c.start, c.stop, c.step = 0, 0xffffffff, 1
	if idxNum&seriesHasStart != 0 {
		c.start = seriesInt(next())
	}
	if idxNum&seriesHasStop != 0 {
		c.stop = seriesInt(next())
	}
	if idxNum&seriesHasStep != 0 {
		if c.step = seriesInt(next()); c.step == 0 {
			c.step = 1
		}
	}
	// A value constraint with no bound on that side starts from the whole
	// int64 range, which it then narrows.
	if idxNum&(seriesHasStart|seriesHasStep) == 0 && idxNum&seriesValueLow != 0 {
		c.start = math.MinInt64
	}
	if idxNum&(seriesHasStop|seriesHasStep) == 0 && idxNum&(seriesValueEq|seriesValueHigh) != 0 {
		c.stop = math.MaxInt64
	}
	base, term := c.start, c.stop
	step := uint64(c.step)
	if c.step < 0 {
		step = -step // |MinInt64| is 1<<63, as in C
	}
	desc := c.step < 0
	if !desc && base > term || desc && base < term {
		return nil
	}
	if idxNum&(seriesValueLow|seriesValueHigh) != 0 {
		lo, hi, ok := seriesValueRange(idxNum, next)
		if !ok {
			return nil
		}
		if !desc {
			if base < lo {
				base = int64(uint64(base) + (uint64(lo)-uint64(base))/step*step)
				if base < lo {
					if base > int64(uint64(math.MaxInt64)-step) {
						return nil
					}
					base = int64(uint64(base) + step)
				}
			}
			term = min(term, hi)
		} else {
			if base > hi {
				base = int64(uint64(base) - (uint64(base)-uint64(hi))/step*step)
				if base > hi {
					if base < int64(1<<63+step) {
						return nil
					}
					base = int64(uint64(base) - step)
				}
			}
			term = max(term, lo)
		}
	}
	var steps uint64
	if !desc {
		if base > term {
			return nil
		}
		steps = (uint64(term) - uint64(base)) / step
	} else {
		if base < term {
			return nil
		}
		steps = (uint64(base) - uint64(term)) / step
	}
	// Count the rows first, to reject a pathologically large series rather than
	// attempt to materialize it.
	const maxRows = 1 << 24 // ~16.7M; far beyond any real test, bounded for safety
	if steps >= maxRows {
		return fmt.Errorf("engine: generate_series would produce %d rows, exceeding this engine's materialization limit", steps+1)
	}
	c.base, c.by = base, int64(step)
	if desc {
		c.by = -c.by
	}
	c.count, c.value = steps+1, base
	return nil
}

// seriesValueRange is the smallest and largest value the value constraints in
// idxNum allow, reading their bounds with next, or false when none is. A bound
// that reads as a REAL (sqlite3_value_numeric_type) is rounded inward.
func seriesValueRange(idxNum int, next func() Value) (lo, hi int64, ok bool) {
	lo, hi = math.MinInt64, math.MaxInt64
	if idxNum&seriesValueEq != 0 {
		v := applyAffinityToValue(next(), affNumeric)
		if v.Typ != Float {
			n := seriesInt(v)
			return n, n, true
		}
		if r := v.F; r != math.Ceil(r) || r < -9223372036854775808.0 || r > 9223372036854775807.0 {
			return 0, 0, false
		}
		n := seriesRealToInt(v.F)
		return n, n, true
	}
	if idxNum&(seriesValueGE|seriesValueGT) != 0 {
		v := applyAffinityToValue(next(), affNumeric)
		gt := idxNum&seriesValueGT != 0
		switch r := v.F; {
		case v.Typ != Float:
			lo = seriesInt(v)
		case r <= -9223372036854775808.0:
			lo, gt = math.MinInt64, false
		case r > 9223372036854775807.0:
			return 0, 0, false
		default:
			lo = seriesRealToInt(math.Ceil(r))
			gt = gt && r == math.Ceil(r)
		}
		if gt {
			if lo == math.MaxInt64 {
				return 0, 0, false
			}
			lo++
		}
	}
	if idxNum&seriesValueHigh != 0 {
		v := applyAffinityToValue(next(), affNumeric)
		lt := idxNum&seriesValueLT != 0
		switch r := v.F; {
		case v.Typ != Float:
			hi = seriesInt(v)
		case r >= 9223372036854775807.0:
			hi, lt = math.MaxInt64, false
		case r <= -9223372036854775808.0:
			return 0, 0, false
		default:
			hi = seriesRealToInt(math.Floor(r))
			lt = lt && r == math.Floor(r)
		}
		if lt {
			if hi == math.MinInt64 {
				return 0, 0, false
			}
			hi--
		}
	}
	return lo, hi, lo <= hi
}

// seriesRealToInt is seriesRealToI64: r to the nearest int64 toward zero,
// saturating.
func seriesRealToInt(r float64) int64 {
	switch {
	case r < -9223372036854774784.0:
		return math.MinInt64
	case r > 9223372036854774784.0:
		return math.MaxInt64
	}
	return int64(r)
}

func (c *seriesCursor) Column(i int) (Value, error) {
	switch i {
	case seriesColValue:
		return Value{Typ: Int, I: c.value}, nil
	case seriesColStart:
		return Value{Typ: Int, I: c.start}, nil
	case seriesColStop:
		return Value{Typ: Int, I: c.stop}, nil
	case seriesColStep:
		return Value{Typ: Int, I: c.step}, nil
	}
	return Value{}, fmt.Errorf("engine: generate_series: no column %d", i)
}

func (c *seriesCursor) Next() error { c.k++; c.value += c.by; return nil }
func (c *seriesCursor) Eof() bool   { return c.k >= c.count }

func (c *seriesCursor) Rowid() (int64, error) { return c.value, nil }
func (c *seriesCursor) Close() error          { return nil }

// seriesInt coerces a bound Value to an int64 the way SQLite's
// sqlite3_value_int64 would for generate_series' integer inputs: an integer is
// used directly; a real is truncated toward zero; text/blob is parsed as far as
// a leading integer goes (0 if none). NULL is handled by the caller before this
// is reached.
func seriesInt(v Value) int64 {
	switch v.Typ {
	case Int:
		return v.I
	case Float:
		return int64(v.F)
	case Text, Blob:
		n, _ := parseIntegerPrefix(string(v.S))
		return n
	}
	return 0
}
