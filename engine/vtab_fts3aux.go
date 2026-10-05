// Package engine implements the fts4aux module: a read-only virtual table
// over an fts3/fts4 table's term index, one row per (term, column) pair plus
// an aggregate row per term.
//
// Example output:
//
//	CREATE VIRTUAL TABLE x USING fts4aux(t);
//	SELECT rowid, * FROM x;
//	  1|one|*|2|2      -- aggregate row: '*' column
//	  2|one|0|2|2      -- per-column rows with column index
//
// Columns are (term, col, documents, occurrences) plus a hidden languageid.
// The aggregate row ('*') comes first for each term, followed by numbered
// per-column rows. Documents counts document count, occurrences counts
// position count. Only terms with at least one live document appear.
//
// The two-argument form "fts4aux(db, table)" qualifies the target database
// (main, temp, or attached). One-argument form uses fts4aux's own database.
// Missing targets or non-fts3/fts4 tables fail at SELECT time, not CREATE.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

func init() {
	RegisterVtabModule("fts4aux", fts3AuxModule{})
}

type fts3AuxModule struct{}

func (fts3AuxModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	var real []string
	for _, a := range args {
		if a = strings.TrimSpace(a); a != "" {
			real = append(real, a)
		}
	}
	// Same-database-in-main rejection is handled by CreateVirtualTable.
	// Connect accepts 1 or 2 arguments uniformly and may re-run on later
	// reads of an already-created table.
	var targetDB, targetArg string
	switch len(real) {
	case 1:
		targetArg = real[0]
	case 2:
		targetDB, targetArg = real[0], real[1]
	default:
		return nil, nil, fmt.Errorf("engine: fts4aux: wrong number of arguments (want the name of one fts3/fts4 table, optionally prefixed with its database: fts4aux(table) or fts4aux(db, table))")
	}
	target, ok := fts3DequoteArg(targetArg)
	if !ok {
		return nil, nil, fmt.Errorf("engine: fts4aux: %q is not a plain table name", targetArg)
	}
	if targetDB != "" {
		if db, ok := fts3DequoteArg(targetDB); ok {
			targetDB = db
		}
	}
	cols := []VtabColumn{
		{Name: "term"},
		{Name: "col"},
		{Name: "documents"},
		{Name: "occurrences"},
		// NumericAffinity: text literals like '1' match as numeric language IDs,
		// requiring numeric-affinity comparison coercion.
		{Name: "languageid", Hidden: true, NumericAffinity: true},
	}
	return cols, fts3AuxTable{targetDB: targetDB, target: target}, nil
}

// fts3AuxTable is a connected fts4aux table. Its rows come from the DATABASE
// rather than from the module, so it is served through fts3AuxRows against the
// pager (vtab.go's dispatch) and never opens a cursor.
//
// targetDB is the two-argument form's own database argument ("main", "temp",
// or an ATTACHed schema name), or "" for the ordinary one-argument form, whose
// target is read from fts4aux's OWN database (fts3AuxResolvePager).
type fts3AuxTable struct {
	targetDB string
	target   string
}

// fts3AuxResolvePager resolves the two-argument form's database argument
// to the pager for reading the target's term index. Main and temp each have
// their own file and shadow tables. An unknown database is looked up among
// attached readers.
func (t fts3AuxTable) fts3AuxResolvePager(p *ReadOnlyPager) (*ReadOnlyPager, error) {
	if t.targetDB == "" {
		return p, nil
	}
	scope, ok := scopeOfQualifier(t.targetDB)
	if !ok {
		if ap, found := p.attachedReaderNamed(t.targetDB); found {
			return ap, nil
		}
		return nil, fmt.Errorf("engine: fts4aux: unknown database %s", t.targetDB)
	}
	// Main and temp each have their own pager and shadow tables.
	owner, found := p.pagerForScope(scope)
	if !found {
		return nil, fmt.Errorf("engine: fts4aux: %s.%s: no such table", t.targetDB, t.target)
	}
	rows, err := owner.Schema()
	if err != nil {
		return nil, err
	}
	if findSchemaTableRow(rows, scopeAny, t.target) == nil {
		return nil, fmt.Errorf("engine: fts4aux: %s.%s: no such table", t.targetDB, t.target)
	}
	return owner, nil
}

// The term-constraint plan bits carried in IdxNum. argv slot 1 holds the
// equality value, 2 the lower bound, 3 the upper bound, 4 an equality
// constraint on the hidden languageid column (fts3AuxLangid).
const (
	fts3AuxEQ          = 0x01
	fts3AuxLower       = 0x02
	fts3AuxUpper       = 0x04
	fts3AuxLowerStrict = 0x08
	fts3AuxUpperStrict = 0x10
	fts3AuxLangid      = 0x20
)

// fts3AuxLangidCol is languageid's position in the Connect column list above.
const fts3AuxLangidCol = 4

