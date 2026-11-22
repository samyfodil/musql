// sqlean's function families (github.com/nalgeon/sqlean), the extension set
// Turso and libSQL servers load: stats, crypto, uuid, regexp, text, fuzzy and
// math. Each family is opt-in, process-wide, as fts5 is (RegisterFTS5): a
// server that wants them calls RegisterSQLean once, before any statement runs.
//
// The functions are value-level bodies called by OpFunction like every other
// scalar function, and the aggregates are accumulators driven by the
// aggregate and window paths (aggUDF, sql_agg.go). Each family's file holds
// its own functions and the sqlean source it follows.
package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// sqleanVersion is what sqlean_version() returns: the sqlean release whose
// semantics these families follow.
const sqleanVersion = "0.28.4"

// sqleanScalar is one sqlean scalar function: its name, the argument counts
// it is registered with (lo..hi, hi < 0 for any count), and its body.
type sqleanScalar struct {
	name   string
	lo, hi int
	// volatile is a function sqlean registers without SQLITE_DETERMINISTIC
	// (the random UUIDs).
	volatile bool
	fn       func(args []Value) (Value, error)
}

// udfAggState is one group's (or one window frame's) running state of a
// registered aggregate: Step takes a row's arguments, Value reads the result
// so far and must leave the state as it was, since a window reads it after
// every row (xValue).
type udfAggState interface {
	Step(args []Value) error
	Value() (Value, error)
}

// sqleanAggregate is one sqlean aggregate (each is also a window function).
type sqleanAggregate struct {
	name   string
	lo, hi int
	// plain is an aggregate registered without xValue and xInverse, which is
	// no window function.
	plain bool
	// orderFree is an aggregate whose result does not depend on the order of
	// its rows (a percentile sorts them).
	orderFree bool
	newAgg    func() udfAggState
}

// sqleanFamily is what one family registers.
type sqleanFamily struct {
	scalars []sqleanScalar
	aggs    []sqleanAggregate
	// modules are the family's table-valued functions.
	modules map[string]VtabModule
}

// sqleanFamilies is every family, filled by each family's file.
var sqleanFamilies = map[string]*sqleanFamily{}

// sqleanRegistered is the families RegisterSQLean has registered.
var sqleanRegistered = map[string]bool{}

// udafs is the registered aggregates, by name.
var udafs = map[string]*sqleanAggregate{}

// sqleanFuncRows is the pragma_function_list rows of the registered families,
// in registration order (the connection half: builtin 0).
var sqleanFuncRows []pragmaFunctionListRow

// sqleanModuleNames is the registered families' modules, for
// pragma_module_list.
var sqleanModuleNames []string

// SQLeanFamilies is the families RegisterSQLean accepts.
func SQLeanFamilies() []string {
	return slices.Sorted(maps.Keys(sqleanFamilies))
}

// RegisterSQLean registers the named sqlean families -- "stats", "crypto",
// "uuid", "regexp", "text", "fuzzy", "math" -- or every family when none is
// named, along with sqlean_version(). Like RegisterFunction it must run before
// any statement does. Registering a family twice is a no-op; a name that is
// not a family, or a function name already taken, is an error.
func RegisterSQLean(families ...string) error {
	if len(families) == 0 {
		families = SQLeanFamilies()
	}
	// Every name is checked before any is registered, so a taken one leaves
	// nothing half registered and a retry without it can succeed.
	taken := func(name string) error {
		if supportedFuncs[name] || isAggregateFuncName(name) {
			return fmt.Errorf("engine: %s is already a function", name)
		}
		return nil
	}
	if len(sqleanRegistered) == 0 {
		if err := taken("sqlean_version"); err != nil {
			return err
		}
	}
	for _, name := range families {
		fam := sqleanFamilies[strings.ToLower(name)]
		if fam == nil {
			return fmt.Errorf("engine: no sqlean family %q (have %s)", name, strings.Join(SQLeanFamilies(), ", "))
		}
		if sqleanRegistered[strings.ToLower(name)] {
			continue
		}
		for _, f := range fam.scalars {
			if err := taken(f.name); err != nil {
				return err
			}
		}
		for _, a := range fam.aggs {
			if err := taken(a.name); err != nil {
				return err
			}
		}
	}
	if len(sqleanRegistered) == 0 {
		if err := registerSQLeanScalar(sqleanScalar{name: "sqlean_version", fn: func([]Value) (Value, error) {
			return Value{Typ: Text, S: []byte(sqleanVersion)}, nil
		}}); err != nil {
			return err
		}
	}
	for _, name := range families {
		name = strings.ToLower(name)
		if sqleanRegistered[name] {
			continue
		}
		fam := sqleanFamilies[name]
		for _, f := range fam.scalars {
			if err := registerSQLeanScalar(f); err != nil {
				return err
			}
		}
		for i := range fam.aggs {
			if err := registerSQLeanAggregate(&fam.aggs[i]); err != nil {
				return err
			}
		}
		for _, mod := range slices.Sorted(maps.Keys(fam.modules)) {
			RegisterVtabModule(mod, fam.modules[mod])
			sqleanModuleNames = append(sqleanModuleNames, mod)
		}
		sqleanRegistered[name] = true
	}
	return nil
}

