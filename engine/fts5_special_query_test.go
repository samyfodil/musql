package engine

import (
	"strings"
	"testing"
)

// TestFts5SpecialDirective is fts5SpecialMatch's own scan (fts5_main.c:1164-1166,
// "while( z[0]==' ' ) z++; for(n=0; z[n] && z[n]!=' '; n++);"), with the cases
// probed against the 3.53.3 oracle in fts5_special_query.go's file comment.
func TestFts5SpecialDirective(t *testing.T) {
	for _, tc := range []struct {
		pat  string
		dir  string
		want bool
	}{
		{"*reads", "reads", true},
		{"*READS", "READS", true},
		{"* reads", "reads", true},
		{"*reads extra", "reads", true},
		{"*id", "id", true},
		{"*", "", true},
		{"*bogus", "bogus", true},
		// The "*" must be the pattern's FIRST byte: the oracle parses
		// " *reads" as an ordinary query and fails with a syntax error.
		{" *reads", "", false},
		{"reads", "", false},
		{"", "", false},
	} {
		dir, ok := fts5SpecialDirective(tc.pat)
		if ok != tc.want || dir != tc.dir {
			t.Errorf("fts5SpecialDirective(%q) = %q,%v; want %q,%v", tc.pat, dir, ok, tc.dir, tc.want)
		}
	}
}

// TestFts5SpecialQueryDeclinesAsNotReproducible gates the DECLINE, not an
// answer: both special queries report the answering build's own instrumentation
// (%_data block count / cursor counter), so this engine must refuse them by
// name rather than invent a number -- and must leave every other MATCH alone.
func TestFts5SpecialQueryDeclinesAsNotReproducible(t *testing.T) {
	p := i1PagerWith(t,
		`CREATE VIRTUAL TABLE x1 USING fts5(x, y)`,
		`INSERT INTO x1(rowid, x, y) VALUES(1, 'alpha beta', 'gamma')`,
	)
	// Every spelling that reaches fts5FilterMethod's "zText[0]=='*'" test: the
	// table-named column, an ordinary column (the test precedes any column
	// handling), the table-valued call form, and the "=" form.
	for _, q := range []string{
		`SELECT rowid, x1 FROM x1 WHERE x1 MATCH '*reads'`,
		`SELECT rowid, x, x1 FROM x1 WHERE x1 MATCH '*reads'`,
		`SELECT rowid, x1 FROM x1 WHERE x MATCH '*READS'`,
		`SELECT rowid, x1 FROM x1('*reads')`,
		`SELECT rowid, x1 FROM x1 WHERE x1 = '*reads'`,
		`SELECT rowid, x1 FROM x1 WHERE rowid > 0 AND x1 MATCH '* reads'`,
		`SELECT rowid, x1 FROM x1 WHERE x1 MATCH '*id'`,
	} {
		_, _, err := p.Query(q)
		if err == nil {
			t.Errorf("%s: answered; want a decline", q)
			continue
		}
		if !strings.Contains(err.Error(), "not reproducible against C SQLite") {
			t.Errorf("%s: %v\n  want the non-reproducible decline", q, err)
		}
	}
	// An UNRECOGNISED directive is left alone: C SQLite errors on it too
	// ("unknown special query: bogus"), so both engines already agree by
	// rejecting it and this guard must not claim it.
	for _, q := range []string{
		`SELECT rowid, x1 FROM x1 WHERE x1 MATCH '*bogus'`,
		`SELECT rowid FROM x1 WHERE x1 MATCH ' *reads'`,
		`SELECT rowid FROM x1 WHERE x1 NOT MATCH '*reads'`,
		// "rank" is the other hidden column, and a MATCH against it is not the
		// special dispatch: the oracle answers rows for it.
		`SELECT rowid FROM x1 WHERE rank MATCH '*reads'`,
	} {
		if _, _, err := p.Query(q); err != nil &&
			strings.Contains(err.Error(), "not reproducible against C SQLite") {
			t.Errorf("%s: claimed by the special-query guard; want its own error", q)
		}
	}
	// An fts3 table has no special queries at all, so its own "*"-leading
	// pattern must be left to decline (or answer) for its own reason, even
	// when an fts5 table shares the statement's FROM.
	p3 := i1PagerWith(t,
		`CREATE VIRTUAL TABLE ft3 USING fts4(c)`,
		`CREATE VIRTUAL TABLE x2 USING fts5(x)`,
	)
	for _, q := range []string{
		`SELECT rowid FROM ft3 WHERE ft3 MATCH '*reads'`,
		`SELECT ft3.rowid FROM ft3, x2 WHERE ft3 MATCH '*reads'`,
	} {
		if _, _, err := p3.Query(q); err != nil &&
			strings.Contains(err.Error(), "not reproducible against C SQLite") {
			t.Errorf("%s: claimed by the fts5 special-query guard", q)
		}
	}
	// ...and an ordinary MATCH still answers.
	_, rows, err := p.Query(`SELECT rowid FROM x1 WHERE x1 MATCH 'alpha'`)
	if err != nil || len(rows) != 1 {
		t.Errorf("ordinary MATCH: rows=%d err=%v; want 1 row", len(rows), err)
	}
}
