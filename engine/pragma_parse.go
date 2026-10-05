// This file re-parses a CREATE TABLE/CREATE INDEX statement's stored SQL for
// the PRAGMAs that describe it (table_info/table_xinfo, index_list/index_info/
// index_xinfo, foreign_key_list). It is deliberately independent of
// parseCreateTableColumnsAndAutoIndexes, which recovers only what the write
// path needs (it skips over a REFERENCES clause, for instance), so a change to
// one parser cannot silently break the other.
package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// pragmaColumn is one CREATE TABLE column definition, as PRAGMA table_info/
// table_xinfo need it.
type pragmaColumn struct {
	name       string
	declType   string
	notNull    bool
	hasDefault bool
	dflt       string // raw source text of the default expression; meaningful only if hasDefault
	// collate is the column's declared COLLATE name, upper-cased, or "" for
	// BINARY. foreign_key_check needs it: the parent-side comparison runs
	// under the parent column's collation.
	//
	// collateSrc is the same name as the CREATE TABLE text spelled it, since
	// index_xinfo's "coll" reports it verbatim ("b TEXT collate nocase"
	// reports "nocase"). Comparisons use the upper-cased form.
	collate    string
	collateSrc string
	// generated / generatedStored record a GENERATED ALWAYS AS (...) column and
	// whether it is STORED (VIRTUAL is SQLite's default). PRAGMA table_info
	// OMITS a generated column altogether -- and renumbers cid over the
	// survivors -- while table_xinfo reports every column and encodes the kind
	// in its "hidden" column: 0 ordinary, 2 VIRTUAL, 3 STORED. Both verified
	// directly against mattn/go-sqlite3.
	generated       bool
	generatedStored bool
}

// pragmaAutoIndexSpec is one inline UNIQUE or PRIMARY KEY constraint that
// implies an automatic index, in the exact order CreateTable/buildAutoIndexes
// (index_write.go) itself numbers sqlite_autoindex_<table>_<N> -- see
// parsePragmaTableDef's doc comment for why this file re-derives that
// numbering independently rather than sharing index_write.go's own.
type pragmaAutoIndexSpec struct {
	cols []string // table column names, in constraint-declaration order
	desc []bool   // parallel to cols: that column declared DESC
	// coll is parallel to cols: the constraint's own "COLLATE x" for that
	// column, as written, or "" when it named none and the column's declared
	// collation governs (build.c:4252-4263).
	coll []string
	kind string // "pk" or "u" -- PRAGMA index_list's "origin" column
}

// pragmaFK is one FOREIGN KEY constraint (column-level "col REFERENCES ..."
// or table-level "FOREIGN KEY (...) REFERENCES ..."), as PRAGMA
// foreign_key_list needs it. toCols is nil when the CREATE TABLE text
// omitted the referenced table's own column list -- C SQLite's
// foreign_key_list then reports the "to" column as NULL rather than
// resolving the referenced table's PRIMARY KEY (verified directly against
// mattn/go-sqlite3), so toCols staying nil (rather than being resolved) is
// what pragma.go's pragmaForeignKeyList needs, not a gap.
type pragmaFK struct {
	fromCols           []string
	toTable            string
	toCols             []string // nil if the SQL text omitted the column list
	onUpdate, onDelete string   // "NO ACTION" unless a clause overrides it
	// deferred is true for "DEFERRABLE INITIALLY DEFERRED" -- the one
	// DEFERRABLE spelling that changes WHEN the constraint is checked (at
	// COMMIT rather than at the end of the statement). "NOT DEFERRABLE",
	// "DEFERRABLE" alone and "DEFERRABLE INITIALLY IMMEDIATE" all leave it
	// false, which is C SQLite's default. Read by fk.go's enforcement,
	// which declines a deferred constraint inside an open transaction rather
	// than checking it at the wrong time; PRAGMA foreign_key_list does not
	// report it at all.
	deferred bool
}

// pragmaTableDef is parsePragmaTableDef's result: everything pragma.go's
// table_info/table_xinfo/index_list/index_info/index_xinfo/foreign_key_list
// (and pragma_table_list.go's table_list) need from one CREATE TABLE
// statement's stored SQL text.
type pragmaTableDef struct {
	cols    []pragmaColumn
	pkPos   map[string]int // lower-cased column name -> 1-based PRIMARY KEY position; 0 (absent) if not part of the PK
	autoIdx []pragmaAutoIndexSpec
	fks     []pragmaFK // declaration order (column-level and table-level FOREIGN KEY constraints interleaved as written)
	// withoutRowid / strict echo the CREATE TABLE's own trailing table-option
	// clauses (see parseTableTailClauses, schema_write.go) -- PRAGMA
	// table_list's own "wr"/"strict" columns need exactly these two, and
	// nothing else here currently exposes them standalone.
	withoutRowid bool
	strict       bool
	// rowidAlias is the column whose PRIMARY KEY collapsed into the rowid
	// alias ("" if none) -- exactly the case in which this table's PRIMARY
	// KEY gets NO entry in autoIdx, because the table's own b-tree key IS it.
	// PRAGMA foreign_key_check needs the distinction: a foreign key naming a
	// parent's rowid-alias column (or naming no parent column at all when the
	// parent has one) resolves to a ROWID seek, which needs no index and can
	// therefore never be a "foreign key mismatch".
	rowidAlias string
}

