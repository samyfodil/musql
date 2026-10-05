package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestRowidRangeAllocation: with a rowid range set, an auto-assigned rowid is
// one past the largest rowid IN the range (or its start), whatever the table
// holds outside it -- including another range's rows above it -- and the
// range's own maximum is reused after a delete exactly as C reuses a table's.
// A rollback restores the cache with the rows; a full range is SQLITE_FULL.
func TestRowidRangeAllocation(t *testing.T) {
	n, err := Create(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	exec := func(s string) {
		t.Helper()
		if _, _, err := n.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	rowids := func() string {
		t.Helper()
		_, rows, err := n.Query(`SELECT group_concat(rowid) FROM (SELECT rowid FROM t ORDER BY rowid)`, nil)
		if err != nil {
			t.Fatal(err)
		}
		return valueText(rows[0][0])
	}
	exec(`CREATE TABLE t(v)`)
	exec(`INSERT INTO t VALUES('pre')`) // before the range: rowid 1
	n.SetRowidRange(100, 105)
	exec(`INSERT INTO t VALUES('a'),('b')`)
	exec(`INSERT INTO t(rowid, v) VALUES(900, 'other range')`) // explicit: untouched
	exec(`INSERT INTO t VALUES('c')`)
	if got, want := rowids(), "1,100,101,102,900"; got != want {
		t.Fatalf("rowids %s, want %s", got, want)
	}
	exec(`DELETE FROM t WHERE rowid = 102`)
	exec(`INSERT INTO t VALUES('d')`) // the range's max was deleted: reused, as C does
	if got, want := rowids(), "1,100,101,102,900"; got != want {
		t.Fatalf("after reusing the deleted max: rowids %s, want %s", got, want)
	}
	exec(`BEGIN`)
	exec(`INSERT INTO t VALUES('e')`) // 103
	exec(`ROLLBACK`)
	exec(`INSERT INTO t VALUES('f')`) // 103 again: the rollback took the cache with it
	if got, want := rowids(), "1,100,101,102,103,900"; got != want {
		t.Fatalf("after a rolled-back insert: rowids %s, want %s", got, want)
	}
	exec(`BEGIN`)
	exec(`DELETE FROM t`)             // a clear: its undo reinstates the whole map
	exec(`INSERT INTO t VALUES('e')`) // 100, into the emptied range
	exec(`ROLLBACK`)
	if got, want := rowids(), "1,100,101,102,103,900"; got != want {
		t.Fatalf("after a rolled-back clear: rowids %s, want %s", got, want)
	}
	exec(`INSERT INTO t VALUES('g')`) // 104, the last in [100,105) -- not 101 again
	_, _, ferr := n.ExecArgs(`INSERT INTO t VALUES('h')`, nil)
	if ferr == nil || !strings.Contains(ferr.Error(), "database or disk is full") {
		t.Fatalf("a full range: want SQLITE_FULL, got %v", ferr)
	}
}

// TestJournalDDL: under full capture a DDL statement on main is logged in its
// place among the row changes (a TEMP one too: the consumer decides); a failed
// one is not; and a ROLLBACK takes a journaled statement with it.
func TestJournalDDL(t *testing.T) {
	n, err := Create(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	db := n.DB
	db.EnableRowChangeCapture()
	for _, s := range []string{
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`ALTER TABLE t ADD COLUMN b`,
		`CREATE TABLE temp.x(a)`,
		`BEGIN`,
		`CREATE INDEX ti ON t(a)`,
		`ROLLBACK`,
	} {
		if _, _, err := n.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, _, err := n.ExecArgs(`CREATE TABLE t(a)`, nil); err == nil {
		t.Fatal("a duplicate CREATE TABLE succeeded")
	}
	var got []string
	for _, c := range db.TakeRowChanges() {
		if c.Kind == RowSchema {
			got = append(got, c.SQL)
		} else {
			got = append(got, fmt.Sprintf("row %d %s", c.Kind, c.Table))
		}
	}
	want := fmt.Sprintf("[CREATE TABLE t(a) row %d t ALTER TABLE t ADD COLUMN b CREATE TABLE temp.x(a)]", RowInsert)
	if g := fmt.Sprint(got); g != want {
		t.Fatalf("log %s\nwant %s", g, want)
	}
}

// TestAlterAddColumnDef: the definition text of an ADD COLUMN, verbatim, and
// nothing for any other statement.
func TestAlterAddColumnDef(t *testing.T) {
	for sql, want := range map[string]string{
		`ALTER TABLE t ADD COLUMN c TEXT DEFAULT 'x y'`:           `c TEXT DEFAULT 'x y'`,
		`alter table main."t" add  "c d" int not null default 3;`: `"c d" int not null default 3`,
		`ALTER TABLE t ADD c`:                                     `c`,
		`ALTER TABLE t ADD CONSTRAINT k CHECK(a>0)`:               ``,
		`ALTER TABLE t RENAME COLUMN a TO b`:                      ``,
		`CREATE TABLE t(a)`:                                       ``,
	} {
		got, ok := AlterAddColumnDef(sql)
		if got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", sql, got, ok, want)
		}
	}
}

// TestAlterRenameColumnQuoted: whether a RENAME COLUMN quoted its new name.
func TestAlterRenameColumnQuoted(t *testing.T) {
	for sql, want := range map[string][2]bool{
		`ALTER TABLE t RENAME COLUMN a TO b`:         {false, true},
		`ALTER TABLE t RENAME a TO "b"`:              {true, true},
		`alter table "t" rename column [a] to [b] ;`: {true, true},
		`ALTER TABLE t RENAME TO u`:                  {false, false},
		`ALTER TABLE t ADD COLUMN b`:                 {false, false},
	} {
		q, ok := AlterRenameColumnQuoted(sql)
		if q != want[0] || ok != want[1] {
			t.Errorf("%s: %v %v, want %v", sql, q, ok, want)
		}
	}
}

// TestAggregateUncorrelatedSubqueryAnswers: an uncorrelated subquery in a
// whole-table aggregate's select list reads no row of the aggregate, so which
// row its anchor is cannot change it -- it was declined as if it could, over an
// indexed table. A correlated one still is guarded.
func TestAggregateUncorrelatedSubqueryAnswers(t *testing.T) {
	n, err := Create(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	for _, s := range []string{
		`CREATE TABLE memos(id TEXT PRIMARY KEY, body TEXT)`,
		`INSERT INTO memos VALUES('a', 'x'), ('b', 'y')`,
		`CREATE TABLE t2(x)`,
		`INSERT INTO t2 VALUES('z')`,
	} {
		if _, _, err := n.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	_, rows, err := n.Query(`SELECT group_concat(body, ',') || '/' || (SELECT group_concat(x) FROM t2) FROM (SELECT body FROM memos ORDER BY id)`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := valueText(rows[0][0]); got != "x,y/z" {
		t.Fatalf("got %q, want x,y/z", got)
	}
	// z > both bodies, so C answers 'z' whichever row is the anchor.
	_, rows, err = n.Query(`SELECT count(*), (SELECT group_concat(x) FROM t2 WHERE x > memos.body) FROM memos`, nil)
	if err == nil && (len(rows) != 1 || rows[0][0].I != 2 || valueText(rows[0][1]) != "z") {
		t.Fatalf("correlated: %v", rows)
	}
}

// TestJournalCTASOrder: CREATE TABLE ... AS SELECT inserts its rows before the
// statement ends; its journal entry still comes first, and its rows take the
// connection's range like any insert's.
func TestJournalCTASOrder(t *testing.T) {
	n, err := Create(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Discard()
	db := n.DB
	for _, s := range []string{`CREATE TABLE t(a)`, `INSERT INTO t VALUES(1), (2)`} {
		if _, _, err := n.ExecArgs(s, nil); err != nil {
			t.Fatal(err)
		}
	}
	db.EnableRowChangeCapture()
	db.TakeRowChanges()
	db.SetRowidRange(100, 200)
	if _, _, err := n.ExecArgs(`CREATE TABLE c AS SELECT a FROM t`, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ch := range db.TakeRowChanges() {
		if ch.Kind == RowSchema {
			got = append(got, "ddl")
		} else {
			got = append(got, fmt.Sprintf("%s@%d", ch.Table, ch.Rowid))
		}
	}
	if g := fmt.Sprint(got); g != "[ddl c@100 c@101]" {
		t.Fatalf("log %s, want [ddl c@100 c@101]", g)
	}
}
