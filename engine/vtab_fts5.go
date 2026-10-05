// The "fts5" full-text virtual-table module (ext/fts5), a writable built-in
// module (vtab.go, vtab_write.go) enabled by an opt-in RegisterFTS5() call.
//
// Schema: CREATE VIRTUAL TABLE t USING fts5(col1, col2, ...), each argument a
// column name optionally followed by UNINDEXED (fts5ParseColumnSpec). The
// engine sees [rowid HIDDEN, col1, col2, ...]: the hidden rowid fills the
// writable-vtab contract's slot 0, so "SELECT *" returns only the text columns,
// as in C.
//
// Reads of a table with content are served from %_content, tokenizing and
// matching each row (evalMatch), since the engine re-applies the WHERE over the
// vtab's rows and the inverted index only changes which rows are scanned, never
// which match. A contentless table (content='', fts5_contentless.go) is read
// back from %_data's index.
//
// The file is fts5's: all five shadow tables (%_data, %_idx, %_content,
// %_docsize, %_config) are written (fts5_shadow.go; the inverted index in
// fts5_index.go), so databases are interchangeable with C both ways
// (TestFts5ShadowLayoutDiff, TestFts5FileInterchange).
//
// Also here or nearby: NEAR queries (fts5_query.go); "rank" and
// bm25/highlight/snippet (fts5_aux.go, fts5_snippet.go, wired by
// fts5_vdbe_aux.go); fts5vocab (vtab_fts5vocab.go); external content
// (content=, content_rowid=, fts5_extcontent.go); contentless tables including
// contentless_delete=1 (fts5_contentless.go). Options are accepted only where
// reproduced exactly (fts5Options). Unknown tokenizers and most command-channel
// commands decline (fts5_shadow.go's fts5CommandInsert).
package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// RegisterFTS5 makes "fts5" (and "fts5vocab", meaningless without it) available
// for CREATE VIRTUAL TABLE, in the registry rtree uses. It is opt-in because the
// default oracle build lacks fts5: auto-registering would make the engine accept
// statements the oracle rejects only for lack of the module. The harness
// registers it in its "-tags sqlite_fts5" build, where the oracle has fts5.
// Idempotent.
func RegisterFTS5() {
	RegisterVtabModule("fts5", fts5Module{})
	RegisterVtabModule("fts5vocab", fts5VocabModule{})
}

// UnregisterFTS5 removes the fts5 and fts5vocab modules (the inverse of
// RegisterFTS5), so a test that enabled fts5 can restore the default no-fts5
// state for other tests sharing the same process.
func UnregisterFTS5() {
	unregisterVtabModule("fts5")
	unregisterVtabModule("fts5vocab")
}

// fts5Module is the fts5 VtabModule. It is writable (newWritableStore, the
// writableVtabModule contract in vtab_write.go).
type fts5Module struct{}

// Connect satisfies VtabModule. fts5 needs a CREATE VIRTUAL TABLE naming its
// columns, so Connect is used only to validate module arguments at CREATE time
// and to declare the column schema; the live table is built by newWritableStore.
func (m fts5Module) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	st, err := m.buildStore("", args)
	if err != nil {
		return nil, nil, err
	}
	return st.columns, st, nil
}

// newWritableStore satisfies the writableVtabModule contract (vtab_write.go).
func (m fts5Module) newWritableStore(tableName string, args []string) (vtabStore, error) {
	return m.buildStore(tableName, args)
}

// fts5NoColumnsErr is C's error for "CREATE VIRTUAL TABLE ftbad USING fts5()".
// fts5 never checks for zero columns: sqlite3Fts5ConfigDeclareVtab
// (fts5_config.c:760-767) emits "CREATE TABLE x(, 'ftbad' HIDDEN, rank
// HIDDEN)", sqlite3_declare_vtab rejects it, and vtab.c:624-626 supplies
//
//	if( zErr==0 ){
//	  *pzErr = sqlite3MPrintf(db, "vtable constructor failed: %s", zModuleName);
//
// where zModuleName is the table's name (vtab.c:589): "vtable constructor
// failed: ftbad".
func fts5NoColumnsErr(tableName string) error {
	if tableName == "" {
		return fmt.Errorf("vtable constructor failed")
	}
	return fmt.Errorf("vtable constructor failed: %s", tableName)
}

func (m fts5Module) buildStore(tableName string, args []string) (*fts5Store, error) {
	if len(args) == 0 {
		return nil, fts5NoColumnsErr(tableName)
	}
	var colNames []string
	var unindexed []bool
	opts := fts5Options{columnsize: true, detail: fts5DetailFull}
	seen := map[string]bool{}
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if key, val, isOpt := fts5SplitOption(a); isOpt {
			if err := opts.apply(key, val); err != nil {
				return nil, err
			}
			continue
		}
		name, unidx, err := fts5ParseColumnSpec(a)
		if err != nil {
			return nil, err
		}
		low := r33sFoldIdent(name)
		if seen[low] {
			return nil, fmt.Errorf("fts5: duplicate column name: %s", name)
		}
		// Real fts5 refuses three column names, each verified against the
		// oracle: "rank" and "rowid" are "reserved fts5 column name: <name>",
		// and a column named after the TABLE is "vtable constructor failed:
		// <name>" -- the last because it would collide with the command
		// channel ("INSERT INTO t(t) VALUES('optimize')"), which is exactly
		// what lets fts5CommandInsert tell a command from a row.
		if low == "rank" || low == "rowid" {
			return nil, fmt.Errorf("reserved fts5 column name: %s", name)
		}
		if tableName != "" && strings.EqualFold(name, tableName) {
			return nil, fmt.Errorf("fts5: a column may not share the table's name (%s): it would collide with fts5's command channel", name)
		}
		seen[low] = true
		colNames = append(colNames, name)
		unindexed = append(unindexed, unidx)
	}
	if len(colNames) == 0 {
		return nil, fts5NoColumnsErr(tableName)
	}
	// The two contentless-only flags, checked in fts5ConfigParse's own order and
	// with its own messages. Both are guarded on the flag being SET, so a 0 is
	// inert on every table; and both are checked BEFORE the "no content= option"
	// defaults below, which is what makes "contentless_delete=1" alone (no
	// content= at all) the "requires a contentless table" error rather than
	// anything about columnsize.
	contentless := opts.hasContent && opts.content == ""
	if opts.contentlessDelete {
		if !contentless {
			return nil, fmt.Errorf("contentless_delete=1 requires a contentless table")
		}
		if !opts.columnsize {
			return nil, fmt.Errorf("contentless_delete=1 is incompatible with columnsize=0")
		}
	}
	if opts.contentlessUnindexed && !contentless {
		return nil, fmt.Errorf("contentless_unindexed=1 requires a contentless table")
	}
	if contentless {
		if err := fts5ContentlessDecline(opts, unindexed); err != nil {
			return nil, err
		}
	}
	// content_rowid= only means anything on an external-content table: real
	// fts5 stores it whatever the content mode is, and on an ordinary table
	// composes "SELECT T.'<name>', T.c0, ... FROM 't_content' T", which %_content
	// has no such column for -- so every read of it is an error there. Declined
	// rather than reproduced.
	if opts.contentRowid != "" && opts.content == "" {
		return nil, fmt.Errorf("fts5: content_rowid=%s without content=<table> is not supported by this engine (C fts5 stores the name and then fails every read, because %%_content has no column of that name)", opts.contentRowid)
	}
	if opts.content != "" {
		// fts5's default is "rowid", which on an ordinary rowid content table
		// resolves to the table's rowid rather than to a declared column
		// (fts5_config.c: 'pRet->zContentRowid = "rowid"').
		if opts.contentRowid == "" {
			opts.contentRowid = "rowid"
		}
		// The indexed-document SET of an external-content table is recoverable
		// only from %_docsize, which columnsize=0 does not create -- see
		// fts5_extcontent.go for why this engine has to know it.
		if !opts.columnsize {
			return nil, fmt.Errorf("fts5: columnsize=0 together with content=%s is not supported by this engine (without %%_docsize there is no record of which rowids the index holds, and an external-content table's rows are not stored anywhere else)", opts.content)
		}
	}
	if err := fts5LocaleDeclineUnindexedContentless(opts.locale, contentless && opts.contentlessUnindexed && fts5AnyUnindexed(unindexed)); err != nil {
		return nil, err
	}
	// Declared columns: leading hidden "rowid" (the storage slot 0 / rowid),
	// then the text columns.
	cols := make([]VtabColumn, 0, len(colNames)+1)
	cols = append(cols, VtabColumn{Name: "rowid", Type: "INTEGER", Hidden: true})
	for i, n := range colNames {
		cols = append(cols, VtabColumn{Name: n, Unindexed: unindexed[i]})
	}
	return &fts5Store{
		name:              tableName,
		colNames:          colNames,
		unindexed:         unindexed,
		columnsize:        opts.columnsize,
		prefixes:          opts.prefixes,
		tok:               opts.tok,
		detail:            opts.detail,
		extContent:        opts.content,
		extRowid:          opts.contentRowid,
		contentless:       contentless,
		contentlessDelete: contentless && opts.contentlessDelete,
		contentUnindexed:  contentless && opts.contentlessUnindexed && fts5AnyUnindexed(unindexed),
		locale:            opts.locale,
		origins:           map[int64]int64{},
		originCntr:        1,
		columns:           cols,
		rows:              map[int64][]Value{},
		locales:           map[int64][]Value{},
	}, nil
}

