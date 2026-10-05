package engine

// This file tests the compilation of direct schema-catalog writes
// (INSERT INTO / UPDATE / DELETE FROM sqlite_master under PRAGMA writable_schema=ON)
// to bytecode rather than falling back to tree-walking.

import (
	"errors"
	"strings"
	"testing"
)

// wsOn is the setup prefix every case shares.
func wsOn(stmts ...string) []string {
	return append([]string{`PRAGMA writable_schema=ON`}, stmts...)
}

// schemaCatalogWriteShapes: statements whose promotion this batch is about.
var schemaCatalogWriteShapes = []conflictShapeCase{
	// The three census shapes, verbatim from vdbe_total_test.go's residual list.
	{[]string{`CREATE TABLE t(a,b)`, `PRAGMA writable_schema=ON`},
		`UPDATE sqlite_master SET sql='CREATE TABLE t(a,b,c)' WHERE name='t'`},
	{[]string{`CREATE TABLE t(a,b)`, `PRAGMA writable_schema=ON`},
		`DELETE FROM sqlite_master WHERE name='t'`},
	{[]string{`CREATE TABLE t(a,b)`, `PRAGMA writable_schema=ON`},
		`INSERT INTO sqlite_master VALUES('table','x','x',5,'CREATE TABLE x(a)')`},

	// The "sqlite_schema" spelling and the "main." qualifier, both of which
	// writableSchemaTarget routes here.
	{wsOn(`CREATE TABLE t(a,b)`), `UPDATE sqlite_schema SET sql='nonsense' WHERE name='t'`},
	{wsOn(`CREATE TABLE t(a,b)`), `DELETE FROM main.sqlite_master WHERE type='table'`},
	{wsOn(`CREATE TABLE t(a,b)`), `INSERT INTO main.sqlite_schema(type,name) VALUES('view','v')`},

	// Real EXPRESSIONS in the WHERE and the SET -- coded as opcodes, not
	// handed to writeRowSelected / writeApplySetList per row.
	// strict2.test's own "sql=(sql||'STRICT')" is here because it is the one
	// mined shape whose SET right-hand side READS the row it is rewriting.
	{wsOn(`CREATE TABLE t1(id ANY PRIMARY KEY, x TEXT)`), `UPDATE sqlite_schema SET sql=(sql||' STRICT') WHERE name='t1'`},
	{wsOn(`CREATE TABLE t(a)`, `CREATE INDEX i ON t(a)`), `UPDATE sqlite_master SET sql=upper(sql) WHERE type='index' AND length(name)=1`},
	{wsOn(`CREATE TABLE t(a)`), `UPDATE sqlite_master SET sql=CASE WHEN type='table' THEN 'x' ELSE sql END`},
	{wsOn(`CREATE TABLE t(a)`, `CREATE TABLE u(a)`), `DELETE FROM sqlite_master WHERE name IN ('t','u') AND type<>'index'`},
	{wsOn(`CREATE TABLE t(a)`), `UPDATE sqlite_master SET name='x1i', tbl_name='x1' WHERE name='t'`},

	// The rowid pseudo-column, in a WHERE and in a SET right-hand side --
	// pragma.test's own "WHERE rowid=2" idiom. Allowed only while no catalog
	// row has ever been removed (writableSchemaPreflight), which is the state
	// these fixtures are in.
	{wsOn(`CREATE TABLE t(a)`, `CREATE TABLE u(a)`), `UPDATE sqlite_schema SET rootpage=3 WHERE rowid=2`},
	{wsOn(`CREATE TABLE t(a)`), `DELETE FROM sqlite_master WHERE rowid=1`},
	{wsOn(`CREATE TABLE t(a)`), `INSERT INTO sqlite_master VALUES('table','y','y',(SELECT max(rootpage)+1 FROM sqlite_master),'CREATE TABLE y(a)')`},

	// A WHERE-less statement of each verb: the row loop must still be a scan.
	{wsOn(`CREATE TABLE t(a)`), `UPDATE sqlite_master SET sql='nonsense'`},
	{wsOn(`CREATE TABLE t(a)`), `DELETE FROM sqlite_master`},

	// A bound parameter (OpVariable), in the WHERE and in the SET.
	{wsOn(`CREATE TABLE t(a)`), `UPDATE sqlite_master SET sql=? WHERE name=?`},
	{wsOn(`CREATE TABLE t(a)`), `DELETE FROM sqlite_master WHERE name=?`},
	{wsOn(`CREATE TABLE t(a)`), `INSERT INTO sqlite_master VALUES(?,?,?,?,?)`},

	// A SUBQUERY, which is NOT declined: it compiles against the write path's
	// PRE-STATEMENT snapshot pager, so it reads the catalog as it stood before
	// this statement. pager1.test's own "SET rootpage=(SELECT ...)" shape.
	{wsOn(`CREATE TABLE t(a)`, `CREATE TABLE u(a)`),
		`UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM sqlite_schema WHERE name='u') WHERE name='t'`},
	{wsOn(`CREATE TABLE t(a)`, `CREATE TABLE keep(a)`), `DELETE FROM sqlite_master WHERE name NOT IN (SELECT name FROM sqlite_master WHERE name='keep')`},

	// An explicit column list on the INSERT, and a multi-row VALUES.
	{wsOn(`CREATE TABLE t(a)`), `INSERT INTO sqlite_master(type,name) VALUES('x','y')`},
	{wsOn(`CREATE TABLE t(a)`), `INSERT INTO sqlite_master VALUES('a','b','c',1,'d'),('e','f','g',2,'h')`},
}

