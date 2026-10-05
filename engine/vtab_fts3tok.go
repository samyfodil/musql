// This file implements the "fts3tokenize" virtual-table module
// (fts3_tokenize_vtab.c): a table that stores NOTHING and exposes an fts3
// tokenizer to SQL.
//
//	CREATE VIRTUAL TABLE t USING fts3tokenize(<tokenizer-name>, <arg>, ...);
//	SELECT token, start, end, position FROM t WHERE input = 'one two three';
//
// Its declared schema is "CREATE TABLE x(input, token, start, end, position)"
// -- five ORDINARY columns, none hidden and none typed, so "SELECT *" returns
// all five and every value's storage class comes from the module. Verified
// against mattn/go-sqlite3 3.53.3 (typeof over an all-text query reports
// text, text, integer, integer, integer).
//
// # The rules, each read off the oracle
//
//   - The module arguments are DEQUOTED individually (fts3tokDequoteArray) and
//     argument 0 NAMES the tokenizer; the rest are that tokenizer's own
//     constructor arguments -- the same handoff sqlite3Fts3InitTokenizer makes
//     for a "tokenize=" specification, which is why both go through
//     fts3NewTokenizer. With NO arguments at all the tokenizer is "simple", so
//     "USING fts3tokenize()" and the bare "USING fts3tokenize" both work.
//   - An unknown tokenizer fails the CREATE ("unknown tokenizer: X"), and so
//     does a tokenizer whose constructor rejects its arguments.
//   - A query MUST carry "input = <expr>". fts3tokBestIndexMethod consumes the
//     first usable EQ constraint on column 0 and sets idxNum 1; with anything
//     else fts3tokFilterMethod returns SQLITE_ERROR, so "SELECT * FROM t" and
//     "SELECT * FROM t WHERE token = 'a'" are both errors (verified).
//   - rowid counts from 1: fts3tokFilterMethod ends by calling xNext, which
//     increments iRowid from 0 before fetching. Verified -- "SELECT rowid, *"
//     over 'a b c' reports 1, 2, 3.
//   - The table is READ-ONLY (no xUpdate): an INSERT into it is an error.
//
// # The OMIT, and the one shape that still declines without it
//
// Real fts3tokenize OMITS the consumed constraint from the outer WHERE
// (fts3tokBestIndexMethod's "pInfo->aConstraintUsage[i].omit = 1",
// fts3_tokenize_vtab.c:248) and reports the "input" column as
// sqlite3_result_text(..., -1) over a copy of sqlite3_value_text()
// (fts3_tokenize_vtab.c:388) -- i.e. the value's TEXT RENDERING,
// strlen-terminated -- while its TOKENIZER is opened with the full
// sqlite3_value_bytes() count (fts3_tokenize_vtab.c:349-356).
//
// Both halves matter, and both were verified against the 3.53.3 oracle:
//
//	WHERE input = 123            -> 123 | 123 | 0 | 3 | 0    (the row's input is TEXT '123')
//	WHERE input = x'6120620063'  -> 'a b' | 'b<NUL>c' | 2 | 5 | 1
//
// The blob case is the sharper one: the "input" column stops at the NUL while
// the TOKENS run past it (NUL is not in simpleDelim's table, so it is a token
// character).
//
// So the reported value is a TRANSFORMATION of the constrained one, and this
// engine re-applies every WHERE conjunct over the rows a module produced (see
// vtab.go) -- which is exactly why generate_series' Column() echoes its hidden
// inputs, and why re-applying "input = 123" over a row whose input is the TEXT
// '123' drops it. Every non-TEXT and every NUL-bearing input therefore used to
// decline here.
//
// It does not any more: vtab_omit.go removes the consumed conjunct from the
// residual WHERE at compile time and tells this module it did
// (VtabConstraint.Omitted), which is what BestIndex below turns into idxNum
// fts3TokIdxInputOmitted. The DECLINE is kept for idxNum fts3TokIdxInput --
// the constraint was consumed but NOT dropped -- because there the re-applied
// conjunct is still standing and answering would be a row short. That is the
// safe direction: a path that never reaches the compile-time rewrite loses an
// answer rather than dropping a row.
//
// OmittedConjunct is this module's half of that bargain (see below).
package engine

import (
	"fmt"
	"strings"
)

func init() {
	RegisterVtabModule("fts3tokenize", fts3TokModule{})
}

// Column indices of FTS3_TOK_SCHEMA.
const (
	fts3TokColInput = 0
	fts3TokColToken = 1
	fts3TokColStart = 2
	fts3TokColEnd   = 3
	fts3TokColPos   = 4
)

// fts3TokIdxInput is fts3tokBestIndexMethod's idxNum 1: argv[0] carries the
// "input =" value. Any other plan (0) is an error at Filter time.
//
// fts3TokIdxInputOmitted is the same plan with the engine's confirmation that
// the conjunct really was removed from the residual WHERE (vtab_omit.go).
// C needs no such split -- omit=1 is simply honoured there -- and this engine
// splits the plan number rather than trusting the request, so a compile that
// never ran the rewrite is a decline instead of a dropped row.
const (
	fts3TokIdxInput        = 1
	fts3TokIdxInputOmitted = 2
)