// fts5AnyUnindexed reports whether any column carries UNINDEXED -- the
// "bUnindexed" fts5ConfigParse computes for its FTS5_CONTENT_UNINDEXED test.
func fts5AnyUnindexed(unindexed []bool) bool {
	for _, u := range unindexed {
		if u {
			return true
		}
	}
	return false
}

// fts5HasContentShadow reports whether this table owns a %_content shadow.
// FTS5_CONTENT_NORMAL and FTS5_CONTENT_UNINDEXED do (fts5_storage.c's
// sqlite3Fts5StorageOpen creates it for exactly those two); an external-content
// table's rows are somebody else's table, and a plain contentless table's are
// nowhere.
func (s *fts5Store) fts5HasContentShadow() bool {
	return s.extContent == "" && (!s.contentless || s.contentUnindexed)
}

// fts5TokenizerOf resolves the tokenize= tokenizer from a stored CREATE VIRTUAL
// TABLE's module arguments, without building a whole store -- the read path's
// entry point (fts5SchemaTok, fts5_match.go), which must be able to see the
// tokenizer of a table REAL SQLITE wrote. A nil tokenizer with a nil error is
// fts5's default; a non-nil error is the reason this engine cannot serve the
// table, and every read of it declines rather than tokenizing it wrongly.
func fts5TokenizerOf(args []string) (*fts5Tokenizer, error) {
	var tok *fts5Tokenizer
	for _, a := range args {
		key, val, isOpt := fts5SplitOption(strings.TrimSpace(a))
		if !isOpt || !strings.EqualFold(key, "tokenize") {
			continue
		}
		tk, err := fts5ParseTokenizer(fts5Dequote(val))
		if err != nil {
			return nil, err
		}
		tok = tk
	}
	return tok, nil
}

// fts5ContentModeOf resolves the stored module arguments' content= option to
// fts5's own Fts5Config.eContent three-way split (fts5_config.c's
// fts5ConfigParseSpecial, which picks off zArg alone): external names the
// table, contentless is content= with an EMPTY value, and both false is an
// ordinary %_content-backed table.
func fts5ContentModeOf(args []string) (external string, contentless bool) {
	for _, a := range args {
		key, val, isOpt := fts5SplitOption(strings.TrimSpace(a))
		if !isOpt {
			continue
		}
		if canon, ok := fts5CanonicalOption(key); !ok || canon != "content" {
			continue
		}
		external = fts5Dequote(val)
		contentless = external == ""
	}
	return external, contentless
}

// fts5Options is the configuration-option state a CREATE VIRTUAL TABLE
// accumulates. Only options reproduced exactly are accepted; anything else
// declines with its reason, since honoring an option wrongly writes a database
// that disagrees with C's from then on. Read off the oracle's shadow bytes:
//
//	columnsize=1  ACCEPTED. Byte-identical to no option at all: same five
//	              shadow tables, same %_data, same %_config. It IS the default.
//	columnsize=0  ACCEPTED. The one non-default value that is exact here: its
//	              only effect is that %_docsize is NOT CREATED. Everything else
//	              is untouched -- same other four shadow tables and their
//	              sqlite_master rows, same %_config, and an averages record
//	              that still carries the per-column token totals (x'010202' for
//	              one row of 'hello world'/'foo bar', exactly as with
//	              columnsize=1), same segment page byte for byte.
//	detail=       ACCEPTED in all three modes (full, none, columns); see
//	              fts5_detail.go. It changes the doclist encoding in %_data and
//	              makes some MATCH queries errors (fts5_query.go), nothing else.
//	tokenize=     ACCEPTED for unicode61 (with or without arguments), ascii
//	              and trigram; others DECLINED (fts5_tokenizers.go).
//	prefix=N      ACCEPTED. A second term space inside the same segment
//	              (fts5BuildIndex); MATCH results are unchanged.
//	tokendata=    ACCEPTED, and inert (see apply()).
//	content=<tbl> / content_rowid=
//	              ACCEPTED (fts5_extcontent.go): no %_content; rows are read
//	              from the named table by the named rowid column.
//	content=''    ACCEPTED (fts5_contentless.go): rows are stored nowhere and
//	              read back from %_data, contentless_unindexed=1 included.
//	              columnsize=0 and detail=none/columns with it still decline.
type fts5Options struct {
	// columnsize is columnsize=, true for 1 (the default) and false for 0.
	// buildStore seeds it true.
	columnsize bool
	// prefixes is every prefix= length, in the order written across all
	// prefix= directives -- the order IS the encoding, since a prefix index's
	// leading term byte is its ordinal here (fts5_index.go).
	prefixes []int
	// tok is the tokenize= tokenizer, nil for fts5's default unicode61 (see
	// fts5_tokenizers.go, which also says which spellings are declined).
	tok *fts5Tokenizer
	// detail is detail= (fts5_detail.go), seeded fts5DetailFull by buildStore
	// -- fts5ConfigParse's own "pRet->eDetail = FTS5_DETAIL_FULL". A repeated
	// detail= is accepted and the LAST one wins, exactly like columnsize=
	// (fts5ConfigParseSpecial simply assigns pConfig->eDetail each time).
	detail fts5Detail
	// content is content='s value, DEQUOTED, and hasContent whether the option
	// was given at all. Together they are fts5's single Fts5Config.eContent
	// (fts5_config.c's fts5ConfigParseSpecial): absent is FTS5_CONTENT_NORMAL,
	// an EMPTY value is FTS5_CONTENT_NONE (contentless) and a non-empty one is
	// FTS5_CONTENT_EXTERNAL, naming the table the index is built over. Only the
	// third is implemented here -- see fts5_extcontent.go.
	content    string
	hasContent bool
	// contentlessDelete / contentlessUnindexed are the two options that only
	// mean anything on a contentless table. Both are plain 0/1 flags
	// (fts5_config.c's fts5ConfigParseSpecial), and a 0 is INERT whatever the
	// table is -- fts5's own "requires a contentless table" checks are guarded
	// on the flag being SET, so "contentless_delete=0" on an ordinary table is
	// accepted there and changes nothing.
	contentlessDelete    bool
	contentlessUnindexed bool
	// locale is locale=1: the table stores a per-column LOCALE STRING beside
	// each value, in extra "l<i>" columns of %_content. See fts5_locale.go,
	// which owns the whole subject -- including why the index bytes are
	// unaffected, and where an EXTERNAL-content table's locales live instead
	// (inside the content table's own values, since it has no l<i> column).
	locale bool
	// contentRowid is content_rowid='s value, DEQUOTED, "" when the option was
	// not given (fts5's own default is "rowid", filled in by buildStore rather
	// than here so that "was it written?" stays answerable for the
	// "multiple content_rowid=... directives" check).
	contentRowid string
	// seenTokenize tracks a repeated tokenize=, which C fts5 rejects
	// outright ("multiple tokenize=... directives", verified) even when both
	// spellings are legal on their own. columnsize= and detail= are NOT like
	// that there: a repeat is accepted and the LAST one wins (verified --
	// "columnsize=1, columnsize=0" has no %_docsize and "columnsize=0,
	// columnsize=1" does), which is exactly what assigning below reproduces.
	seenTokenize bool
}

