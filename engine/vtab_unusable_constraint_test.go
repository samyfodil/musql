// Test virtual table constraints marked unusable and MATCH pattern reading.
// Verify unsatisfiable constraints cause query errors rather than wrong answers.
package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// i1PagerWith creates a database, runs statements, and returns a snapshot pager.
func i1PagerWith(t *testing.T, stmts ...string) *ReadOnlyPager {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "i1.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	return p
}

// TestVtabUnusableInputDeclinesNotEmpty is the wrong-answer gate. Each query
// names a hidden INPUT column of an eponymous module against another FROM
// item's column that this engine's own loop order binds LATER, so the
// constraint really is unusable where the table is materialized. Every one of
// them must ERROR. Returning zero rows -- what dropping the constraint
// produced -- is the failure this test exists to catch, so the assertion
// deliberately rejects "no error, no rows" rather than accepting it.
//
// The equivalent shapes whose correlated table this engine's loop order binds
// FIRST are no longer here: they are answered now, per outer row, and pinned
// against the oracle in TestVtabCorrelatedInputAnswers below. What separates
// the two lists is purely WHICH ORDER computeExecOrder (join.go) chose, which
// is why each query here has a partner there that differs only in FROM order
// or in carrying one extra single-table filter -- the "min remaining need"
// placement puts an own-filtered table first, and a virtual table placed first
// has nothing to correlate to. See vtab_correlated.go's doc comment.
func TestVtabUnusableInputDeclinesNotEmpty(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, j TEXT, n TEXT)`,
		`INSERT INTO t VALUES(1,'[1,2]','t'),(2,'[3]','t')`,
	)
	for _, q := range []string{
		// json_each / json_tree: json.c:5493 rejects an unusable JSON or ROOT.
		`SELECT t.id, e.value FROM json_each e, t WHERE e.json = t.j`,
		`SELECT t.id, e.value FROM json_tree e, t WHERE e.json = t.j`,
		// "e.json = '[9]'" is a single-table filter, so this engine's own
		// placement binds json_each FIRST and the ROOT constraint is left
		// unusable -- json.c:5493 rejects an unusable ROOT exactly as it does
		// an unusable JSON.
		`SELECT t.id, e.value FROM t, json_each e WHERE e.json = '[9]' AND e.root = t.j`,
		// ...and the same thing one conjunct at a time: with no filter of its
		// own json_each is bound second and this is answered (see the partner
		// test); with one, it is bound first and declines.
		`SELECT t.id, e.value FROM t, json_each e WHERE e.json = t.j AND e.value > 1`,
		// generate_series: series.c:867 rejects an unusable start/stop/step.
		`SELECT s.value FROM generate_series s, t WHERE s.start = t.id AND s.stop = 3`,
		// step, not start/stop: with a bounded start and stop supplied, this is
		// the case that ONLY series.c:867's rejection catches. ("... AND s.stop
		// = t.id" is not in this list on purpose -- musql's seriesCursor
		// declines an unbounded series independently, so it errors either way
		// and would be a blind case.)
		`SELECT s.value FROM t, generate_series s WHERE s.start = 1 AND s.stop = 5 AND s.step = t.id`,
		// pragma_*: pragma.c:2904 rejects an unusable equality on a hidden
		// input, and does so without first asking whether a usable one already
		// supplied that input.
		`SELECT t.id, pt.name FROM pragma_table_info pt, t WHERE pt.arg = t.n`,
	} {
		_, rows, err := p.Query(q)
		if err == nil {
			t.Errorf("%s: no error, %d rows -- an unusable input constraint must DECLINE, "+
				"never scan without it (json.c:5493 / series.c:867 / pragma.c:2904)", q, len(rows))
		}
	}
}

// TestVtabCorrelatedInputAnswers is the other half: the same shapes with the
// correlated table bound FIRST, which vtab_correlated.go now answers by driving
// the module once per outer row -- C's own model, where the constraint value is
// coded into a register inside the loop (wherecode.c:1584) and OP_VFilter runs
// there (wherecode.c:1610).
//
// The expected rows are the 3.53.3 oracle's, taken with the harness's own cgo
// worker (SQLITE_SOURCE_ID d4c0e51e...82c62) over exactly this schema; the
// same statements run through differ() in compat-harness/
// vtab_correlated_source_test.go, which is what keeps them honest as the
// oracle moves. They are pinned HERE as well because the engine package has no
// oracle of its own and this is where the mechanism lives.
func TestVtabCorrelatedInputAnswers(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, j TEXT, n TEXT)`,
		`INSERT INTO t VALUES(1,'[1,2]','t'),(2,'[3]','t')`,
	)
	for _, c := range []struct {
		q    string
		want []string
	}{
		{`SELECT t.id, e.value FROM t, json_each e WHERE e.json = t.j`,
			[]string{"1|1", "1|2", "2|3"}},
		{`SELECT t.id, e.value FROM t, json_tree e WHERE e.json = t.j`,
			[]string{"1|[1,2]", "1|1", "1|2", "2|[3]", "2|3"}},
		{`SELECT t.id, pt.name FROM t, pragma_table_info pt WHERE pt.arg = t.n`,
			[]string{"1|id", "1|j", "1|n", "2|id", "2|j", "2|n"}},
		// The module is re-driven per outer row, so its ROWID restarts at 1
		// each time -- the property fts3tok1.test 1.13.2 turns on.
		{`SELECT t.id, e.rowid, e.value FROM t, json_each e WHERE e.json = t.j`,
			[]string{"1|0|1", "1|1|2", "2|0|3"}},
	} {
		_, rows, err := p.Query(c.q)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.q, err)
			continue
		}
		got := make([]string, 0, len(rows))
		for _, r := range rows {
			parts := make([]string, len(r))
			for i, v := range r {
				parts[i] = valueToText(v)
			}
			got = append(got, strings.Join(parts, "|"))
		}
		if !eqStrings(got, c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.q, got, c.want)
		}
	}
}

