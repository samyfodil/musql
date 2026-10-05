package engine

import "fmt"

// This file serves three resource-limit pragmas: threads (per-connection),
// soft_heap_limit (process-global, advisory), and hard_heap_limit
// (process-global, enforced). Only the getter for hard_heap_limit is served.

// sqliteMaxWorkerThreads is the max worker threads limit.
// answers 8 rather than 100, and this engine has to clamp identically or the
// round-trip diverges. It is a build constant of the ORACLE, cited here rather
// than derived: musql launches no worker threads at all, so the number is a
// limit on something that never happens either way.
const sqliteMaxWorkerThreads = 8

// softHeapLimit is sqlite3_soft_heap_limit64's own storage: PROCESS-global, as
// in C, not per connection. That is observable -- setting it on one connection
// and reading it on another reports the value in C -- and reproducing it costs
// one package-level variable. It is advisory in the oracle's build (see this
// file's doc comment), so storing the number IS the whole behaviour.
var softHeapLimit int64

// pragmaThreads answers "PRAGMA threads [= N]" (pragma.c:2708-2717):
//
//	if( zRight && sqlite3DecOrHexToI64(zRight,&N)==SQLITE_OK && N>=0 ){
//	  sqlite3_limit(db, SQLITE_LIMIT_WORKER_THREADS, (int)(N&0x7fffffff));
//	}
//	returnSingleInt(v, sqlite3_limit(db, SQLITE_LIMIT_WORKER_THREADS, -1));
//
// A negative value, and one that does not parse, both leave the limit alone and
// are NOT errors ("PRAGMA threads=-1" and "PRAGMA threads='4abc'" each answer
// the unchanged 0). The setter reports the resulting limit, like every
// returnSingleInt arm and unlike the PragTyp_FLAG setters. The schema qualifier
// is ignored: the arm never looks at iDb.
func pragmaThreads(stmt *PragmaStmt, st *PragmaConnState) (cols []string, rows [][]Value, handled bool, err error) {
	if stmt.HasValue {
		if serr := pragmaValueSpellingSupported(stmt); serr != nil {
			return nil, nil, true, serr
		}
		if n, ok := pragmaDecOrHexToI64OK(stmt.ValueText); ok && n >= 0 {
			n &= 0x7fffffff
			if n > sqliteMaxWorkerThreads {
				n = sqliteMaxWorkerThreads
			}
			st.WorkerThreads = int(n)
		}
	}
	return []string{"threads"}, [][]Value{{{Typ: Int, I: int64(st.WorkerThreads)}}}, true, nil
}

// pragmaSoftHeapLimit answers "PRAGMA soft_heap_limit [= N]"
// (pragma.c:2671-2677):
//
//	if( zRight && sqlite3DecOrHexToI64(zRight, &N)==SQLITE_OK ){
//	  sqlite3_soft_heap_limit64(N);
//	}
//	returnSingleInt(v, sqlite3_soft_heap_limit64(-1));
//
// sqlite3_soft_heap_limit64 treats a NEGATIVE argument as a query, so a
// negative value leaves the limit alone (measured: "=-5" after "=1024" still
// answers 1024), while 0 deactivates it. There is no clamp against the hard
// limit to model here, because the hard limit's setter is declined and it is
// therefore always 0 -- which is the one case sqlite3_soft_heap_limit64 does no
// clamping for.
func pragmaSoftHeapLimit(stmt *PragmaStmt) (cols []string, rows [][]Value, handled bool, err error) {
	if stmt.HasValue {
		if serr := pragmaValueSpellingSupported(stmt); serr != nil {
			return nil, nil, true, serr
		}
		if n, ok := pragmaDecOrHexToI64OK(stmt.ValueText); ok && n >= 0 {
			softHeapLimit = n
		}
	}
	return []string{"soft_heap_limit"}, [][]Value{{{Typ: Int, I: softHeapLimit}}}, true, nil
}

// pragmaHardHeapLimit answers the GETTER of "PRAGMA hard_heap_limit" and
// declines the setter -- see this file's doc comment for the measurement behind
// that split. The value is 0 because nothing here can set it, which is exactly
// what a C connection whose process never set it reports.
func pragmaHardHeapLimit(stmt *PragmaStmt) (cols []string, rows [][]Value, handled bool, err error) {
	if stmt.HasValue {
		return nil, nil, true, fmt.Errorf("%w: PRAGMA hard_heap_limit = N, which C SQLite ENFORCES process-wide (every allocation past the limit fails with SQLITE_NOMEM -- measured: with it at 1024 the oracle refuses a later group_concat(hex(randomblob(2000))) that this engine answers). Accepting it would answer where C errors", errVDBEUnsupported)
	}
	return []string{"hard_heap_limit"}, [][]Value{{{Typ: Int, I: 0}}}, true, nil
}
