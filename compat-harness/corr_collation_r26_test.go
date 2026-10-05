package compat

// Gates that a correlated column's declared collation survives substitution
// into expressions. Each case asserts the oracle acts and this engine either
// declines or answers exactly the same rows.
import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

var ccR26Setup = []string{
	`CREATE TABLE c1(a TEXT COLLATE NOCASE, b TEXT)`,
	`INSERT INTO c1 VALUES('abc','x'),('def','y')`,
}

// ccR26Cases are statements with correlated columns; each is asserted to either
// decline or answer exactly what the oracle answered.
var ccR26Cases = []struct {
	name, stmt string
	knownWrong bool
}{
	{"update-exists-compound", `UPDATE c1 SET b='HIT' WHERE EXISTS (SELECT c1.a INTERSECT SELECT 'ABC')`, false},
	{"update-exists-correlated", `UPDATE c1 SET b='HIT' WHERE EXISTS (SELECT 1 FROM c1 AS z WHERE c1.a='ABC')`, false},
	{"update-scalar-subquery", `UPDATE c1 SET b='HIT' WHERE (SELECT c1.a='ABC')`, false},
	{"update-set-from-subquery", `UPDATE c1 SET b=(SELECT CASE WHEN c1.a='ABC' THEN 'HIT' ELSE b END)`, false},
	{"delete-scalar-subquery", `DELETE FROM c1 WHERE (SELECT c1.a='ABC')`, false},
}

func TestCorrelatedColumnKeepsItsCollation(t *testing.T) {
	for _, tc := range ccR26Cases {
		t.Run(tc.name, func(t *testing.T) {
			edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer edb.Discard()
			cdb, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "c.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer cdb.Close()
			cdb.SetMaxOpenConns(1)
			for _, s := range ccR26Setup {
				if e := edb.Exec(s); e != nil {
					t.Fatalf("engine setup %s: %v", s, e)
				}
				if _, e := cdb.Exec(s); e != nil {
					t.Fatalf("cgo setup %s: %v", s, e)
				}
			}

			if _, e := cdb.Exec(tc.stmt); e != nil {
				t.Fatalf("premise gone: cgo rejected %q: %v", tc.stmt, e)
			}
			cRows, _ := cdb.Query(`SELECT a,b FROM c1 ORDER BY a`)
			var after [][]string
			for cRows.Next() {
				var a, b any
				cRows.Scan(&a, &b)
				after = append(after, []string{tclNormalizeCGOCell(a), tclNormalizeCGOCell(b)})
			}
			cRows.Close()
			var before = [][]string{{"T:abc", "T:x"}, {"T:def", "T:y"}}
			acted := len(after) != len(before)
			for i := range after {
				if i < len(before) && (after[i][0] != before[i][0] || after[i][1] != before[i][1]) {
					acted = true
				}
			}
			if !acted {
				t.Fatalf("premise gone: the oracle no longer changes anything for %q (got %v)", tc.stmt, after)
			}

			eerr := edb.Exec(tc.stmt)
			if eerr != nil && tc.knownWrong {
				t.Fatalf("%q now DECLINES -- clear its knownWrong flag", tc.stmt)
			}
			if e := eerr; e == nil {
				_, gRows, qerr, panicked, pv := tclSafeGoQuery(edb, `SELECT a,b FROM c1 ORDER BY a`)
				if panicked {
					t.Fatalf("engine PANICKED reading back %q: %v", tc.stmt, pv)
				}
				if qerr != nil {
					t.Fatalf("engine read-back failed for %q: %v", tc.stmt, qerr)
				}
				got := gRows
				cols := []string{"c0", "c1"}
				ok, reason := queryResultsMatch(cols, got, cols, after, true)
				if !ok && !tc.knownWrong {
					t.Fatalf("engine ACCEPTED %q and answered differently -- the correlated column lost its collation: %s\n  engine: %v\n  cgo:    %v", tc.stmt, reason, got, after)
				}
				if ok && tc.knownWrong {
					t.Fatalf("%q now MATCHES the oracle -- clear its knownWrong flag", tc.stmt)
				}
				if !ok {
					t.Logf("KNOWN WRONG (write path loses the correlated column's collation): %s\n  engine: %v\n  cgo:    %v", reason, got, after)
				}
			}
		})
	}
}