// fts5OptionNames is fts5ConfigParseSpecial's dispatch chain, IN ITS ORDER
// (ext/fts5/fts5_config.c). Order is semantic, not cosmetic: each arm tests
// `sqlite3_strnicmp(<full name>, zCmd, strlen(zCmd))==0`, i.e. the written key
// is matched as a case-insensitive PREFIX of the full name and the FIRST arm
// that accepts it wins. So "c=t1" is content= (content is reached before
// contentless_delete), "t='trigram'" is tokenize=, "column" is columnsize= and
// a bare "=3" -- an empty key, which prefixes everything -- is prefix=3.
//
// The names not implemented here are still listed, so that an abbreviation
// resolves to the same option C fts5 resolves it to and is then declined
// with that option's own reason rather than as an unknown name.
var fts5OptionNames = []string{
	"prefix",
	"tokenize",
	"content",
	"contentless_delete",
	"contentless_unindexed",
	"content_rowid",
	"columnsize",
	"locale",
	"detail",
	"tokendata",
}

// fts5CanonicalOption resolves a written option key to the full option name
// fts5 dispatches it to, or false if it prefixes none of them.
//
// An EMPTY key would prefix every name, but it never reaches the chain: the
// argument is gobbled first, and fts5ConfigSkipBareword returns NULL for a
// zero-length bareword, so "USING fts5(a, =2)" is `parse error in "=2"` rather
// than prefix=2. Verified against the oracle after this function first got it
// wrong the other way.
func fts5CanonicalOption(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	for _, name := range fts5OptionNames {
		if len(key) <= len(name) && strings.EqualFold(key, name[:len(key)]) {
			return name, true
		}
	}
	return "", false
}

// apply records one "key=value" configuration option, or returns the reason it
// is declined.
func (o *fts5Options) apply(key, val string) error {
	canon, ok := fts5CanonicalOption(key)
	if !ok {
		return fmt.Errorf("unrecognized option: %q", key)
	}
	switch canon {
	case "columnsize":
		// The value is matched LITERALLY, not parsed as a number: C fts5
		// answers "malformed columnsize=... directive" for both 2 and 01.
		switch fts5Dequote(val) {
		case "1":
			o.columnsize = true
			return nil
		case "0":
			o.columnsize = false
			return nil
		}
		return fmt.Errorf("malformed columnsize=... directive")
	case "detail":
		d, ok := fts5ParseDetail(fts5Dequote(val))
		if !ok {
			return fmt.Errorf("malformed detail=... directive")
		}
		o.detail = d
		return nil
	case "tokendata":
		// Matched literally against "0" and "1", like columnsize=
		// (fts5ConfigParseSpecial: "if( (zArg[0]!='0' && zArg[0]!='1') ||
		// zArg[1]!='\0' )").
		//
		// The flag is inert here: bTokendata records which source token
		// produced each term, read only by xInstToken (no SQL reaches it) and
		// fts5BestIndexMethod (where it just stops the vtab consuming "ORDER BY
		// rowid DESC"). Over unicode61, "porter ascii" and trigram, a
		// tokendata=1 table and its plain twin leave identical %_data,
		// %_docsize and %_config and answer MATCH/bm25/highlight the same. It
		// could differ only for a custom tokenizer emitting colocated tokens.
		switch fts5Dequote(val) {
		case "0", "1":
			return nil
		}
		return fmt.Errorf("malformed tokendata=... directive")
	case "tokenize":
		if o.seenTokenize {
			return fmt.Errorf("multiple tokenize=... directives")
		}
		o.seenTokenize = true
		tk, terr := fts5ParseTokenizer(fts5Dequote(val))
		if terr != nil {
			return terr
		}
		o.tok = tk
		return nil
	case "prefix":
		add, perr := fts5ParsePrefixList(fts5Dequote(val))
		if perr != nil {
			return perr
		}
		// A repeated prefix= APPENDS rather than replacing (verified:
		// "prefix=2, prefix=3" leaves the same two term spaces, in the same
		// order, as "prefix='2 3'"), and the LIMIT is counted across the whole
		// accumulated list ("too many prefix indexes (max 31)" for 32 lengths
		// however they are spelled).
		if len(o.prefixes)+len(add) > fts5MaxPrefixIndexes {
			return fmt.Errorf("too many prefix indexes (max %d)", fts5MaxPrefixIndexes)
		}
		o.prefixes = append(o.prefixes, add...)
		return nil
	case "content":
		// fts5ConfigParseSpecial's own guard is "eContent!=FTS5_CONTENT_NORMAL",
		// i.e. a SECOND content= of any value -- including a second empty one --
		// is "multiple content=... directives".
		if o.hasContent {
			return fmt.Errorf("multiple content=... directives")
		}
		o.hasContent = true
		o.content = fts5Dequote(val)
		// An EMPTY value is FTS5_CONTENT_NONE, a CONTENTLESS table: no
		// %_content and no external table either, so the only row source is
		// %_data's own inverted index. fts5_contentless.go reads it.
		return nil
	case "contentless_delete", "contentless_unindexed":
		// Both are matched LITERALLY against "0" and "1", and BOTH report
		// "malformed contentless_delete=... directive" for anything else --
		// fts5_config.c's contentless_unindexed arm carries that same string,
		// which looks like a copy-paste but is what the oracle answers, so it
		// is reproduced rather than corrected.
		on := false
		switch fts5Dequote(val) {
		case "1":
			on = true
		case "0":
		default:
			return fmt.Errorf("malformed contentless_delete=... directive")
		}
		if canon == "contentless_delete" {
			o.contentlessDelete = on
		} else {
			o.contentlessUnindexed = on
		}
		return nil
	case "locale":
		// Matched LITERALLY against "0" and "1", exactly like columnsize=:
		// fts5ConfigParseSpecial's locale arm is
		// "if( (zArg[0]!='0' && zArg[0]!='1') || zArg[1]!='\0' )"
		// (ext/fts5/fts5_config.c:394). fts5_locale.go says what the flag does.
		switch fts5Dequote(val) {
		case "1":
			o.locale = true
			return nil
		case "0":
			o.locale = false
			return nil
		}
		return fmt.Errorf("malformed locale=... directive")
	case "content_rowid":
		if o.contentRowid != "" {
			return fmt.Errorf("multiple content_rowid=... directives")
		}
		o.contentRowid = fts5Dequote(val)
		if o.contentRowid == "" {
			// "SELECT T.'' FROM ..." is not resolvable; C fts5 stores the
			// empty name and fails every read of the table with it.
			return fmt.Errorf("fts5: content_rowid= names no column")
		}
		return nil
	}
	// A name fts5 knows and this engine does not implement: declined by its
	// CANONICAL name, so that "cont=" and "contentless_delete=" report the same
	// thing (and so that an abbreviation is never silently ignored).
	return fmt.Errorf("fts5: the %s=%s option is not supported by this engine", canon, val)
}

