package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestInsertParamsPaths: prepared INSERTs with bound parameters -- the shape a
// bulk load runs, whose program reads each parameter straight into its
// column's register and applies the row's affinities in one instruction --
// against C SQLite, through every path that then reads the row: BEFORE and
// AFTER triggers logging NEW, RETURNING, upserts, OR REPLACE and OR IGNORE, a
// UNIQUE index, generated and DEFAULT columns, an explicit or NULL INTEGER
// PRIMARY KEY, SAVEPOINT / ROLLBACK TO / RELEASE and ROLLBACK, and reads inside
// the open transaction. Every statement's result or error, and the tables at
// the end, must match.
func TestInsertParamsPaths(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a INTEGER, b TEXT DEFAULT 'dflt', c REAL, d BLOB, e NUMERIC, g INTEGER GENERATED ALWAYS AS (a*2) VIRTUAL, u INTEGER UNIQUE)`,
		`CREATE TABLE log(ev TEXT, id INTEGER, a, b, c, u)`,
		`CREATE TRIGGER tb BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('before', NEW.id, NEW.a, NEW.b, NEW.c, NEW.u); END`,
		`CREATE TRIGGER ta AFTER INSERT ON t BEGIN INSERT INTO log VALUES('after', NEW.id, NEW.a, NEW.b, NEW.g, NEW.u); END`,
		`CREATE TABLE p(x, y, z)`,
	}
	c, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetMaxOpenConns(1)
	m, err := sql.Open("sqlite", t.TempDir()+"/ins.db")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetMaxOpenConns(1)
	for _, s := range setup {
		if _, err := c.Exec(s); err != nil {
			t.Fatalf("cgo setup %s: %v", s, err)
		}
		if _, err := m.Exec(s); err != nil {
			t.Fatalf("musql setup %s: %v", s, err)
		}
	}
	rng := rand.New(rand.NewSource(17))
	val := func() any {
		switch rng.Intn(9) {
		case 0:
			return nil
		case 1:
			return fmt.Sprint(rng.Intn(50)) // text that INTEGER/NUMERIC affinity converts
		case 2:
			return []string{"x", "", "12.5", " 7", "abc"}[rng.Intn(5)]
		case 3:
			return float64(rng.Intn(40)) / 4
		case 4:
			return []byte{byte(rng.Intn(256)), 1, 2}
		default:
			return int64(rng.Intn(60) - 5)
		}
	}
	cols := []string{"id", "a", "b", "c", "d", "e", "u"}
	type stmtShape struct {
		sql   string
		nargs int
		query bool
	}
	shape := func() stmtShape {
		perm := rng.Perm(len(cols))
		k := 1 + rng.Intn(len(cols))
		names := make([]string, k)
		qs := make([]string, k)
		for i := 0; i < k; i++ {
			names[i] = cols[perm[i]]
			qs[i] = "?"
			if rng.Intn(6) == 0 {
				qs[i] = "?+0" // not a bare parameter: copied into its column
			}
		}
		verb := []string{"INSERT", "INSERT", "INSERT OR REPLACE", "INSERT OR IGNORE", "REPLACE"}[rng.Intn(5)]
		s := fmt.Sprintf("%s INTO t(%s) VALUES(%s)", verb, strings.Join(names, ","), strings.Join(qs, ","))
		n := k
		if rng.Intn(5) == 0 && verb == "INSERT" {
			if rng.Intn(2) == 0 {
				s += " ON CONFLICT DO NOTHING"
			} else {
				s += " ON CONFLICT(u) DO UPDATE SET a = excluded.a + 1, b = 'up'"
			}
		}
		query := false
		if rng.Intn(4) == 0 {
			s += " RETURNING id, a, b, g, u"
			query = true
		}
		return stmtShape{s, n, query}
	}
	run := func(db *sql.DB, tx *sql.Tx, st stmtShape, args []any) string {
		if st.query {
			var rows *sql.Rows
			var err error
			if tx != nil {
				rows, err = tx.Query(st.sql, args...)
			} else {
				rows, err = db.Query(st.sql, args...)
			}
			if err != nil {
				return "ERR:" + err.Error()
			}
			defer rows.Close()
			var sb strings.Builder
			for rows.Next() {
				vals := make([]any, 5)
				ptrs := make([]any, 5)
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					return "SCANERR:" + err.Error()
				}
				fmt.Fprintf(&sb, "%v;", vals)
			}
			if rows.Err() != nil {
				return "ERR:" + rows.Err().Error()
			}
			return sb.String()
		}
		var err error
		if tx != nil {
			_, err = tx.Exec(st.sql, args...)
		} else {
			_, err = db.Exec(st.sql, args...)
		}
		if err != nil {
			return "ERR:" + err.Error()
		}
		return "ok"
	}
	read := func(db *sql.DB, tx *sql.Tx, q string) string {
		var rows *sql.Rows
		var err error
		if tx != nil {
			rows, err = tx.Query(q)
		} else {
			rows, err = db.Query(q)
		}
		if err != nil {
			return "ERR:" + err.Error()
		}
		defer rows.Close()
		cs, _ := rows.Columns()
		var sb strings.Builder
		for rows.Next() {
			vals := make([]any, len(cs))
			ptrs := make([]any, len(cs))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return "SCANERR:" + err.Error()
			}
			for _, v := range vals {
				if b, ok := v.([]byte); ok {
					v = fmt.Sprintf("x'%x'", b)
				}
				fmt.Fprintf(&sb, "%v|", v)
			}
			sb.WriteString(";")
		}
		return sb.String()
	}
	const readAll = `SELECT id, a, typeof(a), b, typeof(b), c, typeof(c), d, e, typeof(e), g, u FROM t ORDER BY id`
	for round := range 40 {
		inTx := rng.Intn(3) != 0
		var ct, mt *sql.Tx
		if inTx {
			if ct, err = c.Begin(); err != nil {
				t.Fatal(err)
			}
			if mt, err = m.Begin(); err != nil {
				t.Fatal(err)
			}
		}
		savepoint := inTx && rng.Intn(2) == 0
		n := 5 + rng.Intn(400)
		for i := range n {
			if savepoint && i == n/3 {
				for _, x := range []*sql.Tx{ct, mt} {
					if _, err := x.Exec("SAVEPOINT sp"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if savepoint && i == 2*n/3 {
				q := []string{"ROLLBACK TO sp", "RELEASE sp"}[rng.Intn(2)]
				for _, x := range []*sql.Tx{ct, mt} {
					if _, err := x.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
				savepoint = false
			}
			st := shape()
			args := make([]any, st.nargs)
			for k := range args {
				args[k] = val()
			}
			cr, mr := run(c, ct, st, args), run(m, mt, st, args)
			if cr != mr {
				t.Fatalf("round %d stmt %d: %s %v\n  cgo:   %s\n  musql: %s", round, i, st.sql, args, cr, mr)
			}
			if inTx && i%97 == 50 {
				// A read inside the open transaction sees its own rows.
				if a, b := read(c, ct, readAll), read(m, mt, readAll); a != b {
					t.Fatalf("round %d: read inside the transaction\n  cgo:   %s\n  musql: %s", round, a, b)
				}
			}
		}
		if inTx {
			if rng.Intn(4) == 0 {
				ct.Rollback()
				mt.Rollback()
			} else {
				if err := ct.Commit(); err != nil {
					t.Fatal(err)
				}
				if err := mt.Commit(); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, q := range []string{readAll, `SELECT * FROM log ORDER BY rowid`, `SELECT count(*), sum(a), max(id) FROM t`} {
			if a, b := read(c, nil, q), read(m, nil, q); a != b {
				t.Fatalf("round %d: %s\n  cgo:   %s\n  musql: %s", round, q, a, b)
			}
		}
		if rng.Intn(5) == 0 {
			for _, db := range []*sql.DB{c, m} {
				if _, err := db.Exec(`DELETE FROM t WHERE id % 3 = 0`); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
