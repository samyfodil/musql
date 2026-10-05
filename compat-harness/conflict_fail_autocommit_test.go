// This file gates OR FAIL conflict handling at the driver boundary. FAIL stops
// at the offending row but autocommit statements must still commit their changes.
package compat

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// cfaRun executes stmts on ONE connection of one driver and returns a
// per-statement transcript: "ok", "err", or the rows a SELECT produced. Error
// TEXT is deliberately not compared (the drivers word errors differently);
// whether a statement errored, and what the database then holds, are what
// matter here.
func cfaRun(t *testing.T, driver, dsn string, stmts []string) []string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("%s open: %v", driver, err)
	}
	db.SetMaxOpenConns(1) // one connection: autocommit and BEGIN both need session continuity
	defer db.Close()

	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "SELECT") {
			if _, eerr := db.Exec(s); eerr != nil {
				out = append(out, "err")
			} else {
				out = append(out, "ok")
			}
			continue
		}
		rows, qerr := db.Query(s)
		if qerr != nil {
			out = append(out, "err")
			continue
		}
		cols, cerr := rows.Columns()
		if cerr != nil {
			rows.Close()
			out = append(out, "err")
			continue
		}
		var b strings.Builder
		for rows.Next() {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if serr := rows.Scan(ptrs...); serr != nil {
				t.Fatalf("%s scan: %v", driver, serr)
			}
			b.WriteString("|")
			for _, c := range cells {
				fmt.Fprintf(&b, "%v,", c)
			}
		}
		rows.Close()
		out = append(out, b.String())
	}
	return out
}

// cfaDiffer runs stmts on both engines, each in its own file, and fails on the
// first transcript difference.
func cfaDiffer(t *testing.T, name string, stmts []string) {
	t.Helper()
	dir := t.TempDir()
	got := cfaRun(t, "sqlite", filepath.Join(dir, "musql.db"), stmts)
	want := cfaRun(t, "sqlite3", filepath.Join(dir, "cgo.db"), stmts)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%s] statement %d DIVERGES\n  sql:    %s\n  cgo:    %q\n  musql: %q\n  script: %v",
				name, i, stmts[i], want[i], got[i], stmts)
			return
		}
	}
}

// cfaConstraints are the constraint kinds an OR clause can resolve, each with
// a row list whose THIRD row violates it. They are separate cases because the
// write path reaches its conflict resolution from a different place for each,
// and because only the two that admit duplicate rows (NOT NULL, CHECK) can
// show a double-apply at all.
var cfaConstraints = []struct {
	name   string
	create string
	seed   string
	rows   []string
}{
	{"integer-primary-key", `CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES(30,'seed')`,
		[]string{`(10,'x')`, `(20,'y')`, `(30,'dup')`, `(40,'z')`}},
	{"unique-index", `CREATE TABLE t(a, b UNIQUE)`, `INSERT INTO t VALUES(0,'seed')`,
		[]string{`(10,'x')`, `(20,'y')`, `(30,'seed')`, `(40,'z')`}},
	{"not-null", `CREATE TABLE t(a, b NOT NULL)`, `INSERT INTO t VALUES(0,'seed')`,
		[]string{`(10,'x')`, `(20,'y')`, `(30,NULL)`, `(40,'z')`}},
	{"check", `CREATE TABLE t(a, b CHECK(b<>'bad'))`, `INSERT INTO t VALUES(0,'seed')`,
		[]string{`(10,'x')`, `(20,'y')`, `(30,'bad')`, `(40,'z')`}},
	{"multi-column-unique", `CREATE TABLE t(a, b, UNIQUE(a,b))`, `INSERT INTO t VALUES(30,'dup')`,
		[]string{`(10,'x')`, `(20,'y')`, `(30,'dup')`, `(40,'z')`}},
}

// cfaRead reads back what survived. Ordered by rowid so a row inserted twice
// is visible as a repeat rather than hidden by aggregation.
var cfaRead = []string{`SELECT a, b FROM t ORDER BY rowid`}

func TestConflictFailKeepsAutocommitRows(t *testing.T) {
	for _, c := range cfaConstraints {
		for _, verb := range []string{"FAIL", "ABORT", "ROLLBACK", "IGNORE", "REPLACE"} {
			for _, txn := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/txn=%v", c.name, verb, txn)
				stmts := []string{c.create, c.seed}
				if txn {
					stmts = append(stmts, `BEGIN`)
				}
				stmts = append(stmts, fmt.Sprintf(`INSERT OR %s INTO t VALUES%s`, verb, strings.Join(c.rows, ",")))
				if txn {
					stmts = append(stmts, `COMMIT`)
				}
				stmts = append(stmts, cfaRead...)
				cfaDiffer(t, name, stmts)
			}
		}
	}
}