// fts5MaxPrefixIndexes is FTS5_MAX_PREFIX_INDEXES: C fts5 answers "too many
// prefix indexes (max 31)" for a 32nd, verified both as one prefix='1 2 ... 32'
// and as two prefix= directives of 16 each.
const fts5MaxPrefixIndexes = 31

// fts5ParsePrefixList parses a prefix= value into its lengths in written order,
// which decides each one's term-space byte (fts5_index.go), so it is neither
// sorted nor deduplicated: '3 2' and '2 3' produce different files. Separators
// are spaces, a comma, or both ('1 2', '1,2', '1 ,2', '1, 2, 3'); empty or
// all-space is an empty list; lengths are 1..999 ("prefix length out of range
// (max 999)"); leading zeros are fine ('01' is 1); anything else ('+1', '1.0',
// '1 x', ',', '1,') is "malformed prefix=... directive".
func fts5ParsePrefixList(s string) ([]int, error) {
	malformed := fmt.Errorf("malformed prefix=... directive")
	var out []int
	i := 0
	// Only a literal SPACE separates, which is all that was verified; a tab
	// there falls through to "malformed", a decline either way.
	skipSpace := func() {
		for i < len(s) && s[i] == ' ' {
			i++
		}
	}
	for {
		skipSpace()
		if i == len(s) {
			return out, nil
		}
		start := i
		n := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			if n <= 999 { // saturate rather than overflow on a long run of digits
				n = n*10 + int(s[i]-'0')
			}
			i++
		}
		if i == start {
			return nil, malformed
		}
		if n < 1 || n > 999 {
			return nil, fmt.Errorf("prefix length out of range (max 999)")
		}
		out = append(out, n)
		skipSpace()
		if i < len(s) && s[i] == ',' {
			i++
			skipSpace()
			if i == len(s) {
				return nil, malformed
			}
		}
	}
}

// fts5SplitOption splits a raw fts5 module argument into a configuration
// option's key and value at its first TOP-LEVEL '=' (one outside any quote).
// ok is false when there is none -- the argument is then a column.
//
// Verified against the oracle: ANY "key=value" argument is an option and never
// a column ("fts5(a, b=1)" is `unrecognized option: "b"`, not a column named
// b), whitespace around the '=' is insignificant ("COLUMNSIZE = 1" works), and
// the key must be an UNQUOTED bare word -- fts5(a, "detail"=none) and
// fts5(a, [detail]=none) are both "parse error in ...". The key is returned
// RAW, so a quoted one simply matches no option name in apply and is declined.
func fts5SplitOption(a string) (key, val string, ok bool) {
	quote := byte(0)
	for i := 0; i < len(a); i++ {
		c := a[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '=':
			return strings.TrimSpace(a[:i]), strings.TrimSpace(a[i+1:]), true
		}
	}
	return "", "", false
}

// fts5Dequote strips one layer of SQL quoting from a configuration option's
// VALUE. Verified: columnsize='1', columnsize="1" and tokenize=[unicode61] are
// each exactly the bare spelling.
//
// A DOUBLED delimiter inside the quotes is one literal delimiter, which is what
// makes a tokenizer argument's own quoting reachable at all:
// tokenize='unicode61 tokenchars ''-''' has to arrive at fts5ParseTokenizer as
// "unicode61 tokenchars '-'" for its inner '-' to be a quoted argument rather
// than an unterminated one. (SQLite's bracket quoting has no escape, so only
// the three delimiters that double are collapsed.)
func fts5Dequote(s string) string {
	if len(s) < 2 {
		return s
	}
	closer := s[0]
	switch s[0] {
	case '[':
		closer = ']'
	case '\'', '"', '`':
	default:
		return s
	}
	if s[len(s)-1] != closer {
		return s
	}
	inner := s[1 : len(s)-1]
	if closer == ']' {
		return inner
	}
	return strings.ReplaceAll(inner, string([]byte{closer, closer}), string(closer))
}

// fts5ParseColumnSpec parses one fts5 column argument: a bare or quoted name,
// optionally followed by UNINDEXED. Other options decline (ignoring one would
// change which rows MATCH selects).
//
//   - UNINDEXED is case-insensitive and separated by spaces only ("a\tUNINDEXED"
//     is "parse error in ...");
//   - it is the only option: "b NOT NULL" is "parse error in \"b NOT NULL\"",
//     "a UNINDEXEDX" is "unrecognized column option: UNINDEXEDX";
//   - a quoted name is taken whole: fts5("a UNINDEXED") is one indexed column
//     named "a UNINDEXED";
//   - the reserved-name check runs after stripping the modifier: fts5(rank
//     UNINDEXED) is "reserved fts5 column name: rank".
func fts5ParseColumnSpec(a string) (name string, unindexed bool, err error) {
	s := strings.TrimSpace(a)
	if s == "" {
		return "", false, fmt.Errorf("fts5: empty column name")
	}
	var rest string
	switch s[0] {
	case '"', '\'', '`', '[':
		closer := s[0]
		if closer == '[' {
			closer = ']'
		}
		name, rest, err = fts5SplitQuotedName(s, closer)
		if err != nil {
			return "", false, err
		}
		if closer == ']' {
			name = strings.TrimSpace(name)
		}
		if name == "" {
			return "", false, fmt.Errorf("fts5: empty column name")
		}
	default:
		name = s
		if sp := strings.IndexByte(s, ' '); sp >= 0 {
			name, rest = s[:sp], s[sp:]
		}
		if strings.ContainsAny(name, "\t\r\n") {
			return "", false, fmt.Errorf("fts5: column option in %q is not supported by this engine (only a bare name, optionally followed by UNINDEXED)", a)
		}
	}
	if rest = strings.Trim(rest, " "); rest != "" {
		if !strings.EqualFold(rest, "UNINDEXED") {
			return "", false, fmt.Errorf("fts5: column option in %q is not supported by this engine (only UNINDEXED)", a)
		}
		unindexed = true
	}
	return name, unindexed, nil
}

// fts5SplitQuotedName splits s -- which begins with an opening quote whose
// matching closer is closer -- into the dequoted identifier and whatever text
// follows the closing quote. A DOUBLED quote character inside is an escaped
// one, as everywhere else in SQL; a bracketed name has no escape.
func fts5SplitQuotedName(s string, closer byte) (name, rest string, err error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != closer {
			b.WriteByte(s[i])
			continue
		}
		if closer != ']' && i+1 < len(s) && s[i+1] == closer {
			b.WriteByte(closer)
			i++
			continue
		}
		return b.String(), s[i+1:], nil
	}
	return "", "", fmt.Errorf("fts5: malformed quoted column name: %s", s)
}

