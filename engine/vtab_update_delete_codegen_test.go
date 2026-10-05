package engine

// Tests UPDATE and DELETE codegen for virtual tables (fts3/fts4, fts5, rtree).

import (
	"errors"
	"strings"
	"testing"
)

var vtabWriteShapes = []conflictShapeCase{
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `UPDATE f SET x='b'`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `DELETE FROM f`},

	// fts3, fts5, rtree and rtree_i32 -- the rest of the writable module set.
	{[]string{`CREATE VIRTUAL TABLE f USING fts3(x)`, `INSERT INTO f VALUES('a')`}, `UPDATE f SET x='b'`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts3(x)`, `INSERT INTO f VALUES('a')`}, `DELETE FROM f WHERE x='a'`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts5(x)`, `INSERT INTO f VALUES('a')`}, `UPDATE f SET x='b'`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts5(x)`, `INSERT INTO f VALUES('a')`}, `DELETE FROM f WHERE rowid=1`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `INSERT INTO r VALUES(1,0.0,1.0)`}, `UPDATE r SET x1=9.0 WHERE id=1`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `INSERT INTO r VALUES(1,0.0,1.0)`}, `DELETE FROM r WHERE x0<5.0`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree_i32(id,x0,x1)`, `INSERT INTO r VALUES(1,0,1)`}, `UPDATE r SET x1=9 WHERE id=1`},
	{[]string{`CREATE VIRTUAL TABLE r USING rtree_i32(id,x0,x1)`, `INSERT INTO r VALUES(1,0,1)`}, `DELETE FROM r`},

	// Real EXPRESSIONS in the WHERE and the SET, which is the whole point:
	// every one of them is coded into the scan as opcodes (update.c:1282).
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`, `INSERT INTO f VALUES('a','b')`}, `UPDATE f SET x=upper(x)||'!' WHERE length(y)=1`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `DELETE FROM f WHERE x IN ('a','b') AND docid BETWEEN 1 AND 9`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `UPDATE f SET x=CASE WHEN docid=1 THEN 'one' ELSE 'other' END`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`, `INSERT INTO f VALUES('a','b')`}, `UPDATE f SET x=y, y=x`},

	// The hidden and pseudo columns a vtab write scope names: fts3's "docid",
	// the rowid pseudo-column, and a "languageid=" table's own hidden column.
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `UPDATE f SET docid=9 WHERE docid=1`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `DELETE FROM f WHERE rowid=1`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(a,languageid=l)`, `INSERT INTO f(docid,a,l) VALUES(1,'x',0)`}, `UPDATE f SET a='y' WHERE l=0`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(a,languageid=l)`, `INSERT INTO f(docid,a,l) VALUES(1,'x',0)`}, `DELETE FROM f WHERE l=0`},

	// A bound parameter (OpVariable), and a schema-qualified target (the
	// qualifier picks a catalog -- delete.c:345's sqlite3SrcListLookup is the
	// same routine for a vtab).
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `DELETE FROM f WHERE x=?`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `UPDATE main.f SET x=? WHERE docid=?`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `DELETE FROM main.f`},

	// An OR clause the module itself can express: the emitter only codes
	// values, so the clause reaches updateVtab/updateFts3's own gate unchanged.
	{[]string{`CREATE VIRTUAL TABLE f USING fts5(x)`, `INSERT INTO f VALUES('a')`}, `UPDATE OR IGNORE f SET x='b'`},
	{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`}, `UPDATE OR REPLACE f SET docid=9`},

	// The vtab write is itself a TRIGGER BODY statement, so its WHERE names
	// OLD.* -- resolved by OpParam, out of the firing statement's row
	// registers. This is alter.test 17.100's own shape, named in deleteVtab's
	// doc comment.
	{[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('gone')`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN DELETE FROM f WHERE x=old.a; END`,
		`INSERT INTO t VALUES('gone')`},
		`DELETE FROM t`},
	{[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('old')`,
		`CREATE TRIGGER ti AFTER INSERT ON t BEGIN UPDATE f SET x=new.a; END`},
		`INSERT INTO t VALUES('new')`},
}

