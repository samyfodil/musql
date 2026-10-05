package driver

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/internal/filelock"
)

// Tests that TEMP objects survive session rebuilds from refresh and poison.
func seedTempObjects(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, s := range []string{
		`CREATE TABLE m(id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TEMP TABLE tt(id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE INDEX tti ON tt(v)`, // a temp table's index is a TEMP index
		`CREATE TEMP VIEW tv AS SELECT v FROM tt`,
		`CREATE TEMP TRIGGER tg AFTER INSERT ON tt BEGIN UPDATE tt SET v = v || '!' WHERE id = NEW.id; END`,
		`INSERT INTO tt VALUES(1,'x')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// checkTempObjects asserts every seeded temp object still answers.
func checkTempObjects(t *testing.T, db *sql.DB, when string) {
	t.Helper()
	var v string
	if err := db.QueryRow(`SELECT v FROM tt WHERE id=1`).Scan(&v); err != nil {
		t.Fatalf("%s: temp table: %v", when, err)
	}
	if v != "x!" { // the temp TRIGGER ran on the insert
		t.Fatalf("%s: temp table row = %q, want \"x!\"", when, v)
	}
	if err := db.QueryRow(`SELECT v FROM tv`).Scan(&v); err != nil {
		t.Fatalf("%s: temp view: %v", when, err)
	}
	if err := db.QueryRow(`SELECT v FROM tt INDEXED BY tti WHERE v='x!'`).Scan(&v); err != nil {
		t.Fatalf("%s: temp index: %v", when, err)
	}
	// And it is still WRITABLE, which the catalog alone does not prove.
	if _, err := db.Exec(`INSERT INTO tt VALUES(2,'y')`); err != nil {
		t.Fatalf("%s: temp insert: %v", when, err)
	}
	if err := db.QueryRow(`SELECT v FROM tt WHERE id=2`).Scan(&v); err != nil {
		t.Fatalf("%s: temp table after insert: %v", when, err)
	}
	if _, err := db.Exec(`DELETE FROM tt WHERE id=2`); err != nil {
		t.Fatalf("%s: temp delete: %v", when, err)
	}
}

func TestTempSurvivesAnotherConnectionsCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp-refresh.musq")
	a, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetMaxOpenConns(1) // one held session, so the refresh lands on the seeded one
	seedTempObjects(t, a)
	checkTempObjects(t, a, "before")

	b, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Exec(`INSERT INTO m VALUES(9,'other')`); err != nil {
		t.Fatal(err)
	}
	checkTempObjects(t, a, "after another connection committed")
	// main's half really was re-read -- otherwise this proves nothing about the
	// rebuild having happened at all.
	var got string
	if err := a.QueryRow(`SELECT v FROM m WHERE id=9`).Scan(&got); err != nil {
		t.Fatalf("main after the other connection committed: %v", err)
	}
}

func TestTempSurvivesABusyCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temp-poison.musq")
	a, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetMaxOpenConns(1)
	seedTempObjects(t, a)
	checkTempObjects(t, a, "before")

	// Another descriptor holds the lock a commit's append needs, so the next
	// statement's commit fails and poisons the session. See
	// TestSegmentBusyCommitLeavesNothingBehind.
	holder, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if ok, lerr := filelock.LockRange(holder, 0, 0, true); lerr != nil || !ok {
		t.Fatalf("holder: locking the lock file: ok=%v err=%v", ok, lerr)
	}
	oldTimeout := engine.BusyTimeout
	engine.BusyTimeout = 50 * time.Millisecond
	if _, err := a.Exec(`INSERT INTO m VALUES(1,'p')`); err == nil {
		t.Fatal("expected the commit to fail while the lock is held")
	}
	engine.BusyTimeout = oldTimeout
	filelock.UnlockRange(holder, 0, 0)

	checkTempObjects(t, a, "after a poisoned session was rebuilt")
}