// eqStrings is []string equality, order included.
func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestVtabUnusableNonInputStillScans is the other half of json.c's rule, and
// the reason the fix could not simply be "error when a right-hand side does
// not resolve": jsonEachBestIndex skips a constraint on any column before the
// hidden inputs outright --
//
//	json.c:5474  if( pConstraint->iColumn < JEACH_JSON ) continue;
//
// -- so a term on an ORDINARY column of the module's output is left to the
// engine's own WHERE and the query is still answered. These shapes were
// answered correctly before the change and must stay that way.
func TestVtabUnusableNonInputStillScans(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE TABLE t(k INTEGER)`,
		`INSERT INTO t VALUES(1),(2)`,
	)
	for _, c := range []struct {
		q    string
		want []int64
	}{
		{`SELECT e.value FROM json_each('[7,8,9]') e, t WHERE e.key = t.k ORDER BY e.value`, []int64{8, 9}},
		{`SELECT s.value FROM generate_series(5,9) s, t WHERE s.value = t.k + 4 ORDER BY s.value`, []int64{5, 6}},
	} {
		_, rows, err := p.Query(c.q)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.q, err)
			continue
		}
		got := vtabRowsInt(t, rows, 0)
		if !eqInts(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.q, got, c.want)
		}
	}
}

// TestVtabInputValuesStillFold pins the expression forms buildVtabConstraints
// must keep producing a value for now that it COMPILES the right-hand side
// (foldVtabInputValue, vtab.go) instead of walking it: a literal, an operator
// expression, a function call, a CAST and a scalar subquery. A regression here
// would show up as a decline, not a wrong answer -- but it would be a real
// capability loss, and the compiled fold is the only thing standing behind
// every one of these.
func TestVtabInputValuesStillFold(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE TABLE t(j TEXT)`,
		`INSERT INTO t VALUES('[7,8]')`,
	)
	for _, c := range []struct {
		q    string
		want []int64
	}{
		{`SELECT value FROM json_each('[7,8]')`, []int64{7, 8}},
		{`SELECT value FROM json_each('[7,'||'8]')`, []int64{7, 8}},
		{`SELECT value FROM json_each(json_array(7,8))`, []int64{7, 8}},
		{`SELECT value FROM json_each(CAST('[7,8]' AS TEXT))`, []int64{7, 8}},
		{`SELECT value FROM json_each((SELECT j FROM t))`, []int64{7, 8}},
		{`SELECT value FROM json_each WHERE json = (SELECT j FROM t)`, []int64{7, 8}},
		{`SELECT value FROM json_each('{"a":[7,8]}','$.'||'a')`, []int64{7, 8}},
		{`SELECT value FROM generate_series(1+6, 8)`, []int64{7, 8}},
	} {
		_, rows, err := p.Query(c.q)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.q, err)
			continue
		}
		got := vtabRowsInt(t, rows, 0)
		if !eqInts(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.q, got, c.want)
		}
	}
}

