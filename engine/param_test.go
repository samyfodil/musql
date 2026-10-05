// Bound parameter support tested end-to-end: lexing, parsing, numbering, binding.
package engine

import (
	"path/filepath"
	"testing"
)

// mustParseSelect parses SELECT, failing on error.
func mustParseSelect(t *testing.T, sql string) *SelectStmt {
	t.Helper()
	stmt, err := ParseSelect(sql)
	if err != nil {
		t.Fatalf("ParseSelect(%q): %v", sql, err)
	}
	return stmt
}

// paramExprAt extracts the ParamExpr at position i in the select list.
func paramExprAt(t *testing.T, stmt *SelectStmt, i int) ParamExpr {
	t.Helper()
	pe, ok := stmt.Columns[i].Expr.(ParamExpr)
	if !ok {
		t.Fatalf("column %d: got %T, want ParamExpr", i, stmt.Columns[i].Expr)
	}
	return pe
}

// TestParamNumberingBasic covers the anonymous "?" and explicit "?NNN" forms
// and their interaction, matching SQLite's exact sqlite3_bind_parameter_index
// rule: a bare "?" is one more than the largest parameter index already
// assigned anywhere to its left, by ANY form.
func TestParamNumberingBasic(t *testing.T) {
	t.Run("bare ? numbers sequentially from 1", func(t *testing.T) {
		stmt := mustParseSelect(t, "SELECT ?, ?, ?")
		for i := 0; i < 3; i++ {
			if pe := paramExprAt(t, stmt, i); pe.Index != i+1 {
				t.Errorf("column %d: Index=%d, want %d", i, pe.Index, i+1)
			}
		}
		if stmt.Params.NumParams != 3 {
			t.Errorf("NumParams=%d, want 3", stmt.Params.NumParams)
		}
	})

	t.Run("?NNN explicit, then bare ? continues from the max seen", func(t *testing.T) {
		// "?5" assigns index 5 directly; the following bare "?" must become
		// 6 (one more than the largest index assigned so far), NOT 2 (a
		// naive "count of anonymous params seen" would wrongly give 2).
		stmt := mustParseSelect(t, "SELECT ?5, ?")
		if pe := paramExprAt(t, stmt, 0); pe.Index != 5 {
			t.Errorf("?5: Index=%d, want 5", pe.Index)
		}
		if pe := paramExprAt(t, stmt, 1); pe.Index != 6 {
			t.Errorf("bare ? after ?5: Index=%d, want 6", pe.Index)
		}
		if stmt.Params.NumParams != 6 {
			t.Errorf("NumParams=%d, want 6", stmt.Params.NumParams)
		}
	})

	t.Run("?NNN out of order", func(t *testing.T) {
		stmt := mustParseSelect(t, "SELECT ?2, ?1")
		if pe := paramExprAt(t, stmt, 0); pe.Index != 2 {
			t.Errorf("?2: Index=%d, want 2", pe.Index)
		}
		if pe := paramExprAt(t, stmt, 1); pe.Index != 1 {
			t.Errorf("?1: Index=%d, want 1", pe.Index)
		}
		if stmt.Params.NumParams != 2 {
			t.Errorf("NumParams=%d, want 2", stmt.Params.NumParams)
		}
	})

	t.Run("?NNN leaves gaps that still count toward NumParams", func(t *testing.T) {
		// "?1, ?5, ?" -- ?5 is index 5, the trailing bare ? becomes 6, even
		// though indices 2-4 are never referenced anywhere in the statement
		// (matches C SQLite: sqlite3_bind_parameter_count is the highest
		// index used, not the count of distinct ones referenced).
		stmt := mustParseSelect(t, "SELECT ?1, ?5, ?")
		if pe := paramExprAt(t, stmt, 2); pe.Index != 6 {
			t.Errorf("bare ? after ?1,?5: Index=%d, want 6", pe.Index)
		}
		if stmt.Params.NumParams != 6 {
			t.Errorf("NumParams=%d, want 6", stmt.Params.NumParams)
		}
	})

	t.Run("?NNN repeated reuses the same index", func(t *testing.T) {
		stmt := mustParseSelect(t, "SELECT ?1, ?1")
		if stmt.Params.NumParams != 1 {
			t.Errorf("NumParams=%d, want 1", stmt.Params.NumParams)
		}
	})
}