// fts3TokCols is FTS3_TOK_SCHEMA's column list (fts3_tokenize_vtab.c:146) in
// the shape matchVtabConstraint reads. It is a constant -- the module's schema
// does not depend on its arguments -- so OmittedConjunct can answer without
// connecting the module.
var fts3TokCols = []columnInfo{
	{Name: "input"}, {Name: "token"}, {Name: "start"}, {Name: "end"}, {Name: "position"},
}

// OmittedConjunct implements omittingVtabModule (vtab.go): it names the WHERE
// conjunct BestIndex below will consume, so compileScanAttempt can drop it from the
// residual WHERE exactly as fts3tokBestIndexMethod's omit=1 does.
//
// The obligation the interface states is that this and BestIndex must agree.
// BestIndex takes the FIRST USABLE EQ constraint on column 0, and usability is
// decided at run time by whether the right-hand side folds to a value
// (buildVtabConstraints, vtab.go). So this answers for the first EQ conjunct on
// "input" and ONLY when its right-hand side is a literal, which always folds
// (vtabOmitLiteralRHS, vtab_omit.go): the first EQ is then always usable, so it
// is always the one BestIndex consumes. A non-literal right-hand side gets -1
// -- it MIGHT fold, and if it did not, BestIndex would move on to a later
// conjunct while this one had already been dropped.
//
// A non-EQ constraint on "input" is skipped rather than fatal, mirroring the
// loop below: "SELECT * FROM t1 WHERE input < 'b' AND input = 123" consumes the
// SECOND conjunct in C too (verified: it answers the one '123' row, the
// surviving "input < 'b'" being true of the text '123').
func (fts3TokModule) OmittedConjunct(conj []Expr, scopeName string) int {
	for i, e := range conj {
		ci, op, rhs, ok := matchVtabConstraint(e, fts3TokCols, scopeName)
		if !ok || ci != fts3TokColInput || op != VtabEQ {
			continue
		}
		if !vtabOmitLiteralRHS(rhs) {
			return -1
		}
		return i
	}
	return -1
}

type fts3TokModule struct{}

func (fts3TokModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	name := "simple"
	var targs []string
	if len(args) > 0 {
		// fts3tokDequoteArray dequotes EVERY argument, including the name.
		name = fts3DequoteToken(args[0])
		targs = make([]string, 0, len(args)-1)
		for _, a := range args[1:] {
			targs = append(targs, fts3DequoteToken(a))
		}
	}
	tok, err := fts3NewTokenizer("fts3tokenize", name, targs)
	if err != nil {
		return nil, nil, err
	}
	cols := []VtabColumn{
		{Name: "input"},
		{Name: "token"},
		{Name: "start"},
		{Name: "end"},
		{Name: "position"},
	}
	return cols, fts3TokTable{tok: tok}, nil
}

type fts3TokTable struct{ tok *fts3Tokenizer }

// BestIndex consumes the FIRST usable EQ constraint on "input", exactly as
// fts3tokBestIndexMethod does -- which is why "WHERE input = 'a' AND input =
// 'b c'" takes the first and lets the engine's WHERE reject every row against
// the second (verified: no rows).
func (fts3TokTable) BestIndex(info *VtabIndexInfo) error {
	for i, c := range info.Constraints {
		if c.Usable && c.Op == VtabEQ && c.Column == fts3TokColInput {
			info.IdxNum = fts3TokIdxInput
			if c.Omitted {
				// The engine really did drop this conjunct from the residual
				// WHERE (vtab_omit.go), so Filter may report an "input" that
				// no longer compares equal to the constrained value -- which is
				// every non-TEXT and every NUL-bearing one.
				info.IdxNum = fts3TokIdxInputOmitted
			}
			info.Usage[i].ArgvIndex = 1
			// C's own omit=1 (fts3_tokenize_vtab.c:248), stated where a reader
			// looks for it. It is an OUTPUT and this engine's omission is
			// decided on the INPUT side at compile time, so nothing reads it
			// back; keeping it makes the port complete rather than partial.
			info.Usage[i].Omit = true
			return nil
		}
	}
	info.IdxNum = 0
	return nil
}

func (t fts3TokTable) Open() (VtabCursor, error) { return &fts3TokCursor{tok: t.tok}, nil }

// fts3TokCursor materializes every token of the input in Filter, matching this
// engine's eager row model.
//
// input and reported are Fts3tokCursor.zInput read the two ways C reads it: the
// TOKENIZER is opened over the full byte count ("rc = pTab->pMod->xOpen(
// pTab->pTok, pCsr->zInput, nByte, &pCsr->pCsr);", fts3_tokenize_vtab.c:356)
// while xColumn hands column 0 back strlen-terminated
// ("sqlite3_result_text(pCtx, pCsr->zInput, -1, SQLITE_TRANSIENT);",
// fts3_tokenize_vtab.c:388). They differ only for an input carrying an embedded
// NUL, and that difference is observable: x'6120620063' reports 'a b' and still
// yields the token 'b<NUL>c' at 2..5.
type fts3TokCursor struct {
	tok      *fts3Tokenizer
	input    string
	reported string
	spans    []fts3TokenSpan
	pos      int
}

