// This file implements the fts5vocab module: a read-only virtual table over an
// fts5 table's term index, shaped 'row', 'col' or 'instance' by its second
// argument. Like fts4aux it is served from the pager rather than a cursor.
//
// It tokenizes the documents as C fts5 built its index (the same tokenizer
// MATCH uses), so the counts agree. Where the documents live depends on the
// content mode (fts5VocabDocs): %_content, the external content table, or for
// a contentless table %_data itself.
//
//		CREATE VIRTUAL TABLE t USING fts5(a, b);
//		INSERT INTO t VALUES('one two two','two three');
//		INSERT INTO t VALUES('one','four');
//		INSERT INTO t VALUES('five six','six six');
//		CREATE VIRTUAL TABLE vrow  USING fts5vocab(t, 'row');
//		CREATE VIRTUAL TABLE vcol  USING fts5vocab(t, 'col');
//		CREATE VIRTUAL TABLE vinst USING fts5vocab(t, 'instance');
//		SELECT rowid, * FROM vrow  ORDER BY rowid;
//		  1|five|1|1  2|four|1|1  3|one|2|2  4|six|1|3  5|three|1|1  6|two|1|3
//		SELECT rowid, * FROM vcol  ORDER BY rowid;
//		  1|five|a|1|1  2|four|b|1|1  3|one|a|2|2  4|six|a|1|1  5|six|b|1|2
//		  6|three|b|1|1  7|two|a|1|2  8|two|b|1|1
//		SELECT rowid, * FROM vinst ORDER BY rowid;
//		  1|five|3|a|0  2|four|2|b|0  3|one|1|a|0  4|one|2|a|0  5|six|3|a|1
//		  6|six|3|b|0  7|six|3|b|1  8|three|1|b|1  9|two|1|a|1  10|two|1|a|2
//		  11|two|1|b|0
//
//	  - columns: (term, doc, cnt) for 'row', (term, col, doc, cnt) for 'col',
//	    (term, doc, col, offset) for 'instance'; no hidden columns, every type
//	    empty (BLOB affinity, so term/col are TEXT and the rest INTEGER);
//	  - unlike fts4aux, 'col' names the column by its text name and has no '*'
//	    aggregate row;
//	  - doc counts distinct rows; cnt and offset count occurrences. 'instance'
//	    has one row per occurrence, offset being the position within its own
//	    column's tokenization (offsets do not carry across columns);
//	  - rows are ordered by term in byte order; within a term 'col' orders by
//	    column declaration index (not name), 'instance' by doc then column;
//	  - a NULL column contributes no tokens;
//	  - everything is recomputed at query time, so writes show immediately;
//	  - rowid is 1-based in scan order;
//	  - the 'term' constraint compares raw bytes, as fts4aux's does
//	    (fts3AuxKeepTerm is reused; fts3aux2.test's probe splits the same way).
//
// # Arguments
//
// Normally two: the target table and the type ('row', 'col', 'instance',
// case-insensitive, bare or quoted). Zero or one is "wrong number of vtable
// arguments" at CREATE.
//
// A leading third argument, the target's database ("fts5vocab(db, table,
// type)"), is accepted only when fts5vocab itself is created in TEMP:
// fts5_vocab.c:190 sets bDb only for argc==6 with the creating database
// "temp", and line 192 rejects any other argc. Created in TEMP, the database
// and target resolve at SELECT time, as fts4aux's db-qualified form does (see
// CreateVirtualTable and fts5VocabResolvePager). An unknown type fails at
// CREATE ("fts5vocab: unknown table type: '...'"); a missing or non-fts5
// target creates and fails at SELECT ("no such fts5 table: main.X"). A
// "main.t" target is taken literally, as in C ("no such fts5 table:
// main.main.t").
//
// fts5vocab is read-only: it implements no writableVtabModule, so writes fail
// with "table ... may not be modified", as in C.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// fts5VocabModule is registered by RegisterFTS5 (vtab_fts5.go), never from an
// init() here: it is meaningless without fts5 itself, and registering it
// unconditionally would make this engine ACCEPT a CREATE VIRTUAL TABLE the
// default (non -tags sqlite_fts5) oracle build rejects outright with
// "no such module: fts5vocab" -- the exact divergence RegisterFTS5's own doc
// comment explains fts5's opt-in registration exists to avoid.
type fts5VocabModule struct{}

// fts5VocabKind is the table shape selected by fts5vocab's second argument.
type fts5VocabKind int

const (
	fts5VocabRow fts5VocabKind = iota
	fts5VocabCol
	fts5VocabInstance
)

