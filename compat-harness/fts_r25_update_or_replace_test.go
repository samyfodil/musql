// This file gates UPDATE OR REPLACE against FTS3/FTS4 tables.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ftsR25ORDump is the full shadow-table image an fts4 table's OR REPLACE must
// reproduce byte for byte, plus the rows themselves.
func ftsR25ORDump(tbl string) []string {
	return []string{
		`SELECT docid, content FROM ` + tbl + ` ORDER BY docid`,
		`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM ` + tbl + `_segdir ORDER BY level, idx`,
		`SELECT blockid, quote(block) FROM ` + tbl + `_segments ORDER BY blockid`,
		`SELECT docid, quote(size) FROM ` + tbl + `_docsize ORDER BY docid`,
		`SELECT id, quote(value) FROM ` + tbl + `_stat ORDER BY id`,
		`INSERT INTO ` + tbl + `(` + tbl + `) VALUES('integrity-check')`,
	}
}

func TestFtsR25UpdateOrReplaceDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// fts3conf.test 3.4/3.5/3.7 verbatim -- the three mined statements
		// this rule was closed for, in their own file's order and state.
		{"fts3conf.test's own bracket", []string{
			`CREATE VIRTUAL TABLE t3 USING fts4`,
			`REPLACE INTO t3(docid, content) VALUES (1, 'one two')`,
			`REPLACE INTO t3(docid, content) VALUES (2, 'one two three four')`,
			`REPLACE INTO t3(docid, content) VALUES (1, 'one two three four five six')`,
			`UPDATE OR REPLACE t3 SET docid = 2 WHERE docid=1`,
			`SELECT quote(matchinfo(t3, 'na')) FROM t3 WHERE t3 MATCH 'six'`,
			`UPDATE OR REPLACE t3 SET docid = 3 WHERE docid=2`,
			`SELECT quote(matchinfo(t3, 'na')) FROM t3 WHERE t3 MATCH 'six'`,
			`REPLACE INTO t3(docid, content) VALUES (3, 'one two')`,
			`REPLACE INTO t3(docid, content) VALUES(NULL,'one two three four')`,
			`REPLACE INTO t3(docid, content) VALUES(NULL,'one two three four five six')`,
			`SELECT docid FROM t3`,
			`UPDATE OR REPLACE t3 SET docid = 5, content='three four' WHERE docid = 4`,
			`SELECT quote(matchinfo(t3, 'na')) FROM t3 WHERE t3 MATCH 'one'`,
		}},

		// The wipe rule, both directions. This is the pair a "just accept the
		// keyword" change gets WRONG: same statement, same table, and the OR
		// clause alone decides whether every shadow table is thrown away.
		{"OR REPLACE over the table's only row WIPES", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(7,'alpha beta')`,
			`INSERT INTO t(docid,content) VALUES(9,'gamma')`,
			`DELETE FROM t WHERE docid=9`,
			`UPDATE OR REPLACE t SET docid=3 WHERE docid=7`,
		}},
		{"a plain UPDATE over the table's only row does NOT wipe", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(7,'alpha beta')`,
			`INSERT INTO t(docid,content) VALUES(9,'gamma')`,
			`DELETE FROM t WHERE docid=9`,
			`UPDATE t SET docid=3 WHERE docid=7`,
		}},

		// Displacement, with and without a surviving row -- the difference
		// between "the old row's delete wipes" and "it does not".
		{"displacing the only other row wipes", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'one two')`,
			`INSERT INTO t(docid,content) VALUES(2,'three four')`,
			`UPDATE OR REPLACE t SET docid=2 WHERE docid=1`,
		}},
		{"displacing with a third row surviving does not wipe", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'one two')`,
			`INSERT INTO t(docid,content) VALUES(2,'three four')`,
			`INSERT INTO t(docid,content) VALUES(3,'five six')`,
			`UPDATE OR REPLACE t SET docid=2 WHERE docid=1`,
		}},
		{"displacing UPWARDS, several survivors", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'aa bb')`,
			`INSERT INTO t(docid,content) VALUES(2,'bb cc')`,
			`INSERT INTO t(docid,content) VALUES(5,'cc dd')`,
			`INSERT INTO t(docid,content) VALUES(9,'dd ee')`,
			`UPDATE OR REPLACE t SET docid=9 WHERE docid=2`,
		}},
		{"displacing DOWNWARDS, several survivors", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'aa bb')`,
			`INSERT INTO t(docid,content) VALUES(2,'bb cc')`,
			`INSERT INTO t(docid,content) VALUES(5,'cc dd')`,
			`INSERT INTO t(docid,content) VALUES(9,'dd ee')`,
			`UPDATE OR REPLACE t SET docid=1 WHERE docid=9`,
		}},

		// No conflict at all: OR REPLACE must be the plain relocation, except
		// for the wipe check above.
		{"OR REPLACE moving to a fresh docid, survivors present", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'aa bb')`,
			`INSERT INTO t(docid,content) VALUES(2,'bb cc')`,
			`UPDATE OR REPLACE t SET docid=77 WHERE docid=1`,
		}},

		// The docid does not move: the whole conflict branch is skipped in the
		// C ("apVal[0]==pNewRowid"), so this must be byte-identical to the
		// plain UPDATE below it.
		{"OR REPLACE with no docid change", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'one two')`,
			`INSERT INTO t(docid,content) VALUES(2,'three four')`,
			`UPDATE OR REPLACE t SET content='zz' WHERE docid=1`,
		}},
		{"the plain UPDATE it must equal", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'one two')`,
			`INSERT INTO t(docid,content) VALUES(2,'three four')`,
			`UPDATE t SET content='zz' WHERE docid=1`,
		}},
		{"OR REPLACE naming docid with its OWN value", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(4,'alpha beta')`,
			`INSERT INTO t(docid,content) VALUES(6,'gamma')`,
			`UPDATE OR REPLACE t SET docid=docid, content='alpha gamma' WHERE docid=4`,
		}},

		// Inside a transaction, where the displacement's flush point has to
		// fold into the segment the transaction is already accumulating.
		{"OR REPLACE inside a transaction", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'aa')`,
			`INSERT INTO t(docid,content) VALUES(2,'bb')`,
			`INSERT INTO t(docid,content) VALUES(3,'cc')`,
			`BEGIN`,
			`INSERT INTO t(docid,content) VALUES(10,'dd')`,
			`UPDATE OR REPLACE t SET docid=3 WHERE docid=1`,
			`COMMIT`,
		}},
		{"OR REPLACE rolled back", []string{
			`CREATE VIRTUAL TABLE t USING fts4`,
			`INSERT INTO t(docid,content) VALUES(1,'aa')`,
			`INSERT INTO t(docid,content) VALUES(2,'bb')`,
			`INSERT INTO t(docid,content) VALUES(3,'cc')`,
			`BEGIN`,
			`UPDATE OR REPLACE t SET docid=3 WHERE docid=1`,
			`ROLLBACK`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			full := append(append([]string{}, c.stmts...), ftsR25ORDump("t3")...)
			if !strings.Contains(c.stmts[0], "t3") {
				full = append(append([]string{}, c.stmts...), ftsR25ORDump("t")...)
			}
			differAllAccepted(t, c.name, full, len(c.stmts))
		})
	}
}

// TestFtsR25UpdateOrReplaceFts3Diff is the same rule on the fts3 module, which
// has neither %_docsize nor %_stat -- so the wipe is visible only in %_segdir.
func TestFtsR25UpdateOrReplaceFts3Diff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		{"fts3: displacement wipes", []string{
			`CREATE VIRTUAL TABLE t USING fts3(x)`,
			`INSERT INTO t(docid,x) VALUES(1,'one two')`,
			`INSERT INTO t(docid,x) VALUES(2,'three four')`,
			`UPDATE OR REPLACE t SET docid=2 WHERE docid=1`,
		}},
		{"fts3: displacement with a survivor", []string{
			`CREATE VIRTUAL TABLE t USING fts3(x)`,
			`INSERT INTO t(docid,x) VALUES(1,'one two')`,
			`INSERT INTO t(docid,x) VALUES(2,'three four')`,
			`INSERT INTO t(docid,x) VALUES(3,'five six')`,
			`UPDATE OR REPLACE t SET docid=2 WHERE docid=1`,
		}},
		{"fts3 with prefix=: every index gets the same treatment", []string{
			`CREATE VIRTUAL TABLE t USING fts4(x, prefix="2")`,
			`INSERT INTO t(docid,x) VALUES(1,'alpha beta')`,
			`INSERT INTO t(docid,x) VALUES(2,'gamma delta')`,
			`INSERT INTO t(docid,x) VALUES(3,'epsilon')`,
			`UPDATE OR REPLACE t SET docid=2 WHERE docid=1`,
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			full := append(append([]string{}, c.stmts...),
				`SELECT docid, x FROM t ORDER BY docid`,
				`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
				`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
				`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'one' ORDER BY docid)`,
				`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'three' ORDER BY docid)`,
			)
			differAllAccepted(t, c.name, full, len(c.stmts))
		})
	}
}

// TestFtsR25UpdateOrReplaceInterchange is the load-bearing claim: a database
// whose segmentation an OR REPLACE decided must read back identically under
// both engines, whichever one wrote it.
func TestFtsR25UpdateOrReplaceInterchange(t *testing.T) {
	write := []string{
		`CREATE VIRTUAL TABLE t USING fts4(x)`,
		`INSERT INTO t(docid,x) VALUES(1,'alpha beta')`,
		`INSERT INTO t(docid,x) VALUES(4,'gamma delta')`,
		`INSERT INTO t(docid,x) VALUES(9,'alpha epsilon')`,
		`UPDATE OR REPLACE t SET docid=4 WHERE docid=1`, // displaces downwards
		`UPDATE OR REPLACE t SET docid=20 WHERE docid=4`,
		`BEGIN`,
		`INSERT INTO t(docid,x) VALUES(30,'zeta')`,
		`UPDATE OR REPLACE t SET docid=9 WHERE docid=20`, // displaces again
		`COMMIT`,
	}
	read := []string{
		`SELECT docid, x FROM t ORDER BY docid`,
		`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
		`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
		`SELECT id, quote(value) FROM t_stat ORDER BY id`,
		`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'alpha' ORDER BY docid)`,
		`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH 'zeta' ORDER BY docid)`,
	}
	fileInterchangeRoundTrip(t, "or-replace-update", write, read)
}

// TestFtsR25UpdateOrReplaceFuzzDiff randomizes the history, because the
// segmentation is history-dependent and a handful of hand-written scripts
// prove far less than they look like they do. Only SINGLE-row UPDATEs are
// generated: a multi-row one that displaces is deliberately declined (this
// file's header comment), and differ() cannot express "one engine declines".
func TestFtsR25UpdateOrReplaceFuzzDiff(t *testing.T) {
	nIter := 300
	if testing.Short() {
		nIter = 60
	}
	words := []string{"aa", "bb", "cc", "dd", "ee"}
	rng := rand.New(rand.NewSource(20260807))
	text := func() string {
		n := 1 + rng.Intn(2)
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, words[rng.Intn(len(words))])
		}
		return strings.Join(parts, " ")
	}
	for iter := 0; iter < nIter; iter++ {
		schema := `CREATE VIRTUAL TABLE t USING fts4(x)`
		switch rng.Intn(3) {
		case 1:
			schema = `CREATE VIRTUAL TABLE t USING fts3(x)`
		case 2:
			schema = `CREATE VIRTUAL TABLE t USING fts4(x, prefix="2")`
		}
		stmts := []string{schema}
		live := []int64{}
		next := int64(1)
		nSeed := 1 + rng.Intn(4)
		for k := 0; k < nSeed; k++ {
			stmts = append(stmts, fmt.Sprintf(`INSERT INTO t(docid,x) VALUES(%d,'%s')`, next, text()))
			live = append(live, next)
			next++
		}
		inTxn := rng.Intn(2) == 0
		if inTxn {
			stmts = append(stmts, `BEGIN`)
		}
		nOps := 1 + rng.Intn(3)
		for k := 0; k < nOps && len(live) > 0; k++ {
			oldIdx := rng.Intn(len(live))
			old := live[oldIdx]
			switch rng.Intn(4) {
			case 0:
				// A column-only OR REPLACE: no conflict branch at all.
				stmts = append(stmts, fmt.Sprintf(`UPDATE OR REPLACE t SET x='%s' WHERE docid=%d`, text(), old))
			case 1:
				// Move to a FRESH docid.
				newD := next
				next++
				stmts = append(stmts, fmt.Sprintf(`UPDATE OR REPLACE t SET docid=%d WHERE docid=%d`, newD, old))
				live[oldIdx] = newD
			default:
				// Move ONTO another live docid (a displacement) when there is
				// one, otherwise onto a fresh one.
				if len(live) < 2 {
					newD := next
					next++
					stmts = append(stmts, fmt.Sprintf(`UPDATE OR REPLACE t SET docid=%d, x='%s' WHERE docid=%d`, newD, text(), old))
					live[oldIdx] = newD
					break
				}
				tgt := rng.Intn(len(live))
				for tgt == oldIdx {
					tgt = rng.Intn(len(live))
				}
				newD := live[tgt]
				stmts = append(stmts, fmt.Sprintf(`UPDATE OR REPLACE t SET docid=%d WHERE docid=%d`, newD, old))
				live = append(live[:oldIdx], live[oldIdx+1:]...)
			}
		}
		if inTxn {
			stmts = append(stmts, `COMMIT`)
		}
		stmts = append(stmts,
			`SELECT level, idx, start_block, leaves_end_block, end_block, quote(root) FROM t_segdir ORDER BY level, idx`,
			`SELECT blockid, quote(block) FROM t_segments ORDER BY blockid`,
			`SELECT docid, x FROM t ORDER BY docid`,
			`SELECT count(*) FROM t`,
		)
		for _, w := range words {
			stmts = append(stmts, fmt.Sprintf(`SELECT ifnull(group_concat(docid),'') FROM (SELECT docid FROM t WHERE t MATCH '%s' ORDER BY docid)`, w))
		}
		if !differ(t, fmt.Sprintf("or-replace-update-fuzz/%d", iter), stmts) {
			t.Fatalf("stopping at the first divergent history (iteration %d)", iter)
		}
	}
}
