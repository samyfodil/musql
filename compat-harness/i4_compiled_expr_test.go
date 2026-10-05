// This file gates expressions evaluated with no row behind them (ATTACH
// paths, VACUUM INTO targets, trigger NEW/OLD reads) against C SQLite: both
// engines must accept with the same result, or both reject.
package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// i4Pair is one differential session over its own files. "{aux}" in a
// statement becomes that side's scratch path.
type i4Pair struct {
	t    *testing.T
	dir  string
	godb *engine.Session
	cgo  *sql.DB
}

func newI4Pair(t *testing.T) *i4Pair {
	t.Helper()
	dir := t.TempDir()
	// "ATTACH NOT '<path>'" attaches a file named "1" in the working
	// directory; run from the temp dir so no stray file lands in the source tree.
	t.Chdir(dir)
	godb, err := engine.Create(filepath.Join(dir, "go-main.db"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	t.Cleanup(func() { godb.Discard() })
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo-main.db"))
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	cgodb.SetMaxOpenConns(1) // ATTACH is connection-scoped
	t.Cleanup(func() { cgodb.Close() })
	return &i4Pair{t: t, dir: dir, godb: godb, cgo: cgodb}
}

// aux is this side's scratch path for the given tag.
func (p *i4Pair) aux(side, tag string) string {
	return filepath.Join(p.dir, side+"-"+tag+".db")
}

// exec runs one statement on both sides and returns each side's error.
func (p *i4Pair) exec(tmpl, tag string) (goErr, cgoErr error) {
	p.t.Helper()
	_, _, goErr = p.godb.ExecArgs(strings.ReplaceAll(tmpl, "{aux}", p.aux("go", tag)), nil)
	_, cgoErr = p.cgo.Exec(strings.ReplaceAll(tmpl, "{aux}", p.aux("cgo", tag)))
	return goErr, cgoErr
}

// agree requires both engines to accept, or both to reject.
func (p *i4Pair) agree(tmpl, tag string) (accepted bool) {
	p.t.Helper()
	goErr, cgoErr := p.exec(tmpl, tag)
	if (goErr == nil) != (cgoErr == nil) {
		p.t.Errorf("%s: one-sided outcome\n  go : %v\n  cgo: %v", tmpl, goErr, cgoErr)
	}
	return goErr == nil && cgoErr == nil
}

// TestI4AttachPathKeywordClass derives from the oracle which non-identifier
// keywords can open an ATTACH path expression (CASE, EXISTS, NOT) and requires
// the engine to agree keyword for keyword.
func TestI4AttachPathKeywordClass(t *testing.T) {
	// engine/sql_parser.go's nonIdentifierKeywords, verbatim.
	kws := strings.Fields(`ADD ALL ALTER AND AS AUTOINCREMENT BETWEEN CASE CHECK
COLLATE COMMIT CONSTRAINT CREATE DEFAULT DEFERRABLE DELETE DISTINCT DROP ELSE
ESCAPE EXCEPT EXISTS FOREIGN FROM GROUP HAVING IN INDEX INSERT INTERSECT INTO
IS ISNULL JOIN LIMIT NOT NOTHING NOTNULL NULL ON OR ORDER PRIMARY REFERENCES
RETURNING SELECT SET TABLE THEN TO TRANSACTION UNION UNIQUE UPDATE USING
VALUES WHEN WHERE`)
	// The expression shapes a leading keyword can open.
	forms := []string{"%[1]s '%[2]s'", "%[1]s WHEN 1 THEN '%[2]s' END", "%[1]s(SELECT '%[2]s')"}

	var oracleOpens, engineOpens []string
	for _, kw := range kws {
		p := newI4Pair(t)
		for _, form := range forms {
			path := p.aux("cgo", kw)
			stmt := "ATTACH " + fmt.Sprintf(form, kw, path) + " AS zz"
			if _, err := p.cgo.Exec(stmt); err == nil {
				oracleOpens = append(oracleOpens, kw)
				p.cgo.Exec("DETACH zz")
				break
			}
		}
		for _, form := range forms {
			path := p.aux("go", kw)
			stmt := "ATTACH " + fmt.Sprintf(form, kw, path) + " AS zz"
			if _, _, err := p.godb.ExecArgs(stmt, nil); err == nil {
				engineOpens = append(engineOpens, kw)
				p.godb.ExecArgs("DETACH zz", nil)
				break
			}
		}
	}
	sort.Strings(oracleOpens)
	sort.Strings(engineOpens)
	if strings.Join(oracleOpens, ",") != strings.Join(engineOpens, ",") {
		t.Errorf("ATTACH path keyword class diverges:\n  oracle opens: %v\n  engine opens: %v",
			oracleOpens, engineOpens)
	}
	// An empty answer on both sides would agree while proving nothing.
	if want := "CASE,EXISTS,NOT"; strings.Join(oracleOpens, ",") != want {
		t.Errorf("oracle's ATTACH path keyword class is %v, this file was written against %s -- "+
			"re-read engine/attach.go's attachExprOpeningKeywords before changing either", oracleOpens, want)
	}
}

// TestI4AttachPathKeywordValue checks that the keyword-opened paths attach the
// same database, not merely parse.
func TestI4AttachPathKeywordValue(t *testing.T) {
	p := newI4Pair(t)
	// CASE evaluates to the path itself, so a real file is attached.
	if !p.agree("ATTACH CASE WHEN 1 THEN '{aux}' END AS zz", "case") {
		t.Fatal("both engines must accept the CASE-opened ATTACH")
	}
	if !p.agree("CREATE TABLE zz.k(v)", "case") {
		t.Fatal("both engines must accept a write into the attached database")
	}
	for _, side := range []string{"go", "cgo"} {
		if _, err := os.Stat(p.aux(side, "case")); err != nil {
			t.Errorf("%s: CASE-opened ATTACH did not create %s: %v", side, p.aux(side, "case"), err)
		}
	}
	// NOT '<path>' evaluates to 1, so the file attached is named "1"; both
	// engines must agree.
	q := newI4Pair(t)
	if q.agree("ATTACH NOT '{aux}' AS zz", "not") {
		for _, side := range []string{"go", "cgo"} {
			if _, err := os.Stat(q.aux(side, "not")); err == nil {
				t.Errorf("%s: ATTACH NOT '<path>' created the PATH itself; the value is 1, not the path", side)
			}
		}
	}
}

// TestI4VacuumIntoTargetExpression gates VACUUM INTO's compiled target
// expression. The strict type check (no affinity coercion) makes the rejecting
// cases interesting.
func TestI4VacuumIntoTargetExpression(t *testing.T) {
	for _, tc := range []struct {
		name string
		stmt string
		// wantFile is the tag whose path the copy must land on, or "" when both
		// engines must reject.
		wantFile bool
	}{
		{"literal", "VACUUM INTO '{aux}'", true},
		{"concat", "VACUUM INTO '{aux}' || ''", true},
		{"function", "VACUUM INTO printf('%s', '{aux}')", true},
		{"cast", "VACUUM INTO CAST('{aux}' AS TEXT)", true},
		{"case", "VACUUM INTO CASE WHEN 1 THEN '{aux}' END", true},
		{"subquery", "VACUUM INTO (SELECT '{aux}')", true},
		// The target is read from a table, so the compiled seam must see the snapshot.
		{"subquery_from_table", "VACUUM INTO (SELECT p FROM paths)", true},
		// A number is not stringified and NULL is not a filename.
		{"integer", "VACUUM INTO 5", false},
		{"null", "VACUUM INTO NULL", false},
		{"blob", "VACUUM INTO x'6162'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newI4Pair(t)
			for _, s := range []string{"CREATE TABLE t(a)", "INSERT INTO t VALUES(1),(2),(3)", "CREATE TABLE paths(p)"} {
				if goErr, cgoErr := p.exec(s, tc.name); goErr != nil || cgoErr != nil {
					t.Fatalf("setup %q: go=%v cgo=%v", s, goErr, cgoErr)
				}
			}
			// This case needs a row naming this side's own target.
			for side, run := range map[string]func(string) error{
				"go":  func(s string) error { _, _, e := p.godb.ExecArgs(s, nil); return e },
				"cgo": func(s string) error { _, e := p.cgo.Exec(s); return e },
			} {
				if err := run(fmt.Sprintf("INSERT INTO paths VALUES(%q)", p.aux(side, tc.name))); err != nil {
					t.Fatalf("%s: seed paths: %v", side, err)
				}
			}
			accepted := p.agree(tc.stmt, tc.name)
			if accepted != tc.wantFile {
				t.Fatalf("%s: accepted=%v, want %v", tc.stmt, accepted, tc.wantFile)
			}
			if !accepted {
				return
			}
			// An accepted copy must be a real database holding the rows.
			for _, side := range []string{"go", "cgo"} {
				path := p.aux(side, tc.name)
				db, err := sql.Open("sqlite3", exportedForOracle(t, path))
				if err != nil {
					t.Fatalf("%s: open copy %s: %v", side, path, err)
				}
				var n int
				if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
					t.Errorf("%s: read copy %s: %v", side, path, err)
				} else if n != 3 {
					t.Errorf("%s: copy %s holds %d rows, want 3", side, path, n)
				}
				db.Close()
			}
		})
	}
}