// TestFts3MatchPatternIsCompiledToARegister asserts the SEAM, not just the
// answer: the OpMatch instruction compiled for an fts3/fts4 MATCH carries a
// pattern REGISTER, and the instructions that compute it sit BEFORE it. A
// count-of-opcodes check alone could not tell that apart from the pattern
// still being walked at run time, which is exactly what this file's C
// citations say SQLite never does (wherecode.c:1584 codes the value; fts3.c:3365
// reads it back).
func TestFts3MatchPatternIsCompiledToARegister(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts3(c)`,
		`INSERT INTO ft(docid,c) VALUES(1,'alpha beta')`,
		`CREATE TABLE q(id INTEGER PRIMARY KEY, y)`,
		`INSERT INTO q VALUES(1,'alpha')`,
	)
	for _, sqlText := range []string{
		`SELECT docid FROM ft WHERE ft MATCH 'al'||'pha'`,
		`SELECT docid FROM ft WHERE ft MATCH 'alpha'`,
		`SELECT ft.docid FROM ft, q WHERE ft MATCH q.y`,
	} {
		stmt, perr := ParseSelect(sqlText)
		if perr != nil {
			t.Fatalf("%s: %v", sqlText, perr)
		}
		prog, cerr := compileSelectScan(p, stmt, nil)
		if cerr != nil {
			t.Fatalf("%s: %v", sqlText, cerr)
		}
		found := false
		for i, in := range prog.Insns {
			if in.Op != OpMatch {
				continue
			}
			found = true
			info, ok := in.P4.(*matchCompileInfo)
			if !ok {
				t.Fatalf("%s: OpMatch P4 is %T, want *matchCompileInfo", sqlText, in.P4)
			}
			if !info.patCompiled {
				t.Errorf("%s: OpMatch does not carry a compiled pattern register -- "+
					"the query is still being walked at run time", sqlText)
			}
			// The register must be a real one, and an instruction ahead of
			// this OpMatch in the same stream must name it -- otherwise the
			// value read would be whatever a previous row (or nothing at all)
			// left there. Which operand field carries the DESTINATION varies
			// by opcode (OpInteger/OpString use P2, OpConcat P3), so this
			// checks all three rather than encoding a per-opcode table that
			// would rot; the behavioural per-row gate below is what proves
			// the register is actually being re-read.
			if info.patReg < 0 || info.patReg >= prog.NReg {
				t.Fatalf("%s: pattern register %d is outside the program's %d registers",
					sqlText, info.patReg, prog.NReg)
			}
			written := false
			for j := 0; j < i; j++ {
				in := prog.Insns[j]
				if in.P1 == info.patReg || in.P2 == info.patReg || in.P3 == info.patReg {
					written = true
					break
				}
			}
			if !written {
				t.Errorf("%s: nothing before OpMatch writes pattern register %d", sqlText, info.patReg)
			}
		}
		if !found {
			t.Fatalf("%s: compiled to no OpMatch at all", sqlText)
		}
	}
}

// TestFts3MatchPatternIsPerRow is the mutation-visible half: the pattern
// register is recomputed on every OpMatch execution, so a MATCH whose query
// comes from ANOTHER table's column answers each outer row with ITS OWN query.
// Evaluating the pattern once -- for the first row, or at compile time --
// gives the same answer for every row and this test fails. The shape is
// fts3join.test's own 1.1 (verified against the 3.53.3 oracle in
// compat-harness/fts3_join_match_test.go).
func TestFts3MatchPatternIsPerRow(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts3(c)`,
		`INSERT INTO ft(docid,c) VALUES(1,'alpha')`,
		`INSERT INTO ft(docid,c) VALUES(2,'beta')`,
		`INSERT INTO ft(docid,c) VALUES(3,'gamma')`,
		`CREATE TABLE q(id INTEGER PRIMARY KEY, y)`,
		`INSERT INTO q VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
	)
	// Each q row selects exactly its own docid: 1->1, 2->2, 3->3. A
	// once-evaluated pattern would repeat one docid three times (or, for a
	// pattern read before any row was positioned, select nothing).
	_, rows, err := p.Query(`SELECT q.id, ft.docid FROM ft, q WHERE ft MATCH q.y ORDER BY q.id`)
	if err != nil {
		t.Fatalf("per-row MATCH: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("per-row MATCH: %d rows, want 3 -- the pattern is not being re-read per row", len(rows))
	}
	for i, r := range rows {
		if r[0].Typ != Int || r[1].Typ != Int || r[0].I != int64(i+1) || r[1].I != int64(i+1) {
			t.Fatalf("per-row MATCH row %d = %v, want id=docid=%d", i, r, i+1)
		}
	}
}

// TestFts3MatchPatternValueConversion pins the CONVERSION the compiled route
// now performs on the register's value, which is sqlite3_value_text's and not
// a cast (fts3.c:3365): an INTEGER query is its decimal text, a BLOB is its
// bytes read as text, and NULL matches nothing rather than erroring. These
// all went through fts3MatchPattern's literal shortcut before; they go through
// fts3MatchQueryOfValue now, and the answers must be identical.
func TestFts3MatchPatternValueConversion(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts3(c)`,
		`INSERT INTO ft(docid,c) VALUES(1,'alpha')`,
		`INSERT INTO ft(docid,c) VALUES(2,'5')`,
	)
	for _, c := range []struct {
		q    string
		want []int64
	}{
		{`SELECT docid FROM ft WHERE ft MATCH 5`, []int64{2}},
		{`SELECT docid FROM ft WHERE ft MATCH 2+3`, []int64{2}},
		{`SELECT docid FROM ft WHERE ft MATCH x'616c706861'`, []int64{1}},
		{`SELECT docid FROM ft WHERE ft MATCH CAST(5 AS TEXT)`, []int64{2}},
		{`SELECT docid FROM ft WHERE ft MATCH NULL`, nil},
		{`SELECT docid FROM ft WHERE ft MATCH upper('alpha')`, []int64{1}},
		{`SELECT docid FROM ft WHERE ft MATCH 'al'||'pha'`, []int64{1}},
	} {
		_, rows, err := p.Query(c.q)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.q, err)
			continue
		}
		got := vtabRowsInt(t, rows, 0)
		if !eqInts(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.q, got, c.want)
		}
	}
}

// TestFts3MatchDeclineTextMentionsMatch guards the one behaviour change the
// compiled pattern could hide: a pattern the compiler cannot lower declines
// the whole statement (AGENTS.md Rule 1 -- there is nowhere else to route
// it). It must still be a clean error, never a panic and never zero rows
// claiming success.
func TestFts3MatchUnlowerablePatternDeclines(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts3(c)`,
		`INSERT INTO ft(docid,c) VALUES(1,'alpha')`,
	)
	// A pattern naming a column that does not exist is the simplest
	// unlowerable one; the point is the SHAPE of the failure, not this
	// particular expression.
	_, rows, err := p.Query(`SELECT docid FROM ft WHERE ft MATCH nosuchcolumn`)
	if err == nil {
		t.Fatalf("unlowerable MATCH pattern: no error, %d rows", len(rows))
	}
	if strings.Contains(err.Error(), "panic") {
		t.Fatalf("unlowerable MATCH pattern: %v", err)
	}
}
