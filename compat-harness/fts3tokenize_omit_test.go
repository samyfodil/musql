package compat

import "testing"

// Tests fts3tokenize constraint omitting: the module reports a non-TEXT
// "input =" constraint, and that constraint is removed from the residual
// WHERE so rows match the reported TEXT value, not the query value.
func TestFts3TokenizeOmittedConstraint(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts3tokenize(simple)`,
		`CREATE VIRTUAL TABLE t2 USING fts3tokenize()`,
		`CREATE VIRTUAL TABLE t3 USING fts3tokenize(simple, '', 'xyz ')`,
	}
	for _, q := range []string{
		// Corpus statement and storage classes.
		`SELECT * FROM t3 WHERE input = 123`,
		`SELECT *, typeof(input), typeof(token) FROM t3 WHERE input = 123`,
		`SELECT * FROM t1 WHERE input = 1.5`,
		`SELECT * FROM t1 WHERE input = -12`,
		`SELECT * FROM t1 WHERE input = +12`,
		`SELECT * FROM t1 WHERE input = 9223372036854775807`,
		`SELECT * FROM t1 WHERE input = -9223372036854775808`,
		`SELECT * FROM t1 WHERE input = 1e300`,
		`SELECT * FROM t1 WHERE input = -0.0`,
		`SELECT * FROM t1 WHERE input = 0.1`,
		`SELECT * FROM t1 WHERE input = x'616263'`,
		`SELECT * FROM t2 WHERE input = 123`,
		// Residual WHERE: second conjunct must still be applied.
		`SELECT * FROM t1 WHERE input = 123 AND input = 'a b'`,
		`SELECT * FROM t1 WHERE input = 'a b' AND input = 123`,
		// ...and a non-EQ one, which C skips when choosing what to consume and
		// then re-applies against the reported TEXT.
		`SELECT * FROM t1 WHERE input < 'b' AND input = 123`,
		`SELECT * FROM t1 WHERE input >= 'a' AND input = 123`,
		`SELECT * FROM t1 WHERE input = 123 AND input > 'a'`,
		`SELECT * FROM t1 WHERE input = 123 AND position > 0`,
		`SELECT * FROM t1 WHERE input = 12 AND input = 34`,
		`SELECT * FROM t1 WHERE 123 = input`,

		// Where the scan is compiled from.
		`SELECT * FROM (SELECT * FROM t1 WHERE input = 123)`,
		`WITH w AS (SELECT * FROM t1 WHERE input = 123) SELECT * FROM w`,
		`SELECT * FROM t1 WHERE input = 123 UNION ALL SELECT * FROM t1 WHERE input = 45`,
		`SELECT (SELECT count(*) FROM t1 WHERE input = 123)`,
		`SELECT count(*) FROM t1 WHERE input = 123`,
		`SELECT group_concat(token) FROM t1 WHERE input = 123`,
		`SELECT * FROM t1 WHERE input = 123 GROUP BY token`,
		`SELECT * FROM t1 WHERE input = 123 ORDER BY position DESC`,
		`SELECT * FROM t1 WHERE input = 123 LIMIT 1`,
		`SELECT rowid, * FROM t1 WHERE input = 123`,

		// Two tables, each owning its own conjunct; then one name claimed by
		// two aliases, which is ambiguous and must error on both engines.
		`SELECT * FROM t1, t3 WHERE t1.input = 123 AND t3.input = 456`,
		`SELECT * FROM t1 AS a, t1 AS b WHERE a.input = 123 AND b.input = 456`,
		`SELECT * FROM t1 AS a, t1 AS b WHERE input = 123`,
		`SELECT * FROM t1 AS z WHERE z.input = 123`,
		`SELECT * FROM t1 AS z WHERE input = 123`,

		// The TEXT spellings from fts3tok1.test, which must go on answering.
		`SELECT * FROM t1 WHERE input = 'one two three'`,
		`SELECT token FROM t1 WHERE input = 'OnE tWo tHrEe'`,
		`SELECT token FROM t3 WHERE input = '1x2x3x'`,
		`SELECT token FROM t1 WHERE input = '1x2x3x'`,
		`SELECT token FROM t3 WHERE input = '1''2x3x'`,
		`SELECT token FROM t3 WHERE input = ''`,
		`SELECT token FROM t3 WHERE input = NULL`,
		`SELECT * FROM t1 WHERE input = 'a b c' AND token = 'b'`,
		`SELECT * FROM t1 WHERE token = 'b' AND input = 'a b c'`,
		`SELECT * FROM t1 WHERE input < 'b' AND input = 'a b c'`,
		`SELECT * FROM t1 WHERE input = CAST(123 AS TEXT)`,
	} {
		differ(t, "fts3tokenize-omit/"+q, append(append([]string{}, setup...), q))
	}
}

// TestFts3TokenizeStillDeclines tests the boundary: shapes the omit does not
// cover, which stay declined (non-literal RHS, non-top-level AND conjunct,
// embedded NUL in tokenizer).
func TestFts3TokenizeStillDeclines(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t1 USING fts3tokenize(simple)`,
		`CREATE TABLE c1(x)`,
		`INSERT INTO c1(x) VALUES('a b c')`,
		`INSERT INTO c1(x) VALUES('d e f')`,
	}
	for _, tc := range []struct {
		why string
		q   string
	}{
		{"a non-literal right-hand side cannot be proved to be the consumed constraint",
			`SELECT * FROM t1 WHERE input = (SELECT 123)`},
		{"an OR is not a top-level conjunct, so nothing reaches BestIndex",
			`SELECT * FROM t1 WHERE input = 12 OR input = 13`},
		{"a JOIN's ON clause is not pushed to a vtab source",
			`SELECT * FROM c1 LEFT JOIN t1 ON input = 123`},
		{"an embedded NUL is a tokenizer difference, not a WHERE one",
			`SELECT * FROM t1 WHERE input = x'6120620063'`},
	} {
		stmts := append(append([]string{}, setup...), tc.q)
		last := len(stmts) - 1
		cgo := run(t, "cgo", stmts)[last]
		mush := run(t, "musql", stmts)[last]
		rows, _ := cgo["rows"].([]any)
		if cgo["kind"] != "rows" || len(rows) == 0 {
			t.Errorf("[%s] %s: C SQLite no longer ANSWERS it (%v). The decline recorded here was\n"+
				"justified by C answering; re-settle the shape against the oracle.", tc.why, tc.q, cgo)
			continue
		}
		if mush["kind"] != "error" {
			t.Errorf("[%s] %s: musql now ANSWERS it (%v), where it declined before.\n"+
				"That may well be right -- but it needs its own oracle evidence and a case in\n"+
				"TestFts3TokenizeOmittedConstraint, not a silently changed boundary.", tc.why, tc.q, mush)
		}
	}
}
