// The functions the oracle registers outside func.c's core set, or registers in
// func.c for their side effects: sqlite_source_id(), sqlite_log(),
// load_extension(); the r-tree node decoders rtreenode()/rtreedepth()
// (ext/rtree/rtree.c); fts3_tokenizer() (ext/fts3/fts3_tokenizer.c); fts5's
// three scalar helpers (ext/fts5/fts5_main.c); and the five authenticate/
// auth_* functions mattn/go-sqlite3 registers on every connection it opens.
//
// Each is a value-level body called by OpFunction, like every function in
// scalar_call.go.
package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// sqliteSourceID is SQLITE_SOURCE_ID of the 3.53.3 amalgamation the oracle
// links, which sqlite3_sourceid() returns (sqlite3.c's "return
// SQLITE_SOURCE_ID") and sourceidFunc reports (func.c:1019). Its hash is the
// manifest.uuid of the source tree this engine is checked against.
const sqliteSourceID = "2026-06-26 20:14:12 d4c0e51e4aeb96955b99185ab9cde75c339e2c29c3f3f12428d364a10d782c62"

// fts5SourceID is what fts5SourceIdFunc (fts5_main.c:3620) returns once the
// amalgamation build has replaced its "--FTS5-SOURCE-ID--" placeholder: the
// same id behind an "fts5: " prefix.
const fts5SourceID = "fts5: " + sqliteSourceID

// fts5InsttokenSubtype is FTS5_INSTTOKEN_SUBTYPE, fts5_main.c:96.
const fts5InsttokenSubtype = 73

// extFuncArity is the argument counts of this file's always-registered
// functions, from their sqlite3_create_function nArg (and, for rtreecheck's -1,
// the count rtreecheck itself enforces at run time).
func extFuncArity(name string) (lo, hi int, ok bool) {
	switch name {
	case "sqlite_source_id", "auth_enabled":
		return 0, 0, true
	case "sqlite_log", "authenticate", "rtreenode":
		return 2, 2, true
	case "load_extension", "fts3_tokenizer":
		return 1, 2, true
	case "auth_user_add", "auth_user_change":
		return 3, 3, true
	case "auth_user_delete", "rtreedepth":
		return 1, 1, true
	case "rtreecheck":
		return 0, -1, true
	}
	return 0, 0, false
}

// fts5ScalarFuncArity is the same for fts5's scalar functions, which exist
// only when fts5 is registered (sqlite3Fts5Init, fts5_main.c:3821-3845) -- see
// fts5LocaleFuncName for why that is a predicate and not a supportedFuncs
// entry.
func fts5ScalarFuncArity(name string) (lo, hi int, ok bool) {
	switch strings.ToLower(name) {
	case "fts5_locale":
		lo, hi = 2, 2
	case "fts5_source_id":
		lo, hi = 0, 0
	case "fts5", "fts5_insttoken":
		lo, hi = 1, 1
	default:
		return 0, 0, false
	}
	if _, registered := lookupVtabModule("fts5"); !registered {
		return 0, 0, false
	}
	return lo, hi, true
}

// fts5ScalarFuncName reports whether name is one of fts5's scalar functions in
// a process that has fts5 registered.
func fts5ScalarFuncName(name string) bool {
	_, _, ok := fts5ScalarFuncArity(name)
	return ok
}

// nonConstantExtFuncName reports whether name (lower-cased) is one of this
// file's always-registered functions that lacks SQLITE_FUNC_CONSTANT, which
// resolve.c:1220-1228 forbids in an index expression, a partial-index WHERE
// clause and a generated column ("non-deterministic functions prohibited in
// ..."), exactly as it forbids the compile-option diagnostics
// (compileOptionDiagFuncName): sqlite_source_id is a DFUNCTION (func.c:3349),
// load_extension an SFUNCTION (func.c:3290), and the rtree and fts3 functions
// are registered with no SQLITE_DETERMINISTIC (rtree.c:4326-4331,
// fts3_tokenizer.c:480). The authenticate/auth_* functions are registered
// deterministic (mattn/go-sqlite3's RegisterFunc with pure=true) and
// sqlite_log is a FUNCTION, so both stay allowed.
func nonConstantExtFuncName(name string) bool {
	switch name {
	case "sqlite_source_id", "load_extension", "rtreenode", "rtreedepth", "rtreecheck", "fts3_tokenizer":
		return true
	}
	return false
}

