package engine

import (
	"fmt"
	"runtime"
	"strings"
)

// ---- "PRAGMA [schema.]lock_proxy_file": macOS proxy locking ----
//
// The pragma exists only on macOS: pragma.c wraps it in "#if
// SQLITE_ENABLE_LOCKING_STYLE" (pragma.c:1086-1121), which os_unix.c:66 sets
// for __APPLE__ only. Elsewhere it is an unknown pragma (no error, no rows).
// lockProxyFileImplemented mirrors that with runtime.GOOS, a constant.
//
// Proxy locking replaces byte-range locks on the database with locks on a local
// proxy file plus a "conch" file arbitrating which host owns it, for databases
// on network filesystems.
//
//	PRAGMA lock_proxy_file                   read the proxy path
//	PRAGMA lock_proxy_file = ":auto:"|<path> turn proxy locking on / switch it
//
// # Not reproduced
//
// musql does not implement proxy locking; it always takes OFD byte-range locks
// on the database file (internal/filelock, lock.go). This file reproduces the
// pragma's observable contract only: no conch or proxy file is created, and
// setting a path buys no extra safety.
//
// Without a conch the ":auto:" getter cannot be answered: proxyFileControl's
// GET arm (os_unix.c:8233-8246) takes the conch and reports either a generated
// path (proxyGetLockPath, os_unix.c:7432: confstr(_CS_DARWIN_USER_TEMP_DIR) +
// "sqliteplocks/" + the database path with '/' as '_' + ":auto:"), a path an
// earlier connection stored in the conch (os_unix.c:7844-7859, pragma.test
// 16.2.1), or ":auto: (not held)" when another process holds it (lock6.test
// 1.4). confstr is not $TMPDIR and needs libSystem, and the value depends on
// the database's own path and on files outside it, so that getter declines
// (TestPragmaLockProxyFileAutoPathIsNotDerivable). The decline is
// non-comparable rather than a gap: the differential harness gives the two
// engines different database files, so even exact implementations disagree.
//
// # The read form (pragma.c:1096-1103)
//
//	sqlite3OsFileControlHint(pFile, SQLITE_GET_LOCKPROXYFILE, &proxy_file_path);
//	returnSingleText(v, proxy_file_path);
//
// returnSingleText (pragma.c:225) emits no row for NULL, but the column is
// named regardless (setPragmaResultColumnNames, pragma.c:198). A file that is
// not proxy-style answers NULL (os_unix.c:8244): one column named
// "lock_proxy_file" (the pragma's own name; mkpragmatab.tcl has no COLS line
// for it), zero rows.
//
// # The set form (pragma.c:1104-1118 -> proxyFileControl, os_unix.c:8248)
//
// The value is taken verbatim as a path ("=1" stores "1"); an empty string is
// passed as NULL. Then:
//
//	arg NULL (i.e. "")   proxy-style  -> SQLITE_ERROR ("turn off proxy locking
//	                                    - not supported")
//	                     otherwise    -> SQLITE_OK, a pure no-op
//	a path               proxy-style  -> ":auto:" or a path EQUAL to the
//	                                    current one is SQLITE_OK and changes
//	                                    nothing, without consulting the lock.
//	                                    Any OTHER path goes to
//	                                    switchLockProxyPath.
//	                     otherwise    -> proxyTransformUnixFile: turn it on.
//
// Both of the last two start with
//
//	if( pFile->eFileLock!=NO_LOCK ){ return SQLITE_BUSY; }
//
// (os_unix.c:8082-8084 and 8150-8152), and any non-OK result is "failed to set
// lock proxy file" (pragma.c:1115). That is why this is implemented at all:
// lock6.test sets it inside "BEGIN; SELECT * FROM sqlite_master;", holding
// SHARED, and C refuses.
//
// # A qualifier picks the database; only main is modelled
//
// The state is per-database ("PRAGMA aux.lock_proxy_file=<p>" leaves main
// reporting nothing). Only main can become proxy-style here (the setter
// declines for an attachment), so zero rows is the true answer for others.
// "temp." is exact: its setter errors for every value, since the temp file has
// no methods until it opens and sqlite3OsFileControl answers SQLITE_NOTFOUND;
// the getter ignores that and answers one column, no rows.

// lockProxyFileImplemented mirrors C's SQLITE_ENABLE_LOCKING_STYLE build gate
// (os_unix.c:66: 1 for __APPLE__, 0 otherwise). runtime.GOOS is a compile-time
// constant, so this is a constant too and every non-darwin build folds the
// darwin arm out entirely.
const lockProxyFileImplemented = runtime.GOOS == "darwin"

// errLockProxyFileSet is pragma.c:1115's message, verbatim. It is a plain
// error rather than an errVDBEUnsupported decline because C SQLite RAISES
// here: this is an answer the two engines agree on, not a gap.
func errLockProxyFileSet() error {
	return fmt.Errorf("engine: failed to set lock proxy file")
}

