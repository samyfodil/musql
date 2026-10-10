// The scalar function library: the dispatch, the arity table, and the builtins
// that have no better home of their own.
//
// callScalarFuncEnc is what OP_Function does (vdbe.c:8850) -- it takes an
// already-evaluated argument list and produces a value. It never walks an
// expression tree; the arguments arrive in registers, compiled like anything
// else. supportedFuncs and funcArity are the prepare-time gate, so a call with
// the wrong arity is refused when the statement is compiled rather than when it
// runs, matching SQLite's own resolution.
//
// Functions with enough substance to warrant their own file already have one
// (scalar_funcs.go, scalar_datetime.go, scalar_numfmt.go).
package engine

import (
	"bytes"
	"fmt"
	"math"
	"unicode/utf8"
)

// supportedFuncs is the set of scalar functions the executor can evaluate.
// Anything else (unknown functions, and aggregates like count/sum/avg/min/max)
// is rejected at plan time by checkExprSupported.
var supportedFuncs = map[string]bool{
	"abs": true, "length": true, "octet_length": true, "lower": true, "upper": true, "substr": true,
	// substring() is substr()'s documented SQL-standard SPELLING, not a
	// separate function: C SQLite registers the identical implementation
	// under both names. Verified directly against mattn/go-sqlite3 3.53.3 --
	// substring('abcdefg',2,3) -> 'bcd', substring('abcdefg',2) -> 'bcdefg',
	// substring('abcdefg') -> "wrong number of arguments to function
	// substring()" (its OWN name in the message, which is why the alias is
	// resolved at the dispatch site below rather than rewritten at parse time).
	"substring": true,
	"coalesce":  true, "ifnull": true, "nullif": true, "typeof": true, "hex": true,
	"round": true, "sign": true, "replace": true, "instr": true, "quote": true,
	"char": true, "unicode": true, "unhex": true, "zeroblob": true, "randomblob": true,
	"unistr": true, "unistr_quote": true,
	"random": true, "likelihood": true, "likely": true, "unlikely": true, "iif": true,
	// match(x,y) is NOT the "col MATCH pattern" operator (MatchExpr, fts3/
	// fts4/fts5's own dispatch) -- it is the literal function-call spelling,
	// which C SQLite registers globally on every connection as an
	// always-failing stub (func.c:2346,
	// "sqlite3_overload_function(db, \"MATCH\", 2)" inside
	// sqlite3RegisterPerConnectionBuiltinFunctions, called unconditionally
	// at connection-open time -- not virtual-table-dependent, so this is
	// true of every build). It exists so name resolution succeeds and a
	// virtual table's xFindFunction can override it (main.c's own doc
	// comment on sqlite3_overload_function); called directly, with no
	// override in play, it always errors -- but only once actually
	// EVALUATED, so "SELECT 1 WHERE 0 AND match(1,2)" short-circuits past it
	// and succeeds (verified directly against mattn/go-sqlite3 3.53.3).
	// "regexp" is deliberately NOT given the same treatment: unlike MATCH,
	// SQLite core never registers a REGEXP stub, so a bare regexp(x,y) call
	// fails to RESOLVE at all ("no such function"), which is exactly this
	// engine's existing unsupported-function decline already.
	"match": true,
	"ltrim": true, "rtrim": true, "trim": true, "min": true, "max": true,
	"printf": true, "format": true,
	"date": true, "time": true, "datetime": true, "julianday": true, "unixepoch": true,
	"strftime": true,
	"timediff": true,
	"glob":     true,
	"like":     true,
	"concat":   true, "concat_ws": true,

	// json1 and its JSONB twins (json_funcs.go) are registered in init below
	// from jsonFuncFlags, the one table of their names. "->"/"->>" are among
	// them: never spelled that way by the tokenizer's identifier grammar, they
	// are the FuncExpr names sql_parser.go's parseConcat desugars the operator
	// tokens into, so no ordinary SQL identifier can collide with them.

	// subtype(X) is func.c's FUNCTION2(subtype, 1, 0, 0, subtypeFunc,
	// SQLITE_FUNC_TYPEOF), whose whole body is
	// "sqlite3_result_int(context, sqlite3_value_subtype(argv[0]))". It is a
	// json1 function in everything but name: the JSON subtype ('J' = 74) is
	// the only one anything in this engine or in a default SQLite build ever
	// sets, so it reports Value.Subtype (record.go) and nothing else.
	"subtype": true,

	// Both compile-option diagnostics report the ORACLE's build, so each is
	// answerable only for arguments whose answer no build can disagree about,
	// and compile_options.go declines the rest: for used() that is a name no
	// build's option list can contain, or one mattn/go-sqlite3 pins for every
	// build of itself; for get() it is an index no build's list can hold, which
	// main.c:5237 answers NULL. An IN-RANGE get() index still declines -- the
	// list's order is the oracle host's -- but the function itself exists, so
	// its arity errors are SQLite's rather than "no such function".
	"sqlite_compileoption_used": true,
	"sqlite_compileoption_get":  true,
}

