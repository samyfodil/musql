// This file implements sqlite_compileoption_used() and
// sqlite_compileoption_get(), and records which arguments must keep declining.
//
// Both report the oracle's own build (compiler, -D flags), so nothing here can
// be derived from first principles, and the two oracle builds (plain and
// "-tags sqlite_fts5") differ by one option, ENABLE_FTS5. Any answer that
// depends on list membership would be wrong in one of them.
//
// Most calls do not depend on the list, though. sqlite3_compileoption_used is
// a prefix match (main.c:5200-5227):
//
//	if( sqlite3StrNICmp(zOptName, "SQLITE_", 7)==0 ) zOptName += 7;
//	n = sqlite3Strlen30(zOptName);
//	for(i=0; i<nOpt; i++){
//	  if( sqlite3StrNICmp(zOptName, azCompileOpt[i], n)==0
//	   && sqlite3IsIdChar((unsigned char)azCompileOpt[i][n])==0
//	  ){
//	    return 1;
//	  }
//	}
//	return 0;
//
// The terminator test is sqlite3IsIdChar (tokenize.c:190), so
// used('COMPILER=gcc') is 1: the next character of "COMPILER=gcc-12.4.0" is
// '-'.
//
// Two facts make the answered branches safe:
//
//   - 0 branch. O is NAME or NAME=VALUE with NAME all id chars, so the only
//     non-id terminators are '=' and the trailing NUL. A C 1 therefore means the
//     text up to the argument's first '=' is a real option name; a catalogue
//     miss can only coincide with a C 0.
//   - pinned branch. It compares whole text for equality where C accepts a
//     prefix ending at a non-id char. These agree when VALUE is all id chars;
//     every pinned value is "1" or empty, and
//     TestPinnedCompileOptionValuesAreIdCharsOnly keeps it that way.
//
// So used() splits three ways:
//
//   - no candidate -> 0, against sqliteCompileOptionCatalogue (every name
//     sqlite3azCompileOpt[] can emit, across every #ifdef).
//   - a pinned candidate -> compared against its pinned text (options the
//     oracle module always sets, whatever the host).
//   - anything else declines: COMPILER=, ATOMIC_INTRINSICS=, MUTEX_PTHREADS
//     depend on the host; ENABLE_FTS5 on the gate build.
//
// A NULL argument answers NULL.
//
// get(N) for an in-range N declines: list length and order are build-dependent,
// and index 0/1 on this host are ATOMIC_INTRINSICS and COMPILER, facts about the
// C compiler. Reporting musql's own list would not help, as get() is
// positional. The value is non-comparable between independently built
// implementations -- ctime-2.4 (test/ctime.test:199-205) checks only the error
// code -- and evalCompileOptionGet's decline says so in words
// compat-harness's tclIsOutOfScope keys on.
//
// # used(get(0)) is answerable
//
// ctime-2.3 asserts it is 1 (test/ctime.test:190-196), and it is on every
// build:
//
//  1. sqlite3azCompileOpt[] is never empty: its THREADSAFE entry is an
//     #if/#elif/#else chain with an unconditional last arm
//     (tool/mkctimec.tcl:388-396):
//
//     #if defined(SQLITE_THREADSAFE)
//     "THREADSAFE=" CTIMEOPT_VAL(SQLITE_THREADSAFE),
//     #elif defined(THREADSAFE)
//     "THREADSAFE=" CTIMEOPT_VAL(THREADSAFE),
//     #else
//     "THREADSAFE=1",
//     #endif
//
//     COMPILER's chain (mkctimec.tcl:359-369) has no #else, so nOpt >= 2 is not
//     provable, which is why only index 0 qualifies.
//
//  2. get(0) is therefore azCompileOpt[0] (main.c:5233-5240).
//
//  3. used(azCompileOpt[0]) is 1: at i==0 the strings match over n and the next
//     byte is NUL. The "SQLITE_" strip cannot interfere, since mkctimec.tcl
//     strips that prefix from every name (trim_name, mkctimec.tcl:398-404).
//
// # Out-of-range get(N) is answerable
//
//	const char *sqlite3_compileoption_get(int N){
//	  ...
//	  if( N>=0 && N<nOpt ){
//	    return azCompileOpt[N];
//	  }
//	  return 0;                       /* a NULL pointer -> SQL NULL */
//	}
//
// nOpt cannot exceed the 242 distinct names sqlite3azCompileOpt[] can emit (its
// 246 entries include #if chains for COMPILER and THREADSAFE that emit one
// each), which is len(sqliteCompileOptionCatalogue). So N<0 and N>=242 are
// NULL on every build.
package engine

