// Configuration commands for fts5: "INSERT INTO t(t, rank) VALUES('<key>', <value>)"
// writes to %_config shadow table. Some commands schedule merges ('automerge',
// 'usermerge', 'crisismerge', 'deletemerge'); 'hashsize' sizes the pending write
// buffer. This engine re-encodes the whole index after every mutation, so most
// commands are accepted as no-ops except for 'pgsz' (fts5_index.go) and
// 'secure-delete' (fts5NoteRowsRemoved).
//
// Value must be an INTEGER or TEXT integer literal (no REAL, BLOB, NULL).
// Range checks are done against the 32-bit truncation of the value.
package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// fts5ConfigCommands maps each configuration command to its [lo, hi] bounds
// for the 32-bit truncation of the value.
var fts5ConfigCommands = map[string][2]int32{
	"pgsz":          {32, 64 * 1024},                // 31 and 65537 are errors
	"hashsize":      {1, math.MaxInt32},             // 0 and -1 are errors
	"automerge":     {0, 64},                        // -1 and 65 are errors
	"usermerge":     {2, 16},                        // 1 and 17 are errors
	"crisismerge":   {0, math.MaxInt32},             // -1 is an error, 1000000 is not
	"deletemerge":   {math.MinInt32, math.MaxInt32}, // any integer at all
	"secure-delete": {0, math.MaxInt32},             // -1 is an error, 2 and 100 are not
	"insttoken":     {0, math.MaxInt32},             // -1 is an error, 2 is not
}

// fts5SecureDeleteVersion is what %_config's "version" row becomes once a row
// has been removed while 'secure-delete' was on -- see fts5NoteRowsRemoved.
const fts5SecureDeleteVersion = 5

// fts5ConfigInt applies fts5's value rule to v: the value must be an INTEGER
// or TEXT integer literal. REAL and BLOB are rejected, as is TEXT that parses
// as float or overflows int64.
func fts5ConfigInt(v Value) (int64, bool) {
	switch v.Typ {
	case Int:
		return v.I, true
	case Text:
		if isFloat, i, _, ok := parseFullNumeric(string(v.S)); ok && !isFloat {
			return i, true
		}
	}
	return 0, false
}

// fts5SetConfig applies one configuration command to vm, writing (or
// replacing) its %_config row. key is the command's VERBATIM spelling: fts5
// dispatches on it case-insensitively but stores it as written, so
// "INSERT INTO t(t, rank) VALUES('PGSZ', 64)" really does leave a row keyed
// 'PGSZ' (verified, and it still takes effect).
func (db *DB) fts5SetConfig(vm *vtabMeta, key string, val Value) error {
	rng, ok := fts5ConfigCommands[strings.ToLower(key)]
	if !ok {
		return fmt.Errorf("engine: internal error: fts5: %q is not a configuration command", key)
	}
	n, ok := fts5ConfigInt(val)
	if !ok {
		return fmt.Errorf("engine: fts5: the %q command's value must be an integer (real, blob, NULL and non-integer text are errors in C fts5 too)", key)
	}
	if t := int32(n); t < rng[0] || t > rng[1] {
		return fmt.Errorf("engine: fts5: %d is out of range for the %q command (C fts5 requires %d..%d, checked on the value's 32-bit truncation %d)", n, key, rng[0], rng[1], t)
	}
	db.fts5ConfigPut(vm.name, key, Value{Typ: Int, I: n})
	return nil
}

