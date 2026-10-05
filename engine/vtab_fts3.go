// The "fts3" and "fts4" full-text virtual-table modules (ext/fts3), separate
// from fts5 (vtab_fts5.go): they differ in query language, auxiliary functions
// and, above all, shadow-table layout.
//
// An fts3/fts4 table keeps nothing under its own rootpage (its schema row has
// rootpage 0); its data lives in ordinary tables:
//
//	CREATE TABLE 't_content'(docid INTEGER PRIMARY KEY, 'c0a', 'c1b')
//	CREATE TABLE 't_segments'(blockid INTEGER PRIMARY KEY, block BLOB)
//	CREATE TABLE 't_segdir'(level INTEGER,idx INTEGER,start_block INTEGER,leaves_end_block INTEGER,end_block INTEGER,root BLOB,PRIMARY KEY(level, idx))
//	CREATE TABLE 't_docsize'(docid INTEGER PRIMARY KEY, size BLOB)   -- fts4 only
//	CREATE TABLE 't_stat'(id INTEGER PRIMARY KEY, value BLOB)        -- fts4 only
//
// (verbatim, single-quoted identifiers included; see asCreateTableIdent).
// Those tables are the file format, so every byte is derived from C and gated
// both ways by compat-harness/fts3_shadow_test.go.
//
// Where things live:
//   - CREATE/DROP, options and the read row source: this file
//     (notindexed=, matchinfo=fts3, prefix= in fts3_prefix.go, languageid= in
//     fts3_langid.go, content= in fts3_content.go, order=, compress=/
//     uncompress= via fts3ContentCodec).
//   - INSERT/REPLACE (fts3 resolves REPLACE itself) below; DELETE and UPDATE,
//     recorded as delete markers in new segments, in fts3_write.go.
//   - segment encoding in fts3_index.go; transactions and pending terms in
//     fts3_txn.go; automerge in fts3_automerge.go.
//   - MATCH, answered from the segment index (fts3_query.go parses the query
//     language, fts3_search.go evaluates it), and offsets()/snippet()/
//     matchinfo() (fts3_search.go, fts3_snippet.go).
//   - the command channel, "INSERT INTO t(t) VALUES('optimize')"
//     (fts3_command.go).
//
// Each file states what it still declines.
package engine

import (
	"slices"
	"fmt"
	"strings"
)

// fts3MergeCount is FTS3_MERGE_COUNT: C fts3 merges level-N segments down
// into a single level-(N+1) segment as soon as allocating another index at
// level N would reach this many. Verified against the oracle: 20 separate
// single-row INSERTs leave one level-1 segment plus level-0 segments idx 0..3,
// i.e. the 17th INSERT merged the first 16 and then restarted at idx 0.
const fts3MergeCount = 16

// fts3NodeOverhead is the number of bytes C fts3 subtracts from the page
// size to get its segment node size limit (p->nNodeSize = pgsz - 35).
const fts3NodeOverhead = 35

// fts3Module is the fts3/fts4 VtabModule. isFts4 distinguishes the two: fts4
// parses module options at all, accepts the '^' query modifier, and by default
// maintains the %_docsize and %_stat shadow tables that fts3 does not (verified
// -- an fts3 table's schema holds exactly three shadow tables).
//
// "By default", because "matchinfo=fts3" turns %_docsize back off for one fts4
// table: whether a table HAS %_docsize is therefore a property of the table
// (fts3Schema.hasDocsize), not of the module.
type fts3Module struct {
	name   string // "fts3" or "fts4", for error text
	isFts4 bool
}

func init() {
	RegisterVtabModule("fts3", fts3Module{name: "fts3"})
	RegisterVtabModule("fts4", fts3Module{name: "fts4", isFts4: true})
}

// Connect satisfies VtabModule: it validates the module arguments and declares
// the column schema. The declared columns are [docid HIDDEN INTEGER, col0,
// ...]: the leading hidden column carries the docid (so "SELECT docid FROM t"
// resolves, while "SELECT *" returns only the text columns, matching real
// fts3), and "rowid" is served by the engine's ordinary rowid mechanism, which
// for this table is the %_content rowid -- the same integer.
func (m fts3Module) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	return m.ConnectIn(nil, args)
}

// ConnectIn is Connect with the catalog an external-content table that
// declares no columns of its own needs (fts3_content.go). Every caller holding
// a database passes one, through vtabConnect (vtab.go).
func (m fts3Module) ConnectIn(cat fts3Catalog, args []string) ([]VtabColumn, VirtualTable, error) {
	sch, err := m.parseSchemaWith(args, cat)
	if err != nil {
		return nil, nil, err
	}
	return fts3DeclaredColumns(sch), fts3Table{}, nil
}

// fts3ReturningScopeRow reshapes an fts3 staging row into the order a
// RETURNING clause resolves against. The staging row is [docid, col0, ...,
// command, langid] while the declared list is [docid, col0, ..., langid] --
// the command channel is not a declared column here (fts3DeclaredColumns) --
// so the language slot moves down one and the command slot is dropped.
func fts3ReturningScopeRow(sch fts3Schema, nCol int, full []Value) []Value {
	row := make([]Value, 0, nCol+2)
	row = append(row, full[:nCol+1]...) // docid, then every user column
	if sch.langid != "" {
		row = append(row, full[fts3LangidTargetSlot(nCol)])
	}
	return row
}

// fts3DeclaredColumns is an fts3/fts4 table's declared column list: hidden
// docid, the user columns, then a "languageid=" table's hidden language column.
//
// That is not C's order: fts3DeclareVtab (fts3.c:651-655) writes "CREATE TABLE
// x(<cols>, <tablename> HIDDEN, docid HIDDEN, <langid> HIDDEN)", with a command
// column this engine models separately. The read path depends on this order;
// PRAGMA table_xinfo maps onto C's (vtabDeclaredColumnsAsSQLiteReportsThem).
// For name resolution only the names and hidden flags matter, and they agree.
func fts3DeclaredColumns(sch fts3Schema) []VtabColumn {
	cols := make([]VtabColumn, 0, len(sch.cols)+2)
	cols = append(cols, VtabColumn{Name: "docid", Type: "INTEGER", Hidden: true})
	for _, n := range sch.cols {
		cols = append(cols, VtabColumn{Name: n})
	}
	if sch.langid != "" {
		// "languageid=" adds a hidden column last, lining up with %_content's
		// trailing langid column (materializeFts3, fts3_langid.go).
		//
		// NumericAffinity: fts3DeclareVtab declares it bare, "%Q HIDDEN"
		// (fts3.c:653-654), which C gives NUMERIC affinity
		// (VtabColumn.NumericAffinity). Without it "SELECT count(*) FROM t1
		// WHERE l='0'" on "fts4(a,b,languageid=l)" lost its row (fts4aux's
		// languageid column has the same flag, vtab_fts3aux.go).
		cols = append(cols, VtabColumn{Name: sch.langid, Hidden: true, NumericAffinity: true})
	}
	return cols
}

// fts3Table is the VirtualTable Connect hands back. It is never actually
// scanned: an fts3 table's rows live in its %_content shadow table, and
// materializeVtab routes to materializeFts3 (below) before ever opening a
// cursor. It exists only so Connect can satisfy the VtabModule contract.
type fts3Table struct{}

func (fts3Table) BestIndex(info *VtabIndexInfo) error { return nil }
func (fts3Table) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("engine: internal error: an fts3/fts4 table is read through its %%_content shadow table, not a cursor")
}

// fts3Schema is one fts3/fts4 table's declared schema, re-derived from its
// stored CREATE VIRTUAL TABLE arguments: the column names, which of them the
// "notindexed=" module option keeps out of the index, and whether the table has
// a %_docsize shadow table ("matchinfo=fts3" is an fts4 table without one). The
// module keeps no per-table state, so this is the single source of truth for
// both paths.
type fts3Schema struct {
	cols       []string
	notindexed []bool
	hasDocsize bool
	// prefixes is one byte length per PREFIX INDEX, in declaration order
	// (fts3_prefix.go). Empty for a table with no "prefix=" option, which is
	// the only shape with a single index.
	prefixes []int
	// tok is the tokenize= specification, nil for the default "simple" one
	// (fts3_tokenizer.go).
	tok *fts3Tokenizer
	// langid is the DECLARED name of the "languageid=" hidden column, "" for a
	// table without the option. %_content's own trailing column is literally
	// "langid" whatever this says (fts3_langid.go).
	langid string
	// content is the "content=" option's value and hasContent whether it was
	// given at all -- the two together are C fts3's single zContentTbl
	// pointer, which is NULL for an ordinary table, the empty string for a
	// CONTENTLESS one and a table name for an EXTERNAL-CONTENT one
	// (fts3_content.go).
	content    string
	hasContent bool
	// compress/uncompress are the "compress="/"uncompress=" values: SQL functions
	// C fts3 applies to every stored %_content column on the way in and out
	// (fts3ReadExprList/fts3WriteExprList, fts3.c:867-955), run here through
	// fts3ContentCodec. CREATE requires them as a pair (fts3.c:1495-1500,
	// "missing %s parameter in fts4 constructor"), and a "content=" table discards
	// them (fts3.c:1358-1367); parseSchemaWith reproduces both.
	//
	// compressGiven/uncompressGiven record whether the option key appeared at all:
	// C's pairing check, "(zCompress==0)!=(zUncompress==0)" (fts3.c:1495), tests
	// pointer NULLness, so "compress=''" counts as given.
	compress, uncompress           string
	compressGiven, uncompressGiven bool
	// descIdx is fts3Table.bDescIdx: "order=desc". See fts3OrderOption for the
	// three things it changes and fts3_index.go for the doclist encoding.
	descIdx bool
}

// hasCompress reports whether compress=/uncompress= is live on this table --
// "live" because a "content=" table's parseSchemaWith already cleared both
// fields (fts3.c:1358-1367), so no separate hasContent check is needed here.
func (s fts3Schema) hasCompress() bool { return s.compressGiven || s.uncompressGiven }

