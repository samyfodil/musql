package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPlainDerivedColsMatchTheCompile: where plainDerivedCols names a body's
// columns without compiling it, the names are the compiled body's, and the
// shapes it cannot name go to the compile.
func TestPlainDerivedColsMatchTheCompile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.musq")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE t(Alpha INTEGER PRIMARY KEY, beta TEXT, "Gam Ma" REAL, d)`,
		`CREATE TABLE u(x, y)`,
		`CREATE VIEW v AS SELECT x, y FROM u`,
		`CREATE VIEW w AS SELECT beta, alpha, substr(beta, 1, 2) AS cc FROM t WHERE substr(beta, 1, 2) = '13' OR d = 2`,
	} {
		if err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, tc := range []struct {
		body  string
		plain bool
	}{
		{`SELECT alpha, BETA, "gam ma" FROM t`, true},
		{`SELECT t.alpha, beta || 'x' AS b2, abs(d) AS "Q" FROM t WHERE d > 1 ORDER BY beta`, true},
		{`SELECT x.alpha AS a, upper(x.beta) AS b FROM t AS x WHERE x.d IS NOT NULL`, true},
		{`SELECT substr(beta, 1, 2) AS cc, d FROM t WHERE substr(beta, 1, 2) = '13' OR d = 2 LIMIT 3`, true},
		{`SELECT beta || 'x' FROM t`, false},
		{`SELECT alpha, alpha FROM t`, false},
		{`SELECT alpha AS x, beta AS X FROM t`, false},
		{`SELECT rowid, beta AS b FROM t`, false},
		{`SELECT * FROM t`, false},
		{`SELECT x, abs(y) AS z FROM v`, false},
		{`SELECT u.x, abs(t.d) AS z FROM u, t`, false},
		{`SELECT alpha, (SELECT 1) AS s FROM t`, false},
		{`SELECT alpha, beta AS b FROM t WHERE alpha IN (SELECT x FROM u)`, false},
		{`SELECT alpha, beta AS b FROM t GROUP BY alpha`, false},
		{`SELECT alpha, beta AS b FROM t UNION ALL SELECT x, y FROM u`, false},
		{`SELECT main.t.alpha, beta AS b FROM main.t`, false},
		{`SELECT alpha, beta AS b FROM main.t`, true},
	} {
		sub, err := ParseSelect(tc.body)
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		plain, ok := p.plainDerivedCols(sub)
		if ok != tc.plain {
			t.Errorf("%s: plain = %v, want %v", tc.body, ok, tc.plain)
		}
		if !ok {
			continue
		}
		compiled, cok := p.derivedSchemaColsOnly(sub)
		if !cok || len(compiled) != len(plain) {
			t.Errorf("%s: compiled %v (%v), plain %v", tc.body, compiled, cok, plain)
			continue
		}
		for j := range plain {
			if !strings.EqualFold(plain[j].Name, compiled[j].Name) {
				t.Errorf("%s: column %d named %q, compiled %q", tc.body, j, plain[j].Name, compiled[j].Name)
			}
		}
	}
	// A view as the flattener's expander hands it over, its FROM pinned to
	// its own schema.
	d, ok := p.fromItemAsDerived(FromItem{Table: "w"})
	if !ok || d.Subquery == nil {
		t.Fatal("view w did not expand")
	}
	plain, ok := p.plainDerivedCols(d.Subquery)
	if !ok {
		t.Fatal("view w's body was not named without a compile")
	}
	compiled, _ := p.derivedSchemaColsOnly(d.Subquery)
	if len(compiled) != len(plain) {
		t.Fatalf("view w: compiled %v, plain %v", compiled, plain)
	}
	for j := range plain {
		if !strings.EqualFold(plain[j].Name, compiled[j].Name) {
			t.Errorf("view w: column %d named %q, compiled %q", j, plain[j].Name, compiled[j].Name)
		}
	}
}