// init registers the functions whose names live in their own files: the json1
// and JSONB families (jsonFuncFlags, json_funcs.go) and the always-registered
// extension functions (scalar_ext_funcs.go).
func init() {
	for name := range jsonFuncFlags {
		supportedFuncs[name] = true
	}
	for _, name := range []string{
		"sqlite_source_id", "sqlite_log", "load_extension",
		"authenticate", "auth_user_add", "auth_user_change", "auth_user_delete", "auth_enabled",
		"rtreenode", "rtreedepth", "rtreecheck", "fts3_tokenizer",
	} {
		supportedFuncs[name] = true
	}
}

// funcArity returns the [min,max] argument count supportedFuncs' function
// name accepts (max == -1 for unbounded), and ok == false for anything not
// in that set (checkExprSupported's own supportedFuncs check already rejects
// those before ever consulting funcArity). This is the single source of
// truth checkExprSupported's plan-time arity check validates against; see
// its call site's doc comment for why that check exists separately from
// evalFunc's own (still-present, redundant-by-design) per-call checks.
func funcArity(name string) (min, max int, ok bool) {
	if f := udfs[name]; f != nil {
		if f.NArg < 0 {
			return 0, -1, true
		}
		return f.NArg, f.NArg, true
	}
	if lo, hi, ok := vectorArity(name); ok {
		return lo, hi, true
	}
	switch name {
	case "abs", "length", "octet_length", "lower", "upper", "typeof", "hex":
		return 1, 1, true
	case "sqlite_compileoption_used", "sqlite_compileoption_get":
		return 1, 1, true
	case "substr", "substring":
		return 2, 3, true
	case "coalesce":
		// C SQLite requires 2+ (a 0- or 1-argument coalesce() is itself
		// a parse/bind error, not merely "returns its one argument") --
		// verified directly.
		return 2, -1, true
	case "ifnull", "nullif":
		return 2, 2, true
	case "sign", "unicode", "zeroblob", "randomblob", "unistr", "unistr_quote":
		return 1, 1, true
	case "round", "unhex", "ltrim", "rtrim", "trim":
		return 1, 2, true
	case "replace":
		return 3, 3, true
	case "instr", "likelihood":
		return 2, 2, true
	case "likely", "unlikely":
		return 1, 1, true
	case "quote":
		return 1, 1, true
	case "char":
		return 0, -1, true
	case "random":
		return 0, 0, true
	case "iif":
		return 3, 3, true
	case "match":
		return 2, 2, true
	case "min", "max":
		// The 1-argument AGGREGATE form never reaches here -- isAggregateCall
		// (sql_agg.go) routes it entirely differently before checkExprSupported
		// is ever consulted. This is the 2-or-more-argument SCALAR form only.
		return 2, -1, true
	case "printf", "format", "strftime":
		// ZERO arguments is legal and yields NULL, not a "wrong number of
		// arguments" error: C SQLite registers all three as fully variadic
		// (nArg == -1) and their implementations simply return without setting
		// a result when there is nothing to format. Verified directly against
		// mattn/go-sqlite3 3.53.3: "SELECT typeof(strftime())" is 'null',
		// "SELECT quote(format())" is 'NULL'. Contrast concat(), which really
		// does reject its 0-argument form ("wrong number of arguments to
		// function concat()") -- also verified -- and keeps min 1 above.
		return 0, -1, true
	case "date", "time", "datetime", "julianday", "unixepoch":
		return 0, -1, true
	case "timediff":
		return 2, 2, true
	case "glob":
		return 2, 2, true
	case "like":
		// like(X,Y) == "Y LIKE X"; like(X,Y,Z) == "Y LIKE X ESCAPE Z" (SQLite
		// docs). Both arities are the same built-in function overloaded on
		// argument count -- verified directly against C SQLite.
		return 2, 3, true
	case "concat":
		// SQLite 3.44+. concat() itself requires at least 1 argument
		// (concat() with zero arguments is a parse/bind-time "wrong number
		// of arguments" error, not an empty-string result) -- verified
		// directly against mattn/go-sqlite3.
		return 1, -1, true
	case "concat_ws":
		// SQLite 3.44+. concat_ws(sep, X1, ...) requires the separator PLUS
		// at least one value (concat_ws(sep) alone errors the same way) --
		// verified directly.
		return 2, -1, true
	case "subtype":
		return 1, 1, true
	}
	if lo, hi, ok := extFuncArity(name); ok {
		return lo, hi, true
	}
	return jsonFuncArity(name)
}

