package engine

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// ---- the "tuning" pragmas: synchronous, cache_size, journal_size_limit,
// mmap_size -- and default_cache_size, which is not a pragma at all ----
//
// These configure how C talks to the OS (fsync, page cache, journal size,
// mmap), which SQL cannot observe and this engine does not do. Setters are
// recorded and getters echo the stored value, which is the whole behavior.
// cache_spill is answered separately (pragmaCacheSpill): its getter is
// max(cache pages, spill threshold), where cache pages is a geometry for a
// negative cache_size.
//
// Each rule below was measured against the oracle.
//
// # Value parsing differs per pragma
//
//	cache_size, synchronous     sqlite3GetInt32: an optional sign, an optional
//	                            0x HEX form, then digits, STOPPING at the first
//	                            non-digit rather than rejecting it -- so "12abc"
//	                            is 12, "1e5" is 1, "1.9" is 1, "0x10" is 16.
//	                            Leading SPACE is not skipped ("  7" is 0), and
//	                            anything overflowing 32 bits is 0, not a clamp
//	                            ("2147483648" -> 0, while "-2147483648" is
//	                            itself).
//	journal_size_limit,         sqlite3DecOrHexToI64: the same digit-stopping
//	mmap_size                   parse widened to 64 bits, except that this one
//	                            DOES skip leading space ("  7" is 7) and
//	                            SATURATES on overflow ("99999999999999999999" is
//	                            9223372036854775807).
//
// The grammar's plus_num drops a leading "+" and a negative arrives as "-N"
// (minusFlag); ParsePragma does the same, so "+5" reaches synchronous as "5".
//
// # Clamping
//
//	synchronous          level = (v+1) & 7, with 0 becoming 1, reported as
//	                     level-1. So 0..5 pass through, but 8 reports 0 and 99
//	                     reports 3. A value whose first character is NOT a digit
//	                     is a name instead: off/no/false=0, on/yes/true=1,
//	                     full=2, extra=3, and anything unrecognized -- including
//	                     "-1" -- is 1.
//	cache_size           kept as parsed; a negative (kibibytes) is stored as is.
//	journal_size_limit   ANY negative normalizes to -1.
//	mmap_size            a negative becomes 0 (the compiled-in default), and the
//	                     value is capped at SQLITE_MAX_MMAP_SIZE.
//
// journal_size_limit and mmap_size setters echo one row; synchronous and
// cache_size setters return none.
//
// They are per database ("PRAGMA aux.synchronous=0" leaves main at 1). TEMP's
// synchronous always reports 0 and ignores its setter (pragma.c's "else if(
// iDb!=1 )").
//
// default_cache_size is compiled out of the oracle (-DSQLITE_OMIT_DEPRECATED)
// and behaves as an unknown pragma: no error, no columns, no rows.
//
// temp_store is not here: "temp_store=memory" makes execPragma record
// tempJournalModeUnmodelled (keeping "PRAGMA temp.journal_mode" from answering
// "delete" wrongly), a consequence this side would have to carry too.

// pragmaMaxMmapSize is SQLITE_MAX_MMAP_SIZE, the ceiling "PRAGMA mmap_size"
// clamps to. Measured: "PRAGMA mmap_size=999999999999" reports 2147418112.
const pragmaMaxMmapSize = 0x7FFF0000

// pragmaAbsentNames are pragmas the oracle lacks entirely, so they take the
// unknown-pragma path: no error, no columns, no rows, either form, any
// qualifier.
//
//	default_cache_size, legacy_file_format   compiled out by
//	                                         -DSQLITE_OMIT_DEPRECATED
//	                                         (mattn/go-sqlite3's own CFLAGS)
//	multiplex_*                              belong to an EXTENSION that is not
//	                                         loaded (the multiplexor VFS)
//	vdbe_*, parser_trace                     debug-build only
//	omit_readlock, data_store_directory      not built on this platform
//
// lock_proxy_file is absent only on Linux; on macOS it is real and answered by
// pragmaLockProxyFile (pragma_lock_proxy_file.go).
//
// This must stay a curated list. C ignores every unknown pragma, but a blanket
// rule here would turn the pragmas this engine declines on purpose into silent
// no-ops, desynchronizing the engines.
var pragmaAbsentNames = map[string]bool{
	"default_cache_size":   true,
	"legacy_file_format":   true,
	"data_store_directory": true,
	"omit_readlock":        true,
	"multiplex_enabled":    true,
	"multiplex_chunksize":  true,
	"multiplex_filecount":  true,
	"multiplex_truncate":   true,
	"vdbe_listing":         true,
	"vdbe_trace":           true,
	"vdbe_addoptrace":      true,
	"vdbe_debug":           true,
	"parser_trace":         true,
	"shrink_memory":        true,
	// pragma.test#30 uses it to drive error handling; the oracle has no such
	// pragma, so "PRAGMA error='This is the error message'" is silently
	// ignored rather than raising anything.
	"error": true,
}

// pragmaTuningNames is the family, and its membership is what makes a name
// answerable here at all.
var pragmaTuningNames = map[string]bool{
	"synchronous":        true,
	"cache_size":         true,
	"journal_size_limit": true,
	"mmap_size":          true,
}

// pragmaTuningDefault is what a database that never set the pragma reports,
// which depends on the database:
//
//	                    main   temp   ATTACHed
//	synchronous            1      0          2
//	cache_size         -2000      0      -2000
//	journal_size_limit    -1     -1         -1
//
// main's synchronous 1 is the oracle's -DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1;
// an attachment gets SQLITE_DEFAULT_SYNCHRONOUS 2. Build constants, pinned by
// the harness.
func pragmaTuningDefault(name string, isTemp, isAttached bool) int64 {
	switch name {
	case "synchronous":
		switch {
		case isTemp:
			return 0
		case isAttached:
			return 2
		}
		return 1
	case "cache_size":
		if isTemp {
			return 0
		}
		return -2000
	case "journal_size_limit":
		return -1
	}
	return 0 // mmap_size
}

// pragmaTuningEchoesValue is the two whose SETTER reports the new value back
// instead of returning an empty result set.
var pragmaTuningEchoesValue = map[string]bool{
	"journal_size_limit": true,
	"mmap_size":          true,
}

// pragmaTuningKey names one database's value. "" and "main" are the same
// database, so they must land on one key.
func pragmaTuningKey(schema, name string) string {
	s := r33sFoldIdent(strings.TrimSpace(schema))
	if s == "" {
		s = "main"
	}
	return s + "." + name
}