// fts5SetRank applies the 'rank' command: it validates the rank string the
// same way sqlite3Fts5ConfigParseRank does and, if it parses, stores the value
// VERBATIM in %_config under the key as written.
//
// The check is purely SYNTACTIC -- fts5 never looks the function up here, only
// when a query actually reads the "rank" column (fts5FindRankFunction, which is
// where "no such function: <name>" comes from). Verified against the oracle:
// "INSERT INTO t(t,rank) VALUES('rank','nosuchfunc()')" SUCCEEDS and leaves the
// %_config row 'rank' = 'nosuchfunc()', and only the next "SELECT rank FROM t
// WHERE t MATCH ..." fails. Trailing garbage after the closing paren is
// accepted too ('bm25()trailing' is stored whole), while 'garbage', 'bm25(',
// the integer 5 and NULL are each an error.
func (db *DB) fts5SetRank(vm *vtabMeta, key string, val Value) error {
	if val.Typ == Null {
		// sqlite3_value_text() of a NULL is a null pointer, which
		// sqlite3Fts5ConfigParseRank rejects outright ("if( p==0 )").
		return fmt.Errorf("engine: fts5: the %q command's value must be a rank string of the form \"<function>(<args>)\"", key)
	}
	if _, _, ok := fts5ParseRank(valueToText(val)); !ok {
		return fmt.Errorf("engine: fts5: %q is not a valid rank string (C fts5 requires \"<function>(<args>)\", with literal arguments)", valueToText(val))
	}
	db.fts5ConfigPut(vm.name, key, val)
	return nil
}

// fts5ParseRank ports sqlite3Fts5ConfigParseRank (ext/fts5/fts5_config.c): a
// rank string is a BAREWORD function name, optional spaces, '(', a
// comma-separated list of LITERALS, and ')'. argsText is the raw text between
// the parens, empty when there are none; anything after the ')' is ignored.
//
// fts5's own "whitespace" here is the SPACE character alone (fts5_iswhitespace)
// and its bareword set is fts5IsBarewordByte's (fts5_tokenizers.go).
func fts5ParseRank(z string) (name, argsText string, ok bool) {
	i := fts5SkipRankSpace(z, 0)
	start := i
	for i < len(z) && fts5IsBarewordByte(z[i]) {
		i++
	}
	if i == start {
		return "", "", false
	}
	name = z[start:i]
	i = fts5SkipRankSpace(z, i)
	if i >= len(z) || z[i] != '(' {
		return "", "", false
	}
	i = fts5SkipRankSpace(z, i+1)
	if i < len(z) && z[i] == ')' {
		return name, "", true
	}
	end, aok := fts5SkipRankArgs(z, i)
	if !aok {
		return "", "", false
	}
	return name, z[i:end], true
}

// fts5SkipRankSpace is fts5ConfigSkipWhitespace: SPACE only.
func fts5SkipRankSpace(z string, i int) int {
	for i < len(z) && z[i] == ' ' {
		i++
	}
	return i
}

// fts5SkipRankArgs is fts5ConfigSkipArgs: literal (',' literal)* ')'. It
// returns the offset of the ')' that closes the list.
func fts5SkipRankArgs(z string, i int) (int, bool) {
	for {
		i = fts5SkipRankSpace(z, i)
		j, ok := fts5SkipRankLiteral(z, i)
		if !ok {
			return 0, false
		}
		i = fts5SkipRankSpace(z, j)
		if i >= len(z) {
			// Neither ')' nor ',' -- fts5ConfigSkipArgs's own failure arm
			// (the trailing NUL is not a comma).
			return 0, false
		}
		if z[i] == ')' {
			return i, true
		}
		if z[i] != ',' {
			return 0, false
		}
		i++
	}
}

// fts5SkipRankLiteral is fts5ConfigSkipLiteral: NULL, x'<hex>', a '...' string
// (with '' as the escape), or a number -- optional sign, digits, and at most
// one '.' that must be FOLLOWED by a digit. There is deliberately no exponent
// arm: fts5ConfigSkipLiteral's comment mentions 'E' but its code never skips
// one, so '1e5' is not a legal rank argument.
func fts5SkipRankLiteral(z string, i int) (int, bool) {
	if i >= len(z) {
		return 0, false
	}
	switch z[i] {
	case 'n', 'N':
		if len(z)-i >= 4 && strings.EqualFold(z[i:i+4], "null") {
			return i + 4, true
		}
		return 0, false
	case 'x', 'X':
		start := i
		i++
		if i >= len(z) || z[i] != '\'' {
			return 0, false
		}
		i++
		for i < len(z) && isHexDigit(z[i]) {
			i++
		}
		// The (p-pIn)%2 check counts the "x'" and the hex digits together, so
		// an ODD number of hex digits is rejected.
		if i >= len(z) || z[i] != '\'' || (i-start)%2 != 0 {
			return 0, false
		}
		return i + 1, true
	case '\'':
		i++
		for {
			if i >= len(z) {
				return 0, false
			}
			if z[i] == '\'' {
				i++
				if i >= len(z) || z[i] != '\'' {
					return i, true
				}
			}
			i++
		}
	default:
		start := i
		if z[i] == '+' || z[i] == '-' {
			i++
		}
		for i < len(z) && z[i] >= '0' && z[i] <= '9' {
			i++
		}
		if i+1 < len(z) && z[i] == '.' && z[i+1] >= '0' && z[i+1] <= '9' {
			i += 2
			for i < len(z) && z[i] >= '0' && z[i] <= '9' {
				i++
			}
		}
		if i == start {
			return 0, false
		}
		return i, true
	}
}