func (c *fts3TokCursor) Filter(idxNum int, _ string, argv []Value) error {
	if idxNum != fts3TokIdxInput && idxNum != fts3TokIdxInputOmitted {
		// SQLITE_ERROR there, which surfaces as "SQL logic error".
		return fmt.Errorf("engine: fts3tokenize: a query must constrain input with \"input = <text>\"")
	}
	v := argv[0]
	if v.Typ == Null {
		// sqlite3_value_text() is NULL and sqlite3_value_bytes() 0, so the
		// tokenizer sees the empty string and produces nothing.
		c.input, c.reported, c.spans, c.pos = "", "", nil, 0
		return nil
	}
	// The two shapes whose reported "input" is a TRANSFORMATION of the
	// constrained value rather than the value itself, and which therefore need
	// the consumed conjunct to be GONE from the residual WHERE -- see this
	// file's doc comment. idxNum fts3TokIdxInputOmitted is the engine saying it
	// dropped it (vtab_omit.go); without that the row would be re-tested
	// against a value it can no longer equal, so this declines instead.
	if idxNum != fts3TokIdxInputOmitted && v.Typ != Text {
		return fmt.Errorf("engine: fts3tokenize: an input= value of type %s is not supported by this engine here (C fts3tokenize omits the constraint from the outer WHERE and reports input as its TEXT rendering; this compile did not drop the conjunct -- see vtab_omit.go -- so the row would be re-tested against the original value)", typeName(v))
	}
	// An embedded NUL declines under EITHER plan, and for a reason the omit
	// does not touch: it is this engine's TOKENIZER that differs, not its
	// WHERE. simpleCreate's default delimiter table is built by
	// "for(i=1; i<0x80; i++){ t->delim[i] = !fts3_isalnum(i) ? -1 : 0; }"
	// (fts3_tokenizer1.c:89-92) -- the loop starts at ONE, so delim[0] is left
	// zero and NUL is a TOKEN character in C, which is why
	// "WHERE input = x'6120620063'" yields two tokens there ('a', then
	// 'b<NUL>c' spanning 2..5). fts3TokenizeSpans treats every non-alphanumeric
	// byte as a delimiter, NUL included, and answers three ('a','b','c'). That
	// difference is invisible everywhere else, because every INDEXING call site
	// truncates at the first NUL before tokenizing (fts3TokenizerInput,
	// fts3_tokenizer.go, with its own oracle evidence) and only this module
	// opens the tokenizer over the full byte count. porter and unicode61 have
	// not been measured on the same question. A row short is a wrong answer, so
	// this stays a decline until the tokenizer side is settled against the
	// oracle for all three.
	if strings.IndexByte(valueToText(v), 0) >= 0 {
		return fmt.Errorf("engine: fts3tokenize: an input= value carrying a NUL byte is not supported by this engine (C fts3tokenize reports input strlen-terminated while its tokenizer reads the full byte count AND treats NUL as a token character -- fts3_tokenizer1.c:89-92 leaves delim[0] zero -- where this engine's default delimiter table makes it a separator)")
	}
	text := valueToText(v)
	c.input = text
	c.reported = text
	if i := strings.IndexByte(text, 0); i >= 0 {
		c.reported = text[:i]
	}
	// spansOf over the FULL text, not over c.reported: fts3tokFilterMethod
	// opens the tokenizer with an explicit byte count rather than -1
	// (fts3_tokenize_vtab.c:356), so an embedded NUL does not end the input.
	c.spans = c.tok.spansOf(text)
	c.pos = 0
	return nil
}

func (c *fts3TokCursor) Next() error { c.pos++; return nil }
func (c *fts3TokCursor) Eof() bool   { return c.pos >= len(c.spans) }

func (c *fts3TokCursor) Column(i int) (Value, error) {
	sp := c.spans[c.pos]
	switch i {
	case fts3TokColInput:
		return Value{Typ: Text, S: []byte(c.reported)}, nil
	case fts3TokColToken:
		return Value{Typ: Text, S: []byte(sp.term)}, nil
	case fts3TokColStart:
		return Value{Typ: Int, I: int64(sp.start)}, nil
	case fts3TokColEnd:
		return Value{Typ: Int, I: int64(sp.end)}, nil
	case fts3TokColPos:
		return Value{Typ: Int, I: int64(c.pos)}, nil
	}
	return Value{}, fmt.Errorf("engine: fts3tokenize: no column %d", i)
}

// Rowid counts from 1: fts3tokFilterMethod ends by calling xNext, which
// increments iRowid before fetching the first token.
func (c *fts3TokCursor) Rowid() (int64, error) { return int64(c.pos + 1), nil }
func (c *fts3TokCursor) Close() error          { return nil }