// PragmaConnState is the CONNECTION state these pragmas read and write. It is
// one struct rather than a bare map because the family is not all integers:
// temp_store_directory holds a path.
type PragmaConnState struct {
	// Values are the per-database tuning values, keyed "schema.name".
	Values map[string]int64

	// TempStoreDirectory is "PRAGMA temp_store_directory". Empty means unset,
	// which is what the getter reports as ZERO ROWS rather than as "".
	TempStoreDirectory string

	// SecureDelete holds "PRAGMA secure_delete" per database; SecureDeleteDefault
	// is what a database without its own entry reports, including one attached
	// later:
	//
	//	initial                        main=0 db2=0 temp=0   bare getter=0
	//	PRAGMA main.secure_delete=ON   main=1 db2=0 temp=0   bare getter=1
	//	PRAGMA secure_delete=ON        main=1 db2=1 temp=1   -- ALL of them
	//	PRAGMA db2.secure_delete=OFF   main=1 db2=0 temp=1
	//	ATTACH after a bare ON         the NEW database reports 1
	//
	// So a bare setter raises the default and clears overrides; a qualified one
	// sets one database; the bare getter reports main's. The setting is inert
	// here (rewrites start from zeroed images), so storing it is the behavior.
	SecureDelete        map[string]int
	SecureDeleteDefault int

	// CacheSpillOff is "PRAGMA cache_spill"'s CONNECTION-wide flag
	// (SQLITE_CacheSpill), stored inverted so the zero value is the real
	// default, ON. CacheSpillSize is each database's OWN spill threshold
	// (the pager's szSpill) where this engine knows it exactly, and
	// CacheSpillMax an UPPER BOUND on it where it does not. See
	// pragmaCacheSpill for the whole measured rule; the short version is that
	// the getter answers max(that database's cache pages, its szSpill), so an
	// upper bound is enough whenever it does not exceed the cache size.
	CacheSpillOff  bool
	CacheSpillSize map[string]int64
	CacheSpillMax  map[string]int64

	// LegacyAlterTable is "PRAGMA legacy_alter_table" (SQLITE_LegacyAlter), which
	// decides how much an "ALTER TABLE ... RENAME TO" rewrites:
	//
	//	                          legacy ON        legacy OFF
	//	the table's own row       "t1x"(a, b)      "t1x"(a, b)
	//	an INDEX on it            ON "t1x"(a)      ON "t1x"(a)
	//	a TRIGGER's ON clause     ON "t1x"         ON "t1x"
	//	that trigger's BODY       INSERT INTO t1   INSERT INTO "t1x"
	//	another table's REFERENCES REFERENCES t1   REFERENCES "t1x"
	//	a VIEW's body             FROM t1          FROM "t1x"
	//
	// ADD COLUMN, RENAME COLUMN and DROP COLUMN are unaffected. It is connection
	// state surviving BEGIN, ROLLBACK and DDL; a qualifier is ignored.
	LegacyAlterTable bool

	// AnalysisLimit is "PRAGMA analysis_limit" (db->nAnalysisLimit), the cap
	// on how many rows of each index ANALYZE walks. It is not inert -- it
	// changes what ANALYZE writes to sqlite_stat1 -- and the half that serves
	// it is computeStat1Rows' analysisLimitWalk (pragma_analysis_limit.go).
	AnalysisLimit int

	// WorkerThreads is "PRAGMA threads" (db->aLimit[SQLITE_LIMIT_WORKER_THREADS]),
	// per CONNECTION, and inert here by construction: this engine launches no
	// auxiliary threads for a statement, so the number is a ceiling on something
	// that never happens. Served rather than swallowed because the getter used to
	// answer no rows where the oracle answers one -- see pragmaThreads
	// (pragma_resource_limits.go), which also carries the clamp that makes
	// "threads=100" answer 8.
	WorkerThreads int

	// LockProxyOn / LockProxyPath are "PRAGMA lock_proxy_file"'s two pieces of
	// state, mirroring the pair proxyFileControl reads (os_unix.c:8231): whether
	// this database's file is PROXY-STYLE at all -- which is what decides
	// between "no row" and "a row" -- and, when it is, the explicit proxy path,
	// empty for the ":auto:" case whose getter is declined. macOS only; see
	// engine/pragma_lock_proxy_file.go for the whole measured state machine and
	// for what is NOT reproduced (proxy locking itself).
	LockProxyOn   bool
	LockProxyPath string

	// IgnoreCheckConstraints is "PRAGMA ignore_check_constraints"
	// (SQLITE_IgnoreChecks). It lives here because SnapshotPager carries this
	// struct to the read snapshot (where the getter and integrity_check are
	// answered) and the driver keeps it across its per-statement sessions. It
	// is per connection: "PRAGMA temp.ignore_check_constraints=OFF" clears a
	// bare ON, and it is settable inside a BEGIN and survives its ROLLBACK. See
	// DB.SetIgnoreCheckConstraints.
	IgnoreCheckConstraints bool
}

// SecureDeleteStateResult answers "PRAGMA [schema.]secure_delete [= <v>]" from
// per-database state. See PragmaConnState.SecureDelete for the measured rules.
func SecureDeleteStateResult(stmt *PragmaStmt, st *PragmaConnState) (cols []string, rows [][]Value, err error) {
	if st == nil {
		st = &PragmaConnState{}
	}
	schema := r33sFoldIdent(strings.TrimSpace(stmt.Schema))
	if schema == "" {
		schema = "main"
	}
	if stmt.HasValue {
		v := secureDeleteValue(stmt.ValueText)
		if stmt.Schema == "" {
			// Every database takes it, including any attached AFTERWARDS --
			// which is the default plus no overrides left standing.
			st.SecureDeleteDefault = v
			st.SecureDelete = nil
		} else {
			if st.SecureDelete == nil {
				st.SecureDelete = map[string]int{}
			}
			st.SecureDelete[schema] = v
		}
	}
	cur := st.SecureDeleteDefault
	if got, ok := st.SecureDelete[schema]; ok {
		cur = got
	}
	return []string{"secure_delete"}, [][]Value{{{Typ: Int, I: int64(cur)}}}, nil
}

// SecureDeleteMain is the value the connection's MAIN database carries, which
// is what the older single-value plumbing (DB.secureDelete, the pager's copy)
// still mirrors for any caller that has not moved to the state.
func SecureDeleteMain(st *PragmaConnState) int {
	if st == nil {
		return 0
	}
	if v, ok := st.SecureDelete["main"]; ok {
		return v
	}
	return st.SecureDeleteDefault
}