import (
	"fmt"
	"strings"
)

// sqliteCompileOptionsPinned is the options mattn/go-sqlite3 sets on every
// build of itself, from its "#cgo CFLAGS":
//
//	-DSQLITE_ENABLE_RTREE -DSQLITE_THREADSAFE=1 -DSQLITE_ENABLE_FTS3
//	-DSQLITE_ENABLE_FTS3_PARENTHESIS -DSQLITE_OMIT_DEPRECATED
//	-DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1 -DSQLITE_ENABLE_UPDATE_DELETE_LIMIT
//
// (HAVE_USLEEP and SQLITE_TRACE_SIZE_LIMIT are set too but ctime.c does not
// report them.) Keyed by name; the value is the full text ctime.c emits.
var sqliteCompileOptionsPinned = map[string]string{
	"ENABLE_RTREE":               "ENABLE_RTREE",
	"ENABLE_FTS3":                "ENABLE_FTS3",
	"ENABLE_FTS3_PARENTHESIS":    "ENABLE_FTS3_PARENTHESIS",
	"ENABLE_UPDATE_DELETE_LIMIT": "ENABLE_UPDATE_DELETE_LIMIT",
	"OMIT_DEPRECATED":            "OMIT_DEPRECATED",
	"THREADSAFE":                 "THREADSAFE=1",
	"DEFAULT_WAL_SYNCHRONOUS":    "DEFAULT_WAL_SYNCHRONOUS=1",
}

