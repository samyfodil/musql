// Tests that a MATCH's right operand is compiled to a value, never walked
// as an AST. This covers both the read path (OpMatch) and write path (virtual
// table DELETE/UPDATE WHERE MATCH). The main wrong answers gated are:
// MATCH operands that read the matched table (destructive deletes/updates);
// NULL MATCH queries (parse error in fts5, matches nothing in fts3/fts4);
// "NOT MATCH" as a non-liftable constraint.
package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// fts5OperandDB builds a fresh database and runs stmts through it.
func fts5OperandDB(t *testing.T, stmts ...string) *Session {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "m.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			t.Fatalf("Exec(%s): %v", s, err)
		}
	}
	return db
}

func fts5OperandRows(t *testing.T, db *Session, q string) [][]Value {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, qerr := p.Query(q)
	if qerr != nil {
		t.Fatalf("Query(%s): %v", q, qerr)
	}
	return rows
}

// TestFts5MatchPatternIsCompiledToARegister checks that OpMatch carries a
// compiled pattern register with instructions before it.
func TestFts5MatchPatternIsCompiledToARegister(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts5(c)`,
		`INSERT INTO ft(rowid,c) VALUES(1,'alpha beta')`,
		`CREATE TABLE q(id INTEGER PRIMARY KEY, y)`,
		`INSERT INTO q VALUES(1,'alpha')`,
	)
	for _, sqlText := range []string{
		`SELECT rowid FROM ft WHERE ft MATCH 'al'||'pha'`,
		`SELECT rowid FROM ft WHERE ft MATCH 'alpha'`,
		`SELECT ft.rowid FROM ft, q WHERE ft MATCH q.y`,
		// UNUSABLE form: pattern register must be present before raise.
		`SELECT 'lit' MATCH q.y FROM q`,
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
					"the query is still being walked at run time (evalMatch)", sqlText)
				continue
			}
			if info.patReg < 0 || info.patReg >= prog.NReg {
				t.Fatalf("%s: pattern register %d is outside the program's %d registers",
					sqlText, info.patReg, prog.NReg)
			}
			// Check all three operand slots P1/P2/P3 for the pattern register.
			written := false
			for j := 0; j < i; j++ {
				w := prog.Insns[j]
				if w.P1 == info.patReg || w.P2 == info.patReg || w.P3 == info.patReg {
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

// TestFts5MatchPatternIsPerRow checks that the pattern register is
// recomputed for every row when the pattern comes from another table.
func TestFts5MatchPatternIsPerRow(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts5(c)`,
		`INSERT INTO ft(rowid,c) VALUES(1,'alpha')`,
		`INSERT INTO ft(rowid,c) VALUES(2,'beta')`,
		`INSERT INTO ft(rowid,c) VALUES(3,'gamma')`,
		`CREATE TABLE q(id INTEGER PRIMARY KEY, y)`,
		`INSERT INTO q VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
	)
	_, rows, err := p.Query(`SELECT q.id, ft.rowid FROM ft, q WHERE ft MATCH q.y ORDER BY q.id`)
	if err != nil {
		t.Fatalf("per-row MATCH: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("per-row MATCH: %d rows, want 3 -- the pattern is not being re-read per row", len(rows))
	}
	for i, r := range rows {
		if r[0].Typ != Int || r[1].Typ != Int || r[0].I != int64(i+1) || r[1].I != int64(i+1) {
			t.Fatalf("per-row MATCH row %d = %v, want id=rowid=%d", i, r, i+1)
		}
	}
}

// TestMatchPatternMayNotReadTheMatchedTable checks that a MATCH whose
// pattern reads the matched table's columns is rejected as an error,
// where previously it caused destructive deletes and updates.
func TestMatchPatternMayNotReadTheMatchedTable(t *testing.T) {
	for _, mod := range []string{"fts5", "fts4"} {
		db := fts5OperandDB(t,
			`CREATE VIRTUAL TABLE ft USING `+mod+`(a)`,
			`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
		)
		for _, stmt := range []string{
			`DELETE FROM ft WHERE ft MATCH a`,
			`UPDATE ft SET a='x' WHERE ft MATCH a`,
		} {
			if err := db.Exec(stmt); err == nil {
				t.Errorf("%s [%s]: accepted, want an error -- C SQLite reports "+
					"\"unable to use function MATCH in the requested context\"", stmt, mod)
			}
		}
		rows := fts5OperandRows(t, db, `SELECT rowid, a FROM ft ORDER BY rowid`)
		if len(rows) != 3 {
			t.Fatalf("[%s] after the refused statements: %d rows, want 3 -- a refused "+
				"statement wrote to the table", mod, len(rows))
		}
		for i, r := range rows {
			if valueToText(r[1]) == "x" {
				t.Fatalf("[%s] row %d was rewritten by a statement that must have been refused: %v", mod, i, r)
			}
		}
	}
}