// TestVtabWriteCompilesToBytecode is the RULE #1 assertion no differential
// harness can make: every shape above has to COMPILE. A shape this emitter
// stops modelling is a hard prepare error now, and the two trigger-body cases
// carry their vtab write as a SUB-PROGRAM of the firing statement, so a
// regression in either one shows up here and nowhere else.
func TestVtabWriteCompilesToBytecode(t *testing.T) {
	for i, tc := range vtabWriteShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// vtabWriteLoop is the structural shape TestVtabWriteEmitsARealScan looks for
// in one program: the addresses of the scan's parts.
type vtabWriteLoop struct {
	open, rewind, rowid, next, write int
	rowIfNot                         []int
	writeRow                         []int
}

// findVtabWriteLoop locates the vtab write scan in p, or returns nil if p has
// no OpVWrite at all. Addresses are -1 when the part is missing, which is what
// the assertions below are actually looking for.
func findVtabWriteLoop(p *Program) *vtabWriteLoop {
	lp := &vtabWriteLoop{open: -1, rewind: -1, rowid: -1, next: -1, write: -1}
	found := false
	for i := range p.Insns {
		switch in := p.Insns[i]; in.Op {
		case OpOpenDerived:
			if ds, ok := in.P4.(*derivedSource); ok && ds.vtabWrite != nil && lp.open < 0 {
				lp.open = i
			}
		case OpRewind:
			if lp.rewind < 0 {
				lp.rewind = i
			}
		case OpRowid:
			if lp.rowid < 0 {
				lp.rowid = i
			}
		case OpIfNot:
			lp.rowIfNot = append(lp.rowIfNot, i)
		case OpVWriteRow:
			lp.writeRow = append(lp.writeRow, i)
		case OpNext:
			if lp.next < 0 {
				lp.next = i
			}
		case OpVWrite:
			lp.write = i
			found = true
		}
	}
	if !found {
		return nil
	}
	return lp
}

// TestVtabWriteEmitsARealScan is the non-vacuity half, and it is the assertion
// this batch turns on.
//
// "It compiles" is not the property that matters: an emitter
// that produced Init / OpVWriteRow / OpVWrite and nothing else -- no cursor, no
// loop, the WHERE never coded -- would pass that check AND would answer every
// single-row fixture correctly, because over one row "the WHERE selected it"
// and "fire once unconditionally" are indistinguishable. What makes the
// promotion real is that the row loop is a SCAN OF THE VIRTUAL TABLE, which is
// what sqlite3WhereBegin is at update.c:1273 and delete.c:526.
//
// So this asserts the parts and their ORDER: a cursor opened over the module's
// own write-side source (derivedSource.vtabWrite), an OpRewind that guards it,
// the rowid read INSIDE the loop, exactly one OpVWriteRow inside it, an OpNext
// that jumps back above that OpVWriteRow, and exactly one OpVWrite AFTER the
// loop -- the C's second loop, which runs once per statement here for the
// module-state reason vdbe_vtab_write.go records.
//
// It also asserts the WHERE is coded WHERE IT SAYS IT IS: a statement with one
// must emit a conditional skip between the loop top and the OpVWriteRow, and a
// statement without one must emit none. An emitter that dropped the WHERE
// entirely would otherwise only be caught by whichever behavioural fixture had
// a non-matching row.
func TestVtabWriteEmitsARealScan(t *testing.T) {
	for i, tc := range vtabWriteShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		programs := 0
		walkFirePlans(prog, func(p *Program) {
			lp := findVtabWriteLoop(p)
			if lp == nil {
				return
			}
			programs++
			if lp.open < 0 {
				t.Errorf("[%d] %q: OpVWrite with NO OpOpenDerived over a vtabWrite source.\n"+
					"The row loop is not a scan of the virtual table, so the WHERE is not coded against\n"+
					"anything -- and a one-row fixture cannot tell the difference.", i, tc.stmt)
				return
			}
			if lp.rewind < lp.open {
				t.Errorf("[%d] %q: OpRewind at %d precedes the OpOpenDerived at %d", i, tc.stmt, lp.rewind, lp.open)
			}
			if lp.rowid < lp.rewind {
				t.Errorf("[%d] %q: no OpRowid inside the loop -- OpVWriteRow would record rowid 0 for every row",
					i, tc.stmt)
			}
			if len(lp.writeRow) != 1 {
				t.Errorf("[%d] %q: %d OpVWriteRow instructions, want exactly 1", i, tc.stmt, len(lp.writeRow))
				return
			}
			vr := lp.writeRow[0]
			if !(lp.rewind < vr && vr < lp.next && lp.next < lp.write) {
				t.Errorf("[%d] %q: loop is out of order -- rewind=%d writeRow=%d next=%d write=%d;\n"+
					"want rewind < writeRow < next < write (the collect must be INSIDE the scan and the\n"+
					"replay AFTER it, update.c:1320-1327 then :1348)", i, tc.stmt, lp.rewind, vr, lp.next, lp.write)
				return
			}
			if p.Insns[lp.next].P2 > vr {
				t.Errorf("[%d] %q: OpNext jumps to %d, past the OpVWriteRow at %d -- the loop body runs once",
					i, tc.stmt, p.Insns[lp.next].P2, vr)
			}
			if p.Insns[lp.rewind].P2 <= lp.next {
				t.Errorf("[%d] %q: OpRewind's empty-table exit is %d, inside the loop (OpNext is at %d)",
					i, tc.stmt, p.Insns[lp.rewind].P2, lp.next)
			}
			// The WHERE, coded between the loop top and the collect. Whether
			// there IS one is read off the plan's own statement rather than out
			// of the SQL text, because an expression can emit an OpIfNot of its
			// own -- "SET x=CASE WHEN ... END" does.
			plan, ok := p.Insns[lp.write].P4.(*vtabWritePlan)
			if !ok || plan == nil || plan.vm == nil || (plan.del == nil) == (plan.upd == nil) {
				t.Errorf("[%d] %q: OpVWrite with no usable plan (%T)", i, tc.stmt, p.Insns[lp.write].P4)
				return
			}
			wantWhere := plan.del != nil && plan.del.where != nil
			if plan.upd != nil {
				wantWhere = plan.upd.where != nil
			}
			// A ROW skip is one whose jump target is PAST the collect; an
			// OpIfNot an expression emits for its own control flow (a CASE's
			// arm test, vdbe_codegen.go) targets an address inside
			// the expression, i.e. before it.
			rowSkips := 0
			for _, a := range lp.rowIfNot {
				if a > lp.rewind && a < vr && p.Insns[a].P2 > vr {
					rowSkips++
				}
			}
			if wantWhere != (rowSkips > 0) {
				t.Errorf("[%d] %q: %d conditional row skips inside the loop, want %v.\n"+
					"A WHERE that is never coded selects every row; a skip emitted for a statement with no\n"+
					"WHERE selects none.", i, tc.stmt, rowSkips, wantWhere)
			}
		})
		if programs != 1 {
			t.Errorf("[%d] %q: %d programs in the graph carry an OpVWrite, want exactly 1", i, tc.stmt, programs)
		}
	}
}