// sqliteCompileOptionCatalogue is every option name sqlite3azCompileOpt[] can
// emit, across every #ifdef branch. It answers one question: could the
// argument name an option at all? "No" is 0 on any build. Extracted with:
//
//	awk '/static const char \* const sqlite3azCompileOpt/,/^};/' \
//	  sqlite3-binding.c | grep -oE '"[A-Z0-9_]+(=[^"]*)?"' |
//	  tr -d '"' | sed 's/=.*//' | sort -u
//
// ("MAX_MMAP_SIZE_" is not a typo; ctime.c reports both spellings.)
var sqliteCompileOptionCatalogue = map[string]bool{
	"32BIT_ROWID":                      true,
	"4_BYTE_ALIGNED_MALLOC":            true,
	"ALLOW_COVERING_INDEX_SCAN":        true,
	"ALLOW_ROWID_IN_VIEW":              true,
	"ALLOW_URI_AUTHORITY":              true,
	"ATOMIC_INTRINSICS":                true,
	"BITMASK_TYPE":                     true,
	"BUG_COMPATIBLE_20160819":          true,
	"BUG_COMPATIBLE_20250510":          true,
	"CASE_SENSITIVE_LIKE":              true,
	"CHECK_PAGES":                      true,
	"COMPILER":                         true,
	"COVERAGE_TEST":                    true,
	"DEBUG":                            true,
	"DEFAULT_AUTOMATIC_INDEX":          true,
	"DEFAULT_AUTOVACUUM":               true,
	"DEFAULT_CACHE_SIZE":               true,
	"DEFAULT_CKPTFULLFSYNC":            true,
	"DEFAULT_FILE_FORMAT":              true,
	"DEFAULT_FILE_PERMISSIONS":         true,
	"DEFAULT_FOREIGN_KEYS":             true,
	"DEFAULT_JOURNAL_SIZE_LIMIT":       true,
	"DEFAULT_LOCKING_MODE":             true,
	"DEFAULT_LOOKASIDE":                true,
	"DEFAULT_MEMSTATUS":                true,
	"DEFAULT_MMAP_SIZE":                true,
	"DEFAULT_PAGE_SIZE":                true,
	"DEFAULT_PCACHE_INITSZ":            true,
	"DEFAULT_PROXYDIR_PERMISSIONS":     true,
	"DEFAULT_RECURSIVE_TRIGGERS":       true,
	"DEFAULT_ROWEST":                   true,
	"DEFAULT_SECTOR_SIZE":              true,
	"DEFAULT_SYNCHRONOUS":              true,
	"DEFAULT_WAL_AUTOCHECKPOINT":       true,
	"DEFAULT_WAL_SYNCHRONOUS":          true,
	"DEFAULT_WORKER_THREADS":           true,
	"DIRECT_OVERFLOW_READ":             true,
	"DISABLE_DIRSYNC":                  true,
	"DISABLE_FTS3_UNICODE":             true,
	"DISABLE_FTS4_DEFERRED":            true,
	"DISABLE_INTRINSIC":                true,
	"DISABLE_LFS":                      true,
	"DISABLE_PAGECACHE_OVERFLOW_STATS": true,
	"DISABLE_SKIPAHEAD_DISTINCT":       true,
	"DQS":                              true,
	"ENABLE_8_3_NAMES":                 true,
	"ENABLE_API_ARMOR":                 true,
	"ENABLE_ATOMIC_WRITE":              true,
	"ENABLE_BATCH_ATOMIC_WRITE":        true,
	"ENABLE_BYTECODE_VTAB":             true,
	"ENABLE_CARRAY":                    true,
	"ENABLE_CEROD":                     true,
	"ENABLE_COLUMN_METADATA":           true,
	"ENABLE_COLUMN_USED_MASK":          true,
	"ENABLE_COSTMULT":                  true,
	"ENABLE_CURSOR_HINTS":              true,
	"ENABLE_DBPAGE_VTAB":               true,
	"ENABLE_DBSTAT_VTAB":               true,
	"ENABLE_EXPENSIVE_ASSERT":          true,
	"ENABLE_EXPLAIN_COMMENTS":          true,
	"ENABLE_FTS3":                      true,
	"ENABLE_FTS3_PARENTHESIS":          true,
	"ENABLE_FTS3_TOKENIZER":            true,
	"ENABLE_FTS4":                      true,
	"ENABLE_FTS5":                      true,
	"ENABLE_GEOPOLY":                   true,
	"ENABLE_HIDDEN_COLUMNS":            true,
	"ENABLE_ICU":                       true,
	"ENABLE_IOTRACE":                   true,
	"ENABLE_LOAD_EXTENSION":            true,
	"ENABLE_LOCKING_STYLE":             true,
	"ENABLE_MATH_FUNCTIONS":            true,
	"ENABLE_MEMORY_MANAGEMENT":         true,
	"ENABLE_MEMSYS3":                   true,
	"ENABLE_MEMSYS5":                   true,
	"ENABLE_MULTIPLEX":                 true,
	"ENABLE_NORMALIZE":                 true,
	"ENABLE_NULL_TRIM":                 true,
	"ENABLE_OFFSET_SQL_FUNC":           true,
	"ENABLE_ORDERED_SET_AGGREGATES":    true,
	"ENABLE_OVERSIZE_CELL_CHECK":       true,
	"ENABLE_PERCENTILE":                true,
	"ENABLE_PREUPDATE_HOOK":            true,
	"ENABLE_QPSG":                      true,
	"ENABLE_RBU":                       true,
	"ENABLE_RTREE":                     true,
	"ENABLE_SESSION":                   true,
	"ENABLE_SETLK_TIMEOUT":             true,
	"ENABLE_SNAPSHOT":                  true,
	"ENABLE_SORTER_REFERENCES":         true,
	"ENABLE_SQLLOG":                    true,
	"ENABLE_STAT4":                     true,
	"ENABLE_STMT_SCANSTATUS":           true,
	"ENABLE_STMTVTAB":                  true,
	"ENABLE_TREETRACE":                 true,
	"ENABLE_UNKNOWN_SQL_FUNCTION":      true,
	"ENABLE_UNLOCK_NOTIFY":             true,
	"ENABLE_UPDATE_DELETE_LIMIT":       true,
	"ENABLE_URI_00_ERROR":              true,
	"ENABLE_VFSTRACE":                  true,
	"ENABLE_WHERETRACE":                true,
	"ENABLE_ZIPVFS":                    true,
	"EXPLAIN_ESTIMATED_ROWS":           true,
	"EXTRA_AUTOEXT":                    true,
	"EXTRA_IFNULLROW":                  true,
	"EXTRA_INIT":                       true,
	"EXTRA_INIT_MUTEXED":               true,
	"EXTRA_SHUTDOWN":                   true,
	"FTS3_MAX_EXPR_DEPTH":              true,
	"FTS5_ENABLE_TEST_MI":              true,
	"FTS5_NO_WITHOUT_ROWID":            true,
	"HAVE_ISNAN":                       true,
	"HOMEGROWN_RECURSIVE_MUTEX":        true,
	"IGNORE_AFP_LOCK_ERRORS":           true,
	"IGNORE_FLOCK_LOCK_ERRORS":         true,
	"INLINE_MEMCPY":                    true,
	"INT64_TYPE":                       true,
	"INTEGRITY_CHECK_ERROR_MAX":        true,
	"LEGACY_JSON_VALID":                true,
	"LIKE_DOESNT_MATCH_BLOBS":          true,
	"LOCK_TRACE":                       true,
	"LOG_CACHE_SPILL":                  true,
	"MALLOC_SOFT_LIMIT":                true,
	"MAX_ATTACHED":                     true,
	"MAX_COLUMN":                       true,
	"MAX_COMPOUND_SELECT":              true,
	"MAX_DEFAULT_PAGE_SIZE":            true,
	"MAX_EXPR_DEPTH":                   true,
	"MAX_FUNCTION_ARG":                 true,
	"MAX_LENGTH":                       true,
	"MAX_LIKE_PATTERN_LENGTH":          true,
	"MAX_MEMORY":                       true,
	"MAX_MMAP_SIZE":                    true,
	"MAX_MMAP_SIZE_":                   true,
	"MAX_PAGE_COUNT":                   true,
	"MAX_PAGE_SIZE":                    true,
	"MAX_SCHEMA_RETRY":                 true,
	"MAX_SQL_LENGTH":                   true,
	"MAX_TRIGGER_DEPTH":                true,
	"MAX_VARIABLE_NUMBER":              true,
	"MAX_VDBE_OP":                      true,
	"MAX_WORKER_THREADS":               true,
	"MEMDEBUG":                         true,
	"MIXED_ENDIAN_64BIT_FLOAT":         true,
	"MMAP_READWRITE":                   true,
	"MUTEX_NOOP":                       true,
	"MUTEX_OMIT":                       true,
	"MUTEX_PTHREADS":                   true,
	"MUTEX_W32":                        true,
	"NEED_ERR_NAME":                    true,
	"NO_SYNC":                          true,
	"OMIT_ALTERTABLE":                  true,
	"OMIT_ANALYZE":                     true,
	"OMIT_ATTACH":                      true,
	"OMIT_AUTHORIZATION":               true,
	"OMIT_AUTOINCREMENT":               true,
	"OMIT_AUTOINIT":                    true,
	"OMIT_AUTOMATIC_INDEX":             true,
	"OMIT_AUTORESET":                   true,
	"OMIT_AUTOVACUUM":                  true,
	"OMIT_BETWEEN_OPTIMIZATION":        true,
	"OMIT_BLOB_LITERAL":                true,
	"OMIT_CAST":                        true,
	"OMIT_CHECK":                       true,
	"OMIT_COMPLETE":                    true,
	"OMIT_COMPOUND_SELECT":             true,
	"OMIT_CONFLICT_CLAUSE":             true,
	"OMIT_CTE":                         true,
	"OMIT_DATETIME_FUNCS":              true,
	"OMIT_DECLTYPE":                    true,
	"OMIT_DEPRECATED":                  true,
	"OMIT_DESERIALIZE":                 true,
	"OMIT_DISKIO":                      true,
	"OMIT_EXPLAIN":                     true,
	"OMIT_FLAG_PRAGMAS":                true,
	"OMIT_FLOATING_POINT":              true,
	"OMIT_FOREIGN_KEY":                 true,
	"OMIT_GET_TABLE":                   true,
	"OMIT_HEX_INTEGER":                 true,
	"OMIT_INCRBLOB":                    true,
	"OMIT_INTEGRITY_CHECK":             true,
	"OMIT_INTROSPECTION_PRAGMAS":       true,
	"OMIT_JSON":                        true,
	"OMIT_LIKE_OPTIMIZATION":           true,
	"OMIT_LOAD_EXTENSION":              true,
	"OMIT_LOCALTIME":                   true,
	"OMIT_LOOKASIDE":                   true,
	"OMIT_MEMORYDB":                    true,
	"OMIT_OR_OPTIMIZATION":             true,
	"OMIT_PAGER_PRAGMAS":               true,
	"OMIT_PARSER_TRACE":                true,
	"OMIT_POPEN":                       true,
	"OMIT_PRAGMA":                      true,
	"OMIT_PROGRESS_CALLBACK":           true,
	"OMIT_QUICKBALANCE":                true,
	"OMIT_REINDEX":                     true,
	"OMIT_SCHEMA_PRAGMAS":              true,
	"OMIT_SCHEMA_VERSION_PRAGMAS":      true,
	"OMIT_SEH":                         true,
	"OMIT_SHARED_CACHE":                true,
	"OMIT_SHUTDOWN_DIRECTORIES":        true,
	"OMIT_SUBQUERY":                    true,
	"OMIT_TCL_VARIABLE":                true,
	"OMIT_TEMPDB":                      true,
	"OMIT_TEST_CONTROL":                true,
	"OMIT_TRACE":                       true,
	"OMIT_TRIGGER":                     true,
	"OMIT_TRUNCATE_OPTIMIZATION":       true,
	"OMIT_UTF16":                       true,
	"OMIT_VACUUM":                      true,
	"OMIT_VIEW":                        true,
	"OMIT_VIRTUALTABLE":                true,
	"OMIT_WAL":                         true,
	"OMIT_WSD":                         true,
	"OMIT_XFER_OPT":                    true,
	"PERFORMANCE_TRACE":                true,
	"POWERSAFE_OVERWRITE":              true,
	"PREFER_PROXY_LOCKING":             true,
	"PROXY_DEBUG":                      true,
	"REVERSE_UNORDERED_SELECTS":        true,
	"RTREE_INT_ONLY":                   true,
	"SECURE_DELETE":                    true,
	"SMALL_STACK":                      true,
	"SORTER_PMASZ":                     true,
	"SOUNDEX":                          true,
	"STAT4_SAMPLES":                    true,
	"STMTJRNL_SPILL":                   true,
	"STRICT_SUBTYPE":                   true,
	"SUBSTR_COMPATIBILITY":             true,
	"SYSTEM_MALLOC":                    true,
	"TCL":                              true,
	"TEMP_STORE":                       true,
	"TEST":                             true,
	"THREADSAFE":                       true,
	"UNLINK_AFTER_CLOSE":               true,
	"UNTESTABLE":                       true,
	"USE_ALLOCA":                       true,
	"USE_FCNTL_TRACE":                  true,
	"USE_URI":                          true,
	"VDBE_COVERAGE":                    true,
	"WIN32_MALLOC":                     true,
	"ZERO_MALLOC":                      true,
}

