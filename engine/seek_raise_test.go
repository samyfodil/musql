package engine

import (
	"fmt"
	"path/filepath"
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

// TestSeekRaisingTerm: a seek over a WHERE holding a term that can raise
// answers what C SQLite does (each outcome below is C's, verified against the
// oracle by compat-harness's TestSeekRaisingTermVsC). The inner table's second
// row holds malformed JSON under a key no outer row wants: C reads it, and
// fails, exactly when its loop reaches the JSON term on that row -- with no
// index on the key it scans, and with one it never visits the row.
func TestSeekRaisingTerm(t *testing.T) {
	const fail = "malformed JSON"
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
	answer := func(n *Session, q string) string {
		_, rows, err := n.Query(q, nil)
		if err != nil {
			if strings.Contains(err.Error(), fail) {
				return fail
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
	for _, kt := range []struct{ name, decl, k1, k2 string }{
		{"integer", "INTEGER", "1", "2"},
		{"text", "TEXT", "'a'", "'b'"},
		{"blob", "BLOB", "x'01'", "x'02'"},
	} {
		for _, index := range []bool{false, true} {
			for _, stage := range []string{"session", "log", "segments"} {
				open := func(t *testing.T) *Session {
					path := filepath.Join(t.TempDir(), "x.musq")
					n, err := Create(path)
					if err != nil {
						t.Fatal(err)
					}
					stmts := []string{
						fmt.Sprintf(`CREATE TABLE o(id INTEGER PRIMARY KEY, want %s, lim INTEGER)`, kt.decl),
						fmt.Sprintf(`CREATE TABLE i(k %s, j TEXT)`, kt.decl),
						fmt.Sprintf(`INSERT INTO o VALUES (1, %s, 10), (2, %s, 0)`, kt.k1, kt.k1),
						fmt.Sprintf(`INSERT INTO i VALUES (%s, '1'), (%s, 'bad JSON')`, kt.k1, kt.k2),
					}
					if index {
						stmts = append(stmts, `CREATE INDEX i_k ON i(k)`)
					}
					if stage == "segments" {
						stmts = append(stmts, `VACUUM`)
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
						if got, want := answer(n, tc.q), pick(tc.want); got != want {
							t.Errorf("%s\n got %q\nwant %q", tc.q, got, want)
						}
					}
					for _, tc := range writes(kt.k1) {
						w := open(t)
						changed, _, err := w.ExecArgs(tc.q, nil)
						got := fmt.Sprint(changed)
						switch {
						case err != nil && strings.Contains(err.Error(), fail):
							got = fail
						case err != nil:
							got = err.Error()
						}
						if want := pick(tc.want); got != want {
							t.Errorf("%s\n got %q\nwant %q", tc.q, got, want)
						}
						w.Discard()
					}
				})
			}
		}
	}
}

// TestSeekRaisingTermStillSeeks: the seeks that stay exact are still taken --
// a key the JSON term follows, and a key C's own index consumes.
func TestSeekRaisingTermStillSeeks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.musq")
	stmts := []string{`CREATE TABLE i(k INTEGER, j TEXT)`, `CREATE TABLE x(k INTEGER, j TEXT)`, `CREATE INDEX x_k ON x(k)`}
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
}