// TestSchemaCatalogWriteCompilesToBytecode verifies that all schema catalog
// write shapes compile to bytecode.
func TestSchemaCatalogWriteCompilesToBytecode(t *testing.T) {
	for i, tc := range schemaCatalogWriteShapes {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: [%d] %q should COMPILE, got error: %v", i, tc.stmt, err)
		}
	}
}

// TestSchemaCatalogWriteTakesNoEvaluatorSeam verifies that schema writes use
// registers for per-row values, not the evaluator.
func TestSchemaCatalogWriteTakesNoEvaluatorSeam(t *testing.T) {
	for i, tc := range schemaCatalogWriteShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		writes, pre := 0, 0
		for j := range prog.Insns {
			switch prog.Insns[j].Op {
			case OpSchemaWrite:
				writes++
				plan, ok := prog.Insns[j].P4.(*schemaWritePlan)
				if !ok || plan == nil {
					t.Errorf("[%d] %q: OpSchemaWrite with no usable plan (%T)", i, tc.stmt, prog.Insns[j].P4)
					continue
				}
				n := 0
				for _, set := range []bool{plan.ins != nil, plan.upd != nil, plan.del != nil} {
					if set {
						n++
					}
				}
				if n != 1 {
					t.Errorf("[%d] %q: OpSchemaWrite's plan names %d statements, want exactly 1", i, tc.stmt, n)
				}
			case OpSchemaWritePre:
				pre++
				if j != 1 {
					t.Errorf("[%d] %q: OpSchemaWritePre is at %d, not immediately after OpInit.\n"+
						"Everything it asks -- sqlite3IsReadOnly, the SET/IDLIST names, the VALUES arity -- is\n"+
						"asked at PREPARE time in the C, before any value is coded.", i, tc.stmt, j)
				}
			}
		}
		if writes != 1 || pre != 1 {
			t.Errorf("[%d] %q: %d OpSchemaWrite and %d OpSchemaWritePre, want exactly 1 of each",
				i, tc.stmt, writes, pre)
		}
	}
}

// schemaWriteLoop is the structural shape TestSchemaCatalogWriteEmitsARealScan
// looks for: the addresses of the scan's parts. -1 when the part is missing,
// which is what the assertions are actually looking for.
type schemaWriteLoop struct {
	pre, open, rewind, rowid, next, write int
	rowIfNot                              []int
	writeRow                              []int
}