// compileOptionUsed implements sqlite_compileoption_used(X) for an argument
// whose answer does not depend on the oracle's build. ok is false when it
// does, and the caller then DECLINES rather than guessing.
//
// The comparison is case-insensitive throughout, as sqlite3StrNICmp is, and
// the leading "SQLITE_" is stripped first exactly as ctime.c strips it.
func compileOptionUsed(arg string) (used bool, ok bool) {
	name := arg
	if len(name) >= 7 && strings.EqualFold(name[:7], "SQLITE_") {
		name = name[7:]
	}
	// Split the argument into the name it would match and, if it carries one,
	// the value it demands. "THREADSAFE" asks whether the option is present at
	// all; "THREADSAFE=1" asks for that exact text.
	base, want := name, ""
	hasWant := false
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		base, want, hasWant = name[:eq], name[eq+1:], true
	}
	// Not an option name in ANY build: 0, and no build could disagree. This is
	// the branch "", "0", "1.0" and OMIT_COMPILEOPTION_DIAGS take.
	//
	// A STRICT PREFIX of a real name is here too, and is a 0 on the oracle for
	// the same reason: with n < len(option), main.c:5221's terminator test reads
	// option[n], a letter, and sqlite3IsIdChar says yes -- so "ENABLE_FTS" does
	// not match "ENABLE_FTS3". Only whole names, and name=value spellings whose
	// name is real, can reach the map at all; see this file's doc comment for
	// why that is a proof and not an observation about these examples.
	if !sqliteCompileOptionCatalogue[compileOptionKey(base)] {
		return false, true
	}
	full, pinned := sqliteCompileOptionsPinned[compileOptionKey(base)]
	if !pinned {
		// A real option whose presence (or whose value) is the oracle's host's
		// or its build tags' to decide. ENABLE_FTS5 is the one this engine's
		// own gates disagree about; COMPILER= and ATOMIC_INTRINSICS= are the
		// host's.
		return false, false
	}
	if !hasWant {
		return true, true // present, and the bare name matches whatever value it carries
	}
	return strings.EqualFold(full, base+"="+want), true
}

