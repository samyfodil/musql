package compat

import (
	"database/sql"
	"testing"
)

// TestNilBytesBindAgainstOracle: a nil []byte binds as NULL and an empty one
// as a zero-length BLOB, by position and by name, as mattn/go-sqlite3 binds
// them over C SQLite: what is stored, and what a bound value compares as.
func TestNilBytesBindAgainstOracle(t *testing.T) {
	pureC, mattnC := crossConns(t, "nilbytes")
	execBothConn(t, pureC, mattnC, "create", `CREATE TABLE t(id INTEGER PRIMARY KEY, v)`)
	vals := []any{[]byte(nil), []byte{}, []byte{0}, nil}
	for i, v := range vals {
		execBothConn(t, pureC, mattnC, "insert", `INSERT INTO t VALUES(?, ?)`, i, v)
		execBothConn(t, pureC, mattnC, "insert named", `INSERT INTO t VALUES(?, :v)`, i+10, sql.Named("v", v))
	}
	queryBothConn(t, pureC, mattnC, "stored", `SELECT id, typeof(v), length(v), v IS NULL, quote(v) FROM t ORDER BY id`)
	for _, v := range vals {
		queryBothConn(t, pureC, mattnC, "bound", `SELECT ? IS NULL, typeof(?), length(?), quote(?)`, v, v, v, v)
	}
}
