package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// A HELD SESSION LOADS A TABLE WHEN A STATEMENT FIRST TOUCHES IT, not every table
// before every statement (segSource.unloaded). And the laziness must be
// invisible to a rollback: a table loaded, written and rolled back inside one
// transaction answers its committed rows afterwards.
func TestSessionLoadsOnlyTheTablesItReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.musq")
	sess, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE a(id INTEGER PRIMARY KEY, v INTEGER)`,
		`CREATE TABLE b(id INTEGER PRIMARY KEY, v INTEGER)`,
		`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<100) INSERT INTO a SELECT x, x FROM c`,
		`INSERT INTO b SELECT id, v FROM a`,
	} {
		if e := sess.Exec(s); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := sess.Commit(); e != nil {
		t.Fatal(e)
	}
	if e := sess.Close(); e != nil {
		t.Fatal(e)
	}

	sess, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Discard()
	loaded := func(name string) bool { return sess.findTableMeta(name).loaded }
	one := func(q string) Value {
		t.Helper()
		_, rows, qerr := sess.Query(q, nil)
		if qerr != nil {
			t.Fatalf("%s: %v", q, qerr)
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Fatalf("%s: %v", q, rows)
		}
		return rows[0][0]
	}

	if loaded("a") || loaded("b") {
		t.Fatal("a table was loaded before any statement ran")
	}
	one(`SELECT 1`)
	if loaded("a") || loaded("b") {
		t.Fatal("SELECT 1 loaded a table")
	}
	if v := one(`SELECT v FROM a WHERE id = 7`); v.I != 7 {
		t.Fatalf("a.v at 7 = %v", v)
	}
	if !loaded("a") || loaded("b") {
		t.Fatalf("after reading a: a loaded %v, b loaded %v; want only a", loaded("a"), loaded("b"))
	}

	// The INSERT's own SELECT loads b through a source built while b was
	// unloaded, and then writes b; without the cache drop in liveRows, the SELECT
	// after ROLLBACK is served from that source's copy of the rolled-back rows.
	for _, s := range []string{
		`BEGIN`,
		`INSERT INTO b SELECT id + 1000, v FROM b WHERE id <= 50`,
		`ROLLBACK`,
	} {
		if e := sess.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	if v := one(`SELECT sum(v) FROM b WHERE id > 0`); v.I != 5050 {
		t.Fatalf("sum(b.v) after ROLLBACK = %v, want 5050", v)
	}
	// A rowid lookup reads the row store (sum above is answered from the
	// committed column blocks), and 1003 is a row only the rolled-back INSERT had.
	if _, rows, qerr := sess.Query(`SELECT v FROM b WHERE id = 1003`, nil); qerr != nil || len(rows) != 0 {
		t.Fatalf("b at 1003 after ROLLBACK: %v, %v; want no row", rows, qerr)
	}
	if e := sess.Close(); e != nil {
		t.Fatal(e)
	}

	// A FROZEN source (a write's subquery pager) built before the statement
	// loaded b must still read b as it was: the INSERT loads b to write it, and
	// its SELECT must not see the rows it is inserting.
	sess, err = OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Discard()
	if loaded("b") {
		t.Fatal("b loaded before any statement ran")
	}
	if e := sess.Exec(`INSERT INTO b SELECT id + 2000, v FROM b WHERE id > 0`); e != nil {
		t.Fatal(e)
	}
	for id, want := range map[int]int{2001: 1, 2100: 1, 4001: 0, 1: 1} {
		if _, rows, qerr := sess.Query(fmt.Sprintf(`SELECT v FROM b WHERE id = %d`, id), nil); qerr != nil || len(rows) != want {
			t.Fatalf("b at %d after INSERT ... SELECT: %v, %v; want %d rows", id, rows, qerr, want)
		}
	}
}