// compileOptionKey normalizes an option name for the two maps above: upper
// case, since ctime.c's comparison is case-insensitive and every option it
// emits is upper case.
func compileOptionKey(s string) string { return strings.ToUpper(s) }

// evalCompileOptionUsed is sqlite_compileoption_used()'s value semantics: NULL
// in, NULL out; otherwise the argument's TEXT (an integer or real argument is
// converted, so sqlite_compileoption_used(0) asks about "0"), answered as 1/0
// or declined.
func evalCompileOptionUsed(v Value) (Value, error) {
	if v.Typ == Null {
		return Value{Typ: Null}, nil
	}
	arg := string(valueToText(v))
	used, ok := compileOptionUsed(arg)
	if !ok {
		return Value{}, fmt.Errorf("%w: sqlite_compileoption_used(%q) (that option's presence is the ORACLE BUILD's to decide -- see compile_options.go; only options mattn/go-sqlite3 pins for every build of itself, and names that are not compile options at all, are answerable here)", errVDBEUnsupported, arg)
	}
	if used {
		return Value{Typ: Int, I: 1}, nil
	}
	return Value{Typ: Int, I: 0}, nil
}

// evalCompileOptionGet is sqlite_compileoption_get(N) for the range no build
// can disagree about. N is converted as sqlite3_value_int does
// (vdbeapi.c:210-212): sqlite3VdbeIntValue's coercion (bitwiseIntOperand) then
// truncation to a C int. The truncation matters:
//
//	get(4294967296)           index 0 (low 32 bits are 0)
//	get(-9223372036854775808) index 0
//	get(9223372036854775807)  NULL   (low 32 bits are -1)
//	get('1e3')                index 1 -- TEXT stops at 'e'
//
// NULL is index 0 (vdbemem.c:641-657), so it declines like any in-range index.
func evalCompileOptionGet(v Value) (Value, error) {
	n := int32(bitwiseIntOperand(v))
	if n < 0 || int(n) >= len(sqliteCompileOptionCatalogue) {
		return Value{Typ: Null}, nil
	}
	return Value{}, fmt.Errorf("%w: sqlite_compileoption_get(%d) (that index names an entry of the ANSWERING BUILD's own option list, whose length and order belong to the machine and the flags that built it -- see compile_options.go; only an index no build can hold is answerable here, because this is a fact about the build rather than about the SQL and no two independently-built implementations can agree on it)", errVDBEUnsupported, n)
}