// fts5Store is a writable fts5 table's live, in-session state. rows maps a
// rowid to that row's TEXT-column values only (length len(colNames), original
// Values preserved so SELECT returns them verbatim); the rowid is the map key.
type fts5Store struct {
	name     string
	colNames []string // text column names, in declaration order
	// unindexed is parallel to colNames: true for a column declared
	// "<name> UNINDEXED", whose text is stored but never tokenized.
	unindexed []bool
	// columnsize is fts5's columnsize= setting, true (the default) when the
	// table has a %_docsize shadow table and false for columnsize=0, which has
	// none. See fts5Options.
	columnsize bool
	// prefixes is fts5's prefix= lengths, in the order written.
	prefixes []int
	// tok is fts5's tokenize= tokenizer, nil for the unicode61 default. It is
	// what fts5SyncShadows tokenizes %_content through, so it decides the
	// index bytes as much as it decides which rows MATCH selects.
	tok *fts5Tokenizer
	// detail is fts5's detail= mode (fts5_detail.go): the doclist shape
	// fts5BuildIndex writes, and the MATCH queries this table refuses.
	detail fts5Detail
	// extContent is content=<table>: the EXTERNAL table this index is built
	// over, "" for an ordinary %_content-backed one. extRowid is
	// content_rowid=, the column of that table carrying each document's rowid
	// ("rowid" by default, which resolves to the table's own rowid). See
	// fts5_extcontent.go for the whole subject; rows below then holds the
	// INDEXED documents reconstructed from that table, not stored ones.
	extContent string
	extRowid   string
	// contentless is content='' (FTS5_CONTENT_NONE): the table stores no
	// documents at all and its only row source is %_data's inverted index.
	// fts5_contentless.go owns the subject; rows then holds one all-NULL entry
	// per indexed document (C fts5's xColumn returns nothing for such a
	// table) and tokens holds the DOCUMENTS, read back out of the index.
	contentless bool
	// contentlessDelete is contentless_delete=1: the one contentless table a row
	// may be DELETEd from. fts5_contentless.go owns what it changes -- the
	// %_docsize origin column, the V2 structure record, and the tombstones this
	// engine reads but (rebuilding the whole index per write) never has to write.
	contentlessDelete bool
	// contentUnindexed is fts5's FTS5_CONTENT_UNINDEXED: a contentless table
	// carrying contentless_unindexed=1 AND at least one UNINDEXED column, which
	// is exactly the pair fts5_config.c turns into that third content mode
	// ("else if( bUnindexed && pRet->bContentlessUnindexed )"). Such a table DOES
	// have a %_content, holding only its UNINDEXED columns; contentless is still
	// true, because everything else about it -- no stored indexed columns, no
	// DELETE, the index as the row source -- is unchanged. fts5_contentless.go
	// owns it.
	contentUnindexed bool
	// locale is fts5's locale=1: the table's %_content carries an extra "l<i>"
	// column per INDEXED column, holding that value's locale string. locales is
	// what those columns hold, keyed by rowid and parallel to colNames (Null in
	// an UNINDEXED slot, which has no l column). fts5_locale.go owns the
	// subject -- in particular, why every locale this engine ever writes came
	// out of a file C SQLite wrote.
	locale  bool
	locales map[int64][]Value
	// origins is each live document's %_docsize ORIGIN, and originCntr the value
	// the next flushed segment will take. A document with no entry here has not
	// been given one yet -- fts5ContentlessFlush does that at the next sync,
	// which is this engine's flush.
	origins    map[int64]int64
	originCntr int64
	// avgRow / avgSizes are the AVERAGES record of a contentless_delete=1
	// table, which is not derivable from the live documents: fts5's
	// contentless-delete arm (fts5StorageContentlessDelete) never touches the
	// totals, where every other delete path decrements them
	// (fts5StorageDeleteFromIndex's "p->aTotalSize[iCol-1] -= ctx.szCol" and
	// "p->nTotalRow--"). So they only ever GROW, until 'delete-all' reinitializes
	// them -- verified against the oracle, whose two-row table still reports
	// x'0204' after one of the two is deleted, and x'0505' after two of five.
	// They decide bm25()/rank, which is how a wrong one shows up.
	avgRow   int64
	avgSizes []int64
	// originOpen records that the origin currently in originCntr has already
	// been handed to documents flushed inside the OPEN transaction. Real fts5
	// flushes its hash once per transaction, so every row a transaction inserts
	// shares one origin and the counter moves on only when the next flush lands
	// outside it -- verified against the oracle, whose three rows inserted as
	// BEGIN/two INSERTs/COMMIT then one more take origins 1, 1, 2.
	originOpen bool
	// contentlessErr is set at load time when the index of a contentless table
	// cannot be read back into documents. Like extErr it is carried rather than
	// raised, so one unreadable table does not fail OpenWrite for the session.
	contentlessErr error
	// clGhosted says a mismatched 'delete' command has left a GHOST in this
	// contentless table: a rowid the index still holds postings for (the terms
	// the given values did not name) with no %_docsize row. That is exactly the
	// state C fts5 leaves (fts5ContentlessGhost, fts5_extcontent.go), and it
	// is written as such; reads serve it (fts5ContentlessRead, and
	// materializeFts5Contentless for which row source sees it). WRITES decline
	// while it stands, in this session and after a reload
	// (fts5LoadContentless sets it again from the file), until a 'delete-all'
	// clears it along with everything else. It is deliberately NOT checked by
	// fts5SyncShadows: the statement that creates the ghost has to be able to
	// write it.
	clGhosted bool
	// extErr is set at load time when this engine cannot serve the
	// external-content table -- the index in the file is not the one this
	// engine would write for the content table's current rows, so rebuilding it
	// from them (which every write here does) would silently change it.
	// fts5_extcontent.go's fts5LoadExternal is the only writer.
	extErr error
	// extBase / extShadows are the load-time snapshot fts5ExtVerify compares:
	// the reconstructed documents as they were BEFORE this statement touched
	// rows, and the shadow bytes the file held then. extVerified records that
	// the comparison has already been made and passed.
	extBase    map[int64][]Value
	extShadows *fts5ExtShadows
	extSource  *fts5ExtRows
	// extUnresolved is every rowid the index holds postings for that the
	// content table has no row for: a document only a 'delete' command can name
	// (fts5_extcontent.go's fts5ExtDeleteCommand).
	extUnresolved map[int64]bool
	extVerified   bool
	// extRowDrift / extColDrift are the gap between C fts5's running totals (the
	// averages record's nRow and per-column token counts, fts5_storage.c's
	// p->nTotalRow / p->aTotalSize) and a fresh recount of the current documents.
	// C moves those totals by what an INSERT or a 'delete' command's given values
	// imply, not by the affected row's real content. This engine re-derives the
	// index from the documents on every write, so it carries the drift instead,
	// which stays fixed across ordinary writes and changes only when a 'delete'
	// command's values do not reproduce the row: seeded at load by fts5ExtVerify,
	// changed by fts5ExtDeleteOrdinary, reset by fts5ExtDiscardIndex
	// ('delete-all'/'rebuild').
	extRowDrift int64
	extColDrift []int64
	columns     []VtabColumn // full declared schema: [rowid HIDDEN, col1, ...]
	rows        map[int64][]Value

	// tokens caches each row's tokenized columns by rowid. fts5SyncShadows
	// rebuilds the whole index after every write, and re-tokenizing every row was
	// the dominant fts5 write cost (N single-row INSERTs were O(N^2) in
	// tokenization). Each mutation drops only the touched rowid (dropTokens). Not
	// copied by vtabClone nor filled by vtabLoadRow: an empty cache is always
	// correct, so it is never a second source of truth.
	tokens map[int64]fts5RowTokens
}

// fts5RowTokens is one row's tokenized columns, parallel to fts5Store.colNames.
type fts5RowTokens struct {
	cols [][]string
	// holes is a contentless GHOST's tombstone mask (fts5IndexDoc.holes); nil
	// for every ordinary document.
	holes [][]bool
}

// ---- vtabStore (read/materialize/persist/clone) ----