// TestI4ZeroedNameContextPositions checks that VACUUM INTO targets and ATTACH
// paths reject aggregate and window calls, as C SQLite resolves them with a zeroed
// NameContext. Rejecting cases must create no file and must match the oracle's
// message ("misuse of aggregate" vs "misuse of window"); accepting cases keep a
// blanket rejection from passing.
func TestI4ZeroedNameContextPositions(t *testing.T) {
	for _, tc := range []struct {
		name string
		stmt string
		// accept is what both engines must do; when false, no file may exist at
		// either target afterwards.
		accept bool
	}{
		// --- VACUUM INTO: aggregate ---
		{"vac_agg_min", "VACUUM INTO min('{aux}')", false},
		{"vac_agg_group_concat", "VACUUM INTO group_concat('{aux}')", false},
		{"vac_agg_string_agg", "VACUUM INTO string_agg('{aux}','')", false},
		// FILTER with no OVER still reads "aggregate".
		{"vac_agg_filter", "VACUUM INTO min('{aux}') FILTER (WHERE 1)", false},
		// --- VACUUM INTO: window ---
		{"vac_win_over", "VACUUM INTO min('{aux}') OVER ()", false},
		{"vac_win_first_value", "VACUUM INTO first_value('{aux}') OVER ()", false},
		{"vac_win_nth_value", "VACUUM INTO nth_value('{aux}', 1) OVER ()", false},
		{"vac_win_group_concat", "VACUUM INTO group_concat('{aux}') OVER ()", false},
		// A window-only builtin with no OVER.
		{"vac_win_bare", "VACUUM INTO row_number()", false},
		// --- VACUUM INTO: nested, so the offending call is not the root ---
		{"vac_nested_concat", "VACUUM INTO 'p' || min('{aux}')", false},
		{"vac_nested_coalesce", "VACUUM INTO coalesce(min('{aux}'), 'z.db')", false},
		{"vac_nested_case", "VACUUM INTO CASE WHEN 1 THEN min('{aux}') END", false},
		{"vac_nested_cast", "VACUUM INTO CAST(min('{aux}') AS TEXT)", false},
		{"vac_nested_scalar", "VACUUM INTO ltrim(min('{aux}'))", false},
		{"vac_nested_collate", "VACUUM INTO min('{aux}') COLLATE NOCASE", false},
		// --- VACUUM INTO: FILTER/OVER on a non-aggregate ---
		{"vac_filter_nonagg", "VACUUM INTO ltrim('{aux}') FILTER (WHERE 1)", false},
		{"vac_over_nonagg", "VACUUM INTO ltrim('{aux}') OVER ()", false},
		// --- ATTACH: same verdicts ---
		{"att_agg_min", "ATTACH min('{aux}') AS zz", false},
		{"att_agg_max", "ATTACH max('{aux}') AS zz", false},
		{"att_win_over", "ATTACH min('{aux}') OVER () AS zz", false},
		{"att_win_bare", "ATTACH row_number() OVER () AS zz", false},
		{"att_nested_concat", "ATTACH ltrim(min('{aux}')) AS zz", false},
		{"att_filter_nonagg", "ATTACH ltrim('{aux}') FILTER (WHERE 1) AS zz", false},
		{"att_over_nonagg", "ATTACH ltrim('{aux}') OVER () AS zz", false},

		// --- Must stay legal ---
		// A subquery has its own NameContext, so an aggregate inside one is fine.
		{"vac_subquery_agg", "VACUUM INTO (SELECT min('{aux}'))", true},
		{"vac_subquery_agg_nested", "VACUUM INTO '' || (SELECT min('{aux}'))", true},
		{"att_subquery_agg", "ATTACH (SELECT min('{aux}')) AS zz", true},
		// Multi-argument min/max is the scalar form, not an aggregate.
		{"vac_scalar_min", "VACUUM INTO min('{aux}', 'zzzzzzzzzz')", true},
		{"att_scalar_min", "ATTACH min('{aux}', 'zzzzzzzzzz') AS zz", true},
		// Plain scalar calls, unchanged.
		{"vac_scalar_fn", "VACUUM INTO ltrim('{aux}')", true},
		{"att_scalar_fn", "ATTACH ltrim('{aux}') AS zz", true},
		{"vac_literal", "VACUUM INTO '{aux}'", true},
		{"att_literal", "ATTACH '{aux}' AS zz", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newI4Pair(t)
			for _, s := range []string{"CREATE TABLE t(a)", "INSERT INTO t VALUES(1),(2),(3)"} {
				if goErr, cgoErr := p.exec(s, tc.name); goErr != nil || cgoErr != nil {
					t.Fatalf("setup %q: go=%v cgo=%v", s, goErr, cgoErr)
				}
			}
			goErr, cgoErr := p.exec(tc.stmt, tc.name)
			if (goErr == nil) != (cgoErr == nil) {
				t.Fatalf("%s: one-sided outcome\n  go : %v\n  cgo: %v", tc.stmt, goErr, cgoErr)
			}
			if (cgoErr == nil) != tc.accept {
				t.Fatalf("%s: the 3.53.3 oracle now %v this statement (%v); this table was written "+
					"against the opposite answer -- re-read resolve.c before changing either",
					tc.stmt, map[bool]string{true: "ACCEPTS", false: "REJECTS"}[cgoErr == nil], cgoErr)
			}
			for _, side := range []string{"go", "cgo"} {
				_, statErr := os.Stat(p.aux(side, tc.name))
				if tc.accept && statErr != nil {
					t.Errorf("%s: %s: accepted but no file at %s: %v", side, tc.stmt, p.aux(side, tc.name), statErr)
				}
				if !tc.accept && statErr == nil {
					t.Errorf("%s: %s: REJECTED yet a file was written at %s", side, tc.stmt, p.aux(side, tc.name))
				}
			}
			if tc.accept {
				return
			}
			// The expected wording comes from the oracle each run. The engine's error
			// has an error-class prefix, so the oracle's text must be the suffix.
			if !strings.HasSuffix(goErr.Error(), cgoErr.Error()) {
				t.Errorf("%s: both reject, but with different reasons\n  oracle: %v\n  engine: %v",
					tc.stmt, cgoErr, goErr)
			}
		})
	}
}

