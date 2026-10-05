package driver_test

// Catalog-scoping rules for INDEXED BY and UNIQUE constraints are tested
// with hand-coded oracle expectations to avoid triggering pre-existing
// structural issues that appear only in harness tests.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/samyfodil/musql/driver"
)

// step is one statement and its oracle outcome.
type step struct {
	sql  string
	err  bool
	rows []string
}

func ok(s string) step                { return step{sql: s} }
func errs(s string) step              { return step{sql: s, err: true} }
func rows(s string, r ...string) step { return step{sql: s, rows: r} }

// runSteps executes every step on ONE connection (statements share session and
// TEMP-catalog state, exactly as the harness worker does) and checks each
// outcome against the recorded oracle answer.
func runSteps(t *testing.T, name string, steps []step) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for i, s := range steps {
		got, qerr := queryAll(db, s.sql)
		switch {
		case s.err && qerr == nil:
			t.Errorf("[%s] step %d %q: ran and answered %v, but 3.53.3 REFUSES it", name, i, s.sql, got)
		case !s.err && qerr != nil:
			t.Errorf("[%s] step %d %q: %v, but 3.53.3 RUNS it", name, i, s.sql, qerr)
		case !s.err && qerr == nil && strings.Join(got, ",") != strings.Join(s.rows, ","):
			t.Errorf("[%s] step %d %q: answered %v, 3.53.3 answers %v", name, i, s.sql, got, s.rows)
		}
	}
}

// queryAll runs a statement and renders rows as pipe-joined strings.
func queryAll(db *sql.DB, sqlText string) ([]string, error) {
	rs, err := db.Query(sqlText)
	if err != nil {
		if _, eerr := db.Exec(sqlText); eerr != nil {
			return nil, eerr
		}
		return nil, nil
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rs.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = renderCell(c)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func renderCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case int64:
		return itoa(x)
	case string:
		return x
	}
	return "?"
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestIndexedByHintResolvesInTheTargetCatalog verifies that INDEXED BY hints
// resolve in the target table's own catalog.
func TestIndexedByHintResolvesInTheTargetCatalog(t *testing.T) {
	runSteps(t, "the name half", []step{
		ok(`CREATE TABLE t(a,b)`),
		ok(`CREATE INDEX onlymain ON t(a)`),
		ok(`CREATE TEMP TABLE t(a,b)`),
		ok(`INSERT INTO main.t VALUES(1,'x')`),
		ok(`INSERT INTO temp.t VALUES(2,'y')`),
		errs(`SELECT a FROM temp.t INDEXED BY onlymain`),
		errs(`DELETE FROM temp.t INDEXED BY onlymain`),
		errs(`UPDATE temp.t INDEXED BY onlymain SET b='q'`),
		rows(`SELECT a FROM main.t INDEXED BY onlymain`, "1"),
		rows(`SELECT a,b FROM temp.t ORDER BY a`, "2|y"),
		rows(`SELECT a,b FROM main.t ORDER BY a`, "1|x"),
	})
	// Mirror case: TEMP index not visible to MAIN.
	runSteps(t, "the mirror", []step{
		ok(`CREATE TABLE u(a,b)`),
		ok(`CREATE TEMP TABLE u(a,b)`),
		ok(`CREATE INDEX temp.onlytemp ON u(a)`),
		ok(`INSERT INTO main.u VALUES(1,'x')`),
		ok(`INSERT INTO temp.u VALUES(2,'y')`),
		errs(`SELECT a FROM main.u INDEXED BY onlytemp`),
		errs(`DELETE FROM main.u INDEXED BY onlytemp`),
		rows(`SELECT a FROM temp.u INDEXED BY onlytemp`, "2"),
		rows(`SELECT a,b FROM main.u ORDER BY a`, "1|x"),
	})
}

// TestUniqueIndexIsEnforcedInItsOwnCatalog verifies that UNIQUE constraints
// are enforced only within their own catalog.
func TestUniqueIndexIsEnforcedInItsOwnCatalog(t *testing.T) {
	runSteps(t, "temp is not enforced against main's UNIQUE", []step{
		ok(`CREATE TABLE t(a UNIQUE, b)`),
		ok(`INSERT INTO t VALUES(1,'m1')`),
		ok(`CREATE TEMP TABLE t(a, b)`),
		ok(`INSERT INTO temp.t VALUES(1,'x')`),
		ok(`INSERT INTO temp.t VALUES(1,'y')`),
		ok(`UPDATE temp.t SET a=1`),
		rows(`SELECT a,b FROM temp.t ORDER BY b`, "1|x", "1|y"),
		rows(`SELECT a,b FROM main.t ORDER BY b`, "1|m1"),
	})
	runSteps(t, "main is not enforced against temp's UNIQUE", []step{
		ok(`CREATE TABLE u(a, b)`),
		ok(`CREATE TEMP TABLE u(a UNIQUE, b)`),
		ok(`INSERT INTO main.u VALUES(2,'p')`),
		ok(`INSERT INTO main.u VALUES(2,'q')`),
		ok(`INSERT INTO temp.u VALUES(3,'p')`),
		errs(`INSERT INTO temp.u VALUES(3,'q')`),
		rows(`SELECT a,b FROM main.u ORDER BY b`, "2|p", "2|q"),
		rows(`SELECT a,b FROM temp.u ORDER BY b`, "3|p"),
	})
}
