package driver

import (
	sqldriver "database/sql/driver"

	"github.com/samyfodil/musql/engine"
)

// RegisterFunction makes fn callable from SQL as a user-defined scalar function.
// It must run before any statements (typically in init). Plain Go values
// convert to/from SQL. If deterministic, the function may be called by indexes.
func RegisterFunction(name string, nArg int, deterministic bool, fn func(args []any) (any, error)) error {
	return engine.RegisterFunction(engine.ScalarFunction{
		Name:          name,
		NArg:          nArg,
		Deterministic: deterministic,
		Fn: func(args []engine.Value) (engine.Value, error) {
			in := make([]any, len(args))
			for i, a := range args {
				in[i] = engineValueToDriver(a)
			}
			out, err := fn(in)
			if err != nil {
				return engine.Value{}, err
			}
			return driverValueToEngine(sqldriver.Value(out))
		},
	})
}