// fts3ContentCodec is one of the two functions a compress= table runs its
// %_content values through. Real fts3 never reads or writes a column value
// plainly there: every read is "SELECT docid, unzip(x.'c0a'), ..."
// (fts3ReadExprList) and every write "INSERT INTO %_content VALUES(?, zip(?),
// ...)" (fts3WriteExprList, fts3.c:867-955), with the name quoted as an
// identifier and the docid and language columns left alone. So a value is
// transformed by COMPILING that call -- "SELECT "fn"(?1)" -- and running it,
// which raises "no such function" or "wrong number of arguments" exactly where
// the prepare does, and a function the engine has is the same builtin SQLite
// resolves by that name.
type fts3ContentCodec struct{ prog *Program }

// newFts3ContentCodec compiles fn's call; nil (with no error) when the table
// has no compress= option, so every caller can pass values through a nil codec.
func newFts3ContentCodec(sch fts3Schema, fn string) (*fts3ContentCodec, error) {
	if !sch.hasCompress() {
		return nil, nil
	}
	call := FuncExpr{Name: fn, Args: []Expr{ParamExpr{Index: 1}}}
	prog, err := compileSelectNoFromPager(nil, &SelectStmt{Columns: []SelectColumn{{Expr: call}}}, nil)
	if err != nil {
		return nil, err
	}
	return &fts3ContentCodec{prog: prog}, nil
}

// row returns a copy of a %_content row -- slot 0 the docid, then the user
// columns, then the language id -- with every user column passed through the
// codec.
func (cc *fts3ContentCodec) row(sch fts3Schema, rec []Value) ([]Value, error) {
	if cc == nil {
		return rec, nil
	}
	out := slices.Clone(rec)
	for c := 1; c <= len(sch.cols) && c < len(out); c++ {
		rows, err := cc.prog.exec(nil, []Value{out[c]})
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			return nil, fmt.Errorf("%w: fts4 compress= function returned no value", errVDBEUnsupported)
		}
		out[c] = rows[0][0]
	}
	return out, nil
}

// nIndex is fts3Table.nIndex: the number of separate indexes one language of
// this table owns -- the term index plus one per "prefix=" length. It is the
// multiplier in getAbsoluteLevel, so it decides where a language's segments
// live (fts3_langid.go).
func (s fts3Schema) nIndex() int { return len(s.prefixes) + 1 }

// indexed reports whether column c is tokenized into the index. A notindexed
// column is stored in %_content and read back normally; it simply contributes
// no terms, no %_docsize token count and no %_stat bytes.
func (s fts3Schema) indexed(c int) bool { return c >= len(s.notindexed) || !s.notindexed[c] }

// parseSchemaWith parses the module arguments, with the catalog a "content="
// table needs: an
// external-content table that declares no columns of its own borrows the
// content table's, which only a caller holding the database can look up
// (fts3_content.go). cat may be nil, in which case exactly that one shape
// errors -- every other table parses without touching the catalog.
func (m fts3Module) parseSchemaWith(args []string, cat fts3Catalog) (fts3Schema, error) {
	var names []string
	var notindexed []string
	seen := map[string]bool{}
	haveTokenizer := false
	hasDocsize := m.isFts4
	var prefixes []int
	var tok *fts3Tokenizer
	langid := ""
	descIdx := false
	content, hasContent := "", false
	compress, uncompress := "", ""
	compressGiven, uncompressGiven := false, false
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !haveTokenizer && fts3IsTokenizerArg(a) {
			tk, terr := m.parseTokenizer(a)
			if terr != nil {
				return fts3Schema{}, terr
			}
			tok = tk
			haveTokenizer = true
			continue
		}
		if m.isFts4 {
			if key, ok := fts3SpecialColumnKey(a); ok {
				if key == "notindexed" || strings.EqualFold(key, "notindexed") {
					val, ok := fts3DequoteArg(strings.TrimPrefix(a, key+"="))
					if !ok {
						return fts3Schema{}, fmt.Errorf("%s: module argument %q is not a usable column name", m.name, a)
					}
					notindexed = append(notindexed, val)
					continue
				}
				if strings.EqualFold(key, "prefix") {
					// NOT trimmed: fts3IsSpecialColumn takes everything after
					// the first '=' and only dequotes it, so "prefix= 2" is
					// "error parsing prefix parameter:  2" against the oracle
					// -- fts3GobbleInt's first entry has no digits. Accepting
					// it here created a table C SQLite refuses.
					val, ok := fts3DequoteArg(strings.TrimPrefix(a, key+"="))
					if !ok {
						return fts3Schema{}, fmt.Errorf("%s: error parsing prefix parameter: %s", m.name, strings.TrimPrefix(a, key+"="))
					}
					pfx, perr := fts3PrefixParameter(val)
					if perr != nil {
						return fts3Schema{}, fmt.Errorf("%s: %w", m.name, perr)
					}
					// A repeated "prefix=" simply replaces the previous list,
					// as fts3InitVtab's last-one-wins does.
					prefixes = pfx
					continue
				}
				if strings.EqualFold(key, "matchinfo") {
					noDocsize, merr := fts3MatchinfoOption(strings.TrimPrefix(a, key+"="))
					if merr != nil {
						return fts3Schema{}, merr
					}
					hasDocsize = !noDocsize
					continue
				}
				if strings.EqualFold(key, "order") {
					desc, oerr := fts3OrderOption(strings.TrimPrefix(a, key+"="))
					if oerr != nil {
						return fts3Schema{}, fmt.Errorf("%s: %w", m.name, oerr)
					}
					// LAST ONE WINS, as fts3InitVtab's own unconditional
					// assignment does: "order=desc, order=asc" is ASC
					// (verified -- its docids come back ascending), and BOTH
					// occurrences are still validated, so "order=xyz,
					// order=asc" fails the CREATE.
					descIdx = desc
					continue
				}
				if strings.EqualFold(key, "languageid") {
					// fts3IsSpecialColumn takes everything after the FIRST '='
					// and dequotes it, with NO trimming: "languageid= l"
					// declares a hidden column literally named " l" -- verified
					// against the oracle, where "PRAGMA table_xinfo" reports
					// that name and it must be quoted to be selected.
					val, ok := fts3DequoteArg(strings.TrimPrefix(a, key+"="))
					if !ok {
						return fts3Schema{}, fmt.Errorf("%s: module argument %q is not a usable column name", m.name, a)
					}
					// LAST ONE WINS, as fts3InitVtab's own
					// "sqlite3_free(zLanguageid); zLanguageid = zVal" does --
					// verified: "languageid=x, languageid=y" leaves y.
					langid = val
					continue
				}
				if strings.EqualFold(key, "content") {
					// Same rule as every other option: dequoted, NOT trimmed.
					// LAST ONE WINS, as fts3InitVtab's own free-and-assign does.
					val, ok := fts3DequoteArg(strings.TrimPrefix(a, key+"="))
					if !ok {
						return fts3Schema{}, fmt.Errorf("%s: module argument %q is not a usable table name", m.name, a)
					}
					content, hasContent = val, true
					continue
				}
				if strings.EqualFold(key, "compress") {
					// Dequoted, NOT trimmed, LAST ONE WINS -- the same rule
					// content=/languageid= above follow, and the same
					// sqlite3_free-then-assign fts3.c's COMPRESS case applies
					// (fts3.c:1298-1301). The function is not resolved, let
					// alone called, here: C fts3 doesn't either (it only
					// builds the SQL text zWriteExprlist embeds it into,
					// fts3WriteExprList) -- see hasCompress's comment for why
					// this engine never gets further than storing the name.
					val, ok := fts3DequoteArg(strings.TrimPrefix(a, key+"="))
					if !ok {
						return fts3Schema{}, fmt.Errorf("%s: module argument %q is not a usable function name", m.name, a)
					}
					compress, compressGiven = val, true
					continue
				}
				if strings.EqualFold(key, "uncompress") {
					// Same rule, mirroring fts3.c's UNCOMPRESS case
					// (fts3.c:1306-1307).
					val, ok := fts3DequoteArg(strings.TrimPrefix(a, key+"="))
					if !ok {
						return fts3Schema{}, fmt.Errorf("%s: module argument %q is not a usable function name", m.name, a)
					}
					uncompress, uncompressGiven = val, true
					continue
				}
				if err := m.declineModuleOption(a); err != nil {
					return fts3Schema{}, err
				}
				continue
			}
		}
		name, err := m.parseColumnName(a)
		if err != nil {
			return fts3Schema{}, err
		}
		low := r33sFoldIdent(name)
		// Real fts3 rejects both of these at CREATE time ("vtable constructor
		// failed"), verified against the oracle: a duplicate column name, and
		// a column named "docid" (which would collide with %_content's own
		// primary key).
		if seen[low] {
			return fts3Schema{}, fmt.Errorf("%s: duplicate column name: %s", m.name, name)
		}
		if low == "docid" {
			return fts3Schema{}, fmt.Errorf("%s: reserved fts3 column name: %s", m.name, name)
		}
		seen[low] = true
		names = append(names, name)
	}
	if hasContent {
		// Rule 1 of fts3.c's own comment above its content-handling block
		// (fts3.c:1358-1367): "If a content=xxx option was specified... [1.]
		// Ignore any compress= and uncompress= options." Unconditional and
		// order-independent -- the C parses every argument first (the loop
		// above) and only afterward, once, clears zCompress/zUncompress if
		// zContent was set, so "fts4(compress=zip, content=t1)" discards
		// compress= exactly like "fts4(content=t1, compress=zip)" does.
		compress, uncompress = "", ""
		compressGiven, uncompressGiven = false, false
	}
	if compressGiven != uncompressGiven {
		// fts3.c:1495-1500: exactly one of the two given is an error, checked
		// AFTER the content= clearing above (so a content= table can never
		// reach this, whatever it also wrote) and BEFORE fts3CreateTables, so
		// this fails the CREATE with no shadow table ever made -- verified: an
		// unpaired "compress=zip" alone reports "fts4: missing uncompress
		// parameter in fts4 constructor" and creates nothing. Gated on
		// compressGiven/uncompressGiven (whether the KEY was seen), not on the
		// value being non-empty: "compress=''" alone must still report
		// "missing uncompress", not silently pass because the empty value
		// looks the same as "never given".
		zMiss := "compress"
		if compressGiven {
			zMiss = "uncompress"
		}
		return fts3Schema{}, fmt.Errorf("%s: missing %s parameter in fts4 constructor", m.name, zMiss)
	}
	if hasContent && len(names) == 0 {
		// "If no column names were specified as part of the CREATE VIRTUAL
		// TABLE statement, use all columns from the content table" -- and
		// fts3ContentColumns gets them from "SELECT * FROM %Q.%Q", so a
		// content table that does not exist fails the CREATE ("no such table:
		// main.t7", verified) and every column it has becomes an fts column,
		// INCLUDING its INTEGER PRIMARY KEY: "fts4(content=p1)" over
		// "p1(id INTEGER PRIMARY KEY, x TEXT, y)" declares id, x and y, and
		// "SELECT rowid, * FROM fp1" over p1's row 5 answers 5|5|hello|world.
		// This runs BEFORE the empty-argument default below, so a content
		// table with no columns of its own cannot reach it.
		var cerr error
		if names, cerr = fts3ContentColumnsOf(cat, content); cerr != nil {
			return fts3Schema{}, cerr
		}
		if langid != "" {
			// An external-content table reads its language id from the content
			// table's own column of the DECLARED name (fts3ReadExprList's
			// "x.%Q" branch takes p->zLanguageid, not the literal "langid" an
			// ordinary table's %_content uses), and a DERIVED column list has
			// that column removed from it -- verified: "content=t3_data,
			// languageid=l" over "t3_data(l, x, y)" declares x and y only, l
			// becoming the table's hidden language column instead of a text
			// one. A content table with no column of that name (e.g.
			// "content=t8c, languageid=langid" over "t8c(a, b)") is unaffected.
			kept := names[:0:0]
			for _, c := range names {
				if !equalFoldName(c, langid) {
					kept = append(kept, c)
				}
			}
			names = kept
		}
		for _, c := range names {
			seen[r33sFoldIdent(c)] = true
		}
	}
	if len(names) == 0 {
		names = []string{"content"}
	}
	// Every name goes into ONE sqlite3_declare_vtab ("CREATE TABLE x(<cols>,
	// <table> HIDDEN, docid HIDDEN, <langid> HIDDEN)"), so a langid column that
	// duplicates a user column or "docid" fails the CREATE with "vtable
	// constructor failed" -- verified for both. (Its collision with the TABLE's
	// own name is caught in createShadowTables, the only place that knows it.)
	if langid != "" {
		if seen[r33sFoldIdent(langid)] || equalFoldName(langid, "docid") {
			return fts3Schema{}, fmt.Errorf("%s: duplicate column name: %s", m.name, langid)
		}
	}
	sch := fts3Schema{cols: names, notindexed: make([]bool, len(names)), hasDocsize: hasDocsize, prefixes: prefixes, tok: tok, langid: langid, content: content, hasContent: hasContent, compress: compress, uncompress: uncompress, compressGiven: compressGiven, uncompressGiven: uncompressGiven, descIdx: descIdx}
	for _, want := range notindexed {
		found := false
		for i, c := range names {
			if strings.EqualFold(c, want) {
				sch.notindexed[i] = true
				found = true
			}
		}
		if !found {
			return fts3Schema{}, fmt.Errorf("%s: no such column: %s", m.name, want)
		}
	}
	return sch, nil
}