func registerSQLeanScalar(f sqleanScalar) error {
	nArg := f.lo
	if f.lo != f.hi || f.hi < 0 {
		nArg = -1
	}
	ranged := f.lo != f.hi && f.hi >= 0
	if err := RegisterFunction(ScalarFunction{Name: f.name, NArg: nArg, Deterministic: !f.volatile, Fn: f.fn}); err != nil {
		return err
	}
	if ranged {
		u := udfs[f.name]
		u.ranged, u.lo, u.hi = true, f.lo, f.hi
	}
	flags := int64(2048) // SQLITE_DETERMINISTIC, as pragmaFunclistLine reports it
	if f.volatile {
		flags = 0
	}
	if f.hi < 0 {
		sqleanFuncRows = append(sqleanFuncRows, pragmaFunctionListRow{f.name, 0, "s", "utf8", -1, flags})
	}
	for n := f.lo; n <= f.hi; n++ {
		sqleanFuncRows = append(sqleanFuncRows, pragmaFunctionListRow{f.name, 0, "s", "utf8", int64(n), flags})
	}
	return nil
}

func registerSQLeanAggregate(a *sqleanAggregate) error {
	if supportedFuncs[a.name] || isAggregateFuncName(a.name) {
		return fmt.Errorf("engine: %s is already a function", a.name)
	}
	u := maps.Clone(udafs)
	u[a.name] = a
	udafs = u
	typ := "w"
	if a.plain {
		typ = "a"
	}
	for n := a.lo; n <= a.hi; n++ {
		sqleanFuncRows = append(sqleanFuncRows, pragmaFunctionListRow{a.name, 0, typ, "utf8", int64(n), 2048})
	}
	return nil
}

// sqleanAlias is f under another name.
func sqleanAlias(name string, f sqleanScalar) sqleanScalar {
	f.name = name
	return f
}

// sqleanText is a TEXT result.
func sqleanText(s string) Value { return Value{Typ: Text, S: []byte(s)} }

// sqleanBlob is a BLOB result.
func sqleanBlob(b []byte) Value { return Value{Typ: Blob, S: b} }

// sqleanInt is an INTEGER result.
func sqleanInt(i int64) Value { return Value{Typ: Int, I: i} }

// sqleanNull is NULL.
var sqleanNull = Value{Typ: Null}

// sqleanBytes is sqlite3_value_text or sqlite3_value_blob of v: its bytes, a
// number rendered as text.
func sqleanBytes(v Value) []byte {
	switch v.Typ {
	case Text, Blob:
		return v.S
	case Int, Float:
		return []byte(valueToText(v))
	}
	return nil
}

// sqleanAnyNull reports whether any argument is NULL.
func sqleanAnyNull(args []Value) bool {
	for _, a := range args {
		if a.Typ == Null {
			return true
		}
	}
	return false
}
