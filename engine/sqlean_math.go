// sqlean's math family (src/math): the math_* names, each the SQL math
// function of the same name without the prefix (func.c's, as sqlean copies
// them), and math_round, round(). The unprefixed names sqlean also registers
// are this engine's own (scalar_math.go).
package engine

func init() {
	var fs []sqleanScalar
	for _, name := range []string{
		"ceil", "floor", "trunc", "ln", "log", "log10", "log2", "exp", "pow", "mod",
		"acos", "asin", "atan", "atan2", "cos", "sin", "tan", "cosh", "sinh", "tanh",
		"acosh", "asinh", "atanh", "sqrt", "radians", "degrees", "pi", "round",
	} {
		lo, hi, ok := mathFuncArity(name)
		if !ok {
			lo, hi = 1, 2 // round
		}
		fs = append(fs, sqleanScalar{name: "math_" + name, lo: lo, hi: hi, fn: func(args []Value) (Value, error) {
			return callScalarFuncEnc(name, args, false, UTF8, 0)
		}})
	}
	sqleanFamilies["math"] = &sqleanFamily{scalars: fs}
}
