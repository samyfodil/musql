package engine

import (
	"errors"
	"fmt"
	"maps"
)

// ScalarFunction is a user-defined SQL scalar function: what
// sqlite3_create_function registers (main.c, sqlite3CreateFunc), here
// process-wide, as a virtual-table module is (RegisterVtabModule).
//
// Fn gets the call's arguments and must not keep the slice: it is the
// statement's argument buffer, reused by the next call.
type ScalarFunction struct {
	Name string
	// NArg is the argument count the function takes, or -1 for any.
	NArg int
	// Deterministic is SQLITE_DETERMINISTIC: the same arguments always give
	// the same result, so the planner may treat a call with constant arguments
	// as a constant, and an index, a generated column or a partial index's
	// WHERE may use it (resolve.c:1219-1228 refuses any other there).
	Deterministic bool
	Fn            func(args []Value) (Value, error)
}

// udfs is the registered functions, by lower-cased name.
var udfs = map[string]*ScalarFunction{}

// RegisterFunction makes f callable from SQL by its name. Like sql.Register, it
// must run before any statement does -- typically from an init function: the
// tables it adds to are read without a lock. A name that is already a function
// -- built-in or registered -- is an error.
func RegisterFunction(f ScalarFunction) error {
	name := r33sFoldIdent(f.Name)
	switch {
	case name == "":
		return errors.New("engine: a function needs a name")
	case f.Fn == nil:
		return fmt.Errorf("engine: function %s has no Fn", name)
	case f.NArg < -1 || f.NArg > 127:
		// SQLITE_MAX_FUNCTION_ARG's default (sqliteLimit.h); C refuses
		// anything outside -1..that with SQLITE_MISUSE (main.c).
		return fmt.Errorf("engine: function %s: argument count %d is not in -1..127", name, f.NArg)
	case supportedFuncs[name] || isAggregateFuncName(name):
		return fmt.Errorf("engine: %s is already a function", name)
	}
	f.Name = name
	udfs[name] = &f
	// supportedFuncs and nonConstantFuncs are the two tables the compiler
	// consults for whether a name is a function and whether a call is a
	// constant; copy-on-write, so a reader holding the old map is undisturbed.
	sf := maps.Clone(supportedFuncs)
	sf[name] = true
	supportedFuncs = sf
	if !f.Deterministic {
		nc := maps.Clone(nonConstantFuncs)
		nc[name] = true
		nonConstantFuncs = nc
	}
	return nil
}

// udfNonDeterministic reports whether name is a registered function without
// Deterministic -- one an index or a generated column may not call.
func udfNonDeterministic(name string) bool {
	f := udfs[name]
	return f != nil && !f.Deterministic
}

// callUDF calls the registered function name, if there is one.
func callUDF(name string, args []Value) (Value, bool, error) {
	f := udfs[name]
	if f == nil {
		return Value{}, false, nil
	}
	v, err := f.call(args)
	return v, true, err
}

// call runs f on args, checking the count first as C's function lookup does
// (a registered NArg matches exactly or the call does not resolve).
func (f *ScalarFunction) call(args []Value) (Value, error) {
	if f.NArg >= 0 && len(args) != f.NArg {
		return Value{}, fmt.Errorf("engine: wrong number of arguments to function %s()", f.Name)
	}
	return f.Fn(args)
}