// TestMatchPatternOperandIsEvaluatedFirst checks that the pattern operand
// is evaluated before the MATCH error, so pattern errors appear first.
func TestMatchPatternOperandIsEvaluatedFirst(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts5(a)`,
		`INSERT INTO ft(rowid,a) VALUES(1,'alpha')`,
	)
	for _, c := range []struct{ q, want string }{
		{`SELECT 'lit' MATCH abs(-9223372036854775807-1)`, "integer overflow"},
		{`SELECT ft MATCH abs(-9223372036854775807-1) FROM ft`, "integer overflow"},
	} {
		_, _, err := p.Query(c.q)
		if err == nil {
			t.Errorf("%s: accepted, want %q", c.q, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q -- the MATCH target is "+
				"being resolved before its pattern operand is evaluated", c.q, err, c.want)
		}
	}
	// ...and the MATCH error still wins when the pattern itself is fine.
	if _, _, err := p.Query(`SELECT 'lit' MATCH 'x'`); err == nil ||
		!strings.Contains(err.Error(), "unable to use function MATCH") {
		t.Errorf(`SELECT 'lit' MATCH 'x': got %v, want the MATCH error`, err)
	}
}

// TestVtabWriteMatchPatternOperandShapes tests DELETE/UPDATE WHERE MATCH
// patterns compiled as FROM-less selects.
func TestVtabWriteMatchPatternOperandShapes(t *testing.T) {
	for _, c := range []struct {
		stmt string
		left []int64
	}{
		{`DELETE FROM ft WHERE ft MATCH 'alpha'`, []int64{2, 3}},
		{`DELETE FROM ft WHERE ft MATCH 'al'||'pha'`, []int64{2, 3}},
		{`DELETE FROM ft WHERE ft MATCH upper('ALPHA')`, []int64{2, 3}},
		{`DELETE FROM ft WHERE ft MATCH (SELECT 'beta')`, []int64{1, 3}},
		{`DELETE FROM ft WHERE ft MATCH CAST('gamma' AS TEXT)`, []int64{1, 2}},
	} {
		db := fts5OperandDB(t,
			`CREATE VIRTUAL TABLE ft USING fts5(a)`,
			`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
		)
		if err := db.Exec(c.stmt); err != nil {
			t.Errorf("%s: %v", c.stmt, err)
			continue
		}
		rows := fts5OperandRows(t, db, `SELECT rowid FROM ft ORDER BY rowid`)
		got := make([]int64, len(rows))
		for i, r := range rows {
			got[i] = r[0].I
		}
		if !fts5OperandEqInts(got, c.left) {
			t.Errorf("%s: left %v, want %v", c.stmt, got, c.left)
		}
	}
}