// fts3IsTokenizerArg reports whether a is a tokenizer specification: literally
// "tokenize" followed by a character that could not continue an identifier.
// Real fts3's test is `strlen(z)>8 && strnicmp(z,"tokenize",8)==0 &&
// !sqlite3Fts3IsIdChar(z[8])`, so "tokenizer=simple" is NOT one -- it declares a
// column named "tokenizer" (verified).
func fts3IsTokenizerArg(a string) bool {
	if len(a) <= 8 || !strings.EqualFold(a[:8], "tokenize") {
		return false
	}
	return !fts3IsIdentChar(a[8])
}

// parseTokenizer resolves a tokenizer specification, or declines it BY NAME.
// Real fts3 skips exactly ONE character after "tokenize" (`&z[9]`), so
// "tokenize=simple" and "tokenize simple" both name "simple", which is fts3's
// default. Everything after that is fts3_tokenizer.go's business.
func (m fts3Module) parseTokenizer(a string) (*fts3Tokenizer, error) {
	return fts3ParseTokenizerSpec(m.name, a[9:])
}

// fts3MatchinfoOption parses "matchinfo=<val>", reporting whether the table has
// no %_docsize. Only "fts3" is accepted, case-insensitively (fts3InitVtab:
// `if( strlen(zVal)!=4 || sqlite3_strnicmp(zVal,"fts3",4) )` is the error
// path); "matchinfo=fts4" is "unrecognized matchinfo: ...".
//
// Its effects on "fts4(a, b, matchinfo=fts3)":
//
//   - %_docsize is not created; %_stat is still written in full.
//   - matchinfo()'s 'l' (current row's column lengths, which only %_docsize
//     can answer) becomes "unrecognized matchinfo request: l"; 'n', 'a', 's'
//     and the default 'pcx' still work.
//
// Nothing else changes.
func fts3MatchinfoOption(val string) (noDocsize bool, err error) {
	// NOT trimmed: fts3IsSpecialColumn dequotes the value after the first '='
	// and nothing else, so "matchinfo= fts3" is "unrecognized matchinfo:  fts3"
	// against the oracle. Accepting it here created a %_docsize-less table real
	// SQLite refuses outright.
	v, ok := fts3DequoteArg(val)
	if !ok || !strings.EqualFold(v, "fts3") {
		return false, fmt.Errorf("unrecognized matchinfo: %s", val)
	}
	return true, nil
}

// fts3OrderOption parses an fts4 "order=" value: exactly "asc" or "desc",
// case-insensitively, dequoted but not trimmed ("order=[asc]" is fine,
// "order=' asc'" and "order=ascx" are "unrecognized order").
//
// ASC is the default and a complete no-op (the table is byte-identical to one
// without the option).
//
// DESC changes three things:
//
//   - doclists are written in descending docid order, the first docid
//     absolute and each later one a positive decrement (fts3PutDeltaVarint3's
//     bDescIdx branch; fts3_index.go and fts3_search.go);
//   - MATCH returns newest docid first;
//   - so does a plain scan, also in a join, while an explicit "ORDER BY docid"
//     wins (fts3BestIndexMethod prefers the ORDER BY's direction); both come from
//     materializeFts3Item.
//
// Pending terms stay ascending, as C's hash does until a segment is written,
// so the only declined write is the one where C's scan order leaks into the
// segment layout: an UPDATE of more than one row (fts3_write.go).
func fts3OrderOption(val string) (desc bool, err error) {
	v, ok := fts3DequoteArg(val)
	if !ok || (!strings.EqualFold(v, "asc") && !strings.EqualFold(v, "desc")) {
		return false, fmt.Errorf("unrecognized order: %s", val)
	}
	return strings.EqualFold(v, "desc"), nil
}

// fts3SpecialColumnKey splits an fts4 module OPTION into its key. Real fts3
// scans for the first '=' and takes everything before it, however odd
// (fts3IsSpecialColumn), so this deliberately does no validation of its own.
func fts3SpecialColumnKey(a string) (string, bool) {
	key, _, found := strings.Cut(a, "=")
	if !found {
		return "", false
	}
	return key, true
}

// declineModuleOption is reached for a key that is not one of the eight real
// fts3 recognizes (aFts4Opt, fts3.c:1259-1267: matchinfo, prefix, compress,
// uncompress, order, content, languageid, notindexed) -- every one of which
// parseSchemaWith now handles inline with its own "continue", including a key
// that only LOOKS like one of them ("notindexed = b", whose key is
// "notindexed " with the trailing space, matches none of the eight by exact
// length and falls through here same as C fts3's own iOpt==
// SizeofArray(aFts4Opt) case). So every key that reaches this point is
// genuinely unrecognized on both engines, and they already agree.
func (m fts3Module) declineModuleOption(arg string) error {
	// fts3.c:1341 is "sqlite3Fts3ErrMsg(pzErr, \"unrecognized parameter: %s\", z)"
	// -- the module does NOT name itself, so neither does this.
	return fmt.Errorf("unrecognized parameter: %s", arg)
}

// parseColumnName extracts a column's name from one module argument: its first
// token, dequoted, with any type discarded. Both helpers are total, as their C
// originals are. No token, or one dequoting to empty ("\"\""), gives an empty
// column name as C's "Fill in the azColumn array" loop does (fts3.c:1459-1470):
// "fts4(a, \"\", '---')" has a middle column named "", and "fts3(<, b, c)" a
// first. Duplicate names still fail in parseSchemaWith.
func (m fts3Module) parseColumnName(a string) (string, error) {
	tok, ok := fts3FirstToken(a)
	if !ok {
		return "", nil
	}
	inner, _ := fts3DequoteArg(tok)
	return inner, nil
}

// fts3FirstToken is sqlite3Fts3NextToken (fts3_tokenizer.c:128): the bounds of
// the first token in a module argument, skipping characters that cannot start
// one. ok is false only when none can ("n stays 0" in fts3.c, an empty column
// name). An unterminated quote or bracket extends to the end, as C's does,
// though no real CREATE reaches it: the SQL tokenizer rejects such a statement
// first (tokenize.c:396-418, 499-502). Stopping at the first non-identifier
// character makes "xyz=abc" the column "xyz" and "a VARCHAR(10)" the column
// "a".
func fts3FirstToken(a string) (string, bool) {
	i := 0
	for i < len(a) && !fts3IsIdentChar(a[i]) && !fts3IsQuoteChar(a[i]) {
		i++
	}
	if i >= len(a) {
		return "", false
	}
	if a[i] == '[' {
		// No doubling for '[' in the C's NextToken (only its own close, ']',
		// ends the scan) -- a genuinely separate branch from the other three
		// quote characters, unlike fts3DequoteArg below where sqlite3Fts3Dequote
		// reassigns its "quote" variable to ']' and reuses ONE algorithm for
		// all four.
		j := i + 1
		for j < len(a) && a[j] != ']' {
			j++
		}
		if j < len(a) {
			j++
		}
		return a[i:j], true
	}
	if fts3IsQuoteChar(a[i]) {
		// The doubling-absorption rule real NextToken's
		// "while(*++z2 && (*z2!=c || *++z2==c))" implements: an embedded PAIR of
		// the quote character is one escaped character, not the close, so
		// "'it''s'" is a single token, not two.
		quote := a[i]
		j := i + 1
		for j < len(a) {
			if a[j] == quote {
				if j+1 < len(a) && a[j+1] == quote {
					j += 2
					continue
				}
				j++
				break
			}
			j++
		}
		return a[i:j], true
	}
	j := i
	for j < len(a) && fts3IsIdentChar(a[j]) {
		j++
	}
	return a[i:j], true
}

