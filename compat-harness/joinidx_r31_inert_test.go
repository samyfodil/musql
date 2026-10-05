package compat

// Index inertness: shapes where indexes are claimed not to affect plan or row order.
// Two tiers: inert (rows match oracle), notInert (rows match oracle or decline cleanly).
// Fixture is observable: values not in rowid order, so index vs rowid scans differ.

import (
	"encoding/json"
	"testing"
)

// r31InertSchema: t1 joins t2 on (x, p). y/q are observers.
var r31InertSchema = []string{
	`CREATE TABLE t1(x INT, y TEXT, z TEXT)`,
	`INSERT INTO t1 VALUES(2,'a3','d1'),(1,'a1','d3'),(3,'a2','d2'),(1,'a0','d0')`,
	`CREATE TABLE t2(p INT, q TEXT, r TEXT)`,
	`INSERT INTO t2 VALUES(1,'b2','e1'),(3,'b0','e2'),(1,'b1','e0'),(2,'b3','e3')`,
}

// r31JoinKnownWrong is the CEILING on wrong answers in the notInert tier. It stands
// at 1, and that one is the shape this whole bucket is about: an index on the
// JOIN column, which SQLite walks and this engine does not, because
// whereLoopAddBtreeIndex is wired into the SINGLE-table planner only.
//
//	CREATE INDEX i1x ON t1(x);
//	SELECT t1.y, t2.q FROM t1, t2 WHERE t1.x = t2.p LIMIT 4
//	  cgo:    a1|b2  a0|b2  a2|b0  a1|b1     (t2 outer, t1 walked by i1x)
//	  musql: a3|b3  a1|b2  a1|b1  a2|b0
//
// It is a ceiling rather than a per-case expectation so that neither fixing it
// nor adding a case that happens to be served reads as a failure.
var r31JoinKnownWrong = 1

// r31InertCase runs one schema + DDL + query set through both engines. inert
// selects the tier described in this file's doc comment. It returns how many
// queries came back WRONG (a different row set, not a decline).
func r31InertCase(t *testing.T, name string, inert bool, ddl []string, queries ...string) int {
	t.Helper()
	wrong := 0
	t.Run(name, func(t *testing.T) {
		stmts := append([]string{}, r31InertSchema...)
		stmts = append(stmts, ddl...)
		first := len(stmts)
		stmts = append(stmts, queries...)

		cgo := run(t, "cgo", stmts)
		mush := run(t, "musql", stmts)
		for i := 0; i < first; i++ {
			cb, _ := json.Marshal(cgo[i])
			mb, _ := json.Marshal(mush[i])
			if string(cb) != string(mb) {
				t.Fatalf("setup diverges at stmt %d (%s)\n  cgo:    %s\n  musql: %s",
					i, stmts[i], cb, mb)
			}
		}
		for i := first; i < len(stmts); i++ {
			cb, _ := json.Marshal(cgo[i])
			mb, _ := json.Marshal(mush[i])
			if string(cb) == string(mb) {
				continue
			}
			if !inert {
				if mush[i]["kind"] == "error" && cgo[i]["kind"] != "error" {
					// Declined, not answered wrongly. Allowed for this tier; when
					// it becomes served the equality branch above takes over and
					// this case keeps passing.
					t.Logf("still declined (not a wrong answer): %s", stmts[i])
					continue
				}
				// Counted against r31JoinKnownWrong by the caller, and always logged
				// so a new one is visible in the output even before the ceiling
				// trips.
				wrong++
				t.Logf("WRONG (counted against the ceiling)\n  %s\n  cgo:    %s\n  musql: %s",
					stmts[i], cb, mb)
				continue
			}
			t.Errorf("INERT index changed the answer\n  %s\n  cgo:    %s\n  musql: %s",
				stmts[i], cb, mb)
		}
	})
	return wrong
}