// callScalarFuncEnc calls a scalar function in a database whose text encoding may not
// be UTF-8. Only the functions that expose a TEXT value's BYTES differ -- hex()
// and octet_length() -- and both were verified directly: with encoding=UTF-16le,
// "SELECT hex(sqlite_version())" is 33002E00350033002E003300, i.e. even a
// BUILT-IN's result is in the database's encoding. Everything character-oriented
// (length, substr, instr, replace, upper, unicode/char, printf, quote, trim) is
// IDENTICAL in all three encodings, verified side by side, and so is untouched.
// pureCtx is C SQLite's OP_PureFunc: non-empty when this call sits inside
// a schema expression whose value must be DETERMINISTIC -- a CHECK
// constraint, a generated column or an index -- naming that context the way
// sqlite3NotPureFunc does (vdbeaux.c:5634-5641). A date/time function that
// then consults the wall clock or the local zone is
// "non-deterministic use of %s() in %s" (vdbeaux.c:5643) instead of a value.
// Empty everywhere else, which is every ordinary statement.
func callScalarFuncEnc(name string, args []Value, caseSensitiveLike bool, enc TextEncoding, pureCtx uint16) (Value, error) {
	if err := coerceUTF16BlobArgs(name, args, enc); err != nil {
		return Value{}, err
	}
	if v, ok, err := callVectorFunc(name, args); ok {
		return v, err
	}
	switch name {
	case "abs":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: abs() takes exactly 1 argument")
		}
		return fnAbs(args[0])

	case "length":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: length() takes exactly 1 argument")
		}
		return fnLength(args[0]), nil

	case "octet_length":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: octet_length() takes exactly 1 argument")
		}
		return fnOctetLength(encodedTextValue(enc, args[0])), nil

	case "concat":
		if len(args) < 1 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function concat()")
		}
		return fnConcat(args), nil

	case "concat_ws":
		if len(args) < 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function concat_ws()")
		}
		return fnConcatWs(args), nil

	case "lower":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: lower() takes exactly 1 argument")
		}
		if args[0].Typ == Null {
			return args[0], nil
		}
		return Value{Typ: Text, S: []byte(asciiFold(valueToText(args[0]), true))}, nil

	case "upper":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: upper() takes exactly 1 argument")
		}
		if args[0].Typ == Null {
			return args[0], nil
		}
		return Value{Typ: Text, S: []byte(asciiFold(valueToText(args[0]), false))}, nil

	case "substr", "substring":
		if len(args) != 2 && len(args) != 3 {
			return Value{}, fmt.Errorf("engine: %s() takes 2 or 3 arguments", name)
		}
		return fnSubstr(args), nil

	case "coalesce":
		if len(args) < 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function coalesce()")
		}
		for _, a := range args {
			if a.Typ != Null {
				return a, nil
			}
		}
		return Value{Typ: Null}, nil

	case "ifnull":
		if len(args) != 2 {
			return Value{}, fmt.Errorf("engine: ifnull() takes exactly 2 arguments")
		}
		if args[0].Typ != Null {
			return args[0], nil
		}
		return args[1], nil

	case "nullif":
		if len(args) != 2 {
			return Value{}, fmt.Errorf("engine: nullif() takes exactly 2 arguments")
		}
		if args[0].Typ != Null && args[1].Typ != Null && compareValues(args[0], args[1]) == 0 {
			return Value{Typ: Null}, nil
		}
		return args[0], nil

	case "typeof":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: typeof() takes exactly 1 argument")
		}
		return Value{Typ: Text, S: []byte(typeName(args[0]))}, nil

	case "sqlite_compileoption_used":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: sqlite_compileoption_used() takes exactly 1 argument")
		}
		return evalCompileOptionUsed(args[0])

	case "sqlite_compileoption_get":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: sqlite_compileoption_get() takes exactly 1 argument")
		}
		return evalCompileOptionGet(args[0])

	case "hex":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: hex() takes exactly 1 argument")
		}
		if len(args[0].S) > sqliteMaxLength/2 {
			return Value{}, fmt.Errorf("engine: string or blob too big")
		}
		return fnHex(encodedTextValue(enc, args[0])), nil

	case "round":
		if len(args) != 1 && len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function round()")
		}
		return fnRound(args)

	case "sign":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: sign() takes exactly 1 argument")
		}
		return fnSign(args[0]), nil

	case "replace":
		if len(args) != 3 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function replace()")
		}
		return fnReplace(args), nil

	case "instr":
		if len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function instr()")
		}
		return fnInstr(args[0], args[1]), nil

	case "quote":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: quote() takes exactly 1 argument")
		}
		return fnQuote(args[0]), nil

	case "char":
		return fnChar(args), nil

	case "unicode":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: unicode() takes exactly 1 argument")
		}
		return fnUnicode(args[0]), nil

	case "unistr":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function unistr()")
		}
		return fnUnistr(args[0])

	case "unistr_quote":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function unistr_quote()")
		}
		return fnUnistrQuote(args[0]), nil

	case "unhex":
		if len(args) != 1 && len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function unhex()")
		}
		return fnUnhex(args), nil

	case "zeroblob":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: zeroblob() takes exactly 1 argument")
		}
		return fnZeroblob(args[0])

	case "randomblob":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: randomblob() takes exactly 1 argument")
		}
		return fnRandomblob(args[0])

	case "random":
		if len(args) != 0 {
			return Value{}, fmt.Errorf("engine: random() takes exactly 0 arguments")
		}
		return fnRandom(), nil

	case "likelihood":
		if len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function likelihood()")
		}
		// See checkLikelihoodLiteralArg: a FLOAT literal in [0.0, 1.0]. By
		// the time this runs the parse-time check has already accepted the
		// call, so this only re-guards a value that reached evaluation.
		if args[1].Typ != Float || !(args[1].F >= 0.0 && args[1].F <= 1.0) {
			return Value{}, fmt.Errorf("engine: second argument to likelihood() must be a constant between 0.0 and 1.0")
		}
		return args[0], nil

	case "likely":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: likely() takes exactly 1 argument")
		}
		return args[0], nil

	case "unlikely":
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: unlikely() takes exactly 1 argument")
		}
		return args[0], nil

	case "iif":
		if len(args) != 3 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function iif()")
		}
		return fnIif(args[0], args[1], args[2]), nil

	case "match":
		if len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function match()")
		}
		return Value{}, fmt.Errorf("engine: unable to use function MATCH in the requested context")

	case "ltrim":
		if len(args) != 1 && len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function ltrim()")
		}
		return fnTrim(args, true, false), nil

	case "rtrim":
		if len(args) != 1 && len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function rtrim()")
		}
		return fnTrim(args, false, true), nil

	case "trim":
		if len(args) != 1 && len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function trim()")
		}
		return fnTrim(args, true, true), nil

	case "min", "max":
		if len(args) < 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function %s()", name)
		}
		return fnMinMaxScalar(name, args), nil

	case "printf", "format":
		// No format string at all -> NULL (see funcArity's evidence).
		if len(args) < 1 {
			return Value{Typ: Null}, nil
		}
		if args[0].Typ == Null {
			return Value{Typ: Null}, nil
		}
		// An EMPTY format string is NULL too, and an empty OUTPUT is not.
		// printfFunc (func.c:311) inits its StrAccum with a nil buffer and hands
		// sqlite3StrAccumFinish's result straight to sqlite3_result_text, so a
		// run that never appends leaves zText nil and the result is NULL. A
		// conversion specifier always appends, even when it contributes nothing:
		// sqlite3_str_append(p, z, 0) with nAlloc 0 still takes the
		// "p->nChar+N >= p->nAlloc" branch into enlargeAndAppend, which
		// allocates. So the split is on the FORMAT, not on the output --
		// verified against 3.53.3:
		//
		//	printf('')  printf('',1)  format('',1)   -> NULL
		//	printf('%s','')  printf('%.0s','abc')    -> '' (text)
		//	printf('%s', x'')                        -> '' (text)
		if len(valueToText(args[0])) == 0 {
			return Value{Typ: Null}, nil
		}
		return fnPrintf(valueToText(args[0]), args[1:])

	case "date", "time", "datetime", "julianday", "unixepoch":
		v, usedNow, err := fnDateTimeFamily(name, args)
		return pureFuncGuard(name, pureCtx, usedNow, v, err)

	case "timediff":
		v, usedNow, err := fnTimediff(args)
		return pureFuncGuard(name, pureCtx, usedNow, v, err)

	case "strftime":
		// No format string at all -> NULL (see funcArity's evidence).
		if len(args) < 1 {
			return Value{Typ: Null}, nil
		}
		v, usedNow, err := fnStrftime(args)
		return pureFuncGuard(name, pureCtx, usedNow, v, err)

	case "glob":
		// "glob(X,Y) is equivalent to the expression 'Y GLOB X'" (SQLite
		// docs, verified directly: glob('a*','abc') is TRUE, glob('abc','a*')
		// is FALSE) -- X (args[0]) is the PATTERN, Y (args[1]) is the string
		// being matched, the opposite order from the args slice's own
		// left-to-right call-site order.
		if len(args) != 2 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function glob()")
		}
		if args[0].Typ == Null || args[1].Typ == Null {
			return Value{Typ: Null}, nil
		}
		return boolValue(globMatch(valueToText(args[0]), valueToText(args[1]))), nil

	case "like":
		// "like(X,Y) is equivalent to the expression 'Y LIKE X'" (SQLite
		// docs, mirroring glob() above): X (args[0]) is the PATTERN, Y
		// (args[1]) is the string being matched. The optional third
		// argument (args[2]) is the ESCAPE character, evaluated and
		// validated by the exact same rule as the infix "... ESCAPE ..."
		// clause -- see likeEscapeRune's doc comment (including its
		// NULL-propagates / error-takes-precedence-over-NULL ordering,
		// verified directly for this 3-argument form too).
		if len(args) != 2 && len(args) != 3 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function like()")
		}
		var (
			esc     rune
			hasEsc  bool
			escNull bool
		)
		if len(args) == 3 {
			r, isNull, err := likeEscapeRune(args[2])
			if err != nil {
				return Value{}, err
			}
			if isNull {
				escNull = true
			} else {
				esc, hasEsc = r, true
			}
		}
		if escNull || args[0].Typ == Null || args[1].Typ == Null {
			return Value{Typ: Null}, nil
		}
		if hasEsc {
			return boolValue(likeMatchEscape(valueToText(args[0]), valueToText(args[1]), esc, caseSensitiveLike)), nil
		}
		return boolValue(likeMatch(valueToText(args[0]), valueToText(args[1]), caseSensitiveLike)), nil

	case "subtype":
		// func.c's subtypeFunc, verbatim: the argument's function subtype as an
		// integer. json1 is the only thing in a default build that ever sets
		// one, and it always sets JSON_SUBTYPE ('J', 74 -- json.c's
		// "#define JSON_SUBTYPE 74"), so this reads Value.Subtype and reports
		// that constant or 0. Nothing else needs to change for it to be right:
		// the subtype already propagates by ordinary value copy (record.go's
		// Value.Subtype doc comment lists the constructs that keep it and the
		// ones that build a fresh Value and so lose it), which is exactly what
		// makes subtype() a READ-OUT of an existing model rather than a new
		// one -- including the two shapes subtype1.test asks for, where a CTE
		// column reference answers 0 whether the CTE is MATERIALIZED or NOT.
		if len(args) != 1 {
			return Value{}, fmt.Errorf("engine: wrong number of arguments to function subtype()")
		}
		return Value{Typ: Int, I: int64(args[0].Subtype)}, nil

	case "fts5_locale":
		return fts5LocaleFunc(args)

	default:
		if _, ok := jsonFuncFlags[name]; ok {
			return callJSONFunc(name, args, enc)
		}
		if v, ok, err := callExtFunc(name, args, enc); ok {
			return v, err
		}
		if v, ok, err := callUDF(name, args); ok {
			return v, err
		}
		return Value{}, fmt.Errorf("engine: unsupported function %s()", name)
	}
}