// fts3IsIdentChar is sqlite3Fts3IsIdChar (fts3_tokenizer.c:113): alphanumeric,
// '_', '$', or any byte with its high bit set -- C fts3's own table
// ("return c&0x80 || isFtsIdChar[(int)(c)]") treats every non-ASCII UTF-8
// byte as an identifier character unconditionally, and '$' (0x24) is marked
// in the table alongside the usual alnum/underscore set.
func fts3IsIdentChar(c byte) bool {
	return c&0x80 != 0 || c == '_' || c == '$' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func fts3IsQuoteChar(c byte) bool {
	return c == '\'' || c == '"' || c == '`' || c == '['
}

// fts3DequoteArg is sqlite3Fts3Dequote (fts3.c:464), total in C: strip one
// layer of quoting, collapsing a doubled interior quote (for '[...]' too, since
// C reuses the algorithm with ']' as the quote, unlike fts3FirstToken). An
// unclosed quote runs to the end; an unquoted argument passes through. ok is
// always true; callers keep their fallback for it.
func fts3DequoteArg(a string) (string, bool) {
	if a == "" {
		return "", true
	}
	quote := a[0]
	if quote != '\'' && quote != '"' && quote != '`' && quote != '[' {
		return a, true
	}
	if quote == '[' {
		quote = ']'
	}
	var b strings.Builder
	for i := 1; i < len(a); {
		if a[i] == quote {
			if i+1 < len(a) && a[i+1] == quote {
				b.WriteByte(quote)
				i += 2
				continue
			}
			break
		}
		b.WriteByte(a[i])
		i++
	}
	return b.String(), true
}

// ---- shadow tables ----

// fts3ShadowSuffixes is every shadow table an fts3 table owns, in the order
// C SQLite creates them (which is the order they appear in sqlite_schema).
var fts3ShadowSuffixes = []string{"_content", "_segments", "_segdir"}

// fts4ShadowSuffixes adds the two tables only fts4 maintains -- minus
// %_docsize, which a "matchinfo=fts3" table does not have.
var fts4ShadowSuffixes = []string{"_content", "_segments", "_segdir", "_docsize", "_stat"}
var fts4NoDocsizeShadowSuffixes = []string{"_content", "_segments", "_segdir", "_stat"}

// fts3WithoutContent drops %_content from each of the three: a
// "content=" table keeps its rows elsewhere (or nowhere), so fts3CreateTables
// skips that one CREATE entirely -- verified, "fts4(content=t1)" leaves exactly
// ft1_segments, ft1_segdir, ft1_docsize and ft1_stat.
func fts3WithoutContent(suffixes []string) []string { return suffixes[1:] }

func (m fts3Module) shadowSuffixes(sch fts3Schema) []string {
	var all []string
	switch {
	case sch.hasDocsize:
		all = fts4ShadowSuffixes
	case m.isFts4:
		all = fts4NoDocsizeShadowSuffixes
	default:
		all = fts3ShadowSuffixes
	}
	if sch.hasContent {
		return fts3WithoutContent(all)
	}
	return all
}

// createShadowTables creates a new fts3/fts4 table's shadow tables with C's
// CREATE text byte for byte (see the file doc; fts3QuoteName for the %q
// quoting).
//
// It also refuses a column named after the table itself: fts3 declares a hidden
// column of that name, so sqlite3_declare_vtab sees a duplicate: "CREATE
// VIRTUAL TABLE tt USING fts4(tt)" is "vtable constructor failed: tt". That
// keeps the command channel unambiguous (fts3_command.go), and only CREATE
// knows the table's name.
func (m fts3Module) createShadowTables(db *DB, name string, sch fts3Schema, isTemp bool) error {
	colNames := sch.cols
	for _, c := range colNames {
		if strings.EqualFold(c, name) {
			return fmt.Errorf("%s: a column may not share the table's name (%s): it would collide with %s's command channel", m.name, c, m.name)
		}
	}
	// A "languageid=" naming the table itself is the same duplicate in the same
	// sqlite3_declare_vtab -- "fts4(a, languageid=tB)" on table tB is "vtable
	// constructor failed: tB", verified.
	if sch.langid != "" && strings.EqualFold(sch.langid, name) {
		return fmt.Errorf("%s: a column may not share the table's name (%s): it would collide with %s's command channel", m.name, sch.langid, m.name)
	}
	// A shadow table belongs to the SAME catalog as the fts3/fts4 table itself
	// -- verified against the oracle: "CREATE VIRTUAL TABLE temp.t1 USING
	// fts3(a)" files t1, t1_content, t1_segments, t1_segdir AND its autoindex
	// in sqlite_temp_master and none of them in sqlite_master. "CREATE TABLE"
	// (not "CREATE TEMP TABLE") already puts a shadow in main, matching this
	// module's existing bare/main behavior unchanged.
	tempKw := ""
	if isTemp {
		tempKw = "TEMP "
	}
	var stmts []string
	if !sch.hasContent {
		var contentCols strings.Builder
		contentCols.WriteString("docid INTEGER PRIMARY KEY")
		for i, c := range colNames {
			fmt.Fprintf(&contentCols, ", %s", fts3QuoteName(fmt.Sprintf("c%d%s", i, c)))
		}
		if sch.langid != "" {
			// Literally "langid", unquoted, NOT the declared name:
			// fts3CreateTables composes "%z, langid" and never substitutes the
			// name it passes (see fts3_langid.go). Verified byte for byte
			// against the oracle.
			contentCols.WriteString(", langid")
		}
		stmts = append(stmts, fmt.Sprintf("CREATE %sTABLE %s(%s)", tempKw, fts3QuoteName(name+"_content"), contentCols.String()))
	}
	stmts = append(stmts,
		fmt.Sprintf("CREATE %sTABLE %s(blockid INTEGER PRIMARY KEY, block BLOB)", tempKw, fts3QuoteName(name+"_segments")),
		fmt.Sprintf("CREATE %sTABLE %s(level INTEGER,idx INTEGER,start_block INTEGER,leaves_end_block INTEGER,end_block INTEGER,root BLOB,PRIMARY KEY(level, idx))", tempKw, fts3QuoteName(name+"_segdir")))
	if sch.hasDocsize {
		stmts = append(stmts,
			fmt.Sprintf("CREATE %sTABLE %s(docid INTEGER PRIMARY KEY, size BLOB)", tempKw, fts3QuoteName(name+"_docsize")))
	}
	if m.isFts4 {
		stmts = append(stmts,
			fmt.Sprintf("CREATE %sTABLE %s(id INTEGER PRIMARY KEY, value BLOB)", tempKw, fts3QuoteName(name+"_stat")))
	}
	// A shadow name already taken makes the whole CREATE fail with NOTHING
	// left behind -- verified against the oracle: with a "t_segdir" table
	// already present, "CREATE VIRTUAL TABLE t USING fts4(a,b)" errors and
	// leaves no t_content either. So undo any table already created here.
	created := make([]string, 0, len(stmts))
	for i, s := range stmts {
		if err := db.CreateTable(s); err != nil {
			for _, n := range created {
				if tbl := db.findTableMeta(n); tbl != nil {
					db.removeTableAndIndexes(tbl)
				}
			}
			return err
		}
		created = append(created, name+m.shadowSuffixes(sch)[i])
	}
	return nil
}

// fts3FamilyModule reports whether a module NAME is one of the four this file
// tree implements -- the two that hold data (fts3/fts4) and the two read-only
// views over one that does (fts4aux, fts3tokenize). It answers from the name
// rather than a registered module because CreateVirtualTable needs it before
// any table exists; see the counter rule there.
func fts3FamilyModule(name string) bool {
	switch strings.ToLower(name) {
	case "fts3", "fts4", "fts4aux", "fts3tokenize":
		return true
	}
	return false
}

// fts3ModuleOf returns the fts3/fts4 module backing vm, if vm is one.
func fts3ModuleOf(vm *vtabMeta) (fts3Module, bool) {
	m, ok := lookupVtabModule(vm.module)
	if !ok {
		return fts3Module{}, false
	}
	fm, ok := m.(fts3Module)
	return fm, ok
}

// schemaOf is the table's schema including the notindexed flags, which only the write path
// needs (a notindexed column is read back like any other). db supplies the
// catalog an external-content table that declares no columns of its own is
// re-derived through (fts3_content.go).
func (m fts3Module) schemaOf(db *DB, vm *vtabMeta) (fts3Schema, error) {
	return m.parseSchemaWith(vm.args, db.fts3Catalog())
}

// dropShadowTables removes the shadow tables of fts3/fts4 table name, as C's
// xDestroy does. It drops all five names regardless of what the table owns,
// because fts3DestroyMethod runs five unconditional "DROP TABLE IF EXISTS",
// taking same-named user tables with it ("fts3(a,b)" plus hand-made t3_docsize
// and t3_stat: "DROP TABLE t3" empties sqlite_master).
//
// A "content=" table is the exception: the fifth statement's "%s" prefix is
// "--" there, commenting out the %_content drop, so a same-named user
// t5_content survives "DROP TABLE ft5".
func (m fts3Module) dropShadowTables(db *DB, name string, hasContent bool) {
	suffixes := fts4ShadowSuffixes
	if hasContent {
		suffixes = fts3WithoutContent(suffixes)
	}
	for _, suffix := range suffixes {
		if tbl := db.findTableMeta(name + suffix); tbl != nil {
			db.removeTableAndIndexes(tbl)
		}
	}
}

// ---- read path ----

// materializeFts3 presents an fts3/fts4 table's rows by reading %_content, as C
// does for a query without MATCH: [docid, col0, ...], docid from the %_content
// rowid. A row written into %_content behind the index's back shows in "SELECT
// * FROM t" on both engines but not under MATCH, which is why MATCH is answered
// from the index (fts3_search.go).
func (p *ReadOnlyPager) materializeFts3(name string, cols []columnInfo) ([]columnInfo, [][]Value, []int64, error) {
	return p.materializeFts3Item(FromItem{Table: name}, name, cols, fts3Schema{})
}

// materializeFts3Item is materializeFts3 for a known schema, so a "content="
// table can use its own row source (fts3_content.go); it carries the
// statement's top-level WHERE conjuncts, which choose between the content
// table and the index.
//
// An "order=desc" table's rows come newest docid first, MATCH or not: C's full
// scan prepares "... ORDER BY rowid DESC" (fts3FilterMethod), also in a join.
// An explicit ORDER BY docid/rowid wins (fts3BestIndexMethod) and other ORDER BYs are
// sorted above this source. A MATCH is a predicate over these same rows
// (evalFts3Match).
func (p *ReadOnlyPager) materializeFts3Item(it FromItem, name string, cols []columnInfo, sch fts3Schema) ([]columnInfo, [][]Value, []int64, error) {
	outCols, rows, outRowids, err := p.materializeFts3Ascending(it, name, cols, sch)
	if err != nil {
		return nil, nil, nil, err
	}
	if meta, ok := p.fts3TableInfo(name); ok && meta.descIdx {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
			outRowids[i], outRowids[j] = outRowids[j], outRowids[i]
		}
	}
	return outCols, rows, outRowids, nil
}

