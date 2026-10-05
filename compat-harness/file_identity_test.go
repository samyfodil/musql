package compat

import (
	"database/sql"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// fileIdentical maps scripts whose exports should be byte-identical to C SQLite's
// VACUUM INTO output.
var fileIdentical = map[string]bool{
	"01-create": true, "02-insert3": true, "03-index": true, "04-index-after": true,
	"05-tail-drop": true, "06-nontail-drop": true, "07-view-trigger": true,
	"09-rename": true, "10-1000rows": true, "11-delete-half": true, "12-update-grow": true,
	"13-drop-index": true, "14-txn-batch": true, "15-autoinc": true, "16-unique": true,
	"17-without-rowid": true, "18-drop-recreate": true, "19-vacuum": true, "20-page-size": true,
	"21-ctas": true, "22-fts4": true, "23-fts4-rename": true, "24-temp": true, "25-savepoint": true, "26-rowid-gap": true,
}

// TestFileIdentityMatchesCSQLite verifies file format identity with C SQLite.
func TestFileIdentityMatchesCSQLite(t *testing.T) {
	checkFileIdentity(t, fileIdentical)
}

func fileIdentityScripts() map[string][]string {
	rows := func(n int, tmpl string) []string {
		var out []string
		for i := 1; i <= n; i++ {
			out = append(out, fmt.Sprintf(tmpl, i, i, i))
		}
		return out
	}
	return map[string][]string{
		"01-create":        {`CREATE TABLE t(a)`},
		"02-insert3":       {`CREATE TABLE t(a)`, `INSERT INTO t VALUES (1),(2),(3)`},
		"03-index":         {`CREATE TABLE t(a)`, `CREATE INDEX i ON t(a)`, `INSERT INTO t VALUES (1),(2),(3)`},
		"04-index-after":   {`CREATE TABLE t(a)`, `INSERT INTO t VALUES (3),(1),(2)`, `CREATE INDEX i ON t(a)`},
		"05-tail-drop":     {`CREATE TABLE a(x)`, `CREATE TABLE b(x)`, `DROP TABLE b`, `CREATE TABLE c(x)`},
		"06-nontail-drop":  {`CREATE TABLE a(x)`, `CREATE TABLE b(x)`, `DROP TABLE a`, `CREATE TABLE c(x)`},
		"07-view-trigger":  {`CREATE TABLE t(a)`, `CREATE VIEW v AS SELECT a FROM t`, `CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END`},
		"08-alter-add":     {`CREATE TABLE t(a)`, `INSERT INTO t VALUES (1)`, `ALTER TABLE t ADD COLUMN b DEFAULT 5`},
		"09-rename":        {`CREATE TABLE t(a)`, `ALTER TABLE t RENAME TO u`},
		"10-1000rows":      append([]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`}, rows(1000, `INSERT INTO t VALUES (%d, printf('%%.*c', 50 + %d %% 40, 'x'))-- %d`)...),
		"11-delete-half":   append(append([]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`}, rows(500, `INSERT INTO t VALUES (%d, printf('%%.*c', 50 + %d %% 40, 'x'))-- %d`)...), `DELETE FROM t WHERE a % 2 = 0`),
		"12-update-grow":   append(append([]string{`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`}, rows(300, `INSERT INTO t VALUES (%d, 'x')-- %d %d`)...), `UPDATE t SET b = printf('%.*c', 200, 'y') WHERE a % 3 = 0`),
		"13-drop-index":    {`CREATE TABLE t(a)`, `CREATE INDEX i ON t(a)`, `INSERT INTO t VALUES (1),(2)`, `DROP INDEX i`},
		"14-txn-batch":     append(append([]string{`CREATE TABLE t(a, b)`, `BEGIN`}, rows(400, `INSERT INTO t VALUES (%d, x'' || %d)-- %d`)...), `COMMIT`),
		"15-autoinc":       {`CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b)`, `INSERT INTO t(b) VALUES (1),(2)`},
		"16-unique":        {`CREATE TABLE t(a UNIQUE, b PRIMARY KEY)`, `INSERT INTO t VALUES (1,2),(3,4)`},
		"17-without-rowid": {`CREATE TABLE t(a PRIMARY KEY, b) WITHOUT ROWID`, `INSERT INTO t VALUES (1,2),(3,4)`},
		"18-drop-recreate": append(append([]string{`CREATE TABLE a(x)`, `CREATE TABLE b(x)`}, rows(200, `INSERT INTO a VALUES (printf('%%.*c', 300, 'z') || %d)-- %d %d`)...), `DROP TABLE a`, `CREATE TABLE c(y)`, `INSERT INTO c VALUES (1)`),
		"20-page-size":     append([]string{`PRAGMA page_size = 1024`, `CREATE TABLE t(a)`}, rows(50, `INSERT INTO t VALUES (printf('%%.*c', 300, 'q') || %d)-- %d %d`)...),
		"19-vacuum":        append(append([]string{`CREATE TABLE t(a)`}, rows(200, `INSERT INTO t VALUES (printf('%%.*c', 300, 'z') || %d)-- %d %d`)...), `DELETE FROM t`, `VACUUM`),
		"21-ctas":          {`CREATE TABLE t(a, b)`, `INSERT INTO t VALUES (1, 'x'), (2, 'y')`, `CREATE TABLE u AS SELECT b, a FROM t`},
		"22-fts4":          {`CREATE VIRTUAL TABLE f USING fts4(a)`, `INSERT INTO f(a) VALUES ('one two'), ('three')`},
		"23-fts4-rename":   {`CREATE VIRTUAL TABLE f USING fts4(a)`, `INSERT INTO f(a) VALUES ('one two')`, `ALTER TABLE f RENAME TO g`, `INSERT INTO g(a) VALUES ('three')`},
		"24-temp":          {`CREATE TABLE m(a)`, `CREATE TEMP TABLE tt(z)`, `INSERT INTO tt VALUES (1)`, `INSERT INTO m VALUES (2)`},
		"25-savepoint":     {`CREATE TABLE t(a)`, `INSERT INTO t VALUES (1)`, `SAVEPOINT s`, `INSERT INTO t VALUES (2)`, `ROLLBACK TO s`, `RELEASE s`, `INSERT INTO t VALUES (3)`},
		"26-rowid-gap":     {`CREATE TABLE t(a INTEGER PRIMARY KEY, b)`, `INSERT INTO t VALUES (1, 'a'), (2, 'b'), (3, 'c')`, `DELETE FROM t WHERE a = 2`, `INSERT INTO t(b) VALUES ('d')`},
	}
}