// fts5ConfigPut writes k=v into the %_config shadow table of the fts5 table
// named name, replacing any row with exactly that key (%_config's primary key
// is BINARY, so 'PGSZ' and 'pgsz' are two rows, which is what the oracle does
// too), and bumps the configuration cookie.
func (db *DB) fts5ConfigPut(name, k string, v Value) {
	cfg := db.findTableMeta(name + "_config")
	if cfg == nil {
		return
	}
	// %_config is WITHOUT ROWID, so its map key is a purely internal
	// identifier (1, 2, 3, ... -- schema_load_objects.go assigns them on load, and
	// never 0) and the "k" column is what orders its b-tree. Replacing a row
	// therefore means finding the one already holding that key.
	var key, max uint64
	for rid, rec := range cfg.rows.all() {
		if rid > max {
			max = rid
		}
		if len(rec) > 0 && rec[0].Typ == Text && string(rec[0].S) == k {
			key = rid
		}
	}
	if key == 0 {
		key = max + 1
	}
	cfg.putRow(key, []Value{{Typ: Text, S: []byte(k)}, v})
	// The ON-DISK cookie, which is what a C SQLite reader of this file consults.
	db.fts5BumpConfigCookie(name)
	// ...and OUR OWN, which is what the compiled-plan cache consults: a program
	// that reads "rank" baked in which auxiliary function to call, and this row is
	// what decides it. See DB.fts5ConfigGen.
	db.fts5ConfigGen++
}

// fts5ConfigGet returns the value of the %_config row whose key matches k
// case-insensitively, which is how C fts5 reads its own configuration back
// (a row keyed 'PGSZ' still sets the page size).
func (db *DB) fts5ConfigGet(name, k string) (Value, bool) {
	cfg := db.findTableMeta(name + "_config")
	if cfg == nil {
		return Value{}, false
	}
	for _, rec := range cfg.rows.all() {
		if len(rec) >= 2 && rec[0].Typ == Text && strings.EqualFold(string(rec[0].S), k) {
			return rec[1], true
		}
	}
	return Value{}, false
}

// fts5ConfigReadOf is fts5ConfigGet's READ-side twin: it reads the %_config
// row whose key matches k case-insensitively straight off the snapshot, for the
// compiler (fts5_vdbe_aux.go's fts5RankFunction). ok is false when the table
// holds no such row -- including when it has no %_config shadow at all, which
// only a hand-built file can manage and which is not worth failing a SELECT
// over.
func (p *ReadOnlyPager) fts5ConfigReadOf(name, k string) (Value, bool) {
	if p == nil {
		return Value{}, false
	}
	_, recs, err := p.fts5ExtShadowRows(name + "_config")
	if err != nil {
		return Value{}, false
	}
	for _, rec := range recs {
		if len(rec) >= 2 && rec[0].Typ == Text && strings.EqualFold(string(rec[0].S), k) {
			return rec[1], true
		}
	}
	return Value{}, false
}