// TestParamNumberingNamed covers :name/@name/$name first-appearance
// numbering and same-name reuse, and confirms different sigils for the
// "same" bare name are DISTINCT parameters (verified against C SQLite via
// mattn/go-sqlite3: ":foo" and "@foo" do NOT unify -- see
// compat-harness/param_diff_test.go's oracle-verified equivalent).
func TestParamNumberingNamed(t *testing.T) {
	t.Run("named param first-appearance numbering", func(t *testing.T) {
		stmt := mustParseSelect(t, "SELECT :a, @b, $c")
		wantIdx := []int{1, 2, 3}
		for i, want := range wantIdx {
			if pe := paramExprAt(t, stmt, i); pe.Index != want {
				t.Errorf("column %d: Index=%d, want %d", i, pe.Index, want)
			}
		}
		if stmt.Params.NumParams != 3 {
			t.Errorf("NumParams=%d, want 3", stmt.Params.NumParams)
		}
		want := map[string]int{":a": 1, "@b": 2, "$c": 3}
		for name, idx := range want {
			if got, ok := stmt.Params.ParamNames[name]; !ok || got != idx {
				t.Errorf("ParamNames[%q]=%d,%v want %d,true", name, got, ok, idx)
			}
		}
	})

	t.Run("repeated :name resolves to the same index", func(t *testing.T) {
		stmt := mustParseSelect(t, "SELECT :x, :x")
		p0, p1 := paramExprAt(t, stmt, 0), paramExprAt(t, stmt, 1)
		if p0.Index != p1.Index {
			t.Errorf("repeated :x: indices differ: %d vs %d", p0.Index, p1.Index)
		}
		if stmt.Params.NumParams != 1 {
			t.Errorf("NumParams=%d, want 1", stmt.Params.NumParams)
		}
		if len(stmt.Params.ParamNames) != 1 {
			t.Errorf("ParamNames=%v, want exactly one entry", stmt.Params.ParamNames)
		}
	})

	t.Run(":foo and @foo are distinct parameters", func(t *testing.T) {
		stmt := mustParseSelect(t, "SELECT :foo, @foo")
		p0, p1 := paramExprAt(t, stmt, 0), paramExprAt(t, stmt, 1)
		if p0.Index == p1.Index {
			t.Errorf(":foo and @foo unified to the same index %d; C SQLite treats them as distinct parameters", p0.Index)
		}
		if stmt.Params.NumParams != 2 {
			t.Errorf("NumParams=%d, want 2", stmt.Params.NumParams)
		}
	})

	t.Run("named param mixed with bare ? shares the numbering sequence", func(t *testing.T) {
		stmt := mustParseSelect(t, "SELECT :a, ?")
		p0, p1 := paramExprAt(t, stmt, 0), paramExprAt(t, stmt, 1)
		if p0.Index != 1 || p1.Index != 2 {
			t.Errorf("Index=(%d,%d), want (1,2)", p0.Index, p1.Index)
		}
	})
}

// TestParamInsertUpdateDeleteParamInfo confirms INSERT/UPDATE/DELETE also
// expose ParamInfo (via ParseParamInfo, insert_write.go) with the same
// numbering rules as SELECT, since they share the same parser/paramTracker
// machinery.
func TestParamInsertUpdateDeleteParamInfo(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"insert values", "INSERT INTO t VALUES (?, ?5, ?)", 6},
		{"insert named", "INSERT INTO t(a,b) VALUES (:x, :x)", 1},
		{"update set+where", "UPDATE t SET a = ? WHERE b = ?", 2},
		{"update named", "UPDATE t SET a = :v WHERE id = :v", 1},
		{"delete where", "DELETE FROM t WHERE a = ?1 OR b = ?3", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, err := ParseParamInfo(c.sql)
			if err != nil {
				t.Fatalf("ParseParamInfo(%q): %v", c.sql, err)
			}
			if info.NumParams != c.want {
				t.Errorf("NumParams=%d, want %d", info.NumParams, c.want)
			}
		})
	}

	t.Run("CREATE TABLE has zero params, no error", func(t *testing.T) {
		info, err := ParseParamInfo("CREATE TABLE t(a, b)")
		if err != nil {
			t.Fatalf("ParseParamInfo: %v", err)
		}
		if info.NumParams != 0 {
			t.Errorf("NumParams=%d, want 0", info.NumParams)
		}
	})
}