// callExtFunc dispatches this file's functions; ok is false for any other name.
func callExtFunc(name string, args []Value, enc TextEncoding) (v Value, ok bool, err error) {
	switch name {
	case "sqlite_source_id":
		return Value{Typ: Text, S: []byte(sqliteSourceID)}, true, nil
	case "sqlite_log":
		// errlogFunc (func.c:1035) hands its arguments to sqlite3_log() and
		// sets no result.
		return Value{}, true, nil
	case "load_extension":
		// loadExt (func.c:1824): a connection that has not enabled extension
		// loading refuses, and mattn/go-sqlite3 enables it only for the length
		// of its own LoadExtension call.
		return Value{}, true, fmt.Errorf("engine: not authorized")
	case "authenticate", "auth_user_add", "auth_user_change", "auth_user_delete", "auth_enabled":
		v, err = mattnAuthFunc(name, args)
		return v, true, err
	case "rtreedepth":
		v, err = rtreeDepthFunc(args[0])
		return v, true, err
	case "rtreenode":
		v, err = rtreeNodeFunc(args[0], args[1])
		return v, true, err
	case "rtreecheck":
		// rtreecheck (rtree.c:4282) reads the shadow tables, so it runs as
		// OpFunction's own arm, which has the statement's database in reach
		// (rtree_check.go). Any other evaluation has none: only the run-time
		// arity check (rtree.c:4287) is answerable there.
		if len(args) != 1 && len(args) != 2 {
			return Value{}, true, fmt.Errorf("engine: wrong number of arguments to function rtreecheck()")
		}
		return Value{}, true, fmt.Errorf("%w: rtreecheck() with no database to read", errVDBEUnsupported)
	case "fts3_tokenizer":
		v, err = fts3TokenizerFunc(args, enc)
		return v, true, err
	case "fts5_source_id":
		return Value{Typ: Text, S: []byte(fts5SourceID)}, true, nil
	case "fts5":
		// fts5Fts5Func (fts5_main.c:3604) writes the fts5_api pointer through
		// an argument bound with sqlite3_bind_pointer(..., "fts5_api_ptr"),
		// which no SQL value is; otherwise it sets no result.
		return Value{}, true, nil
	case "fts5_insttoken":
		// fts5InsttokenFunc (fts5_main.c:3694): the argument, retagged.
		v = args[0]
		v.Subtype = fts5InsttokenSubtype
		return v, true, nil
	}
	return Value{}, false, nil
}

// mattnAuthFunc is the five functions mattn/go-sqlite3 registers in its
// connection setup (sqlite3.go) over its sqlite3_opt_userauth_omit.go
// stubs, the build the oracle uses: every one returns 0. What is observable is
// the driver's argument conversion (callback.go): a Go string parameter accepts
// only TEXT or BLOB ("argument must be BLOB or TEXT") and an int parameter only
// INTEGER ("argument must be an INTEGER").
func mattnAuthFunc(name string, args []Value) (Value, error) {
	str := func(v Value) error {
		if v.Typ != Text && v.Typ != Blob {
			return fmt.Errorf("engine: argument must be BLOB or TEXT")
		}
		return nil
	}
	integer := func(v Value) error {
		if v.Typ != Int {
			return fmt.Errorf("engine: argument must be an INTEGER")
		}
		return nil
	}
	var checks []func(Value) error
	switch name {
	case "authenticate":
		checks = []func(Value) error{str, str}
	case "auth_user_add", "auth_user_change":
		checks = []func(Value) error{str, str, integer}
	case "auth_user_delete":
		checks = []func(Value) error{str}
	}
	for i, check := range checks {
		if err := check(args[i]); err != nil {
			return Value{}, err
		}
	}
	return Value{Typ: Int, I: 0}, nil
}

// rtreeDepthFunc is rtreedepth, rtree.c:3818: the big-endian 16-bit depth at
// the front of an r-tree node blob.
func rtreeDepthFunc(v Value) (Value, error) {
	if v.Typ != Blob || len(v.S) < 2 {
		return Value{}, fmt.Errorf("engine: Invalid argument to rtreedepth()")
	}
	return Value{Typ: Int, I: int64(binary.BigEndian.Uint16(v.S))}, nil
}

// rtreeNodeFunc is rtreenode, rtree.c:3766: a node blob's cells rendered as
// "{rowid c1 c2 ...}", space-separated, for a tree of nDim dimensions.
func rtreeNodeFunc(dim, node Value) (Value, error) {
	// tree.nDim = (u8)sqlite3_value_int(apArg[0])
	nDim := int(uint8(int32(sqliteValueInt64(dim))))
	if nDim < 1 || nDim > rtreeMaxDimensions {
		return Value{}, nil
	}
	nBytesPerCell := 8 + 8*nDim
	// sqlite3_value_blob: a NULL, or a zero-length TEXT/BLOB, is a null
	// pointer; a number is its text rendering.
	var zData []byte
	switch node.Typ {
	case Null:
		return Value{}, nil
	case Text, Blob:
		zData = node.S
	default:
		zData = []byte(valueToText(node))
	}
	if len(zData) < 4 {
		return Value{}, nil
	}
	nCell := int(binary.BigEndian.Uint16(zData[2:]))
	if len(zData) < 4+nCell*nBytesPerCell {
		return Value{}, nil
	}
	if nCell == 0 {
		// sqlite3_str_finish of an accumulator nothing was appended to is a
		// null pointer, so the result is NULL.
		return Value{}, nil
	}
	var sb strings.Builder
	for ii := 0; ii < nCell; ii++ {
		if ii > 0 {
			sb.WriteByte(' ')
		}
		cell := zData[4+nBytesPerCell*ii:]
		fmt.Fprintf(&sb, "{%d", int64(binary.BigEndian.Uint64(cell)))
		for jj := 0; jj < 2*nDim; jj++ {
			coord := math.Float32frombits(binary.BigEndian.Uint32(cell[8+4*jj:]))
			// sqlite3_str_appendf(pOut, " %g", (double)cell.aCoord[jj].f)
			g, err := fnPrintf("%g", []Value{{Typ: Float, F: float64(coord)}})
			if err != nil {
				return Value{}, err
			}
			sb.WriteByte(' ')
			sb.Write(g.S)
		}
		sb.WriteByte('}')
	}
	return Value{Typ: Text, S: []byte(sb.String())}, nil
}