// TestVtabUpdateFillsEverySetRegister pins the SET register block the same way
// TestVtabInsertEmitsAVInsertOpcode pins the VALUES one, and for the same
// reason: a register nothing wrote reads as NULL (a fresh register's zero
// Value), so an emitter that silently skipped one assignment would store a NULL
// into that column and only be caught by whichever behavioural case happened to
// look at it.
//
// OpVWriteRow's P1..P1+P3-1 is that block. compileVtabUpdateStmt writes into it
// with OpSCopy, or the expression compiles straight into the slot -- so a
// register is "written" if any instruction before the opcode names it as a
// destination in P2 (the value-load opcodes) or P3 (the ordinary expression
// ones).
//
// Every shape is walked, not just the UPDATEs: "P3 == 0" is exactly the DELETEs
// (they collect a rowid and no values), so they skip themselves.
func TestVtabUpdateFillsEverySetRegister(t *testing.T) {
	for i, tc := range vtabWriteShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		walkFirePlans(prog, func(p *Program) {
			for j := range p.Insns {
				in := p.Insns[j]
				if in.Op != OpVWriteRow || in.P3 == 0 {
					continue
				}
				written := map[int]bool{}
				for k := 0; k < j; k++ {
					switch p.Insns[k].Op {
					case OpSCopy, OpCopy, OpInteger, OpInt64, OpReal, OpString8, OpNull, OpBlob, OpVariable, OpRowid:
						written[p.Insns[k].P2] = true
					default:
						written[p.Insns[k].P3] = true
					}
				}
				for v := 0; v < in.P3; v++ {
					if !written[in.P1+v] {
						t.Errorf("[%d] %q: SET register %d (value %d of %d) is never written before the\n"+
							"OpVWriteRow at %d. It would READ as NULL, because a fresh register's zero Value is\n"+
							"NULL -- which is exactly why no behavioural test catches this.",
							i, tc.stmt, in.P1+v, v, in.P3, j)
					}
				}
			}
		})
	}
}

