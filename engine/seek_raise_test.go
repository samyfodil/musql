package engine

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExprMayRaise(t *testing.T) {
	for _, tc := range []struct {
		where string
		want  bool
	}{
		{`a = 1`, false},
		{`a < b + 1 AND b IS NOT NULL`, false},
		{`a || b = 'x'`, false},
		{`lower(a) = 'x' OR coalesce(b, 1) > 0`, false},
		{`a IN (1, 2) AND a BETWEEN 0 AND 9`, false},
		{`CASE WHEN a THEN 1 ELSE CAST(b AS INTEGER) END = 1`, false},
		{`a LIKE 'x%'`, false},
		{`a LIKE 'x%' ESCAPE '\'`, true},
		{`a LIKE b`, true},
		{`json_extract(a, '$') > 0`, true},
		{`a ->> '$.x' = 1`, true},
		{`abs(a) > 0`, true},
		{`a REGEXP 'x'`, true},
		{`a IN (SELECT 1)`, true},
		{`(SELECT 1) = a`, true},
		{`EXISTS (SELECT 1)`, true},
	} {
		stmt, err := ParseSelect(`SELECT 1 FROM t WHERE ` + tc.where)
		if err != nil {
			t.Fatalf("%s: %v", tc.where, err)
		}
		if got := exprMayRaise(stmt.Where); got != tc.want {
			t.Errorf("exprMayRaise(%s) = %v, want %v", tc.where, got, tc.want)
		}
	}
}

const seekRaiseFail = "malformed JSON"

// seekRaiseAnswer is what q answers on n: each row's integers, or the failure.
func seekRaiseAnswer(n *Session, q string) string {
	_, rows, err := n.Query(q, nil)
	if err != nil {
		if strings.Contains(err.Error(), seekRaiseFail) {
			return seekRaiseFail
		}
		return err.Error()
	}
	var out []string
	for _, r := range rows {
		var vs []string
		for _, v := range r {
			vs = append(vs, fmt.Sprint(v.I))
		}
		out = append(out, strings.Join(vs, ","))
	}
	return strings.Join(out, " ")
}

// seekRaiseWrite is what the write q answers on n: the rows it changed, or the
// failure.
func seekRaiseWrite(n *Session, q string) string {
	changed, _, err := n.ExecArgs(q, nil)
	switch {
	case err != nil && strings.Contains(err.Error(), seekRaiseFail):
		return seekRaiseFail
	case err != nil:
		return err.Error()
	}
	return fmt.Sprint(changed)
}

