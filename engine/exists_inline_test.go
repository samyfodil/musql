package engine

import (
	"fmt"
	"testing"
)

// TestExistsInlineMatchesSubProgram: a correlated EXISTS relocated into its
// parent as a subroutine (existsInlinePeephole) must answer exactly as the
// per-row sub-program does -- the plain side of the pair runs with the
// peepholes off. The shapes stress the relocation: NOT EXISTS, several EXISTS
// in one program, EXISTS in the select list, a reference two levels out, NULL
// and non-integer keys, an empty inner table, and rows the log holds.
func TestExistsInlineMatchesSubProgram(t *testing.T) {
	stmts := []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, bid, k INTEGER, s TEXT)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, label TEXT, w REAL)`,
		`CREATE TABLE e (id INTEGER PRIMARY KEY, x INTEGER)`,
	}
	for i := 0; i < 400; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO b VALUES (%d, 'l%d', %d.5)`, i*3, i%7, i%11))
	}
	keys := []string{"%d", "%d", "%d", "NULL", "'%d'", "%d.0", "%d.5", "-%d"}
	for i := 1; i <= 600; i++ {
		k := keys[i%len(keys)]
		if k != "NULL" {
			k = fmt.Sprintf(k, i)
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %d, 's%d')`, i, k, i%5, i%13))
	}
	p := newSegPair(t, stmts...)
	queries := []string{
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid)`,
		`SELECT count(*) FROM t WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.w > 3)`,
		`SELECT t.id FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid) AND NOT EXISTS (SELECT 1 FROM b WHERE b.id = t.id) ORDER BY t.id`,
		`SELECT t.id, EXISTS (SELECT 1 FROM b WHERE b.id = t.bid), NOT EXISTS (SELECT 1 FROM b WHERE b.label = t.s) FROM t ORDER BY t.id`,
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.label = 'l' || t.k AND b.id > t.id)`,
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM e WHERE e.id = t.bid)`, // empty inner table
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.rowid = t.bid AND b.label IS NOT NULL)`,
		`SELECT k, count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid) GROUP BY k ORDER BY k`,
		`SELECT count(*) FROM t AS o WHERE EXISTS (SELECT 1 FROM b WHERE b.id = o.bid AND EXISTS (SELECT 1 FROM t AS i WHERE i.id = o.id AND i.k = b.id % 5))`,
		`SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM b WHERE b.id = t.bid AND b.id < ?)`,
	}
	run := func(stage string) {
		for _, q := range queries {
			_, want, err := p.plain(q, Value{Typ: Int, I: 600})
			if err != nil {
				t.Fatalf("%s plain %s: %v", stage, q, err)
			}
			_, got, err := p.fast(q, Value{Typ: Int, I: 600})
			if err != nil {
				t.Fatalf("%s fast %s: %v", stage, q, err)
			}
			if g, w := fmt.Sprint(typedRows(got)), fmt.Sprint(typedRows(want)); g != w {
				t.Errorf("%s %s:\n inlined     %.300s\n sub-program %.300s", stage, q, g, w)
			}
		}
	}
	run("segments")
	p.delta(`INSERT INTO b VALUES (1, 'new', 1.5)`, `DELETE FROM b WHERE id = 3`, `UPDATE t SET bid = 1 WHERE id = 2`, `INSERT INTO e VALUES (5, 5)`)
	run("with log")
}

// TestExistsInlineRewritesTheProgram is the gate's non-vacuity check: the
// shape this file targets must actually lose its OpExists.
func TestExistsInlineRewritesTheProgram(t *testing.T) {
	prog := &Program{NReg: 3, NCursors: 1, Insns: []Instruction{
		{Op: OpInit, P2: 1},
		{Op: OpExists, P1: 2, P5: p5Correlated, P4: &Program{NReg: 2, NCursors: 1, Insns: []Instruction{
			{Op: OpInit, P2: 1},
			{Op: OpOpenRead, P1: 0},
			{Op: OpOuterColumn, P1: 0, P2: 1, P3: 0, P5: 1},
			{Op: OpSeekRowidHint, P1: 0, P2: 0},
			{Op: OpRewind, P1: 0, P2: 7},
			{Op: OpResultRow, P1: 1, P2: 1},
			{Op: OpNext, P1: 0, P2: 5},
			{Op: OpClose, P1: 0},
			{Op: OpHalt},
		}}},
		{Op: OpHalt},
	}}
	existsInlinePeephole(prog)
	if prog.Insns[1].Op != OpGosub {
		t.Fatalf("OpExists was not replaced: %v", prog.Insns[1].Op)
	}
	for _, in := range prog.Insns {
		if in.Op == OpOuterColumn {
			t.Fatal("a level-1 OuterColumn survived the relocation")
		}
	}
	if prog.NReg != 3+2+1 || prog.NCursors != 2 {
		t.Fatalf("NReg %d NCursors %d", prog.NReg, prog.NCursors)
	}
}
