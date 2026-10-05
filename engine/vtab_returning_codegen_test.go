package engine

// INSERT INTO <vtab> ... RETURNING is lowered by compiling each output
// column into a program over the written row, matching C's approach.

import (
	"strings"
	"testing"
)

// vtabInsertReturningShapes: the statements this promotion is about.
var vtabInsertReturningShapes = []conflictShapeCase{
	{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, `INSERT INTO g VALUES('a','b') RETURNING x`},
	{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, `INSERT INTO g VALUES('a','b') RETURNING *`},
	{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, `INSERT INTO g VALUES('a','b') RETURNING rowid, x||y AS c, typeof(x)`},
	{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, `INSERT INTO g VALUES('a','b'),('c','d') RETURNING rowid,x`},
	{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, `INSERT INTO g(y,x) VALUES('B','A') RETURNING x,y`},
	{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, `INSERT INTO g(rowid,x,y) VALUES(7,'a','b') RETURNING rowid,x`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`}, `INSERT INTO r VALUES(1,0.5,1.5) RETURNING *`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`}, `INSERT INTO r VALUES(1,0,1) RETURNING id, typeof(x0)`},
	// FROM-LESS subquery.
	{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, `INSERT INTO g VALUES('a','b') RETURNING (SELECT 1)`},
	// fts3/fts4 (store-less).
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`}, `INSERT INTO f VALUES('a') RETURNING x`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts3(x)`}, `INSERT INTO f VALUES('a') RETURNING x`},
}

// TestVtabInsertReturningCompilesToBytecode checks that each shape compiles
// to bytecode containing an OpVInsert to perform the write.
func TestVtabInsertReturningCompilesToBytecode(t *testing.T) {
	for i, tc := range vtabInsertReturningShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
			continue
		}
		found := false
		for j := range prog.Insns {
			if prog.Insns[j].Op == OpVInsert {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("[%d] %q compiled without an OpVInsert -- an emitter that produced Init/Halt\n"+
				"and nothing else would compile just as cleanly while writing nothing.", i, tc.stmt)
		}
	}
}