// pragmaStdTypeNames is SQLite's own standard-type-name list (sqlite3StdTypeName):
// a declared type matching one of these case-insensitively is reported by PRAGMA
// table_info/table_xinfo in this CANONICAL UPPER-CASE spelling, while every other
// declared type is reported verbatim. Verified directly against
// mattn/go-sqlite3: "a integer" reports INTEGER and "c int" reports INT, but
// "e DoUbLe" reports DoUbLe, "d varchar(10)" reports varchar(10), and
// "g numeric"/"h decimal"/"i boolean"/"j date" all stay lower-case -- so this is
// NOT a blanket upper-casing, and the list is exactly these six.
var pragmaStdTypeNames = map[string]string{
	"ANY":     "ANY",
	"BLOB":    "BLOB",
	"INT":     "INT",
	"INTEGER": "INTEGER",
	"REAL":    "REAL",
	"TEXT":    "TEXT",
}

// pragmaDeclTypeText is a column's declared type as table_info reports it: the
// canonical spelling for one of pragmaStdTypeNames, otherwise the verbatim
// source span of its type tokens -- C preserves interior spacing ("int
// unsigned" with three spaces stays so), which a token re-join would collapse.
// The span is then dequoted, as sqlite3AddColumn calls sqlite3Dequote on it
// (dequoteTypeSpan).
func pragmaDeclTypeText(createSQL string, typeToks []token) string {
	if len(typeToks) == 0 {
		return ""
	}
	span := joinTypeTokens(typeToks)
	start, end := typeToks[0].Start, typeToks[len(typeToks)-1].End
	if start >= 0 && end <= len(createSQL) && start < end {
		span = dequoteTypeSpan(createSQL[start:end])
	}
	// A type token that swallowed a trailing ALWAYS gets it trimmed back off --
	// BEFORE the canonical-spelling lookup, which is how "int<7 spaces>Always"
	// ends up reported as "INT" rather than either the verbatim text or "int".
	if stripped, ok := stripTypeTokenAlways(span); ok {
		span = stripped
	}
	if canon, ok := pragmaStdTypeNames[strings.ToUpper(span)]; ok {
		return canon
	}
	return span
}