// materializeFts3Ascending is materializeFts3Item's row source, always in
// ascending docid order.
func (p *ReadOnlyPager) materializeFts3Ascending(it FromItem, name string, cols []columnInfo, sch fts3Schema) ([]columnInfo, [][]Value, []int64, error) {
	if sch.hasContent {
		return p.materializeFts3Content(it, name, cols, sch)
	}
	// compress=/uncompress= is an fts4-only option (parseSchemaWith only ever
	// parses it under "if m.isFts4"), so a schema that reaches hasCompress()
	// here is always fts4 -- there is no fts3Module in scope on a
	// *ReadOnlyPager to ask.
	rowids, records, err := p.Rows(name + "_content")
	if err != nil {
		return nil, nil, nil, err
	}
	// When the read expression list is prepared decides whether an uncompress=
	// naming no function is an error. A full scan or a docid lookup prepares
	// it in xFilter (fts3.c:3399-3404), rows or none. A MATCH prepares it only
	// in fts3CursorSeek, for a row whose content a column actually reads --
	// never, when the select list reads no column (fts3ContentUnneeded).
	// ponytail: a MATCH that reads content prepares here once %_content has a
	// row, where C waits for a MATCHING one; the difference is an error where
	// C answers no rows.
	var codec *fts3ContentCodec
	match := fts3WhereHasMatch(andConjuncts(it.tvfWhere))
	if !it.fts3ContentUnneeded && (!match || len(records) > 0) {
		if codec, err = newFts3ContentCodec(sch, sch.uncompress); err != nil {
			return nil, nil, nil, err
		}
	}
	rows := make([][]Value, len(records))
	outRowids := make([]int64, len(records))
	have := make(map[int64]bool, len(records))
	for i := range records {
		rec, cerr := codec.row(sch, records[i])
		if cerr != nil {
			return nil, nil, nil, cerr
		}
		row := make([]Value, len(cols))
		row[0] = Value{Typ: Int, I: int64(rowids[i])}
		for j := 1; j < len(cols) && j < len(rec); j++ {
			row[j] = rec[j]
		}
		rows[i] = row
		outRowids[i] = int64(rowids[i])
		have[int64(rowids[i])] = true
	}
	if !it.fts3ContentUnneeded {
		return cols, rows, outRowids, nil
	}
	// it.fts3ContentUnneeded (set by withVtabWhere from
	// fts3StmtContentUnneeded) proves the WHERE is one MATCH on this table
	// and nothing reads a real column, so C never seeks %_content for a row
	// (fts3ColumnMethod, fts3.c:3462-3505) and every indexed docid must be
	// offered even if %_content lacks it. The per-row MATCH test
	// (evalFts3Match with contentUnneeded) filters them, as for a
	// "content=" table's extra rows.
	//
	// The langid is 0: that shape leaves no room for a langid conjunct, so
	// an unconstrained MATCH searches language 0 (as fts3IndexOnlyDocids
	// does).
	// it.fts3ContentWanted is the lazy term set fts3StmtContentUnneeded
	// gathered for this MATCH, keeping this as lazy as fts3MatchDocids; nil
	// (eager) only for a non-literal pattern.
	ix, err := p.fts3LoadIndex(name, fts3LevelBase(0, sch.nIndex(), 0), it.fts3ContentWanted)
	if err != nil {
		return nil, nil, nil, err
	}
	var extra []int64
	for _, d := range ix.allDocids() {
		if !have[d] {
			extra = append(extra, d)
		}
	}
	if len(extra) == 0 {
		return cols, rows, outRowids, nil
	}
	// Merged ASCENDING by docid -- %_content's own rowids are already
	// ascending (p.Rows scans a b-tree in key order) and so is
	// ix.allDocids(), mirroring materializeFts3Content's identical merge
	// (fts3_content.go) for the same "content=" shape.
	merged := make([][]Value, 0, len(rows)+len(extra))
	mergedRowids := make([]int64, 0, len(rows)+len(extra))
	i, j := 0, 0
	for i < len(rows) || j < len(extra) {
		if j >= len(extra) || (i < len(rows) && outRowids[i] < extra[j]) {
			merged = append(merged, rows[i])
			mergedRowids = append(mergedRowids, outRowids[i])
			i++
			continue
		}
		row := make([]Value, len(cols))
		row[0] = Value{Typ: Int, I: extra[j]}
		merged = append(merged, row)
		mergedRowids = append(mergedRowids, extra[j])
		j++
	}
	return cols, merged, mergedRowids, nil
}

// ---- write path ----

// vtabInsertRowValues assembles an INSERT's rows, one []Value per row in
// target-column order, from pre: the tuples the compiled route (OpVInsert)
// already evaluated into registers, as C codes each tuple (insert.c:1431) and
// hands the registers to xUpdate (vdbe.c:8736-8743). Only the arity check
// happens here, since the target list is resolved per module by the caller.
// Shared by every writable virtual table (fts3/fts4 below, and insertIntoVtab
// for rtree and fts5).
func (db *DB) vtabInsertRowValues(stmt *insertStmt, targets []int, pre [][]Value, preWidth int, args []Value, outer *evalCtx) ([][]Value, error) {
	// DEFAULT VALUES needs no arm here: the parser models it as one empty
	// tuple with a non-nil empty column list ("nColumn = 0", insert.c:1214),
	// both target resolvers map that to an empty target set, and
	// insertIntoVtab NULL-fills every column, as C's coding loop emits
	// OP_Null for columns without a DEFAULT (insert.c:1400-1410; no module
	// here declares one):
	//
	//	fts4(x,y)          docid 1, x NULL, y NULL, no indexed term
	//	fts5(x,y)          rowid 1, x NULL, y NULL
	//	rtree(id,x0,x1)    rowid 1, id 1, x0 0.0, x1 0.0
	//
	// A column list before DEFAULT VALUES is rejected by the parser
	// (insert.c:1255-1258).
	if pre != nil {
		if preWidth >= 0 && preWidth != len(targets) {
			// The SOURCE SELECT's arity, checked even when it yielded no rows
			// -- the per-row check just below cannot see an empty source, and
			// C SQLite reports the mismatch at PREPARE time regardless
			// ("nColumn = pSelect->pEList->nExpr;", insert.c:1154, feeding
			// insert.c:1249-1253). Verified: over an EMPTY s, "INSERT INTO f
			// SELECT x,x FROM s" is "table f has 1 columns but 2 values were
			// supplied" on the oracle.
			return nil, fmt.Errorf("engine: table %s has %d columns but %d values were supplied", stmt.table, len(targets), preWidth)
		}
		for _, vals := range pre {
			if len(vals) != len(targets) {
				return nil, fmt.Errorf("engine: table %s has %d columns but %d values were supplied", stmt.table, len(targets), len(vals))
			}
		}
		return pre, nil
	}
	// pre is the only way a row reaches here (the compiled route lowers both
	// VALUES and SELECT sources); its absence is an internal error, never a
	// second evaluator (RULE #1).
	return nil, fmt.Errorf("engine: internal: INSERT into %s reached row assembly with no compiled rows", stmt.table)
}