func (fts5VocabModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	var real []string
	for _, a := range args {
		if a = strings.TrimSpace(a); a != "" {
			real = append(real, a)
		}
	}
	// Both the ordinary (table, type) and the db-qualified (db, table, type)
	// forms are accepted here UNIFORMLY, exactly like fts4aux's own Connect
	// (vtab_fts3aux.go): the arity/catalog rule that makes a 3-argument call
	// legal only in TEMP (see this file's package comment) is checked one
	// layer up, in CreateVirtualTable (vtab.go), which alone knows whether
	// fts5vocab itself is being created in TEMP -- this Connect reruns on
	// EVERY later read/reconnect of an already-created table, by which point
	// that check has already passed once and must not run again.
	var targetDB, targetArg, typeArg string
	switch len(real) {
	case 2:
		targetArg, typeArg = real[0], real[1]
	case 3:
		targetDB, targetArg, typeArg = real[0], real[1], real[2]
	default:
		return nil, nil, fmt.Errorf("engine: fts5vocab: wrong number of vtable arguments")
	}
	target, ok := fts3DequoteArg(targetArg)
	if !ok {
		return nil, nil, fmt.Errorf("engine: fts5vocab: %q is not a plain table name", targetArg)
	}
	if targetDB != "" {
		if db, ok := fts3DequoteArg(targetDB); ok {
			targetDB = db
		}
	}
	typeText, ok := fts3DequoteArg(typeArg)
	if !ok {
		return nil, nil, fmt.Errorf("engine: fts5vocab: %q is not a plain type argument", typeArg)
	}
	var kind fts5VocabKind
	var cols []VtabColumn
	switch strings.ToLower(typeText) {
	case "row":
		kind = fts5VocabRow
		cols = []VtabColumn{{Name: "term"}, {Name: "doc"}, {Name: "cnt"}}
	case "col":
		kind = fts5VocabCol
		cols = []VtabColumn{{Name: "term"}, {Name: "col"}, {Name: "doc"}, {Name: "cnt"}}
	case "instance":
		kind = fts5VocabInstance
		cols = []VtabColumn{{Name: "term"}, {Name: "doc"}, {Name: "col"}, {Name: "offset"}}
	default:
		return nil, nil, fmt.Errorf("engine: fts5vocab: unknown table type: %q (want 'row', 'col' or 'instance')", typeText)
	}
	return cols, fts5VocabTable{targetDB: targetDB, target: target, kind: kind}, nil
}

// fts5VocabTable is a connected fts5vocab table. Its rows come from the
// TARGET fts5 table's %_content, so it is served through fts5VocabRows
// against the pager (vtab.go's dispatch) and never opens a cursor.
//
// targetDB is the three-argument db-qualified form's own database argument
// ("main", "temp", or an ATTACHed schema name), or "" for the ordinary
// two-argument form, whose target is read from fts5vocab's OWN database
// (fts5VocabResolvePager) -- the exact counterpart of fts3AuxTable.targetDB
// (vtab_fts3aux.go).
type fts5VocabTable struct {
	targetDB string
	target   string
	kind     fts5VocabKind
}

// BestIndex consumes a constraint on TERM (always column 0, in every one of
// the three shapes). Reuses fts3aux's term-range bit flags and byte-compare
// helpers verbatim -- see the package comment for why that trick applies
// unchanged here.
func (fts5VocabTable) BestIndex(info *VtabIndexInfo) error {
	for i, c := range info.Constraints {
		if c.Column != 0 || !c.Usable {
			continue
		}
		switch c.Op {
		case VtabEQ:
			if info.IdxNum&fts3AuxEQ == 0 {
				info.IdxNum |= fts3AuxEQ
				info.Usage[i].ArgvIndex = 1
			}
		case VtabGE, VtabGT:
			if info.IdxNum&fts3AuxLower == 0 {
				info.IdxNum |= fts3AuxLower
				if c.Op == VtabGT {
					info.IdxNum |= fts3AuxLowerStrict
				}
				info.Usage[i].ArgvIndex = 2
			}
		case VtabLE, VtabLT:
			if info.IdxNum&fts3AuxUpper == 0 {
				info.IdxNum |= fts3AuxUpper
				if c.Op == VtabLT {
					info.IdxNum |= fts3AuxUpperStrict
				}
				info.Usage[i].ArgvIndex = 3
			}
		}
	}
	return nil
}

func (fts5VocabTable) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("engine: internal error: an fts5vocab table is read from the pager, not through a cursor")
}

