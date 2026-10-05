package compat

// Tests harness semantics for DML: return types, execution counts, and state changes.

import (
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func r38dShow(t *testing.T, eng string, res []map[string]any) []string {
	t.Helper()
	out := make([]string, len(res))
	for i, r := range res {
		b, _ := json.Marshal(r)
		out[i] = string(b)
	}
	return out
}

func TestR38DHarnessSemanticsProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("r38d probe: full run")
	}

	// (1)+(2): a multi-row INSERT OR FAIL whose SECOND row violates UNIQUE.
	// After it, row 4 must be present and row 5 must not -- ONCE. If the engine
	// ran it twice the second attempt fails on row 4 instead, which is
	// state-idempotent here, so the trigger log is what counts executions: a
	// BEFORE INSERT trigger fires per attempted row, so log length is 2 for one
	// execution and 3 for two (the replay stops at the first row, which now
	// conflicts).
	partial := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE, c)`,
		`CREATE TABLE log(n)`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`,
		`CREATE TRIGGER tr BEFORE INSERT ON t BEGIN INSERT INTO log(n) VALUES(new.a); END`,
		`INSERT OR FAIL INTO t(a,b,c) VALUES(4,'q',40),(5,'y',50),(6,'r',60)`,
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT n FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes(), last_insert_rowid()`,
	}
	m := run(t, "musql", partial)
	c := run(t, "cgo", partial)
	for i := range partial {
		mb, _ := json.Marshal(m[i])
		cb, _ := json.Marshal(c[i])
		mark := "  "
		if string(mb) != string(cb) {
			mark = "!!"
		}
		t.Logf("%s [%d] %s\n      cgo: %s\n      mus: %s", mark, i, partial[i], cb, mb)
	}

	// (3): does a SELECT clobber changes()?
	chg := []string{
		`CREATE TABLE u(x)`,
		`INSERT INTO u VALUES(1),(2),(3)`,
		`SELECT changes()`,
		`SELECT count(*) FROM u`,
		`SELECT changes()`,
		`PRAGMA integrity_check`,
		`SELECT changes()`,
	}
	mc := run(t, "musql", chg)
	cc := run(t, "cgo", chg)
	t.Logf("changes-after-select cgo: %v", r38dShow(t, "cgo", cc))
	t.Logf("changes-after-select mus: %v", r38dShow(t, "musql", mc))

	// (4): last_insert_rowid() and changes() after a FAILED insert.
	fail := []string{
		`CREATE TABLE v(k INTEGER PRIMARY KEY, w)`,
		`INSERT INTO v VALUES(1,'a')`,
		`INSERT INTO v VALUES(1,'b')`,
		`SELECT changes(), last_insert_rowid(), total_changes()`,
	}
	mf := run(t, "musql", fail)
	cf := run(t, "cgo", fail)
	t.Logf("after-failed-insert cgo: %v", r38dShow(t, "cgo", cf))
	t.Logf("after-failed-insert mus: %v", r38dShow(t, "musql", mf))
}
