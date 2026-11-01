package compat

// Seeks where the WHERE holds a term that can raise: C fails on the first row
// its own loop brings to that term, so a seek must neither skip such a row nor
// reach the term on a row C never visits. Rows and failures must match, over
// the log and over segments.

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

// seekRaiseConns opens a connection to each engine on a database of its own.
func seekRaiseConns(t *testing.T) (pure, mattn *sql.Conn) {
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
	return open(driver.DriverName, "pure"), open("sqlite3", "mattn")
}

// seekRaiseOpen opens both engines on one schema: an inner table whose second
// row, under a key no outer row wants, holds malformed JSON.
func seekRaiseOpen(t *testing.T, decl, k1, k2 string, index, segments bool) (pure, mattn *sql.Conn) {
	t.Helper()
	pure, mattn = seekRaiseConns(t)
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

// seekRaiseWriteOutcome is what a write answers on c, and what dump reads of
// the table after it.
func seekRaiseWriteOutcome(c *sql.Conn, q, dump string) string {
	res, err := c.ExecContext(context.Background(), q)
	if err != nil {
		if strings.Contains(err.Error(), "malformed JSON") {
			return "error: malformed JSON; " + seekRaiseOutcome(c, dump)
		}
		return "error: " + err.Error()
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("changes %d; %s", n, seekRaiseOutcome(c, dump))
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
						const dump = `SELECT rowid, k, j FROM i ORDER BY rowid`
						if got, want := seekRaiseWriteOutcome(pure, q, dump), seekRaiseWriteOutcome(mattn, q, dump); got != want {
							t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
						}
					}
				})
			}
		}
	}
}

// seekRaiseSubOpen opens both engines on a table whose first row holds
// malformed JSON under a key the queries do not want, a one-row table for the
// sub-selects to read, and an outer table of the keys wanted.
func seekRaiseSubOpen(t *testing.T, index, segments bool) (pure, mattn *sql.Conn) {
	t.Helper()
	pure, mattn = seekRaiseConns(t)
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
	if segments {
		stmts = append(stmts, `VACUUM`)
	}
	for _, s := range stmts {
		execBothConn(t, pure, mattn, "setup", s)
	}
	return pure, mattn
}

// seekRaiseSubWheres: C codes a correlated sub-select's term after every
// other (TERM_VARSELECT) and the rest in WHERE order, so with no index on value
// a raising term before "value = 7" reaches the malformed row, and one after
// it does not.
func seekRaiseSubWheres() []string {
	const (
		uncorrelated = `json_extract(j, '$') < (SELECT max(v) FROM other)`
		correlated   = `json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > i.value - 100)`
		key          = `value = 7`
	)
	var wheres []string
	bothOrders := func(raise, key string) {
		terms := seekRaiseTerms(raise, key)
		wheres = append(wheres, terms[:]...)
	}
	for _, raise := range []string{
		uncorrelated,
		correlated,
		`json_extract(j, '$') IN (SELECT v FROM other)`,
		`EXISTS (SELECT 1 FROM other WHERE json_extract(i.j, '$') < v)`,
		`json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > (SELECT max(v) FROM other WHERE v > i.value - 100) - 100)`,
	} {
		bothOrders(raise, key)
	}
	wheres = append(wheres,
		correlated+" AND "+uncorrelated+" AND "+key,
		uncorrelated+" AND "+correlated+" AND "+key)
	// The sub-select raises whichever row reaches it.
	bothOrders(`(SELECT json_extract('[', '$')) IS NULL`, key)
	bothOrders(`(SELECT json_extract('[', '$')) IS NULL`, `value = 9`)
	return wheres
}

