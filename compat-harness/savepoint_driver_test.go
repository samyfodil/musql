// This file gates SAVEPOINT / RELEASE / ROLLBACK TO through the driver.
// Savepoints must hold a session across the statement boundary like BEGIN does.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestSavepointThroughDriver(t *testing.T) {
	for _, c := range []struct {
		name  string
		stmts []string
	}{
		{"release-commits", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`, `RELEASE a`,
			// the transaction is over, so this COMMIT must FAIL
			`COMMIT`}},
		{"commit-after-savepoint", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`, `COMMIT`}},
		{"rollback-after-savepoint", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`, `ROLLBACK`}},
		{"rollback-to-keeps-txn-open", []string{
			`SAVEPOINT c`, `INSERT INTO t VALUES(1)`, `ROLLBACK TO c`,
			`INSERT INTO t VALUES(2)`, `COMMIT`}},
		{"rollback-to-then-release", []string{
			`SAVEPOINT c`, `INSERT INTO t VALUES(1)`, `ROLLBACK TO c`,
			`INSERT INTO t VALUES(2)`, `RELEASE c`}},
		{"nested-release-outer", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`,
			`SAVEPOINT b`, `INSERT INTO t VALUES(2)`, `RELEASE a`, `COMMIT`}},
		{"nested-rollback-inner", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`,
			`SAVEPOINT b`, `INSERT INTO t VALUES(2)`, `ROLLBACK TO b`, `RELEASE a`}},
		{"inside-explicit-begin", []string{
			`BEGIN`, `SAVEPOINT c`, `INSERT INTO t VALUES(1)`, `RELEASE c`, `COMMIT`}},
		{"inside-begin-no-release", []string{
			`BEGIN`, `SAVEPOINT c`, `INSERT INTO t VALUES(1)`, `COMMIT`}},
		{"inside-begin-rollback-to", []string{
			`BEGIN`, `SAVEPOINT c`, `INSERT INTO t VALUES(1)`, `ROLLBACK TO c`,
			`INSERT INTO t VALUES(2)`, `COMMIT`}},
		{"inside-begin-then-rollback", []string{
			`BEGIN`, `SAVEPOINT c`, `INSERT INTO t VALUES(1)`, `ROLLBACK`}},
		// error paths: nothing open, and an unknown name
		{"release-nothing-open", []string{`RELEASE nope`}},
		{"rollback-to-nothing-open", []string{`ROLLBACK TO nope`}},
		{"release-unknown-name", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`, `RELEASE zzz`, `RELEASE a`}},
		{"rollback-to-unknown-name", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`, `ROLLBACK TO zzz`, `RELEASE a`}},
		// the spellings the grammar allows
		{"release-savepoint-keyword", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`, `RELEASE SAVEPOINT a`}},
		{"rollback-transaction-to", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`, `ROLLBACK TRANSACTION TO SAVEPOINT a`, `RELEASE a`}},
		{"quoted-name", []string{
			`SAVEPOINT "a b"`, `INSERT INTO t VALUES(1)`, `RELEASE "a b"`}},
		// a savepoint reused after its release
		{"reuse-name", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1)`, `RELEASE a`,
			`SAVEPOINT a`, `INSERT INTO t VALUES(2)`, `RELEASE a`}},
		// DDL and a multi-row write under a savepoint
		{"ddl-under-savepoint", []string{
			`SAVEPOINT a`, `CREATE TABLE u(x)`, `INSERT INTO u VALUES(1)`, `ROLLBACK TO a`,
			`INSERT INTO t VALUES(9)`, `RELEASE a`}},
		{"multirow-under-savepoint", []string{
			`SAVEPOINT a`, `INSERT INTO t VALUES(1),(2),(3)`, `ROLLBACK TO a`,
			`INSERT INTO t VALUES(4)`, `RELEASE a`}},
	} {
		stmts := append([]string{`CREATE TABLE t(a)`}, c.stmts...)
		stmts = append(stmts,
			`SELECT count(*) FROM t`,
			`SELECT quote(a) FROM t ORDER BY a`,
			`SELECT count(*) FROM sqlite_master`)
		differ(t, c.name, stmts)
	}
}

// TestSavepointThroughDriverFuzz randomizes the script, because what survives
// depends on the ORDER of the savepoint verbs and on which name each one
// carries -- and the interesting cases are the ones where a release or a
// rollback lands on a savepoint that is not the innermost.
func TestSavepointThroughDriverFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260803))
	for i := 0; i < 120; i++ {
		i := i
		t.Run(fmt.Sprintf("s%03d", i), func(t *testing.T) {
			stmts := []string{`CREATE TABLE t(a)`}
			var open []string // the savepoint names believed open, outermost first
			if rng.Intn(3) == 0 {
				stmts = append(stmts, `BEGIN`)
			}
			for k := 0; k < 3+rng.Intn(6); k++ {
				switch rng.Intn(6) {
				case 0:
					n := fmt.Sprintf("sp%d", rng.Intn(3))
					stmts = append(stmts, `SAVEPOINT `+n)
					open = append(open, n)
				case 1:
					if len(open) > 0 {
						n := open[rng.Intn(len(open))]
						stmts = append(stmts, `RELEASE `+n)
					} else {
						stmts = append(stmts, `RELEASE sp0`)
					}
				case 2:
					if len(open) > 0 {
						n := open[rng.Intn(len(open))]
						stmts = append(stmts, `ROLLBACK TO `+n)
					} else {
						stmts = append(stmts, `ROLLBACK TO sp0`)
					}
				case 3:
					stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d)`, k))
				case 4:
					stmts = append(stmts, fmt.Sprintf(`DELETE FROM t WHERE a=%d`, rng.Intn(6)))
				default:
					stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES(%d),(%d)`, k, k+100))
				}
			}
			// close out whatever is open, both ways
			if rng.Intn(2) == 0 {
				stmts = append(stmts, `COMMIT`)
			} else {
				stmts = append(stmts, `ROLLBACK`)
			}
			stmts = append(stmts,
				`SELECT count(*) FROM t`,
				`SELECT quote(a) FROM t ORDER BY a`)
			differ(t, fmt.Sprintf("s%03d %s", i, strings.Join(stmts[1:len(stmts)-2], "; ")), stmts)
		})
	}
}
