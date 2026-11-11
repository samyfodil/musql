package engine_test

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// explainDB creates a test database for EXPLAIN tests.
func explainDB(t *testing.T) *engine.ReadOnlyPager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "explain.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)`,
		`CREATE INDEX i1 ON t(b)`,
		`CREATE TABLE u(x, y)`,
		`INSERT INTO t VALUES(1,2,3),(2,3,4)`,
		`INSERT INTO u VALUES(2,9)`,
	} {
		mustExec(t, db, s)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// TestExplainQueryPlanText pins the plan text against C SQLite 3.53.3's own,
// captured from the oracle for each of these statements against this exact
// schema. The two the oracle words differently are deliberately absent: it
// SEARCHes "a>1" through the rowid and reads "count(*)" out of the covering
// index, neither of which this engine's program does -- and the plan describes
// the program that RUNS, so writing C's text there would be a lie about musql.
func TestExplainQueryPlanText(t *testing.T) {
	p := explainDB(t)
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`EXPLAIN QUERY PLAN SELECT a FROM t WHERE a=1`, []string{"SEARCH t USING INTEGER PRIMARY KEY (rowid=?)"}},
		{`EXPLAIN QUERY PLAN SELECT * FROM t`, []string{"SCAN t"}},
		{`EXPLAIN QUERY PLAN SELECT * FROM t WHERE b=3`, []string{"SEARCH t USING INDEX i1 (b=?)"}},
		{`EXPLAIN QUERY PLAN SELECT * FROM t, u WHERE t.b=u.x`, []string{"SCAN u", "SEARCH t USING INDEX i1 (b=?)"}},
		{`EXPLAIN QUERY PLAN SELECT 1`, []string{"SCAN CONSTANT ROW"}},
		{`EXPLAIN QUERY PLAN VALUES(1)`, []string{"SCAN CONSTANT ROW"}},
		{`EXPLAIN QUERY PLAN VALUES(1),(2)`, []string{"SCAN 2-ROW VALUES CLAUSE"}},
	} {
		cols, rows, err := p.Query(tc.sql)
		if err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		if got := strings.Join(cols, ","); got != "id,parent,notused,detail" {
			t.Errorf("%s: columns = %s", tc.sql, got)
		}
		if len(rows) != len(tc.want) {
			t.Errorf("%s: %d rows, want %d", tc.sql, len(rows), len(tc.want))
			continue
		}
		for i, w := range tc.want {
			if got := string(rows[i][3].S); got != w {
				t.Errorf("%s: row %d detail = %q, want %q", tc.sql, i, got, w)
			}
		}
	}
}

// TestExplainListing checks the listing's shape: C SQLite's eight columns,
// one row per instruction, an opcode name in column 1, and NULL (not "") in
// the two columns it leaves empty.
func TestExplainListing(t *testing.T) {
	p := explainDB(t)
	cols, rows, err := p.Query(`EXPLAIN SELECT a FROM t WHERE a=1`)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	if got := strings.Join(cols, ","); got != "addr,opcode,p1,p2,p3,p4,p5,comment" {
		t.Fatalf("columns = %s", got)
	}
	if len(rows) == 0 {
		t.Fatal("no instructions listed")
	}
	sawNull := false
	for i, r := range rows {
		if len(r) != 8 {
			t.Fatalf("row %d has %d columns", i, len(r))
		}
		if r[0].Typ != engine.Int || r[0].I != int64(i) {
			t.Errorf("row %d addr = %v", i, r[0])
		}
		if r[1].Typ != engine.Text || len(r[1].S) == 0 {
			t.Errorf("row %d opcode = %v", i, r[1])
		}
		if r[5].Typ == engine.Null {
			sawNull = true
		}
	}
	if !sawNull {
		t.Error("no instruction left p4 NULL; C SQLite leaves it NULL whenever the opcode has no p4")
	}
	if got := string(rows[len(rows)-1][1].S); got != "Halt" {
		t.Errorf("last opcode = %s, want Halt", got)
	}
}