// TestBindByName covers BindByName (sql_ast.go): resolving a name->Value
// binding into a positional args slice via a statement's ParamInfo.
func TestBindByName(t *testing.T) {
	stmt := mustParseSelect(t, "SELECT :a, ?, @b")
	// Indices: :a=1, bare ?=2, @b=3.
	args := BindByName(stmt.Params, map[string]Value{
		":a": {Typ: Int, I: 10},
		"@b": {Typ: Text, S: []byte("hi")},
	})
	if len(args) != 3 {
		t.Fatalf("len(args)=%d, want 3", len(args))
	}
	if args[0].Typ != Int || args[0].I != 10 {
		t.Errorf("args[0]=%v, want Int 10", args[0])
	}
	if args[1].Typ != Null {
		t.Errorf("args[1] (unbound anonymous ?)=%v, want NULL", args[1])
	}
	if args[2].Typ != Text || string(args[2].S) != "hi" {
		t.Errorf("args[2]=%v, want Text 'hi'", args[2])
	}
}

// buildParamTestDB creates a tiny engine-written database (no reference
// engine involved -- see compat-harness/param_diff_test.go for the
// differential-vs-real-SQLite gate) for the end-to-end QueryArgs/ExecArgs
// tests below.
func buildParamTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "param.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, val INTEGER)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	for _, row := range []string{
		"INSERT INTO t VALUES (1, 'a', 10)",
		"INSERT INTO t VALUES (2, 'b', 20)",
		"INSERT INTO t VALUES (3, 'c', 30)",
	} {
		if err := db.Exec(row); err != nil {
			t.Fatalf("%s: %v", row, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func TestQueryArgsPositional(t *testing.T) {
	path := buildParamTestDB(t)
	pager, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pager.Close()

	cols, rows, err := pager.QueryArgs("SELECT name, val FROM t WHERE id = ?", []Value{{Typ: Int, I: 2}})
	if err != nil {
		t.Fatalf("QueryArgs: %v", err)
	}
	if len(rows) != 1 || string(rows[0][0].S) != "b" || rows[0][1].I != 20 {
		t.Errorf("got cols=%v rows=%v, want one row (b, 20)", cols, rows)
	}
}

func TestQueryArgsOutOfOrderAndInList(t *testing.T) {
	path := buildParamTestDB(t)
	pager, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pager.Close()

	// ?2, ?1 out of order in the select list.
	_, rows, err := pager.QueryArgs("SELECT ?2, ?1", []Value{{Typ: Int, I: 100}, {Typ: Int, I: 200}})
	if err != nil {
		t.Fatalf("QueryArgs: %v", err)
	}
	if len(rows) != 1 || rows[0][0].I != 200 || rows[0][1].I != 100 {
		t.Errorf("got %v, want [[200 100]]", rows)
	}

	// Param in an IN list.
	_, rows, err = pager.QueryArgs("SELECT id FROM t WHERE id IN (?, ?) ORDER BY id", []Value{{Typ: Int, I: 1}, {Typ: Int, I: 3}})
	if err != nil {
		t.Fatalf("QueryArgs: %v", err)
	}
	if len(rows) != 2 || rows[0][0].I != 1 || rows[1][0].I != 3 {
		t.Errorf("got %v, want [[1] [3]]", rows)
	}

	// Param in LIMIT/OFFSET.
	_, rows, err = pager.QueryArgs("SELECT id FROM t ORDER BY id LIMIT ? OFFSET ?", []Value{{Typ: Int, I: 1}, {Typ: Int, I: 1}})
	if err != nil {
		t.Fatalf("QueryArgs: %v", err)
	}
	if len(rows) != 1 || rows[0][0].I != 2 {
		t.Errorf("got %v, want [[2]]", rows)
	}
}

func TestQueryArgsRepeatedNamedParam(t *testing.T) {
	path := buildParamTestDB(t)
	pager, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pager.Close()

	stmt, err := ParseSelect("SELECT :x, :x")
	if err != nil {
		t.Fatalf("ParseSelect: %v", err)
	}
	args := BindByName(stmt.Params, map[string]Value{":x": {Typ: Int, I: 42}})
	cols, rows, err := pager.QueryArgs("SELECT :x, :x", args)
	if err != nil {
		t.Fatalf("QueryArgs: %v", err)
	}
	if len(rows) != 1 || rows[0][0].I != 42 || rows[0][1].I != 42 {
		t.Errorf("got cols=%v rows=%v, want [[42 42]]", cols, rows)
	}
}

func TestQueryArgsUnboundTrailingParamIsNull(t *testing.T) {
	path := buildParamTestDB(t)
	pager, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pager.Close()

	// ?1 is bound; ?2 has no corresponding entry in args at all.
	_, rows, err := pager.QueryArgs("SELECT ?1, ?2", []Value{{Typ: Int, I: 7}})
	if err != nil {
		t.Fatalf("QueryArgs: %v", err)
	}
	if len(rows) != 1 || rows[0][0].I != 7 || rows[0][1].Typ != Null {
		t.Errorf("got %v, want [[7 NULL]]", rows)
	}
}

func TestQueryArgsEveryValueType(t *testing.T) {
	path := buildParamTestDB(t)
	pager, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pager.Close()

	args := []Value{
		{Typ: Null},
		{Typ: Int, I: -7},
		{Typ: Float, F: 3.5},
		{Typ: Text, S: []byte("hi")},
		{Typ: Blob, S: []byte{0xde, 0xad}},
	}
	_, rows, err := pager.QueryArgs("SELECT ?, ?, ?, ?, ?", args)
	if err != nil {
		t.Fatalf("QueryArgs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got[0].Typ != Null {
		t.Errorf("col0=%v, want NULL", got[0])
	}
	if got[1].Typ != Int || got[1].I != -7 {
		t.Errorf("col1=%v, want Int -7", got[1])
	}
	if got[2].Typ != Float || got[2].F != 3.5 {
		t.Errorf("col2=%v, want Float 3.5", got[2])
	}
	if got[3].Typ != Text || string(got[3].S) != "hi" {
		t.Errorf("col3=%v, want Text 'hi'", got[3])
	}
	if got[4].Typ != Blob || string(got[4].S) != "\xde\xad" {
		t.Errorf("col4=%v, want Blob de ad", got[4])
	}
}

func TestInsertUpdateDeleteArgs(t *testing.T) {
	path := buildParamTestDB(t)
	db, err := OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}

	if _, err := db.InsertArgs("INSERT INTO t VALUES (?, ?, ?)", []Value{
		{Typ: Int, I: 4}, {Typ: Text, S: []byte("d")}, {Typ: Int, I: 40},
	}); err != nil {
		t.Fatalf("InsertArgs: %v", err)
	}

	// Bind both positionally: ?=1 is the new val, :n=2 is the WHERE name.
	n, err := db.UpdateArgs("UPDATE t SET val = ? WHERE name = :n", []Value{
		{Typ: Int, I: 999}, {Typ: Text, S: []byte("a")},
	})
	if err != nil {
		t.Fatalf("UpdateArgs: %v", err)
	}
	if n != 1 {
		t.Errorf("UpdateArgs affected %d rows, want 1", n)
	}

	n, err = db.DeleteArgs("DELETE FROM t WHERE id = ?", []Value{{Typ: Int, I: 2}})
	if err != nil {
		t.Fatalf("DeleteArgs: %v", err)
	}
	if n != 1 {
		t.Errorf("DeleteArgs affected %d rows, want 1", n)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	pager, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pager.Close()
	_, rows, err := pager.Query("SELECT id, name, val FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := map[int64][2]any{
		1: {"a", int64(999)},
		3: {"c", int64(30)},
		4: {"d", int64(40)},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		id := r[0].I
		w, ok := want[id]
		if !ok {
			t.Fatalf("unexpected row id=%d", id)
		}
		if string(r[1].S) != w[0] || r[2].I != w[1] {
			t.Errorf("row id=%d: got (%s,%d), want %v", id, r[1].S, r[2].I, w)
		}
	}
}

// TestUnparameterizedQueryUnaffected is a narrow regression check that plain
// literal SQL (no placeholders at all) still behaves exactly as before this
// feature was added -- the bulk of that assurance comes from the existing
// engine test suite (query_test.go etc.) and the SLT read gate
// (TestPureEngineSLTCoverage, compat-harness) continuing to pass unchanged.
func TestUnparameterizedQueryUnaffected(t *testing.T) {
	path := buildParamTestDB(t)
	pager, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pager.Close()

	cols, rows, err := pager.Query("SELECT name, val FROM t WHERE id = 2")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 || string(rows[0][0].S) != "b" || rows[0][1].I != 20 {
		t.Errorf("got cols=%v rows=%v, want one row (b, 20)", cols, rows)
	}
	stmt := mustParseSelect(t, "SELECT name, val FROM t WHERE id = 2")
	if stmt.Params.NumParams != 0 || stmt.Params.ParamNames != nil {
		t.Errorf("unparameterized statement has non-zero Params: %+v", stmt.Params)
	}
}
