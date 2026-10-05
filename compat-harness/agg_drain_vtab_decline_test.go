package compat

import (
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestZZDrainVtabInSubqueryDeclines tests the one shape where sorted GROUP BY
// drain refuses virtual table sources in aggregate subqueries.
func TestZZDrainVtabInSubqueryDeclines(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE f USING fts4(body)`,
		`INSERT INTO f(body) VALUES('alpha beta'),('beta gamma'),('gamma delta')`,
		`CREATE TABLE m(k INTEGER, q TEXT)`,
		`INSERT INTO m VALUES(1,'beta'),(1,'gamma'),(2,'delta')`,
	}
	// THESE TWO NOW ANSWER, and the gap closed from the direction this test
	// did not anticipate. The doc above says the fix is to teach the vtab
	// source to read the drain's row block; instead the statements stopped
	// reaching the drain at all. "GROUP BY k ORDER BY k" asks for exactly the
	// order the hash drain already emits, so it no longer needs a sorter
	// (orderIsGroupKeyAscending, vdbe_agg_codegen.go) -- and the hash path with
	// its cursors still live is the CONTROL this test already asserted answers
	// correctly. The two arms met.
	//
	// The answers are pinned against the oracle in
	// TestAggDrainVtabInSubqueryAnswers (agg_drain_subquery_test.go), which is
	// what this test's own message asked for. Kept here as a MUST-ANSWER so the
	// shape cannot quietly go back to declining.
	answers := []string{
		`SELECT k, sum((SELECT count(*) FROM f WHERE f.body MATCH m.q)) FROM m GROUP BY k ORDER BY k`,
		`SELECT k, group_concat((SELECT group_concat(body) FROM f WHERE f MATCH m.q)) FROM m GROUP BY k ORDER BY k`,
	}
	edb, err := engine.Create(filepath.Join(t.TempDir(), "e.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer edb.Close()
	for _, s := range setup {
		if err := edb.Exec(s); err != nil {
			t.Fatalf("setup %s: %v", s, err)
		}
	}
	for _, q := range answers {
		p, perr := edb.SnapshotPager()
		if perr != nil {
			t.Fatalf("SnapshotPager: %v", perr)
		}
		_, _, qerr := p.QueryArgs(q, nil)
		p.Close()
		if qerr != nil {
			t.Errorf("%s: %v -- this shape answers now (see the note above); a decline here is a REGRESSION, "+
				"not the old gap", q, qerr)
		}
	}
	// The controls, against the oracle: the hash path answers the same
	// statement, and an ordinary derived table in the same position answers on
	// the drain too.
	zzProbe(t, setup, []string{
		`SELECT k, sum((SELECT count(*) FROM f WHERE f.body MATCH m.q)) FROM m GROUP BY k`,
		`SELECT k, sum((SELECT count(*) FROM (SELECT 1 AS one))) FROM m GROUP BY k ORDER BY k`,
		`SELECT k, sum((SELECT count(*) FROM (SELECT q AS z FROM m m2 WHERE m2.k = m.k))) FROM m GROUP BY k ORDER BY k`,
		`SELECT body, sum((SELECT 1)) FROM f WHERE f MATCH 'beta' GROUP BY body ORDER BY body`,
	})
}