// TestExplainOfAWriteRunsNothing: EXPLAIN describes a write's program without
// running it. C SQLite prints no plan row for an INSERT (it scans nothing)
// and one for the UPDATE/DELETE scan that its WHERE drives -- both verified
// against the 3.53.3 oracle on this schema.
func TestExplainOfAWriteRunsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)`)
	mustExec(t, db, `INSERT INTO t VALUES(1,2,3),(2,3,4)`)

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`EXPLAIN QUERY PLAN INSERT INTO t VALUES(9,9,9)`, nil},
		// "a" IS the INTEGER PRIMARY KEY, so this is a rowid SEARCH, and that
		// is what C SQLite reports for it. This line used to expect "SCAN t"
		// -- recording a DIVERGENCE, not a behaviour: the write path had no
		// rowid point lookup, so it really did scan, and EXPLAIN said so
		// honestly. Verified against 3.53.3 after the seek landed: both engines
		// now say SEARCH for this and for the rowid spelling, and both still say
		// SCAN for "WHERE b=2", which pins nothing.
		{`EXPLAIN QUERY PLAN UPDATE t SET b=1 WHERE a=1`,
			[]string{"SEARCH t USING INTEGER PRIMARY KEY (rowid=?)"}},
		{`EXPLAIN QUERY PLAN UPDATE t SET b=1 WHERE rowid=1`,
			[]string{"SEARCH t USING INTEGER PRIMARY KEY (rowid=?)"}},
		{`EXPLAIN QUERY PLAN DELETE FROM t WHERE a=1`,
			[]string{"SEARCH t USING INTEGER PRIMARY KEY (rowid=?)"}},
		{`EXPLAIN QUERY PLAN DELETE FROM t WHERE b=2`, []string{"SCAN t"}},
	} {
		_, rows, err := p.Query(tc.sql)
		if err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		if len(rows) != len(tc.want) {
			t.Errorf("%s: %d rows, want %d", tc.sql, len(rows), len(tc.want))
			continue
		}
		for i, w := range tc.want {
			if got := string(rows[i][3].S); got != w {
				t.Errorf("%s: row %d = %q, want %q", tc.sql, i, got, w)
			}
		}
	}
	if _, rows, err := p.Query(`EXPLAIN INSERT INTO t VALUES(9,9,9)`); err != nil {
		t.Fatalf("EXPLAIN INSERT: %v", err)
	} else if len(rows) == 0 {
		t.Fatal("EXPLAIN INSERT listed no instructions")
	}

	// Nothing above may have written: EXPLAIN compiles, it does not run.
	p2, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := p2.Query(`SELECT count(*) FROM t`)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) != 1 || rows[0][0].I != 2 {
		t.Errorf("after EXPLAIN of INSERT/UPDATE/DELETE, count(*) = %v, want 2", rows)
	}
}

// TestExplainIsNotAPrefixOfATableNamedExplain: "EXPLAIN" is an ordinary
// identifier to the lexer, so a table may be named it, and the keyword only
// introduces an EXPLAIN when a statement follows -- a bare "EXPLAIN" is a
// syntax error in C SQLite, not a query.
func TestExplainIsNotAPrefixOfATableNamedExplain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kw.musq")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mustExec(t, db, `CREATE TABLE explain(a)`)
	mustExec(t, db, `INSERT INTO explain VALUES(7)`)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p, err := engine.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	_, rows, err := p.Query(`SELECT a FROM explain`)
	if err != nil {
		t.Fatalf("SELECT FROM explain: %v", err)
	}
	if len(rows) != 1 || rows[0][0].I != 7 {
		t.Errorf("rows = %v, want one row holding 7", rows)
	}
	if _, _, err := p.Query(`EXPLAIN`); err == nil {
		t.Error("bare EXPLAIN answered; C SQLite reports a syntax error")
	}
}

// TestExplainBehindAComment: the keyword can sit behind a leading comment, a
// shape the corpus uses verbatim (where2.test has "-- random() is not
// optimized out\nEXPLAIN SELECT ..."), so splitExplain's cheap prefix check
// must look past one.
func TestExplainBehindAComment(t *testing.T) {
	p := explainDB(t)
	for _, sql := range []string{
		"-- a comment\nEXPLAIN QUERY PLAN SELECT * FROM t",
		"/* a comment */ EXPLAIN QUERY PLAN SELECT * FROM t",
		"  \n\t/* one */ -- two\n EXPLAIN QUERY PLAN SELECT * FROM t",
	} {
		cols, rows, err := p.Query(sql)
		if err != nil {
			t.Errorf("%q: %v", sql, err)
			continue
		}
		if len(cols) != 4 || len(rows) != 1 || string(rows[0][3].S) != "SCAN t" {
			t.Errorf("%q: cols=%v rows=%v, want one SCAN t row", sql, cols, rows)
		}
	}
	// ...and a table whose name merely STARTS with the keyword is not one.
	if _, _, err := p.Query(`SELECT * FROM explainer`); err == nil {
		t.Error(`SELECT * FROM explainer answered; there is no such table`)
	} else if !strings.Contains(err.Error(), "explainer") {
		t.Errorf("error does not name the missing table: %v", err)
	}
}

// TestExplainQueryPlanCompound: a compound's plan is a TREE, and the flat
// family -- every connective UNION ALL, no statement-level ORDER BY -- is the
// one C SQLite prints without merging. Details and parent structure are
// 3.53.3's own, for two arms and for three; only the ids differ, because C's
// are program addresses and these are positions.
//
// Everything that MERGES stays declined rather than approximated: see
// explainCompoundPlan for the measured shapes.
func TestExplainQueryPlanCompound(t *testing.T) {
	p := explainDB(t)
	for _, tc := range []struct {
		sql  string
		want []string // "id|parent|detail"
	}{
		{`EXPLAIN QUERY PLAN SELECT a FROM t UNION ALL SELECT x FROM u`, []string{
			"1|0|COMPOUND QUERY", "2|1|LEFT-MOST SUBQUERY", "3|2|SCAN t",
			"4|1|UNION ALL", "5|4|SCAN u",
		}},
		{`EXPLAIN QUERY PLAN SELECT a FROM t UNION ALL SELECT x FROM u UNION ALL SELECT a FROM t`, []string{
			"1|0|COMPOUND QUERY", "2|1|LEFT-MOST SUBQUERY", "3|2|SCAN t",
			"4|1|UNION ALL", "5|4|SCAN u", "6|1|UNION ALL", "7|6|SCAN t",
		}},
		{`EXPLAIN QUERY PLAN SELECT a FROM t WHERE b=3 UNION ALL SELECT 1`, []string{
			"1|0|COMPOUND QUERY", "2|1|LEFT-MOST SUBQUERY", "3|2|SEARCH t USING COVERING INDEX i1 (b=?)",
			"4|1|UNION ALL", "5|4|SCAN CONSTANT ROW",
		}},
	} {
		_, rows, err := p.Query(tc.sql)
		if err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = strconv.FormatInt(r[0].I, 10) + "|" + strconv.FormatInt(r[1].I, 10) + "|" + string(r[3].S)
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s:\n got %v\nwant %v", tc.sql, got, tc.want)
		}
	}
	// The merging families print the tree this engine runs: each arm under
	// its operator, and one sort for an ORDER BY. C prints a MERGE tree there,
	// which describes a program this engine does not run.
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`EXPLAIN QUERY PLAN SELECT a FROM t UNION SELECT x FROM u`, []string{
			"1|0|COMPOUND QUERY", "2|1|LEFT-MOST SUBQUERY", "3|2|SCAN t", "4|1|UNION", "5|4|SCAN u",
		}},
		{`EXPLAIN QUERY PLAN SELECT a FROM t INTERSECT SELECT x FROM u EXCEPT SELECT 1`, []string{
			"1|0|COMPOUND QUERY", "2|1|LEFT-MOST SUBQUERY", "3|2|SCAN t", "4|1|INTERSECT", "5|4|SCAN u",
			"6|1|EXCEPT", "7|6|SCAN CONSTANT ROW",
		}},
		{`EXPLAIN QUERY PLAN SELECT a FROM t UNION ALL SELECT x FROM u ORDER BY 1`, []string{
			"1|0|COMPOUND QUERY", "2|1|LEFT-MOST SUBQUERY", "3|2|SCAN t", "4|1|UNION ALL", "5|4|SCAN u",
			"6|0|USE TEMP B-TREE FOR ORDER BY",
		}},
	} {
		if got := explainPlanRows(t, p, tc.sql); strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s:\n got %v\nwant %v", tc.sql, got, tc.want)
		}
	}
}

// TestExplainQueryPlanSubqueries: the plan of every program a statement runs
// hangs under the line that names it (#123): a FROM subquery's under its
// MATERIALIZE line, before its scan, and an expression's under a SUBQUERY line.
func TestExplainQueryPlanSubqueries(t *testing.T) {
	p := explainDB(t)
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`EXPLAIN QUERY PLAN SELECT sum(a) FROM (SELECT a FROM t UNION SELECT x FROM u)`, []string{
			"1|0|MATERIALIZE (subquery-1)", "2|1|COMPOUND QUERY", "3|2|LEFT-MOST SUBQUERY", "4|3|SCAN t",
			"5|2|UNION", "6|5|SCAN u", "7|0|SCAN (subquery-1)",
		}},
		{`EXPLAIN QUERY PLAN SELECT sum(a) FROM (SELECT DISTINCT a FROM (SELECT a FROM t UNION ALL SELECT x FROM u))`, []string{
			"1|0|MATERIALIZE (subquery-1)", "2|1|MATERIALIZE (subquery-2)", "3|2|COMPOUND QUERY",
			"4|3|LEFT-MOST SUBQUERY", "5|4|SCAN t", "6|3|UNION ALL", "7|6|SCAN u",
			"8|1|SCAN (subquery-2)", "9|0|SCAN (subquery-1)",
		}},
		{`EXPLAIN QUERY PLAN SELECT a FROM t WHERE b IN (SELECT y FROM u)`, []string{
			"1|0|SCAN t", "2|0|LIST SUBQUERY 1", "3|2|SCAN u",
		}},
		{`EXPLAIN QUERY PLAN SELECT (SELECT max(x) FROM u WHERE u.y > t.a) FROM t`, []string{
			"1|0|SCAN t", "2|0|CORRELATED SCALAR SUBQUERY 1", "3|2|SCAN u",
		}},
		{`EXPLAIN QUERY PLAN SELECT * FROM t, (SELECT x, count(*) FROM u GROUP BY x) g WHERE g.x = t.b`, []string{
			"1|0|MATERIALIZE (subquery-1)", "2|1|SCAN u", "3|0|SCAN (subquery-1)", "4|0|SEARCH t USING INDEX i1 (b=?)",
		}},
	} {
		if got := explainPlanRows(t, p, tc.sql); strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s:\n got %v\nwant %v", tc.sql, got, tc.want)
		}
	}
}

func explainPlanRows(t *testing.T, p *engine.ReadOnlyPager, sql string) []string {
	t.Helper()
	_, rows, err := p.Query(sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = strconv.FormatInt(r[0].I, 10) + "|" + strconv.FormatInt(r[1].I, 10) + "|" + string(r[3].S)
	}
	return got
}

// TestExplainThroughEngineExec: EXPLAIN runs nothing, so it belongs on the
// query side whichever entry point the caller used -- sqlite3_exec("EXPLAIN
// ...") steps the description and discards its rows. The driver routes one
// that way already; this pins the ENGINE-DIRECT path, which is a separate
// route and the one the mined SQLite corpus drives (it never goes through
// driver), so without it every EXPLAIN in the corpus still errors.
func TestExplainThroughEngineExec(t *testing.T) {
	db, err := engine.Create(filepath.Join(t.TempDir(), "e.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE t(a,b)`)
	mustExec(t, db, `INSERT INTO t VALUES(1,'x'),(2,'y')`)

	for _, sql := range []string{
		`EXPLAIN SELECT a FROM t`,
		`EXPLAIN QUERY PLAN SELECT a FROM t`,
		`EXPLAIN INSERT INTO t VALUES(9,'z')`,
		`EXPLAIN QUERY PLAN DELETE FROM t WHERE a=1`,
	} {
		if err := db.Exec(sql); err != nil {
			t.Errorf("Exec(%s): %v", sql, err)
		}
	}
	// Nothing ran: the rows are untouched, and changes() still reports the
	// INSERT's 2 -- an EXPLAIN is not DML, so it publishes nothing (verified
	// against the 3.53.3 oracle, which likewise leaves changes() at 2).
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, err := p.Query(`SELECT count(*), changes() FROM t`)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) != 1 || rows[0][0].I != 2 || rows[0][1].I != 2 {
		t.Errorf("after four EXPLAINs: count/changes = %v, want 2/2", rows)
	}
}

