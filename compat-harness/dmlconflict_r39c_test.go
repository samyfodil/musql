package compat

// This file tests conflict clauses on view DML, UPDATE ... FROM, and
// UPDATE ... RETURNING, comparing rows and connection state against C SQLite.

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// r39cNorm renders one engine.Value the way worker/main.go's normalize renders
// the same cell scanned out of database/sql, so the two sides are comparable.
func r39cNorm(v engine.Value) string {
	switch v.Typ {
	case engine.Null:
		return "N"
	case engine.Int:
		return "I:" + strconv.FormatInt(v.I, 10)
	case engine.Float:
		return "F:" + strconv.FormatFloat(v.F, 'g', -1, 64)
	case engine.Text:
		return "T:" + string(v.S)
	default:
		return "X:" + fmt.Sprintf("%x", v.S)
	}
}

func r39cNormAny(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case int64:
		return "I:" + strconv.FormatInt(x, 10)
	case float64:
		return "F:" + strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "T:" + x
	case []byte:
		return "T:" + string(x)
	case bool:
		if x {
			return "I:1"
		}
		return "I:0"
	default:
		return fmt.Sprintf("?%v", v)
	}
}

// r39cCase is one comparison: a setup, the statement under test, and the
// readbacks whose answers must match.
type r39cCase struct {
	name      string
	setup     []string
	dml       string
	readbacks []string
}

// r39cRunMusql drives the case through engine.DB directly. It reports whether
// the DML errored and the readbacks' rows.
func r39cRunMusql(t *testing.T, c r39cCase) (bool, []string) {
	t.Helper()
	db, err := engine.Create(t.TempDir()+"/r39c.sqlite")
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()
	for _, s := range c.setup {
		if _, _, serr := db.ExecArgs(s, nil); serr != nil {
			t.Fatalf("%s: setup %q: %v", c.name, s, serr)
		}
	}
	var dmlErr error
	if engine.StatementHasReturning(c.dml) {
		_, _, dmlErr = db.ExecReturningArgs(c.dml, nil)
	} else {
		_, _, dmlErr = db.ExecArgs(c.dml, nil)
	}
	out := make([]string, 0, len(c.readbacks))
	pager, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatalf("%s: snapshot: %v", c.name, perr)
	}
	for _, q := range c.readbacks {
		_, rows, qerr := pager.QueryArgs(q, nil)
		if qerr != nil {
			out = append(out, "ERR")
			continue
		}
		var b strings.Builder
		for _, r := range rows {
			b.WriteString("(")
			for i, v := range r {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(r39cNorm(v))
			}
			b.WriteString(")")
		}
		out = append(out, b.String())
	}
	return dmlErr != nil, out
}

// r39cRunOracle drives the same case through mattn/go-sqlite3 on one
// connection, in this process -- one execution of the DML, no worker.
func r39cRunOracle(t *testing.T, c r39cCase) (bool, []string) {
	t.Helper()
	sdb, err := sql.Open("sqlite3", t.TempDir()+"/oracle.sqlite")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)
	for _, s := range c.setup {
		if _, serr := sdb.Exec(s); serr != nil {
			t.Fatalf("%s: oracle setup %q: %v", c.name, s, serr)
		}
	}
	// Exec, not Query: go-sqlite3's Exec steps the statement to completion and
	// discards any RETURNING rows, which is what makes this ONE execution.
	_, dmlErr := sdb.Exec(c.dml)
	out := make([]string, 0, len(c.readbacks))
	for _, q := range c.readbacks {
		rows, qerr := sdb.Query(q)
		if qerr != nil {
			out = append(out, "ERR")
			continue
		}
		cols, _ := rows.Columns()
		var b strings.Builder
		for rows.Next() {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if serr := rows.Scan(ptrs...); serr != nil {
				t.Fatalf("%s: oracle scan: %v", c.name, serr)
			}
			b.WriteString("(")
			for i, v := range cells {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(r39cNormAny(v))
			}
			b.WriteString(")")
		}
		rows.Close()
		out = append(out, b.String())
	}
	return dmlErr != nil, out
}