// fnAbs implements abs(). math.MinInt64's magnitude has no positive int64
// representation, so C SQLite's abs() raises "integer overflow" for that
// EXACT integer value -- but ONLY when the argument is ITSELF already a
// genuine INTEGER (verified: abs(-9223372036854775808) errors, while
// abs(-9223372036854775808.0) -- already a REAL -- does not, simply
// returning the [inexact, as any float64 magnitude that large already is]
// positive double). A TEXT or BLOB argument ALWAYS returns REAL and NEVER
// raises this overflow error, even when its text happens to read as that
// exact integer -- verified directly against C SQLite: typeof(abs('5'))
// is "real" (not "integer"), and abs('-9223372036854775808') returns the
// plain double 9223372036854775808.0 with no error at all, because real
// SQLite's abs() converts a TEXT/BLOB argument via sqlite3_value_double()
// (REAL semantics throughout), never sqlite3_value_int64(). An earlier
// version of this function got this wrong (it used toNumericLoose's
// isFloat flag to decide Int-vs-Float for TEXT/BLOB, and applied the
// integer-overflow check there too) -- fixed per this package's own
// scalar-function conformance pass; see compat-harness/
// vdbe_scalarfunc_test.go's abs() cases for the differential proof.
func fnAbs(v Value) (Value, error) {
	switch v.Typ {
	case Null:
		return v, nil
	case Int:
		if v.I == math.MinInt64 {
			return Value{}, fmt.Errorf("engine: integer overflow")
		}
		if v.I < 0 {
			return Value{Typ: Int, I: -v.I}, nil
		}
		return v, nil
	case Float:
		return Value{Typ: Float, F: math.Abs(v.F)}, nil
	default: // Text, Blob
		return Value{Typ: Float, F: math.Abs(valueToFloatLoose(v))}, nil
	}
}