// seekRaiseSession runs stmts on a new database and returns a session over it.
// At stage "session" they are still in the session, at "log" each was committed
// to the change log, and at "segments" the file was then rewritten into
// segments.
func seekRaiseSession(t *testing.T, stage string, stmts []string) *Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.musq")
	n, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if stage == "segments" {
		stmts = append(slices.Clone(stmts), `VACUUM`)
	}
	for _, s := range stmts {
		if err := n.Exec(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if stage != "session" {
			if _, err := n.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if stage == "session" {
		return n
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSeekRaisingTerm: a seek over a WHERE holding a term that can raise
// answers what C SQLite does (each outcome below is C's, verified against the
// oracle by compat-harness's TestSeekRaisingTermVsC). The inner table's second
// row holds malformed JSON under a key no outer row wants: C reads it, and
// fails, exactly when its loop reaches the JSON term on that row -- with no
// index on the key it scans, and with one it never visits the row.
func TestSeekRaisingTerm(t *testing.T) {
	const fail = seekRaiseFail
	type outcome struct{ noIndex, index string }
	same := func(s string) outcome { return outcome{s, s} }
	reads := func(k1 string) []struct {
		q    string
		want outcome
	} {
		raise, key := `json_extract(i.j, '$') < o.lim`, `i.k = o.want`
		both := [2]string{raise + " AND " + key, key + " AND " + raise}
		return []struct {
			q    string
			want outcome
		}{
			{`SELECT o.id FROM o WHERE EXISTS (SELECT 1 FROM i WHERE ` + both[0] + `) ORDER BY o.id`, same("1")},
			{`SELECT o.id FROM o WHERE EXISTS (SELECT 1 FROM i WHERE ` + both[1] + `) ORDER BY o.id`, same("1")},
			{`SELECT o.id FROM o WHERE NOT EXISTS (SELECT 1 FROM i WHERE ` + both[0] + `) ORDER BY o.id`, outcome{fail, "2"}},
			{`SELECT o.id FROM o WHERE NOT EXISTS (SELECT 1 FROM i WHERE ` + both[1] + `) ORDER BY o.id`, same("2")},
			{`SELECT o.id, (SELECT count(*) FROM i WHERE ` + both[0] + `) FROM o ORDER BY o.id`, outcome{fail, "1,1 2,0"}},
			{`SELECT o.id, (SELECT count(*) FROM i WHERE ` + both[1] + `) FROM o ORDER BY o.id`, same("1,1 2,0")},
			{`SELECT o.id FROM o WHERE o.want IN (SELECT i.k FROM i WHERE ` + both[0] + `) ORDER BY o.id`, outcome{fail, "1"}},
			{`SELECT o.id FROM o WHERE o.want IN (SELECT i.k FROM i WHERE ` + both[1] + `) ORDER BY o.id`, same("1")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND k = ` + k1, outcome{fail, "1"}},
			{`SELECT rowid FROM i WHERE k = ` + k1 + ` AND json_extract(j, '$') > 0`, same("1")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid = 'x'`, same("")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid = 1.5`, same("")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid = NULL`, same("")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid = '1'`, same("1")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid < 2`, same("1")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid <= 1.5`, same("1")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid > 'abc'`, same("")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid >= 2.5`, same("")},
			{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND rowid < 'abc'`, same(fail)},
		}
	}
	writes := func(k1 string) []struct {
		q    string
		want outcome
	} {
		return []struct {
			q    string
			want outcome
		}{
			{`UPDATE i SET j = j || '' WHERE json_extract(j, '$') > 0 AND k = ` + k1, outcome{fail, "1"}},
			{`UPDATE i SET j = j || '' WHERE k = ` + k1 + ` AND json_extract(j, '$') > 0`, same("1")},
			{`DELETE FROM i WHERE json_extract(j, '$') > 0 AND k = ` + k1, outcome{fail, "1"}},
			{`DELETE FROM i WHERE k = ` + k1 + ` AND json_extract(j, '$') > 0`, same("1")},
			{`DELETE FROM i WHERE json_extract(j, '$') > 0 AND rowid = 'x'`, same("0")},
		}
	}
	for _, kt := range []struct{ name, decl, k1, k2 string }{
		{"integer", "INTEGER", "1", "2"},
		{"text", "TEXT", "'a'", "'b'"},
		{"blob", "BLOB", "x'01'", "x'02'"},
	} {
		for _, index := range []bool{false, true} {
			for _, stage := range []string{"session", "log", "segments"} {
				stmts := []string{
					fmt.Sprintf(`CREATE TABLE o(id INTEGER PRIMARY KEY, want %s, lim INTEGER)`, kt.decl),
					fmt.Sprintf(`CREATE TABLE i(k %s, j TEXT)`, kt.decl),
					fmt.Sprintf(`INSERT INTO o VALUES (1, %s, 10), (2, %s, 0)`, kt.k1, kt.k1),
					fmt.Sprintf(`INSERT INTO i VALUES (%s, '1'), (%s, 'bad JSON')`, kt.k1, kt.k2),
				}
				if index {
					stmts = append(stmts, `CREATE INDEX i_k ON i(k)`)
				}
				open := func(t *testing.T) *Session { return seekRaiseSession(t, stage, stmts) }
				pick := func(o outcome) string {
					if index {
						return o.index
					}
					return o.noIndex
				}
				t.Run(fmt.Sprintf("%s/index=%v/%s", kt.name, index, stage), func(t *testing.T) {
					n := open(t)
					defer n.Discard()
					for _, tc := range reads(kt.k1) {
						if got, want := seekRaiseAnswer(n, tc.q), pick(tc.want); got != want {
							t.Errorf("%s\n got %q\nwant %q", tc.q, got, want)
						}
					}
					for _, tc := range writes(kt.k1) {
						w := open(t)
						if got, want := seekRaiseWrite(w, tc.q), pick(tc.want); got != want {
							t.Errorf("%s\n got %q\nwant %q", tc.q, got, want)
						}
						w.Discard()
					}
				})
			}
		}
	}
}

// TestSeekRaiseSubselect: a term holding a sub-select that names nothing
// outside itself is coded in WHERE order, as any term is, so a seek on a key
// behind one that can raise must not skip the row it raises on; C tests a
// correlated one after every other term, the key included. The first row holds
// malformed JSON under a key the queries do not want. Each outcome is C's,
// verified by compat-harness's TestSeekRaiseSubselectVsC.
func TestSeekRaiseSubselect(t *testing.T) {
	const (
		fail         = seekRaiseFail
		uncorrelated = `json_extract(j, '$') < (SELECT max(v) FROM other)`
		correlated   = `json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > i.value - 100)`
		constant     = `(SELECT json_extract('[', '$')) IS NULL`
	)
	type outcome struct{ noIndex, index string }
	same := func(s string) outcome { return outcome{s, s} }
	type test struct {
		q    string
		want outcome
		with []string // run before q
	}
	where := func(terms string, want outcome) test {
		return test{q: `SELECT value FROM i WHERE ` + terms, want: want}
	}
	// A trigger's NEW is found before lookupName climbs out of a sub-select, so
	// a sub-select that names only NEW is not correlated; one that also names
	// the table is.
	trigger := func(terms string, want outcome) test {
		return test{q: `INSERT INTO t VALUES (1)`, want: want, with: []string{
			`CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log SELECT value FROM i WHERE ` + terms + `; END`}}
	}
	onlyNew := `json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > new.x - 100)`
	newAndTable := `json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > new.x + i.value - 100)`
	// The sub-select names a table of the query the EXISTS is in.
	outer := `json_extract(i.j, '$') < (SELECT max(v) FROM other WHERE v < o.lim)`
	reads := []test{
		where(uncorrelated+` AND value = 7`, outcome{fail, "7"}),
		where(`value = 7 AND `+uncorrelated, same("7")),
		where(correlated+` AND value = 7`, same("7")),
		where(`value = 7 AND `+correlated, same("7")),
		where(`json_extract(j, '$') IN (SELECT v FROM other) AND value = 7`, outcome{fail, ""}),
		where(`value = 7 AND json_extract(j, '$') IN (SELECT v FROM other)`, same("")),
		where(`EXISTS (SELECT 1 FROM other WHERE json_extract(i.j, '$') < v) AND value = 7`, same("7")),
		where(`json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > (SELECT max(v) FROM other WHERE v > i.value - 100) - 100) AND value = 7`, same("7")),
		where(correlated+` AND `+uncorrelated+` AND value = 7`, outcome{fail, "7"}),
		where(uncorrelated+` AND `+correlated+` AND value = 7`, outcome{fail, "7"}),
		where(constant+` AND value = 7`, same(fail)),
		where(`value = 7 AND `+constant, same(fail)),
		where(constant+` AND value = 9`, outcome{fail, ""}),
		where(`value = 9 AND `+constant, same("")),
		{q: `SELECT o.id FROM o WHERE EXISTS (SELECT 1 FROM i WHERE ` + outer + ` AND i.value = o.want) ORDER BY o.id`, want: same("1")},
		{q: `SELECT o.id FROM o WHERE EXISTS (SELECT 1 FROM i WHERE i.value = o.want AND ` + outer + `) ORDER BY o.id`, want: same("1")},
	}
	writes := []test{
		{q: `UPDATE i SET j = j || '' WHERE ` + uncorrelated + ` AND value = 7`, want: outcome{fail, "1"}},
		{q: `UPDATE i SET j = j || '' WHERE value = 7 AND ` + uncorrelated, want: same("1")},
		{q: `DELETE FROM i WHERE ` + uncorrelated + ` AND value = 7`, want: outcome{fail, "1"}},
		{q: `DELETE FROM i WHERE ` + correlated + ` AND value = 7`, want: same("1")},
		{q: `DELETE FROM i WHERE ` + constant + ` AND value = 9`, want: outcome{fail, "0"}},
		trigger(onlyNew+` AND value = 7`, outcome{fail, "1"}),
		trigger(`value = 7 AND `+onlyNew, same("1")),
		trigger(newAndTable+` AND value = 7`, same("1")),
	}
	for _, index := range []bool{false, true} {
		for _, stage := range []string{"session", "log", "segments"} {
			stmts := []string{
				`CREATE TABLE i(value INT, j TEXT)`,
				`CREATE TABLE other(v)`,
				`CREATE TABLE o(id INTEGER PRIMARY KEY, want INT, lim INT)`,
				`CREATE TABLE t(x INT)`,
				`CREATE TABLE log(a)`,
				`INSERT INTO other VALUES (5)`,
				`INSERT INTO i VALUES (1, 'bad'), (7, '1')`,
				`INSERT INTO o VALUES (1, 7, 10), (2, 9, 10)`,
			}
			if index {
				stmts = append(stmts, `CREATE INDEX i_value ON i(value)`)
			}
			pick := func(o outcome) string {
				if index {
					return o.index
				}
				return o.noIndex
			}
			t.Run(fmt.Sprintf("index=%v/%s", index, stage), func(t *testing.T) {
				n := seekRaiseSession(t, stage, stmts)
				defer n.Discard()
				for _, tc := range reads {
					if got, want := seekRaiseAnswer(n, tc.q), pick(tc.want); got != want {
						t.Errorf("%s\n got %q\nwant %q", tc.q, got, want)
					}
				}
				for _, tc := range writes {
					w := seekRaiseSession(t, stage, append(slices.Clone(stmts), tc.with...))
					if got, want := seekRaiseWrite(w, tc.q), pick(tc.want); got != want {
						t.Errorf("%s\n got %q\nwant %q", tc.q, got, want)
					}
					w.Discard()
				}
			})
		}
	}
}

// TestSeekRaisingTermStillSeeks: the seeks that stay exact are still taken --
// a key the JSON term follows, a key C's own index consumes, and a key that a
// correlated sub-select, which C tests last, comes before.
func TestSeekRaisingTermStillSeeks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	stmts := []string{
		`CREATE TABLE i(k INTEGER, j TEXT)`, `CREATE TABLE x(k INTEGER, j TEXT)`, `CREATE INDEX x_k ON x(k)`,
		`CREATE TABLE other(v)`, `INSERT INTO other VALUES (1000)`,
	}
	for r := 0; r < 600; r++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO i VALUES (%d, '%d')`, r%30, r), fmt.Sprintf(`INSERT INTO x VALUES (%d, '%d')`, r%30, r))
	}
	buildDB(t, path, stmts...)
	execDB(t, path, `VACUUM`)
	for _, tc := range []struct {
		q    string
		seek bool
	}{
		{`SELECT rowid FROM i WHERE k = 7 AND json_extract(j, '$') > 0`, true},
		{`SELECT rowid FROM i WHERE json_extract(j, '$') > 0 AND k = 7`, false},
		{`SELECT rowid FROM x WHERE json_extract(j, '$') > 0 AND k = 7`, true},
		{`SELECT rowid FROM i WHERE k = 7 AND json_extract(j, '$') < (SELECT max(v) FROM other)`, true},
		{`SELECT rowid FROM i WHERE json_extract(j, '$') < (SELECT max(v) FROM other) AND k = 7`, false},
		{`SELECT rowid FROM i WHERE json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > i.k - 100) AND k = 7`, true},
		{`SELECT rowid FROM x WHERE json_extract(j, '$') < (SELECT max(v) FROM other) AND k = 7`, true},
	} {
		segIndexSeeksServed.Store(0)
		got, err := queryDB(t, path, tc.q)
		if err != nil || strings.Count(got, "\n") != 20 {
			t.Fatalf("%s: %q, %v", tc.q, got, err)
		}
		if seeked := segIndexSeeksServed.Load() > 0; seeked != tc.seek {
			t.Errorf("%s: seeked %v, want %v", tc.q, seeked, tc.seek)
		}
	}
	for _, tc := range []struct {
		q    string
		seek bool
	}{
		{`DELETE FROM i WHERE k = 7 AND json_extract(j, '$') < (SELECT max(v) FROM other)`, true},
		{`DELETE FROM i WHERE json_extract(j, '$') < (SELECT max(v) FROM other) AND k = 7`, false},
		{`DELETE FROM i WHERE json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > i.k - 100) AND k = 7`, true},
	} {
		n, err := OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		served := writeIndexSeeksServed.Load()
		changed, _, err := n.ExecArgs(tc.q, nil)
		n.Discard()
		if err != nil || changed != 20 {
			t.Fatalf("%s: deleted %d, %v", tc.q, changed, err)
		}
		if seeked := writeIndexSeeksServed.Load() > served; seeked != tc.seek {
			t.Errorf("%s: seeked %v, want %v", tc.q, seeked, tc.seek)
		}
	}
}