func TestSeekRaiseSubselectVsC(t *testing.T) {
	for _, index := range []bool{false, true} {
		for _, segments := range []bool{false, true} {
			t.Run(fmt.Sprintf("index=%v/segments=%v", index, segments), func(t *testing.T) {
				pure, mattn := seekRaiseSubOpen(t, index, segments)
				var queries []string
				for _, where := range seekRaiseSubWheres() {
					queries = append(queries, `SELECT value FROM i WHERE `+where)
				}
				// The sub-select names a table of the query the EXISTS is in.
				for _, terms := range seekRaiseTerms(`json_extract(i.j, '$') < (SELECT max(v) FROM other WHERE v < o.lim)`, `i.value = o.want`) {
					queries = append(queries, `SELECT o.id FROM o WHERE EXISTS (SELECT 1 FROM i WHERE `+terms+`) ORDER BY o.id`)
				}
				for _, q := range queries {
					if got, want := seekRaiseOutcome(pure, q), seekRaiseOutcome(mattn, q); got != want {
						t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
					}
				}
			})
			t.Run(fmt.Sprintf("index=%v/segments=%v/writes", index, segments), func(t *testing.T) {
				for _, where := range seekRaiseSubWheres() {
					for _, q := range []string{`UPDATE i SET j = j || '' WHERE ` + where, `DELETE FROM i WHERE ` + where} {
						pure, mattn := seekRaiseSubOpen(t, index, segments)
						const dump = `SELECT rowid, value, j FROM i ORDER BY rowid`
						if got, want := seekRaiseWriteOutcome(pure, q, dump), seekRaiseWriteOutcome(mattn, q, dump); got != want {
							t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
						}
					}
				}
			})
		}
	}
}

// TestSeekRaiseTriggerVsC: in a trigger, a sub-select naming only NEW and OLD
// is not correlated; one that also names a table of the statement is.
func TestSeekRaiseTriggerVsC(t *testing.T) {
	const key = `value = 7`
	var wheres []string
	for _, raise := range []string{
		`json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > new.x - 100)`,
		`json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > new.x + i.value - 100)`,
	} {
		terms := seekRaiseTerms(raise, key)
		wheres = append(wheres, terms[:]...)
	}
	for _, index := range []bool{false, true} {
		for _, segments := range []bool{false, true} {
			t.Run(fmt.Sprintf("index=%v/segments=%v", index, segments), func(t *testing.T) {
				for _, where := range wheres {
					pure, mattn := seekRaiseSubOpen(t, index, segments)
					execBothConn(t, pure, mattn, "setup", `CREATE TRIGGER tr AFTER INSERT ON t BEGIN INSERT INTO log SELECT value FROM i WHERE `+where+`; END`)
					const q, dump = `INSERT INTO t VALUES (1)`, `SELECT a FROM log ORDER BY a`
					if got, want := seekRaiseWriteOutcome(pure, q, dump), seekRaiseWriteOutcome(mattn, q, dump); got != want {
						t.Errorf("%s\n  musql: %s\n  C:     %s", where, got, want)
					}
				}
			})
		}
	}
}

// seekRaiseGeneratedOpen opens both engines on a table with generated columns,
// VIRTUAL and STORED, one of them reading another.
func seekRaiseGeneratedOpen(t *testing.T, index, segments bool) (pure, mattn *sql.Conn) {
	t.Helper()
	pure, mattn = seekRaiseConns(t)
	stmts := []string{
		`CREATE TABLE i(value INT, j TEXT, g AS (json_extract(j, '$')) VIRTUAL, h AS (value * 2) VIRTUAL,
			g2 AS (g + 1) VIRTUAL, s AS (json_extract(j, '$') * 2) STORED)`,
		`INSERT INTO i(value, j) VALUES (1, '5'), (7, '1'), (7, '0'), (3, '2')`,
	}
	if index {
		stmts = append(stmts, `CREATE INDEX i_value ON i(value)`)
	}
	if segments {
		stmts = append(stmts, `VACUUM`)
	}
	for _, s := range stmts {
		execBothConn(t, pure, mattn, "setup", s)
	}
	return pure, mattn
}