// insertIntoFts3 executes an INSERT whose target is the fts3/fts4 virtual
// table vm. It writes the %_content row, the FTS4 %_docsize/%_stat rows, and
// one new level-0 %_segdir segment holding this statement's terms.
//
// Nothing is stored until every check has passed and the segment has been
// encoded: a statement this path declines (a spill, a merge, a duplicate
// docid) must leave the table exactly as it found it, or the two engines would
// disagree about a row the oracle's own rolled-back probe never kept either.
func (db *DB) insertIntoFts3(vm *vtabMeta, m fts3Module, stmt *insertStmt, pre [][]Value, preWidth int, args []Value, outer *evalCtx, ret *returningState) (int, error) {
	// Rebuilding the shadow tables' b-trees is a full-materialize shape, and
	// the CREATE VIRTUAL TABLE that made them already disqualified the
	// session anyway.

	// REPLACE is the one conflict mode reproduced here, because it is the one
	// fts3 itself implements: sqlite3Fts3UpdateMethod branches on
	// sqlite3_vtab_on_conflict(db)==SQLITE_REPLACE and calls fts3DeleteByRowid
	// for the conflicting docid BEFORE inserting, which is the delete-then-
	// insert-into-one-pending-set shape the UPDATE path already reproduces --
	// see the displacement block in the staging loop below. Every other mode
	// takes the other branch -- fts3InsertData first, then a duplicate-rowid
	// SQLITE_CONSTRAINT the vtab layer resolves -- and stays declined.
	replace := stmt.orAction == conflictReplace
	if (stmt.orAction != conflictAbort && !replace) || stmt.upsert != nil {
		return 0, fmt.Errorf("engine: INSERT into an %s table with an OR/ON CONFLICT/UPSERT clause is not supported by this write path", m.name)
	}
	// "INSERT INTO <fts3/fts4 table> DEFAULT VALUES" used to be declined here.
	// It is served now, by vtabInsertRowValues' own defaultValues arm -- one
	// all-NULL row one value wide per declared column, which is what
	// fts3InsertTargets(names==nil) resolves to. Verified against the 3.53.3
	// oracle over "CREATE VIRTUAL TABLE f USING fts4(x,y)": two DEFAULT VALUES
	// inserts give docid 1 and 2 with every column NULL (typeof 'null') and no
	// term in the index.

	sch, err := m.schemaOf(db, vm)
	if err != nil {
		return 0, err
	}
	colNames := sch.cols
	// RETURNING: C codes it from a synthesized trigger that ignores which
	// module the table belongs to (the IsVirtual fork, trigger.c:843-853,
	// only picks BEFORE), so this is the same BEFORE capture insertIntoVtab
	// does, over the candidate tuple, scoped by fts3DeclaredColumns. On
	// "fts4(x)": "INSERT INTO f VALUES('a') RETURNING rowid, docid, x,
	// typeof(x)" is "-1, NULL, a, text"; "INSERT INTO f(docid,x)
	// VALUES(9,'b') RETURNING rowid, docid, x" is "-1, 9, b"; "RETURNING *"
	// expands to the user columns.
	var vplan *vtabReturningPlan
	var retCols []columnInfo // the RETURNING scope's columns, built once
	if stmt.returning != nil {
		if outer != nil {
			// C SQLite forbids RETURNING inside a trigger body outright --
			// (build.c:1443-1444).
			return 0, fmt.Errorf("engine: INSERT ... RETURNING inside a trigger body is not supported (C SQLite forbids RETURNING in triggers)")
		}
		if stmt.selectStmt != nil {
			// insertIntoVtab declines the same combination for the same
			// reason: the per-row capture is wired into the VALUES-sourced
			// loop only, and a SELECT source is unmeasured here.
			return 0, fmt.Errorf("engine: INSERT ... SELECT ... RETURNING into an %s table is not supported by this write path", m.name)
		}
		retPager, rperr := db.writeSubqueryPager()
		if rperr != nil {
			return 0, rperr
		}
		retCols = vtabColumnInfos(fts3DeclaredColumns(sch))
		scope := vtabEvalScope(stmt.table, retCols)
		mode := colNameMode{full: db.fullColumnNames, short: !db.shortColumnNamesOff}
		p, verr := buildVtabReturningPlan(scope, stmt.returning, mode, retPager)
		if verr != nil {
			return 0, verr
		}
		p.caseSensitiveLike = db.caseSensitiveLike
		vplan = p
	}
	// A "content=" table has no %_content of its own and NEVER writes one: the
	// row it is handed contributes to the index, %_docsize and %_stat alone
	// (fts3_content.go).
	content := db.findTableMeta(vm.name + "_content")
	segdir := db.findTableMeta(vm.name + "_segdir")
	segments := db.findTableMeta(vm.name + "_segments")
	if (content == nil && !sch.hasContent) || segdir == nil || segments == nil {
		return 0, fmt.Errorf("engine: %s table %s is missing its shadow tables", m.name, vm.name)
	}

	// Resolve the INSERT's target columns over [docid, col0, ...]. "rowid" is
	// accepted as a spelling of docid, exactly as C fts3 accepts it, and the
	// table's OWN name is the hidden command-channel slot at nCol+1.
	nCol := len(colNames)
	targets, err := fts3InsertTargets(m, vm.name, sch, stmt.cols)
	if err != nil {
		return 0, err
	}

	// The row VALUES, from either a VALUES list or a source SELECT. Both end up
	// in the same staging loop below, which is what makes "INSERT ... SELECT"
	// work here at all: C fts3 flushes ONE segment per STATEMENT, and this
	// path already builds exactly one, so a multi-row source needs no different
	// segment handling than a multi-row VALUES list does -- including the
	// ascending-docid rule the loop already enforces.
	valueRows, err := db.vtabInsertRowValues(stmt, targets, pre, preWidth, args, outer)
	if err != nil {
		return 0, err
	}

	// An INSERT naming the fts3/fts4 table itself as a target column is its
	// COMMAND CHANNEL, not a row -- unless the value there is NULL, which real
	// fts3 treats as an ordinary all-NULL row (fts3_command.go).
	if cmdSlot := fts3CommandSlot(vm.name, stmt.cols); cmdSlot >= 0 {
		if handled, n, cerr := db.fts3CommandInsert(vm, m, stmt.table, valueRows, cmdSlot); handled {
			if cerr == nil {
				if rerr := captureVtabCommandRow(ret, vplan, retCols, args); rerr != nil {
					return n, rerr
				}
				// A command stores no row, so fts3's xUpdate returns without
				// touching *pRowid and OP_VUpdate publishes the zero it
				// initialized -- see markVtabInsertRowid.
				db.markVtabInsertRowid(0)
			}
			return n, cerr
		}
	}

	// A compress= table's command channel above never touches %_content, but
	// every row past this point writes it through the compress= call, which
	// fts3InsertData prepares before anything else (fts3_write.c:1005).
	writeCodec, werr := newFts3ContentCodec(sch, sch.compress)
	if werr != nil {
		return 0, werr
	}
	// A REAL ROW insert (not a command, already handled above) into an
	// automerge-enabled table INSIDE an explicit transaction or savepoint is
	// not yet supported -- see engine/fts3_automerge.go's own file comment.
	// Checked before anything is staged, so a decline here leaves the table
	// exactly as it found it, same as every other decline in this function.
	if derr := db.fts3DeclineAutomergeInTxn(m, vm.name); derr != nil {
		return 0, derr
	}

	// Stage every change; commit only once the whole statement has succeeded.
	var staged []fts3StagedRow
	takenDocids := map[int64]bool{}
	// stagedMax tracks the largest docid this statement has assigned so far.
	// Real fts3 writes each %_content row as it goes, so a later row's
	// AUTO-assigned docid is one past whatever the statement itself has
	// already stored: "INSERT INTO t(docid,a) VALUES(5,'x'),(NULL,'y')" gives
	// the second row docid 6, not docid 1. Staging the rows instead of writing
	// them means that has to be carried by hand.
	stagedMax := int64(0)
	stagedAny := false
	nextDocid := func() (int64, error) {
		rid, rerr := nextRowidForTable(content)
		if rerr != nil {
			return 0, rerr
		}
		next := int64(rid)
		if stagedAny && stagedMax+1 > next {
			next = stagedMax + 1
		}
		return next, nil
	}

	pt := newFts3PendingSet(sch.prefixes, sch.descIdx)
	// REPLACE's displacement half needs the row it is about to overwrite, and
	// how many rows %_content still holds -- fts3DeleteByRowid's fts3IsEmpty
	// check. A "content=" table (including a contentless one) never reaches
	// either: sqlite3Fts3UpdateMethod guards the whole conflict branch with
	// "p->zContentTbl==0". Verified -- REPLACE at an existing docid of an
	// fts4(a, content="") table adds a THIRD segment with no delete markers and
	// takes %_stat's nDoc to 3.
	var oldRows map[int64][]Value
	liveCount := 0
	wiped := false
	if replace && !sch.hasContent {
		oldDocids, rows, rerr := fts3ContentRows(content, sch)
		if rerr != nil {
			return 0, rerr
		}
		oldRows = make(map[int64][]Value, len(oldDocids))
		for i, d := range oldDocids {
			oldRows[d] = rows[i]
		}
		liveCount = len(oldDocids)
	}
	stmtLangid := int64(0)
	haveLangid := false
	// midFlushed holds every pending set a LANGUAGE CHANGE has already sealed
	// off mid-statement, in order; each becomes its own %_segdir segment set
	// at commit, ahead of the still-accumulating final one in pt. Mirrors
	// fts3Mutation's own mid/midFlushed (fts3_write.go, fts3_txn.go's
	// fts3MidFlush) but keyed on language alone: an INSERT's own docids are
	// already required ascending across the WHOLE statement (the check
	// below), so unlike a docid-changing UPDATE's two-per-row operations,
	// this loop never needs a backwards-docid trigger of its own -- only the
	// language one fts3PendingTermsDocid ALSO flushes on
	// ("p->iPrevLangid!=iLangid", fts3_write.c:905, independent of docid
	// order; see fts3_langid.go).
	var midFlushed []*fts3PendingSet
	for _, rowVals := range valueRows {
		// [docid, col0, ..., command, langid], all NULL to start. The
		// command-channel slot is one an ordinary INSERT only ever holds NULL
		// in and nothing below reads; the langid slot exists only for a
		// "languageid=" table (fts3_langid.go).
		full := make([]Value, fts3LangidTargetSlot(nCol)+1)
		for i, v := range rowVals {
			full[targets[i]] = v
		}
		// The BEFORE-trigger capture, over this row's candidate tuple and
		// before anything is staged -- see vtabCandidateRow (vtab_write.go)
		// for the C and for the rowid rule. A subquery's pager is snapshotted
		// here for the same reason insertIntoVtab snapshots one: it must see
		// every EARLIER row of this statement and not this one.
		if ret != nil && vplan != nil {
			var pager *ReadOnlyPager
			if vplan.hasSubquery {
				pager, err = db.SnapshotPager()
				if err != nil {
					return 0, err
				}
			}
			// The rowid a virtual table's RETURNING reads is always the
			// BEFORE trigger's -1 here: an fts3 table declares "docid", not
			// "rowid", so insert.c:1096's sqlite3IsRowid branch never fires
			// for its column list and ipkColumn stays -1 (insert.c:1452).
			// Confirmed against the oracle: "INSERT INTO f(docid,x)
			// VALUES(9,'b') RETURNING rowid, docid" answers -1 and 9.
			if cerr := captureVtabRow(ret, vplan, vtabBeforeTriggerRowid,
				vtabCandidateRow(retCols, fts3ReturningScopeRow(sch, nCol, full), vtabBeforeTriggerRowid), args, pager); cerr != nil {
				return 0, cerr
			}
		}
		// sqlite3Fts3UpdateMethod refuses a NEGATIVE language id with
		// SQLITE_CONSTRAINT before writing anything, and stores every other
		// value through sqlite3_value_int (see fts3_langid.go). The check runs
		// even for a table with no "languageid=", where the slot is always
		// NULL and therefore always 0.
		rowLangid, langidOK := fts3LangidValue(full[fts3LangidTargetSlot(nCol)])
		if !langidOK {
			return 0, fmt.Errorf("engine: INSERT into %s: constraint failed", stmt.table)
		}
		if haveLangid && rowLangid != stmtLangid {
			// fts3PendingTermsDocid flushes when the language changes
			// (fts3_write.c:888-915, the check at 905), so seal pt into its own
			// segment and start fresh: "INSERT INTO t1(a,b,l) VALUES ('zero
			// zero','zero zero',0),('one two','three four',1),('five six','seven
			// eight',2)" on "fts4(a,b,languageid=l)" leaves three %_segdir rows,
			// one per language (fts3aux2.test). Segments are written at commit in
			// sealing order.
			midFlushed = append(midFlushed, pt)
			pt = newFts3PendingSet(sch.prefixes, sch.descIdx)
		}
		stmtLangid, haveLangid = rowLangid, true
		pt.langid = rowLangid
		var docid int64
		if sch.hasContent {
			// fts3InsertData's "content=" branch takes the docid (or the rowid
			// alias) and returns SQLITE_CONSTRAINT unless it is an INTEGER --
			// before writing anything, and with no conflict check of any kind,
			// because there is no %_content row to conflict with. Verified: a
			// NULL, a text and a 2.5 docid are each "constraint failed", while
			// the SAME docid inserted twice simply lands in the index twice
			// (two %_segdir rows, one %_docsize row, nDoc 2).
			if full[0].Typ != Int {
				return 0, fmt.Errorf("engine: INSERT into %s: constraint failed", stmt.table)
			}
			docid = full[0].I
		} else if full[0].Typ == Null {
			docid, err = nextDocid()
			if err != nil {
				return 0, err
			}
		} else {
			if full[0].Typ != Int {
				// Real fts3 requires an integer docid: "SQL logic error"
				// territory this engine has not pinned, so decline.
				return 0, fmt.Errorf("engine: INSERT into %s: a non-integer docid is not supported by this write path", stmt.table)
			}
			docid = full[0].I
		}
		if !sch.hasContent && !replace {
			// A REPLACE has no duplicate to fail on: an existing docid is
			// DISPLACED below, and one repeated within the statement is caught
			// by the ascending rule that follows (its insert half would arrive
			// at a docid the previous row's insert already used, which is
			// fts3PendingTermsDocid's "iDocid==iPrevDocid && bPrevDelete==0"
			// flush -- two segments, verified).
			if takenDocids[docid] {
				return 0, fmt.Errorf("engine: INSERT into %s: constraint failed (duplicate docid %d)", stmt.table, docid)
			}
			if _, exists := content.rows.get(uint64(docid)); exists {
				return 0, fmt.Errorf("engine: INSERT into %s: constraint failed (duplicate docid %d)", stmt.table, docid)
			}
		}
		// Real fts3 FLUSHES its pending terms -- ending one segment and
		// starting another -- as soon as a docid arrives that is not greater
		// than the previous one, because a doclist is delta-encoded and cannot
		// step backwards. Verified against the oracle: "VALUES(9,'aa'),
		// (3,'aa bb')" leaves TWO %_segdir rows. This path builds exactly one
		// segment per statement, so a non-ascending multi-row INSERT is
		// declined rather than silently folded into one.
		if stagedAny && docid <= stagedMax {
			return 0, fmt.Errorf("engine: INSERT into %s: a multi-row INSERT whose docids are not ascending is not supported by this write path (C fts3 splits it across index segments)", stmt.table)
		}
		takenDocids[docid] = true
		if !stagedAny || docid > stagedMax {
			stagedMax, stagedAny = docid, true
		}

		// A REPLACE at a docid %_content already holds is a DELETE followed by
		// an INSERT into ONE pending set: fts3DeleteByRowid records a marker
		// for every term of the old row's text, and fts3PendingTermsDocid then
		// does NOT flush when the insert half arrives, because the docid equals
		// the previous one AND the previous operation was a delete
		// (iDocid==iPrevDocid && bPrevDelete). Verified against
		// mattn/go-sqlite3 3.53.3: over a two-row fts4 table,
		// "REPLACE INTO t(docid,a) VALUES(1,'ee')" leaves ONE new %_segdir row
		// holding "aa"->{docid 1, POS_END}, "bb"->{docid 1, POS_END} and
		// "ee"->{docid 1, pos 0} -- the same bytes the equivalent UPDATE writes.
		var szDel []int64
		var byteDel int64
		if replace && !sch.hasContent {
			if old, exists := oldRows[docid]; exists {
				if oldLangid := fts3RowLangid(sch, old); oldLangid != rowLangid {
					// The delete half lands under the OLD row's language and
					// the insert half under the new one, so C fts3 flushes
					// between them and writes two segments in two different
					// indexes. Verified: on "fts4(a, languageid=lid)" holding
					// docid 1 at language 3, "REPLACE INTO t(docid,a,lid)
					// VALUES(1,'gamma',5)" leaves a marker-only segment at
					// level 3072 and the insert at level 5120 (fts3_langid.go).
					return 0, fmt.Errorf("engine: INSERT into %s: a REPLACE that moves a row to a different %s language id is not supported by this write path (C fts3 splits it across index segments)", stmt.table, m.name)
				}
				szDel, byteDel = fts3DeleteTermsInto(pt, sch, nCol, docid, old)
				liveCount--
				if liveCount == 0 {
					// fts3DeleteByRowid checks whether this was the last row and if so
					// runs fts3DeleteAll: every shadow table is emptied and pending terms
					// are dropped, so %_segdir restarts at idx 0. "REPLACE INTO
					// t(docid,a) VALUES(7,'gamma')" over a table holding only docid 7
					// writes what "UPDATE t SET a='gamma' WHERE docid=7" writes. Only the
					// statement's first row can get here: rows arrive in ascending docid
					// order, so any earlier row is still in %_content.
					wiped = true
					pt = newFts3PendingSet(sch.prefixes, sch.descIdx)
					pt.langid = rowLangid
					szDel, byteDel = nil, 0
				}
			}
			liveCount++
		}

		// %_content stores the docid in the rowid key, so its own slot is NULL
		// -- the ordinary INTEGER PRIMARY KEY convention (insert_write.go).
		// A "languageid=" table's %_content has one more column, holding the
		// language as an INTEGER whatever was written (fts3_langid.go).
		rec := make([]Value, nCol+1)
		copy(rec, full[:nCol+1])
		rec[0] = Value{Typ: Null}
		if sch.langid != "" {
			rec = append(rec, Value{Typ: Int, I: rowLangid})
		}

		sizes := make([]int, nCol)
		var nByte int64
		for c := 0; c < nCol; c++ {
			v := full[c+1]
			// A notindexed= column is skipped exactly as a NULL one is: no
			// terms, a zero in its %_docsize slot, and nothing added to
			// %_stat's column or byte totals (parseSchemaWith).
			if v.Typ == Null || !sch.indexed(c) {
				continue
			}
			// Real fts3 tokenizes sqlite3_value_text(), so a non-text value is
			// indexed through its TEXT rendering (an INTEGER 7 indexes as the
			// term "7", a BLOB x'414243' as "abc") and contributes that
			// rendering's byte length to %_stat -- both verified against the
			// oracle.
			text := valueToText(v)
			nByte += int64(len(text))
			terms := sch.tok.tokenize(text)
			sizes[c] = len(terms)
			for pos, term := range terms {
				pt.add(term, docid, c, pos)
			}
		}
		staged = append(staged, fts3StagedRow{docid: docid, vals: rec, sizes: sizes, nByte: nByte, szDel: szDel, byteDel: byteDel})
	}

	// FTS4's %_stat has to be read, folded and re-encoded; do that BEFORE
	// anything is written so a %_stat this path cannot safely update (a shape
	// it did not write) is declined with nothing already stored.
	var newStat []byte
	shadows := &fts3Shadows{}
	if err := fts3ResolveFts4Shadows(db, vm.name, m, sch, shadows); err != nil {
		return 0, err
	}
	docsize := shadows.docsize
	if m.isFts4 {
		st, serr := db.fts3LoadStat(shadows.stat, nCol)
		if serr != nil {
			return 0, serr
		}
		if wiped {
			// fts3DeleteAll emptied %_stat along with everything else, so what
			// this statement puts back counts from zero.
			st = &fts4Stat{colSizes: make([]int64, nCol)}
		}
		for _, d := range staged {
			// One fts3UpdateDocTotals call per row, since each row is its own
			// xUpdate: +1 document, except for a row that DISPLACED one, whose
			// delete and insert halves net to zero. The sizes fold in through
			// the same u32 saturating arithmetic (fts3_write.go), which for an
			// ordinary INSERT -- szDel nil, so every subtrahend zero -- is the
			// plain running total it has always been.
			nChng := 1
			if d.szDel != nil {
				nChng = 0
			}
			szIns := make([]int64, nCol)
			for c, n := range d.sizes {
				szIns[c] = int64(n)
			}
			st.applyDocTotals(nCol, nChng, szIns, d.szDel, d.nByte, d.byteDel)
		}
		newStat = st.encode()
	}

	// The compress= values, all of them before anything is written, so a
	// function that fails on some row fails the statement whole.
	contentVals := make([][]Value, len(staged))
	for i, st := range staged {
		v, cerr := writeCodec.row(sch, st.vals)
		if cerr != nil {
			return 0, cerr
		}
		contentVals[i] = v
	}

	// Every check has passed. A REPLACE that displaced the table's LAST row ran
	// fts3DeleteAll first, so every shadow table is empty and the transaction's
	// accumulated segment is gone before anything below is written -- the row
	// goes back into a %_segdir that restarts at idx 0 (fts3_write.go's wipe).
	if wiped {
		fts3ClearTable(content)
		fts3ClearTable(segdir)
		fts3ClearTable(segments)
		fts3ClearTable(docsize)
		fts3ClearTable(shadows.stat)
		db.fts3TxnDiscard()
	}

	// Language changes may have sealed chunks mid-statement; each becomes
	// its own segment before the final one (as fts3Mutation.commit does for
	// a docid-changing UPDATE). A wipe can only happen on row 0, before any
	// split, so it never discards one. The statement sub-transaction
	// preseal must precede the statement's first write, which for a split
	// statement is the first mid-flushed chunk, so it is applied here
	// (as fts3Mutation.commit's db.fts3TxnStmtSeal does) rather than by
	// fts3TxnSegmentForInsert, which would be too late.
	stmtSubTxn := fts3InsertOpensStmtSubTxn(stmt)
	hadMidFlush := len(midFlushed) > 0
	if hadMidFlush {
		// A statement whose own language split needs more than one %_segdir
		// write, combined with automerge being enabled, is declined BEFORE any
		// of them is flushed -- see fts3_automerge.go's own file comment for
		// why this combination is not reproduced rather than approximated.
		if db.fts3ReadAutoincrmerge(vm.name) != 0 {
			return 0, fmt.Errorf("engine: %s table %s: a single INSERT that flushes more than one %%_segdir write of its own (e.g. a multi-language INSERT into a \"languageid=\" table) is not supported by this write path while automerge is enabled (see engine/fts3_automerge.go)", m.name, vm.name)
		}
		db.fts3TxnStmtSeal(stmtSubTxn)
		for _, chunk := range midFlushed {
			if rerr := db.fts3TxnFlushMid(vm.name, sch.prefixes, chunk.langid, sch.descIdx, chunk, m, segdir, segments); rerr != nil {
				return 0, rerr
			}
		}
		stmtSubTxn = false // already applied above
	}

	// Inside a TRANSACTION the terms join the segment this transaction is
	// already accumulating, which is rewritten in place further down -- see
	// fts3_txn.go for why C fts3 leaves ONE %_segdir row for two SINGLE-ROW
	// statements there, and how a multi-row VALUES / an INSERT ... SELECT (this
	// statement's own sub-transaction) and a backwards docid each force a new
	// one.
	txnSeg := db.fts3TxnSegmentForInsert(vm.name, sch.prefixes, stmtLangid, sch.descIdx, staged, stmtSubTxn)

	// Write the segments FIRST, index by index, so a %_segdir a merge cannot
	// cascade through (fts3MaxMergeDepth, reachable only from a hand-built
	// shadow table) still declines before %_content moves. txnSeg is nil here
	// unconditionally when automerge is active for this table -- the
	// fts3DeclineAutomergeInTxn check above already refused any statement for
	// which it would not be.
	if txnSeg == nil {
		if ferr := db.fts3AutocommitFlushAndAutomerge(m, vm.name, sch, segdir, segments, hadMidFlush, pt); ferr != nil {
			return 0, ferr
		}
	}
	if content != nil && !sch.hasContent {
		for i, s := range staged {
			content.putRow(uint64(s.docid), contentVals[i])
		}
	}
	if docsize != nil {
		for _, d := range staged {
			docsize.putRow(uint64(d.docid), []Value{{Typ: Null}, {Typ: Blob, S: encodeFts4Docsize(d.sizes)}})
		}
	}
	if m.isFts4 {
		shadows.stat.putRow(0, []Value{{Typ: Null}, {Typ: Blob, S: newStat}})
	}
	if txnSeg != nil {
		if rerr := db.fts3TxnMergeAndRewrite(txnSeg, pt, m, segdir, segments); rerr != nil {
			return 0, rerr
		}
	}
	if len(staged) > 0 {
		db.markVtabInsertRowid(staged[len(staged)-1].docid)
	}
	return len(staged), nil
}

