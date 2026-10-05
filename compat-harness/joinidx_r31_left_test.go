package compat

// Tests LEFT-JOIN inner-side seek logic.
// The correlated seek used to decline every outer join. For a LEFT source it
// need not: emitJoinLevel sets the match flag only after the ON test passes on
// a fetched row, so restricting the fetched rows to a superset of the matching
// ones cannot change match-vs-NULL-extend. What it DOES change is the order the
// matching inner rows arrive in -- from this engine's rowid scan to the index's
// own walk, which is what codeOneLoopStart's OP_SeekGE/OP_Next emits.
//
// READOUT: a plain SELECT, not group_concat. The harness compares result rows
// IN ORDER, so a bare projection reports the nesting order directly -- and it
// is the only readout available here, because every AGGREGATE readout over an
// indexed table is currently refused by the aggregate anchor guard
// (anchorNoIndexInPlay, vdbe_agg_codegen.go). The first version of this file
// used group_concat and every single case came back "declined", measuring the
// guard instead of the seek.
//
// The fixture is built so the rowid order and the index walk order differ:
//
//   - the index is TWO columns (k,v), so within one k run the walk is ordered by
//     v while a rowid scan is not. A single-column index is the one shape where
//     the rowid order already IS the walk order;
//   - v descends as the rowid ascends, so the two orders are reversed rather
//     than merely different;
//   - the outer side carries a key matching NOTHING (7) and a NULL key -- the
//     two shapes that must still NULL-extend -- plus a duplicate key, so the
//     inner loop is re-entered with the same probe.

import (
	"encoding/json"
	"testing"
)

var r31LeftSchema = []string{
	`CREATE TABLE o(x INT, tag TEXT)`,
	`INSERT INTO o VALUES(2,'o1'),(1,'o2'),(7,'o3'),(NULL,'o4'),(1,'o5'),(3,'o6')`,
	`CREATE TABLE i(k INT, v TEXT, w TEXT)`,
	// v descends while the rowid ascends within every k run.
	`INSERT INTO i VALUES(1,'d','w1'),(1,'c','w2'),(2,'b','w3'),(1,'a','w4'),(3,'z','w5'),(2,'a','w6')`,
}

// r31LeftKnownWrong is the CEILING on cases that answer DIFFERENTLY from the
// oracle. It stands at 1: "right-join-unchanged", where a RIGHT JOIN's
// preserved side comes out in the wrong within-key order. RIGHT/FULL are
// deliberately still declined by the seek (their unmatched-row second pass needs
// the full inner row set marked), so that case measures a DIFFERENT hole and is
// carried here rather than fixed.
var r31LeftKnownWrong = 1

func TestR31LeftJoinSeekOrder(t *testing.T) {
	cases := []struct {
		name  string
		ddl   []string
		query string
	}{
		{
			"two-col-index",
			[]string{`CREATE INDEX ikv ON i(k,v)`},
			`SELECT o.tag, i.v FROM o LEFT JOIN i ON i.k = o.x`,
		},
		{
			"two-col-index-rev-operands",
			[]string{`CREATE INDEX ikv ON i(k,v)`},
			`SELECT o.tag, i.v FROM o LEFT JOIN i ON o.x = i.k`,
		},
		{
			"three-col-index",
			[]string{`CREATE INDEX ikvw ON i(k,v,w)`},
			`SELECT o.tag, i.v, i.w FROM o LEFT JOIN i ON i.k = o.x`,
		},
		{
			"desc-second-column",
			[]string{`CREATE INDEX ikvd ON i(k,v DESC)`},
			`SELECT o.tag, i.v FROM o LEFT JOIN i ON i.k = o.x`,
		},
		{
			"nocase-second-column",
			[]string{`CREATE INDEX ikvn ON i(k,v COLLATE NOCASE)`},
			`SELECT o.tag, i.v FROM o LEFT JOIN i ON i.k = o.x`,
		},
		{
			// The anti-join: only the NULL-extended rows survive, so this fails
			// if the seek ever loses a match.
			"anti-join",
			[]string{`CREATE INDEX ikv ON i(k,v)`},
			`SELECT o.tag FROM o LEFT JOIN i ON i.k = o.x WHERE i.k IS NULL`,
		},
		{
			// A second ON conjunct the seek does NOT key on: it is still
			// re-tested per fetched row, and a row it rejects must not suppress
			// the NULL-extension of an outer row with no other match.
			"extra-on-conjunct",
			[]string{`CREATE INDEX ikv ON i(k,v)`},
			`SELECT o.tag, i.v FROM o LEFT JOIN i ON i.k = o.x AND i.v > 'b'`,
		},
		{
			// A WHERE conjunct on the inner side, applied AFTER the
			// NULL-extension, which must never key the seek.
			"where-on-inner",
			[]string{`CREATE INDEX ikv ON i(k,v)`},
			`SELECT o.tag, i.v FROM o LEFT JOIN i ON i.k = o.x WHERE i.v IS NULL OR i.v < 'd'`,
		},
		{
			// A LEFT join nested under an INNER one: both levels are seeked.
			"inner-then-left",
			[]string{`CREATE INDEX ikv ON i(k,v)`, `CREATE TABLE m(j INT, s TEXT)`,
				`INSERT INTO m VALUES(1,'m1'),(2,'m2'),(1,'m3')`},
			`SELECT o.tag, m.s, i.v FROM o JOIN m ON m.j = o.x LEFT JOIN i ON i.k = m.j`,
		},
		{
			// The rowid path: an INTEGER PRIMARY KEY inner side is a point
			// lookup, so the seeked set is at most one row and the order cannot
			// change -- what this checks is that the NULL-extension survives it.
			"rowid-seek",
			[]string{`CREATE TABLE r(id INTEGER PRIMARY KEY, s TEXT)`,
				`INSERT INTO r VALUES(1,'r1'),(2,'r2'),(3,'r3')`},
			`SELECT o.tag, r.s FROM o LEFT JOIN r ON r.id = o.x`,
		},
		{
			// A RIGHT join stays declined by the seek; asserted here so a later
			// change that lifts that decline has to face the oracle.
			"right-join-unchanged",
			[]string{`CREATE INDEX ikv ON i(k,v)`},
			`SELECT o.tag, i.v FROM o RIGHT JOIN i ON i.k = o.x`,
		},
	}

	wrong := 0
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stmts := append([]string{}, r31LeftSchema...)
			stmts = append(stmts, c.ddl...)
			last := len(stmts)
			stmts = append(stmts, c.query)

			cgo := run(t, "cgo", stmts)
			mush := run(t, "musql", stmts)
			cb, _ := json.Marshal(cgo[last])
			mb, _ := json.Marshal(mush[last])
			if string(cb) == string(mb) {
				return
			}
			if mush[last]["kind"] == "error" && cgo[last]["kind"] != "error" {
				t.Logf("declined (not a wrong answer): %s", c.query)
				return
			}
			wrong++
			t.Logf("WRONG (counted against the ceiling)\n  %s\n  cgo:    %s\n  musql: %s",
				c.query, cb, mb)
		})
	}
	if wrong > r31LeftKnownWrong {
		t.Errorf("%d wrong answers, ceiling is %d -- a NEW one appeared", wrong, r31LeftKnownWrong)
	}
	if wrong < r31LeftKnownWrong {
		t.Logf("only %d wrong answers left of %d: lower r31LeftKnownWrong", wrong, r31LeftKnownWrong)
	}
}