// TestI4TriggerRowOuterColumnValue gates reading a trigger's NEW/OLD column
// where there is no cursor (a FROM-less body SELECT and its subqueries). It
// covers both ordinary columns and the rowid, which come from different slices.
func TestI4TriggerRowOuterColumnValue(t *testing.T) {
	p := newI4Pair(t)
	for _, s := range []string{
		`CREATE TABLE t(a, b)`,
		`CREATE TABLE log(tag, v)`,
		// A FROM-less body SELECT with a correlated subquery over NEW.
		`CREATE TRIGGER tr_ins AFTER INSERT ON t BEGIN
		   INSERT INTO log SELECT 'a', (SELECT new.a);
		   INSERT INTO log SELECT 'b', (SELECT new.b);
		   INSERT INTO log SELECT 'rowid', (SELECT new.rowid);
		 END`,
		`CREATE TRIGGER tr_upd AFTER UPDATE ON t BEGIN
		   INSERT INTO log SELECT 'old.a', (SELECT old.a);
		   INSERT INTO log SELECT 'new.a', (SELECT new.a);
		 END`,
		`INSERT INTO t VALUES(11, 22)`,
		`UPDATE t SET a = 99`,
	} {
		if goErr, cgoErr := p.exec(s, "trig"); goErr != nil || cgoErr != nil {
			t.Fatalf("%q: go=%v cgo=%v", s, goErr, cgoErr)
		}
	}
	const q = `SELECT tag, v FROM log ORDER BY tag, v`
	pager, err := p.godb.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pager.Close()
	_, goRows, err := pager.QueryArgs(q, nil)
	if err != nil {
		t.Fatalf("engine query: %v", err)
	}
	var got []string
	for _, r := range goRows {
		got = append(got, fmt.Sprintf("%s=%s", valText(r[0]), valText(r[1])))
	}
	cgoRows, err := p.cgo.Query(q)
	if err != nil {
		t.Fatalf("cgo query: %v", err)
	}
	defer cgoRows.Close()
	var want []string
	for cgoRows.Next() {
		var tag string
		var v any
		if err := cgoRows.Scan(&tag, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want = append(want, fmt.Sprintf("%s=%v", tag, v))
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("trigger NEW/OLD reads through the live row diverge:\n  go : %v\n  cgo: %v", got, want)
	}
	if len(want) != 5 {
		t.Fatalf("the oracle produced %d log rows, want 5 -- the triggers did not fire, so this gate proved nothing", len(want))
	}
}

// valText renders an engine.Value as the oracle's scan prints it.
func valText(v engine.Value) string {
	switch v.Typ {
	case engine.Null:
		return "<nil>"
	case engine.Int:
		return fmt.Sprintf("%d", v.I)
	case engine.Text:
		return string(v.S)
	}
	return fmt.Sprintf("%v", v)
}