// vtabWriteDeclinedShapes: shapes this compiler deliberately does NOT model.
// Each must decline CLEANLY -- half-serving one here would be a wrong answer
// rather than a gap. See compileVtabDeleteStmt / compileVtabUpdateStmt for the
// reason attached to each.
var vtabWriteDeclinedShapes = []struct {
	why string
	tc  conflictShapeCase
}{
	// The two RETURNING entries are GONE from this table -- not promoted to
	// bytecode, REJECTED. They are pinned by TestVtabReturningIsRejected below,
	// which is the assertion this list cannot make: these two statements must
	// now be prepare-time ERRORS, because that is what C SQLite makes them
	// (vtabReturningRejected has the C and the oracle measurements).
	{"a leading WITH clause (its CTE scope is not pushed for this compile)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`WITH c(z) AS (VALUES('a')) DELETE FROM f WHERE x='a'`}},
	{"INDEXED BY on the target (a virtual table has no index for the hint to name)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `CREATE TABLE o(k)`, `CREATE INDEX ix ON o(k)`,
			`INSERT INTO f VALUES('a')`}, `DELETE FROM f INDEXED BY ix WHERE x='a'`}},
	{"an AS alias on a DELETE target (the write scope is named for the TABLE)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`DELETE FROM f AS q WHERE q.x='a'`}},
	{"an AS alias on an UPDATE target",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`UPDATE f AS q SET x='b' WHERE q.x='a'`}},
	{"a subquery in WHERE (the module halves decline one, with their own wording)",
		conflictShapeCase{[]string{`CREATE TABLE s(z)`, `CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`DELETE FROM f WHERE x=(SELECT z FROM s)`}},
	{"a subquery in SET",
		conflictShapeCase{[]string{`CREATE TABLE s(z)`, `CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`UPDATE f SET x=(SELECT z FROM s)`}},
	{"a MATCH in WHERE (fts3 declines it; fts5 answers it through evalMatch)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`DELETE FROM f WHERE f MATCH 'a'`}},
	{"UPDATE ... FROM (update.c:1230's pSrc->nSrc>1 arm is a different algorithm)",
		conflictShapeCase{[]string{`CREATE TABLE m(k,nv)`, `CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`UPDATE f SET x=m.nv FROM m WHERE m.k=f.x`}},
	{"a SET target the scan scope does not name (\"no such column\", update.c:500)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`UPDATE f SET nosuchcol='b'`}},
	// fts3WriteScope names [docid, col0.., langid?] and none of the three rowid
	// spellings, so the SET-target check declines this -- which is also what
	// keeps updateFts3's only SLOTLESS target (fts3SetRowidSynonymNoOp, whose
	// right-hand side writeApplySetList evaluates in a SECOND pass) off the
	// compiled route, and with it the only way the two routes could evaluate a
	// SET list in different orders.
	// "a rowid-synonym SET target on fts4" WAS here and is LOWERED now
	// (321d9cc): the compiled validation learned updateFts3's rule -- the
	// assignment is dropped, the RHS still evaluated -- so the shape no longer
	// needs a fallback to be correct.
	{"a READ-ONLY module (fts4aux has no vtabStore at all)",
		conflictShapeCase{[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`,
			`CREATE VIRTUAL TABLE fa USING fts4aux(f)`}, `DELETE FROM fa`}},
}

// TestVtabWriteDeclinedShapesDeclineCleanly pins the OTHER half of "never
// wrong": a shape this emitter does not model must DECLINE, not be
// approximated.
//
// It requires errVDBEUnsupported EXACTLY. A shape that COMPILED would be
// served with semantics this emitter does not have, and a shape that failed
// with some other error would reach the caller as something other than "this
// engine does not model that".
func TestVtabWriteDeclinedShapesDeclineCleanly(t *testing.T) {
	for _, d := range vtabWriteDeclinedShapes {
		prog, err := compileShape(t, d.tc)
		if err != nil {
			// A hard decline is the CORRECT outcome: errVDBEUnsupported is what
			// RULE #1 prescribes for a shape this emitter does not model. What
			// the case pins is the half that matters -- the shape must not be
			// SERVED with semantics this emitter does not have.
			if !errors.Is(err, errVDBEUnsupported) {
				t.Errorf("%q declined with %v, which is not errVDBEUnsupported.\n"+
					"A shape this emitter does not model must decline CLEANLY: %s.", d.tc.stmt, err, d.why)
			}
			continue
		}
		if prog != nil {
			t.Errorf("%q COMPILED, but this batch does not model it: %s.\n"+
				"Serving it here without the semantics the shape needs is a WRONG ANSWER,\n"+
				"not a promotion (AGENTS.md invariant 1).", d.tc.stmt, d.why)
		}
	}
}

// TestVtabWriteCompiledAnswers is the behavioural half: the compiled route must
// answer what the 3.53.3 oracle answers (measured when deleteVtab / updateVtab
// / deleteFromFts3 / updateFts3 were written -- see their doc comments for the
// measurements).
//
// Every fixture holds THREE rows and every WHERE selects the MIDDLE one, so a
// loop that ran once, or ignored the WHERE, or applied the SET to every row,
// fails here as well as structurally. Each case first asserts the statement
// really compiled, so a decline can never quietly turn a case into a test of
// nothing.
func TestVtabWriteCompiledAnswers(t *testing.T) {
	fts4 := []string{`CREATE VIRTUAL TABLE f USING fts4(x,y)`,
		`INSERT INTO f VALUES('a','p'),('b','q'),('c','r')`}
	fts5 := []string{`CREATE VIRTUAL TABLE f USING fts5(x,y)`,
		`INSERT INTO f VALUES('a','p'),('b','q'),('c','r')`}
	rtree := []string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`,
		`INSERT INTO r VALUES(1,0.0,1.0),(2,2.0,3.0),(3,4.0,5.0)`}
	cases := []struct {
		name  string
		setup []string
		stmt  string
		query string
		want  []string
	}{
		{"fts4 UPDATE selects one of three", fts4,
			`UPDATE f SET x='Z' WHERE y='q'`, `SELECT docid,x,y FROM f ORDER BY docid`,
			[]string{"1,a,p", "2,Z,q", "3,c,r"}},
		{"fts4 UPDATE with no WHERE touches all three", fts4,
			`UPDATE f SET x='Z'`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"1,Z", "2,Z", "3,Z"}},
		{"fts4 DELETE selects one of three", fts4,
			`DELETE FROM f WHERE x='b'`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"1,a", "3,c"}},
		{"fts4 DELETE with no WHERE empties it", fts4,
			`DELETE FROM f`, `SELECT docid FROM f`, nil},
		{"fts4 DELETE that matches nothing changes nothing", fts4,
			`DELETE FROM f WHERE x='zzz'`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"1,a", "2,b", "3,c"}},
		{"fts4 SET is simultaneous, not chained", fts4,
			`UPDATE f SET x=y, y=x WHERE docid=2`, `SELECT x,y FROM f WHERE docid=2`,
			[]string{"q,b"}},
		{"fts4 SET reads the OLD row", fts4,
			`UPDATE f SET x=x||y WHERE docid=2`, `SELECT x FROM f WHERE docid=2`, []string{"bq"}},
		// Every row gets its OWN computed value. This is what catches an
		// OpVWriteRow that ALIASED the SET register block instead of copying it:
		// each row overwrites the same registers, so all three rows would end
		// up holding the LAST row's value.
		{"fts4 UPDATE computes a different value per row", fts4,
			`UPDATE f SET x=x||'!'`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"1,a!", "2,b!", "3,c!"}},
		{"fts4 docid moves", fts4,
			`UPDATE f SET docid=9 WHERE x='b'`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"1,a", "3,c", "9,b"}},
		// The index really is rewritten, not just %_content: a MATCH after the
		// write is the only thing that says so.
		{"fts4 UPDATE rewrites the index", fts4,
			`UPDATE f SET x='zeta' WHERE docid=2`, `SELECT docid FROM f WHERE f MATCH 'zeta'`,
			[]string{"2"}},
		{"fts4 DELETE removes the postings", fts4,
			`DELETE FROM f WHERE docid=2`, `SELECT docid FROM f WHERE f MATCH 'b'`, nil},

		{"fts5 UPDATE selects one of three", fts5,
			`UPDATE f SET x='Z' WHERE y='q'`, `SELECT rowid,x,y FROM f ORDER BY rowid`,
			[]string{"1,a,p", "2,Z,q", "3,c,r"}},
		{"fts5 DELETE selects one of three", fts5,
			`DELETE FROM f WHERE x='b'`, `SELECT rowid,x FROM f ORDER BY rowid`,
			[]string{"1,a", "3,c"}},
		{"fts5 UPDATE rewrites the index", fts5,
			`UPDATE f SET x='zeta' WHERE rowid=2`, `SELECT rowid FROM f WHERE f MATCH 'zeta'`,
			[]string{"2"}},

		// rtree COERCES every coordinate to REAL inside the module
		// (coerceCoords, vtab_rtree.go), so the stored value is not the
		// register's own. Pinned because a compiled emitter that "helpfully"
		// applied an affinity of its own would still pass every count check.
		{"rtree UPDATE stores REAL coordinates", rtree,
			`UPDATE r SET x1=9 WHERE id=2`, `SELECT id,typeof(x1),x1 FROM r ORDER BY id`,
			[]string{"1,real,1", "2,real,9", "3,real,5"}},
		{"rtree DELETE selects one of three", rtree,
			`DELETE FROM r WHERE x0>1.0 AND x0<3.0`, `SELECT id FROM r ORDER BY id`,
			[]string{"1", "3"}},

		// A bound parameter in both halves.
		{"fts4 parameters in WHERE and SET", fts4,
			`UPDATE f SET x=? WHERE y=?`, `SELECT docid,x FROM f ORDER BY docid`,
			[]string{"1,a", "2,P1", "3,c"}},

		// A trigger body's vtab DELETE, whose WHERE names OLD.* -- resolved by
		// OpParam, out of the firing statement's row registers.
		{"trigger body DELETE from an fts4 table",
			[]string{`CREATE TABLE t(a)`, `CREATE VIRTUAL TABLE f USING fts4(x)`,
				`INSERT INTO f VALUES('a'),('b'),('c')`, `INSERT INTO t VALUES('b')`,
				`CREATE TRIGGER td AFTER DELETE ON t BEGIN DELETE FROM f WHERE x=old.a; END`},
			`DELETE FROM t`, `SELECT x FROM f ORDER BY docid`, []string{"a", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("compile %q: %v", tc.stmt, cerr)
			}
			var args []Value
			if strings.Contains(tc.stmt, "?") {
				args = []Value{{Typ: Text, S: []byte("P1")}, {Typ: Text, S: []byte("q")}}
			}
			if _, _, err := db.ExecArgs(tc.stmt, args); err != nil {
				t.Fatalf("exec %q: %v", tc.stmt, err)
			}
			got := rvdRowStrings(rvdQuery(t, db, tc.query))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("%q then %q: got %v, want %v", tc.stmt, tc.query, got, tc.want)
			}
		})
	}
}

// TestVtabWriteScansTheLiveRowSource is the assertion that makes
// vtabWriteRows' choice of row source testable, and it is a NEVER-WRONG
// assertion rather than a performance one.
//
// The compiled scan reads the MODULE's own write-side rows, resolved at RUN
// time. Point it at a compile-time *ReadOnlyPager snapshot instead -- which is
// what the READ path's materializeVtab would give, and what the view twin does
// -- and the same statement text run twice would replay the FIRST run's image,
// because a program carrying no WritePager is CACHED (cachedWriteProgram).
// Neither schemaGen nor txGen moves between the two runs below, so the cache
// really is hit.
//
// This is not hypothetical: it is exactly the bug batch P left in the VIEW
// path, found while writing this file and fixed in beginViewWriteScan --
// see TestViewWriteIsNotCachedAcrossAChangedBase.
func TestVtabWriteScansTheLiveRowSource(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE VIRTUAL TABLE f USING fts4(x)`,
		`INSERT INTO f VALUES('keep'),('drop')`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	const stmt = `DELETE FROM f WHERE x='drop'`
	if err := db.Exec(stmt); err != nil {
		t.Fatalf("first %q: %v", stmt, err)
	}
	// A row the FIRST compile could not have seen.
	if err := db.Exec(`INSERT INTO f VALUES('drop')`); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	n, _, err := db.ExecArgs(stmt, nil)
	if err != nil {
		t.Fatalf("second %q: %v", stmt, err)
	}
	if n != 1 {
		t.Fatalf("the second %q reported %d rows deleted, want 1.\n"+
			"A scan bound to a compile-time snapshot would not see the row inserted after it.", stmt, n)
	}
	got := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM f ORDER BY docid`))
	if strings.Join(got, "|") != "keep" {
		t.Fatalf("after two %q the table reads %v, want [keep]", stmt, got)
	}
}

// TestVtabWriteEmptySelectionStaysOnTheCompiledRoute pins the one thing
// opVWrite has to do that nothing else can: hand the module a NON-NIL selection
// when the scan matched no row at all.
//
// nil is the module halves' "no compiled row selection arrived" signal, so a
// nil selection does not merely lose the promotion -- it reaches
// writeRowSelected (vtab_write.go), which has no second route left to decide
// the selection with and raises errVDBEUnsupported. The shape below is
// deleteVtab's own alter.test 17.100 case, and its WHERE matches nothing, so
// the mutation's answer is not a different row set but a hard failure for a
// statement that must succeed.
//
// MEASURED: this is the test that catches "sel = vtabWriteRowSet{} -> sel =
// nil". Every other case in this file escaped it, because a WHERE that selects
// nothing and is then decided a SECOND time still selects nothing -- the answer
// only diverges when that second decision cannot RUN at all.
func TestVtabWriteEmptySelectionStaysOnTheCompiledRoute(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE t(a)`,
		`CREATE VIRTUAL TABLE f USING fts4(x)`,
		`INSERT INTO f VALUES('other')`,
		`INSERT INTO t VALUES('nomatch')`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN DELETE FROM f WHERE x=old.a; END`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	if _, cerr := db.compileWrite(`DELETE FROM t`); cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if err := db.Exec(`DELETE FROM t`); err != nil {
		t.Fatalf("DELETE FROM t: %v\n"+
			"The body's vtab DELETE matched no row, so its selection is EMPTY -- and an empty\n"+
			"selection that reaches the module as nil is read as NO compiled selection at all,\n"+
			"which writeRowSelected has nothing left to serve.", err)
	}
	got := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM f`))
	if strings.Join(got, "|") != "other" {
		t.Fatalf("f reads %v, want [other] -- the body's WHERE matches no row of f", got)
	}
}