// TestNotMatchIsNotALiftableConstraint checks that "X NOT MATCH Y" is
// rejected as a non-liftable constraint, like "NOT (X MATCH Y)".
func TestNotMatchIsNotALiftableConstraint(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts5(a)`,
		`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
		`CREATE VIRTUAL TABLE f3 USING fts4(a)`,
		`INSERT INTO f3(rowid,a) VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
	)
	for _, q := range []string{
		`SELECT rowid FROM ft WHERE ft NOT MATCH 'beta'`,
		`SELECT rowid FROM ft WHERE NOT (ft MATCH 'beta')`,
		`SELECT rowid FROM f3 WHERE f3 NOT MATCH 'beta'`,
		`SELECT rowid FROM f3 WHERE NOT (f3 MATCH 'beta')`,
	} {
		if _, rows, err := p.Query(q); err == nil {
			t.Errorf("%s: answered %d rows, want an error -- C SQLite reports "+
				`"unable to use function MATCH in the requested context"`, q, len(rows))
		}
	}
}

// TestFts5NullMatchQueryIsAnError checks that fts5 rejects NULL query strings
// as a parse error, while fts3/fts4 accept them as matching nothing.
func TestFts5NullMatchQueryIsAnError(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft USING fts5(a)`,
		`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta')`,
		`CREATE VIRTUAL TABLE f3 USING fts4(a)`,
		`INSERT INTO f3(rowid,a) VALUES(1,'alpha'),(2,'beta')`,
	)
	for _, q := range []string{
		`SELECT rowid FROM ft WHERE ft MATCH NULL`,
		`SELECT rowid FROM ft WHERE ft MATCH NULLIF('a','a')`,
		`SELECT rowid FROM ft WHERE ft MATCH (SELECT NULL)`,
	} {
		if _, _, err := p.Query(q); err == nil {
			t.Errorf("%s: accepted, want an error -- C fts5 reports "+
				`"fts5: syntax error near \"\""`, q)
		}
	}
	// The fts3/fts4 arm must NOT have moved: a NULL query there really is
	// "matches nothing", with no error.
	if _, rows, err := p.Query(`SELECT rowid FROM f3 WHERE f3 MATCH NULL`); err != nil || len(rows) != 0 {
		t.Errorf("fts4 MATCH NULL: got %d rows, err %v -- want no rows and no error", len(rows), err)
	}
	// ...and so must the write path's fts5 answer.
	db := fts5OperandDB(t,
		`CREATE VIRTUAL TABLE ft USING fts5(a)`,
		`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta')`,
	)
	if err := db.Exec(`DELETE FROM ft WHERE ft MATCH NULL`); err == nil {
		t.Error(`DELETE FROM ft WHERE ft MATCH NULL: accepted, want an error`)
	}
	if n := len(fts5OperandRows(t, db, `SELECT rowid FROM ft`)); n != 2 {
		t.Errorf("after the refused DELETE: %d rows, want 2", n)
	}
}

// TestVtabWriteMatchPatternReadsParams checks that bound parameters reach
// the compiled MATCH operand from the enclosing DELETE/UPDATE.
func TestVtabWriteMatchPatternReadsParams(t *testing.T) {
	db := fts5OperandDB(t,
		`CREATE VIRTUAL TABLE ft USING fts5(a)`,
		`INSERT INTO ft(rowid,a) VALUES(1,'alpha'),(2,'beta'),(3,'gamma')`,
	)
	if _, _, err := db.ExecArgs(`DELETE FROM ft WHERE ft MATCH ?`, []Value{{Typ: Text, S: []byte("beta")}}); err != nil {
		t.Fatalf("parameterized MATCH delete: %v", err)
	}
	rows := fts5OperandRows(t, db, `SELECT rowid FROM ft ORDER BY rowid`)
	got := make([]int64, len(rows))
	for i, r := range rows {
		got[i] = r[0].I
	}
	if !fts5OperandEqInts(got, []int64{1, 3}) {
		t.Errorf("after \"MATCH ?\" bound to 'beta': left %v, want [1 3]", got)
	}
}

func fts5OperandEqInts(a, b []int64) bool {
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
