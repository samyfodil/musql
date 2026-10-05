package engine_test

import (
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
		// SCAN, NOT "SEARCH t USING INDEX i1 (b=?)", and the difference is the
		// FORMAT rather than the plan text being wrong. C SQLite searches; so did
		// this engine when these fixtures were SQLite files. A segment database
		// carries an index's SQL and not its DATA (segment_open.go), so there is
		// nothing to seek and the read visits every row and applies the WHERE --
		// same rows, and EXPLAIN QUERY PLAN says what actually happens rather than
		// what a different format would have done. Index data in the format is a
		// later, measurement-driven addition; when it lands, these two expectations
		// go back to SEARCH and this comment goes away.
		{`EXPLAIN QUERY PLAN SELECT * FROM t WHERE b=3`, []string{"SCAN t"}},
		{`EXPLAIN QUERY PLAN SELECT * FROM t, u WHERE t.b=u.x`, []string{"SCAN u", "SCAN t"}},
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
			// SCAN rather than SEARCH for the format's reason -- see
			// TestExplainQueryPlanText's own note on the same two shapes.
			"1|0|COMPOUND QUERY", "2|1|LEFT-MOST SUBQUERY", "3|2|SCAN t",
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
	// The merging families decline, cleanly and with their reason.
	for _, sql := range []string{
		`EXPLAIN QUERY PLAN SELECT a FROM t UNION SELECT x FROM u`,
		`EXPLAIN QUERY PLAN SELECT a FROM t INTERSECT SELECT x FROM u`,
		`EXPLAIN QUERY PLAN SELECT a FROM t EXCEPT SELECT x FROM u`,
		`EXPLAIN QUERY PLAN SELECT a FROM t UNION ALL SELECT x FROM u ORDER BY 1`,
	} {
		if _, _, err := p.Query(sql); err == nil {
			t.Errorf("%s answered; C SQLite reports a MERGE tree this engine does not derive", sql)
		} else if !strings.Contains(err.Error(), "MERGE") {
			t.Errorf("%s: %v -- the decline should name what it cannot reproduce", sql, err)
		}
	}
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