func fnLength(v Value) Value {
	switch v.Typ {
	case Null:
		return v
	case Blob:
		return Value{Typ: Int, I: int64(len(v.S))}
	case Text:
		// Characters BEFORE the first embedded NUL only (textBeforeNUL), counted
		// by SQLite's OWN reader rather than Go's -- see sqliteUTF8Skip for the
		// oracle evidence that the two disagree on malformed input.
		s := v.S
		if i := bytes.IndexByte(s, 0); i >= 0 {
			s = s[:i]
		}
		n := 0
		for i := 0; i < len(s); n++ {
			i += sqliteUTF8Skip(s[i:])
		}
		return Value{Typ: Int, I: int64(n)}
	default:
		// Int/Float render without NULs, so no truncation is needed here.
		return Value{Typ: Int, I: int64(utf8.RuneCountInString(valueToText(v)))}
	}
}

// asciiFold upper/lower-cases only ASCII letters, matching SQLite's built-in
// (non-ICU) lower()/upper().
func asciiFold(s string, toLower bool) string {
	b := []byte(s)
	for i, c := range b {
		if toLower && c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		} else if !toLower && c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func typeName(v Value) string {
	switch v.Typ {
	case Null:
		return "null"
	case Int:
		return "integer"
	case Float:
		return "real"
	case Text:
		return "text"
	case Blob:
		return "blob"
	}
	return "null"
}

// fnHex matches SQLite's actual (slightly surprising) hex(): unlike almost
// every other scalar function, it does not propagate NULL. C SQLite's
// hexFunc calls sqlite3_value_blob/sqlite3_value_bytes directly, which for a
// NULL argument yield a NULL pointer and a zero length, producing the empty
// string rather than SQL NULL.
func fnHex(v Value) Value {
	var b []byte
	if v.Typ == Blob || v.Typ == Text {
		b = v.S
	} else {
		b = []byte(valueToText(v))
	}
	const digits = "0123456789ABCDEF"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i] = digits[c>>4]
		out[2*i+1] = digits[c&0xf]
	}
	return Value{Typ: Text, S: out}
}