// TestVtabWriteMarksTheConnectionStateBeforeTheWhere pins the ORDER of the two
// session flags deleteVtab/updateVtab set, which the compiled route has to
// reproduce from a different place.
//
// markConnStateOpaque (conn_state.go) has to run BEFORE a single WHERE
// expression is evaluated. deleteVtab/updateVtab call it at their own top, but
// they run at OpVWrite -- only AFTER the whole scan -- so this route calls it
// again at the scan open (vtabWriteRows). It is observable: the flag makes
// total_changes() and last_insert_rowid() DECLINE for the rest of the session,
// so a WHERE that calls one of them ERRORS. Leave the marking to OpVWrite alone
// and those WHEREs answer instead.
//
// MEASURED: deleting those two lines from vtabWriteRows fails NOTHING else in
// this file, because the module half sets both again on its way through. This
// is the only test that sees the order.
func TestVtabWriteMarksTheConnectionStateBeforeTheWhere(t *testing.T) {
	for _, tc := range []struct {
		stmt string
		want string
	}{
		{`DELETE FROM f WHERE total_changes()>=0`,
			"engine: total_changes() after a virtual-table write is not reproducible on this engine"},
		{`UPDATE f SET x='z' WHERE total_changes()>=0`,
			"engine: total_changes() after a virtual-table write is not reproducible on this engine"},
		{`DELETE FROM f WHERE last_insert_rowid()>=0`,
			"engine: last_insert_rowid() after a virtual-table write is not reproducible on this engine"},
	} {
		t.Run(tc.stmt, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range []string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a'),('b')`} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			err = db.Exec(tc.stmt)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("%q: got %v, want the error %q.\n"+
					"markConnStateOpaque must run BEFORE the WHERE is evaluated, which on this route\n"+
					"means at the scan open and not at OpVWrite.", tc.stmt, err, tc.want)
			}
			got := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM f ORDER BY docid`))
			if strings.Join(got, "|") != "a|b" {
				t.Fatalf("the failed %q changed the table to %v", tc.stmt, got)
			}
		})
	}
}

// TestVtabWriteReachesTheFile is the DURABILITY assertion, and it is the one
// every "does it answer correctly" test above cannot make.
//
// compileVtabDeleteStmt/compileVtabUpdateStmt deliberately emit NO
// OpChangeCounter, because deleteVtab/updateVtab call db.markFileChanged()
// themselves and do it unconditionally -- stronger than OpChangeCounter, whose
// runWrite half is additionally gated on wc.wroteRows(). Since 4938066 doCommit
// short-circuits to success whenever commitIsNoOp() does, so a statement that
// marks nothing returns success, answers every in-session query correctly, and
// silently drops its write at Close. That is the defect class 0a9bbc1 and
// 7a39a8c were both about; this is the only thing standing between it and this
// emitter.
func TestVtabWriteReachesTheFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create string
		seed   string
		stmt   string
		query  string
		want   string
	}{
		{"fts4 UPDATE", `CREATE VIRTUAL TABLE v USING fts4(x)`, `INSERT INTO v VALUES('before')`,
			`UPDATE v SET x='after'`, `SELECT x FROM v`, "after"},
		{"fts4 DELETE", `CREATE VIRTUAL TABLE v USING fts4(x)`, `INSERT INTO v VALUES('a'),('b')`,
			`DELETE FROM v WHERE x='a'`, `SELECT x FROM v`, "b"},
		{"fts5 UPDATE", `CREATE VIRTUAL TABLE v USING fts5(x)`, `INSERT INTO v VALUES('before')`,
			`UPDATE v SET x='after'`, `SELECT x FROM v`, "after"},
		{"fts5 DELETE", `CREATE VIRTUAL TABLE v USING fts5(x)`, `INSERT INTO v VALUES('a'),('b')`,
			`DELETE FROM v WHERE x='a'`, `SELECT x FROM v`, "b"},
		{"rtree UPDATE", `CREATE VIRTUAL TABLE v USING rtree(id,x0,x1)`, `INSERT INTO v VALUES(1,0.0,1.0)`,
			`UPDATE v SET x1=9.0`, `SELECT x1 FROM v`, "9"},
		{"rtree DELETE", `CREATE VIRTUAL TABLE v USING rtree(id,x0,x1)`,
			`INSERT INTO v VALUES(1,0.0,1.0),(2,2.0,3.0)`, `DELETE FROM v WHERE id=1`, `SELECT id FROM v`, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/vw.musq"
			db, err := Create(path)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := db.Exec(tc.create); err != nil {
				t.Fatalf("%q: %v", tc.create, err)
			}
			if err := db.Exec(tc.seed); err != nil {
				t.Fatalf("%q: %v", tc.seed, err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close (seed): %v", err)
			}

			// A SECOND session, so the write is the only thing it does -- with
			// the CREATE/INSERT in the same session their own markFileChanged
			// would mask a missing one here.
			db2, err := OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite: %v", err)
			}
			if _, cerr := db2.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			if err := db2.Exec(tc.stmt); err != nil {
				t.Fatalf("%q: %v", tc.stmt, err)
			}
			if err := db2.Close(); err != nil {
				t.Fatalf("Close (write): %v", err)
			}

			db3, err := OpenWrite(path)
			if err != nil {
				t.Fatalf("OpenWrite (readback): %v", err)
			}
			defer db3.Discard()
			got := rvdRowStrings(rvdQuery(t, db3, tc.query))
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("the compiled vtab %s did not survive Close: got %v, want [%s].\n"+
					"deleteVtab/updateVtab's own markFileChanged is what this emitter relies on in place of\n"+
					"OpChangeCounter -- without it commitIsNoOp throws the write away.", tc.name, got, tc.want)
			}
		})
	}
}

// TestVtabWriteReportsRowsAffected pins the counter opVWrite has to publish by
// hand: nothing else on this route assigns wctx.rowsAffected, so both
// ExecArgs's count and changes() come from that one line.
func TestVtabWriteReportsRowsAffected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		stmt  string
		want  int64
	}{
		{"fts4 DELETE of two of three",
			[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a'),('b'),('c')`},
			`DELETE FROM f WHERE x IN ('a','c')`, 2},
		{"fts4 UPDATE of all three",
			[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a'),('b'),('c')`},
			`UPDATE f SET x='z'`, 3},
		{"fts4 DELETE that matches nothing",
			[]string{`CREATE VIRTUAL TABLE f USING fts4(x)`, `INSERT INTO f VALUES('a')`},
			`DELETE FROM f WHERE x='zzz'`, 0},
		{"rtree DELETE of one of two",
			[]string{`CREATE VIRTUAL TABLE r USING rtree(id,x0,x1)`, `INSERT INTO r VALUES(1,0.0,1.0),(2,2.0,3.0)`},
			`DELETE FROM r WHERE id=2`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			for _, s := range tc.setup {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("compile: %v", cerr)
			}
			n, _, err := db.ExecArgs(tc.stmt, nil)
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if n != tc.want {
				t.Fatalf("ExecArgs reported %d rows affected, want %d (opVWrite must set wctx.rowsAffected)", n, tc.want)
			}
			if got := db.nChange; int64(got) != tc.want {
				t.Fatalf("changes() is %d, want %d", got, tc.want)
			}
		})
	}
}

