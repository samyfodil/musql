// This file tests that writes made while fts3/fts4 automerge is enabled
// actually survive the session. All documents must remain reachable by MATCH.
package compat

import "testing"

// TestFts3AutomergeWritesSurvive tests that documents written after automerge is enabled remain reachable by MATCH.
func TestFts3AutomergeWritesSurvive(t *testing.T) {
	for _, mod := range []string{"fts3", "fts4"} {
		for _, n := range []string{"0", "2", "8", "16"} {
			t.Run(mod+"/automerge="+n, func(t *testing.T) {
				differ(t, mod+" automerge="+n+" writes survive", []string{
					`CREATE VIRTUAL TABLE t USING ` + mod + `(a)`,
					`INSERT INTO t(docid,a) VALUES(1,'alpha')`,
					`INSERT INTO t(t) VALUES('automerge=` + n + `')`,
					`INSERT INTO t(docid,a) VALUES(2,'beta')`,
					`INSERT INTO t(docid,a) VALUES(3,'gamma delta')`,
					`UPDATE t SET a='epsilon' WHERE docid=2`,
					`DELETE FROM t WHERE docid=3`,
					`INSERT INTO t(docid,a) VALUES(4,'zeta')`,
					`SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid`,
					`SELECT docid FROM t WHERE t MATCH 'beta' ORDER BY docid`,
					`SELECT docid FROM t WHERE t MATCH 'epsilon' ORDER BY docid`,
					`SELECT docid FROM t WHERE t MATCH 'gamma' ORDER BY docid`,
					`SELECT docid FROM t WHERE t MATCH 'delta' ORDER BY docid`,
					`SELECT docid FROM t WHERE t MATCH 'zeta' ORDER BY docid`,
					`SELECT docid, a FROM t ORDER BY docid`,
					`PRAGMA integrity_check`,
				})
			})
		}
	}
}

// TestFts3AutomergeManyWritesSurvive tests automerge with many writes that trigger the actual merge operation.
func TestFts3AutomergeManyWritesSurvive(t *testing.T) {
	stmts := []string{
		`CREATE VIRTUAL TABLE t USING fts4(a)`,
		`INSERT INTO t(t) VALUES('automerge=2')`,
	}
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}
	for i := 1; i <= 40; i++ {
		stmts = append(stmts, `INSERT INTO t(docid,a) VALUES(`+itoa(i)+`,'`+words[i%len(words)]+` w`+itoa(i)+`')`)
	}
	for _, w := range words {
		stmts = append(stmts, `SELECT docid FROM t WHERE t MATCH '`+w+`' ORDER BY docid`)
	}
	stmts = append(stmts,
		`SELECT docid FROM t WHERE t MATCH 'w37' ORDER BY docid`,
		`SELECT count(*) FROM t`,
		`PRAGMA integrity_check`,
	)
	differ(t, "fts4 automerge=2 many writes survive", stmts)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