// TestSeekRaiseGeneratedVsC: a term reading a VIRTUAL column computes it; a
// STORED column was computed when the row was written.
func TestSeekRaiseGeneratedVsC(t *testing.T) {
	const key = `value = 7`
	var wheres []string
	for _, raise := range []string{`g > 0`, `i.g > 0`, `g + 1 > 0`, `g2 > 0`, `h > 0`, `abs(h) > 0`, `s > 0`} {
		terms := seekRaiseTerms(raise, key)
		wheres = append(wheres, terms[:]...)
	}
	for _, index := range []bool{false, true} {
		for _, segments := range []bool{false, true} {
			t.Run(fmt.Sprintf("index=%v/segments=%v", index, segments), func(t *testing.T) {
				pure, mattn := seekRaiseGeneratedOpen(t, index, segments)
				for _, where := range wheres {
					q := `SELECT value, g, h, g2, s FROM i WHERE ` + where + ` ORDER BY rowid`
					if got, want := seekRaiseOutcome(pure, q), seekRaiseOutcome(mattn, q); got != want {
						t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
					}
				}
			})
			t.Run(fmt.Sprintf("index=%v/segments=%v/writes", index, segments), func(t *testing.T) {
				var writes []string
				for _, where := range wheres {
					writes = append(writes, `UPDATE i SET j = j || '' WHERE `+where, `DELETE FROM i WHERE `+where)
				}
				writes = append(writes,
					`INSERT INTO i(value, j) VALUES (9, 'bad')`,
					`UPDATE i SET j = 'bad' WHERE value = 3`,
					`UPDATE i SET j = 'bad' WHERE value = 9`)
				for _, q := range writes {
					pure, mattn := seekRaiseGeneratedOpen(t, index, segments)
					const dump = `SELECT rowid, value, j, g, s FROM i ORDER BY rowid`
					if got, want := seekRaiseWriteOutcome(pure, q, dump), seekRaiseWriteOutcome(mattn, q, dump); got != want {
						t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
					}
				}
			})
		}
	}
}

// TestSeekRaiseConsumedFirstVsC: C's index on (a, b) visits only a = 1 AND b =
// 2, so a seek on one column must test both equalities before the raising term.
func TestSeekRaiseConsumedFirstVsC(t *testing.T) {
	const keys = `a = 1 AND b = 2`
	var wheres []string
	for _, raise := range []string{
		`json_extract(j, '$') > 0`,
		`json_extract(j, '$') < (SELECT max(v) FROM other)`,
		`json_extract(j, '$') < (SELECT max(v) FROM other WHERE v > m.a - 100)`,
	} {
		wheres = append(wheres, raise+" AND "+keys, "a = 1 AND "+raise+" AND b = 2", keys+" AND "+raise)
	}
	for _, segments := range []bool{false, true} {
		open := func(t *testing.T) (pure, mattn *sql.Conn) {
			pure, mattn = seekRaiseConns(t)
			stmts := []string{
				`CREATE TABLE m(a INT, b INT, j TEXT)`,
				`CREATE TABLE other(v)`,
				`INSERT INTO other VALUES (5)`,
				`INSERT INTO m VALUES (1, 1, 'bad'), (1, 2, '1'), (2, 2, 'bad')`,
				`CREATE INDEX m_ab ON m(a, b)`,
			}
			if segments {
				stmts = append(stmts, `VACUUM`)
			}
			for _, s := range stmts {
				execBothConn(t, pure, mattn, "setup", s)
			}
			return pure, mattn
		}
		t.Run(fmt.Sprintf("segments=%v", segments), func(t *testing.T) {
			pure, mattn := open(t)
			for _, where := range wheres {
				q := `SELECT a, b FROM m WHERE ` + where
				if got, want := seekRaiseOutcome(pure, q), seekRaiseOutcome(mattn, q); got != want {
					t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
				}
			}
		})
		t.Run(fmt.Sprintf("segments=%v/writes", segments), func(t *testing.T) {
			for _, where := range wheres {
				for _, q := range []string{`UPDATE m SET j = j || '' WHERE ` + where, `DELETE FROM m WHERE ` + where} {
					pure, mattn := open(t)
					const dump = `SELECT rowid, a, b, j FROM m ORDER BY rowid`
					if got, want := seekRaiseWriteOutcome(pure, q, dump), seekRaiseWriteOutcome(mattn, q, dump); got != want {
						t.Errorf("%s\n  musql: %s\n  C:     %s", q, got, want)
					}
				}
			}
		})
	}
}