// fts5ConfigPgsz is the leaf page budget the encoder should use for name: its
// %_config 'pgsz' row if it holds one in range, and FTS5_DEFAULT_PAGE_SIZE
// otherwise. Out-of-range values can only come from a file whose %_config was
// hand-edited (the command itself rejects them), and are ignored rather than
// trusted.
func (db *DB) fts5ConfigPgsz(name string) int {
	v, ok := db.fts5ConfigGet(name, "pgsz")
	if !ok {
		return fts5PageSize
	}
	n, ok := fts5ConfigInt(v)
	if !ok {
		return fts5PageSize
	}
	rng := fts5ConfigCommands["pgsz"]
	if t := int32(n); t >= rng[0] && t <= rng[1] {
		return int(t)
	}
	return fts5PageSize
}

// fts5SecureDeleteBumps reports whether removing a row from vm right now would
// move %_config's "version" row from 4 to 5 -- the one thing 'secure-delete'
// changes outside %_data.
//
// The rule, pinned against the oracle: version goes 4 -> 5 the first time a row
// is actually removed while 'secure-delete' is nonzero, and never moves again.
// Every boundary was checked --
//
//	base                       4    (nothing removed)
//	secure-delete=1, INSERT    4    (an insert removes nothing)
//	secure-delete=1, DELETE    5
//	secure-delete=1, UPDATE    5    (an update rewrites the old row)
//	secure-delete=1, DELETE of no rows    4
//	secure-delete=1, UPDATE of no rows    4
//	secure-delete=0, DELETE    4    (zero is off)
//	secure-delete=2, DELETE    5    (any nonzero)
//	DELETE then secure-delete=1           4  (order matters)
//	secure-delete=1, DELETE, secure-delete=0, DELETE   5  (sticky)
//	secure-delete=1, optimize/rebuild/merge           4  (no row removed)
//
// -- and 'Secure-Delete' spelled with capitals bumps it too, because fts5
// reads its own configuration back case-insensitively.
//
// Nothing else about the setting needs honouring: what secure-delete asks for
// is that a removed row leave no trace in the index, and an index rebuilt from
// %_content after every mutation (fts5SyncShadows) has never held one.
func (db *DB) fts5SecureDeleteBumps(vm *vtabMeta, nRemoved int) bool {
	if nRemoved == 0 {
		return false
	}
	if _, isFts5 := vm.store.(*fts5Store); !isFts5 {
		return false
	}
	sd, ok := db.fts5ConfigGet(vm.name, "secure-delete")
	if !ok {
		return false
	}
	if n, ok := fts5ConfigInt(sd); !ok || n == 0 {
		return false
	}
	v, ok := db.fts5ConfigGet(vm.name, "version")
	return !ok || v.Typ != Int || v.I != fts5SecureDeleteVersion
}

// fts5SecureDeleteGuard is the ONE shape of that rule an explicit transaction
// complicates: the bump is not a property of the statement, it lands when
// C fts5 FLUSHES its in-memory pending-index buffer, and inside a
// transaction that is a schedule rather than a rule -- measured against the
// oracle, with secure-delete on and a DELETE already issued inside a BEGIN,
// %_config still reads 4 after the DELETE itself, after a second DELETE,
// after an INSERT, after a MATCH, after 'integrity-check' and after 'rebuild'
// (which discards the pending removal outright), and reads 5 after COMMIT,
// after an explicit SAVEPOINT, after 'merge' or any configuration command,
// after an UPDATE of a DIFFERENT row, and after a schema-changing DDL (CREATE
// TABLE / CREATE INDEX / CREATE VIEW / DROP TABLE / ALTER TABLE -- but not
// CREATE TRIGGER, and not PRAGMA user_version).
//
// A DELETE's bump (deferrable=true, from deleteVtab) is now DEFERRED rather
// than declined: fts5_txn.go tracks the pending table set and applies it at
// the same points C fts5's xSync/xSavepoint do (see that file's header for
// the exact citations and the two shapes still left unmodeled). An UPDATE's
// bump (deferrable=false, from updateVtab) is still declined inside a
// transaction: C fts5's per-write flush trigger for an UPDATE runs through
// sqlite3Fts5IndexBeginWrite's rowid-ordering rule (fts5_index.c:6788-6810),
// not only the three txn callbacks, and this engine has no per-write hash to
// place that rule against.
//
// It is also where an external-content table's ROW SOURCE is checked
// (fts5_extcontent.go's fts5ExtRowSourceGuard): DELETE and UPDATE are the only
// two statements whose answer depends on which rows a scan of the fts5 table
// produces, and this is the gate both of them run before touching anything.
// nSelected is how many rows the statement's WHERE selected and nRemoved how
// many of them the INDEX actually loses. They differ in exactly one shape --
// the %_content-only UPDATE of a contentless_unindexed=1 table (vtab_fts5.go's
// UpdateRowSet), which touches rows without touching a posting -- and the two
// guards want different ones: the row-source guard asks whether the WHERE found
// anything, the secure-delete bump asks whether anything left the index.
func (db *DB) fts5SecureDeleteGuard(vm *vtabMeta, table string, nSelected, nRemoved int, deferrable bool) error {
	if rerr := db.fts5ExtRowSourceGuard(vm, table); rerr != nil {
		return rerr
	}
	if rerr := fts5ContentlessRowSourceGuard(vm, nSelected); rerr != nil {
		return rerr
	}
	if !db.inTransaction() || !db.fts5SecureDeleteBumps(vm, nRemoved) {
		return nil
	}
	if deferrable {
		return nil
	}
	return fmt.Errorf("engine: removing a row from the fts5 table %s via UPDATE while 'secure-delete' is on, inside an explicit transaction, is not supported by this engine: C fts5's per-write flush trigger for an UPDATE depends on an in-memory hash's rowid ordering that this engine does not keep", table)
}

