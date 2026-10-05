// This file owns fts5's "detail=" setting: the three doclist shapes an fts5
// index can write, and which MATCH queries error on each.
package engine

import (
	"fmt"
	"strings"
)

// fts5Detail is the FTS5_DETAIL_* enum.
type fts5Detail int

const (
	fts5DetailFull    fts5Detail = iota // full (default)
	fts5DetailNone                      // none
	fts5DetailColumns                   // columns
)

// fts5DetailNames is the set of detail= setting names.
// fts5ConfigSetEnum walks the whole table and fails on the SECOND arm that
// accepts, so a value that prefixes two names is an error rather than resolving
// to whichever came first.
var fts5DetailNames = []struct {
	name string
	val  fts5Detail
}{
	{"none", fts5DetailNone},
	{"full", fts5DetailFull},
	{"columns", fts5DetailColumns},
}

// fts5ParseDetail resolves a written detail= value the way fts5ConfigSetEnum
// (ext/fts5/fts5_config.c) does: the value is a case-insensitive PREFIX of an
// enum name, and it is an error unless EXACTLY ONE name accepts it.
//
// So "col", "colu" and "columns" are all FTS5_DETAIL_COLUMNS ("detail=col" is
// what fts5detail.test and fts5fault5.test write), "n"/"no"/"non"/"none" is
// FTS5_DETAIL_NONE, "f"/"fu"/"ful"/"full" is FTS5_DETAIL_FULL -- while the
// EMPTY value prefixes all three and is "malformed detail=... directive", and
// so is anything longer than the name it starts ("columnsX": strnicmp compares
// nEnum bytes, which reaches the name's terminating NUL).
//
// Note this is NOT the prefix rule fts5CanonicalOption implements for the
// option KEY: that one takes the first arm that accepts and never reports an
// ambiguity. The value's rule is the stricter of the two.
func fts5ParseDetail(val string) (fts5Detail, bool) {
	found := false
	var out fts5Detail
	for _, d := range fts5DetailNames {
		if len(val) <= len(d.name) && strings.EqualFold(val, d.name[:len(val)]) {
			if found {
				return 0, false // prefixes more than one name
			}
			found, out = true, d.val
		}
	}
	return out, found
}

// fts5DetailOf resolves the detail= mode from a stored CREATE VIRTUAL TABLE's
// module arguments, without building a whole store -- the read path's entry
// point, so that a table REAL SQLITE wrote is read in the mode it was written
// in. It mirrors fts5TokenizerOf (fts5_match.go): a later detail= overrides an
// earlier one, and a malformed value is reported rather than defaulted, because
// defaulting to full over a detail=none table would ANSWER the very queries
// fts5 rejects.
func fts5DetailOf(args []string) (fts5Detail, error) {
	out := fts5DetailFull
	for _, a := range args {
		key, val, isOpt := fts5SplitOption(strings.TrimSpace(a))
		if !isOpt {
			continue
		}
		if canon, ok := fts5CanonicalOption(key); !ok || canon != "detail" {
			continue
		}
		d, ok := fts5ParseDetail(fts5Dequote(val))
		if !ok {
			return fts5DetailFull, fmt.Errorf("malformed detail=... directive")
		}
		out = d
	}
	return out, nil
}

// ---- the two query refusals ----
//
// Both are fts5 PARSE errors, raised while the expression tree is built, so
// they fire whether or not any row would have matched -- an empty table still
// errors. fts5_query.go raises them from the two places the C does.

// errFts5DetailNoneColumn is sqlite3Fts5ParseSetColset's refusal
// (ext/fts5/fts5_expr.c). It covers every way a column filter can reach an
// expression: the "col:term" and "{a b}:term" forms (the grammar's
// `cnearset ::= colset COLON nearset`), the parenthesized-group form
// (`expr ::= colset COLON LP expr RP`), and the IMPLICIT filter
// sqlite3Fts5ExprNew applies when the MATCH's left operand is a user column
// rather than the table ("SELECT ... WHERE t.a MATCH 'x'"), which goes through
// exactly the same function.
var errFts5DetailNoneColumn = fmt.Errorf("fts5: column queries are not supported (detail=none)")

// fts5DetailPhraseErr is sqlite3Fts5ParseNode's refusal for an FTS5_STRING node
// when eDetail != FTS5_DETAIL_FULL (ext/fts5/fts5_expr.c):
//
//	if( pNear->nPhrase!=1 || pPhrase->nTerm>1
//	 || (pPhrase->nTerm>0 && pPhrase->aTerm[0].bFirst) ){
//	  ... "fts5: %s queries are not supported (detail!=full)",
//	      pNear->nPhrase==1 ? "phrase" : "NEAR"
//
// So a MULTI-TERM phrase ("a b" or a+b), an ANCHORED one (^a, whatever its
// term count) and any NEAR set of more than one phrase are all refused, and
// the word in the message is decided purely by the PHRASE COUNT -- a
// single-phrase NEAR("a b") is reported as a "phrase" query, and an anchored
// term inside a two-phrase NEAR as a "NEAR" one.
func fts5DetailPhraseErr(nPhrase int) error {
	kind := "NEAR"
	if nPhrase == 1 {
		kind = "phrase"
	}
	return fmt.Errorf("fts5: %s queries are not supported (detail!=full)", kind)
}