func TestR31IndexInertRules(t *testing.T) {
	wrong := 0
	// The order-observing readouts. flat and limited are used for the INERT tier
	// because neither routes through the aggregate anchor guard: flat is a
	// whole-table aggregate with no bare column, and limited has no aggregate at
	// all. Both report the nesting AND each level's visit order.
	flat := `SELECT group_concat(t1.y||'-'||t2.q) FROM t1, t2 WHERE t1.x = t2.p`
	limited := `SELECT t1.y, t2.q FROM t1, t2 WHERE t1.x = t2.p LIMIT 4`
	// grouped reads a BARE column out of a grouped aggregate, which is the
	// strongest readout here -- and is currently DECLINED for every one of these
	// shapes by anchorNoIndexInPlay (vdbe_agg_codegen.go), whose access-path test
	// is still "does any scanned table carry an index at all". It is therefore
	// only ever used at the notInert tier, which tolerates a decline. Measured:
	// with anchorNoIndexInPlay's disjunct widened to
	// "|| wherePlanIndexesProvablyInert(p, srcs, stmt)" every inert case below
	// serves `grouped` and matches the oracle, and the 800-shape battery's dead
	// family goes 42 -> 0. Moving grouped up to the inert tier is the assertion
	// to add when that lands.
	grouped := `SELECT t1.x, count(*), t2.q FROM t1, t2 WHERE t1.x = t2.p GROUP BY t1.x ORDER BY 1`

	// INERT: an index on a column no query names. whereScanInit finds no term on
	// its leading column, indexMightHelpWithOrderBy is 0, and it covers nothing
	// the query reads -- so whereLoopAddBtree builds no loop for it and SQLite
	// plans exactly as it would with no index at all.
	wrong += r31InertCase(t, "dead-column", true, []string{`CREATE INDEX i1z ON t1(z)`}, flat, limited)
	wrong += r31InertCase(t, "dead-column-both", true,
		[]string{`CREATE INDEX i1z ON t1(z)`, `CREATE INDEX i2r ON t2(r)`}, flat, limited)
	wrong += r31InertCase(t, "dead-column-grouped", false, []string{`CREATE INDEX i1z ON t1(z)`}, grouped)

	// INERT: an index whose column the query only READS. colUsed carries other
	// columns too, so m != 0 and the covering full-index-scan candidate is not
	// built either.
	wrong += r31InertCase(t, "observer-column", true, []string{`CREATE INDEX i1y ON t1(y)`}, flat, limited)

	// INERT: an index over EVERY column, led by a dead one. It IS covering
	// (m == 0), but estimateIndexWidth then equals estimateTableWidth and the
	// candidate needs szIdxRow strictly less than szTabRow.
	//
	// Only the un-ordered query: a covering index by definition holds every
	// column the statement reads, so as soon as the statement asks ANY ordering
	// question over a column of this table, that column is one of the index's
	// key columns and indexMightHelpWithOrderBy answers 1. Adding `grouped` here
	// (GROUP BY t1.x, and x is this index's third key column) is what first made
	// this case fail -- the label was wrong, not the port.
	wrong += r31InertCase(t, "covering-not-narrower", true,
		[]string{`CREATE INDEX i1all ON t1(z,y,x)`}, flat)

	// NOT INERT: covering AND narrower. The flat query reads only x and y of t1,
	// this index holds both (m == 0), and (5+1+1)*4 is below the table's
	// (1+5+5+1)*4 -- so whereLoopAddBtree builds the covering full-index-scan
	// candidate and it can win.
	wrong += r31InertCase(t, "covering-and-narrower", false,
		[]string{`CREATE INDEX i1yx ON t1(y,x)`}, flat)

	// INERT: DISTINCT with no ORDER BY, whose pOrderBy is the RESULT SET -- which
	// names y and q, neither of which is a key column of an index on z.
	wrong += r31InertCase(t, "dead-column-distinct", true, []string{`CREATE INDEX i1z ON t1(z)`},
		`SELECT DISTINCT t1.y, t2.q FROM t1, t2 WHERE t1.x = t2.p`)

	// INERT: three tables, the widest FROM the multi-table planner accepts.
	wrong += r31InertCase(t, "three-table-dead", true,
		[]string{
			`CREATE TABLE t3(m INT, n TEXT, o TEXT)`,
			`INSERT INTO t3 VALUES(1,'c1','f2'),(2,'c0','f0'),(1,'c2','f1')`,
			`CREATE INDEX i3o ON t3(o)`,
		},
		`SELECT group_concat(t1.y||t2.q||t3.n) FROM t1, t2, t3 WHERE t1.x = t2.p AND t2.p = t3.m`)

	// NOT INERT: an index on the JOIN column. whereScanInit matches the equality
	// and whereLoopAddBtreeIndex prices a real seek loop.
	wrong += r31InertCase(t, "join-column", false, []string{`CREATE INDEX i1x ON t1(x)`}, flat, limited, grouped)
	wrong += r31InertCase(t, "join-column-inner", false, []string{`CREATE INDEX i2p ON t2(p)`}, flat, limited, grouped)

	// NOT INERT: an index LED by the join column, even though its other key
	// columns are dead.
	wrong += r31InertCase(t, "leading-join-column", false,
		[]string{`CREATE INDEX i1xz ON t1(x,z)`}, flat, grouped)

	// NOT INERT: an index on a column a WHERE inequality pins, which is not the
	// join column -- opMask carries WO_GT as well as WO_EQ.
	wrong += r31InertCase(t, "constrained-column", false, []string{`CREATE INDEX i1z ON t1(z)`},
		`SELECT group_concat(t1.y||'-'||t2.q) FROM t1, t2 WHERE t1.x = t2.p AND t1.z > 'd0'`)

	// NOT INERT: the dead column becomes an ordering term, so
	// indexMightHelpWithOrderBy answers 1 and the full-index-scan candidate
	// exists.
	wrong += r31InertCase(t, "dead-column-ordered", false, []string{`CREATE INDEX i1z ON t1(z)`},
		`SELECT t1.z, count(*), t2.q FROM t1, t2 WHERE t1.x = t2.p GROUP BY t1.z ORDER BY 1`)

	// NOT INERT: an ORDER BY ORDINAL. resolve.c rewrites it into the select-list
	// expression it names, which this AST still holds as a bare integer -- so the
	// port has to read the select list, not just the ORDER BY terms.
	wrong += r31InertCase(t, "ordinal-order-by", false, []string{`CREATE INDEX i1y ON t1(y)`},
		`SELECT t1.y, count(*), t2.q FROM t1, t2 WHERE t1.x = t2.p GROUP BY t1.y ORDER BY 1`)

	// NOT INERT: a ROWID ordering term is satisfied by ANY index of a rowid
	// table, every one of which ends in XN_ROWID.
	wrong += r31InertCase(t, "rowid-order-by", false, []string{`CREATE INDEX i1z ON t1(z)`},
		`SELECT t1.rowid, count(*), t2.q FROM t1, t2 WHERE t1.x = t2.p GROUP BY t1.rowid ORDER BY 1`)

	// NOT REPRODUCIBLE: wherePlanIndexList declines a partial or expression index
	// outright, so the whole statement falls back rather than being assumed inert.
	wrong += r31InertCase(t, "partial-index", false,
		[]string{`CREATE INDEX i1zp ON t1(z) WHERE x > 1`}, flat, grouped)
	wrong += r31InertCase(t, "expression-index", false,
		[]string{`CREATE INDEX i1ze ON t1(z||'!')`}, flat, grouped)

	// NOT INERT: INDEXED BY makes that index the ONLY probe and drops the rowid
	// scan; NOT INDEXED stops the chain at the rowid scan but leaves
	// columnIsGoodIndexCandidate walking the real indexes anyway.
	wrong += r31InertCase(t, "indexed-by", false, []string{`CREATE INDEX i1z ON t1(z)`},
		`SELECT group_concat(t1.y||'-'||t2.q) FROM t1 INDEXED BY i1z, t2 WHERE t1.x = t2.p`)
	wrong += r31InertCase(t, "not-indexed", false, []string{`CREATE INDEX i1x ON t1(x)`},
		`SELECT group_concat(t1.y||'-'||t2.q) FROM t1 NOT INDEXED, t2 WHERE t1.x = t2.p`)

	if wrong > r31JoinKnownWrong {
		t.Errorf("%d wrong answers in the notInert tier, ceiling is %d -- a NEW one appeared "+
			"(each is logged above as WRONG)", wrong, r31JoinKnownWrong)
	}
	if wrong < r31JoinKnownWrong {
		t.Logf("only %d wrong answers left of %d: lower r31JoinKnownWrong", wrong, r31JoinKnownWrong)
	}
}