func fnSubstr(args []Value) Value {
	x := args[0]
	if x.Typ == Null {
		return x
	}
	if args[1].Typ == Null {
		return Value{Typ: Null}
	}
	y := valueToInt64Trunc(args[1])
	hasZ := len(args) == 3
	var z int64
	if hasZ {
		if args[2].Typ == Null {
			return Value{Typ: Null}
		}
		z = valueToInt64Trunc(args[2])
	}
	if x.Typ == Blob {
		// A ZERO-LENGTH blob argument yields SQL NULL, not an empty blob --
		// C SQLite's substrFunc takes the blob branch and immediately
		// "if( z==0 ) return;" (leaving the result unset, i.e. NULL) because
		// sqlite3_value_blob() hands back a NULL POINTER for an empty blob.
		// Verified directly: typeof(substr(x'', 1, 1)), typeof(substr(x'', 1))
		// and typeof(substr(zeroblob(0), 1, 1)) are all 'null', whatever the
		// offset/length, while an empty TEXT argument (typeof(substr('',1,1))
		// = 'text') and any non-empty blob behave normally.
		if len(x.S) == 0 {
			return Value{Typ: Null}
		}
		start, count := substrRange(len(x.S), y, hasZ, z)
		return Value{Typ: Blob, S: append([]byte(nil), x.S[start:start+count]...)}
	}
	// The two character positions are found with SQLite's own reader, and the
	// RAW BYTES between them are returned -- never re-encoded. Going through
	// []rune substituted U+FFFD for every byte Go's decoder rejected, so
	// substr(cast(x'41ff42' as text),2,1) answered EF BF BD where C SQLite
	// answers the FF it was given (verified against 3.53.3). See sqliteUTF8Skip.
	s := []byte(textBeforeNUL(valueToText(x)))
	starts := sqliteUTF8Starts(s)
	start, count := substrRange(len(starts)-1, y, hasZ, z)
	return Value{Typ: Text, S: append([]byte(nil), s[starts[start]:starts[start+count]]...)}
}

