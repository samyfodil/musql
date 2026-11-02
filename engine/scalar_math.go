// The SQL math functions of an SQLITE_ENABLE_MATH_FUNCTIONS build: func.c's
// ceilingFunc, logFunc, math1Func, math2Func and piFunc (func.c:2458-2630),
// registered at func.c:3404. The C library functions they call come from
// internal/libm, which answers as glibc does, so a REAL here prints the
// digits C SQLite prints on a glibc host.
package engine

import (
	"fmt"
	"math"

	"github.com/samyfodil/musql/internal/libm"
)

// mathFuncArgs is each math function's argument count, -1 for log's one or
// two.
var mathFuncArgs = map[string]int{
	"ceil": 1, "ceiling": 1, "floor": 1, "trunc": 1,
	"ln": 1, "log": -1, "log10": 1, "log2": 1, "exp": 1,
	"pow": 2, "power": 2, "mod": 2,
	"acos": 1, "asin": 1, "atan": 1, "atan2": 2, "cos": 1, "sin": 1, "tan": 1,
	"cosh": 1, "sinh": 1, "tanh": 1, "acosh": 1, "asinh": 1, "atanh": 1,
	"sqrt": 1, "radians": 1, "degrees": 1, "pi": 0,
}

func init() {
	for name := range mathFuncArgs {
		supportedFuncs[name] = true
	}
}

func mathFuncArity(name string) (lo, hi int, ok bool) {
	n, ok := mathFuncArgs[name]
	if n < 0 {
		return 1, 2, ok
	}
	return n, n, ok
}

// callMathFunc is name(args) for a math function, and ok == false for any
// other name. It is the first thing callScalarFuncEnc tries, so it decides
// with one switch.
func callMathFunc(name string, args []Value, enc TextEncoding) (v Value, ok bool, err error) {
	var f1 func(float64) float64
	switch name {
	case "exp":
		f1 = libm.Exp
	case "sin":
		f1 = libm.Sin
	case "cos":
		f1 = libm.Cos
	case "tan":
		f1 = libm.Tan
	case "asin":
		f1 = libm.Asin
	case "acos":
		f1 = libm.Acos
	case "atan":
		f1 = libm.Atan
	case "sinh":
		f1 = libm.Sinh
	case "cosh":
		f1 = libm.Cosh
	case "tanh":
		f1 = libm.Tanh
	case "asinh":
		f1 = libm.Asinh
	case "acosh":
		f1 = libm.Acosh
	case "atanh":
		f1 = libm.Atanh
	case "sqrt":
		f1 = math.Sqrt
	case "radians":
		// degToRad: x*(M_PI/180.0), whose double is the one nearest pi/180.
		f1 = func(x float64) float64 { return x * (math.Pi / 180) }
	case "degrees":
		f1 = func(x float64) float64 { return x * (180 / math.Pi) }
	case "pi", "ceil", "ceiling", "floor", "trunc", "ln", "log", "log10", "log2", "pow", "power", "mod", "atan2":
	default:
		return Value{}, false, nil
	}
	if lo, hi, _ := mathFuncArity(name); len(args) < lo || len(args) > hi {
		return Value{}, true, fmt.Errorf("engine: wrong number of arguments to function %s()", name)
	}
	switch name {
	case "pi":
		return Value{Typ: Float, F: math.Pi}, true, nil
	case "ceil", "ceiling", "floor", "trunc":
		return mathCeiling(name, args[0]), true, nil
	case "ln", "log", "log10", "log2":
		return mathLog(name, args, enc), true, nil
	case "pow", "power", "mod", "atan2":
		x, xok := mathNumeric(args[0])
		y, yok := mathNumeric(args[1])
		switch {
		case !xok || !yok:
			return Value{Typ: Null}, true, nil
		case name == "mod":
			return mathResult(math.Mod(x, y)), true, nil
		case name == "atan2":
			return mathResult(libm.Atan2(x, y)), true, nil
		}
		return mathResult(libm.Pow(x, y)), true, nil
	}
	x, xok := mathNumeric(args[0])
	if !xok {
		return Value{Typ: Null}, true, nil
	}
	return mathResult(f1(x)), true, nil
}

// mathCeiling is ceilingFunc: an INTEGER is its own answer, and a REAL goes
// to ceil, floor or trunc.
func mathCeiling(name string, v Value) Value {
	isInt, i, f, ok := mathNumericType(v)
	switch {
	case !ok:
		return Value{Typ: Null}
	case isInt:
		return Value{Typ: Int, I: i}
	}
	switch name {
	case "floor":
		f = math.Floor(f)
	case "trunc":
		f = math.Trunc(f)
	default:
		f = math.Ceil(f)
	}
	return Value{Typ: Float, F: f}
}

// mathLog is logFunc: ln, log or log10, and log2 of a positive argument, and
// log(B,X) for a base whose log is positive. X is read with
// sqlite3_value_double, not its numeric type, so a TEXT X counts by its
// numeric prefix: log(10,'100x') is 2.0.
func mathLog(name string, args []Value, enc TextEncoding) Value {
	x, ok := mathNumeric(args[0])
	if !ok || x <= 0 {
		return Value{Typ: Null}
	}
	if len(args) == 2 {
		b := libm.Log(x)
		if b <= 0 {
			return Value{Typ: Null}
		}
		xv := args[1]
		if xv.Typ == Blob && isUTF16(enc) {
			// sqlite3AtoF reads a BLOB in the database's encoding, ignoring
			// an odd last byte.
			xv = Value{Typ: Text, S: decodeTextBytes(enc, xv.S[:len(xv.S)&^1])}
		}
		if x = sqliteValueDouble(xv); x <= 0 {
			return Value{Typ: Null}
		}
		return mathResult(libm.Log(x) / b)
	}
	switch name {
	case "log", "log10":
		return mathResult(libm.Log10(x))
	case "log2":
		return mathResult(libm.Log2(x))
	}
	return mathResult(libm.Log(x))
}

// mathNumericType is sqlite3_value_numeric_type and the value it leaves: an
// INTEGER or a REAL as it is, TEXT that is wholly a number as that number
// (numeric affinity), and nothing else numeric.
func mathNumericType(v Value) (isInt bool, i int64, f float64, ok bool) {
	switch v.Typ {
	case Int:
		return true, v.I, float64(v.I), true
	case Float:
		return false, 0, v.F, true
	case Text:
		isF, ii, ff, ok := parseFullNumeric(string(v.S))
		switch {
		case !ok:
			return false, 0, 0, false
		case isF:
			return false, 0, ff, true
		}
		return true, ii, float64(ii), true
	}
	return false, 0, 0, false
}

// mathNumeric is sqlite3_value_double of an argument whose numeric type is
// INTEGER or REAL, and ok == false for any other.
func mathNumeric(v Value) (float64, bool) {
	_, _, f, ok := mathNumericType(v)
	return f, ok
}

// mathResult is sqlite3_result_double, under which a NaN is NULL.
func mathResult(f float64) Value {
	if f != f {
		return Value{Typ: Null}
	}
	return Value{Typ: Float, F: f}
}