// fts3StagedRow is one row of an in-flight INSERT: its docid, the %_content
// record to store ([NULL, col0, ...] -- the docid rides in the rowid key), and
// the per-column token counts and total indexed byte count FTS4's
// %_docsize/%_stat need. Rows are staged rather than written as they are
// evaluated so a statement this path ends up declining (a spill, a merge, a
// duplicate docid in a later row) leaves the table untouched.
type fts3StagedRow struct {
	docid int64
	vals  []Value
	sizes []int
	nByte int64

	// szDel/byteDel are the DISPLACED row's per-column token counts and
	// indexed byte count, set only when a REPLACE overwrote an existing docid
	// -- %_stat's decrement for that xUpdate call, and (through szDel != nil)
	// what makes its net document-count change 0 rather than +1.
	szDel   []int64
	byteDel int64
}

// fts3InsertTargets maps an INSERT's column list onto positions in
// [docid, col0, ..., command]. A nil list (the implicit "INSERT INTO t
// VALUES(...)" form) targets the text columns only, exactly like C fts3.
// Both "docid" and "rowid" name slot 0, and the TABLE'S OWN NAME -- C fts3's
// hidden command-channel column -- names the trailing slot len(colNames)+1,
// which insertIntoFts3 never stores (fts3_command.go).
func fts3InsertTargets(m fts3Module, tableName string, sch fts3Schema, names []string) ([]int, error) {
	colNames := sch.cols
	if names == nil {
		idx := make([]int, len(colNames))
		for i := range colNames {
			idx[i] = i + 1
		}
		return idx, nil
	}
	out := make([]int, len(names))
	sawDocidSlot := false
	for i, n := range names {
		if strings.EqualFold(n, tableName) {
			out[i] = len(colNames) + 1
			continue
		}
		if sch.langid != "" && strings.EqualFold(n, sch.langid) {
			out[i] = fts3LangidTargetSlot(len(colNames))
			continue
		}
		if strings.EqualFold(n, "docid") || strings.EqualFold(n, "rowid") {
			// Naming BOTH spellings of the same slot is an error in real
			// fts3, not a last-one-wins: "INSERT INTO t4 (rowid, docid, c)
			// VALUES (14, 15, 'bad test')" is "SQL logic error" (verified
			// against the oracle; fts3b.test provokes it deliberately).
			if sawDocidSlot {
				return nil, fmt.Errorf("engine: INSERT into %s: rowid and docid are the same column and may not both be given", tableName)
			}
			sawDocidSlot = true
			out[i] = 0
			continue
		}
		pos := -1
		for j, c := range colNames {
			if strings.EqualFold(c, n) {
				pos = j + 1
				break
			}
		}
		if pos < 0 {
			return nil, fmt.Errorf("engine: table %s has no column named %s", tableName, n)
		}
		out[i] = pos
	}
	return out, nil
}

