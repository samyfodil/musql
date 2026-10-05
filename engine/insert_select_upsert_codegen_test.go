package engine

// UPSERT codegen for INSERT INTO t(...) SELECT ...
// ON CONFLICT(...) DO ... must compile and probe the conflict once per row,
// inside the source scan loop.

import (
	"strings"
	"testing"
)

// insertSelectUpsertShapes: statements whose promotion this slice is about.
var insertSelectUpsertShapes = []conflictShapeCase{
	// UNIQUE constraint with DO UPDATE.
	{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`},
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b`},

	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE s(a,b)`},
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO NOTHING`},
	{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`},
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE t.c=1`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE s(a,b)`},
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`},
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT DO UPDATE SET c=coalesce(c,0)+1`},
	// COMPOUND source (UNION ALL).
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE s(a,b)`},
		`INSERT INTO t(a,b) SELECT a,b FROM s UNION ALL SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b`},
}

// TestInsertSelectUpsertCompilesToBytecode asserts statements compile.
func TestInsertSelectUpsertCompilesToBytecode(t *testing.T) {
	for i, tc := range insertSelectUpsertShapes {
		_, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
			continue
		}
	}
}

// TestInsertSelectUpsertProbesInsideTheScanLoop asserts OpUpsertFind sits
// inside the scan loop, between OpRewind and OpNext, and the insert operation
// is also in the loop.
func TestInsertSelectUpsertProbesInsideTheScanLoop(t *testing.T) {
	for i, tc := range insertSelectUpsertShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		opened, rewind, next, find, ins := -1, -1, -1, -1, -1
		cursor := -1
		for j := range prog.Insns {
			switch prog.Insns[j].Op {
			case OpOpenDerived:
				opened, cursor = j, prog.Insns[j].P1
			case OpRewind:
				rewind = j
			case OpNext:
				next = j
			case OpUpsertFind:
				find = j
			case OpInsert:
				ins = j
			}
		}
		if opened < 0 || rewind < 0 || next < 0 {
			t.Errorf("[%d] %q: open=%d rewind=%d next=%d -- the source is not being SCANNED",
				i, tc.stmt, opened, rewind, next)
			continue
		}
		if prog.Insns[rewind].P1 != cursor || prog.Insns[next].P1 != cursor {
			t.Errorf("[%d] %q: the loop drives cursor %d/%d, not the derived source's cursor %d",
				i, tc.stmt, prog.Insns[rewind].P1, prog.Insns[next].P1, cursor)
		}
		if find < 0 {
			t.Errorf("[%d] %q: no OpUpsertFind -- the ON CONFLICT clause was PARSED and then dropped,\n"+
				"which is the exact failure mode AGENTS.md invariant 1 names", i, tc.stmt)
			continue
		}
		if !(rewind < find && find < next) {
			t.Errorf("[%d] %q: the OpUpsertFind at %d is OUTSIDE the loop (rewind=%d next=%d) -- it would\n"+
				"probe once for the whole statement, which a one-row source cannot tell apart from correct",
				i, tc.stmt, find, rewind, next)
		}
		if ins < 0 || !(rewind < ins && ins < next) {
			t.Errorf("[%d] %q: the tail's no-conflict OpInsert is at %d, outside the loop (rewind=%d next=%d)",
				i, tc.stmt, ins, rewind, next)
		}
	}
}

// TestInsertSelectUpsertAnswers verifies correct answers for various upsert scenarios.
func TestInsertSelectUpsertAnswers(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		// Conflict with a row inserted earlier in this statement.
		{"conflict-with-a-row-this-statement-just-inserted",
			[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`,
				`INSERT INTO s VALUES(1,'first'),(1,'second'),(2,'other')`},
			`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=coalesce(c,0)+1`,
			`SELECT a,b,c FROM t ORDER BY a`, []string{"1,first,1", "2,other,<NULL>"}},
		{"changes-counts-every-source-row",
			[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`,
				`INSERT INTO s VALUES(1,'first'),(1,'second'),(2,'other')`},
			`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=coalesce(c,0)+1`,
			`SELECT changes()`, []string{"3"}},
		{"do-nothing", []string{`CREATE TABLE u(a UNIQUE,b)`, `CREATE TABLE us(a,b)`,
			`INSERT INTO u VALUES(1,'old')`, `INSERT INTO us VALUES(1,'new'),(2,'two')`},
			`INSERT INTO u(a,b) SELECT a,b FROM us WHERE true ON CONFLICT(a) DO NOTHING`,
			`SELECT a,b FROM u ORDER BY a`, []string{"1,old", "2,two"}},
		{"do-nothing-changes", []string{`CREATE TABLE u(a UNIQUE,b)`, `CREATE TABLE us(a,b)`,
			`INSERT INTO u VALUES(1,'old')`, `INSERT INTO us VALUES(1,'new'),(2,'two')`},
			`INSERT INTO u(a,b) SELECT a,b FROM us WHERE true ON CONFLICT(a) DO NOTHING`,
			`SELECT changes()`, []string{"1"}},
		// DO UPDATE WHERE filters per row.
		{"do-update-where-filters-per-row", []string{`CREATE TABLE v(a UNIQUE,b,c)`, `CREATE TABLE vs(a,b)`,
			`INSERT INTO v VALUES(1,'x',1),(2,'y',2)`, `INSERT INTO vs VALUES(1,'p'),(2,'q')`},
			`INSERT INTO v(a,b) SELECT a,b FROM vs WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b WHERE v.c=1`,
			`SELECT a,b,c FROM v ORDER BY a`, []string{"1,p,1", "2,y,2"}},
		// "excluded" is THIS source row, not the first one.
		{"excluded-is-the-current-source-row", []string{`CREATE TABLE w(a UNIQUE,b)`, `CREATE TABLE ws(a,b)`,
			`INSERT INTO w VALUES(1,'old'),(2,'old2')`, `INSERT INTO ws VALUES(1,'new'),(2,'new2')`},
			`INSERT INTO w(a,b) SELECT a,b FROM ws WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b||'!'`,
			`SELECT a,b FROM w ORDER BY a`, []string{"1,new!", "2,new2!"}},
		{"empty-source", []string{`CREATE TABLE z(a UNIQUE,b)`, `CREATE TABLE zs(a,b)`},
			`INSERT INTO z(a,b) SELECT a,b FROM zs WHERE true ON CONFLICT(a) DO UPDATE SET b=excluded.b`,
			`SELECT count(*) FROM z`, []string{"0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			_, cerr := db.compileWrite(tc.stmt)
			if cerr != nil {
				t.Fatalf("compile %q: %v", tc.stmt, cerr)
			}
			if err := db.Exec(tc.stmt); err != nil {
				t.Fatalf("exec %q: %v", tc.stmt, err)
			}
			got := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("%q then %q: got %v, want %v", tc.stmt, tc.query, got, tc.want)
			}
		})
	}
}

// TestInsertSelectUpsertSetSubqueryCompiles verifies INSERT ... SELECT with
// subquery in DO UPDATE SET compiles.
func TestInsertSelectUpsertSetSubqueryCompiles(t *testing.T) {
	tc := conflictShapeCase{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE s(a,b)`},
		`INSERT INTO t(a,b) SELECT a,b FROM s WHERE true ON CONFLICT(a) DO UPDATE SET c=(SELECT count(*) FROM s)`}
	if _, err := compileShape(t, tc); err != nil {
		t.Errorf("%q: %v", tc.stmt, err)
	}
}