// pragmaTempStoreDirectory answers "PRAGMA temp_store_directory", the
// directory C puts temporary files in, unobservable through SQL, so tracking
// the string is the behavior:
//
//	PRAGMA temp_store_directory              cols=[temp_store_directory], and
//	                                         ZERO rows while unset; one row
//	                                         with the path once set
//	PRAGMA temp_store_directory='/tmp/'      accepted, no rows; the trailing
//	                                         slash is kept VERBATIM ("/tmp/"
//	                                         and "/tmp" read back as written)
//	PRAGMA temp_store_directory=''           clears it -- the getter is back to
//	                                         zero rows
//	PRAGMA temp_store_directory='/NON/EXISTENT/PATH/FOOBAR'
//	                                         ERROR "not a writable directory"
//	PRAGMA temp_store_directory=/tmp         "near "/": syntax error" -- the
//	                                         grammar's, and ParsePragma already
//	                                         rejects the bare form
//
// The setter's validation matters: pagerfault and pager1 set a nonexistent
// path for that error.
func pragmaTempStoreDirectory(stmt *PragmaStmt, st *PragmaConnState) ([]string, [][]Value, bool, error) {
	if !stmt.HasValue {
		if st.TempStoreDirectory == "" {
			return []string{stmt.Name}, [][]Value{}, true, nil
		}
		return []string{stmt.Name}, [][]Value{{{Typ: Text, S: []byte(st.TempStoreDirectory)}}}, true, nil
	}
	if err := pragmaValueSpellingSupported(stmt); err != nil {
		return nil, nil, true, err
	}
	dir := stmt.ValueText
	if dir != "" {
		if err := checkWritableDirectory(dir); err != nil {
			return nil, nil, true, err
		}
	}
	st.TempStoreDirectory = dir
	return []string{}, [][]Value{}, true, nil
}

