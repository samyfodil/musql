package compat

// Seeks against C SQLite where the WHERE holds a term that can raise: C
// evaluates its terms in one order on the rows its own loop visits, and fails
// on the first row that reaches the raising term. A seek must neither skip a
// row C reaches that term on nor reach it on a row C never visits. Every
// statement runs on both engines, over the log the rows were written to and
// then over segments, and the outcome -- the rows, or that it failed -- must
// match.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// seekRaiseKeys are the key classes: the inner table's two keys, the first
// the one the outer rows want.
var seekRaiseKeys = []struct{ name, decl, k1, k2 string }{
	{"integer", "INTEGER", "1", "2"},
	{"text", "TEXT", "'a'", "'b'"},
	{"blob", "BLOB", "x'01'", "x'02'"},
}

// seekRaiseOpen opens both engines on one schema: an inner table whose second
// row, under a key no outer row wants, holds malformed JSON.
func seekRaiseOpen(t *testing.T, decl, k1, k2 string, index, segments bool) (pure, mattn *sql.Conn) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	open := func(drv, tag string) *sql.Conn {
		db, err := sql.Open(drv, filepath.Join(dir, tag+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	pure, mattn = open(driver.DriverName, "pure"), open("sqlite3", "mattn")
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE o(id INTEGER PRIMARY KEY, want %s, lim INTEGER)`, decl),
		fmt.Sprintf(`CREATE TABLE i(k %s, j TEXT)`, decl),
		fmt.Sprintf(`INSERT INTO o VALUES (1, %s, 10), (2, %s, 0)`, k1, k1),
		fmt.Sprintf(`INSERT INTO i VALUES (%s, '1'), (%s, 'bad JSON')`, k1, k2),
	}
	if index {
		stmts = append(stmts, `CREATE INDEX i_k ON i(k)`)
	}
	if segments {
		stmts = append(stmts, `VACUUM`)
	}
	for _, s := range stmts {
		execBothConn(t, pure, mattn, "setup", s)
	}
	return pure, mattn
}

// seekRaiseOutcome is what q answers on c: its rows, or that it failed.
func seekRaiseOutcome(c *sql.Conn, q string) string {
	failed := func(err error) string {
		if strings.Contains(err.Error(), "malformed JSON") {
			return "error: malformed JSON"
		}
		return "error: " + err.Error()
	}
	rows, err := c.QueryContext(context.Background(), q)
	if err != nil {
		return failed(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return failed(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return failed(err)
		}
		var vs []string
		for _, v := range vals {
			if b, ok := v.([]byte); ok {
				v = fmt.Sprintf("%x", b)
			}
			vs = append(vs, fmt.Sprint(v))
		}
		out = append(out, strings.Join(vs, "|"))
	}
	if err := rows.Err(); err != nil {
		return failed(err)
	}
	return "rows: " + strings.Join(out, " ")
}

// seekRaiseWriteOutcome is what a write answers on c, and the table after it.
func seekRaiseWriteOutcome(c *sql.Conn, q string) string {
	res, err := c.ExecContext(context.Background(), q)
	if err != nil {
		if strings.Contains(err.Error(), "malformed JSON") {
			return "error: malformed JSON; " + seekRaiseOutcome(c, `SELECT rowid, k, j FROM i ORDER BY rowid`)
		}
		return "error: " + err.Error()
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("changes %d; %s", n, seekRaiseOutcome(c, `SELECT rowid, k, j FROM i ORDER BY rowid`))
}

// seekRaiseTerms are the inner WHERE in both orders: the term that can raise
// first, then the key, and the key first.
func seekRaiseTerms(raise, key string) [2]string {
	return [2]string{raise + " AND " + key, key + " AND " + raise}
}

func TestSeekRaisingTermVsC(t *testing.T) {
	for _, kt := range seekRaiseKeys {
		for _, index := range []bool{false, true} {
			for _, segments := range []bool{false, true} {
				name := fmt.Sprintf("%s/index=%v/segments=%v", kt.name, index, segments)
				t.Run(name, func(t *testing.T) {
					pure, mattn := seekRaiseOpen(t, kt.decl, kt.k1, kt.k2, index, segments)
					var queries []string
					for _, terms := range seekRaiseTerms(`json_extract(i.j, '$') < o.lim`, `i.k = o.want`) {
						queries = append(queries,
							`SELECT o.id FROM o WHERE EXISTS (SELECT 1 FROM i WHERE `+terms+`) ORDER BY o.id`,
							`SELECT o.id FROM o WHERE NOT EXISTS (SELECT 1 FROM i WHERE `+terms+`) ORDER BY o.id`,
							`SELECT o.id, (SELECT count(*) FROM i WHERE `+terms+`) FROM o ORDER BY o.id`,
							`SELECT o.id FROM o WHERE o.want IN (SELECT i.k FROM i WHERE `+terms+`) ORDER BY o.id`)
					}
					for _, terms := range seekRaiseTerms(`json_extract(j, '$') > 0`, `k = `+kt.k1) {
						queries = append(queries, `SELECT rowid FROM i WHERE `+terms)
					}
					for _, key := range []string{`rowid = 'x'`, `rowid = 1.5`, `rowid = NULL`, `rowid = '1'`,
						`rowid < 2`, `rowid <= 1.5`, `rowid > 'abc'`, `rowid >= 2.5`, `rowid < 'abc'`} {
						for _, terms := range seekRaiseTerms(`json_extract(j, '$') > 0`, key) {
							queries = append(queries, `SELECT rowid FROM i WHERE `+terms)
						}
					}
					for _, q := range queries {
						if got, want := seekRaiseOutcome(pure, q), seekRaiseOutcome(mattn, q); got != want {
							t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
						}
					}
				})
				t.Run(name+"/writes", func(t *testing.T) {
					var writes []string
					for _, terms := range seekRaiseTerms(`json_extract(j, '$') > 0`, `k = `+kt.k1) {
						writes = append(writes, `UPDATE i SET j = j || '' WHERE `+terms, `DELETE FROM i WHERE `+terms)
					}
					for _, terms := range seekRaiseTerms(`json_extract(j, '$') > 0`, `rowid = 'x'`) {
						writes = append(writes, `UPDATE i SET j = j || '' WHERE `+terms, `DELETE FROM i WHERE `+terms)
					}
					for _, q := range writes {
						pure, mattn := seekRaiseOpen(t, kt.decl, kt.k1, kt.k2, index, segments)
						if got, want := seekRaiseWriteOutcome(pure, q), seekRaiseWriteOutcome(mattn, q); got != want {
							t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
						}
					}
				})
			}
		}
	}
}