// fts5NoteRowsRemoved applies (in autocommit) or defers (inside a
// transaction) the bump fts5SecureDeleteGuard has already cleared, once the
// rows really are gone. Only a DELETE ever reaches the deferred branch: the
// guard above declines an UPDATE outright before either the mutation loop or
// this call.
func (db *DB) fts5NoteRowsRemoved(vm *vtabMeta, nRemoved int) {
	if !db.fts5SecureDeleteBumps(vm, nRemoved) {
		return
	}
	if db.inTransaction() {
		db.fts5TxnDefer(vm.name)
		return
	}
	db.fts5ConfigPut(vm.name, "version", Value{Typ: Int, I: fts5SecureDeleteVersion})
}

// fts5BumpConfigCookie increments the CONFIGURATION COOKIE, the structure
// record's leading 4-byte big-endian field.
//
// Verified against the oracle: it counts %_config row writes -- one per
// configuration command (including one that stores a value the row already
// held) and one for the secure-delete version bump, but NOT for 'optimize',
// 'rebuild', 'merge', 'integrity-check' or any ordinary INSERT/DELETE -- it is
// written into %_data immediately, and every later write carries it forward,
// across connections.
func (db *DB) fts5BumpConfigCookie(name string) {
	data := db.findTableMeta(name + "_data")
	if data == nil {
		return
	}
	rec, ok := data.rows.get(uint64(fts5StructureRowid))
	if !ok || len(rec) < 2 || rec[1].Typ != Blob || len(rec[1].S) < 4 {
		return
	}
	blk := append([]byte(nil), rec[1].S...)
	binary.BigEndian.PutUint32(blk[:4], binary.BigEndian.Uint32(blk[:4])+1)
	data.putRow(uint64(fts5StructureRowid), []Value{{Typ: Null}, {Typ: Blob, S: blk}})
}

// fts5ConfigCookie reads the cookie back out of the structure record, so a
// rebuild can carry it forward rather than resetting it to zero.
func fts5ConfigCookie(data *tableMeta) uint32 {
	if data == nil {
		return 0
	}
	rec, ok := data.rows.get(uint64(fts5StructureRowid))
	if !ok || len(rec) < 2 || rec[1].Typ != Blob || len(rec[1].S) < 4 {
		return 0
	}
	return binary.BigEndian.Uint32(rec[1].S[:4])
}

// fts5SetConfigCookie stamps cookie into a freshly encoded structure record.
func fts5SetConfigCookie(rec []byte, cookie uint32) {
	if len(rec) >= 4 {
		binary.BigEndian.PutUint32(rec[:4], cookie)
	}
}