func r39cCompare(t *testing.T, cases []r39cCase) {
	t.Helper()
	for _, c := range cases {
		mErr, mOut := r39cRunMusql(t, c)
		cErr, cOut := r39cRunOracle(t, c)
		if mErr != cErr {
			t.Errorf("%s: %s\n  oracle errored=%v, this engine errored=%v -- the two must agree on whether the statement is accepted",
				c.name, c.dml, cErr, mErr)
			continue
		}
		for i := range c.readbacks {
			if mOut[i] != cOut[i] {
				t.Errorf("%s: %s\n  readback %q\n    oracle: %s\n    engine: %s",
					c.name, c.dml, c.readbacks[i], cOut[i], mOut[i])
			}
		}
	}
}

// ---------------------------------------------------------------------------

// r39cViewSetup is a view over t with the three INSTEAD OF triggers, each
// logging its own fire and then doing the write itself. The log is what makes
// "which fires ran, and did their writes survive" observable.
func r39cViewSetup() []string {
	return []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c)`,
		`CREATE TABLE log(e TEXT, x, y)`,
		`CREATE VIEW vt AS SELECT a,b,c FROM t`,
		`CREATE TRIGGER gvi INSTEAD OF INSERT ON vt BEGIN INSERT INTO log(e,x,y) VALUES('vi',new.a,new.b); INSERT INTO t(a,b,c) VALUES(new.a,new.b,new.c); END`,
		`CREATE TRIGGER gvu INSTEAD OF UPDATE ON vt BEGIN INSERT INTO log(e,x,y) VALUES('vu',old.a,new.b); UPDATE t SET b=new.b, c=new.c WHERE a=old.a; END`,
		`CREATE TRIGGER gvd INSTEAD OF DELETE ON vt BEGIN INSERT INTO log(e,x,y) VALUES('vd',old.a,old.b); DELETE FROM t WHERE a=old.a; END`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`,
	}
}

var r39cViewReadbacks = []string{
	`SELECT a,b,c FROM t ORDER BY a`,
	`SELECT e,x,y FROM log ORDER BY rowid`,
	`SELECT changes(), total_changes()`,
}

// TestR39CViewConflictClause pins that a view DML's OR clause governs its
// INSTEAD OF trigger body -- insert.c:1495 and update.c:984 both hand the
// statement's onError to sqlite3CodeRowTrigger, and codeTriggerProgram
// (trigger.c:1137) imposes it on every body step. The `if( !isView )` guard at
// insert.c:1501 skips the row operation only.
//
// The FAIL case is the one that was DATA LOSS: vdbe.c:1268 says outright "Do
// not rollback if P2==OE_Fail", so the fires before the failing one keep what
// they wrote, and this engine's per-statement snapshot used to unwind them all.
func TestR39CViewConflictClause(t *testing.T) {
	multi := `INTO vt(a,b,c) VALUES(4,'q',40),(2,'y',50),(6,'r',60)`
	cases := []r39cCase{
		{"view-ins-fail", r39cViewSetup(), `INSERT OR FAIL ` + multi, r39cViewReadbacks},
		{"view-ins-ignore", r39cViewSetup(), `INSERT OR IGNORE ` + multi, r39cViewReadbacks},
		{"view-ins-replace", r39cViewSetup(), `INSERT OR REPLACE ` + multi, r39cViewReadbacks},
		{"view-ins-abort", r39cViewSetup(), `INSERT OR ABORT ` + multi, r39cViewReadbacks},
		{"view-ins-rollback", r39cViewSetup(), `INSERT OR ROLLBACK ` + multi, r39cViewReadbacks},
		{"view-ins-plain", r39cViewSetup(), `INSERT ` + multi, r39cViewReadbacks},
		{"view-replace-into", r39cViewSetup(), `REPLACE INTO vt(a,b,c) VALUES(2,'q',99)`, r39cViewReadbacks},
		{"view-ins-default-values", r39cViewSetup(), `INSERT OR REPLACE INTO vt DEFAULT VALUES`, r39cViewReadbacks},
		{"view-ins-nocols", r39cViewSetup(), `INSERT OR ROLLBACK INTO vt VALUES(4,'q',40)`, r39cViewReadbacks},
		{"view-ins-select", r39cViewSetup(), `INSERT OR IGNORE INTO vt(a,b,c) SELECT a,b,c FROM (SELECT 4 a,'q' b,40 c UNION ALL SELECT 2,'y',50)`, r39cViewReadbacks},
		{"view-upd-plain", r39cViewSetup(), `UPDATE vt SET c = c + 1 WHERE a >= 2`, r39cViewReadbacks},
		{"view-upd-ignore", r39cViewSetup(), `UPDATE OR IGNORE vt SET c = c + 1 WHERE a >= 2`, r39cViewReadbacks},
		{"view-upd-replace", r39cViewSetup(), `UPDATE OR REPLACE vt SET c = c + 1 WHERE a >= 2`, r39cViewReadbacks},
		{"view-del", r39cViewSetup(), `DELETE FROM vt WHERE a >= 2`, r39cViewReadbacks},
	}
	r39cCompare(t, cases)
}

