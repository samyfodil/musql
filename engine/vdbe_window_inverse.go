package engine

// Moving-frame aggregates, run the way C runs them. A window aggregate over a
// frame that does not start at UNBOUNDED PRECEDING is not recomputed per row:
// C keeps one accumulator, steps each row in as the frame's end passes it and
// inverts each row out as the frame's start does (sqlite3WindowCodeStep,
// window.c). For sum/avg/total/count that is the same answer only while every
// partial result is exact; once an infinity or a NaN has been added, taking it
// back out leaves NaN behind (Inf - Inf), an integer overflow that a later
// inverse would have cancelled is already latched, and an integer total that
// went approximate stays approximate. group_concat's inverse is positional (it
// drops the front of a buffer by the length of the value and its separator), so
// a separator that varies row to row shows. windowIncrementalAggregate repeats
// the sequence of steps and inverses C performs and reads the result after each
// row.

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
)

// inverse is sumInverse / countInverse (func.c:1962, 2084): take one row back
// out of a window accumulator. The row is skipped by the same tests step applies
// (FILTER, NULL), so a row that was never stepped is never inverted.
func (it *aggItem) inverse(ctx *evalCtx, regs []Value) error {
	skip, v, _, err := it.windowOperands(ctx, regs)
	if err != nil || skip {
		return err
	}
	switch it.kind {
	case aggCountStar, aggCount:
		it.cnt--
	case aggSum, aggTotal, aggAvg:
		it.cnt--
		asInt, i, f := aggNumericContribution(v)
		it.sumInverse(asInt, i, f)
	default:
		return fmt.Errorf("engine: internal: aggregate kind %d has no inverse step", it.kind)
	}
	return nil
}

// windowOperands is the front of step: whether this row contributes at all
// (FILTER true, argument not NULL), and its argument and separator values.
func (it *aggItem) windowOperands(ctx *evalCtx, regs []Value) (skip bool, v, sepVal Value, err error) {
	if it.filter != nil {
		fv, ferr := it.rowValue(aggExprFilter, ctx, regs)
		if ferr != nil {
			return false, Value{}, Value{}, ferr
		}
		if !isTruthy(fv) {
			return true, Value{}, Value{}, nil
		}
	}
	if it.kind == aggCountStar {
		return false, Value{}, Value{}, nil
	}
	v, err = it.rowValue(aggExprArg, ctx, regs)
	if err != nil {
		return false, Value{}, Value{}, err
	}
	if it.kind == aggGroupConcat && it.sepExpr != nil {
		sepVal, err = it.rowValue(aggExprSep, ctx, regs)
		if err != nil {
			return false, Value{}, Value{}, err
		}
	}
	return v.Typ == Null, v, sepVal, nil
}

// sumInverse is sumInverse's arithmetic (func.c:1969-1992) over a classified
// contribution.
func (it *aggItem) sumInverse(asInt bool, i int64, f float64) {
	if !it.sumApprox {
		if s, ok := subInt64(it.iSum, i); ok {
			it.iSum = s
			return
		}
		it.sumOverflow = true
		it.sumApprox = true
		it.kbnInit(it.iSum)
	}
	if asInt {
		if i != math.MinInt64 {
			it.kbnStepInt64(-i)
		} else {
			it.kbnStepInt64(math.MaxInt64)
			it.kbnStepInt64(1)
		}
		return
	}
	it.kbnStep(-f)
}

// groupConcatWindow is func.c's GroupConcatCtx: the text so far plus what
// groupConcatInverse needs to take the front back off it.
type groupConcatWindow struct {
	buf          []byte
	started      bool  // str.mxAlloc != 0: a term has been appended since the buffer last emptied
	nAccum       int   // terms accumulated
	nFirstSepLen int   // separator length recorded with the first term
	sepLens      []int // per-term separator lengths, once any differed (pnSepLengths)
}

// step is groupConcatStep (func.c:2191). hasSep is "the call had two arguments".
func (g *groupConcatWindow) step(txt, sep string, hasSep bool) {
	firstTerm := !g.started
	g.started = true
	switch {
	case !hasSep:
		if !firstTerm {
			g.buf = append(g.buf, ',')
		} else {
			g.nFirstSepLen = 1
		}
	case !firstTerm:
		nSep := len(sep)
		g.buf = append(g.buf, sep...)
		if nSep != g.nFirstSepLen || g.sepLens != nil {
			if g.sepLens == nil {
				g.sepLens = make([]int, g.nAccum+1)
				for i := 0; i < g.nAccum-1; i++ {
					g.sepLens[i] = g.nFirstSepLen
				}
			} else {
				for len(g.sepLens) < g.nAccum {
					g.sepLens = append(g.sepLens, 0)
				}
			}
			if g.nAccum > 0 {
				g.sepLens[g.nAccum-1] = nSep
			}
		}
	default:
		g.nFirstSepLen = len(sep)
	}
	g.nAccum++
	g.buf = append(g.buf, txt...)
}

// inverse is groupConcatInverse (func.c:2263): drop the oldest term and the
// separator after it from the front of the buffer.
func (g *groupConcatWindow) inverse(txt string) {
	nVS := len(txt)
	g.nAccum--
	if g.sepLens != nil {
		if g.nAccum > 0 {
			nVS += g.sepLens[0]
			copy(g.sepLens, g.sepLens[1:g.nAccum])
		}
	} else {
		nVS += g.nFirstSepLen
	}
	if nVS >= len(g.buf) {
		g.buf = g.buf[:0]
	} else {
		g.buf = g.buf[nVS:]
	}
	if len(g.buf) == 0 {
		g.started = false
		g.sepLens = nil
	}
}

