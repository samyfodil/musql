package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/musql/driver"
)

// TestUniqueProbeIndexMatchesOracle verifies UNIQUE index probe hashing.
func TestUniqueProbeIndexMatchesOracle(t *testing.T) {
	dir := t.TempDir()
	mq, err := sql.Open(driver.DriverName, filepath.Join(dir, "m.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer mq.Close()
	mq.SetMaxOpenConns(1)
	c, err := sql.Open("sqlite3", filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetMaxOpenConns(1)

	schema := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, a TEXT COLLATE NOCASE UNIQUE, b TEXT COLLATE RTRIM, n, x, y)`,
		`CREATE UNIQUE INDEX tb ON t(b)`,
		`CREATE UNIQUE INDEX tn ON t(n)`,
		`CREATE UNIQUE INDEX txy ON t(x, y)`,
		// A WITHOUT ROWID table's store is keyed by a synthetic id, and its PRIMARY
		// KEY is itself a unique index -- the probe must hold for both.
		`CREATE TABLE w(k TEXT COLLATE NOCASE PRIMARY KEY, u UNIQUE, v) WITHOUT ROWID`,
	}
	for _, s := range schema {
		if _, err := mq.Exec(s); err != nil {
			t.Fatalf("musql %s: %v", s, err)
		}
		if _, err := c.Exec(s); err != nil {
			t.Fatalf("oracle %s: %v", s, err)
		}
	}

	rng := rand.New(rand.NewSource(20260928))
	text := func() string {
		base := []string{"ab", "AB", "Ab", "ab ", "ab  ", "AB ", "x", "X", "1", "1.0", "é", "É"}[rng.Intn(12)]
		return "'" + base + "'"
	}
	num := func() string {
		return []string{"1", "1.0", "1.5", "2", "2.0", "-0.0", "0", "'1'", "x'01'", "NULL", "9007199254740993", "9007199254740992.0"}[rng.Intn(12)]
	}
	small := func() string { return []string{"1", "2", "1.0", "'1'", "NULL"}[rng.Intn(5)] }
	val := func(col string) string {
		switch col {
		case "a", "b":
			if rng.Intn(6) == 0 {
				return "NULL"
			}
			return text()
		case "n":
			return num()
		}
		return small()
	}
	cols := []string{"a", "b", "n", "x", "y"}
	stmt := func() string {
		if rng.Intn(4) == 0 {
			switch rng.Intn(5) {
			case 0, 1:
				or := []string{"", "OR IGNORE ", "OR REPLACE "}[rng.Intn(3)]
				return fmt.Sprintf("INSERT %sINTO w(k,u,v) VALUES(%s,%s,%d)", or, text(), small(), rng.Intn(9))
			case 2:
				or := []string{"", "OR IGNORE ", "OR REPLACE "}[rng.Intn(3)]
				return fmt.Sprintf("UPDATE %sw SET u = %s WHERE v %% 3 = %d", or, small(), rng.Intn(3))
			case 3:
				or := []string{"", "OR IGNORE ", "OR REPLACE "}[rng.Intn(3)]
				return fmt.Sprintf("UPDATE %sw SET k = %s WHERE v = %d", or, text(), rng.Intn(9))
			}
			return fmt.Sprintf("DELETE FROM w WHERE v = %d", rng.Intn(9))
		}
		switch k := rng.Intn(20); {
		case k < 8:
			or := []string{"", "OR IGNORE ", "OR REPLACE "}[rng.Intn(3)]
			return fmt.Sprintf("INSERT %sINTO t(a,b,n,x,y) VALUES(%s,%s,%s,%s,%s)", or, val("a"), val("b"), val("n"), val("x"), val("y"))
		case k < 12:
			col := cols[rng.Intn(len(cols))]
			or := []string{"", "OR IGNORE ", "OR REPLACE "}[rng.Intn(3)]
			return fmt.Sprintf("UPDATE %st SET %s = %s WHERE id %% 5 = %d", or, col, val(col), rng.Intn(5))
		case k < 14:
			return fmt.Sprintf("DELETE FROM t WHERE id %% 7 = %d", rng.Intn(7))
		case k < 16:
			return fmt.Sprintf("INSERT INTO t(a,b,n,x,y) VALUES(%s,%s,%s,%s,%s) ON CONFLICT(a) DO UPDATE SET n = excluded.n", val("a"), val("b"), val("n"), val("x"), val("y"))
		case k < 17:
			return "SAVEPOINT s"
		case k < 18:
			return "ROLLBACK TO s"
		case k < 19:
			return "RELEASE s"
		}
		return fmt.Sprintf("INSERT OR IGNORE INTO t(a,b,n,x,y) SELECT upper(a), b || ' ', n, y, x FROM t WHERE id %% 3 = %d", rng.Intn(3))
	}
	dump := func(db *sql.DB) string {
		var sb strings.Builder
		for _, q := range []string{
			`SELECT id, quote(a), quote(b), quote(n), quote(x), quote(y) FROM t ORDER BY id`,
			`SELECT quote(k), quote(u), quote(v), '', '', '' FROM w ORDER BY k COLLATE BINARY`,
		} {
			rows, err := db.Query(q)
			if err != nil {
				return "error: " + err.Error()
			}
			for rows.Next() {
				var c [6]string
				rows.Scan(&c[0], &c[1], &c[2], &c[3], &c[4], &c[5])
				fmt.Fprintf(&sb, "%s\n", strings.Join(c[:], "|"))
			}
			rows.Close()
			sb.WriteString("--\n")
		}
		return sb.String()
	}
	for i := 0; i < 3000; i++ {
		s := stmt()
		_, merr := mq.Exec(s)
		_, cerr := c.Exec(s)
		if (merr == nil) != (cerr == nil) {
			t.Fatalf("stmt %d %s\n  musql: %v\n  oracle: %v\n--- musql\n%s--- oracle\n%s", i, s, merr, cerr, dump(mq), dump(c))
		}
		if i%100 == 99 {
			if m, o := dump(mq), dump(c); m != o {
				t.Fatalf("after stmt %d (%s) the tables differ\n--- musql\n%s--- oracle\n%s", i, s, m, o)
			}
		}
	}
	if m, o := dump(mq), dump(c); m != o {
		t.Fatalf("final tables differ\n--- musql\n%s--- oracle\n%s", m, o)
	}
}