// TestR39CUpsertOnViewIsPrepareError pins insert.c:1296 -- "cannot UPSERT a
// view" -- AND the fact that it is raised during code generation, so the
// connection's counters keep whatever the previous statement left them at
// rather than being republished as 0. That second half is the whole finding:
// both engines reject the statement, and only a counter readback tells them
// apart.
func TestR39CUpsertOnViewIsPrepareError(t *testing.T) {
	r39cCompare(t, []r39cCase{
		{"upsert-view-nothing", r39cViewSetup(),
			`INSERT INTO vt(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO NOTHING`, r39cViewReadbacks},
		{"upsert-view-doupdate", r39cViewSetup(),
			`INSERT OR IGNORE INTO vt(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO UPDATE SET c = excluded.c + 1`, r39cViewReadbacks},
		{"upsert-view-where", r39cViewSetup(),
			`INSERT INTO vt(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO UPDATE SET c = excluded.c WHERE vt.c < 25`, r39cViewReadbacks},
	})
}

// ---------------------------------------------------------------------------

// TestR39CUpdateFromConflict pins that "UPDATE ... FROM" resolves a conflict
// exactly as the single-table form does. update.c:1031's
// sqlite3GenerateConstraintChecks, which carries the statement's onError, sits
// outside every nChangeFrom test; nChangeFrom decides only how the candidate
// rows reach the second pass (update.c:694's updateFromSelect fills an
// ephemeral table instead of update.c:742's WHERE scan).
func TestR39CUpdateFromConflict(t *testing.T) {
	setup := func(ddl ...string) []string {
		out := append([]string{}, ddl...)
		return append(out,
			`CREATE TABLE log(e TEXT, x, y)`,
			`CREATE TABLE src(a INTEGER, b TEXT, c)`,
			`INSERT INTO src(a,b,c) VALUES(4,'q',40),(2,'x',50),(5,'r',60)`,
			`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`,
		)
	}
	readbacks := []string{
		`SELECT a,b,c FROM t ORDER BY a`,
		`SELECT e,x,y FROM log ORDER BY rowid`,
		`SELECT changes(), total_changes()`,
		`PRAGMA integrity_check`,
	}
	rowid := `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c)`
	// b UNIQUE is what makes the conflict clause matter at all: src's matching
	// row sets t's a=2 to b='x', which a=1 already holds.
	uniq := `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE, c)`
	wor := `CREATE TABLE t(a INTEGER, b TEXT UNIQUE, c, PRIMARY KEY(a)) WITHOUT ROWID`
	trig := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE, c)`,
		`CREATE TRIGGER g1 BEFORE UPDATE ON t BEGIN INSERT INTO log(e,x,y) VALUES('bu',old.a,new.b); END`,
		`CREATE TRIGGER g2 AFTER UPDATE ON t BEGIN INSERT INTO log(e,x,y) VALUES('au',old.a,new.b); END`,
	}
	dml := func(or string) string {
		return `UPDATE ` + or + ` t SET b = src.b, c = src.c FROM src WHERE src.a = t.a`
	}
	var cases []r39cCase
	for _, s := range []struct {
		name string
		ddl  []string
	}{
		{"rowid", []string{rowid}}, {"uniq", []string{uniq}},
		{"without-rowid", []string{wor}}, {"trig", trig},
	} {
		for _, or := range []string{``, `OR ABORT`, `OR IGNORE`, `OR REPLACE`, `OR FAIL`, `OR ROLLBACK`} {
			name := "upfrom/" + s.name + "/" + strings.ToLower(strings.TrimPrefix(or+"none", ""))
			cases = append(cases, r39cCase{name, setup(s.ddl...), dml(or), readbacks})
		}
	}
	// A declared column default rather than a statement clause: the third term
	// of the decline this replaced (tableHasDeclaredConflict) covered exactly
	// this, and it must still resolve per-constraint.
	cases = append(cases, r39cCase{"upfrom/declared-ignore",
		setup(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE ON CONFLICT IGNORE, c)`),
		dml(``), readbacks})
	cases = append(cases, r39cCase{"upfrom/declared-replace",
		setup(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE ON CONFLICT REPLACE, c)`),
		dml(``), readbacks})
	// A NOT NULL violation reaches the SET-phase resolution rather than the
	// apply-phase one -- the other half of buildPendingUpdateConflict.
	cases = append(cases, r39cCase{"upfrom/notnull-ignore",
		setup(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT NOT NULL, c)`),
		`UPDATE OR IGNORE t SET b = NULL FROM src WHERE src.a = t.a`, readbacks})
	cases = append(cases, r39cCase{"upfrom/notnull-fail",
		setup(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT NOT NULL, c)`),
		`UPDATE OR FAIL t SET b = NULL FROM src WHERE src.a = t.a`, readbacks})
	r39cCompare(t, cases)
}

// ---------------------------------------------------------------------------

// TestR39CUpdateReturningConflict pins the ROWS and the TABLE an
// "UPDATE OR ... RETURNING" leaves behind. The RETURNING rows themselves are
// checked separately (r39cReturningRows) because r39cCompare compares state,
// not result sets, and the oracle side runs the statement through Exec.
func TestR39CUpdateReturningConflict(t *testing.T) {
	setup := func(ddl string) []string {
		return []string{ddl, `CREATE TABLE log(e TEXT, x, y)`,
			`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`}
	}
	readbacks := []string{
		`SELECT a,b,c FROM t ORDER BY a`,
		`PRAGMA integrity_check`,
	}
	uniq := `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE, c)`
	chk := `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c, CHECK(c < 100))`
	nn := `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT NOT NULL, c)`
	var cases []r39cCase
	for _, or := range []string{``, `OR ABORT`, `OR IGNORE`, `OR REPLACE`, `OR FAIL`, `OR ROLLBACK`} {
		cases = append(cases,
			r39cCase{"updret/uniq" + or, setup(uniq),
				`UPDATE ` + or + ` t SET b = 'x' WHERE a >= 2 RETURNING a, b, c`, readbacks},
			r39cCase{"updret/check" + or, setup(chk),
				`UPDATE ` + or + ` t SET c = 200 WHERE a >= 2 RETURNING *`, readbacks},
			r39cCase{"updret/notnull" + or, setup(nn),
				`UPDATE ` + or + ` t SET b = NULL WHERE a >= 2 RETURNING a, b`, readbacks},
		)
	}
	r39cCompare(t, cases)
}

// r39cReturningRows is the RETURNING RESULT SET half: which rows an
// OR-clause UPDATE reports. The oracle's answer is read through Query here
// (one execution either way, since these cases all SUCCEED).
func TestR39CUpdateReturningRows(t *testing.T) {
	type rc struct {
		name  string
		setup []string
		dml   string
	}
	uniq := []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE, c)`,
		`INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`,
	}
	for _, c := range []rc{
		// IGNORE: a=2's new b collides with a=1's, so that row is skipped and
		// reports NO RETURNING row -- it jumps past update.c:1118's AFTER-list
		// call, which is where the TRIGGER_AFTER RETURNING trigger lives.
		{"ignore-skips-its-row", uniq, `UPDATE OR IGNORE t SET b = 'x' WHERE a >= 2 RETURNING a, b`},
		// REPLACE: the row IS stored (the OTHER one is deleted), so it reports.
		{"replace-reports-its-row", uniq, `UPDATE OR REPLACE t SET b = 'x' WHERE a = 2 RETURNING a, b`},
		{"plain-reports-every-row", uniq, `UPDATE t SET c = c + 1 WHERE a >= 2 RETURNING a, c`},
		{"ignore-reports-every-row", uniq, `UPDATE OR IGNORE t SET c = c + 1 WHERE a >= 2 RETURNING a, c`},
		{"replace-rowid-order", uniq, `UPDATE OR REPLACE t SET c = c + 1 WHERE a >= 2 RETURNING a, c`},
	} {
		mrows := r39cMusqlReturning(t, c.setup, c.dml)
		orows := r39cOracleReturning(t, c.setup, c.dml)
		if mrows != orows {
			t.Errorf("%s: %s\n  oracle: %s\n  engine: %s", c.name, c.dml, orows, mrows)
		}
	}
}