func checkFileIdentity(t *testing.T, identical map[string]bool) {
	scripts := fileIdentityScripts()
	for _, name := range slices.Sorted(maps.Keys(scripts)) {
		dir := t.TempDir()
		pp, cp := filepath.Join(dir, "p.musq"), filepath.Join(dir, "c.db")
		pure, err := sql.Open("sqlite", pp)
		if err != nil {
			t.Fatal(err)
		}
		pure.SetMaxOpenConns(1)
		cgo, err := sql.Open("sqlite3", cp)
		if err != nil {
			t.Fatal(err)
		}
		cgo.SetMaxOpenConns(1)
		bad := ""
		for _, s := range scripts[name] {
			_, perr := pure.Exec(s)
			_, cerr := cgo.Exec(s)
			if (perr == nil) != (cerr == nil) {
				bad = fmt.Sprintf(" EXEC MISMATCH %q pure=%v cgo=%v", s, perr, cerr)
				break
			}
		}
		cv := filepath.Join(dir, "c-vacuum-into.db")
		if _, verr := cgo.Exec(`VACUUM INTO '` + cv + `'`); verr != nil {
			t.Fatalf("%s: C VACUUM INTO: %v", name, verr)
		}
		if _, verr := pure.Exec(`VACUUM`); bad == "" && verr != nil {
			t.Fatalf("%s: musql VACUUM: %v", name, verr)
		}
		pure.Close()
		cgo.Close()
		pb, perr := os.ReadFile(exportedForOracle(t, pp))
		cb, cerr := os.ReadFile(cv)
		if perr != nil || cerr != nil {
			t.Fatalf("%s: read files: %v, %v", name, perr, cerr)
		}
		for _, b := range [][]byte{pb, cb} {
			if len(b) >= 44 {
				copy(b[40:44], []byte{0, 0, 0, 0})
			}
		}
		ps := 4096
		var diffs []string
		n := max(len(pb), len(cb)) / ps
		for pg := 0; pg < n; pg++ {
			lo, hi := pg*ps, (pg+1)*ps
			var a, b []byte
			if hi <= len(pb) {
				a = pb[lo:hi]
			}
			if hi <= len(cb) {
				b = cb[lo:hi]
			}
			if a == nil || b == nil {
				diffs = append(diffs, fmt.Sprintf("p%d:missing", pg+1))
				continue
			}
			var offs []string
			for i := range a {
				if a[i] != b[i] {
					offs = append(offs, fmt.Sprint(i))
					if len(offs) >= 4 {
						break
					}
				}
			}
			if len(offs) > 0 {
				diffs = append(diffs, fmt.Sprintf("p%d@%s", pg+1, strings.Join(offs, ",")))
			}
		}
		status := "IDENTICAL"
		if len(diffs) > 0 {
			status = fmt.Sprintf("DIFF pages(pure=%d cgo=%d) %s", len(pb)/ps, len(cb)/ps, strings.Join(diffs[:min(len(diffs), 8)], " "))
		}
		switch {
		case bad != "":
			t.Errorf("%s:%s", name, bad)
		case identical[name] && len(diffs) > 0:
			t.Errorf("%s: no longer byte-identical to C SQLite: %s", name, status)
		case !identical[name] && len(diffs) == 0:
			t.Logf("%s: now byte-identical -- add it to fileIdentical", name)
		case len(diffs) > 0:
			t.Logf("%s: %s", name, status)
		}
	}
}