// TestConflictFailKeepsAutocommitRowsOtherStatements covers the other
// statement kinds that carry an OR clause: UPDATE resolves its conflicts on a
// different path from INSERT, and INSERT..SELECT is a third.
func TestConflictFailKeepsAutocommitRowsOtherStatements(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`,
		`INSERT INTO t VALUES(1,'one'),(2,'two'),(3,'three'),(4,'four')`,
		`CREATE TABLE src(a,b)`,
		`INSERT INTO src VALUES(10,'x'),(20,'y'),(3,'dup'),(40,'z')`,
	}
	for _, verb := range []string{"FAIL", "ABORT", "ROLLBACK", "IGNORE"} {
		for _, stmt := range []string{
			`UPDATE OR %s t SET a=a+2`,
			`UPDATE OR %s t SET b='u' WHERE a<3`,
			`INSERT OR %s INTO t SELECT a,b FROM src`,
			`INSERT OR %s INTO t(a,b) SELECT a,b FROM src ORDER BY a`,
		} {
			for _, txn := range []bool{false, true} {
				s := fmt.Sprintf(stmt, verb)
				stmts := append([]string{}, setup...)
				if txn {
					stmts = append(stmts, `BEGIN`)
				}
				stmts = append(stmts, s)
				if txn {
					stmts = append(stmts, `COMMIT`)
				}
				stmts = append(stmts, cfaRead...)
				cfaDiffer(t, fmt.Sprintf("%s/txn=%v", s, txn), stmts)
			}
		}
	}
}

// TestConflictFailWithTriggersFuzz randomizes the script, because FAIL's rule
// is about WHAT SURVIVES, and that depends on how far the statement got before
// the offending row: which row conflicts, how many precede it, whether a
// trigger wrote to ANOTHER table on the way (those writes are the part only
// the statement snapshot can see), and whether the whole thing sits inside a
// transaction.
func TestConflictFailWithTriggersFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260802))
	verbs := []string{"FAIL", "ABORT", "ROLLBACK", "IGNORE", "REPLACE"}
	for k := 0; k < 90; k++ {
		k := k
		name := fmt.Sprintf("f%02d", k)
		t.Run(name, func(t *testing.T) {
			stmts := []string{
				`CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)`,
				`CREATE TABLE log(m)`,
				`INSERT INTO t VALUES(100,'seed')`,
			}
			switch rng.Intn(4) {
			case 1:
				stmts = append(stmts, `CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('bi'||new.a); END`)
			case 2:
				stmts = append(stmts, `CREATE TRIGGER ai AFTER INSERT ON t BEGIN INSERT INTO log VALUES('ai'||new.a); END`)
			case 3:
				stmts = append(stmts,
					`CREATE TRIGGER bi BEFORE INSERT ON t BEGIN INSERT INTO log VALUES('bi'||new.a); END`,
					`CREATE TRIGGER ai AFTER INSERT ON t BEGIN INSERT INTO log VALUES('ai'||new.a); END`)
			}
			// The conflicting row sits at a random position, and conflicts
			// sometimes on the rowid and sometimes on the UNIQUE index -- two
			// different resolution points.
			n := 2 + rng.Intn(4)
			bad := rng.Intn(n)
			var rows []string
			for r := 0; r < n; r++ {
				switch {
				case r != bad:
					rows = append(rows, fmt.Sprintf(`(%d,'v%d')`, r+1, r+1))
				case rng.Intn(2) == 0:
					rows = append(rows, `(100,'rowid-dup')`)
				default:
					rows = append(rows, fmt.Sprintf(`(%d,'seed')`, 200+r))
				}
			}
			inTxn := rng.Intn(3) == 0
			if inTxn {
				stmts = append(stmts, `BEGIN`)
			}
			stmts = append(stmts, fmt.Sprintf(`INSERT OR %s INTO t VALUES%s`,
				verbs[rng.Intn(len(verbs))], strings.Join(rows, ",")))
			if inTxn {
				stmts = append(stmts, `COMMIT`)
			}
			stmts = append(stmts,
				`SELECT a, b FROM t ORDER BY rowid`,
				`SELECT m FROM log ORDER BY rowid`)
			cfaDiffer(t, name, stmts)
		})
	}
}