func findSchemaWriteLoop(p *Program) *schemaWriteLoop {
	lp := &schemaWriteLoop{pre: -1, open: -1, rewind: -1, rowid: -1, next: -1, write: -1}
	found := false
	for i := range p.Insns {
		switch in := p.Insns[i]; in.Op {
		case OpSchemaWritePre:
			if lp.pre < 0 {
				lp.pre = i
			}
		case OpOpenDerived:
			if ds, ok := in.P4.(*derivedSource); ok && ds.schemaWrite && lp.open < 0 {
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
		case OpSchemaWriteRow:
			lp.writeRow = append(lp.writeRow, i)
		case OpNext:
			if lp.next < 0 {
				lp.next = i
			}
		case OpSchemaWrite:
			lp.write = i
			found = true
		}
	}
	if !found {
		return nil
	}
	return lp
}

// TestSchemaCatalogWriteEmitsARealScan verifies that schema writes emit a
// real scan of the catalog with proper cursor management and WHERE filtering.
func TestSchemaCatalogWriteEmitsARealScan(t *testing.T) {
	for i, tc := range schemaCatalogWriteShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		lp := findSchemaWriteLoop(prog)
		if lp == nil {
			t.Errorf("[%d] %q: no OpSchemaWrite at all", i, tc.stmt)
			continue
		}
		plan, ok := prog.Insns[lp.write].P4.(*schemaWritePlan)
		if !ok || plan == nil {
			t.Errorf("[%d] %q: OpSchemaWrite with no usable plan", i, tc.stmt)
			continue
		}
		if plan.ins != nil {
			// An INSERT reads no existing row, so it has no scan by design --
			// insert.c codes its tuples and stores, with no sqlite3WhereBegin
			// anywhere. Checked separately by
			// TestSchemaCatalogInsertFillsEveryTupleRegister.
			if lp.open >= 0 || len(lp.writeRow) != 0 {
				t.Errorf("[%d] %q: an INSERT emitted a catalog SCAN (open=%d, %d collects)",
					i, tc.stmt, lp.open, len(lp.writeRow))
			}
			continue
		}
		if lp.open < 0 {
			t.Errorf("[%d] %q: OpSchemaWrite with NO OpOpenDerived over a schemaWrite source.\n"+
				"The row loop is not a scan of the catalog, so the WHERE is coded against nothing --\n"+
				"and a one-row fixture cannot tell the difference.", i, tc.stmt)
			continue
		}
		if !(lp.pre >= 0 && lp.pre < lp.open) {
			t.Errorf("[%d] %q: OpSchemaWritePre at %d does not precede the OpOpenDerived at %d.\n"+
				"Opening the cursor runs wsCurrentCatalog (a whole buildImage) for a statement the\n"+
				"read-only refusal must reject first.", i, tc.stmt, lp.pre, lp.open)
		}
		if lp.rewind < lp.open {
			t.Errorf("[%d] %q: OpRewind at %d precedes the OpOpenDerived at %d", i, tc.stmt, lp.rewind, lp.open)
		}
		if lp.rowid < lp.rewind {
			t.Errorf("[%d] %q: no OpRowid inside the loop -- OpSchemaWriteRow would record rowid 0 for\n"+
				"every row, and every edit would land on the first catalog row", i, tc.stmt)
		}
		if len(lp.writeRow) != 1 {
			t.Errorf("[%d] %q: %d OpSchemaWriteRow instructions, want exactly 1", i, tc.stmt, len(lp.writeRow))
			continue
		}
		wr := lp.writeRow[0]
		if !(lp.rewind < wr && wr < lp.next && lp.next < lp.write) {
			t.Errorf("[%d] %q: loop is out of order -- rewind=%d writeRow=%d next=%d write=%d;\n"+
				"want rewind < writeRow < next < write (the collect INSIDE the scan, delete.c:582,\n"+
				"and the apply AFTER it)", i, tc.stmt, lp.rewind, wr, lp.next, lp.write)
			continue
		}
		if prog.Insns[lp.next].P2 > wr {
			t.Errorf("[%d] %q: OpNext jumps to %d, past the OpSchemaWriteRow at %d -- the loop body runs once",
				i, tc.stmt, prog.Insns[lp.next].P2, wr)
		}
		if prog.Insns[lp.rewind].P2 <= lp.next {
			t.Errorf("[%d] %q: OpRewind's empty-table exit is %d, inside the loop (OpNext is at %d)",
				i, tc.stmt, prog.Insns[lp.rewind].P2, lp.next)
		}
		// The WHERE, coded between the loop top and the collect. Whether there
		// IS one is read off the plan's own statement rather than out of the
		// SQL text, because an expression can emit an OpIfNot of its own --
		// "SET sql=CASE WHEN ... END" does.
		wantWhere := plan.del != nil && plan.del.where != nil
		if plan.upd != nil {
			wantWhere = plan.upd.where != nil
		}
		rowSkips := 0
		for _, a := range lp.rowIfNot {
			if a > lp.rewind && a < wr && prog.Insns[a].P2 > wr {
				rowSkips++
			}
		}
		if wantWhere != (rowSkips > 0) {
			t.Errorf("[%d] %q: %d conditional row skips inside the loop, want %v.\n"+
				"A WHERE that is never coded selects every row; a skip emitted for a statement with no\n"+
				"WHERE selects none.", i, tc.stmt, rowSkips, wantWhere)
		}
	}
}

// schemaWriteRegWritten reports which registers some instruction before addr
// names as a destination -- the same "written before it is read" test
// TestVtabUpdateFillsEverySetRegister uses, and for the same reason: a register
// nothing wrote reads as NULL (a fresh register's zero Value), so an emitter
// that silently skipped one value would store a NULL and be caught only by
// whichever behavioural case happened to look at that column.
func schemaWriteRegWritten(p *Program, addr int) map[int]bool {
	written := map[int]bool{}
	for k := 0; k < addr; k++ {
		switch p.Insns[k].Op {
		case OpSCopy, OpCopy, OpInteger, OpInt64, OpReal, OpString8, OpNull, OpBlob, OpVariable, OpRowid:
			written[p.Insns[k].P2] = true
		default:
			written[p.Insns[k].P3] = true
		}
	}
	return written
}

// TestSchemaCatalogUpdateFillsEverySetRegister pins the SET register block:
// OpSchemaWriteRow's P1..P1+P3-1.
func TestSchemaCatalogUpdateFillsEverySetRegister(t *testing.T) {
	for i, tc := range schemaCatalogWriteShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		for j := range prog.Insns {
			in := prog.Insns[j]
			if in.Op != OpSchemaWriteRow || in.P3 == 0 {
				continue
			}
			written := schemaWriteRegWritten(prog, j)
			for v := 0; v < in.P3; v++ {
				if !written[in.P1+v] {
					t.Errorf("[%d] %q: SET register %d (value %d of %d) is never written before the\n"+
						"OpSchemaWriteRow at %d. It would READ as NULL, because a fresh register's zero\n"+
						"Value is NULL -- which is exactly why no behavioural test catches this.",
						i, tc.stmt, in.P1+v, v, in.P3, j)
				}
			}
		}
	}
}