// TestExplainListingCompound: a compound lists each arm's program in turn
// (#131), addressed as one listing, every jump landing inside its own arm, and
// each later arm's first line naming the operator that brings it in.
func TestExplainListingCompound(t *testing.T) {
	p := explainDB(t)
	for _, tc := range []struct {
		sql string
		ops []string
	}{
		{`EXPLAIN SELECT 1 a UNION ALL SELECT 2`, []string{"", "UNION ALL"}},
		{`EXPLAIN SELECT a FROM t WHERE b > 2 UNION SELECT x FROM u WHERE y = 9`, []string{"", "UNION"}},
		{`EXPLAIN SELECT b FROM t INTERSECT SELECT x FROM u EXCEPT SELECT 7 ORDER BY 1 LIMIT 3`, []string{"", "INTERSECT", "EXCEPT"}},
		{`EXPLAIN VALUES (1), (2), (3)`, []string{"", "UNION ALL", "UNION ALL"}},
	} {
		_, rows, err := p.Query(tc.sql)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		var starts []int
		for i, r := range rows {
			if r[0].I != int64(i) {
				t.Fatalf("%s: row %d addr = %d", tc.sql, i, r[0].I)
			}
			if string(r[1].S) == "Init" {
				starts = append(starts, i)
			}
		}
		if len(starts) != len(tc.ops) || starts[0] != 0 {
			t.Fatalf("%s: arms start at %v, want %d arms", tc.sql, starts, len(tc.ops))
		}
		for k, at := range starts {
			end := len(rows)
			if k+1 < len(starts) {
				end = starts[k+1]
			}
			comment := string(rows[at][7].S)
			if tc.ops[k] != "" && !strings.HasPrefix(comment, tc.ops[k]+": start at ") {
				t.Errorf("%s: arm %d begins %q", tc.sql, k, comment)
			}
			if got := string(rows[end-1][1].S); got != "Halt" {
				t.Errorf("%s: arm %d ends with %s", tc.sql, k, got)
			}
			for i := at; i < end; i++ {
				switch string(rows[i][1].S) {
				case "Init", "Goto", "Rewind", "Next", "If", "IfNot", "IsNull", "NotNull", "Gosub":
					if p2 := int(rows[i][3].I); p2 < at || p2 >= end {
						t.Errorf("%s: %d %s jumps to %d, outside its arm [%d,%d)", tc.sql, i, rows[i][1].S, p2, at, end)
					}
				}
			}
		}
	}
}