func (s *fts5Store) vtabColumns() []VtabColumn { return s.columns }

func (s *fts5Store) vtabRows() (rowids []int64, rows [][]Value) {
	rowids = s.sortedRowids()
	rows = make([][]Value, len(rowids))
	for i, rid := range rowids {
		row := make([]Value, len(s.columns))
		row[0] = Value{Typ: Int, I: rid}
		copy(row[1:], s.rows[rid])
		rows[i] = row
	}
	return rowids, rows
}

func (s *fts5Store) sortedRowids() []int64 {
	rowids := make([]int64, 0, len(s.rows))
	for rid := range s.rows {
		rowids = append(rowids, rid)
	}
	// slices.Sort, not the insertion sort this used to run: rowids come out of
	// a map in random order, so that was O(n^2) on EVERY call -- and
	// fts5SyncShadows calls it once per write statement, making it one of two
	// independent quadratics in the fts5 write path (4.3% of a 1600-insert
	// profile). Ascending int64 order is identical either way.
	slices.Sort(rowids)
	return rowids
}

// dropTokens invalidates the cached tokenization of one row. Every mutation
// calls it; anything it misses costs a re-tokenize, never a wrong answer.
func (s *fts5Store) dropTokens(rowid int64) {
	if s.tokens != nil {
		delete(s.tokens, rowid)
	}
}

func (s *fts5Store) vtabClone() vtabStore {
	cp := *s
	cp.tokens = nil // see fts5Store.tokens: an empty cache is always correct
	if s.contentless {
		// ...except for a CONTENTLESS table, where it is not a cache: st.rows
		// holds NULLs and there is nothing to re-tokenize, so dropping it would
		// empty the index at the next sync (fts5_contentless.go).
		cp.tokens = make(map[int64]fts5RowTokens, len(s.tokens))
		for rid, tk := range s.tokens {
			cols := make([][]string, len(tk.cols))
			for i, c := range tk.cols {
				cols[i] = append([]string(nil), c...)
			}
			var holes [][]bool
			if tk.holes != nil {
				holes = make([][]bool, len(tk.holes))
				for i, h := range tk.holes {
					holes[i] = append([]bool(nil), h...)
				}
			}
			cp.tokens[rid] = fts5RowTokens{cols: cols, holes: holes}
		}
	}
	cp.rows = make(map[int64][]Value, len(s.rows))
	for rid, vals := range s.rows {
		cp.rows[rid] = append([]Value(nil), vals...)
	}
	// The ORIGIN map is state a ROLLBACK has to be able to put back exactly, the
	// same as rows: a transaction that inserted rows and was rolled back must
	// leave the counter where it was, because C fts5 keeps the counter in the
	// structure record and so undoes it with everything else.
	// Locales are row state exactly like the values beside them, so a ROLLBACK
	// has to be able to put them back (fts5_locale.go).
	cp.locales = make(map[int64][]Value, len(s.locales))
	for rid, locs := range s.locales {
		cp.locales[rid] = append([]Value(nil), locs...)
	}
	cp.origins = make(map[int64]int64, len(s.origins))
	maps.Copy(cp.origins, s.origins)
	cp.avgSizes = append([]int64(nil), s.avgSizes...)
	// extColDrift is state a ROLLBACK has to restore too -- exactly like
	// avgSizes above, a bare `cp := *s` shares the slice's backing array, so
	// a later mutation through cp would silently reach back into s.
	cp.extColDrift = append([]int64(nil), s.extColDrift...)
	return &cp
}

// vtabDetach is the interface's own contract (vtab_write.go): the documents and
// their locales are what this store holds, and both came from a read that may
// have aliased a segment file's mapping.
func (s *fts5Store) vtabDetach() {
	for rid, vals := range s.rows {
		s.rows[rid] = ownValues(vals)
	}
	for rid, vals := range s.locales {
		s.locales[rid] = ownValues(vals)
	}
}

func (s *fts5Store) vtabLoadRow(rowid int64, record []Value) {
	if len(record) < 1 {
		return
	}
	// A locale=1 table's record carries its "l<i>" columns after every "c<i>"
	// one, and they are the ONLY locales this engine ever holds (fts5_locale.go).
	body, locs := fts5LocaleSplitRecord(s, record)
	vals := make([]Value, len(body))
	copy(vals, body)
	s.rows[rowid] = vals
	fts5LocaleSet(s, rowid, locs)
	// The store is fresh here (this runs at OpenWrite), so there is nothing to
	// invalidate -- but this is the one other place s.rows is written without
	// going through InsertRow/UpdateRow/DeleteRow, and leaving it out would
	// make the cache's invariant depend on that staying true.
	s.dropTokens(rowid)
}

// ---- VirtualTable (read cursor) ----

func (s *fts5Store) BestIndex(info *VtabIndexInfo) error { return nil }

func (s *fts5Store) Open() (VtabCursor, error) {
	rowids, rows := s.vtabRows()
	return &fts5Cursor{rowids: rowids, rows: rows}, nil
}

type fts5Cursor struct {
	rowids []int64
	rows   [][]Value
	pos    int
}

func (c *fts5Cursor) Filter(int, string, []Value) error { c.pos = 0; return nil }
func (c *fts5Cursor) Next() error                       { c.pos++; return nil }
func (c *fts5Cursor) Eof() bool                         { return c.pos >= len(c.rows) }
func (c *fts5Cursor) Column(i int) (Value, error)       { return c.rows[c.pos][i], nil }
func (c *fts5Cursor) Rowid() (int64, error)             { return c.rowids[c.pos], nil }
func (c *fts5Cursor) Close() error                      { return nil }

// ---- VtabUpdater (writes) ----