// fts5VocabResolvePager is the three-argument db-qualified form's database
// argument, resolved to the pager fts5VocabRows should read the target's term
// index from. Mirrors fts3AuxResolvePager (vtab_fts3aux.go) verbatim -- see
// that function's doc comment for the full reasoning, which applies here
// unchanged: "main" and "temp" are two databases (temp_store.go), so the
// qualifier picks the pager the target's own shadow tables live in. An argument
// naming neither is looked up among the primary's ATTACHed readers
// (cross_db.go).
func (t fts5VocabTable) fts5VocabResolvePager(p *ReadOnlyPager) (*ReadOnlyPager, error) {
	if t.targetDB == "" {
		return p, nil
	}
	scope, ok := scopeOfQualifier(t.targetDB)
	if !ok {
		if ap, found := p.attachedReaderNamed(t.targetDB); found {
			return ap, nil
		}
		return nil, fmt.Errorf("engine: fts5vocab: unknown database %s", t.targetDB)
	}
	owner, found := p.pagerForScope(scope)
	if !found {
		return nil, fmt.Errorf("engine: fts5vocab: no such fts5 table: %s.%s", t.targetDB, t.target)
	}
	rows, err := owner.Schema()
	if err != nil {
		return nil, err
	}
	if findSchemaTableRow(rows, scopeAny, t.target) == nil {
		return nil, fmt.Errorf("engine: fts5vocab: no such fts5 table: %s.%s", t.targetDB, t.target)
	}
	return owner, nil
}

// fts5VocabTargetColNames resolves target to its fts5 text-column names (the
// %_content column order), which of those columns are UNINDEXED, and the
// target's TOKENIZER -- or the same "no such fts5 table" error the oracle gives
// for both a missing target and one that is not an fts5 table.
func (p *ReadOnlyPager) fts5VocabTargetColNames(target string) ([]string, []bool, *fts5Tokenizer, error) {
	mod, args, ok, err := p.createdVtabDef(target)
	if err != nil {
		return nil, nil, nil, err
	}
	if !ok || !strings.EqualFold(mod, "fts5") {
		return nil, nil, nil, fmt.Errorf("engine: fts5vocab: no such fts5 table: %s", target)
	}
	// buildStore is pure (no DB access): for a table CREATEd here its arguments
	// were validated once already, so re-parsing them only recovers the column
	// names and tokenizer. It CAN still fail for a table REAL SQLITE wrote --
	// one whose tokenize= this engine declines -- and that failure has to
	// surface rather than fall back to the default tokenizer, which would list
	// terms that are not in the index.
	st, berr := fts5Module{}.buildStore("", args)
	if berr != nil {
		return nil, nil, nil, fmt.Errorf("engine: fts5vocab: %s: %w", target, berr)
	}
	return st.colNames, st.unindexed, st.tok, nil
}

// fts5VocabDoc is one document of the target table, tokenized column by
// column -- the shape fts5VocabRows folds into term occurrences, whichever of
// fts5's three content modes the target is in.
type fts5VocabDoc struct {
	rowid int64
	cols  [][]string
}

// fts5VocabDocs resolves the target's documents in ascending rowid order. C's
// fts5vocab reads the index (sqlite3Fts5IterNew); this engine reconstructs the
// same postings from the documents, which each content mode keeps elsewhere:
//
//	NORMAL     the %_content shadow table, tokenized here.
//	EXTERNAL   the table content= names, reconstructed and CHECKED against the
//	           index before it can be trusted (fts5_extcontent.go).
//	NONE       nothing at all: the documents come back out of %_data itself
//	           (fts5_contentless.go).
func (p *ReadOnlyPager) fts5VocabDocs(target string, colNames []string, unindexed []bool, tokenizer *fts5Tokenizer) ([]fts5VocabDoc, error) {
	tokenizeRow := func(rowid int64, vals []Value) fts5VocabDoc {
		cols := make([][]string, len(colNames))
		for ci := range colNames {
			if unindexed[ci] || ci >= len(vals) {
				continue
			}
			cols[ci] = tokenizer.tokenize(valueToText(vals[ci]))
		}
		return fts5VocabDoc{rowid: rowid, cols: cols}
	}
	sch, isFts5 := p.fts5SchemaTok(target)
	switch {
	case isFts5 && sch.cl != nil:
		docs, err := p.fts5ContentlessDocsOf(target)
		if err != nil {
			return nil, err
		}
		out := make([]fts5VocabDoc, 0, len(docs.rowids))
		for _, rid := range docs.rowids {
			out = append(out, fts5VocabDoc{rowid: rid, cols: docs.tokens[rid].cols})
		}
		return out, nil
	case isFts5 && sch.extContent != "":
		st, err := p.fts5ExtStoreOf(target)
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, fmt.Errorf("engine: fts5vocab: no such fts5 table: %s", target)
		}
		if verr := st.fts5ExtEnsureVerified(); verr != nil {
			return nil, verr
		}
		out := make([]fts5VocabDoc, 0, len(st.rows))
		for _, rid := range st.sortedRowids() {
			out = append(out, tokenizeRow(rid, st.rows[rid]))
		}
		return out, nil
	}
	rowids, records, err := p.Rows(target + "_content")
	if err != nil {
		return nil, err
	}
	out := make([]fts5VocabDoc, 0, len(rowids))
	for i, rid := range rowids {
		rec := records[i]
		vals := rec
		if len(rec) > 0 {
			vals = rec[1:] // %_content's slot 0 is the id, carried by the b-tree key
		}
		out = append(out, tokenizeRow(int64(rid), vals))
	}
	return out, nil
}