// TestVtabInsertReturningCaptureIsCompiled verifies that output columns
// compile to programs, except for table-reading subqueries which cannot.
func TestVtabInsertReturningCaptureIsCompiled(t *testing.T) {
	for _, tc := range []struct {
		setup    []string
		tbl      string
		stmt     string
		compiled bool
	}{
		{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, "g", `INSERT INTO g VALUES('a','b') RETURNING x`, true},
		{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, "g", `INSERT INTO g VALUES('a','b') RETURNING *`, true},
		{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, "g", `INSERT INTO g VALUES('a','b') RETURNING rowid, upper(x)||y`, true},
		{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`}, "g", `INSERT INTO g VALUES('a','b') RETURNING (SELECT 1)`, true},
		{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`}, "r", `INSERT INTO r VALUES(1,0,1) RETURNING *`, true},
		{[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`, `CREATE TABLE s(z)`}, "g",
			`INSERT INTO g VALUES('a','b') RETURNING x,(SELECT count(*) FROM s)`, false},
	} {
		t.Run(tc.stmt, func(t *testing.T) {
			db, err := Create(t.TempDir()+"/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			stmt, perr := parseInsertStmt(tc.stmt)
			if perr != nil {
				t.Fatalf("parse: %v", perr)
			}
			vm := db.findVtabMetaIn(scopeAny, tc.tbl)
			if vm == nil || vm.store == nil {
				t.Fatalf("no writable vtab %q", tc.tbl)
			}
			scope := vtabEvalScope(tc.tbl, vtabColumnInfos(vm.store.vtabColumns()))
			mode := colNameMode{full: db.fullColumnNames, short: !db.shortColumnNamesOff}
			plan, verr := buildVtabReturningPlan(scope, stmt.returning, mode, nil)
			if verr != nil {
				t.Fatalf("buildVtabReturningPlan: %v", verr)
			}
			if len(plan.progs) != len(plan.outCols) {
				t.Fatalf("plan carries %d programs for %d output columns", len(plan.progs), len(plan.outCols))
			}
			if got := plan.compiled(); got != tc.compiled {
				var detail []string
				for i, pr := range plan.progs {
					if pr == nil || pr.prog == nil {
						detail = append(detail, plan.names[i])
					}
				}
				t.Fatalf("plan.compiled() is %v, want %v (columns that did not lower: %v).\n"+
					"compileVtabInsertStmt keys its whole RETURNING decision on this, so a false\n"+
					"positive lowers a statement whose capture has no program to run, and a false\n"+
					"negative declines one that lowers fine.", got, tc.compiled, detail)
			}
		})
	}
}

// TestVtabInsertReturningCompiledAnswers verifies rows, column names, error
// text, and counters for various insertion scenarios.
func TestVtabInsertReturningCompiledAnswers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    []string
		stmt     string
		wantCols string
		wantRows []string
		wantErr  string
		changes  int64
		inserted int64
		query    string
		want     []string
	}{
		{
			name:  "fts5 one column",
			setup: []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			stmt:  `INSERT INTO g VALUES('a','b') RETURNING x`, wantCols: "x", wantRows: []string{"a"},
			changes: 1, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,a,b"},
		},
		{
			name:  "fts5 star",
			setup: []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			stmt:  `INSERT INTO g VALUES('a','b') RETURNING *`, wantCols: "x|y", wantRows: []string{"a,b"},
			changes: 1, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,a,b"},
		},
		{
			name:     "fts5 rowid and expressions",
			setup:    []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			stmt:     `INSERT INTO g VALUES('a','b') RETURNING rowid, x||y AS c, typeof(x)`,
			wantCols: "rowid|c|typeof(x)", wantRows: []string{"-1,ab,text"},
			changes: 1, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,a,b"},
		},
		{
			name:     "fts5 multi-row, one capture per row",
			setup:    []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			stmt:     `INSERT INTO g VALUES('a','b'),('c','d') RETURNING rowid,x`,
			wantCols: "rowid|x", wantRows: []string{"-1,a", "-1,c"},
			changes: 2, inserted: 2, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,a,b", "2,c,d"},
		},
		{
			name:  "fts5 named out of order",
			setup: []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			stmt:  `INSERT INTO g(y,x) VALUES('B','A') RETURNING x,y`, wantCols: "x|y", wantRows: []string{"A,B"},
			changes: 1, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,A,B"},
		},
		{
			name:  "fts5 explicit rowid",
			setup: []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			stmt:  `INSERT INTO g(rowid,x,y) VALUES(7,'a','b') RETURNING rowid,x`, wantCols: "rowid|x", wantRows: []string{"7,a"},
			changes: 1, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"7,a,b"},
		},
		{
			name:     "fts5 OR REPLACE displacing a row",
			setup:    []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`, `INSERT INTO g(rowid,x,y) VALUES(1,'p','q')`},
			stmt:     `INSERT OR REPLACE INTO g(rowid,x,y) VALUES(1,'a','b') RETURNING rowid,x`,
			wantCols: "rowid|x", wantRows: []string{"1,a"},
			changes: 1, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,a,b"},
		},
		{
			name:     "fts5 OR IGNORE returns the row it did not store",
			setup:    []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`, `INSERT INTO g(rowid,x,y) VALUES(1,'p','q')`},
			stmt:     `INSERT OR IGNORE INTO g(rowid,x,y) VALUES(1,'a','b') RETURNING rowid,x`,
			wantCols: "rowid|x", wantRows: []string{"1,a"},
			changes: 0, inserted: 0, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,p,q"},
		},
		{
			name:    "fts5 capture error on the second row",
			setup:   []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			stmt:    `INSERT INTO g VALUES(1,'p'),(1000000000000,'q') RETURNING zeroblob(x)`,
			wantErr: `engine: RETURNING: engine: string or blob too big`,
			changes: 0, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: nil,
		},
		{
			name:     "fts5 command channel returns an empty row",
			setup:    []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`, `INSERT INTO g VALUES('seed','row')`},
			stmt:     `INSERT INTO g(g) VALUES('rebuild') RETURNING rowid, x, y`,
			wantCols: "rowid|x|y", wantRows: []string{"-1,<NULL>,<NULL>"},
			changes: 1, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,seed,row"},
		},
		{
			name:     "fts5 DEFAULT VALUES",
			setup:    []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			stmt:     `INSERT INTO g DEFAULT VALUES RETURNING rowid,x,y`,
			wantCols: "rowid|x|y", wantRows: []string{"-1,<NULL>,<NULL>"},
			changes: 1, inserted: 1, query: `SELECT rowid,x,y FROM g ORDER BY rowid`, want: []string{"1,<NULL>,<NULL>"},
		},
		{
			name:  "rtree star, the module's REAL coercion reaching the capture",
			setup: []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			stmt:  `INSERT INTO r VALUES(1,0.5,1.5) RETURNING *`, wantCols: "id|x0|x1", wantRows: []string{"1,0.5,1.5"},
			changes: 1, inserted: 1, query: `SELECT id,x0,x1 FROM r`, want: []string{"1,0.5,1.5"},
		},
		{
			name:     "rtree typeof reads the DECLARED affinity of the candidate",
			setup:    []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			stmt:     `INSERT INTO r VALUES(1,0,1) RETURNING id, typeof(x0)`,
			wantCols: "id|typeof(x0)", wantRows: []string{"1,real"},
			changes: 1, inserted: 1, query: `SELECT id,x0,x1 FROM r`, want: []string{"1,0,1"},
		},
		{
			// fts3/fts4 is STORE-LESS -- its rows live in %_content/%_segdir,
			// not a vtabStore -- and used to refuse RETURNING outright for
			// that reason. It no longer needs one: the capture reads the
			// CANDIDATE tuple, whose column scope comes from the declared
			// column list. Oracle-verified, along with the docid and multi-row
			// spellings; see insertIntoFts3 (vtab_fts3.go).
			name:     "fts4 RETURNING is served without a store",
			setup:    []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			stmt:     `INSERT INTO f VALUES('a') RETURNING rowid, docid, x, typeof(x)`,
			wantCols: "rowid|docid|x|typeof(x)", wantRows: []string{"-1,<NULL>,a,text"},
			changes: 1, inserted: 1, query: `SELECT docid,x FROM f`, want: []string{"1,a"},
		},
		{
			name:     "fts4 RETURNING an explicit docid",
			setup:    []string{`CREATE VIRTUAL TABLE f USING fts4(x)`},
			stmt:     `INSERT INTO f(docid,x) VALUES(9,'b') RETURNING rowid, docid, x`,
			wantCols: "rowid|docid|x", wantRows: []string{"-1,9,b"},
			changes: 1, inserted: 1, query: `SELECT docid,x FROM f`, want: []string{"9,b"},
		},
		{
			name:     "fts3 multi-row RETURNING star",
			setup:    []string{`CREATE VIRTUAL TABLE f USING fts3(x)`},
			stmt:     `INSERT INTO f VALUES('c'),('d') RETURNING *`,
			wantCols: "x", wantRows: []string{"c", "d"},
			changes: 2, inserted: 2, query: `SELECT docid,x FROM f`, want: []string{"1,c", "2,d"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir()+"/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("compile %q: %v", tc.stmt, cerr)
			}
			cols, rows, xerr := db.ExecReturningArgs(tc.stmt, nil)
			if tc.wantErr != "" {
				if xerr == nil {
					t.Fatalf("%q SUCCEEDED, want %q", tc.stmt, tc.wantErr)
				}
				if xerr.Error() != tc.wantErr {
					t.Fatalf("%q: got %q, want %q (the wording recorded before the promotion, which must not drift)",
						tc.stmt, xerr.Error(), tc.wantErr)
				}
			} else {
				if xerr != nil {
					t.Fatalf("%q: %v", tc.stmt, xerr)
				}
				if got := strings.Join(cols, "|"); got != tc.wantCols {
					t.Fatalf("%q RETURNING columns are %q, want %q", tc.stmt, got, tc.wantCols)
				}
				if got := rvdRowStrings(rows); strings.Join(got, "|") != strings.Join(tc.wantRows, "|") {
					t.Fatalf("%q RETURNING rows are %v, want %v", tc.stmt, got, tc.wantRows)
				}
			}
			if db.nChange != tc.changes {
				t.Fatalf("%q left changes() at %d, want %d", tc.stmt, db.nChange, tc.changes)
			}
			if got := db.RowsInserted(); got != tc.inserted {
				t.Fatalf("%q left the rows-inserted counter at %d, want %d", tc.stmt, got, tc.inserted)
			}
			stored := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(stored, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("%q stored %v, want %v", tc.stmt, stored, tc.want)
			}
		})
	}
}

