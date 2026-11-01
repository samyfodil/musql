package compat

// Composite-key seeks against C SQLite: the EXPLAIN QUERY PLAN line C words for
// each shape, and the rows, with every key bound and then written as a literal.
// The differ cannot bind a parameter, so these open both drivers directly.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/driver"
)

// multiColSeekSchema covers the ways a term may or may not drive an index
// column: composite and UNIQUE keys, NOCASE and DESC columns, partial and
// WITHOUT ROWID indexes, a collation not the column's, and a key over columns
// of no affinity holding every class.
func multiColSeekSchema() []string {
	s := []string{
		`CREATE TABLE c(tbl TEXT, pk BLOB, col TEXT, v, PRIMARY KEY(tbl, pk, col))`,
		`CREATE TABLE o(site TEXT, seq INTEGER, hlc INTEGER, PRIMARY KEY(site, seq))`,
		`CREATE TABLE e(a INTEGER, b TEXT COLLATE NOCASE, r REAL, v)`,
		`CREATE UNIQUE INDEX e_ab ON e(a, b)`,
		`CREATE TABLE m(k TEXT PRIMARY KEY, v)`,
		`CREATE TABLE d(a, b, c)`,
		`CREATE INDEX d_ab ON d(a DESC, b)`,
		`CREATE TABLE p(a, b, c)`,
		`CREATE INDEX p_ab ON p(a, b) WHERE c > 0`,
		`CREATE TABLE w(a TEXT, b INTEGER, c, PRIMARY KEY(a, b)) WITHOUT ROWID`,
		`CREATE TABLE n(a TEXT COLLATE NOCASE, b TEXT, v, PRIMARY KEY(a, b))`,
		`CREATE TABLE k(a TEXT, b TEXT, v)`,
		`CREATE INDEX k_ab ON k(a COLLATE NOCASE, b)`,
		`CREATE TABLE y(a, b, c, UNIQUE(a, b))`,
	}
	ins := func(f string, args ...any) { s = append(s, fmt.Sprintf(f, args...)) }
	for i := 0; i < 600; i++ {
		tbl := "t"
		if i%10 == 0 {
			tbl = "u"
		}
		ins(`INSERT INTO c VALUES('%s', x'%04x', 'c%d', %d)`, tbl, i/5, i%5, i)
	}
	ins(`INSERT INTO c VALUES('t', x'7071', 'c1', 7777)`)
	for i := 0; i < 450; i++ {
		ins(`INSERT INTO o VALUES('s%d', %d, %d)`, i%3, i/3, 1000-i)
	}
	for i := 0; i < 400; i++ {
		ins(`INSERT INTO e VALUES(%d, 'Name%d', %d.5, %d)`, i/4, i%4, i%7, i)
	}
	for i := 0; i < 200; i++ {
		ins(`INSERT INTO m VALUES('k%04d', %d)`, (i*37)%200, i)
		ins(`INSERT INTO d VALUES(%d, %d, %d)`, i%20, i/20, i)
		ins(`INSERT INTO p VALUES(%d, %d, %d)`, i%10, i%7, i%3-1)
		ins(`INSERT INTO w VALUES('w%d', %d, %d)`, i%5, i, i*2)
		key := "KEY"
		if i%2 == 1 {
			key = "key"
		}
		ins(`INSERT INTO n VALUES('%s%d', 'b%d', %d)`, key, i%10, i, i)
		ins(`INSERT INTO k VALUES('%s%d', 'b%d', %d)`, key, i%10, i%5, i)
	}
	vals := []string{"1", "'1'", "2.5", "NULL", "x'31'", "'abc'", "'ABC'"}
	for i := 0; i < len(vals)*len(vals); i++ {
		ins(`INSERT INTO y VALUES(%s, %s, %d)`, vals[i%len(vals)], vals[i/len(vals)], i)
	}
	return s
}

// multiColSeekCase is one query with its keys bound. eqp is false for a shape
// whose plan line this engine does not reproduce yet, named in why.
type multiColSeekCase struct {
	sql  string
	args []any
	eqp  bool
	why  string
}