// sqliteValueInt64 is sqlite3VdbeIntValue (vdbemem.c:641): an INTEGER as is, a
// REAL through sqlite3RealToI64, and TEXT/BLOB through sqlite3Atoi64 alone --
// an integer prefix, never a real one.
func sqliteValueInt64(v Value) int64 {
	switch v.Typ {
	case Int:
		return v.I
	case Float:
		return floatToInt64Saturating(v.F)
	case Text, Blob:
		n, _ := sqliteAtoi64(v.S, len(v.S))
		return n
	}
	return 0
}

// sqliteValueDouble is sqlite3VdbeRealValue (vdbemem.c:770): a REAL as is, an
// INTEGER widened, and TEXT/BLOB through sqlite3AtoF (sqlite3MemRealValueRC,
// vdbemem.c:735) -- whatever numeric prefix it finds, 0.0 for none. NULL is
// 0.0.
func sqliteValueDouble(v Value) float64 {
	switch v.Typ {
	case Float:
		return v.F
	case Int:
		return float64(v.I)
	case Text, Blob:
		r, _ := sqliteAtoF(v.S)
		return r
	}
	return 0
}

// fts3BuiltinTokenizers is the hash sqlite3Fts3Init fills (fts3.c:4157-4163)
// in a build without ICU: the names fts3_tokenizer() can find.
var fts3BuiltinTokenizers = map[string]bool{"simple": true, "porter": true, "unicode61": true}

// fts3TokenizerFunc is fts3TokenizerFunc, fts3_tokenizer.c:64, for the only
// forms an SQL literal can reach: the two-argument form is refused unless
// SQLITE_DBCONFIG_ENABLE_FTS3_TOKENIZER is on, which it never is here, and the
// one-argument form's result -- the tokenizer module's ADDRESS as an 8-byte
// blob -- is returned only under that same config or for a BOUND argument
// (sqlite3_value_frombind). compileFunc declines any call with a bound
// parameter in its arguments, so what reaches here returns NULL for a known
// name and "unknown tokenizer" for any other.
func fts3TokenizerFunc(args []Value, enc TextEncoding) (Value, error) {
	if len(args) == 2 {
		return Value{}, fmt.Errorf("engine: fts3tokenize disabled")
	}
	ctx := &jsonCtx{enc: enc}
	zName := ctx.valueText(args[0])
	if zName == nil {
		return Value{}, fmt.Errorf("engine: unknown tokenizer: (null)")
	}
	// The hash key is the text plus its terminator (nName = bytes+1), compared
	// with strncmp over that length, so an embedded NUL never matches.
	if !fts3BuiltinTokenizers[string(zName)] {
		return Value{}, fmt.Errorf("engine: unknown tokenizer: %s", jsonCString(zName))
	}
	return Value{}, nil
}

// exprCallsUnsafeSchemaFunc reports whether e calls a function a schema object
// may not use once its expressions are DDL-origin (unsafeSchemaFunc,
// trusted_schema.go). A CHECK constraint resolves under NC_FromDDL for every
// non-TEMP table (sqlite3ResolveSelfReference, resolve.c:2326-2328), so such a
// call is refused at CREATE TABLE by sqlite3ExprFunctionUsable (expr.c:1276)
// when the function is DIRECTONLY or the schema untrusted, and a DEFAULT's
// function nodes carry EP_FromDDL once the schema is reloaded
// (sqlite3ExprIsConstantOrFunction with isInit, expr.c:2560) and are refused
// when the INSERT codes them (expr.c:5392). The CHECK and DEFAULT sites here
// know neither the catalog nor the connection's trust setting, so they decline
// such an expression instead.
func exprCallsUnsafeSchemaFunc(e Expr) (string, bool) {
	if x, ok := e.(FuncExpr); ok && unsafeSchemaFunc(x.Name) {
		return r33sFoldIdent(x.Name), true
	}
	name, found := "", false
	walkExprOperands(e, func(sub Expr) {
		if !found && sub != nil {
			name, found = exprCallsUnsafeSchemaFunc(sub)
		}
	})
	return name, found
}

// fts3TokenizerCallDeclined reports whether a fts3_tokenizer() call must be
// declined at compile time: any bound parameter among its arguments may be the
// sqlite3_value_frombind() value that makes the oracle return a pointer or
// register a tokenizer.
func fts3TokenizerCallDeclined(x FuncExpr) bool {
	if !strings.EqualFold(x.Name, "fts3_tokenizer") {
		return false
	}
	for _, a := range x.Args {
		if exprHasParam(a) {
			return true
		}
	}
	return false
}