// TestVtabReturningCaptureArmIsGated verifies that every output column
// compiles to a runnable program.
func TestVtabReturningCaptureArmIsGated(t *testing.T) {
	for _, tc := range []struct {
		name           string
		setup          []string
		stmt           string
		wantUncompiled int
		explanation    string
	}{
		{"fts5 plain column", []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			`INSERT INTO g VALUES('a','b') RETURNING x`, 0, "one column, compiled"},
		{"fts5 star", []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			`INSERT INTO g VALUES('a','b') RETURNING *`, 0, "two columns, both compiled"},
		{"fts5 expression", []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			`INSERT INTO g VALUES('a','b') RETURNING rowid, upper(x)||y`, 0, "two columns, both compiled"},
		{"rtree star", []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`},
			`INSERT INTO r VALUES(1,0,1) RETURNING *`, 0, "three columns, all compiled"},
		{"fts5 multi-row compiles per row", []string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`},
			`INSERT INTO g VALUES('a','b'),('c','d') RETURNING x`, 0, "two rows x one column"},
		{"table-reading subquery compiles too",
			[]string{`CREATE VIRTUAL TABLE g USING fts5(x,y)`, `CREATE TABLE s(z)`},
			`INSERT INTO g VALUES('a','b') RETURNING x,(SELECT count(*) FROM s)`, 0,
			"the subquery column lowers now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir()+"/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, _, err := db.ExecReturningArgs(tc.stmt, nil); err != nil {
				t.Fatalf("%q: %v (%s) -- every column here must lower; the "+
					"second capture arm no longer exists to catch it",
					tc.stmt, err, tc.explanation)
			}
			if tc.wantUncompiled != 0 {
				t.Fatalf("%q expects %d uncompiled captures, but the arm is gone; "+
					"rewrite or delete this case", tc.stmt, tc.wantUncompiled)
			}
		})
	}
}