func (s *fts5Store) InsertRow(vals []Value) (int64, error) {
	if len(vals) != len(s.columns) {
		return 0, fmt.Errorf("fts5: internal: %d values for %d columns", len(vals), len(s.columns))
	}
	// An fts5_locale() value carries its own TEXT plus the locale to store
	// beside it (fts5_locale.go). Unwrapped BEFORE anything is stored or
	// tokenized, which is where fts5UpdateMethod does it too -- the locale=0
	// rejection it raises has to precede every effect of the statement.
	vals, locs, lerr := fts5LocaleUnwrapRow(s, vals)
	if lerr != nil {
		return 0, lerr
	}
	var rowid int64
	if vals[0].Typ == Null {
		rowid = s.nextRowid()
	} else {
		rowid = fts5RowidValue(vals[0])
	}
	if _, exists := s.rows[rowid]; exists {
		if s.contentless {
			// Same as external content below, for the same reason: a
			// contentless table has no %_content to reject the duplicate
			// either, so fts5UpdateMethod takes eConflict=SQLITE_ABORT
			// ("eContent==FTS5_CONTENT_NORMAL || bContentlessDelete") and
			// simply indexes the row again at that rowid, leaving the index
			// holding two documents' postings under one -- and %_docsize
			// holding only the SECOND one's sizes, so the two disagree from
			// then on. This engine holds one document per rowid.
			return 0, fmt.Errorf("engine: fts5: inserting a second row at rowid %d of the contentless table %s is not supported by this engine (C fts5 has no %%_content to reject the duplicate, and merges both documents' postings under that one rowid)", rowid, s.name)
		}
		if s.extContent != "" {
			// An EXTERNAL-CONTENT table has no %_content and therefore no rowid
			// uniqueness to violate: fts5UpdateMethod takes eConflict=SQLITE_ABORT
			// for it and then simply indexes the row AGAIN at the same rowid, so
			// the index ends up holding two documents' postings under one. This
			// engine holds one document per rowid and cannot express that.
			return 0, fmt.Errorf("engine: fts5: inserting a second row at rowid %d of the external-content table %s is not supported by this engine (C fts5 has no %%_content to reject the duplicate, and merges both documents' postings under that one rowid)", rowid, s.name)
		}
		// The %_content table's rowid IS its declared PRIMARY KEY for a
		// NORMAL-content table, so this INSERT's %_content step
		// (sqlite3Fts5StorageContentInsert, fts5_storage.c) hits a real
		// SQLITE_CONSTRAINT -- unlike the two declined shapes above, which
		// have no %_content at all to raise one. OE_Ignore-eligible.
		return 0, vtabConstraint(fmt.Errorf("UNIQUE constraint failed: %s.rowid", s.name))
	}
	if s.contentless {
		// A CONTENTLESS table INDEXES the values given and STORES nothing
		// (fts5_main.c's fts5UpdateMethod reaches fts5StorageInsert, whose
		// sqlite3Fts5StorageContentInsert is a no-op for FTS5_CONTENT_NONE). So
		// the row's tokens are taken now, while the values are in hand, and the
		// row itself becomes the NULLs every later read of it returns.
		if s.contentlessErr != nil {
			return 0, s.contentlessErr
		}
		if s.clGhosted {
			return 0, fts5GhostedErr(s.name)
		}
		// ...except under contentless_unindexed=1, where the UNINDEXED columns
		// really are stored -- in a %_content holding only them
		// (fts5_contentless.go). Every other column stays the NULL a read of a
		// contentless table returns.
		s.setContentlessRow(rowid, vals)
		s.setTokens(rowid, vals[1:])
		return rowid, nil
	}
	s.rows[rowid] = append([]Value(nil), vals[1:]...)
	s.dropTokens(rowid)
	// locs is nil for a row with no fts5_locale() value in any column, and
	// fts5LocaleSet then CLEARS the rowid's entry rather than leaving a stale
	// one. That matters because the one path that reuses a live rowid without
	// going through DeleteRow first (INSERT OR REPLACE straight onto it) goes
	// through InsertRowReplace below, which calls DeleteRow explicitly first.
	fts5LocaleSet(s, rowid, locs)
	return rowid, nil
}

// replaceForgiven reports whether this table's own xUpdate grants OR REPLACE
// forgiveness for a rowid collision on INSERT -- fts5_main.c:2003-2005's
// eConflict computation, ported exactly: "pConfig->eContent==
// FTS5_CONTENT_NORMAL || pConfig->bContentlessDelete". A DIFFERENT (WIDER)
// gate than OE_Ignore's own eligibility check (fts5_main.c:445,
// eContent==FTS5_CONTENT_NORMAL only -- already ported as this store's
// vtabConstraint wrapping in InsertRow/updateRowMasked above) -- the two must
// stay independent; do not merge them.
func (s *fts5Store) replaceForgiven() bool {
	return (!s.contentless && s.extContent == "") || s.contentlessDelete
}

// InsertRowReplace is InsertRow's OR REPLACE counterpart, called by
// vtabInsertRowOne when replaceForgiven() holds and a rowid was given:
// fts5UpdateMethod's INSERT branch, "if( eConflict==SQLITE_REPLACE &&
// eType1==SQLITE_INTEGER ){ ... sqlite3Fts5StorageDelete(iNew) ... }
// fts5StorageInsert(...)" (fts5_main.c:2047-2051). An auto-assigned rowid
// cannot collide. removed reports whether a row was displaced, for
// secure-delete bookkeeping; DeleteRow already handles every replaceForgiven()
// table.
func (s *fts5Store) InsertRowReplace(vals []Value) (rowid int64, removed bool, err error) {
	if !s.replaceForgiven() || vals[0].Typ == Null {
		rowid, err = s.InsertRow(vals)
		return rowid, false, err
	}
	target := fts5RowidValue(vals[0])
	if _, exists := s.rows[target]; exists {
		if derr := s.DeleteRow(target); derr != nil {
			return 0, false, derr
		}
		removed = true
	}
	rowid, err = s.InsertRow(vals)
	return rowid, removed, err
}

// setTokens records one row's DOCUMENT: the tokenization of vals, which for a
// contentless table is the only copy of it there will ever be.
func (s *fts5Store) setTokens(rowid int64, vals []Value) {
	cols := make([][]string, len(s.colNames))
	for i := range cols {
		if s.unindexed[i] {
			continue
		}
		var v Value
		if i < len(vals) {
			v = vals[i]
		}
		cols[i] = s.tok.tokenize(valueToText(v))
	}
	if s.tokens == nil {
		s.tokens = map[int64]fts5RowTokens{}
	}
	s.tokens[rowid] = fts5RowTokens{cols: cols}
}

// UpdateRow is the maskless VtabUpdater entry point: vals is the whole new
// row, so every column counts as WRITTEN -- which for a locale=1 table means
// none of the old row's locales is carried over (fts5_locale.go). Every UPDATE
// this engine compiles arrives through UpdateRowSet, which does have the mask.
func (s *fts5Store) UpdateRow(oldRowid int64, vals []Value) (int64, error) {
	return s.updateRowMasked(oldRowid, vals, fts5AllModified(len(s.columns)))
}

// fts5AllModified is the SET-list mask of an update that named every column.
func fts5AllModified(n int) []bool {
	m := make([]bool, n)
	for i := range m {
		m[i] = true
	}
	return m
}

func (s *fts5Store) updateRowMasked(oldRowid int64, vals []Value, modified []bool) (int64, error) {
	if len(vals) != len(s.columns) {
		return 0, fmt.Errorf("fts5: internal: %d values for %d columns", len(vals), len(s.columns))
	}
	vals, given, lerr := fts5LocaleUnwrapRow(s, vals)
	if lerr != nil {
		return 0, lerr
	}
	if s.contentless {
		// A contentless UPDATE's outcome depends on WHICH columns the SET list
		// named, which UpdateRow is not told -- UpdateRowSet is (fts5's own
		// sqlite3_value_nochange). Reaching here means the caller had no mask
		// to give, which no write path in this engine does.
		return 0, fmt.Errorf("engine: internal error: fts5: UPDATE of the contentless table %s reached the maskless write path", s.name)
	}
	newRowid := oldRowid
	if vals[0].Typ != Null {
		newRowid = fts5RowidValue(vals[0])
	}
	if newRowid != oldRowid {
		if _, exists := s.rows[newRowid]; exists {
			// fts5UpdateMethod's iOld!=iNew non-REPLACE arm inserts into
			// %_content at the new rowid (sqlite3Fts5StorageContentInsert,
			// bReplace=0), a real SQLITE_CONSTRAINT when that rowid is taken, but
			// only for a table with a %_content (default content).
			//
			// OR IGNORE eligibility is narrower: bConstraint is declared only for
			// FTS5_CONTENT_NORMAL (fts5_main.c:445), so an external-content table
			// is excluded too. "UPDATE ft SET rowid=13 WHERE rowid=14" onto an
			// existing rowid of a content=tbl table raises no error in C (its rows
			// live in the user's table). Such a table must not be wrapped in
			// vtabConstraint; its own over-eager "UNIQUE constraint failed" decline
			// is a separate gap.
			if s.extContent == "" {
				return 0, vtabConstraint(fmt.Errorf("UNIQUE constraint failed: %s.rowid", s.name))
			}
			return 0, fmt.Errorf("UNIQUE constraint failed: %s.rowid", s.name)
		}
	}
	// The surviving locales are settled BEFORE the old row is dropped: they are
	// read off it (fts5_locale.go's fts5LocaleAfterUpdate), and then the ones
	// this statement GAVE -- a SET list naming a column with an fts5_locale()
	// value -- are laid over them (fts5_storage.c:999).
	locs := fts5LocaleMerge(fts5LocaleAfterUpdate(s, oldRowid, modified), given)
	delete(s.rows, oldRowid)
	s.dropTokens(oldRowid)
	fts5LocaleSet(s, oldRowid, nil)
	s.rows[newRowid] = append([]Value(nil), vals[1:]...)
	s.dropTokens(newRowid)
	fts5LocaleSet(s, newRowid, locs)
	return newRowid, nil
}