// TestSchemaCatalogInsertFillsEveryTupleRegister verifies that every VALUES
// tuple register is written before use.
func TestSchemaCatalogInsertFillsEveryTupleRegister(t *testing.T) {
	seen := 0
	for i, tc := range schemaCatalogWriteShapes {
		prog, err := compileShape(t, tc)
		if err != nil {
			t.Errorf("[%d] %q: compile: %v", i, tc.stmt, err)
			continue
		}
		for j := range prog.Insns {
			in := prog.Insns[j]
			if in.Op != OpSchemaWrite {
				continue
			}
			plan, ok := in.P4.(*schemaWritePlan)
			if !ok || plan == nil || plan.ins == nil {
				continue
			}
			seen++
			if in.P2 != len(plan.ins.rows[0]) || in.P3 != len(plan.ins.rows) {
				t.Errorf("[%d] %q: OpSchemaWrite says %d values x %d tuples, statement has %d x %d",
					i, tc.stmt, in.P2, in.P3, len(plan.ins.rows[0]), len(plan.ins.rows))
			}
			written := schemaWriteRegWritten(prog, j)
			for v := 0; v < in.P2*in.P3; v++ {
				if !written[in.P1+v] {
					t.Errorf("[%d] %q: VALUES register %d (slot %d of %d) is never written before the\n"+
						"OpSchemaWrite at %d -- it would store a NULL.", i, tc.stmt, in.P1+v, v, in.P2*in.P3, j)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no INSERT shape reached this assertion -- the fixture list stopped measuring anything")
	}
}

// schemaCatalogDeclinedShapes: shapes this compiler deliberately does NOT
// model. Each must decline CLEANLY -- half-serving one here would be a wrong
// answer rather than a gap. See compileSchemaCatalogUpdate / Delete / Insert.
var schemaCatalogDeclinedShapes = []struct {
	why string
	tc  conflictShapeCase
}{
	{"UPDATE ... RETURNING (execWritableSchemaUpdate declines it outright)",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`), `UPDATE sqlite_master SET sql='x' RETURNING name`}},
	{"UPDATE ... FROM (a different algorithm; the overlay declines it)",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`, `CREATE TABLE m(k,nv)`),
			`UPDATE sqlite_master SET sql=m.nv FROM m WHERE m.k=sqlite_master.name`}},
	{"an explicit OR clause on the UPDATE",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`), `UPDATE OR REPLACE sqlite_master SET sql='x'`}},
	{"DELETE ... RETURNING",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`), `DELETE FROM sqlite_master WHERE name='t' RETURNING name`}},
	{"INSERT ... SELECT (its row source is not a register block)",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`, `CREATE TABLE saved(a,b,c,d,e)`),
			`INSERT INTO sqlite_master SELECT * FROM saved`}},
	{"an INSERT upsert clause",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`),
			`INSERT INTO sqlite_master VALUES(1,2,3,4,5) ON CONFLICT DO NOTHING`}},
	{"an explicit OR clause on the INSERT",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`), `INSERT OR REPLACE INTO sqlite_master VALUES(1,2,3,4,5)`}},
	{"a leading WITH clause (its CTE scope is not pushed for this compile)",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`),
			`WITH c(z) AS (VALUES('t')) DELETE FROM sqlite_master WHERE name='t'`}},
	{"INDEXED BY on the target (this emitter has no way to honour the hint)",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`, `CREATE INDEX ix ON t(a)`),
			`DELETE FROM sqlite_master INDEXED BY ix WHERE name='t'`}},
	{"an AS alias on the target (this emitter's scope is named for the table)",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`), `DELETE FROM sqlite_master AS q WHERE q.name='t'`}},
	{"an AS alias on an UPDATE target",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`), `UPDATE sqlite_master AS q SET sql='x' WHERE q.name='t'`}},
	{"RAGGED VALUES tuples (no single register stride; an arity error either way)",
		conflictShapeCase{wsOn(`CREATE TABLE t(a)`), `INSERT INTO sqlite_master VALUES(1,2,3,4,5),(1,2)`}},
}

// TestSchemaCatalogWriteInATriggerBodyIsStillAHardError verifies that catalog
// writes in trigger bodies are still rejected.
func TestSchemaCatalogWriteInATriggerBodyIsStillAHardError(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	if err := wsExecAll(t, db, `PRAGMA writable_schema=ON`, `CREATE TABLE t(a)`,
		`CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE sqlite_master SET sql='x' WHERE name='t'; END`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, cerr := db.compileWrite(`INSERT INTO t VALUES(1)`)
	if cerr == nil || !strings.Contains(cerr.Error(), "no such table") {
		t.Errorf("compiling the firing statement gave %v, want a \"no such table\" error", cerr)
	}
}

// TestSchemaCatalogDeclinedShapesDeclineCleanly verifies that unimplemented
// schema write shapes decline cleanly with errVDBEUnsupported.
func TestSchemaCatalogDeclinedShapesDeclineCleanly(t *testing.T) {
	for _, d := range schemaCatalogDeclinedShapes {
		prog, err := compileShape(t, d.tc)
		if err != nil {
			if !errors.Is(err, errVDBEUnsupported) {
				t.Errorf("%q declined with %v, which is not errVDBEUnsupported.\n"+
					"A shape this emitter does not model must decline CLEANLY: %s.", d.tc.stmt, err, d.why)
			}
			continue
		}
		if prog != nil {
			t.Errorf("%q COMPILED, but this batch does not model it: %s.\n"+
				"Serving it here without the semantics that shape needs is a WRONG ANSWER,\n"+
				"not a promotion (AGENTS.md invariant 1).", d.tc.stmt, d.why)
		}
	}
}

// wsExecAll runs stmts on one fresh session and reports the first error, with
// the session left open for the caller to query.
func wsExecAll(t *testing.T, db *Session, stmts ...string) error {
	t.Helper()
	for _, s := range stmts {
		if err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// wsCatalogDump reads the catalog back through the ENGINE (not driver) as
// "type|name|tbl_name|sql" per row.
//
// rootpage is deliberately NOT read: schemaCatalogQueryGuard (query.go)
// declines a plain SELECT of that column once a catalog row has been removed,
// so a dump that included it could not be used for the DELETE cases at all --
// and none of these fixtures is about page numbering. The one case that IS
// about rootpage asks for it separately, while its own session still permits
// the read.
func wsCatalogDump(t *testing.T, db *Session) []string {
	t.Helper()
	rows := rvdQuery(t, db, `SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY name, type`)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, v := range r {
			cells[i] = valueText(v)
			if v.Typ == Null {
				cells[i] = "<NULL>"
			}
		}
		out = append(out, strings.Join(cells, "|"))
	}
	return out
}

// wsChanges reads changes() back through a snapshot of the session, the way
// every other engine-side codegen test does.
func wsChanges(t *testing.T, db *Session) int64 {
	t.Helper()
	rows := rvdQuery(t, db, `SELECT changes()`)
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("SELECT changes() returned %d rows", len(rows))
	}
	return valueInt(rows[0][0])
}

// TestSchemaCatalogWriteCompiledAnswers is the behavioural half: the compiled
// route must answer exactly what the 3.53.3 oracle does. Every expectation was
// measured against it when schema_write_direct.go was written -- see its
// package comment for the measurements.
//
// Every fixture holds THREE catalog rows and every WHERE selects the MIDDLE
// one, so a loop that ran once, or ignored the WHERE, or applied the SET to
// every row, fails here as well as structurally. Each case first asserts the
// statement really compiled, so a decline shows up as itself rather than as a
// confusing exec failure further down.
func TestSchemaCatalogWriteCompiledAnswers(t *testing.T) {
	three := []string{`CREATE TABLE a1(x)`, `CREATE TABLE b2(x)`, `CREATE TABLE c3(x)`, `PRAGMA writable_schema=ON`}
	cases := []struct {
		name    string
		setup   []string
		stmt    string
		changes int64
		want    []string
	}{
		{"UPDATE selects one of three", three,
			`UPDATE sqlite_master SET sql='EDITED' WHERE name='b2'`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|EDITED", "table|c3|c3|CREATE TABLE c3(x)"}},
		{"UPDATE with no WHERE touches all three", three,
			`UPDATE sqlite_master SET sql='ALL'`, 3,
			[]string{"table|a1|a1|ALL", "table|b2|b2|ALL", "table|c3|c3|ALL"}},
		// A WHERE-less UPDATE whose SET produces a DIFFERENT value per row.
		// ADDED BY MUTATION: with every fixture above, replacing
		// OpSchemaWriteRow's "copy(vals, m.regs[...])" with a direct SLICE of
		// the register block escaped every assertion in this file -- the
		// aliased rows all read the LAST iteration's values, which is
		// invisible when the three rows are assigned the same text.
		{"UPDATE with no WHERE computes a different value per row", three,
			`UPDATE sqlite_master SET sql=name||'-X'`, 3,
			[]string{"table|a1|a1|a1-X", "table|b2|b2|b2-X", "table|c3|c3|c3-X"}},
		{"UPDATE reads the OLD row in its own SET", three,
			`UPDATE sqlite_master SET sql=sql||'!' WHERE name='b2'`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|CREATE TABLE b2(x)!", "table|c3|c3|CREATE TABLE c3(x)"}},
		{"UPDATE assigns simultaneously, not in sequence", three,
			`UPDATE sqlite_master SET name=tbl_name||'N', tbl_name=name||'T' WHERE name='b2'`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2N|b2T|CREATE TABLE b2(x)", "table|c3|c3|CREATE TABLE c3(x)"}},
		{"UPDATE last assignment to a repeated target wins", three,
			`UPDATE sqlite_master SET sql='first', sql='second' WHERE name='b2'`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|second", "table|c3|c3|CREATE TABLE c3(x)"}},
		// TEXT affinity on sql (9 -> '9'), and INT affinity on rootpage
		// ('7' -> 7), which the separate assertion after this loop reads back
		// while the session still permits a rootpage SELECT.
		{"UPDATE applies the catalog's declared affinity", three,
			`UPDATE sqlite_master SET rootpage='7', sql=9 WHERE name='b2'`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|9", "table|c3|c3|CREATE TABLE c3(x)"}},
		{"UPDATE by rowid picks the second row", three,
			`UPDATE sqlite_master SET sql='ROWID2' WHERE rowid=2`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|ROWID2", "table|c3|c3|CREATE TABLE c3(x)"}},
		{"DELETE selects one of three", three,
			`DELETE FROM sqlite_master WHERE name='b2'`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|c3|c3|CREATE TABLE c3(x)"}},
		{"DELETE with no WHERE empties the catalog", three,
			`DELETE FROM sqlite_master`, 3, nil},
		{"DELETE matching nothing changes nothing", three,
			`DELETE FROM sqlite_master WHERE name='nosuch'`, 0,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|CREATE TABLE b2(x)", "table|c3|c3|CREATE TABLE c3(x)"}},
		{"INSERT appends one row with affinity applied", three,
			`INSERT INTO sqlite_master VALUES('table','z9','z9','7','CREATE TABLE z9(q)')`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|CREATE TABLE b2(x)",
				"table|c3|c3|CREATE TABLE c3(x)", "table|z9|z9|CREATE TABLE z9(q)"}},
		{"INSERT with a column list leaves the rest NULL", three,
			// The column list is deliberately NOT in catalog order (sql is
			// column 4, name is column 1, type is column 0), because a
			// positional list makes the whole IDLIST mapping a no-op: ADDED BY
			// MUTATION, after dropping wsInsertRowFromValues' "if cols != nil {
			// target = colIdx[i] }" escaped every assertion in this file while
			// the fixture read "(type,name)".
			`INSERT INTO sqlite_master(sql,name,type) VALUES('CREATE VIEW v0','v0','view')`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|CREATE TABLE b2(x)",
				"table|c3|c3|CREATE TABLE c3(x)", "view|v0|<NULL>|CREATE VIEW v0"}},
		{"INSERT of two tuples appends both", three,
			`INSERT INTO sqlite_master VALUES('table','y8','y8',7,'s1'),('table','z9','z9',8,'s2')`, 2,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|CREATE TABLE b2(x)",
				"table|c3|c3|CREATE TABLE c3(x)", "table|y8|y8|s1", "table|z9|z9|s2"}},
		{"a WHERE subquery selects the middle row", three,
			`UPDATE sqlite_master SET sql='SUB' WHERE name=(SELECT min(name) FROM sqlite_master WHERE name>'a1')`, 1,
			[]string{"table|a1|a1|CREATE TABLE a1(x)", "table|b2|b2|SUB", "table|c3|c3|CREATE TABLE c3(x)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer db.Discard()
			if err := wsExecAll(t, db, tc.setup...); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if _, cerr := db.compileWrite(tc.stmt); cerr != nil {
				t.Fatalf("%q: compile: %v", tc.stmt, cerr)
			}
			if err := db.Exec(tc.stmt); err != nil {
				t.Fatalf("%q: %v", tc.stmt, err)
			}
			if got := wsChanges(t, db); got != tc.changes {
				t.Errorf("%q: changes() = %d, want %d", tc.stmt, got, tc.changes)
			}
			got := wsCatalogDump(t, db)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("%q: catalog is\n  %s\nwant\n  %s", tc.stmt,
					strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}
}

// TestSchemaCatalogUpdateAppliesRootpageAffinity is the INT half of the
// affinity case above, split out because it is the one assertion that has to
// SELECT rootpage -- which schemaCatalogQueryGuard (query.go) permits only
// while no catalog row has ever been removed, as it is here.
//
// Assigning to rootpage is served unconditionally (see
// schema_write_direct.go's package comment: the write is a no-op within the
// connection on both engines), so what this pins is only that the assigned
// value took the column's declared INT affinity -- '7' stored as the integer
// 7, not the text '7'.
func TestSchemaCatalogUpdateAppliesRootpageAffinity(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	if err := wsExecAll(t, db, `CREATE TABLE a1(x)`, `PRAGMA writable_schema=ON`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	stmt := `UPDATE sqlite_master SET rootpage='7' WHERE name='a1'`
	if _, cerr := db.compileWrite(stmt); cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	if err := db.Exec(stmt); err != nil {
		t.Fatalf("%q: %v", stmt, err)
	}
	rows := rvdQuery(t, db, `SELECT typeof(rootpage), rootpage FROM sqlite_master WHERE name='a1'`)
	if len(rows) != 1 || valueText(rows[0][0]) != "integer" || valueInt(rows[0][1]) != 7 {
		t.Errorf("rootpage reads back as %v, want the integer 7 (INT affinity applied to '7')", rows)
	}
}

// TestSchemaCatalogWritePreflightIsAskedFirst pins the ORDER of the
// prepare-time checks, which is the C's: sqlite3IsReadOnly (insert.c:1009,
// update.c:411, delete.c:388) before the SET/IDLIST name resolution
// (update.c:500) and before the VALUES arity check (insert.c:1249-1252). Each
// case below would report a DIFFERENT error if the emitter asked them in a
// different order, so the wording IS the assertion.
func TestSchemaCatalogWritePreflightIsAskedFirst(t *testing.T) {
	cases := []struct {
		setup []string
		stmt  string
		want  string
	}{
		// Flag OFF: C SQLite's own refusal wins over everything else.
		{[]string{`CREATE TABLE t(a)`}, `UPDATE sqlite_master SET sql='x'`, "may not be modified"},
		{[]string{`CREATE TABLE t(a)`}, `UPDATE sqlite_master SET nosuchcol='x'`, "may not be modified"},
		{[]string{`CREATE TABLE t(a)`}, `DELETE FROM sqlite_master`, "may not be modified"},
		{[]string{`CREATE TABLE t(a)`}, `INSERT INTO sqlite_master VALUES(1,2,3,4)`, "may not be modified"},
		{[]string{`CREATE TABLE t(a)`}, `INSERT INTO sqlite_master(nosuchcol) VALUES(1)`, "may not be modified"},
		// Flag ON: the name resolution, then the arity.
		{wsOn(`CREATE TABLE t(a)`), `UPDATE sqlite_master SET nosuchcol='x'`, "has no column named nosuchcol"},
		{wsOn(`CREATE TABLE t(a)`), `INSERT INTO sqlite_master(nosuchcol) VALUES(1)`, "has no column named nosuchcol"},
		{wsOn(`CREATE TABLE t(a)`), `INSERT INTO sqlite_master VALUES(1,2,3,4)`, "has 5 columns but 4 values were supplied"},
		{wsOn(`CREATE TABLE t(a)`), `INSERT INTO sqlite_master(type,name) VALUES(1)`, "has 5 columns but 1 values were supplied"},
	}
	for _, tc := range cases {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := wsExecAll(t, db, tc.setup...); err != nil {
			t.Fatalf("setup: %v", err)
		}
		err = db.Exec(tc.stmt)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error %v, want one containing %q", tc.stmt, err, tc.want)
		}
		db.Discard()
	}
}

// TestSchemaCatalogWriteReadOnlyRefusalLeavesChangesAlone is the connection-
// state half of the refusal above, and it is invisible to the harness
// (differ() records {"kind":"error"} with no message and never reads
// changes()).
//
// C SQLite raises "table sqlite_master may not be modified" from
// sqlite3IsReadOnly while GENERATING code (delete.c:121) -- before the VM
// exists -- so the statement never reaches RUN state, sqlite3VdbeHalt's
// changeCntOn block never runs (vdbeaux.c, under the VDBE_RUN_STATE assert),
// and changes() still reads whatever the last DML left. Verified against
// mattn/go-sqlite3 3.53.3 in
// compat-harness/writable_schema_readonly_changes_test.go.
//
// OpSchemaWritePre reproduces that with wc.prepareFailed (vdbe_write.go): the
// refusal is raised before any row is touched, and the change counter is left
// exactly as the previous DML left it.
func TestSchemaCatalogWriteReadOnlyRefusalLeavesChangesAlone(t *testing.T) {
	for _, stmt := range []string{
		`UPDATE sqlite_master SET sql='x'`,
		`DELETE FROM sqlite_master`,
		`INSERT INTO sqlite_master VALUES('table','z','z',7,'CREATE TABLE z(a)')`,
	} {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := wsExecAll(t, db, `CREATE TABLE t(a)`, `INSERT INTO t VALUES(1),(2),(3)`); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if got := wsChanges(t, db); got != 3 {
			t.Fatalf("setup left changes() = %d, want 3", got)
		}
		if err := db.Exec(stmt); err == nil {
			t.Errorf("%q succeeded with writable_schema OFF", stmt)
		}
		if got := wsChanges(t, db); got != 3 {
			t.Errorf("%q: changes() = %d after the read-only refusal, want 3 left untouched", stmt, got)
		}
		db.Discard()
	}
}

// TestSchemaCatalogWritePreflightIsNotCACHED is the probe rule 4 of this
// project's own lessons exists for: the write-program cache
// (cachedWriteProgram, vdbe_write.go) keys on the SQL TEXT and is invalidated
// only by db.schemaGen and db.txGen -- and NEITHER "PRAGMA writable_schema"
// nor a direct catalog DELETE moves either one (grep: only bumpSchema touches
// schemaGen, and no catalog-write path calls it). So every answer
// OpSchemaWritePre gives depends on connection state no cache key covers,
// which is why it is an OPCODE rather than a compile-time check.
//
// This is INVISIBLE through driver, which opens a fresh *DB per statement
// and so never hits the cache at all. Both probes run ENGINE-DIRECT, twice on
// ONE connection, which is what makes the second execution a cache hit.
//
// MUTATION-MEASURED: moving either check to compile time (into
// compileSchemaCatalog*) fails this test and nothing else in the tree.
func TestSchemaCatalogWritePreflightIsNotCACHED(t *testing.T) {
	t.Run("the writable_schema flag can turn off between executions", func(t *testing.T) {
		db, err := Create(t.TempDir() + "/x.musq")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		defer db.Discard()
		if err := wsExecAll(t, db, `CREATE TABLE t(a)`, `PRAGMA writable_schema=ON`,
			`UPDATE sqlite_master SET sql='one' WHERE name='t'`, `PRAGMA writable_schema=OFF`); err != nil {
			t.Fatalf("setup: %v", err)
		}
		err = db.Exec(`UPDATE sqlite_master SET sql='one' WHERE name='t'`)
		if err == nil || !strings.Contains(err.Error(), "may not be modified") {
			t.Errorf("the SECOND execution of the identical statement, with the flag now OFF, gave %v;\n"+
				"want \"table sqlite_master may not be modified\". A compile-time flag check would be\n"+
				"cached from the first execution and this write would silently land.", err)
		}
	})
	// A third subtest stood here. Its SUBJECT was the same one the two above
	// cover -- the preflight is re-run, not cached -- but its VEHICLE was the
	// private-store decline an rtree used to trigger: "an rtree vtab created
	// between executions turns a served write into a decline".
	//
	// That vehicle is gone twice over now that rtree writes real shadow tables
	// (rtree_shadow.go). There is no decline to observe, and CREATE VIRTUAL
	// TABLE now creates three ordinary tables, which bumps schemaGen and
	// clears the write-program cache outright -- so the cached program the
	// subtest needed to still be live cannot be. Removed rather than rewritten
	// around a vehicle that no longer exists; the caching property itself is
	// pinned above and by TestSchemaCatalogWriteScanSeesTheCurrentCatalog.
}

// TestSchemaCatalogWriteScanSeesTheCurrentCatalog is the second cache-shaped
// probe, over the SCAN rather than the preflight. The catalog row set comes
// from db.wsCurrentCatalog at RUN time (OpOpenDerived's schemaWrite branch);
// materializing it at COMPILE time and stashing it on the derivedSource -- the
// obvious shortcut, and what the READ path's catalogScope does -- would freeze
// the first execution's rows onto every later one.
//
// Engine-direct and twice on one connection, for the reason above: driver
// re-prepares and would never hit the cached program.
//
// The row the catalog grows between the two executions is added by a catalog
// INSERT, and that choice is MEASURED, not incidental: growing it with a
// "CREATE TABLE b2(x)" instead lets the mutation ESCAPE, because a CREATE
// bumps db.schemaGen and clears the whole write-program cache, so the second
// UPDATE is recompiled and gets a fresh frozen image anyway. A catalog INSERT
// moves neither schemaGen nor txGen -- it only grows db.wsInserted -- so the
// second UPDATE really is the same cached *Program.
func TestSchemaCatalogWriteScanSeesTheCurrentCatalog(t *testing.T) {
	db, err := Create(t.TempDir() + "/x.musq")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Discard()
	if err := wsExecAll(t, db, `CREATE TABLE a1(x)`, `PRAGMA writable_schema=ON`,
		`UPDATE sqlite_master SET sql='EDITED'`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := wsChanges(t, db); got != 1 {
		t.Fatalf("the first WHERE-less UPDATE changed %d rows, want 1", got)
	}
	if err := db.Exec(`INSERT INTO sqlite_master VALUES('table','z9','z9',7,'s')`); err != nil {
		t.Fatalf("catalog insert: %v", err)
	}
	if err := db.Exec(`UPDATE sqlite_master SET sql='EDITED'`); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if got := wsChanges(t, db); got != 2 {
		t.Errorf("the second WHERE-less UPDATE changed %d rows, want 2 -- the scan is reading a stale\n"+
			"catalog image rather than the one wsCurrentCatalog builds per execution", got)
	}
	want := []string{"table|a1|a1|EDITED", "table|z9|z9|EDITED"}
	if got := wsCatalogDump(t, db); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("catalog is\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