func multiColSeekCases() []multiColSeekCase {
	b := func(x ...byte) []byte { return x }
	q := func(sql string, args ...any) multiColSeekCase {
		return multiColSeekCase{sql: sql, args: args, eqp: true}
	}
	no := func(why, sql string, args ...any) multiColSeekCase {
		return multiColSeekCase{sql: sql, args: args, why: why}
	}
	return []multiColSeekCase{
		// The replication clock and op log.
		q(`SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, "t", b(0, 1), "c1"),
		q(`SELECT col, v FROM c WHERE tbl=? AND pk=?`, "t", b(0, 1)),
		q(`SELECT col FROM c WHERE tbl=? AND pk=?`, "u", b(0, 2)),
		q(`SELECT v FROM c WHERE pk=? AND col=? AND tbl=?`, b(0, 3), "c3", "t"),
		q(`SELECT v FROM c WHERE tbl=? AND pk=? AND col>?`, "t", b(0, 4), "c1"),
		q(`SELECT v FROM c WHERE tbl=? AND pk=? AND col>=? AND col<?`, "t", b(0, 5), "c1", "c4"),
		q(`SELECT v FROM c WHERE tbl=? AND pk=? AND col<>?`, "t", b(0, 6), "c0"),
		q(`SELECT count(*) FROM c WHERE tbl=? AND pk=?`, "t", b(0, 7)),
		q(`SELECT group_concat(v) FROM c WHERE tbl=? AND pk=?`, "t", b(0, 8)),
		q(`SELECT max(v) FROM c WHERE tbl=? AND pk=?`, "t", b(0, 9)),
		q(`SELECT hlc FROM o WHERE site=? AND seq=?`, "s1", 7),
		q(`SELECT 1 FROM o WHERE site=? AND seq=?`, "s2", 9),
		q(`SELECT seq, hlc FROM o WHERE site=? AND seq>?`, "s0", 140),
		q(`SELECT v FROM m WHERE k=?`, "k0017"),
		// The item is named as written: its alias, or its schema and name.
		q(`SELECT x.v FROM c AS x WHERE x.tbl=? AND x.pk=?`, "t", b(0, 18)),
		q(`SELECT v FROM main.c WHERE tbl=? AND pk=?`, "t", b(0, 19)),
		q(`SELECT v FROM C WHERE TBL=? AND PK=?`, "t", b(0, 21)),
		no("an ATTACHed table is not planned by the port", `SELECT v FROM aux.c WHERE tbl=? AND pk=? AND col=?`, "t", b(0, 22), "c1"),
		q(`SELECT v FROM c WHERE tbl=? AND pk=? LIMIT 2`, "t", b(0, 15)),
		q(`SELECT DISTINCT col FROM c WHERE tbl=? AND pk=?`, "t", b(0, 16)),
		q(`SELECT hlc FROM o WHERE seq>?`, 140),
		q(`SELECT v FROM c WHERE tbl=? AND pk IN (?, ?)`, "t", b(0, 1), b(0, 2)),
		// Keys of another class than the column: an INTEGER bound to TEXT, TEXT
		// to BLOB, a REAL and a numeric TEXT to INTEGER, a BLOB to TEXT, NULL.
		q(`SELECT v FROM c WHERE tbl=? AND pk=?`, 1, b(0, 1)),
		q(`SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, "t", "pq", "c1"),
		q(`SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, "t", b('p', 'q'), "c1"),
		q(`SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, "t", b(0, 1), b('c', '1')),
		q(`SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, "t", b(0, 1), nil),
		q(`SELECT hlc FROM o WHERE site=? AND seq=?`, "s1", "7"),
		q(`SELECT hlc FROM o WHERE site=? AND seq=?`, "s1", 7.0),
		q(`SELECT hlc FROM o WHERE site=? AND seq=?`, "s1", 7.5),
		q(`SELECT v FROM e WHERE a=? AND b=?`, 5, "NAME1"),
		q(`SELECT v FROM e WHERE a=? AND b=?`, "5", "name2"),
		q(`SELECT v FROM e WHERE a=? AND b=?`, 5.0, "Name3"),
		q(`SELECT v FROM e WHERE a=? AND b>?`, 7, "name1"),
		q(`SELECT v FROM e WHERE a=? AND r=?`, 6, 1.5),
		q(`SELECT typeof(c), c FROM y WHERE a=? AND b=?`, 1, "abc"),
		q(`SELECT typeof(c), c FROM y WHERE a=? AND b=?`, "1", 1),
		q(`SELECT typeof(c), c FROM y WHERE a=? AND b=?`, b('1'), 2.5),
		q(`SELECT typeof(c), c FROM y WHERE a=? AND b=?`, "ABC", "abc"),
		// IS, IS NULL, IN on a prefix column.
		q(`SELECT v FROM c WHERE tbl IS ? AND pk IS ?`, "t", b(0, 10)),
		q(`SELECT typeof(c), c FROM y WHERE a=? AND b IS NULL`, "abc"),
		q(`SELECT typeof(c), c FROM y WHERE a IS ? AND b=?`, nil, "abc"),
		q(`SELECT v FROM c WHERE tbl IN ('t', 'u') AND pk=?`, b(0, 11)),
		q(`SELECT hlc FROM o WHERE site IN (?, ?) AND seq=?`, "s0", "s2", 12),
		// Collation: a NOCASE key column, an index whose collation is not its
		// column's (the term cannot use it unless it says COLLATE NOCASE), and
		// a comparison under another sequence than the index's.
		q(`SELECT v FROM n WHERE a=? AND b=?`, "key3", "b13"),
		q(`SELECT v FROM n WHERE a=? AND b=?`, "KEY4", "b14"),
		q(`SELECT v FROM n WHERE a=? COLLATE BINARY AND b=?`, "key5", "b15"),
		q(`SELECT v FROM k WHERE a=? AND b=?`, "key3", "b3"),
		q(`SELECT v FROM k WHERE a=? COLLATE NOCASE AND b=?`, "KEY3", "b3"),
		q(`SELECT v FROM c WHERE tbl=? COLLATE NOCASE AND pk=?`, "T", b(0, 12)),
		// A DESC index column and a partial index.
		q(`SELECT c FROM d WHERE a=? AND b=?`, 3, 4),
		q(`SELECT c FROM d WHERE a=? AND b>?`, 3, 4),
		q(`SELECT c FROM p WHERE a=? AND b=? AND c>0`, 3, 3),
		q(`SELECT c FROM p WHERE a=? AND b=?`, 3, 3),
		// What this engine does not word yet.
		no("a WITHOUT ROWID table is not planned by the port", `SELECT c FROM w WHERE a=? AND b=?`, "w2", 7),
		no("a WITHOUT ROWID table is not planned by the port", `SELECT c FROM w WHERE a=?`, "w3"),
		no("IS NULL alone emits no seek", `SELECT c FROM y WHERE a IS NULL`),
		no("a full scan of a covering index is not worded", `SELECT seq FROM o WHERE seq>?`, 140),
		no("a range alone emits no seek", `SELECT seq FROM o WHERE site>?`, "s1"),
		no("an ORDER BY scan emits no seek", `SELECT seq, hlc FROM o WHERE site=? AND seq>? ORDER BY seq`, "s1", 130),
		no("an ORDER BY scan emits no seek", `SELECT seq FROM o WHERE site=? AND seq>? ORDER BY seq DESC LIMIT 3`, "s2", 100),
		no("an ORDER BY scan emits no seek", `SELECT col, v FROM c WHERE tbl=? AND pk=? ORDER BY col`, "t", b(0, 13)),
		no("a GROUP BY scan emits no seek", `SELECT col, count(*) FROM c WHERE tbl=? AND pk=? GROUP BY col`, "t", b(0, 14)),
		no("a GROUP BY scan emits no seek", `SELECT tbl, count(*) FROM c WHERE tbl=? AND pk=? GROUP BY tbl`, "u", b(0, 20)),
		no("an ORDER BY scan emits no seek", `SELECT v FROM c WHERE tbl=? AND pk=? ORDER BY v`, "t", b(0, 17)),
	}
}

// sqlLiteral writes a bound argument as the literal that means the same.
func sqlLiteral(a any) string {
	switch v := a.(type) {
	case nil:
		return "NULL"
	case int:
		return strconv.Itoa(v)
	case float64:
		s := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(s, ".e") {
			s += ".0"
		}
		return s
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case []byte:
		return "x'" + hex.EncodeToString(v) + "'"
	}
	panic(fmt.Sprintf("sqlLiteral: %T", a))
}

// withLiterals is sqlText with each ? replaced by its argument's literal.
func withLiterals(sqlText string, args []any) string {
	var out strings.Builder
	for _, r := range sqlText {
		if r == '?' && len(args) > 0 {
			out.WriteString(sqlLiteral(args[0]))
			args = args[1:]
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// planDetails is EXPLAIN QUERY PLAN's detail column for q.
func planDetails(t *testing.T, c *sql.Conn, q string, args ...any) []string {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN %s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// multiColSeekConns opens one connection to each engine over the schema, with
// a database of the same schema ATTACHed as aux.
func multiColSeekConns(t *testing.T) (pure, mattn *sql.Conn) {
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
		if _, err := c.ExecContext(ctx, fmt.Sprintf("ATTACH DATABASE '%s' AS aux", filepath.Join(dir, tag+".aux.db"))); err != nil {
			t.Fatal(err)
		}
		return c
	}
	pure, mattn = open(driver.DriverName, "pure"), open("sqlite3", "mattn")
	execBothConn(t, pure, mattn, "schema", `BEGIN`)
	for _, s := range multiColSeekSchema() {
		execBothConn(t, pure, mattn, "schema", s)
	}
	execBothConn(t, pure, mattn, "aux", `CREATE TABLE aux.c(tbl TEXT, pk BLOB, col TEXT, v, PRIMARY KEY(tbl, pk, col))`)
	for i := 0; i < 300; i++ {
		execBothConn(t, pure, mattn, "aux", `INSERT INTO aux.c VALUES(?, ?, ?, ?)`, "t", []byte{0, byte(i / 3)}, fmt.Sprintf("c%d", i%3), -i)
	}
	execBothConn(t, pure, mattn, "schema", `COMMIT`)
	return pure, mattn
}

// TestMultiColumnSeekVsC: every composite-key shape answers C's rows, bound and
// literal, and words C's EXPLAIN QUERY PLAN line.
func TestMultiColumnSeekVsC(t *testing.T) {
	pure, mattn := multiColSeekConns(t)
	// Over the log the schema was written to, then over segments.
	for _, stage := range []string{"log", "segments"} {
		if stage == "segments" {
			execBothConn(t, pure, mattn, stage, `VACUUM`)
		}
		for _, tc := range multiColSeekCases() {
			lit := withLiterals(tc.sql, tc.args)
			queryBothConn(t, pure, mattn, stage+" bound", tc.sql, tc.args...)
			queryBothConn(t, pure, mattn, stage+" literal", lit)
			for _, form := range [][2]any{{tc.sql, tc.args}, {lit, []any(nil)}} {
				q, args := form[0].(string), form[1].([]any)
				got, want := planDetails(t, pure, q, args...), planDetails(t, mattn, q, args...)
				if strings.Join(got, "\n") == strings.Join(want, "\n") {
					if !tc.eqp {
						t.Logf("%s: now words C's plan (%q); drop it from the exceptions", q, want)
					}
					continue
				}
				if tc.eqp {
					t.Errorf("%s: EXPLAIN QUERY PLAN %s\n  musql: %q\n  C:     %q", stage, q, got, want)
				}
			}
		}
	}
}

// TestMultiColumnSeekReruns: one prepared statement per shape, re-run with
// different keys -- found, missing, of another class -- answers C's rows each
// time, so nothing about the first run's keys is kept.
func TestMultiColumnSeekReruns(t *testing.T) {
	pure, mattn := multiColSeekConns(t)
	ctx := context.Background()
	for _, tc := range []struct {
		sql  string
		args func(i int) []any
	}{
		{`SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, func(i int) []any {
			tbl := []any{"t", "u", 1, nil}[i%4]
			return []any{tbl, []byte{0, byte(i % 130)}, fmt.Sprintf("c%d", i%6)}
		}},
		{`SELECT col, v FROM c WHERE tbl=? AND pk=?`, func(i int) []any {
			return []any{[]string{"t", "u"}[i%2], []byte{0, byte(i)}}
		}},
		{`SELECT hlc FROM o WHERE site=? AND seq=?`, func(i int) []any {
			return []any{fmt.Sprintf("s%d", i%4), []any{i, fmt.Sprint(i), float64(i), float64(i) + 0.5}[i%4]}
		}},
		{`SELECT v FROM e WHERE a=? AND b=?`, func(i int) []any {
			return []any{i % 120, []string{"name0", "NAME1", "Name2", "nAmE3", "x"}[i%5]}
		}},
		{`SELECT v FROM aux.c WHERE tbl=? AND pk=? AND col=?`, func(i int) []any {
			return []any{"t", []byte{0, byte(i % 110)}, fmt.Sprintf("c%d", i%4)}
		}},
	} {
		ps, err := pure.PrepareContext(ctx, tc.sql)
		if err != nil {
			t.Fatal(err)
		}
		ms, err := mattn.PrepareContext(ctx, tc.sql)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 60; i++ {
			args := tc.args(i)
			pr, err := ps.QueryContext(ctx, args...)
			if err != nil {
				t.Fatalf("%s %v: %v", tc.sql, args, err)
			}
			pCols, pOut := collectRows(t, pr)
			pr.Close()
			mr, err := ms.QueryContext(ctx, args...)
			if err != nil {
				t.Fatalf("%s %v: %v", tc.sql, args, err)
			}
			mCols, mOut := collectRows(t, mr)
			mr.Close()
			if ok, reason := queryResultsMatch(pCols, pOut, mCols, mOut, true); !ok {
				t.Errorf("%s %v: %s\n  musql: %v\n  C:     %v", tc.sql, args, reason, pOut, mOut)
			}
		}
		ps.Close()
		ms.Close()
	}
}