// checkWritableDirectory is C SQLite's own guard on the setter, which calls
// access(zRight, W_OK|X_OK) and raises "not a writable directory" when it
// fails. Writability is PROBED rather than derived from the mode bits, which
// say nothing reliable about the calling user; the probe file is removed again
// and os.CreateTemp keeps the name unique, so a directory that really is
// writable is left exactly as it was found.
func checkWritableDirectory(dir string) error {
	errNotWritable := fmt.Errorf("engine: not a writable directory")
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return errNotWritable
	}
	f, err := os.CreateTemp(dir, ".musql-tempdir-probe-*")
	if err != nil {
		return errNotWritable
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

// pragmaCacheSpill answers "PRAGMA [db.]cache_spill [= <v>]".
//
//	GETTER   0 when the connection-wide flag is OFF; otherwise
//	         max(numberOfCachePages(db), db's szSpill) for the named database
//	         (bare: main). numberOfCachePages is cache_size when non-negative
//	         (0 is 0, pcache.c:280) and otherwise the geometry
//	         floor((-1024*cache_size)/(page_size + szExtra)): 483 at page_size
//	         4096, 1765 at 1024 (szExtra: cacheSpillPageOverhead).
//
//	SETTER   no rows; two independent effects (pragma.c):
//	           int size = 1;
//	           if( sqlite3GetInt32(zRight,&size) ) BtreeSetSpillSize(pBt,size);
//	           flag = sqlite3GetBoolean(zRight, size!=0);
//	         A non-numeric value leaves szSpill alone and only moves the flag,
//	         and "=bogus" turns it ON (size stays 1). =OFF -> 0; =ON -> the
//	         cache size; =100000 -> 100000; =25 -> unchanged at 2000 (the max
//	         keeps the cache pages); "=0" turns it off.
//
// A negative spill value is szSpill = floor((-1024*v)/(page_size + szExtra))
// (sqlite3PcacheSetSpillsize, pcache.c:872-885), computed at set time from
// this database's page size (PragmaTuningDB.PageSize).
//
// Without a page size for the named database (temp), the getter declines, and
// the setter uses the bound szSpill <= 2*|v| (smallest page 512) so a spill
// that cannot exceed the cache pages is still exact through the max. Anything
// not computable exactly is declined.
func pragmaCacheSpill(stmt *PragmaStmt, db PragmaTuningDB, st *PragmaConnState) ([]string, [][]Value, bool, error) {
	schema := r33sFoldIdent(strings.TrimSpace(stmt.Schema))
	if schema == "" {
		schema = "main"
	}

	if stmt.HasValue {
		if serr := pragmaValueSpellingSupported(stmt); serr != nil {
			return nil, nil, true, serr
		}
		text := strings.TrimSpace(stmt.ValueText)
		switch {
		case pragmaCacheSpillIsInteger(text):
			// A numeric value moves BOTH: sqlite3GetInt32 succeeds, so
			// szSpill takes it, and the flag takes getSafetyLevel's own
			// leading-digit branch -- which returns the value as a u8, so
			// "=256" turns the flag OFF where "=100000" leaves it on.
			n := pragmaGetInt32(text)
			switch {
			case n > 0:
				pragmaCacheSpillSet(st, schema, true, n, 0)
			case n < 0:
				// sqlite3PcacheSetSpillsize (pcache.c:872-885) computes this
				// ONCE, here, off THIS database's page size -- not deferred
				// to the getter -- so where that size is known it is stored
				// EXACT from here on, same as the n>0 case above.
				if pages, ok := numberOfCachePages(n, db.PageSize); ok {
					pragmaCacheSpillSet(st, schema, true, pages, 0)
				} else {
					pragmaCacheSpillSet(st, schema, false, 0, -2*n)
				}
			}
			if text[0] >= '0' && text[0] <= '9' {
				st.CacheSpillOff = byte(n) == 0
			} else {
				// No leading digit (a negative), so getSafetyLevel falls back
				// to its default argument, which is size!=0.
				st.CacheSpillOff = n == 0
			}
		default:
			on := pragmaGetBoolean(text, false)
			// A non-numeric value leaves szSpill untouched -- pragma.c only
			// calls BtreeSetSpillSize when sqlite3GetInt32 SUCCEEDS.
			st.CacheSpillOff = !on
		}
		return []string{}, [][]Value{}, true, nil
	}

	if st.CacheSpillOff {
		return []string{"cache_spill"}, [][]Value{{{Typ: Int, I: 0}}}, true, nil
	}
	// cache_spill's geometry uses the PCache's own szCache, which starts at
	// SQLITE_DEFAULT_CACHE_SIZE (-2000, sqliteLimit.h:161) for every Btree
	// (btree.c:2811). That differs from pragmaTuningDefault's cache_size,
	// which is 0 for temp until sqlite3InitOne syncs it (prepare.c:326-331):
	// a fresh connection's "PRAGMA temp.cache_size" is 0 while "PRAGMA
	// temp.cache_spill" is still 483 at page_size 4096.
	szCache := int64(-2000)
	if got, seen := st.Values[pragmaTuningKey(stmt.Schema, "cache_size")]; seen {
		szCache = got
	}
	pages, ok := numberOfCachePages(szCache, db.PageSize)
	if !ok {
		return nil, nil, true, fmt.Errorf("%w: PRAGMA %scache_spill while that database's cache_size is %d (a negative cache size makes C SQLite's answer a page-cache GEOMETRY this call site has no page size to compute -- set a positive cache_size, or turn cache_spill off, and it is exact)", errVDBEUnsupported, pragmaCacheSpillQualifier(stmt.Schema), szCache)
	}
	if s, ok := st.CacheSpillSize[schema]; ok {
		// s IS the database's szSpill, exact, set by an earlier setter --
		// max(pages, s), and s>pages was the only case still worth its own
		// branch (the other returns pages either way).
		if s > pages {
			return []string{"cache_spill"}, [][]Value{{{Typ: Int, I: s}}}, true, nil
		}
		return []string{"cache_spill"}, [][]Value{{{Typ: Int, I: pages}}}, true, nil
	}
	if u, ok := st.CacheSpillMax[schema]; ok {
		// The exact szSpill is unknown here (an earlier negative setter ran
		// where this call site had no page size), but it is bounded by u. If
		// u does not exceed pages, the true value cannot either, so the
		// max() is pages regardless of which value within the bound it is.
		if u > pages {
			return nil, nil, true, fmt.Errorf("%w: PRAGMA %scache_spill after a NEGATIVE spill value that may exceed the cache size (its exact threshold is floor((-1024*v)/(page_size+szExtra)), the same page-cache geometry the getter above declines here; this engine knows only that it is at most %d)", errVDBEUnsupported, pragmaCacheSpillQualifier(stmt.Schema), u)
		}
		return []string{"cache_spill"}, [][]Value{{{Typ: Int, I: pages}}}, true, nil
	}
	// Neither was ever set: szSpill is still sqlite3PcacheOpen's own default,
	// 1 (pcache.c:350), not 0 -- max(pages, 1), so a cache_size of exactly 0
	// (or a negative one whose geometry rounds down to 0) answers 1, not 0.
	if pages < 1 {
		pages = 1
	}
	return []string{"cache_spill"}, [][]Value{{{Typ: Int, I: pages}}}, true, nil
}

// cacheSpillPageOverhead is szExtra in numberOfCachePages and
// sqlite3PcacheSetSpillsize (pcache.c:277-289, 872-885): the bytes appended to
// each cached page, set at sqlite3PagerOpen (pager.c:5070) to sizeof(MemPage)
// (btree.c:2682). MemPage (btreeInt.h:273-303) has no #ifdef, so only word
// width decides it; both oracle builds agree.
//
// 136 is pinned: solving floor(1024*2000/(szPage+e)) against the getter at
// page sizes 512..65536 leaves exactly 136, which also reproduces pragma2-5.3
// (page_size 16384, cache_size 2, "PRAGMA cache_spill(-51)" -> 3).
const cacheSpillPageOverhead = 136

// numberOfCachePages is pcache.c:277-289's own function, verbatim: a
// non-negative szCache IS the page count (no geometry, no page size needed --
// szCache==0 answers 0 exactly, not the geometry this file used to treat it
// as), and a negative one is bytes-to-pages against pageSize and the fixed
// overhead above, clamped the same way pcache.c:287 clamps it. ok is false
// only when szCache<0 and pageSize is unknown (0) to the caller -- there is
// no page size to divide by, and that remains the one declined shape.
func numberOfCachePages(szCache int64, pageSize uint32) (n int64, ok bool) {
	if szCache >= 0 {
		return szCache, true
	}
	if pageSize == 0 {
		return 0, false
	}
	n = min((-1024*szCache)/(int64(pageSize)+cacheSpillPageOverhead),
		// pcache.c:287's own ceiling
		1000000000)
	return n, true
}

// pragmaCacheSpillQualifier renders a schema qualifier for a decline message.
func pragmaCacheSpillQualifier(schema string) string {
	if s := strings.TrimSpace(schema); s != "" {
		return r33sFoldIdent(s) + "."
	}
	return ""
}

// pragmaCacheSpillSet records one database's szSpill: EXACT (which a
// negative setter's own geometry can legitimately round all the way down to
// 0 -- sqlite3PcacheSetSpillsize assigns p->szSpill=mxPage unconditionally
// once its outer "if(mxPage)" on the RAW input passed, pcache.c:876-882, so
// exactKnown is a separate flag rather than inferred from exact>0) when this
// call site could compute it, an upper bound when only that is known. Either
// one REPLACES whatever was there.
func pragmaCacheSpillSet(st *PragmaConnState, schema string, exactKnown bool, exact, upper int64) {
	delete(st.CacheSpillSize, schema)
	delete(st.CacheSpillMax, schema)
	if exactKnown {
		if st.CacheSpillSize == nil {
			st.CacheSpillSize = map[string]int64{}
		}
		st.CacheSpillSize[schema] = exact
		return
	}
	if upper > 0 {
		if st.CacheSpillMax == nil {
			st.CacheSpillMax = map[string]int64{}
		}
		st.CacheSpillMax[schema] = upper
	}
}

// pragmaCacheSpillIsInteger reports whether text is a spelling sqlite3GetInt32
// would ACCEPT -- an optional sign then at least one digit, or the "0x" hex
// form. That is the bit pragma.c branches on, and pragmaGetInt32 alone cannot
// report it (it answers 0 for a failure and for a real zero alike).
func pragmaCacheSpillIsInteger(text string) bool {
	rest := text
	if len(rest) > 0 && (rest[0] == '-' || rest[0] == '+') {
		rest = rest[1:]
	} else if _, isHex := pragmaHex32(rest); isHex {
		return true
	}
	return len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9'
}

// PragmaTuningDB is what the CALLER knows about the database a tuning pragma
// names. Both flags change the answer rather than decorating it: Attached
// selects different defaults (see pragmaTuningDefault), and MemoryBacked is
// what makes mmap_size answer nothing at all.
type PragmaTuningDB struct {
	InTransaction bool
	Attached      bool
	MemoryBacked  bool

	// LockHeld is "this connection holds a lock on the database file now", C's
	// pFile->eFileLock != NO_LOCK. Only "PRAGMA lock_proxy_file" reads it, to
	// refuse a proxy-path change where switchLockProxyPath /
	// proxyTransformUnixFile answer SQLITE_BUSY (os_unix.c:8077, 8144).
	//
	// musql takes its locks per operation (lock.go), so this answers
	// conservatively: false means provably unlocked, true means assume locked;
	// over-reporting refuses a set C would take, which is the safe direction.
	// C refuses after:
	//
	//	BEGIN IMMEDIATE / BEGIN EXCLUSIVE     a write lock is taken at once
	//	BEGIN; <anything touching a page>     SELECT over a table, INSERT, CREATE
	//	                                      TABLE, PRAGMA user_version, PRAGMA
	//	                                      page_count -- the read/write txn
	//	                                      stays open, so SHARED stays held
	//	SAVEPOINT s; SELECT * FROM t          same, without an explicit BEGIN
	//	PRAGMA locking_mode=exclusive; SELECT the mode KEEPS the lock afterwards
	//
	// and accepts after a bare BEGIN or SAVEPOINT, "BEGIN; SELECT 1", a completed
	// COMMIT/ROLLBACK, autocommit statements, and "PRAGMA journal_mode=WAL" with
	// nothing read yet (the first read opens the WAL and takes SHARED).
	//
	// Callers answer "a transaction is open, or exclusive locking mode was
	// entered, or the database is in WAL", over-reporting four lock-free cases
	// (the in-transaction ones and WAL-with-nothing-read) and under-reporting
	// none.
	LockHeld bool

	// PageSize is the schema pragmaCacheSpill was called for's own on-disk
	// page size, when this call site can name it exactly -- 0 otherwise. Only
	// main and an ATTACHed database populate it (both are read straight off a
	// real file header); temp is left 0 deliberately, since musql does not
	// model a real page size for the temp catalog and a wrong page size here
	// would make cache_spill's geometry a wrong VALUE, not merely a gap. See
	// pragmaCacheSpill/numberOfCachePages.
	PageSize uint32
}

// PragmaTuningResult answers one tuning pragma out of vals, which a SETTER also
// updates in place. It is exported because the values are CONNECTION state and
// this engine's DB is per-statement in the driver: driver's Conn owns the
// map for the same reason it owns Conn.queryOnly.
//
// handled is false for any other pragma, leaving it to the normal path.
func PragmaTuningResult(stmt *PragmaStmt, db PragmaTuningDB, st *PragmaConnState) (cols []string, rows [][]Value, handled bool, err error) {
	if st == nil {
		st = &PragmaConnState{}
	}
	if stmt == nil {
		return nil, nil, false, nil
	}
	// BEFORE pragmaAbsentNames, because lock_proxy_file is absent on every
	// platform but darwin and pragmaLockProxyFile owns both halves of that
	// split -- see engine/pragma_lock_proxy_file.go.
	if stmt.Name == "lock_proxy_file" {
		return pragmaLockProxyFile(stmt, db, st)
	}
	if pragmaAbsentNames[stmt.Name] {
		return []string{}, [][]Value{}, true, nil
	}
	if stmt.Name == "temp_store_directory" {
		return pragmaTempStoreDirectory(stmt, st)
	}
	if stmt.Name == "cache_spill" {
		return pragmaCacheSpill(stmt, db, st)
	}
	// The db->flags bits, before the per-DATABASE machinery below: they are
	// per-CONNECTION, so their key ignores the schema qualifier entirely.
	if pragmaConnFlagTracked(stmt.Name) {
		return pragmaInertConnFlag(stmt, st)
	}
	if stmt.Name == "busy_timeout" {
		return pragmaBusyTimeout(stmt, st)
	}
	if stmt.Name == "analysis_limit" {
		return pragmaAnalysisLimit(stmt, st)
	}
	// The three RESOURCE limits, before the per-DATABASE machinery below for the
	// same reason the flag bits are: none of their arms looks at iDb in C, so a
	// schema qualifier is ignored rather than declined. See
	// pragma_resource_limits.go for what each one is and why one of the six forms
	// stays declined.
	switch stmt.Name {
	case "threads":
		return pragmaThreads(stmt, st)
	case "soft_heap_limit":
		return pragmaSoftHeapLimit(stmt)
	case "hard_heap_limit":
		return pragmaHardHeapLimit(stmt)
	}
	if !pragmaTuningNames[stmt.Name] {
		return nil, nil, false, nil
	}
	isTemp := equalFoldName(strings.TrimSpace(stmt.Schema), "temp")
	// mmap_size is the one that is not merely unused here but ABSENT: a
	// database with no file behind it -- temp, a ":memory:" main, an
	// ATTACH ':memory:' -- answers it with a named column and no rows, in
	// both directions. Verified across all three.
	if stmt.Name == "mmap_size" && (db.MemoryBacked || isTemp) {
		if stmt.HasValue {
			if serr := pragmaValueSpellingSupported(stmt); serr != nil {
				return nil, nil, true, serr
			}
		}
		return []string{stmt.Name}, [][]Value{}, true, nil
	}
	// The temp database's safety level is fixed at 0 and unsettable.
	tempSync := stmt.Name == "synchronous" && isTemp
	key := pragmaTuningKey(stmt.Schema, stmt.Name)

	if !stmt.HasValue {
		v := pragmaTuningDefault(stmt.Name, isTemp, db.Attached)
		switch {
		case tempSync:
			v = 0
		default:
			if got, seen := st.Values[key]; seen {
				v = got
			}
		}
		return []string{stmt.Name}, [][]Value{{{Typ: Int, I: v}}}, true, nil
	}

	// A setter runs the same two guards the no-op accept path runs, so
	// answering here can never accept a spelling or a placement the oracle
	// rejects -- which the corpus scores as WRONG, not as a coverage gap.
	if serr := pragmaValueSpellingSupported(stmt); serr != nil {
		return nil, nil, true, serr
	}
	if db.InTransaction {
		if terr := pragmaSetterInTransactionError(stmt.Name); terr != nil {
			return nil, nil, true, terr
		}
	}
	v := pragmaTuningNormalize(stmt.Name, stmt.ValueText)
	if tempSync {
		v = 0
	} else {
		if st.Values == nil {
			st.Values = map[string]int64{}
		}
		st.Values[key] = v
	}
	if pragmaTuningEchoesValue[stmt.Name] {
		return []string{stmt.Name}, [][]Value{{{Typ: Int, I: v}}}, true, nil
	}
	return []string{}, [][]Value{}, true, nil
}

// pragmaTuningHandles reports whether name is answered here at all.
func pragmaTuningHandles(name string) bool {
	// The three RESOURCE limits belong here for the reason the whole family does,
	// and leaving them out was a measured REGRESSION rather than a gap: they used
	// to be silent no-ops in execPragmaTuningNoop, and moving them to
	// pragma_resource_limits.go wired only the routes PragmaTuningResult is called
	// on DIRECTLY (driver's tuningPragma), while this gate keeps the ENGINE's
	// own exec and read paths out. The corpus runs engine-direct, so a whole-sweep
	// run came back unsupported=11 against main's 0 -- softheap1.test's eight
	// "PRAGMA soft_heap_limit" statements plus sort2/sort4's "PRAGMA threads",
	// every one of them a statement C runs and this engine refused.
	return pragmaAbsentNames[name] || pragmaTuningNames[name] ||
		pragmaConnFlagTracked(name) ||
		name == "temp_store_directory" || name == "cache_spill" ||
		name == "busy_timeout" || name == "lock_proxy_file" ||
		name == "threads" || name == "soft_heap_limit" || name == "hard_heap_limit"
}

// BusyTimeout is how long this connection waits on another connection's lock
// before SQLITE_BUSY: "PRAGMA busy_timeout" (or the driver's _busy_timeout), in
// milliseconds as sqlite3_busy_timeout takes it; the package default until one
// is set. Nil-safe.
func (st *PragmaConnState) BusyTimeout() time.Duration {
	if st != nil {
		if v, ok := st.Values["busy_timeout"]; ok {
			return time.Duration(v) * time.Millisecond
		}
	}
	return BusyTimeout
}

// busyTimeout is this session's connection's lock wait; see
// PragmaConnState.BusyTimeout.
func (db *DB) busyTimeout() time.Duration {
	if db == nil {
		return BusyTimeout
	}
	return db.pragmaState.BusyTimeout()
}

// pragmaBusyTimeout answers "PRAGMA busy_timeout [= N]" from connection state.
// pragma.c's arm (src/pragma.c:2651):
//
//		if( zRight ){ sqlite3_busy_timeout(db, sqlite3Atoi(zRight)); }
//		returnSingleInt(v, db->busyTimeout);
//
//	  - sqlite3Atoi is sqlite3GetInt32's parse (as cache_size);
//	  - sqlite3_busy_timeout (main.c:1855) stores ms only when ms>0, otherwise
//	    clears the handler and busyTimeout becomes 0;
//	  - the setter falls through to returnSingleInt, so it answers one row;
//	  - db->busyTimeout is per connection, so a qualifier is ignored;
//	  - the column is "timeout" (mkpragmatab.tcl:378, "COLS: timeout").
//
// Bare SQLite defaults to 0, but the oracle (mattn/go-sqlite3 v1.14.48) sets
// 5000 on every connection (sqlite3.go:1623). The default here is BusyTimeout
// (lock.go), the engine's actual lock wait, which is also 5s, so the two
// cannot drift apart silently. A set value is the wait this session's lock
// waits use (DB.busyTimeout); only holdLockingModeLock still uses the package
// default.
func pragmaBusyTimeout(stmt *PragmaStmt, st *PragmaConnState) (cols []string, rows [][]Value, handled bool, err error) {
	if stmt.HasValue {
		if serr := pragmaValueSpellingSupported(stmt); serr != nil {
			return nil, nil, true, serr
		}
		v := max(pragmaGetInt32(stmt.ValueText), 0)
		if st.Values == nil {
			st.Values = map[string]int64{}
		}
		st.Values[stmt.Name] = v
	}
	v, set := st.Values[stmt.Name]
	if !set {
		v = int64(BusyTimeout / time.Millisecond)
	}
	return []string{"timeout"}, [][]Value{{{Typ: Int, I: v}}}, true, nil
}

// ---- the inert PragTyp_FLAG pragmas ----

// pragmaInertConnFlagNames are db->flags bits (pragma.c's PragTyp_FLAG arm)
// this engine accepts and must report back. Each affects only fsync policy, a
// C-API callback convention, or shared-cache isolation, never results:
//
//	checkpoint_fullfsync  SQLITE_CkptFullFSync  F_FULLFSYNC on a WAL checkpoint,
//	                                            which this format's log has none
//	                                            of (fullfsync is ACTIVE: below)
//	empty_result_callbacks SQLITE_NullCallback  sqlite3_exec's zero-row callback,
//	                                            which no database/sql driver has
//	read_uncommitted      SQLITE_ReadUncommit   shared-cache isolation, and there
//	                                            is no shared cache here or in the
//	                                            oracle connection
//	cell_size_check       SQLITE_CellSizeCk     btreeInitPage's extra per-cell
//	                                            bounds check; this engine has no
//	                                            pages for it to check
//
// The getter answers one INTEGER row named after the pragma
// (compat-harness/tail_r31_pragma_survives_test.go). A flag that changes a
// later statement's answer belongs in pragmaActiveConnFlagNames instead.
var pragmaInertConnFlagNames = map[string]bool{
	"checkpoint_fullfsync":   true,
	"empty_result_callbacks": true,
	"read_uncommitted":       true,
	"cell_size_check":        true,
}

// pragmaActiveConnFlagNames are PragTyp_FLAG bits tracked like the inert ones
// but that change what later statements answer; each names where that effect
// is implemented:
//
//   - count_changes (SQLITE_CountRows): DML appends a ResultRow of its row
//     counter named after the verb (sqlite3CodeChangeCount: "rows inserted" |
//     "rows updated" | "rows deleted"). ExecArgs carries no rows, so the
//     driver produces it on the Query path (Conn.countChangesRow).
//
//   - reverse_unordered_selects (SQLITE_ReverseOrder): sqlite3WhereBegin ends
//     with "if( pWInfo->pOrderBy==0 && (db->flags & SQLITE_ReverseOrder)!=0 )
//     whereReverseScanOrder(pWInfo);" (where.c:7126), reversing every FROM
//     item's scan (where.c:6727). Served by the planner port
//     (wherePlanSingleTableIndexOrder / wherePlanSingleIndexKey,
//     wherePlanMultiTableOrder / whereReversedScanKey).
var pragmaActiveConnFlagNames = map[string]bool{
	"count_changes":             true,
	"reverse_unordered_selects": true,
	"fullfsync":                 true, // a commit's sync (Session.commitSync)
}

// pragmaConnFlagTracked reports whether name is one of the db->flags bits this
// file stores on the connection, of either kind.
func pragmaConnFlagTracked(name string) bool {
	return pragmaInertConnFlagNames[name] || pragmaActiveConnFlagNames[name]
}

// CountChanges reports "PRAGMA count_changes" for this connection. It is a
// method rather than a raw map read so the key spelling lives in one place --
// driver reads it to decide whether a DML answers its change-count row.
func (st *PragmaConnState) CountChanges() bool {
	return st != nil && st.Values["count_changes"] != 0
}

// FullFsync reports "PRAGMA fullfsync" -- db->flags & SQLITE_FullFSync, which
// makes a commit's sync F_FULLFSYNC on darwin (commitSync, fsyncFile).
func (st *PragmaConnState) FullFsync() bool {
	return st != nil && st.Values["fullfsync"] != 0
}

// ReverseUnorderedSelects reports "PRAGMA reverse_unordered_selects" for this
// connection -- db->flags & SQLITE_ReverseOrder. Same one-place-for-the-spelling
// reason as CountChanges above; the planner reads it through
// ReadOnlyPager.ReverseUnorderedSelects (pragma.go).
func (st *PragmaConnState) ReverseUnorderedSelects() bool {
	return st != nil && st.Values["reverse_unordered_selects"] != 0
}

// pragmaInertConnFlag answers one of those flags, both forms, from connection
// state, keyed on the bare name (PragTyp_FLAG never looks at iDb). The getter
// is "returnSingleInt(v, (db->flags & pPragma->iArg)!=0)", one INTEGER row
// named after the pragma; the setter returns nothing. A fresh connection
// answers 0. Values parse with pragmaGetBoolean, a port of sqlite3GetBoolean.
func pragmaInertConnFlag(stmt *PragmaStmt, st *PragmaConnState) (cols []string, rows [][]Value, handled bool, err error) {
	if !stmt.HasValue {
		return []string{stmt.Name}, [][]Value{{{Typ: Int, I: st.Values[stmt.Name]}}}, true, nil
	}
	if serr := pragmaValueSpellingSupported(stmt); serr != nil {
		return nil, nil, true, serr
	}
	on := pragmaGetBoolean(stmt.ValueText, false)
	if st.Values == nil {
		st.Values = map[string]int64{}
	}
	st.Values[stmt.Name] = boolToInt64(on)
	return []string{}, [][]Value{}, true, nil
}

// execTuningPragma answers a tuning pragma on the WRITE path, where the values
// live (DB.tuning) and where every PRAGMA the corpus replays is routed --
// tclIsQuery sends anything that is not SELECT-shaped to ExecArgs.
//
// It runs BEFORE execPragma's attachedPragmaScope switch, which would decline
// the qualified form of these names: that switch's pragmaScopeDeclined bucket
// meant "neither per-database nor per-connection HERE", and a per-database
// value for exactly these four is what this file adds.
func (db *DB) execTuningPragma(stmt *PragmaStmt) (bool, error) {
	if !pragmaTuningHandles(stmt.Name) {
		return false, nil
	}
	// A qualifier naming no database is still an error ("unknown database aux"),
	// which this must not answer past. Only an UNRECOGNIZED name is checked:
	// checkWriteSchemaQualifier refuses an attachment outright ("cannot write
	// into ATTACHed database"), which is the right answer for a statement that
	// writes into it and the wrong one for a pragma that merely names it.
	if schema := r33sFoldIdent(strings.TrimSpace(stmt.Schema)); schema != "" &&
		schema != "main" && schema != "temp" && db.attachedNamed(stmt.Schema) == nil {
		if err := db.checkWriteSchemaQualifier(stmt.Schema); err != nil {
			return true, err
		}
	}
	if db.pragmaState == nil {
		db.pragmaState = &PragmaConnState{}
	}
	_, _, handled, err := PragmaTuningResult(stmt, db.pragmaTuningDB(stmt.Schema), db.pragmaState)
	return handled, err
}

// pragmaTuningDB describes the database a qualifier names, which the DEFAULTS
// and mmap_size's very existence depend on.
func (db *DB) pragmaTuningDB(schema string) PragmaTuningDB {
	out := PragmaTuningDB{
		InTransaction: db.inTransaction(),
		// See PragmaTuningDB.LockHeld: the conservative "assume locked" half.
		// db.lockingMain is exclusive locking mode, which holdLockingModeLock
		// (lock.go) really does keep across statements here; db.segWAL is the
		// SHARED lock C SQLite holds on the file for as long as it is in WAL.
		LockHeld: db.inTransaction() || db.lockingMain || db.segWAL,
	}
	switch r33sFoldIdent(strings.TrimSpace(schema)) {
	case "", "main":
		out.MemoryBacked = db.inMemory
		out.PageSize = db.pageSize
	case "temp":
		out.MemoryBacked = true
	default:
		if a := db.attachedNamed(schema); a != nil {
			out.Attached, out.MemoryBacked = true, a.isMem
			if a.pager != nil {
				out.PageSize = a.pager.meta.pageSize
			}
		}
	}
	return out
}

// queryTuningPragma is the read side, answering a getter from the values
// SnapshotPager carried over. A setter is declined: a read snapshot cannot
// record it, and dropping it would make the next getter contradict it. A
// qualifier naming an attached database is declined too: attachedReaders is
// set only for a cross-database read, so this side cannot tell an attachment
// from a typo, and their defaults differ.
func (p *ReadOnlyPager) queryTuningPragma(stmt *PragmaStmt) ([]string, [][]Value, bool, error) {
	if !pragmaTuningHandles(stmt.Name) {
		return nil, nil, false, nil
	}
	schema := r33sFoldIdent(strings.TrimSpace(stmt.Schema))
	if schema != "" && schema != "main" && schema != "temp" {
		return nil, nil, false, nil
	}
	if stmt.HasValue {
		return nil, nil, true, fmt.Errorf("%w: PRAGMA %s=%s on the read side (a read snapshot cannot record connection state; run it through the write path)", errVDBEUnsupported, stmt.Name, stmt.ValueText)
	}
	tdb := PragmaTuningDB{MemoryBacked: schema == "temp" || p.inMemory}
	if schema == "" || schema == "main" {
		// p IS main's own pager here (an attached qualifier already returned
		// above), so its header is exactly this database's page size -- see
		// PragmaTuningDB.PageSize.
		tdb.PageSize = p.meta.pageSize
	}
	cols, rows, handled, err := PragmaTuningResult(stmt, tdb, p.pragmaState)
	return cols, rows, handled, err
}

// SetPragmaTuningValues copies a connection's per-database tuning values onto a
// pager opened from disk, for the driver's autocommit reads (Conn.pragmaState),
// which do not go through SnapshotPager. pragma_cache_size as a table-valued
// function (vtab_pragma.go) is answered from that read snapshot. The map is
// copied, not aliased, so later setters do not leak into this statement's view;
// p.pragmaState is updated in place to keep other stamped state
// (SetIgnoreCheckConstraints).
func (p *ReadOnlyPager) SetPragmaTuningValues(st *PragmaConnState) {
	if p == nil {
		return
	}
	// reverse_unordered_selects is read at compile time (scan direction,
	// where.c:7126), and planCache is keyed on SQL text, so a change must
	// drop the cache, as for SetAutomaticIndex: the driver reuses one warm
	// pager across autocommit reads, and two identical SELECTs around the
	// setter would share a plan (TestR33QReverseUnorderedPlanCache).
	was := p.pragmaState.ReverseUnorderedSelects()
	defer func() {
		if p.pragmaState.ReverseUnorderedSelects() != was {
			p.planCache = nil
			// ...and the SESSION's cache, which outlives this pager. Detaching the
			// pointer alone would leave every later statement replaying the plans
			// compiled under the old setting.
			if p.sharedPlans != nil {
				p.sharedPlans.m = nil
			}
		}
	}()
	if st == nil || len(st.Values) == 0 {
		if p.pragmaState != nil {
			p.pragmaState.Values = nil
		}
		return
	}
	if p.pragmaState != nil && pragmaTuningValuesEqual(p.pragmaState.Values, st.Values) {
		return // already current; the driver re-stamps on EVERY read hand-out
	}
	if p.pragmaState == nil {
		p.pragmaState = &PragmaConnState{}
	}
	vals := make(map[string]int64, len(st.Values))
	for k, v := range st.Values {
		vals[k] = v
	}
	p.pragmaState.Values = vals
}

// SetPragmaTuningValues is ReadOnlyPager.SetPragmaTuningValues for a write
// session: a driver answers the tuning pragmas from its own connection state
// (never reaching a session), and one of them -- reverse_unordered_selects --
// is read by the WRITE compiler too, for an UPDATE's one-pass scan order. Left
// unset, a driver connection's UPDATE walked forward under the pragma where C
// walks backward: "UPDATE t SET n = (SELECT sum(n) FROM t AS u WHERE u.id <=
// t.id)" over n=1..5 stored 1,3,7,15,31 where C stores 1,3,6,10,15.
// No write-plan cache needs keying on it: the one compile that reads it is an
// UPDATE with a SET subquery, whose Program carries a WritePager and so is
// never cached (cachedWriteProgram).
func (db *DB) SetPragmaTuningValues(st *PragmaConnState) {
	if db == nil {
		return
	}
	if db.pragmaState == nil {
		db.pragmaState = &PragmaConnState{}
	}
	if st == nil || len(st.Values) == 0 {
		db.pragmaState.Values = nil
		return
	}
	if pragmaTuningValuesEqual(db.pragmaState.Values, st.Values) {
		return
	}
	vals := make(map[string]int64, len(st.Values))
	for k, v := range st.Values {
		vals[k] = v
	}
	db.pragmaState.Values = vals
}

// pragmaTuningValuesEqual is the "nothing to copy" test SetPragmaTuningValues
// runs first, so re-stamping a warm read pager that already carries this
// connection's values costs a comparison rather than a map allocation on every
// single autocommit read.
func pragmaTuningValuesEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// pragmaTuningNormalize is the stored value for a setter, per the per-pragma
// parse and clamp rules at the top of this file.
func pragmaTuningNormalize(name, text string) int64 {
	switch name {
	case "synchronous":
		level := (pragmaSafetyLevel(text) + 1) & 7
		if level == 0 {
			level = 1
		}
		return level - 1
	case "cache_size":
		return pragmaGetInt32(text)
	case "journal_size_limit":
		v := max(pragmaDecOrHexToI64(text), -1)
		return v
	case "mmap_size":
		v := max(pragmaDecOrHexToI64(text), 0)
		if v > pragmaMaxMmapSize {
			v = pragmaMaxMmapSize
		}
		return v
	}
	return 0
}

// pragmaSafetyLevel is C SQLite's getSafetyLevel (pragma.c) as PRAGMA
// synchronous calls it -- "getSafetyLevel(zRight,0,1)" (pragma.c:1140): a
// value whose FIRST character is a digit is a 32-bit integer, and anything
// else is one of eight names, with an unrecognized one taking the default
// of 1.
func pragmaSafetyLevel(text string) int64 {
	return pragmaSafetyLevelDflt(text, false, 1)
}

// pragmaSafetyLevelDflt is the same function with its other two arguments:
// omitFull suppresses the FULL and EXTRA names (which is what
// sqlite3GetBoolean passes, pragma.c:98), and dflt is what an empty or
// unrecognized string answers.
func pragmaSafetyLevelDflt(text string, omitFull bool, dflt int64) int64 {
	if len(text) > 0 && text[0] >= '0' && text[0] <= '9' {
		// C casts to u8, which TRUNCATES: "=256" is 0, not 256.
		return int64(uint8(pragmaGetInt32(text)))
	}
	switch strings.ToLower(text) {
	case "off", "no", "false":
		return 0
	case "on", "yes", "true":
		return 1
	case "full":
		if omitFull {
			return dflt
		}
		return 2
	case "extra":
		if omitFull {
			return dflt
		}
		return 3
	}
	return dflt
}

// pragmaGetInt32 is sqlite3GetInt32 (util.c). The two properties that matter
// and that a strtol would get wrong: it STOPS at the first non-digit instead of
// failing, and it yields 0 -- not a clamp and not a wrap -- for anything that
// does not fit in 32 bits.
func pragmaGetInt32(z string) int64 {
	neg := false
	switch {
	case strings.HasPrefix(z, "-"):
		neg, z = true, z[1:]
	case strings.HasPrefix(z, "+"):
		z = z[1:]
	default:
		// The hex form is reachable only WITHOUT a sign, as in the original.
		if v, isHex := pragmaHex32(z); isHex {
			return v
		}
	}
	z = strings.TrimLeft(z, "0")
	var v int64
	i := 0
	for ; i < 11 && i < len(z) && z[i] >= '0' && z[i] <= '9'; i++ {
		v = v*10 + int64(z[i]-'0')
	}
	if i > 10 {
		return 0
	}
	// The bound is asymmetric by one: -2147483648 is representable.
	var borrow int64
	if neg {
		borrow = 1
	}
	if v-borrow > math.MaxInt32 {
		return 0
	}
	if neg {
		return -v
	}
	return v
}

// pragmaHex32 is sqlite3GetInt32's hex branch: "0x" plus at least one hex
// digit. A value with the sign bit set, or with more than 8 significant digits,
// FAILS -- and a failed sqlite3GetInt32 leaves sqlite3Atoi's result at 0.
func pragmaHex32(z string) (int64, bool) {
	if len(z) < 3 || z[0] != '0' || (z[1] != 'x' && z[1] != 'X') || !isHexDigitByte(z[2]) {
		return 0, false
	}
	rest := strings.TrimLeft(z[2:], "0")
	var u uint32
	i := 0
	for ; i < 8 && i < len(rest) && isHexDigitByte(rest[i]); i++ {
		u = u*16 + uint32(hexDigitValue(rest[i]))
	}
	if u&0x80000000 != 0 || (i < len(rest) && isHexDigitByte(rest[i])) {
		return 0, true
	}
	return int64(u), true
}

// pragmaDecOrHexToI64 is sqlite3DecOrHexToI64 (util.c), whose decimal side is
// sqlite3Atoi64: leading whitespace IS skipped here (unlike the 32-bit parse),
// and an overflow SATURATES rather than yielding 0.
func pragmaDecOrHexToI64(z string) int64 {
	if v, isHex := pragmaHex64(z); isHex {
		return v
	}
	i := 0
	for i < len(z) && isSpaceByte(z[i]) {
		i++
	}
	neg := false
	if i < len(z) && (z[i] == '-' || z[i] == '+') {
		neg = z[i] == '-'
		i++
	}
	for i < len(z) && z[i] == '0' {
		i++
	}
	var u uint64
	digits := 0
	overflow := false
	for ; i < len(z) && z[i] >= '0' && z[i] <= '9'; i++ {
		digits++
		if digits > 19 {
			overflow = true
			continue
		}
		u = u*10 + uint64(z[i]-'0')
	}
	if overflow || u > math.MaxInt64 {
		if neg {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	if neg {
		return -int64(u)
	}
	return int64(u)
}

// pragmaHex64 is the 64-bit hex branch, which unlike the 32-bit one does not
// require a hex digit to follow "0x" (an empty one is simply zero).
func pragmaHex64(z string) (int64, bool) {
	if len(z) < 2 || z[0] != '0' || (z[1] != 'x' && z[1] != 'X') {
		return 0, false
	}
	i := 2
	for i < len(z) && z[i] == '0' {
		i++
	}
	var u uint64
	for ; i < len(z) && isHexDigitByte(z[i]); i++ {
		u = u*16 + uint64(hexDigitValue(z[i]))
	}
	return int64(u), true
}

// hexDigitValue is sqlite3HexToInt for one byte already known to be a hex
// digit (isHexDigitByte, scalar_funcs.go).
func hexDigitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// lockProxyOn reports whether "PRAGMA lock_proxy_file" has turned proxy locking
// on for this connection, tolerating a nil state so every call site can ask
// without a guard of its own. It is false on every platform but darwin by
// construction: the pragma itself is gated there (pragma_lock_proxy_file.go).
func (st *PragmaConnState) lockProxyOn() bool {
	return st != nil && st.LockProxyOn
}