// ---- fts5vocab ----
//
// fts5vocab reads the index, so it is the one reader that detail= really does
// change: a column number or an offset that the mode never recorded comes back
// NULL, and detail=none collapses the per-column view onto a single row.

// fts5VocabDetailOf resolves the detail= mode of an fts5vocab table's TARGET.
// vtab_fts5vocab.go's fts5VocabTargetColNames answers the same question for the
// tokenizer, off the same stored CREATE.
func (p *ReadOnlyPager) fts5VocabDetailOf(target string) (fts5Detail, error) {
	_, args, ok, err := p.createdVtabDef(target)
	if err != nil || !ok {
		return fts5DetailFull, err
	}
	return fts5DetailOf(args)
}

// fts5VocabTermRows builds one term's fts5vocab rows for the target's detail=
// mode, ported from fts5_vocab.c's fts5VocabNextMethod (which accumulates
// aDoc[]/aCnt[]) and fts5VocabColumnMethod (which decides what each column
// reports). occs is every occurrence of term in %_content, in ascending
// (doc, column, offset) order.
//
// The three modes differ exactly where the INDEX differs (fts5_index.go):
//
//	'row'       doc counts distinct documents in every mode -- one doclist
//	            entry each. cnt counts INSTANCES, which only detail=full
//	            records, so it is NULL under the other two: fts5VocabNextMethod
//	            walks the position list for cnt only when eDetail is FULL, and
//	            fts5VocabColumnMethod's trailing "if( iVal>0 )" leaves an
//	            uncounted zero as NULL rather than 0.
//	'col'       detail=full and detail=columns both give one row per column
//	            the term occurs in, and detail=columns leaves cnt NULL for the
//	            same reason ('row' above). detail=none has no column
//	            information at all, so it books everything under aDoc[0] and
//	            emits a SINGLE row per term whose col is NULL.
//	'instance'  detail=full gives one row per instance; detail=columns one row
//	            per (document, column), with offset NULL; detail=none one row
//	            per document, with both col and offset NULL.
func fts5VocabTermRows(kind fts5VocabKind, detail fts5Detail, term string, occs []fts5VocabOcc, colNames []string) [][]Value {
	txt := func(s string) Value { return Value{Typ: Text, S: []byte(s)} }
	num := func(n int64) Value { return Value{Typ: Int, I: n} }
	null := Value{Typ: Null}
	// cnt is only ever reported under detail=full.
	cnt := func(n int) Value {
		if detail != fts5DetailFull {
			return null
		}
		return num(int64(n))
	}

	var rows [][]Value
	switch kind {
	case fts5VocabRow:
		docSet := map[int64]bool{}
		for _, o := range occs {
			docSet[o.doc] = true
		}
		rows = append(rows, []Value{txt(term), num(int64(len(docSet))), cnt(len(occs))})

	case fts5VocabCol:
		if detail == fts5DetailNone {
			docSet := map[int64]bool{}
			for _, o := range occs {
				docSet[o.doc] = true
			}
			return [][]Value{{txt(term), null, num(int64(len(docSet))), null}}
		}
		byCol := make([][]fts5VocabOcc, len(colNames))
		for _, o := range occs {
			byCol[o.col] = append(byCol[o.col], o)
		}
		for ci, colOccs := range byCol {
			if len(colOccs) == 0 {
				continue
			}
			docSet := map[int64]bool{}
			for _, o := range colOccs {
				docSet[o.doc] = true
			}
			rows = append(rows, []Value{txt(term), txt(colNames[ci]), num(int64(len(docSet))), cnt(len(colOccs))})
		}

	case fts5VocabInstance:
		switch detail {
		case fts5DetailNone:
			// One row per doclist entry, i.e. per document.
			var last int64 = -1
			for _, o := range occs {
				if o.doc == last {
					continue
				}
				last = o.doc
				rows = append(rows, []Value{txt(term), num(o.doc), null, null})
			}
		case fts5DetailColumns:
			// One row per collist entry, i.e. per (document, column).
			lastDoc, lastCol := int64(-1), -1
			for _, o := range occs {
				if o.doc == lastDoc && o.col == lastCol {
					continue
				}
				lastDoc, lastCol = o.doc, o.col
				rows = append(rows, []Value{txt(term), num(o.doc), txt(colNames[o.col]), null})
			}
		default:
			for _, o := range occs {
				rows = append(rows, []Value{txt(term), num(o.doc), txt(colNames[o.col]), num(o.offset)})
			}
		}
	}
	return rows
}