// BestIndex consumes a constraint on TERM as a raw byte comparison (not SQL
// comparison), and an equality constraint on the hidden languageid column.
// This pushdown is safe: the engine re-applies WHERE to all returned rows, so
// byte filters only remove rows that real fts4aux never produced. Neither
// constraint uses Omit, so the engine always enforces them.
func (fts3AuxTable) BestIndex(info *VtabIndexInfo) error {
	for i, c := range info.Constraints {
		if !c.Usable {
			continue
		}
		if c.Column == fts3AuxLangidCol {
			if c.Op == VtabEQ && info.IdxNum&fts3AuxLangid == 0 {
				info.IdxNum |= fts3AuxLangid
				info.Usage[i].ArgvIndex = 4
			}
			continue
		}
		if c.Column != 0 {
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

// fts3AuxLangidValue truncates to int32 and clamps negative results to 0
// (the VDBE layer re-checks languageid constraints, so zero is safe).
func fts3AuxLangidValue(v Value) int64 {
	n := int64(int32(valueToInt64Trunc(v)))
	if n < 0 {
		return 0
	}
	return n
}

// fts3AuxTermBytes returns the constraint value as TEXT, where BLOB contributes
// raw bytes and numbers contribute decimal text. NULL bounds are not applied.
func fts3AuxTermBytes(argv []Value, slot int) (string, bool) {
	if slot >= len(argv) || argv[slot].Typ == Null {
		return "", false
	}
	return valueToText(argv[slot]), true
}

// fts3AuxKeepTerm applies the pushed-down byte comparison.
func fts3AuxKeepTerm(term string, idxNum int, argv []Value) bool {
	if idxNum&fts3AuxEQ != 0 {
		if want, ok := fts3AuxTermBytes(argv, 0); ok && term != want {
			return false
		}
	}
	if idxNum&fts3AuxLower != 0 {
		if lo, ok := fts3AuxTermBytes(argv, 1); ok {
			if c := strings.Compare(term, lo); c < 0 || (c == 0 && idxNum&fts3AuxLowerStrict != 0) {
				return false
			}
		}
	}
	if idxNum&fts3AuxUpper != 0 {
		if hi, ok := fts3AuxTermBytes(argv, 2); ok {
			if c := strings.Compare(term, hi); c > 0 || (c == 0 && idxNum&fts3AuxUpperStrict != 0) {
				return false
			}
		}
	}
	return true
}

func (fts3AuxTable) Open() (VtabCursor, error) {
	return nil, fmt.Errorf("engine: internal error: an fts4aux table is read from the pager, not through a cursor")
}

// fts3AuxRows decodes the target's whole term index and folds it into the
// (term, col, documents, occurrences) rows above, in the order real fts4aux
// returns them: term ascending by raw byte order (fts3Index.terms already is),
// the '*' row before that term's numbered rows, and those ascending.
func (t fts3AuxTable) fts3AuxRows(p *ReadOnlyPager, idxNum int, argv []Value) ([][]Value, []int64, error) {
	tp, err := t.fts3AuxResolvePager(p)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := tp.fts3TableInfo(t.target); !ok {
		return nil, nil, fmt.Errorf("engine: fts4aux: %s is not an fts3/fts4 table", t.target)
	}
	// Language 0 by default, or selected by the hidden languageid constraint
	// (BestIndex above). Always pass nIndex=1 to fts3LevelBase, regardless of
	// the target's actual prefix indexes.
	langid := int64(0)
	if idxNum&fts3AuxLangid != 0 {
		langid = fts3AuxLangidValue(argv[3])
	}
	ix, err := tp.fts3LoadIndex(t.target, fts3LevelBase(langid, 1, 0), nil)
	if err != nil {
		return nil, nil, err
	}
	var rows [][]Value
	for i, term := range ix.terms {
		if !fts3AuxKeepTerm(term, idxNum, argv) {
			continue
		}
		var totalDocs, totalOcc int64
		perColDocs := map[int]int64{}
		perColOcc := map[int]int64{}
		for _, cols := range ix.post[i] {
			if cols == nil {
				// A delete marker: the newest entry for this (term, docid) says
				// the pair is gone, so it counts for nothing.
				continue
			}
			totalDocs++
			for c, ps := range cols {
				perColDocs[c]++
				perColOcc[c] += int64(len(ps))
				totalOcc += int64(len(ps))
			}
		}
		if totalDocs == 0 {
			continue
		}
		rows = append(rows, []Value{
			{Typ: Text, S: []byte(term)},
			{Typ: Text, S: []byte("*")},
			{Typ: Int, I: totalDocs},
			{Typ: Int, I: totalOcc},
			{Typ: Int, I: langid},
		})
		colIdx := make([]int, 0, len(perColDocs))
		for c := range perColDocs {
			colIdx = append(colIdx, c)
		}
		sort.Ints(colIdx)
		for _, c := range colIdx {
			rows = append(rows, []Value{
				{Typ: Text, S: []byte(term)},
				{Typ: Int, I: int64(c)},
				{Typ: Int, I: perColDocs[c]},
				{Typ: Int, I: perColOcc[c]},
				{Typ: Int, I: langid},
			})
		}
	}
	rowids := make([]int64, len(rows))
	for i := range rowids {
		rowids[i] = int64(i) + 1
	}
	return rows, rowids, nil
}
