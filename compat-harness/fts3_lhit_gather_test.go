// FTS3 per-row phrase reachability computation for MATCH.
package compat

import "testing"

var fts3LHitDocs = []string{
	`INSERT INTO t(docid,a,b) VALUES(1,'alpha bravo','x')`,
	`INSERT INTO t(docid,a,b) VALUES(2,'alpha charlie','x')`,
	`INSERT INTO t(docid,a,b) VALUES(3,'delta charlie','x')`,
}

func fts3LHitDiffer(t *testing.T, name, module string, queries []string) {
	t.Helper()
	stmts := []string{"CREATE VIRTUAL TABLE t USING " + module + "(a,b)"}
	stmts = append(stmts, fts3LHitDocs...)
	for _, q := range queries {
		stmts = append(stmts,
			`SELECT docid, quote(matchinfo(t,'xyb')) FROM t WHERE t MATCH '`+q+`' ORDER BY docid`)
	}
	differ(t, name, stmts)
}

// TestFts3LHitGatherAndCoOccurrence is the direct target case: "alpha AND
// bravo" really does match (docid 1), so neither phrase is globally dead and
// the AND itself is not globally dead either -- the engine's old nodeDead
// check (OR of two globally-dead flags) would find nothing to suppress here.
// But at docid 2 (reached only via the "OR charlie" branch), alpha has a raw
// hit while the AND's own iterator has already gone permanently past docid 1
// with nothing left on bravo's side -- so alpha's 'y'/'b' bits must read 0 at
// docid 2 even though alpha genuinely occurs in column a there.
func TestFts3LHitGatherAndCoOccurrence(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		fts3LHitDiffer(t, module+" AND-co-occurrence OR sibling", module, []string{
			`(alpha AND bravo) OR charlie`,
			`charlie OR (alpha AND bravo)`,
		})
	}
}

// TestFts3LHitGatherNearCoOccurrence is the same shape through NEAR, which
// fts3EvalNextRow documents as "treated as AND" for its own iterator (the
// position-distance test is a separate, later check) -- so a NEAR pair's
// candidate set is the same plain intersection as AND's, and the suppression
// at docid 2 must hold even though "alpha NEAR/0 bravo" also individually
// passes the proximity test at docid 1.
func TestFts3LHitGatherNearCoOccurrence(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		fts3LHitDiffer(t, module+" NEAR-co-occurrence OR sibling", module, []string{
			`(alpha NEAR/0 bravo) OR charlie`,
			`(alpha NEAR/2 bravo) OR charlie`,
		})
	}
}

// TestFts3LHitGatherTrueMatchNotOversuppressed pins the flip side of the same
// case: at docid 1, where "alpha AND bravo" is a REAL match, neither alpha's
// nor bravo's 'y'/'b' bits may be suppressed -- the fix must not turn into a
// blanket "any node with an AND ancestor is always suppressed" bug.
func TestFts3LHitGatherTrueMatchNotOversuppressed(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		fts3LHitDiffer(t, module+" true co-occurrence row not oversuppressed", module, []string{
			`(alpha AND bravo) OR charlie`,
		})
	}
}

// TestFts3LHitGatherThreeWayAnd nests one more level: "alpha AND bravo AND
// delta" can never match anything (bravo and delta never share a docid at
// all), but the INNER "alpha AND bravo" pair still has its own real match at
// docid 1. Parsed left-associatively as AND(AND(alpha,bravo),delta), delta's
// own doclist ({3}) never intersects the inner AND's ({1}), so the outer
// AND's candidate set is empty everywhere -- alpha and bravo must both be
// suppressed at every row this reaches (including docid 1, where the OR
// sibling still pulls the row in but the 3-way AND never does).
func TestFts3LHitGatherThreeWayAnd(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		fts3LHitDiffer(t, module+" three-way AND never fully intersects", module, []string{
			`(alpha AND bravo AND delta) OR charlie OR delta`,
		})
	}
}