// fts5VocabOcc is one occurrence of a term: the %_content row it was found
// in, which declared column, and its 0-based position within that column's
// own tokenization, not a table-wide offset.
type fts5VocabOcc struct {
	doc    int64
	col    int
	offset int64
}

// fts5VocabRows re-tokenizes the target's %_content exactly as MATCH does
// (fts5_match.go's buildFts5Doc) and folds the result into the (term, ...)
// rows fts5vocab's kind describes, in the order C fts5vocab returns them.
func (t fts5VocabTable) fts5VocabRows(p *ReadOnlyPager, idxNum int, argv []Value) ([][]Value, []int64, error) {
	// The db-qualified form's database argument, resolved once here -- every
	// lookup below reads the target out of THIS pager rather than p, so a
	// "fts5vocab(temp, t1, row)" reads temp's t1 even while p is (say) the
	// primary's ambient scan and an unrelated main.t1 also exists. See
	// fts5VocabResolvePager's doc comment.
	tp, err := t.fts5VocabResolvePager(p)
	if err != nil {
		return nil, nil, err
	}
	colNames, unindexed, tokenizer, err := tp.fts5VocabTargetColNames(t.target)
	if err != nil {
		return nil, nil, err
	}
	// fts5vocab is a view of the INDEX, so unlike MATCH it really does report
	// differently per detail= mode (fts5_detail.go's fts5VocabTermRows, which
	// owns every rule): a column or an offset the mode never recorded is NULL,
	// and detail=none has one 'col' row per term rather than one per column.
	detail, err := tp.fts5VocabDetailOf(t.target)
	if err != nil {
		return nil, nil, err
	}
	docs, err := tp.fts5VocabDocs(t.target, colNames, unindexed, tokenizer)
	if err != nil {
		return nil, nil, err
	}

	// terms[term] accumulates every occurrence of that term, appended in
	// %_content's ascending-rowid scan order and, within one row, ascending
	// column-declaration order and ascending tokenize position -- which is
	// already the exact (doc, col, offset) order 'instance' must return, so
	// no re-sort is needed for it (see the package comment's doc-major
	// ordering evidence).
	terms := map[string][]fts5VocabOcc{}
	for _, d := range docs {
		for ci := range colNames {
			// An UNINDEXED column is in the index nowhere, so it is in this
			// view of the index nowhere either. Verified against the oracle:
			// over fts5(a, b UNINDEXED) holding ('hello world','foo bar'),
			// every fts5vocab kind reports only 'hello' and 'world', each in
			// column a.
			if unindexed[ci] || ci >= len(d.cols) {
				continue
			}
			for pos, tok := range d.cols[ci] {
				terms[tok] = append(terms[tok], fts5VocabOcc{doc: d.rowid, col: ci, offset: int64(pos)})
			}
		}
	}

	sorted := make([]string, 0, len(terms))
	for term := range terms {
		sorted = append(sorted, term)
	}
	sort.Strings(sorted)

	var rows [][]Value
	for _, term := range sorted {
		if !fts3AuxKeepTerm(term, idxNum, argv) {
			continue
		}
		rows = append(rows, fts5VocabTermRows(t.kind, detail, term, terms[term], colNames)...)
	}
	rowidsOut := make([]int64, len(rows))
	for i := range rowidsOut {
		rowidsOut[i] = int64(i) + 1
	}
	return rows, rowidsOut, nil
}