// dequoteTypeSpan is sqlite3Dequote over a declared-type source span. A type can
// be written quoted and C reports it stripped:
//
//	a "weird type", b [brack et], c `back tick`, d 'sing le', e "has""quote"
//
// reports `weird type`, `brack et`, `back tick`, `sing le` and `has"quote`.
// Interior spacing survives, and the scan stops at the first unescaped close
// quote, so `i "a" "b"` reports just `a`. Unquoted text is unchanged. Unlike
// the C, this also stops at the end of the string.
func dequoteTypeSpan(s string) string {
	if s == "" {
		return s
	}
	quote := s[0]
	switch quote {
	case '\'', '"', '`':
	case '[':
		quote = ']'
	default:
		return s
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != quote {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == quote {
			b.WriteByte(quote)
			i++
			continue
		}
		break
	}
	return b.String()
}

// stripTypeTokenAlways implements C's trim of a trailing ALWAYS from a declared
// type, reporting whether it applied.
//
// The grammar lets a type token absorb the ALWAYS of "GENERATED ALWAYS AS", so
// C trims a type whose last six bytes are "always" (any case), with the
// whitespace before it, when the untrimmed type is at least 16 bytes
// (len("GENERATED ALWAYS")):
//
//	int<6 spaces>Always   (15)  reported verbatim
//	int<7 spaces>Always   (16)  reported INT
//	int<7 spaces>Zlways   (16)  reported verbatim  -- not the keyword
//	text<6 spaces>Always  (16)  reported TEXT      -- canonicalized after the trim
//	foo<7 spaces>Always   (16)  reported foo       -- no canonical form to take
//	intXXXXXXXAlways      (16)  reported intXXXXXXX -- no whitespace to trim
//	alwaysalwaysalways    (18)  reported alwaysalways -- one suffix only
//
// A real "GENERATED ALWAYS AS (...)" never reaches here; the parser consumes it
// as the constraint.
func stripTypeTokenAlways(t string) (string, bool) {
	const kw = "always"
	if len(t) < 16 || !equalFoldName(t[len(t)-len(kw):], kw) {
		return t, false
	}
	return strings.TrimRight(t[:len(t)-len(kw)], " \t\n\v\f\r"), true
}

// pragmaIndexCol is one key column of an index, as PRAGMA index_info/
// index_xinfo need it. coll is the key's OWN explicit "COLLATE x" spelled as
// written, or "" when the key wrote none (in which case index_xinfo falls back
// to the column's declared collation -- see pragmaIndexInfo).
type pragmaIndexCol struct {
	name string
	desc bool
	coll string
}

// addAutoIndex appends spec unless an earlier constraint in the same CREATE
// TABLE already implied an index over the same columns, in which case C builds
// one (as buildAutoIndexes does). The first entry survives with its
// sqlite_autoindex_<table>_<N> position, but a later PRIMARY KEY promotes its
// origin to "pk": "CREATE TABLE t(c, d, UNIQUE(c,d), PRIMARY KEY(c,d))" has one
// sqlite_autoindex_t_1 with origin='pk'.
//
// Columns are compared positionally by name and effective collation, as
// sqlite3CreateIndex does (build.c:4344-4352), so UNIQUE(c,d) and UNIQUE(d,c)
// stay two indexes, as do UNIQUE(a COLLATE nocase) and UNIQUE(a).
func (def *pragmaTableDef) addAutoIndex(spec pragmaAutoIndexSpec) {
	for i, prior := range def.autoIdx {
		if len(prior.cols) != len(spec.cols) {
			continue
		}
		same := true
		for k := range spec.cols {
			if !equalFoldName(prior.cols[k], spec.cols[k]) ||
				!strings.EqualFold(def.autoIndexColl(prior, k), def.autoIndexColl(spec, k)) {
				same = false
				break
			}
		}
		if same {
			if spec.kind == "pk" {
				def.autoIdx[i].kind = "pk"
			}
			return
		}
	}
	def.autoIdx = append(def.autoIdx, spec)
}

// autoIndexColl is the collation spec's k-th key column is indexed under: the
// constraint's own COLLATE, else the column's declared one, else BINARY
// (build.c:4252-4265).
func (def *pragmaTableDef) autoIndexColl(spec pragmaAutoIndexSpec, k int) string {
	if k < len(spec.coll) && spec.coll[k] != "" {
		return spec.coll[k]
	}
	for _, c := range def.cols {
		if equalFoldName(c.name, spec.cols[k]) && c.collateSrc != "" {
			return c.collateSrc
		}
	}
	return "BINARY"
}

// matchParen returns the index of the ")" token matching toks[openIdx] (a
// "("), or -1 if unbalanced.
func matchParen(toks []token, openIdx int) int {
	depth := 0
	for i := openIdx; i < len(toks); i++ {
		if toks[i].kind != tkPunct {
			continue
		}
		switch toks[i].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitTopLevelCommas splits toks (already the tokens strictly INSIDE one
// pair of parens) into comma-separated segments, ignoring commas nested
// inside a deeper paren level -- e.g. a column's own "VARCHAR(10)" type or a
// constraint's own "(a, b)" column list must not split the outer list.
func splitTopLevelCommas(toks []token) [][]token {
	var segments [][]token
	var cur []token
	depth := 0
	for _, t := range toks {
		switch {
		case t.kind == tkPunct && t.text == "(":
			depth++
			cur = append(cur, t)
		case t.kind == tkPunct && t.text == ")":
			depth--
			cur = append(cur, t)
		case t.kind == tkPunct && t.text == "," && depth == 0:
			segments = append(segments, cur)
			cur = nil
		default:
			cur = append(cur, t)
		}
	}
	segments = append(segments, cur)
	return segments
}

// pragmaColConstraintStart mirrors sql_parser.go's colConstraintStart: the
// keywords that end a column definition's own declared-type token run.
//
// It had DRIFTED from the set it claims to mirror, missing CONSTRAINT, so a
// named constraint was read as part of the type: "PRAGMA table_info" reported
// "id INTEGER CONSTRAINT cx NOT NULL PRIMARY KEY" as declaring the type
// "INTEGER CONSTRAINT cx" where C SQLite says "INTEGER". NULL is in both
// sets now as well -- SQLite's grammar has a bare "NULL" column constraint
// (parse.y's `ccons ::= NULL onconf`), so "id INTEGER NULL" declares INTEGER.
var pragmaColConstraintStart = map[string]bool{
	"PRIMARY": true, "NOT": true, "NULL": true, "UNIQUE": true, "CHECK": true,
	"CONSTRAINT": true, "DEFAULT": true, "COLLATE": true, "REFERENCES": true,
	"GENERATED": true, "AS": true,
}

// parsePragmaTableDef parses a CREATE TABLE statement's stored SQL into a
// pragmaTableDef (see the file comment for why it is a separate parse).
//
// WITHOUT ROWID (sqlTextTableIsWithoutRowid) changes two things: a PRIMARY KEY
// column is implicitly NOT NULL (applied as a post-pass), and the
// single-INTEGER-column rowid-alias rule never applies, so the PRIMARY KEY
// always gets an automatic index entry. C's index_list/index_info/index_xinfo
// report it as sqlite_autoindex_<table>_<N>, origin='pk', even though no schema
// row backs it (on the write side it is indexMeta.isTablePK).
func parsePragmaTableDef(createSQL string) (*pragmaTableDef, error) {
	withoutRowid := sqlTextTableIsWithoutRowid(createSQL)
	toks, err := lex(createSQL)
	if err != nil {
		return nil, fmt.Errorf("parsing CREATE TABLE: %w", err)
	}
	i := 0
	kw := func(s string) bool {
		if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == s {
			i++
			return true
		}
		return false
	}
	if !kw("CREATE") {
		return nil, fmt.Errorf("CREATE TABLE: expected CREATE")
	}
	kw("TEMP")
	kw("TEMPORARY")
	if !kw("TABLE") {
		return nil, fmt.Errorf("CREATE TABLE: expected TABLE")
	}
	if kw("IF") {
		kw("NOT")
		kw("EXISTS")
	}
	// A single-quoted table/column NAME is an identifier in a CREATE TABLE --
	// see sql_parser.go's asCreateTableIdent, which this must stay in step
	// with so PRAGMA table_info describes an fts3/fts4 shadow table exactly
	// as the read path resolves it.
	if i >= len(toks) {
		return nil, fmt.Errorf("CREATE TABLE: expected table name")
	}
	if _, ok := asCreateTableIdent(toks[i]); !ok {
		return nil, fmt.Errorf("CREATE TABLE: expected table name")
	}
	i++
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i++
		if !(i < len(toks) && toks[i].kind == tkIdent) {
			return nil, fmt.Errorf("CREATE TABLE: expected table name after schema qualifier")
		}
		i++
	}
	if !(i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "(") {
		return nil, fmt.Errorf("CREATE TABLE: expected '('")
	}
	end := matchParen(toks, i)
	if end < 0 {
		return nil, fmt.Errorf("CREATE TABLE: unterminated column list")
	}
	segments := splitTopLevelCommas(toks[i+1 : end])

	def := &pragmaTableDef{pkPos: map[string]int{}}
	// rowidAliasName is the column (if any) whose PRIMARY KEY collapsed into
	// the rowid alias, recorded by both the column-level and the table-level
	// PRIMARY KEY branches below. Consulted only by the STRICT/WITHOUT ROWID
	// implicit-NOT NULL post-pass at the end, which must exempt it: real
	// SQLite leaves an INTEGER PRIMARY KEY nullable (NULL means
	// "auto-assign") even in a STRICT table -- verified directly, PRAGMA
	// table_info reports notnull=0 for it.
	rowidAliasName := ""
	tableConstraintKeywords := map[string]bool{
		"PRIMARY": true, "UNIQUE": true, "CHECK": true, "FOREIGN": true, "CONSTRAINT": true,
	}

	for _, seg := range segments {
		if len(seg) == 0 {
			continue
		}
		if seg[0].kind == tkString {
			seg[0], _ = asCreateTableIdent(seg[0])
		}
		first := seg[0]
		if first.kind == tkIdent && tableConstraintKeywords[first.upper()] {
			kwTok := first
			rest := seg
			if kwTok.upper() == "CONSTRAINT" {
				k := 1
				// The name may be a single-quoted STRING, which is a NAME in
				// this position -- see pragmaConstraintName. Failing to skip it
				// left kwTok pointing at the string, whose upper() is "", so
				// the switch below matched nothing and the WHOLE constraint was
				// dropped: "CONSTRAINT 'fk1' FOREIGN KEY(y) REFERENCES par(x)"
				// was not reported by foreign_key_list and not enforced at all,
				// while C SQLite rejects the orphan row.
				if k < len(seg) {
					if _, ok := pragmaConstraintName(seg[k]); ok {
						k++
					}
				}
				if k >= len(seg) {
					continue
				}
				kwTok = seg[k]
				rest = seg[k:]
			}
			switch kwTok.upper() {
			case "PRIMARY":
				names, pkDesc, pkColl := extractParenIdentListFull(rest)
				if len(names) == 0 {
					continue
				}
				for pos, nm := range names {
					def.pkPos[r33sFoldIdent(nm)] = pos + 1
				}
				// A WITHOUT ROWID table's PRIMARY KEY still appears in the
				// index pragmas (sqlite_autoindex_<table>_<N>, origin='pk'),
				// so it is added like any other PRIMARY KEY -- and the
				// single-INTEGER-column rowid-alias exemption never applies,
				// since there is no rowid.
				if len(names) == 1 && !withoutRowid {
					collapsed := false
					for _, c := range def.cols {
						if equalFoldName(c.name, names[0]) && equalFoldName(strings.TrimSpace(c.declType), "INTEGER") {
							collapsed = true
							break
						}
					}
					if collapsed {
						rowidAliasName = names[0]
						continue
					}
				}
				def.addAutoIndex(pragmaAutoIndexSpec{cols: names, desc: pkDesc, coll: pkColl, kind: "pk"})
			case "UNIQUE":
				names, uDesc, uColl := extractParenIdentListFull(rest)
				if len(names) == 0 {
					continue
				}
				def.addAutoIndex(pragmaAutoIndexSpec{cols: names, desc: uDesc, coll: uColl, kind: "u"})
			case "FOREIGN":
				if fk, ok := parseTableLevelFK(rest); ok {
					def.fks = append(def.fks, fk)
				}
			}
			continue
		}

		// Column definition.
		name := first.text
		j := 1
		var typeToks []token
		for j < len(seg) && !(seg[j].kind == tkIdent && pragmaColConstraintStart[seg[j].upper()]) {
			typeToks = append(typeToks, seg[j])
			j++
		}
		col := pragmaColumn{name: name, declType: pragmaDeclTypeText(createSQL, typeToks)}
		isColPK := false
		pkDesc := false
		for j < len(seg) {
			t := seg[j]
			switch {
			case t.kind == tkIdent && t.upper() == "PRIMARY":
				isColPK = true
				j++
				if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "KEY" {
					j++
				}
				if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "DESC" {
					pkDesc = true
					j++
				} else if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "ASC" {
					j++
				}
			case t.kind == tkIdent && t.upper() == "NOT" && j+1 < len(seg) && seg[j+1].kind == tkIdent && seg[j+1].upper() == "NULL":
				col.notNull = true
				j += 2
			case t.kind == tkIdent && t.upper() == "UNIQUE":
				def.addAutoIndex(pragmaAutoIndexSpec{cols: []string{name}, kind: "u"})
				j++
			case t.kind == tkIdent && t.upper() == "DEFAULT":
				j++
				text, nj := extractDefaultText(createSQL, seg, j)
				col.hasDefault = true
				col.dflt = text
				j = nj
			case t.kind == tkIdent && t.upper() == "COLLATE":
				// Recorded (upper-cased) rather than skipped: PRAGMA
				// foreign_key_check compares a child value against a parent
				// column under THAT column's collating sequence -- see
				// pragmaColumn.collate.
				j++
				if j < len(seg) && seg[j].kind == tkIdent {
					col.collate = strings.ToUpper(seg[j].text)
					col.collateSrc = seg[j].text
					j++
				}
			case t.kind == tkIdent && (t.upper() == "GENERATED" || t.upper() == "AS"):
				// A GENERATED column, in either spelling ("GENERATED ALWAYS AS
				// (expr)" or the bare "AS (expr)"), optionally followed by
				// STORED or VIRTUAL (VIRTUAL is the default). Recorded rather
				// than skipped: PRAGMA table_info OMITS generated columns
				// entirely and table_xinfo reports the kind in its "hidden"
				// column -- see pragmaTableInfo.
				col.generated = true
				j++
				if t.upper() == "GENERATED" {
					if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "ALWAYS" {
						j++
					}
					if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "AS" {
						j++
					}
				}
				// Skip the parenthesized expression, then look for STORED.
				if j < len(seg) && seg[j].kind == tkPunct && seg[j].text == "(" {
					if close := matchParen(seg, j); close >= 0 {
						j = close + 1
					} else {
						j = len(seg)
					}
				}
				if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "STORED" {
					col.generatedStored = true
					j++
				} else if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "VIRTUAL" {
					j++
				}
			case t.kind == tkIdent && t.upper() == "REFERENCES":
				j++
				fk, nj := parseReferencesClause(seg, j, name)
				def.fks = append(def.fks, fk)
				j = nj
			default:
				j++
			}
		}
		rowidAlias := !withoutRowid && isColPK && !pkDesc && equalFoldName(strings.TrimSpace(col.declType), "INTEGER")
		def.cols = append(def.cols, col)
		if isColPK {
			def.pkPos[r33sFoldIdent(name)] = 1
			// rowidAlias is always false when withoutRowid (see its own
			// definition above), so this PRIMARY KEY always gets an
			// automatic index for a WITHOUT ROWID table -- see the
			// table-level PRIMARY KEY case's identical doc comment above for
			// why (verified directly against C SQLite's own PRAGMA
			// index_list/index_info).
			if !rowidAlias {
				def.addAutoIndex(pragmaAutoIndexSpec{cols: []string{name}, kind: "pk"})
			} else {
				rowidAliasName = name
			}
		}
	}
	strict := tailDeclaresStrict(toks[end+1:])
	def.withoutRowid = withoutRowid
	def.strict = strict
	if withoutRowid || strict {
		// WITHOUT ROWID's and STRICT's implicit "every PRIMARY KEY column is
		// NOT NULL", applied once pkPos is fully populated. "CREATE TABLE
		// pk1(a INT PRIMARY KEY, b TEXT) STRICT" reports notnull=1 for a
		// (0 without STRICT). The rowid alias is exempt; a WITHOUT ROWID table
		// has none.
		for i := range def.cols {
			if equalFoldName(def.cols[i].name, rowidAliasName) {
				continue
			}
			if def.pkPos[r33sFoldIdent(def.cols[i].name)] > 0 {
				def.cols[i].notNull = true
			}
		}
	}
	def.rowidAlias = rowidAliasName
	return def, nil
}

// extractDefaultText returns the raw source text of the default expression
// starting at seg[j] (just after DEFAULT), and the index just past it. A
// parenthesized expression has its outer parens stripped, as C reports the
// inner text; every other form is returned verbatim, quotes included.
//
// A signed form spans the sign and the whole term (parse.y:394-399):
//
//	ccons ::= DEFAULT PLUS(A) scantok(Z) term(X).
//	                            {sqlite3AddDefaultValue(pParse,X,A.z,&Z.z[Z.n]);}
//	ccons ::= DEFAULT MINUS(A) scantok(Z) term(X). {
//	  Expr *p = sqlite3PExpr(pParse, TK_UMINUS, X, 0);
//	  sqlite3AddDefaultValue(pParse,p,A.z,&Z.z[Z.n]);
//	}
//
// "term" is NULL|FLOAT|BLOB|STRING|INTEGER|CTIME_KW|QNUMBER (parse.y:1193-1195,
// :1329, :2141) -- isDefaultTerm's set, which parseColumnDefault also matches
// on. So "a TEXT DEFAULT -'abc'" reports "-'abc'".
func extractDefaultText(src string, seg []token, j int) (string, int) {
	if j >= len(seg) {
		return "", j
	}
	t := seg[j]
	if t.kind == tkPunct && t.text == "(" {
		end := matchParen(seg, j)
		if end < 0 {
			end = len(seg) - 1
		}
		inner := strings.TrimSpace(src[t.End:seg[end].Start])
		return inner, end + 1
	}
	if t.kind == tkPunct && (t.text == "-" || t.text == "+") && j+1 < len(seg) && isDefaultTerm(seg[j+1]) {
		return src[t.Start:seg[j+1].End], j + 2
	}
	return src[t.Start:t.End], j + 1
}

// parseFKTrailingClauses consumes zero or more "ON DELETE <action>"/
// "ON UPDATE <action>"/"MATCH <name>"/"[NOT] DEFERRABLE [INITIALLY
// DEFERRED|IMMEDIATE]" clauses from seg starting at j, filling in fk's
// onDelete/onUpdate (MATCH's own argument is intentionally discarded: real
// SQLite's foreign_key_list always reports "NONE" for the match column
// regardless of what was written -- verified directly against
// mattn/go-sqlite3). Returns the index just past the last clause matched.
func parseFKTrailingClauses(seg []token, j int, fk *pragmaFK) int {
	for j < len(seg) {
		t := seg[j]
		if t.kind != tkIdent {
			break
		}
		switch t.upper() {
		case "ON":
			if j+1 >= len(seg) || seg[j+1].kind != tkIdent {
				return j
			}
			which := seg[j+1].upper()
			if which != "DELETE" && which != "UPDATE" {
				return j
			}
			action, nj := parseFKAction(seg, j+2)
			if which == "DELETE" {
				fk.onDelete = action
			} else {
				fk.onUpdate = action
			}
			j = nj
		case "MATCH":
			j += 2 // "MATCH <name>"; the name itself is discarded (see doc comment)
		case "DEFERRABLE":
			j++
			if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "INITIALLY" {
				if j+1 < len(seg) && seg[j+1].kind == tkIdent && seg[j+1].upper() == "DEFERRED" {
					fk.deferred = true
				}
				j += 2
			}
		case "NOT":
			if j+1 < len(seg) && seg[j+1].kind == tkIdent && seg[j+1].upper() == "DEFERRABLE" {
				j += 2
				if j < len(seg) && seg[j].kind == tkIdent && seg[j].upper() == "INITIALLY" {
					j += 2
				}
			} else {
				return j
			}
		default:
			return j
		}
	}
	return j
}

// parseFKAction parses one ON DELETE/ON UPDATE action keyword sequence
// ("CASCADE", "RESTRICT", "SET NULL", "SET DEFAULT", or "NO ACTION")
// starting at seg[k], returning its canonical text and the index just past
// it. An unrecognized/missing action defaults to "NO ACTION" without
// consuming anything, matching C SQLite's own default.
func parseFKAction(seg []token, k int) (string, int) {
	if k >= len(seg) || seg[k].kind != tkIdent {
		return "NO ACTION", k
	}
	switch seg[k].upper() {
	case "CASCADE":
		return "CASCADE", k + 1
	case "RESTRICT":
		return "RESTRICT", k + 1
	case "SET":
		if k+1 < len(seg) && seg[k+1].kind == tkIdent {
			switch seg[k+1].upper() {
			case "NULL":
				return "SET NULL", k + 2
			case "DEFAULT":
				return "SET DEFAULT", k + 2
			}
		}
		return "NO ACTION", k + 1
	case "NO":
		if k+1 < len(seg) && seg[k+1].kind == tkIdent && seg[k+1].upper() == "ACTION" {
			return "NO ACTION", k + 2
		}
		return "NO ACTION", k + 1
	}
	return "NO ACTION", k
}

// pragmaConstraintName reads a name at t: an identifier in any of its four
// spellings, or a single-quoted string, which the grammar's "nm" production
// (ID|STRING|JOIN_KW) also accepts and sqlite3StringToId turns into an
// identifier. So 'pone' in "REFERENCES 'pone'" names the table pone.
//
// It is local rather than asCreateTableIdent because this file is an
// independent parse. Positions it covers, all enforcing and reported dequoted
// by foreign_key_list:
//
//	CREATE TABLE 'pone'(a REFERENCES 'pone', b, PRIMARY KEY(b))
//	CREATE TABLE k1(y REFERENCES par('x'))
//	CREATE TABLE k5(y, FOREIGN KEY('y') REFERENCES 'par'('x'))
//	CREATE TABLE c2(u, v, FOREIGN KEY('u','v') REFERENCES "p2"('a',[b]))
//	CREATE TABLE kA(y, CONSTRAINT 'fk1' FOREIGN KEY(y) REFERENCES par(x))
//	CREATE TABLE k6(y REFERENCES 'par'('x') ON DELETE CASCADE)
//
// A missing parent keeps its name: "REFERENCES 'no such parent'" is "no such
// table: main.no such parent". Without this a quoted constraint name made the
// table-level FOREIGN KEY unrecognizable and silently unenforced.
func pragmaConstraintName(t token) (string, bool) {
	switch t.kind {
	case tkIdent:
		return t.text, true
	case tkString:
		return t.str, true
	}
	return "", false
}

// parseReferencesClause parses one column-level "REFERENCES table[(cols)]
// [clauses...]" starting at seg[j] (the token right after the REFERENCES
// keyword), for the single column fromCol. Returns the resulting pragmaFK
// and the segment index just past the clause.
func parseReferencesClause(seg []token, j int, fromCol string) (pragmaFK, int) {
	fk := pragmaFK{fromCols: []string{fromCol}, onUpdate: "NO ACTION", onDelete: "NO ACTION"}
	if j < len(seg) {
		if name, ok := pragmaConstraintName(seg[j]); ok {
			fk.toTable = name
			j++
		}
	}
	if j < len(seg) && seg[j].kind == tkPunct && seg[j].text == "(" {
		end := matchParen(seg, j)
		if end > j {
			for k := j + 1; k < end; k++ {
				if name, ok := pragmaConstraintName(seg[k]); ok {
					fk.toCols = append(fk.toCols, name)
				}
			}
			j = end + 1
		}
	}
	j = parseFKTrailingClauses(seg, j, &fk)
	return fk, j
}

// parseTableLevelFK parses one table-level "FOREIGN KEY (cols) REFERENCES
// table[(cols)] [clauses...]" constraint (rest starts at the FOREIGN
// keyword, any leading "CONSTRAINT name" already stripped by the caller).
func parseTableLevelFK(rest []token) (pragmaFK, bool) {
	i := 0
	if !(i < len(rest) && rest[i].kind == tkIdent && rest[i].upper() == "FOREIGN") {
		return pragmaFK{}, false
	}
	i++
	if i < len(rest) && rest[i].kind == tkIdent && rest[i].upper() == "KEY" {
		i++
	}
	if !(i < len(rest) && rest[i].kind == tkPunct && rest[i].text == "(") {
		return pragmaFK{}, false
	}
	end := matchParen(rest, i)
	if end < 0 {
		return pragmaFK{}, false
	}
	var fromCols []string
	for k := i + 1; k < end; k++ {
		if name, ok := pragmaConstraintName(rest[k]); ok {
			fromCols = append(fromCols, name)
		}
	}
	i = end + 1
	if !(i < len(rest) && rest[i].kind == tkIdent && rest[i].upper() == "REFERENCES") {
		return pragmaFK{}, false
	}
	i++
	fk := pragmaFK{fromCols: fromCols, onUpdate: "NO ACTION", onDelete: "NO ACTION"}
	if i < len(rest) {
		if name, ok := pragmaConstraintName(rest[i]); ok {
			fk.toTable = name
			i++
		}
	}
	if i < len(rest) && rest[i].kind == tkPunct && rest[i].text == "(" {
		e2 := matchParen(rest, i)
		if e2 > i {
			for k := i + 1; k < e2; k++ {
				if name, ok := pragmaConstraintName(rest[k]); ok {
					fk.toCols = append(fk.toCols, name)
				}
			}
			i = e2 + 1
		}
	}
	parseFKTrailingClauses(rest, i, &fk)
	return fk, true
}

// parsePragmaIndexHeader parses just enough of an explicit "CREATE [UNIQUE]
// INDEX ... ON table (...) [WHERE ...]" statement's SQL text to recover
// whether it's UNIQUE and whether it's partial (has a WHERE clause) --
// PRAGMA index_list's own "unique"/"partial" columns. This engine's own
// CREATE INDEX write path (index_write.go) always rejects a WHERE clause
// outright, so partial is always false for any index this engine's own
// write path could have created; it is still detected here for robustness
// (e.g. a database file this engine only READS, via OpenWrite recovering an
// existing file another engine wrote -- out of scope to fully support, but
// this detection doesn't hurt).
func parsePragmaIndexHeader(sqlText string) (unique, partial bool) {
	toks, err := lex(sqlText)
	if err != nil {
		return false, false
	}
	i := 0
	if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "CREATE" {
		i++
	}
	if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "UNIQUE" {
		unique = true
		i++
	}
	depth := 0
	seenColumnList := false
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.kind == tkPunct && t.text == "(":
			depth++
		case t.kind == tkPunct && t.text == ")":
			depth--
			if depth == 0 {
				seenColumnList = true
			}
		case depth == 0 && seenColumnList && t.kind == tkIdent && t.upper() == "WHERE":
			partial = true
			return unique, partial
		}
	}
	return unique, partial
}

// parsePragmaIndexColumns parses an explicit CREATE INDEX statement's SQL into
// its key-column names, DESC flags and explicit COLLATE names ("" where the
// key inherits the column's collation). index_info/index_xinfo need them, and
// so does foreign_key_check, which must match a parent index's collation
// against the parent column's: C rejects the FK with "foreign key mismatch"
// when they differ.
func parsePragmaIndexColumns(sqlText string) (names []string, descs []bool, colls []string, unique bool, err error) {
	toks, lerr := lex(sqlText)
	if lerr != nil {
		return nil, nil, nil, false, lerr
	}
	i := 0
	if !(i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "CREATE") {
		return nil, nil, nil, false, fmt.Errorf("CREATE INDEX: expected CREATE")
	}
	i++
	if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "UNIQUE" {
		unique = true
		i++
	}
	if !(i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "INDEX") {
		return nil, nil, nil, false, fmt.Errorf("CREATE INDEX: expected INDEX")
	}
	i++
	if i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "IF" {
		i += 3 // IF NOT EXISTS
	}
	if i < len(toks) && toks[i].kind == tkIdent {
		i++
	}
	if i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "." {
		i++
		if i < len(toks) && toks[i].kind == tkIdent {
			i++
		}
	}
	if !(i < len(toks) && toks[i].kind == tkIdent && toks[i].upper() == "ON") {
		return nil, nil, nil, false, fmt.Errorf("CREATE INDEX: expected ON")
	}
	i++
	if i < len(toks) && toks[i].kind == tkIdent {
		i++ // table name
	}
	if !(i < len(toks) && toks[i].kind == tkPunct && toks[i].text == "(") {
		return nil, nil, nil, false, fmt.Errorf("CREATE INDEX: expected '('")
	}
	end := matchParen(toks, i)
	if end < 0 {
		return nil, nil, nil, false, fmt.Errorf("CREATE INDEX: unterminated column list")
	}
	for _, seg := range splitTopLevelCommas(toks[i+1 : end]) {
		if len(seg) == 0 {
			continue
		}
		desc := false
		for _, t := range seg[1:] {
			if t.kind == tkIdent && t.upper() == "DESC" {
				desc = true
			}
		}
		name, coll := indexKeyColumnName(seg)
		names = append(names, name)
		descs = append(descs, desc)
		colls = append(colls, coll)
	}
	return names, descs, colls, unique, nil
}

// indexKeyColumnName returns the column name one CREATE INDEX key entry names
// (and its explicit COLLATE, upper-cased, or ""), or "" when the entry is an
// expression -- which index_info reports as cid -2 with a NULL name ("ON
// t(b, a*2)" gives (0, 1, 'b'), (1, -2, NULL)). A plain column is a single
// identifier optionally followed by COLLATE <name> and/or ASC/DESC, as
// tryParseSimpleIndexColumn accepts.
func indexKeyColumnName(seg []token) (name, collate string) {
	if len(seg) == 0 || seg[0].kind != tkIdent {
		return "", ""
	}
	for i := 1; i < len(seg); i++ {
		t := seg[i]
		if t.kind != tkIdent {
			return "", ""
		}
		switch t.upper() {
		case "ASC", "DESC":
		case "COLLATE":
			i++
			if i < len(seg) && seg[i].kind == tkIdent {
				// Spelled as written, not upper-cased: PRAGMA index_xinfo
				// reports it verbatim (see pragmaColumn.collateSrc). Its only
				// other consumer, pragmaForeignKeyCheck, compares it with
				// strings.EqualFold, so the case carries no meaning there.
				collate = seg[i].text
			}
		default:
			return "", ""
		}
	}
	return seg[0].text, collate
}

// autoIndexSuffixNumber recovers N from an automatic index's own name
// ("sqlite_autoindex_<table>_<N>") -- the 1-based position into that
// table's own pragmaTableDef.autoIdx this index corresponds to (see
// parsePragmaTableDef's doc comment: this file numbers automatic indexes in
// exactly the same order index_write.go's buildAutoIndexes does). Taking
// just the trailing "_<N>" (rather than requiring an exact
// "sqlite_autoindex_<table>_" prefix match) is robust to a table name that
// itself contains underscores.
func autoIndexSuffixNumber(idxName string) int {
	i := strings.LastIndexByte(idxName, '_')
	if i < 0 || i+1 >= len(idxName) {
		return -1
	}
	n, err := strconv.Atoi(idxName[i+1:])
	if err != nil {
		return -1
	}
	return n
}
