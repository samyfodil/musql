package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// cellText renders a Value as plain text for comparison.
func cellText(v Value) string {
	if v.Typ == Int {
		return fmt.Sprintf("%d", v.I)
	}
	return string(v.S)
}

// TestCrossDatabaseThreePartReferenceThroughNestedJoin tests schema-qualified
// references through nested joins in attached databases.
// regardless of which database the write actually named -- so the query would
// have answered a wrong row count rather than erroring, per that function's
// own doc comment. Fixed by making tableScope.dbIdx (already threaded onto
// every scope) an actual discriminator: see dbIdxForSchema
// (cross_db.go), resolveInScopes/qualifiedScopeAmbiguous (vdbe_codegen.go),
// sameNamedScopeAlsoExposes/starQualifierAmbiguousAcrossDB (query.go).
//
// Verified directly against C SQLite 3.53.3 (selectD.test itself, and the
// compat-harness TestCrossDBThreePartReferenceThroughNestedJoin differential
// test, compat-harness/crossdb_diff_test.go): both the AS-aliased (2.5) and
// doubly-unaliased (2.4) forms answer {111 x1 222 x2 444 x4 555 x5}.
func TestCrossDatabaseThreePartReferenceThroughNestedJoin(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Discard()
	for _, s := range []string{
		`ATTACH ':memory:' AS aux1`,
		`CREATE TABLE t1(a,b)`, `INSERT INTO t1 VALUES(111,'x1')`,
		`CREATE TABLE t2(a,b)`, `INSERT INTO t2 VALUES(222,'x2')`,
		`CREATE TABLE main.t4(a,b)`, `INSERT INTO main.t4 VALUES(444,'x4')`,
		`CREATE TABLE aux1.t4(a,b)`, `INSERT INTO aux1.t4 VALUES(555,'x5')`,
	} {
		if e := db.Exec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetAttachedReadersForTest(db)

	want := [][]string{{"111", "x1", "222", "x2", "444", "x4", "555", "x5"}}
	for name, q := range map[string]string{
		// selectD-i.2.4: neither t4 is aliased -- the bare alias "t4" names
		// two different tables from two different databases.
		"2.4_bothUnaliased": `
			SELECT *
			  FROM t1 JOIN (t2 JOIN (main.t4 JOIN aux1.t4 ON aux1.t4.a=main.t4.a+111)
			                        ON main.t4.a=t2.a+222)
			               ON t2.a=t1.a+111`,
		// selectD-i.2.5: main.t4 is aliased "x", aux1.t4 stays bare "t4" --
		// still exercises the same 3-part "aux1.t4.a" resolution.
		"2.5_localAliased": `
			SELECT *
			  FROM t1 JOIN (t2 JOIN (main.t4 AS x JOIN aux1.t4 ON aux1.t4.a=x.a+111)
			                        ON x.a=t2.a+222)
			               ON t2.a=t1.a+111`,
	} {
		t.Run(name, func(t *testing.T) {
			_, rows, qerr := p.QueryArgs(q, nil)
			if qerr != nil {
				t.Fatalf("%s: %v", q, qerr)
			}
			if len(rows) != len(want) {
				t.Fatalf("got %d rows, want %d: %v", len(rows), len(want), rows)
			}
			for ri, wr := range want {
				if len(rows[ri]) != len(wr) {
					t.Fatalf("row %d: got %d cols, want %d", ri, len(rows[ri]), len(wr))
				}
				for ci, wv := range wr {
					if got := cellText(rows[ri][ci]); got != wv {
						t.Errorf("row %d col %d: got %q, want %q", ri, ci, got, wv)
					}
				}
			}
		})
	}

	// selectD-i.2.6: the SAME shape but naming an attached database that was
	// never ATTACHed ("aux", not "aux1") must still decline with "no such
	// table" -- this bucket only closes a gap for a qualifier that DOES
	// resolve, never widens what an unresolvable one accepts.
	if _, _, qerr := p.QueryArgs(`
		SELECT *
		  FROM t1 JOIN (t2 JOIN (main.t4 JOIN aux.t4 ON aux.t4.a=main.t4.a+111)
		                        ON main.t4.a=t2.a+222)
		               ON t2.a=t1.a+111`, nil); qerr == nil {
		t.Error("aux.t4 (never ATTACHed) was accepted; want an error")
	}

	// selectD-i.2.7: both sides aliased distinctly ("x"/"y") -- already
	// resolved correctly before this fix (no name collision at all), kept
	// here as a same-shape regression check.
	_, rows, qerr := p.QueryArgs(`
		SELECT x.a, y.b
		  FROM t1 JOIN (t2 JOIN (main.t4 x JOIN aux1.t4 y ON y.a=x.a+111)
		                        ON x.a=t2.a+222)
		               ON t2.a=t1.a+111`, nil)
	if qerr != nil {
		t.Fatalf("2.7: %v", qerr)
	}
	if len(rows) != 1 || len(rows[0]) != 2 || cellText(rows[0][0]) != "444" || cellText(rows[0][1]) != "x5" {
		t.Errorf("2.7: got %v, want [[444 x5]]", rows)
	}
}
