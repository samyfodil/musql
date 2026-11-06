package engine

import (
	"fmt"
	"testing"
)

// TestSetTriggersEnabled is SQLITE_DBCONFIG_ENABLE_TRIGGER: off, a write fires
// only TEMP triggers -- those on a main table and a TEMP table's own -- and
// none of the database's (trigger.c, triggersReallyExist). A statement run
// before the switch fires what the switch says when it runs again.
func TestSetTriggersEnabled(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	exec := func(stmts ...string) {
		t.Helper()
		for _, s := range stmts {
			if err := db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	exec("CREATE TABLE log(s)",
		"CREATE TABLE m(x)",
		"CREATE TRIGGER main_tr AFTER INSERT ON m BEGIN INSERT INTO log VALUES('main'); END",
		"CREATE TEMP TRIGGER temp_tr AFTER INSERT ON m BEGIN INSERT INTO log VALUES('temp'); END",
		"CREATE TEMP TABLE tt(x)",
		"CREATE TEMP TRIGGER tt_tr AFTER INSERT ON tt BEGIN INSERT INTO log VALUES('temp table'); END",
		"INSERT INTO m VALUES(1)")
	db.SetTriggersEnabled(false)
	exec("INSERT INTO m VALUES(1)", "INSERT INTO tt VALUES(1)")
	db.SetTriggersEnabled(true)
	exec("INSERT INTO m VALUES(1)")

	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := p.Query("SELECT s FROM log ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, string(r[0].S))
	}
	if s, want := fmt.Sprint(got), "[temp main temp temp table temp main]"; s != want {
		t.Fatalf("fired %s, want %s", s, want)
	}
}