// value is groupConcatValue: NULL before any term, and for terms that are all
// empty the text C builds with sqlite3_result_text(context, "", 1, ...), whose
// length of 1 makes it one NUL byte (func.c:2329), else the text.
func (g *groupConcatWindow) value() Value {
	if len(g.buf) == 0 {
		if g.nAccum > 0 {
			return Value{Typ: Text, S: []byte{0}}
		}
		return Value{Typ: Null}
	}
	return Value{Typ: Text, S: append([]byte(nil), g.buf...)}
}

// windowIncrementalEligible reports whether windowAggregate can run this call
// the way C does, step in / inverse out: an aggregate C gives an inverse, a
// frame that starts after UNBOUNDED PRECEDING (so C does invert), no EXCLUDE
// clause (C then rescans the frame), nothing about the row order in doubt, and
// frames that only move forward.
func windowIncrementalEligible(call windowCall, tmpl *aggItem, spans []windowFrameSpan) bool {
	fr := call.spec.Frame
	if fr == nil || fr.Exclude || fr.Start.Type == FrameUnboundedPreceding {
		return false
	}
	switch tmpl.kind {
	case aggSum, aggTotal, aggAvg, aggCount, aggCountStar, aggGroupConcat:
	default:
		return false
	}
	if tmpl.distinct || tmpl.orderBy != nil || tmpl.orderStrict {
		return false
	}
	for k, sp := range spans {
		if sp.lo > sp.hi || sp.hi > len(spans) {
			return false
		}
		if k > 0 && (sp.lo < spans[k-1].lo || sp.hi < spans[k-1].hi) {
			return false
		}
	}
	return true
}

// windowStepsBeforeInverses is the order C performs a row's two movements in.
// Usually the inverse of the rows leaving the frame comes right after the
// previous row is returned and the step of the rows entering right before the
// next one (step, return, inverse). A RANGE frame whose start is PRECEDING with
// a PRECEDING end, or FOLLOWING, does both before it returns the row, step first
// (window.c: bRPS, and windowCodeRangeTest's AGGINVERSE before RETURN_ROW).
func windowStepsBeforeInverses(fr *WindowFrame) bool {
	return fr.Mode == FrameRange &&
		((fr.Start.Type == FramePreceding && fr.End.Type == FramePreceding) || fr.Start.Type == FrameFollowing)
}

// windowRunningEligible reports whether windowAggregate can keep one running
// accumulator for the partition: a frame fixed at the partition's first row
// whose end only moves forward, no EXCLUDE clause, and an aggregate whose value
// can be read without ending it (aggItem.finalize leaves count, sum, total,
// avg, min and max as they were), with nothing about the row order in doubt.
// It steps the rows in the order the per-row rescan does, so the values are
// the rescan's.
func windowRunningEligible(call windowCall, tmpl *aggItem, spans []windowFrameSpan) bool {
	fr := call.spec.Frame
	if fr != nil && fr.Exclude {
		return false
	}
	switch tmpl.kind {
	case aggSum, aggTotal, aggAvg, aggCount, aggCountStar, aggMin, aggMax:
	default:
		return false
	}
	if tmpl.distinct || tmpl.orderBy != nil || tmpl.orderStrict || tmpl.minMaxLastWins {
		return false
	}
	for k, sp := range spans {
		if sp.lo != 0 || sp.hi > len(spans) || (k > 0 && sp.hi < spans[k-1].hi) {
			return false
		}
	}
	return true
}

// windowPartitionKey spells k, a PARTITION BY key, as a string two keys share
// exactly when keysEqualCollated holds for them under colls: a numeric value
// by its exact number (an integral REAL is its integer, as compareIntFloat
// has it), a text by its bytes under NOCASE's ASCII fold or RTRIM's trailing
// spaces, a blob by its bytes. ok is false where that equality is not a plain
// byte equality: a UTF-16 database's comparison, or a NOCASE text holding a
// NUL (sqlite3StrNICmp stops there).
func windowPartitionKey(k []Value, colls []string, enc TextEncoding) (string, bool) {
	if isUTF16(enc) {
		return "", false
	}
	var b []byte
	for i, v := range k {
		switch v.Typ {
		case Null:
			b = append(b, 'N')
		case Int:
			b = append(b, 'I')
			b = strconv.AppendInt(b, v.I, 10)
		case Float:
			f := v.F
			if f == math.Trunc(f) && f >= -9223372036854775808 && f < 9223372036854775808 {
				b = append(b, 'I')
				b = strconv.AppendInt(b, int64(f), 10)
			} else {
				b = append(b, 'F')
				b = strconv.AppendUint(b, math.Float64bits(f), 16)
			}
		case Text:
			s := v.S
			coll := atOrEmpty(colls, i)
			switch {
			case asciiEqualFold(coll, "NOCASE"):
				if bytes.IndexByte(s, 0) >= 0 {
					return "", false
				}
				folded := make([]byte, len(s))
				for j, c := range s {
					folded[j] = asciiLowerByte(c)
				}
				s = folded
			case asciiEqualFold(coll, "RTRIM"):
				s = rtrimTrailingSpaces(s)
			}
			b = append(b, 'T')
			b = strconv.AppendInt(b, int64(len(s)), 10)
			b = append(b, ':')
			b = append(b, s...)
		case Blob:
			b = append(b, 'B')
			b = strconv.AppendInt(b, int64(len(v.S)), 10)
			b = append(b, ':')
			b = append(b, v.S...)
		default:
			return "", false
		}
		b = append(b, ';')
	}
	return string(b), true
}
