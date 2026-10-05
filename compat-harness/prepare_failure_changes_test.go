package compat

// Tests changes() counter for statements failing PREPARE-TIME validation.

import "testing"

func TestPrepareFailureLeavesChangesAlone(t *testing.T) {
	seed := []string{
		`CREATE TABLE t(a)`,
		`CREATE TABLE s(a)`,
		`INSERT INTO t VALUES(1),(2),(3)`,
		`SELECT changes()`,
	}
	cases := []struct {
		name string
		stmt string
	}{
		{"update-set-no-such-column", `UPDATE t SET a=zzz`},
		{"delete-where-no-such-function", `DELETE FROM t WHERE a=nosuchfn(1)`},
		{"delete-where-no-such-column", `DELETE FROM t WHERE zzz=1`},
		{"update-where-no-such-column", `UPDATE t SET a=9 WHERE zzz=1`},
		{"insert-select-no-such-function", `INSERT INTO t SELECT nosuchfn(a) FROM s`},
		{"insert-select-no-such-column", `INSERT INTO t SELECT zzz FROM s`},
		{"returning-no-such-function", `INSERT INTO t VALUES(1) RETURNING nosuchfn(a)`},
	}
	for _, tc := range cases {
		stmts := append(append([]string(nil), seed...), tc.stmt, `SELECT changes()`, `SELECT count(*) FROM t`)
		differ(t, "prepare-failure/"+tc.name, stmts)
	}
}