// TestViewWriteIsNotCachedAcrossAChangedBase pins the LIVE bug this batch found
// in the neighbouring view path (batch P) and fixed in beginViewWriteScan.
//
// compileViewMaterialize takes a SnapshotPager at COMPILE time and stashes it
// on the derivedSource, so a view UPDATE/DELETE program is a frozen image of
// the base table. Without Program.WritePager set, cachedWriteProgram happily
// reuses that program for the same SQL text, and the second run replays the
// FIRST run's image: below, the second "DELETE FROM v" logged 1 alone where the
// oracle logs 1 and 2. Neither schemaGen nor txGen moves between the runs, so
// the cache really is hit; a subquery in the WHERE hid it, because that path
// already declared its own frozen pager.
//
// It lives in this file rather than view_update_delete_codegen_test.go because
// it is this batch's finding, and because the same question -- "what does the
// scan actually read?" -- is what TestVtabWriteScansTheLiveRowSource asks of
// the virtual-table twin.
func TestViewWriteIsNotCachedAcrossAChangedBase(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	for _, s := range []string{
		`CREATE TABLE b(a,c)`,
		`CREATE TABLE log(x)`,
		`CREATE VIEW v AS SELECT a,c FROM b`,
		`CREATE TRIGGER vd INSTEAD OF DELETE ON v BEGIN INSERT INTO log VALUES(old.a); END`,
		`CREATE TRIGGER vu INSTEAD OF UPDATE ON v BEGIN INSERT INTO log VALUES(old.a*100); END`,
		`INSERT INTO b VALUES(1,1)`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	for _, s := range []string{`DELETE FROM v`, `UPDATE v SET c=9`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("first %q: %v", s, err)
		}
	}
	// A row neither first compile could have seen. The INSTEAD OF bodies write
	// only log, so b still holds row 1 as well.
	if err := db.Exec(`INSERT INTO b VALUES(2,2)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, s := range []string{`DELETE FROM v`, `UPDATE v SET c=9`} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("second %q: %v", s, err)
		}
	}
	got := rvdRowStrings(rvdQuery(t, db, `SELECT x FROM log ORDER BY rowid`))
	want := []string{"1", "100", "1", "2", "100", "200"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("log reads %v, want %v.\n"+
			"A view write program that carries a compile-time materialization snapshot must not be\n"+
			"CACHED -- the second run of each statement would replay the first run's image.", got, want)
	}
}