// TestMultiColumnSeekInTransaction: rows an open transaction wrote, moved and
// deleted are found -- or not -- by a composite-key seek exactly as C finds
// them, before and after the transaction ends.
func TestMultiColumnSeekInTransaction(t *testing.T) {
	pure, mattn := multiColSeekConns(t)
	reads := func(stage string) {
		for _, tc := range multiColSeekCases() {
			queryBothConn(t, pure, mattn, stage, tc.sql, tc.args...)
		}
		queryBothConn(t, pure, mattn, stage, `SELECT v FROM c WHERE tbl=? AND pk=? AND col=?`, "t", []byte{9, 9}, "c9")
		queryBothConn(t, pure, mattn, stage, `SELECT col, v FROM c WHERE tbl=? AND pk=?`, "t", []byte{9, 9})
		queryBothConn(t, pure, mattn, stage, `SELECT hlc FROM o WHERE site=? AND seq=?`, "s9", 1)
	}
	for _, end := range []string{"ROLLBACK", "COMMIT"} {
		execBothConn(t, pure, mattn, "begin", `BEGIN`)
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO c VALUES(?, ?, ?, ?)`, []any{"t", []byte{9, 9}, "c9", 9001}},
			{`INSERT INTO c VALUES(?, ?, ?, ?)`, []any{"t", []byte{0, 1}, "c8", 9002}},
			{`UPDATE c SET col = ? WHERE tbl = ? AND pk = ? AND col = ?`, []any{"c7", "t", []byte{0, 3}, "c3"}},
			{`DELETE FROM c WHERE tbl = ? AND pk = ? AND col = ?`, []any{"t", []byte{0, 1}, "c1"}},
			{`INSERT INTO o VALUES(?, ?, ?)`, []any{"s9", 1, 5}},
			{`UPDATE o SET seq = seq + 1000 WHERE site = ? AND seq = ?`, []any{"s1", 7}},
			{`INSERT INTO e VALUES(?, ?, ?, ?)`, []any{5, "NAME9", 1.5, 9003}},
			{`DELETE FROM e WHERE a = ? AND b = ?`, []any{5, "name1"}},
			{`INSERT OR REPLACE INTO c VALUES(?, ?, ?, ?)`, []any{"t", []byte{0, 4}, "c1", 9004}},
		} {
			execBothConn(t, pure, mattn, "write", s.sql, s.args...)
		}
		reads("in " + end)
		execBothConn(t, pure, mattn, "end", end)
		reads("after " + end)
	}
}
