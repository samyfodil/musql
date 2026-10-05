// This file implements generate_series as a built-in virtual-table module.
//
// Schema: CREATE TABLE x(value, start HIDDEN, stop HIDDEN, step HIDDEN).
// value runs from start to stop, inclusive, incrementing by step (default 1).
// step=0 is treated as 1; negative step produces descending. start defaults to 0.
// NULL bounds produce an empty result.
//
// An unbounded series (no stop bound) is declined with an error. This engine
// materializes all FROM rows up front, unlike SQLite's lazy evaluation.
package engine

import "fmt"

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
		{Name: "start", Hidden: true},
		{Name: "stop", Hidden: true},
		{Name: "step", Hidden: true},
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

// idxNum bits: which bounds Filter's argv carries, in argv order start,stop,step.
const (
	seriesHasStart = 1 << 0
	seriesHasStop  = 1 << 1
	seriesHasStep  = 1 << 2
)

type seriesTable struct{}

// BestIndex consumes at most one EQ constraint per hidden bound column
// (start/stop/step), assigning argv slots in the fixed order start, stop, step
// and recording which are present in IdxNum. Any other constraint (e.g. a range
// on value) is left for the engine's WHERE to apply.
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

// seriesCursor eagerly materializes the whole series in Filter (matching the
// engine's eager row model) and then walks it.
type seriesCursor struct {
	vals              []int64
	start, stop, step int64
	pos               int
}

func (c *seriesCursor) Filter(idxNum int, _ string, argv []Value) error {
	// Read argv in the order BestIndex assigned: start, stop, step.
	i := 0
	next := func() Value { v := argv[i]; i++; return v }
	c.start = 0
	hasStop := false
	c.step = 1
	var startV, stopV, stepV Value
	if idxNum&seriesHasStart != 0 {
		startV = next()
	}
	if idxNum&seriesHasStop != 0 {
		stopV = next()
		hasStop = true
	}
	if idxNum&seriesHasStep != 0 {
		stepV = next()
	}
	// Any supplied NULL bound -> empty result (SQLite's documented behavior).
	if (idxNum&seriesHasStart != 0 && startV.Typ == Null) ||
		(idxNum&seriesHasStop != 0 && stopV.Typ == Null) ||
		(idxNum&seriesHasStep != 0 && stepV.Typ == Null) {
		c.vals = nil
		c.pos = 0
		return nil
	}
	if idxNum&seriesHasStart != 0 {
		c.start = seriesInt(startV)
	}
	if idxNum&seriesHasStep != 0 {
		c.step = seriesInt(stepV)
		if c.step == 0 {
			c.step = 1
		}
	}
	if !hasStop {
		return fmt.Errorf("engine: generate_series requires a bounded stop in this engine (unbounded series are not materializable)")
	}
	c.stop = seriesInt(stopV)

	// Count the rows first, to reject a pathologically large series rather than
	// attempt to allocate it (this engine materializes eagerly).
	const maxRows = 1 << 24 // ~16.7M; far beyond any real test, bounded for safety
	// In uint64: the span of a series over the whole int64 range does not fit
	// in an int64, and an overflowed count once let
	// "generate_series(-9223372036854775808, 9223372036854775807, 2)" past this
	// check into a 2^63-row loop (tabfunc01.test 1300, killed at 27 GB).
	var count uint64
	if c.step > 0 {
		if c.stop >= c.start {
			count = (uint64(c.stop)-uint64(c.start))/uint64(c.step) + 1
		}
	} else { // c.step < 0; -(step+1)+1 is |step| even for MinInt64
		if c.stop <= c.start {
			count = (uint64(c.start)-uint64(c.stop))/(uint64(-(c.step+1))+1) + 1
		}
	}
	if count > maxRows {
		return fmt.Errorf("engine: generate_series would produce %d rows, exceeding this engine's materialization limit", count)
	}
	// Exactly count values: a bound check on v itself wraps at the int64
	// ends, and nothing else stops the loop.
	c.vals = make([]int64, 0, count)
	for v, k := c.start, uint64(0); k < count; k++ {
		c.vals = append(c.vals, v)
		v += c.step
	}
	c.pos = 0
	return nil
}

func (c *seriesCursor) Next() error { c.pos++; return nil }
func (c *seriesCursor) Eof() bool   { return c.pos >= len(c.vals) }

func (c *seriesCursor) Column(i int) (Value, error) {
	switch i {
	case seriesColValue:
		return Value{Typ: Int, I: c.vals[c.pos]}, nil
	case seriesColStart:
		return Value{Typ: Int, I: c.start}, nil
	case seriesColStop:
		return Value{Typ: Int, I: c.stop}, nil
	case seriesColStep:
		return Value{Typ: Int, I: c.step}, nil
	}
	return Value{}, fmt.Errorf("engine: generate_series: no column %d", i)
}

func (c *seriesCursor) Rowid() (int64, error) { return c.vals[c.pos], nil }
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