// TestExplainListingSubqueries: the programs a statement runs follow its own
// in the listing (#130), each begun by a line naming the instruction that runs
// it, and every jump lands inside its own program.
func TestExplainListingSubqueries(t *testing.T) {
	p := explainDB(t)
	for _, tc := range []struct {
		sql  string
		subs []string // what each later program's first line is brought in by
	}{
		{`EXPLAIN SELECT sum(a) FROM (SELECT DISTINCT a FROM (SELECT a FROM t UNION ALL SELECT x FROM u))`,
			[]string{"OpenDerived", "OpenDerived", "OpenDerived, UNION ALL"}},
		{`EXPLAIN SELECT a FROM t WHERE b IN (SELECT y FROM u)`, []string{"InSub"}},
		{`EXPLAIN SELECT (SELECT max(x) FROM u WHERE u.y > t.a) FROM t`, []string{"Subquery"}},
	} {
		_, rows, err := p.Query(tc.sql)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		var starts []int
		for i, r := range rows {
			if r[0].I != int64(i) {
				t.Fatalf("%s: row %d addr = %d", tc.sql, i, r[0].I)
			}
			if string(r[1].S) == "Init" {
				starts = append(starts, i)
			}
		}
		if len(starts) != 1+len(tc.subs) {
			t.Fatalf("%s: programs start at %v, want %d", tc.sql, starts, 1+len(tc.subs))
		}
		for k, at := range starts {
			end := len(rows)
			if k+1 < len(starts) {
				end = starts[k+1]
			}
			if k > 0 {
				comment := string(rows[at][7].S)
				var by int
				var rest string
				if n, _ := fmt.Sscanf(comment, "subquery of %d", &by); n != 1 || by >= at {
					t.Errorf("%s: program %d begins %q", tc.sql, k, comment)
					continue
				}
				want := strings.SplitN(tc.subs[k-1], ", ", 2)
				if got := string(rows[by][1].S); got != want[0] {
					t.Errorf("%s: program %d is brought in by %d %s, want %s", tc.sql, k, by, got, want[0])
				}
				if len(want) == 2 {
					rest = ", " + want[1] + ": "
				} else {
					rest = ": "
				}
				if !strings.HasPrefix(comment, fmt.Sprintf("subquery of %d%s", by, rest)) {
					t.Errorf("%s: program %d begins %q", tc.sql, k, comment)
				}
			}
			for i := at; i < end; i++ {
				switch string(rows[i][1].S) {
				case "Init", "Goto", "Rewind", "Next", "If", "IfNot", "IsNull", "NotNull", "Gosub", "Distinct":
					if p2 := int(rows[i][3].I); p2 < at || p2 >= end {
						t.Errorf("%s: %d %s jumps to %d, outside its program [%d,%d)", tc.sql, i, rows[i][1].S, p2, at, end)
					}
				}
			}
		}
	}
}