// substrRange implements SQLite's substr(X,Y[,Z]) index algorithm (see
// func.c's substrFunc), returning a 0-based [start, start+count) byte/rune
// range into a sequence of the given length. Y is 1-based, possibly
// negative (counts from the end); Z, if given, is a length, possibly
// negative (grabs characters preceding position Y instead of following it).
func substrRange(length int, y int64, hasZ bool, z int64) (start, count int) {
	p1 := y
	// The two-argument form is not "unbounded": substrFunc gives p2 the
	// connection's SQLITE_LIMIT_LENGTH, and that finite value is load-bearing
	// for a hugely negative Y, because the underflow branch below subtracts
	// from it. substr('abcdefghij', -1e19) is '' in C SQLite -- p1 lands at
	// SMALLEST_INT64, p2 goes to 1e9 + p1 which is negative, and the p2<0
	// block collapses the range to nothing. Treating the length as unlimited
	// answered the WHOLE string.
	p2 := int64(sqliteMaxLength)
	if hasZ {
		p2 = z
	}
	if p1 < 0 {
		p1 += int64(length)
		if p1 < 0 {
			if p2 < 0 {
				p2 = 0
			} else {
				p2 += p1
			}
			p1 = 0
		}
	} else if p1 > 0 {
		p1--
	} else if p2 > 0 {
		p2--
	}
	if p2 < 0 {
		if p2 < -p1 {
			p2 = p1
		} else {
			p2 = -p2
		}
		p1 -= p2
	}
	// substrFunc's own "assert( p1>=0 && p2>=0 )" holds here. What follows is
	// its two output loops, which simply stop at the end of the value.
	if p1 > int64(length) {
		p1 = int64(length)
	}
	if p2 > int64(length)-p1 {
		p2 = int64(length) - p1
	}
	return int(p1), int(p2)
}