// compileOptionUsedOfGetIsOne reports whether x is
// sqlite_compileoption_used(sqlite_compileoption_get(0)), which is 1 on every
// build (see the file comment).
//
// The argument must be one constant token -- sqlite3ValueFromExpr's contract
// (vdbemem.c:1978) -- converted as evalCompileOptionGet does, so get(0),
// get(0.0), get('0') and get(NULL) all fold. Only index 0 is provably in range;
// get(1) declines because a THREADSAFE-only build answers NULL. Signed tokens
// ("get(-0)") are UnaryExprs here and are not folded; adding them would be a
// pure widening.
func compileOptionUsedOfGetIsOne(x FuncExpr) bool {
	if r33sFoldIdent(x.Name) != "sqlite_compileoption_used" || len(x.Args) != 1 {
		return false
	}
	inner, ok := x.Args[0].(FuncExpr)
	if !ok || r33sFoldIdent(inner.Name) != "sqlite_compileoption_get" || len(inner.Args) != 1 {
		return false
	}
	lit, ok := inner.Args[0].(LiteralExpr)
	if !ok {
		return false
	}
	return int32(bitwiseIntOperand(lit.Val)) == 0
}

// compileOptionDiagFuncName reports whether name (lower-cased) is one of the
// two compile-option diagnostics. C forbids both in index expressions,
// partial-index WHERE clauses and generated columns, and they reach those
// sites through supportedFuncs, so without this the DDL would be accepted.
//
// func.c:3294-3295 registers them with DFUNCTION -- no SQLITE_FUNC_CONSTANT
// (sqliteInt.h:2164-2166) -- and resolve.c:1220-1228 rejects that:
//
//	if( (pDef->funcFlags & SQLITE_FUNC_CONSTANT)==0 ){
//	  ...
//	  sqlite3ResolveNotValid(pParse, pNC, "non-deterministic functions",
//	                         NC_IdxExpr|NC_PartIdx|NC_GenCol, 0, pExpr);
//	}
//
// A CHECK constraint and a view body are deliberately exempt. conn_state.go's
// four (sqlite_version, changes, total_changes, last_insert_rowid) get the same
// effect by staying out of supportedFuncs; these two cannot, since they compile
// as ordinary scalar calls.
func compileOptionDiagFuncName(name string) bool {
	return name == "sqlite_compileoption_used" || name == "sqlite_compileoption_get"
}

// exprCallsCompileOptionDiag reports whether e contains a call to either
// compile-option diagnostic anywhere in its operand tree. Used by the
// GENERATED COLUMN gate, which -- unlike the index-expression one -- inspects
// the parsed expression rather than consulting a per-name predicate.
func exprCallsCompileOptionDiag(e Expr) bool {
	if x, ok := e.(FuncExpr); ok && (compileOptionDiagFuncName(r33sFoldIdent(x.Name)) || nonConstantExtFuncName(r33sFoldIdent(x.Name))) {
		return true
	}
	found := false
	walkExprOperands(e, func(sub Expr) {
		if !found && exprCallsCompileOptionDiag(sub) {
			found = true
		}
	})
	return found
}