// updateReplaceForgiven is UPDATE's narrower slice of replaceForgiven. C's
// REPLACE branch for a rowid-changing UPDATE (fts5_main.c:2072-2077) also
// covers contentless_delete=1 tables, but updateRowMasked refuses every
// contentless table, and that path (UpdateRowSet's bSeenIndexNC==0 branch) is
// unmeasured, so the combination stays declined through UpdateRowSet's
// collision check.
func (s *fts5Store) updateReplaceForgiven() bool {
	return !s.contentless && s.extContent == ""
}

// UpdateRowReplace is updateRowMasked's OR REPLACE counterpart, called by
// vtabUpdateRowConflict when updateReplaceForgiven() holds: fts5UpdateMethod's
// iOld!=iNew REPLACE branch (fts5_main.c:2072-2077), "delete iOld (bSaveRow=1),
// delete iNew if present, insert". An unchanged rowid cannot collide and falls
// through to updateRowMasked, so callers need not check. updateRowMasked
// already deletes oldRowid first, so deleting the target beforehand reaches
// C's final state (disjoint rowids). removed reports a displaced row, for
// secure-delete bookkeeping.
func (s *fts5Store) UpdateRowReplace(oldRowid int64, vals []Value, modified []bool) (newRowid int64, removed bool, err error) {
	target := oldRowid
	if vals[0].Typ != Null {
		target = fts5RowidValue(vals[0])
	}
	if target != oldRowid {
		if _, exists := s.rows[target]; exists {
			if derr := s.DeleteRow(target); derr != nil {
				return 0, false, derr
			}
			removed = true
		}
	}
	newRowid, err = s.updateRowMasked(oldRowid, vals, modified)
	return newRowid, removed, err
}

// UpdateRowSet is UpdateRow with the SET list's mask (vtabSetListUpdater),
// which only a contentless table's UPDATE rule needs. A port of
// fts5ContentlessUpdate, which decides from two counts over the indexed
// columns, how many the SET list named (bSeenIndex) and how many it did not
// (bSeenIndexNC):
//
//	bSeenIndex == 0 and the rowid unchanged
//	    a %_content-only write; the index (and, with contentless_delete=1,
//	    the origin) is untouched. Without a %_content (every contentless
//	    table but contentless_unindexed=1) that is a successful no-op
//	    (sqlite3Fts5StorageContentInsert returns at once for
//	    FTS5_CONTENT_NONE): "UPDATE ft1 SET b=b" over an UNINDEXED b.
//	bSeenIndexNC == 0 and contentless_delete=1
//	    a delete and re-insert of a fully specified document; the row takes
//	    a fresh origin even with the same rowid (%_docsize's origin moved 1
//	    to 2).
//	otherwise
//	    fts5's own error, in its two wordings.
func (s *fts5Store) UpdateRowSet(oldRowid int64, vals []Value, modified []bool) (int64, error) {
	if !s.contentless {
		return s.updateRowMasked(oldRowid, vals, modified)
	}
	if len(vals) != len(s.columns) {
		return 0, fmt.Errorf("fts5: internal: %d values for %d columns", len(vals), len(s.columns))
	}
	// A contentless table has no %_content, so it has no l<i> column to put a
	// locale in: the value is unwrapped to the text that gets indexed and the
	// locale dropped. The locale=0 rejection still applies (fts5_locale.go).
	vals, _, lerr := fts5LocaleUnwrapRow(s, vals)
	if lerr != nil {
		return 0, lerr
	}
	if s.contentlessErr != nil {
		return 0, s.contentlessErr
	}
	if s.contentless && s.clGhosted {
		return 0, fts5GhostedErr(s.name)
	}
	newRowid := oldRowid
	if vals[0].Typ != Null {
		newRowid = fts5RowidValue(vals[0])
	}
	seenIndex, seenIndexNC := 0, 0
	for i := range s.colNames {
		if s.unindexed[i] {
			continue
		}
		// modified is indexed by DECLARED column, whose slot 0 is the hidden
		// rowid, so fts5 column i is slot i+1.
		if i+1 < len(modified) && modified[i+1] {
			seenIndex++
		} else {
			seenIndexNC++
		}
	}
	if seenIndex == 0 && newRowid == oldRowid {
		s.setContentlessRow(oldRowid, vals)
		return oldRowid, nil
	}
	if seenIndexNC != 0 || !s.contentlessDelete {
		if s.contentlessDelete {
			return 0, fmt.Errorf("cannot UPDATE a subset of columns on fts5 contentless-delete table: %s", s.name)
		}
		return 0, fmt.Errorf("cannot UPDATE contentless fts5 table: %s", s.name)
	}
	if newRowid != oldRowid {
		if _, exists := s.rows[newRowid]; exists {
			// C fts5 answers "constraint failed" and leaves the table untouched.
			// Not wrapped in vtabConstraint: contentless tables never get
			// SQLITE_VTAB_CONSTRAINT_SUPPORT (fts5_main.c:445), so "UPDATE OR
			// IGNORE" over this collision still raises the same error in C.
			return 0, fmt.Errorf("constraint failed")
		}
	}
	delete(s.rows, oldRowid)
	s.dropTokens(oldRowid)
	delete(s.origins, oldRowid)
	delete(s.origins, newRowid)
	s.setContentlessRow(newRowid, vals)
	s.setTokens(newRowid, vals[1:])
	return newRowid, nil
}

// setContentlessRow stores what a contentless table keeps of a row: NULLs for
// every column, with the UNINDEXED ones overwritten from vals when the table is
// contentless_unindexed=1 (fts5_contentless.go). vals is the full declared row,
// rowid first.
func (s *fts5Store) setContentlessRow(rowid int64, vals []Value) {
	row := fts5ContentlessRow(len(s.colNames))
	if s.contentUnindexed {
		for i := range s.colNames {
			if s.unindexed[i] && i+1 < len(vals) {
				row[i] = vals[i+1]
			}
		}
	}
	s.rows[rowid] = row
}

func (s *fts5Store) DeleteRow(rowid int64) error {
	if s.contentless && !s.contentlessDelete {
		// fts5_main.c's fts5UpdateMethod: "It is only possible to DELETE from a
		// contentless table if the contentless_delete=1 flag is set"
		// (fts5_contentless.go).
		return fmt.Errorf("cannot DELETE from contentless fts5 table: %s", s.name)
	}
	delete(s.rows, rowid)
	s.dropTokens(rowid)
	delete(s.origins, rowid)
	fts5LocaleSet(s, rowid, nil)
	return nil
}

func (s *fts5Store) nextRowid() int64 {
	var max int64
	found := false
	for rid := range s.rows {
		if !found || rid > max {
			max, found = rid, true
		}
	}
	if !found {
		return 1
	}
	return max + 1
}

// fts5RowidValue coerces a rowid input value to int64 (sqlite3_value_int64
// semantics), mirroring rtreeInt.
func fts5RowidValue(v Value) int64 {
	switch v.Typ {
	case Int:
		return v.I
	case Float:
		return int64(v.F)
	case Text, Blob:
		if isF, i, f, ok := parseNumericPrefix(string(v.S)); ok {
			if isF {
				return int64(f)
			}
			return i
		}
	}
	return 0
}