// pragmaLockProxyFile answers "PRAGMA [schema.]lock_proxy_file [= <path>]".
// See this file's doc comment for the C citations and for what is NOT
// reproduced; db.LockHeld is the eFileLock!=NO_LOCK half and is documented on
// PragmaTuningDB.
func pragmaLockProxyFile(stmt *PragmaStmt, db PragmaTuningDB, st *PragmaConnState) (cols []string, rows [][]Value, handled bool, err error) {
	if !lockProxyFileImplemented {
		// No such pragma in this build of C SQLite: its unknown-pragma path,
		// which is no error, no columns and no rows in either direction. This
		// is the behavior lock_proxy_file had here before darwin was served,
		// and pragmaAbsentNames' doc comment records the reason.
		return []string{}, [][]Value{}, true, nil
	}
	const col = "lock_proxy_file"
	schema := r33sFoldIdent(strings.TrimSpace(stmt.Schema))
	isMain := schema == "" || schema == "main"

	if !stmt.HasValue {
		switch {
		case !isMain || !st.LockProxyOn:
			// Not proxy-style: proxyFileControl hands back NULL and
			// returnSingleText emits no row, but the column is still named.
			return []string{col}, [][]Value{}, true, nil
		case st.LockProxyPath == "":
			// The one declined form. "not reproducible against C SQLite" is the
			// phrase compat-harness's tclIsOutOfScope keys on, so it is booked as
			// non-comparable. C's value is a function of the database file's own
			// path (proxyGetLockPath, os_unix.c:7432, with confstr's per-user temp
			// dir, which needs libSystem), or of a path an earlier connection wrote
			// into <db>-conch (proxyTakeConch, os_unix.c:7844-7859), or ":auto: (not
			// held)" when another process holds the conch (os_unix.c:8241). Any of
			// these would be a guess.
			return nil, nil, true, fmt.Errorf("%w: PRAGMA lock_proxy_file after \":auto:\" reports a value that is not reproducible against C SQLite -- it is the conch protocol's own path (proxyGetLockPath, os_unix.c:7432: the per-uid darwin temp dir from confstr(_CS_DARWIN_USER_TEMP_DIR), then \"sqliteplocks/\", then THE DATABASE FILE'S OWN PATH with every '/' turned into '_', then \":auto:\"), or a path some earlier connection wrote into <db>-conch (proxyTakeConch, os_unix.c:7844-7859), or the literal \":auto: (not held)\" -- so it is a function of a file path and of a directory no CGo-free engine can read, and never of the SQL; see engine/pragma_lock_proxy_file.go", errVDBEUnsupported)
		default:
			return []string{col}, [][]Value{{{Typ: Text, S: []byte(st.LockProxyPath)}}}, true, nil
		}
	}

	// A setter runs the same spelling guard every other accepted setter runs,
	// so this can never accept a spelling C SQLite's grammar rejects -- the
	// corpus scores that as WRONG, not as a gap.
	if serr := pragmaValueSpellingSupported(stmt); serr != nil {
		return nil, nil, true, serr
	}
	path := stmt.ValueText
	// A memory-backed main is the temp case by another route:
	// sqlite3PagerFile returns a sqlite3_file with pMethods 0 for
	// ":memory:", and sqlite3OsFileControl returns SQLITE_NOTFOUND for that
	// (os.c:130), so pragma.c:1114's res!=SQLITE_OK fires and the setter
	// raises for every value. That also keeps st.LockProxyOn false, so the
	// getter keeps answering zero rows.
	if isMain && db.MemoryBacked {
		return nil, nil, true, errLockProxyFileSet()
	}
	if !isMain {
		if schema == "temp" {
			// Measured for a path AND for "": both raise, because the temp
			// pager's file has no methods and sqlite3OsFileControl answers
			// SQLITE_NOTFOUND.
			return nil, nil, true, errLockProxyFileSet()
		}
		// An attachment: C SQLite really does turn proxy locking on for it.
		// Declined rather than tracked, which is what keeps "zero rows" the
		// truthful getter answer for every schema but main.
		return nil, nil, true, fmt.Errorf("%w: PRAGMA %s.lock_proxy_file=%s (proxy locking is tracked for main only here -- see engine/pragma_lock_proxy_file.go)", errVDBEUnsupported, strings.ToLower(stmt.Schema), path)
	}

	switch {
	case path == "":
		// SQLITE_SET_LOCKPROXYFILE with a NULL argument: "turn off proxy
		// locking - not supported" once it is on, a no-op while it is off.
		// Note this arm never consults the lock (measured: it succeeds inside
		// a held read transaction).
		if st.LockProxyOn {
			return nil, nil, true, errLockProxyFileSet()
		}
	case st.LockProxyOn:
		if path == ":auto:" || path == st.LockProxyPath {
			break // proxyFileControl's own short-circuit, BEFORE any lock check
		}
		if db.LockHeld {
			return nil, nil, true, errLockProxyFileSet() // switchLockProxyPath's SQLITE_BUSY
		}
		st.LockProxyPath = path
	default:
		if db.LockHeld {
			return nil, nil, true, errLockProxyFileSet() // proxyTransformUnixFile's SQLITE_BUSY
		}
		// proxyTransformUnixFile normalizes ":auto:" (and "", unreachable
		// here) to a NULL lockProxyPath, which is the state whose getter this
		// engine declines.
		st.LockProxyOn = true
		if path != ":auto:" {
			st.LockProxyPath = path
		}
	}
	// PragFlg_NoColumns1 (mkpragmatab.tcl:222): a pragma given an argument
	// reports no columns and no rows at all.
	return []string{}, [][]Value{}, true, nil
}