func r39cMusqlReturning(t *testing.T, setup []string, dml string) string {
	t.Helper()
	db, err := engine.Create(t.TempDir()+"/r39c.sqlite")
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()
	for _, s := range setup {
		if _, _, serr := db.ExecArgs(s, nil); serr != nil {
			t.Fatalf("setup %q: %v", s, serr)
		}
	}
	cols, rows, rerr := db.ExecReturningArgs(dml, nil)
	if rerr != nil {
		return "ERR: " + rerr.Error()
	}
	return r39cRender(cols, rows)
}

func r39cRender(cols []string, rows [][]engine.Value) string {
	var b strings.Builder
	b.WriteString(strings.Join(cols, "|"))
	// RETURNING's row order is documented as unspecified, so the comparison
	// sorts: what is pinned here is WHICH rows appear, not in what order.
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, v := range r {
			cells[i] = r39cNorm(v)
		}
		lines = append(lines, "("+strings.Join(cells, ",")+")")
	}
	sortStrings(lines)
	b.WriteString(" " + strings.Join(lines, ""))
	return b.String()
}

func r39cOracleReturning(t *testing.T, setup []string, dml string) string {
	t.Helper()
	sdb, err := sql.Open("sqlite3", t.TempDir()+"/oracle.sqlite")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)
	for _, s := range setup {
		if _, serr := sdb.Exec(s); serr != nil {
			t.Fatalf("oracle setup %q: %v", s, serr)
		}
	}
	rows, qerr := sdb.Query(dml)
	if qerr != nil {
		return "ERR: " + qerr.Error()
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	lines := []string{}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			t.Fatalf("oracle scan: %v", serr)
		}
		out := make([]string, len(cols))
		for i, v := range cells {
			out[i] = r39cNormAny(v)
		}
		lines = append(lines, "("+strings.Join(out, ",")+")")
	}
	if rerr := rows.Err(); rerr != nil {
		return "ERR: " + rerr.Error()
	}
	sortStrings(lines)
	return strings.Join(cols, "|") + " " + strings.Join(lines, "")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