// TestFts3LHitGatherOrInsideAnd checks the OTHER nesting: an OR feeding an
// AND, i.e. AND(OR(alpha,delta),charlie). alpha/delta's union is {1,2,3};
// intersected with charlie's {2,3} gives {2,3} -- a real, non-empty
// candidate set unlike the plain-AND cases above -- so this exercises
// fts3NodeCandidateSet's OR branch feeding directly into an AND intersection,
// not just OR-of-AND.
func TestFts3LHitGatherOrInsideAnd(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		fts3LHitDiffer(t, module+" OR feeding an AND intersection", module, []string{
			`(alpha OR delta) AND charlie`,
		})
	}
}

// TestFts3LHitGatherNotSiblingUnaffected pins NOT's own rule
// (fts3.c:5460-5463: a NOT node's iDocid/bEof come from its LEFT child alone,
// the excluded right side never touches them) against the same co-occurrence
// shape: "(alpha AND bravo) NOT delta" reduces to the same {1}-only candidate
// set as the plain AND (delta excludes nothing here, docid 1 is not in
// delta's own doclist), so this must suppress identically to
// TestFts3LHitGatherAndCoOccurrence's bare AND case.
func TestFts3LHitGatherNotSiblingUnaffected(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		fts3LHitDiffer(t, module+" NOT sibling does not change AND's own set", module, []string{
			`((alpha AND bravo) NOT delta) OR charlie`,
		})
	}
}

// TestFts3LHitGatherDescIdx is the same AND/OR co-occurrence shape on an
// "order=desc" table, whose rows -- and, per fts3EvalNextRow's bDescIdx, whose
// underlying merge-join direction -- are reversed. fts3NodeCandidateSet's own
// design note argues the fix is direction-independent (intersection/union of
// two SETS does not care what order their members were produced in); this
// pins that empirically rather than only by proof.
func TestFts3LHitGatherDescIdx(t *testing.T) {
	stmts := []string{`CREATE VIRTUAL TABLE t USING fts4(a,b,order=desc)`}
	stmts = append(stmts, fts3LHitDocs...)
	stmts = append(stmts,
		`SELECT docid, quote(matchinfo(t,'xyb')) FROM t WHERE t MATCH '(alpha AND bravo) OR charlie' ORDER BY docid`,
		`SELECT docid, quote(matchinfo(t,'xyb')) FROM t WHERE t MATCH '(alpha AND bravo) OR charlie'`,
	)
	differ(t, "fts4 order=desc AND-co-occurrence OR sibling", stmts)
}

// TestFts3LHitGatherRegressionSweep re-runs a wide sample of the existing
// (already-passing) fts3/fts4 MATCH/matchinfo batteries' shapes over the
// fts3LHitGather-specific documents, as a regression net: every one of these
// queries either has no AND/NEAR/NOT at all (must be completely unaffected by
// this change) or combines terms in ways the old whole-table-emptiness
// approximation already handled correctly (every phrase either always
// co-occurs with its AND partner or never occurs at all) -- so old and new
// code must agree, and both must agree with the oracle.
func TestFts3LHitGatherRegressionSweep(t *testing.T) {
	for _, module := range []string{"fts3", "fts4"} {
		fts3LHitDiffer(t, module+" regression sweep", module, []string{
			`alpha`, `bravo`, `charlie`, `delta`, `nosuchterm`,
			`alpha OR delta`, `alpha OR nosuchterm`, `nosuchterm OR alpha`,
			`alpha AND charlie`, `alpha NOT charlie`, `charlie NOT alpha`,
			`alpha NEAR/0 bravo`, `alpha NEAR/5 delta`,
			`(alpha AND bravo)`, `(nosuchterm AND alpha)`, `(alpha AND nosuchterm)`,
			`(nosuchterm AND nosuchterm2)`,
			`alpha AND bravo OR delta`, `alpha OR bravo OR charlie OR delta`,
			`(alpha OR bravo) AND (charlie OR delta)`,
		})
	}
}