// TestVtabReturningIsABeforeTrigger verifies that virtual table RETURNING
// clauses execute as BEFORE triggers, with correct rowid and affinity handling.
func TestVtabReturningIsABeforeTrigger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  string
		stmt   string
		oracle string // what 3.53.3 answers
	}{
		{"rowid is the BEFORE trigger's -1", `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r VALUES(1,2,3) RETURNING rowid, id`, "-1,1"},
		{"a NULL ipk is still NULL: the module has not assigned one yet", `CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
			`INSERT INTO r(id,x0,x1) VALUES(NULL,1,2) RETURNING id, rowid`, "NULL,-1"},
		{"a coordinate reads as WRITTEN under the declared affinity, not as stored", `CREATE VIRTUAL TABLE q USING rtree_i32(id,x0,x1)`,
			`INSERT INTO q VALUES(1,2.7,3.9) RETURNING x0, typeof(x0)`, "2.7,real"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir()+"/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			if err := db.Exec(tc.setup); err != nil {
				t.Fatalf("setup: %v", err)
			}
			_, rows, rerr := db.ExecReturningArgs(tc.stmt, nil)
			if rerr != nil {
				t.Fatalf("%q: %v", tc.stmt, rerr)
			}
			if len(rows) != 1 {
				t.Fatalf("%q: got %d rows, want 1", tc.stmt, len(rows))
			}
			got := make([]string, len(rows[0]))
			for i, v := range rows[0] {
				got[i] = valueDebugString(v)
			}
			if s := strings.Join(got, ","); s != tc.oracle {
				t.Errorf("%q answers %q, want the oracle's %q", tc.stmt, s, tc.oracle)
			}
		})
	}
}