// fts3NextBlockID is SQL_NEXT_SEGMENTS_ID: the blockid a spilling segment's
// first block takes, i.e. one past the largest blockid %_segments currently
// holds, or 1 when it holds none. Block ids are never reused, so this keeps
// climbing across statements exactly as C fts3's does.
func fts3NextBlockID(segments *tableMeta) int64 {
	if segments == nil {
		return 1
	}
	if v, ok := maxRowidOfTable(segments); ok {
		return int64(v) + 1
	}
	return 1
}

// fts3StoreSegment writes one encoded segment out: its %_segments blocks (if
// it spilled) and its %_segdir row.
//
// end_block is TEXT, not an integer: C fts3 stores "<last block id> <total
// leaf bytes>" there whenever it has a leaf-byte count to record, which for a
// root-only segment (no %_segments blocks at all) is "0 <len(root)>". Verified
// against the oracle -- a 44-byte root stores the string "0 44", and a spilled
// segment ending at block 2 with 4345 bytes of leaves stores "2 4345". Writing
// a bare integer here reads back with the wrong TYPE, which "SELECT end_block
// FROM t_segdir" sees.
func fts3StoreSegment(segdir, segments *tableMeta, level, idx int64, img *fts3SegmentImage) error {
	if len(img.blocks) > 0 {
		if segments == nil {
			return fmt.Errorf("engine: fts3: this segment spills into %%_segments, which this table is missing")
		}
		for i, b := range img.blocks {
			segments.putRow(uint64(img.firstBlock+int64(i)), []Value{{Typ: Null}, {Typ: Blob, S: b}})
		}
	}
	rowid, err := nextRowidForTable(segdir)
	if err != nil {
		return err
	}
	segdir.putRow(rowid, []Value{
		{Typ: Int, I: level},
		{Typ: Int, I: idx},
		{Typ: Int, I: img.firstBlock},
		{Typ: Int, I: img.leavesEnd},
		{Typ: Text, S: []byte(fmt.Sprintf("%d %d", img.endBlock, img.nLeafData))},
		{Typ: Blob, S: img.root},
	})
	return nil
}

// fts3LoadStat reads FTS4's %_stat row id=0 for a table with nCol columns. An
// ABSENT row is the "no documents yet" state (all zeroes). A row holding
// anything other than a BLOB is a shape this path did not write -- a test (or
// a corrupt file) can store into a shadow table directly -- and is declined
// rather than silently reset to zero, which would make every later document
// total wrong. See fts3_index.go's fts4Stat for the layout and the oracle
// evidence.
func (db *DB) fts3LoadStat(stat *tableMeta, nCol int) (*fts4Stat, error) {
	row, ok := stat.rows.get(0)
	if !ok {
		return decodeFts4Stat(nil, nCol)
	}
	if len(row) < 2 || row[1].Typ != Blob {
		return nil, fmt.Errorf("engine: fts4: %%_stat row 0 does not hold a blob; this write path cannot update it")
	}
	return decodeFts4Stat(row[1].S, nCol)
}
