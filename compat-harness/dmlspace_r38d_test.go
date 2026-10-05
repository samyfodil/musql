package compat

// Tests write path (INSERT, UPDATE, DELETE, upsert, RETURNING, triggers, conflict clauses)
// via pairwise enumeration. Compares every cell against C SQLite with ground-truth readback
// and handles the execution-count difference between engines correctly.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r38dCell is one cell of one axis. Its sql means whatever its axis says it
// means; %TBL% is substituted with the DML's target (see the trigger axis).
type r38dCell struct{ name, sql string }

// Fixture: every cell declares t(a,b,c) with a as PRIMARY KEY. Conflict at a=2 (key collision)
// and optionally at b='y' (UNIQUE column in some cells).

const r38dParent = `INSERT INTO p(k,v) VALUES(10,'p10'),(20,'p20'),(30,'p30'),(40,'p40'),(50,'p50'),(60,'p60'),(99,'p99')`

// r38dSeed populates t. It runs BEFORE the trigger axis creates anything, so a
// trigger never fires on the seed and the log holds only what the statement
// under test caused.
const r38dSeed = `INSERT INTO t(a,b,c) VALUES(1,'x',10),(2,'y',20),(3,'z',30)`

// r38dSrc's middle row is the conflicting one: a=2 collides on the primary key,
// and b='x' collides with t's a=1 under the UNIQUE cells — two different
// constraints from one row, so OR REPLACE has a choice to get wrong.
const r38dSrc = `INSERT INTO src(a,b,c) VALUES(4,'q',40),(2,'x',50),(5,'r',60)`

// ---------------------------------------------------------------------------
// AXIS 1 — the STATEMENT.
// ---------------------------------------------------------------------------

var r38dStmtAxis = []r38dCell{
	{"ins-values", `INSERT %OR% INTO %TBL%(a,b,c) VALUES(4,'q',40)`},
	// Multi-row is the shape that separates the conflict clauses from each
	// other: row 2 conflicts, so ABORT keeps nothing, FAIL keeps row 1 only,
	// IGNORE keeps rows 1 and 3, REPLACE keeps all three.
	{"ins-multi", `INSERT %OR% INTO %TBL%(a,b,c) VALUES(4,'q',40),(2,'y',50),(6,'r',60)`},
	// No column list: the implicit list is where a generated column decides
	// whether it counts toward the arity.
	{"ins-nocols", `INSERT %OR% INTO %TBL% VALUES(4,'q',40)`},
	{"ins-default", `INSERT %OR% INTO %TBL% DEFAULT VALUES`},
	{"ins-select", `INSERT %OR% INTO %TBL%(a,b,c) SELECT a,b,c FROM src`},
	// Same rows, reversed arrival order: under OR REPLACE/OR IGNORE the row
	// that survives depends on which one arrived first.
	{"ins-select-desc", `INSERT %OR% INTO %TBL%(a,b,c) SELECT a,b,c FROM src ORDER BY a DESC`},
	{"ins-with", `WITH s(a,b,c) AS (VALUES(4,'q',40),(2,'y',50)) INSERT %OR% INTO %TBL%(a,b,c) SELECT a,b,c FROM s`},
	{"replace", `REPLACE INTO %TBL%(a,b,c) VALUES(2,'q',99)`},
	{"update", `UPDATE %OR% %TBL% SET c = c*10, b = CASE a WHEN 3 THEN 'x' ELSE b END WHERE a >= 2`},
	// Moving the primary key onto an occupied slot: a=2 becomes 3, which the
	// existing row 3 already holds. Conflicts under every schema cell.
	{"update-pk", `UPDATE %OR% %TBL% SET a = a + 1 WHERE a >= 2`},
	{"update-from", `UPDATE %OR% %TBL% SET b = src.b, c = src.c FROM src WHERE src.a = %TBL%.a`},
	// Zero matching rows: changes() must report 0 and the trigger must not fire.
	{"update-nomatch", `UPDATE %OR% %TBL% SET c = 1 WHERE a = 999`},
	{"delete", `DELETE FROM %TBL% WHERE a >= 2`},
	// Unqualified DELETE takes SQLite's truncate path, which is a different
	// opcode sequence and skips the per-row cursor entirely.
	{"delete-all", `DELETE FROM %TBL%`},
	// Deleting the PARENT is the only way to reach ON DELETE CASCADE/RESTRICT.
	{"delete-parent", `DELETE FROM p WHERE k = 10`},
	{"upsert-nothing", `INSERT %OR% INTO %TBL%(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO NOTHING`},
	{"upsert-update", `INSERT %OR% INTO %TBL%(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO UPDATE SET c = excluded.c + 1`},
	{"upsert-where", `INSERT %OR% INTO %TBL%(a,b,c) VALUES(2,'w',99) ON CONFLICT(a) DO UPDATE SET c = excluded.c WHERE %TBL%.c < 25`},
	{"ins-returning", `INSERT %OR% INTO %TBL%(a,b,c) VALUES(4,'q',40),(2,'y',50) RETURNING *`},
	{"update-returning", `UPDATE %OR% %TBL% SET c = c + 1 WHERE a >= 2 RETURNING *`},
	{"delete-returning", `DELETE FROM %TBL% WHERE a >= 2 RETURNING *`},
}

// Conflict clause axis. Inert on DELETE and REPLACE INTO.

var r38dConflictAxis = []r38dCell{
	{"none", ``},
	{"ignore", `OR IGNORE`},
	{"replace", `OR REPLACE`},
	{"abort", `OR ABORT`},
	{"fail", `OR FAIL`},
	{"rollback", `OR ROLLBACK`},
}

// Schema axis. Column constraints, indexes, triggers, and foreign keys.

var r38dSchemaAxis = []r38dCell{
	{"rowid", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c)`},
	{"without-rowid", `CREATE TABLE t(a INTEGER, b TEXT, c, PRIMARY KEY(a)) WITHOUT ROWID`},
	{"uniq-col", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT UNIQUE, c)`},
	// The same constraint spelled as a separate index rather than inline: it is
	// the same b-tree, but a different path through the schema builder.
	{"uniq-index", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c);
CREATE UNIQUE INDEX ub ON t(b)`},
	{"uniq-partial", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c);
CREATE UNIQUE INDEX ub ON t(b) WHERE c > 15`},
	{"uniq-composite", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c, UNIQUE(b,c))`},
	{"check", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c, CHECK(c < 100))`},
	{"notnull", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT NOT NULL, c)`},
	{"default", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT DEFAULT 'dflt', c DEFAULT 7)`},
	{"gen-virtual", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c, d AS (c*2))`},
	{"gen-stored", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c, d AS (c*2) STORED)`},
	{"autoincrement", `CREATE TABLE t(a INTEGER PRIMARY KEY AUTOINCREMENT, b TEXT, c)`},
	{"strict", `CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c ANY) STRICT`},
	{"fk", `PRAGMA foreign_keys=ON;
CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k))`},
	{"fk-cascade", `PRAGMA foreign_keys=ON;
CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k) ON DELETE CASCADE)`},
	{"fk-deferred", `PRAGMA foreign_keys=ON;
CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REFERENCES p(k) DEFERRABLE INITIALLY DEFERRED)`},
}

// Trigger axis. Each logs to log(e,x,y) table; instead-of cell redirects target to view.

const r38dInsteadOf = `CREATE TRIGGER gvi INSTEAD OF INSERT ON vt BEGIN INSERT INTO log(e,x,y) VALUES('vi',new.a,new.b); INSERT INTO t(a,b,c) VALUES(new.a,new.b,new.c); END;
CREATE TRIGGER gvu INSTEAD OF UPDATE ON vt BEGIN INSERT INTO log(e,x,y) VALUES('vu',old.a,new.b); UPDATE t SET b=new.b, c=new.c WHERE a=old.a; END;
CREATE TRIGGER gvd INSTEAD OF DELETE ON vt BEGIN INSERT INTO log(e,x,y) VALUES('vd',old.a,old.b); DELETE FROM t WHERE a=old.a; END`

var r38dTriggerAxis = []r38dCell{
	{"none", ``},
	{"before-insert", `CREATE TRIGGER g1 BEFORE INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('bi',new.a,new.b); END`},
	{"after-insert", `CREATE TRIGGER g1 AFTER INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('ai',new.a,new.b); END`},
	{"before-update", `CREATE TRIGGER g1 BEFORE UPDATE ON t BEGIN INSERT INTO log(e,x,y) VALUES('bu',old.a,new.b); END`},
	{"after-update", `CREATE TRIGGER g1 AFTER UPDATE ON t BEGIN INSERT INTO log(e,x,y) VALUES('au',old.a,new.b); END`},
	{"before-delete", `CREATE TRIGGER g1 BEFORE DELETE ON t BEGIN INSERT INTO log(e,x,y) VALUES('bd',old.a,old.b); END`},
	{"after-delete", `CREATE TRIGGER g1 AFTER DELETE ON t BEGIN INSERT INTO log(e,x,y) VALUES('ad',old.a,old.b); END`},
	{"when-clause", `CREATE TRIGGER g1 AFTER INSERT ON t WHEN new.a > 4 BEGIN INSERT INTO log(e,x,y) VALUES('wi',new.a,new.b); END`},
	// Two triggers on one event: firing ORDER is observable through the log.
	{"two-on-insert", `CREATE TRIGGER g1 AFTER INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('a1',new.a,new.b); END;
CREATE TRIGGER g2 AFTER INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('a2',new.a,new.b); END`},
	// One trigger per event, so a single cell covers whichever event the
	// statement axis happens to raise.
	{"all-events", `CREATE TRIGGER g1 AFTER INSERT ON t BEGIN INSERT INTO log(e,x,y) VALUES('ai',new.a,new.b); END;
CREATE TRIGGER g2 AFTER UPDATE ON t BEGIN INSERT INTO log(e,x,y) VALUES('au',old.a,new.b); END;
CREATE TRIGGER g3 AFTER DELETE ON t BEGIN INSERT INTO log(e,x,y) VALUES('ad',old.a,old.b); END`},
	{"raise-abort", `CREATE TRIGGER g1 BEFORE INSERT ON t WHEN new.a = 6 BEGIN SELECT RAISE(ABORT,'no6'); END`},
	{"raise-ignore", `CREATE TRIGGER g1 BEFORE INSERT ON t WHEN new.a = 6 BEGIN SELECT RAISE(IGNORE); END`},
	{"raise-rollback", `CREATE TRIGGER g1 BEFORE INSERT ON t WHEN new.a = 6 BEGIN SELECT RAISE(ROLLBACK,'no6'); END`},
	// A trigger that writes back into the table it fires on, while the outer
	// statement's cursor is still open on it.
	{"writeback", `CREATE TRIGGER g1 AFTER INSERT ON t BEGIN UPDATE t SET c = c + 1000 WHERE a = 1; INSERT INTO log(e,x,y) VALUES('wb',new.a,new.b); END`},
	{"instead-of", r38dInsteadOf},
}

// Transaction axis. Autocommit, BEGIN/COMMIT, or SAVEPOINT variants.

var r38dTxnAxis = []r38dCell{
	{"autocommit", ``},
	{"begin", `open`},        // BEGIN before, nothing after: readbacks run inside
	{"begin-commit", `both`}, // BEGIN before, COMMIT after: deferred FKs fire here
	{"savepoint", `save`},    // SAVEPOINT/RELEASE
	{"savepoint-rollback", `back`},
}

// Readback axis. Observes write effect via multiple channels (changes, rows, log, etc).

var r38dReadAxis = []r38dCell{
	{"none", ``},
	{"changes", `SELECT changes()`},
	{"last-insert-rowid", `SELECT last_insert_rowid()`},
	{"total-changes", `SELECT total_changes()`},
	// No ORDER BY: the PHYSICAL order rows come back in, which an ordered
	// readback hides and which a write that rebuilds a page can change.
	{"scan-order", `SELECT * FROM t`},
	{"rowid-dump", `SELECT rowid, * FROM t ORDER BY rowid`},
	{"typeof", `SELECT a, typeof(a), b, typeof(b), c, typeof(c) FROM t ORDER BY a`},
	{"quote", `SELECT quote(a), quote(b), quote(c) FROM t ORDER BY a`},
	// Ordering by b reads through the UNIQUE index where one exists, so an index
	// that disagrees with its table shows up here and nowhere else.
	{"index-order", `SELECT b, c FROM t ORDER BY b`},
	{"aggregate", `SELECT count(*), sum(c), group_concat(b,'|') FROM t`},
	{"sqlite-sequence", `SELECT * FROM sqlite_sequence`},
	{"foreign-key-check", `PRAGMA foreign_key_check`},
	{"view", `SELECT * FROM vt ORDER BY a`},
	{"schema-sql", `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY name`},
	{"table-info", `PRAGMA table_info(t)`},
}

// Ground-truth readback appended to every cell: counters, table state, log, parent, integrity check.
var r38dGroundTruth = []string{
	`SELECT changes(), total_changes(), last_insert_rowid()`,
	`SELECT * FROM t ORDER BY a`,
	`SELECT e,x,y FROM log ORDER BY rowid`,
	`SELECT k,v FROM p ORDER BY k`,
	`PRAGMA integrity_check`,
}

// Tracked backlog of divergences. Empty when all cells agree.
var r38dOpen = map[string]string{}

var r38dAxisNames = []string{"stmt", "conflict", "schema", "trigger", "txn", "read"}

func r38dAxes() [][]r38dCell {
	return [][]r38dCell{
		r38dStmtAxis, r38dConflictAxis, r38dSchemaAxis,
		r38dTriggerAxis, r38dTxnAxis, r38dReadAxis,
	}
}

// r38dCoord is a cell's address in the space.
type r38dCoord struct{ stmt, conflict, schema, trigger, txn, read string }

func (c r38dCoord) String() string {
	return fmt.Sprintf("stmt=%s conflict=%s schema=%s trigger=%s txn=%s read=%s",
		c.stmt, c.conflict, c.schema, c.trigger, c.txn, c.read)
}

// Cell's statement list and the index of the DML statement under test.
type r38dPlan struct {
	stmts  []string
	dmlIdx int
}

// Parses multi-statement DDL into individual statements, splitting on ";\n" only.
func r38dSplit(blob string) []string {
	var out []string
	for _, s := range strings.Split(blob, ";\n") {
		if s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";")); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Assembles a cell. ok=false if the combination is not a parity question.
func r38dBuild(ix []int) (r38dPlan, r38dCoord, bool) {
	ax := r38dAxes()
	st, cf := ax[0][ix[0]], ax[1][ix[1]]
	sc, tg := ax[2][ix[2]], ax[3][ix[3]]
	tx, rd := ax[4][ix[4]], ax[5][ix[5]]
	co := r38dCoord{st.name, cf.name, sc.name, tg.name, tx.name, rd.name}

	// Schema axis owns column d; no other axis can define it.
	var pre []string
	pre = append(pre, `CREATE TABLE p(k INTEGER PRIMARY KEY, v TEXT)`, r38dParent)
	pre = append(pre, r38dSplit(sc.sql)...)
	pre = append(pre, `CREATE TABLE src(a INTEGER, b TEXT, c)`, r38dSrc)
	pre = append(pre, `CREATE TABLE log(e TEXT, x, y)`, r38dSeed)
	// View exists in every cell; only instead-of trigger makes it a write target.
	pre = append(pre, `CREATE VIEW vt AS SELECT a,b,c FROM t`)
	pre = append(pre, r38dSplit(tg.sql)...)

	target := "t"
	if co.trigger == "instead-of" {
		target = "vt"
	}
	dml := st.sql
	dml = strings.ReplaceAll(dml, "%TBL%", target)
	// Conflict clause dropped from statements that don't support it.
	if strings.Contains(dml, "%OR%") {
		if cf.sql == "" {
			dml = strings.ReplaceAll(dml, " %OR%", "")
		} else {
			dml = strings.ReplaceAll(dml, "%OR%", cf.sql)
		}
	}

	switch tx.sql {
	case "open", "both":
		pre = append(pre, `BEGIN`)
	case "save", "back":
		pre = append(pre, `SAVEPOINT sp`)
	}
	stmts := append([]string(nil), pre...)
	dmlIdx := len(stmts)
	stmts = append(stmts, dml)
	switch tx.sql {
	case "both":
		stmts = append(stmts, `COMMIT`)
	case "save":
		stmts = append(stmts, `RELEASE sp`)
	case "back":
		stmts = append(stmts, `ROLLBACK TO sp`, `RELEASE sp`)
	}
	if rd.sql != "" {
		stmts = append(stmts, rd.sql)
	}
	stmts = append(stmts, r38dGroundTruth...)
	return r38dPlan{stmts, dmlIdx}, co, true
}

// r38dVerdict is what one cell resolved to.
type r38dVerdict int

const (
	r38dAgreed r38dVerdict = iota
	r38dMutualReject
	r38dDecline    // we reject, the oracle serves
	r38dWrongServe // we serve, the oracle rejects — a wrong answer
	r38dWrongState // both ran; an observation differs
	r38dArtifact   // differed only because runOne ran the failing DML twice
)

var r38dVerdictName = map[r38dVerdict]string{
	r38dAgreed: "agreed", r38dMutualReject: "mutual-reject", r38dDecline: "decline",
	r38dWrongServe: "WRONG-SERVE", r38dWrongState: "WRONG-STATE", r38dArtifact: "harness-artifact",
}

func r38dJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Finds the first statement whose result differs between engines.
func r38dFirstDiff(stmts []string, m, c []map[string]any, from int) (int, bool) {
	n := len(stmts)
	if len(m) < n || len(c) < n {
		return -1, false
	}
	for i := from; i < n; i++ {
		if r38dJSON(m[i]) != r38dJSON(c[i]) {
			return i, true
		}
	}
	return -1, false
}

// Runs a cell on both engines and classifies the result.
// Handles execution-count correction when DML errors (runs twice vs once in oracle).
func r38dRunCell(t *testing.T, p r38dPlan) (r38dVerdict, string) {
	t.Helper()
	m := run(t, "musql", p.stmts)
	c := run(t, "cgo", p.stmts)
	if len(m) != len(p.stmts) || len(c) != len(p.stmts) {
		return r38dWrongState, fmt.Sprintf("worker returned %d/%d results for %d statements", len(m), len(c), len(p.stmts))
	}
	// Setup must agree; schema-level divergences are reported, not silently compared.
	if i, ok := r38dFirstDiff(p.stmts, m, c, 0); ok && i < p.dmlIdx {
		return r38dWrongState, fmt.Sprintf("SETUP diverges at [%d] %s\n    cgo: %s\n    mus: %s",
			i, p.stmts[i], r38dJSON(c[i]), r38dJSON(m[i]))
	}
	mErr := m[p.dmlIdx]["kind"] == "error"
	cErr := c[p.dmlIdx]["kind"] == "error"
	switch {
	case mErr && !cErr:
		return r38dDecline, fmt.Sprintf("[%d] %s — oracle serves, this engine rejects", p.dmlIdx, p.stmts[p.dmlIdx])
	case !mErr && cErr:
		return r38dWrongServe, fmt.Sprintf("[%d] %s — this engine serves what the oracle rejects", p.dmlIdx, p.stmts[p.dmlIdx])
	}
	i, differs := r38dFirstDiff(p.stmts, m, c, p.dmlIdx)
	if !differs {
		if mErr {
			return r38dMutualReject, ""
		}
		return r38dAgreed, ""
	}
	if mErr {
		// Re-run oracle with matching execution count for comparison.
		dbl := append([]string(nil), p.stmts[:p.dmlIdx+1]...)
		dbl = append(dbl, p.stmts[p.dmlIdx])
		dbl = append(dbl, p.stmts[p.dmlIdx+1:]...)
		c2 := run(t, "cgo", dbl)
		if len(c2) == len(dbl) {
			same := true
			for j := p.dmlIdx + 1; j < len(p.stmts); j++ {
				if r38dJSON(m[j]) != r38dJSON(c2[j+1]) {
					same = false
					break
				}
			}
			if same {
				return r38dArtifact, ""
			}
			for j := p.dmlIdx + 1; j < len(p.stmts); j++ {
				if r38dJSON(m[j]) != r38dJSON(c2[j+1]) {
					return r38dWrongState, fmt.Sprintf("[%d] %s\n    dml: %s (both reject; compared at EQUAL execution counts)\n    cgo: %s\n    mus: %s",
						j, p.stmts[j], p.stmts[p.dmlIdx], r38dJSON(c2[j+1]), r38dJSON(m[j]))
				}
			}
		}
		return r38dArtifact, ""
	}
	return r38dWrongState, fmt.Sprintf("[%d] %s\n    dml: %s\n    cgo: %s\n    mus: %s",
		i, p.stmts[i], p.stmts[p.dmlIdx], r38dJSON(c[i]), r38dJSON(m[i]))
}

// Generates axis combinations: pairwise by default, full product or pinned axes on request.
func r38dCombos() [][]int {
	ax := r38dAxes()
	sizes := make([]int, len(ax))
	for i := range ax {
		sizes[i] = len(ax[i])
	}
	switch {
	case os.Getenv("R38D_FULL") != "":
		out := [][]int{{}}
		for _, n := range sizes {
			next := make([][]int, 0, len(out)*n)
			for _, pre := range out {
				for i := 0; i < n; i++ {
					next = append(next, append(append([]int(nil), pre...), i))
				}
			}
			out = next
		}
		return out
	case os.Getenv("R38D_AXES") != "":
		pin := map[string]bool{}
		for _, a := range strings.Split(os.Getenv("R38D_AXES"), ",") {
			pin[strings.TrimSpace(a)] = true
		}
		sel := make([]int, len(sizes))
		for i := range sizes {
			sel[i] = 1
			if pin[r38dAxisNames[i]] {
				sel[i] = sizes[i]
			}
		}
		out := [][]int{{}}
		for _, n := range sel {
			next := make([][]int, 0, len(out)*n)
			for _, pre := range out {
				for i := 0; i < n; i++ {
					next = append(next, append(append([]int(nil), pre...), i))
				}
			}
			out = next
		}
		return out
	default:
		return r36Pairwise(sizes)
	}
}

// Walks the DML shape space and reports divergences by cell.
func TestR38DDMLShapeSpace(t *testing.T) {
	if testing.Short() {
		t.Skip("r38d DML shape space: full run")
	}
	ax := r38dAxes()
	combos := r38dCombos()

	tally := map[r38dVerdict]int{}
	byAxisCell := map[string]map[r38dVerdict]int{}
	type finding struct {
		co   r38dCoord
		ix   []int
		v    r38dVerdict
		note string
	}
	var findings []finding

	for _, ix := range combos {
		p, co, ok := r38dBuild(ix)
		if !ok {
			continue
		}
		v, note := r38dRunCell(t, p)
		tally[v]++
		for i := range ax {
			k := r38dAxisNames[i] + "=" + ax[i][ix[i]].name
			if byAxisCell[k] == nil {
				byAxisCell[k] = map[r38dVerdict]int{}
			}
			byAxisCell[k][v]++
		}
		if v == r38dDecline || v == r38dWrongServe || v == r38dWrongState {
			findings = append(findings, finding{co, append([]int(nil), ix...), v, note})
		}
	}

	t.Logf("R38D DML SHAPE SPACE: cells=%d agreed=%d mutualReject=%d artifact=%d decline=%d WRONG-SERVE=%d WRONG-STATE=%d",
		len(combos), tally[r38dAgreed], tally[r38dMutualReject], tally[r38dArtifact],
		tally[r38dDecline], tally[r38dWrongServe], tally[r38dWrongState])

	// Causal attribution: reset each axis to baseline and re-run to identify cause.
	if len(findings) > 0 && os.Getenv("R38D_NO_ATTRIBUTION") == "" {
		blame := map[string]int{}
		for _, f := range findings {
			for a := range ax {
				if f.ix[a] == 0 {
					continue // already baseline on this axis
				}
				alt := append([]int(nil), f.ix...)
				alt[a] = 0
				p, _, ok := r38dBuild(alt)
				if !ok {
					continue
				}
				if v, _ := r38dRunCell(t, p); v == r38dAgreed || v == r38dMutualReject || v == r38dArtifact {
					blame[r38dAxisNames[a]+"="+ax[a][f.ix[a]].name]++
				}
			}
		}
		keys := make([]string, 0, len(blame))
		for k := range blame {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if blame[keys[i]] != blame[keys[j]] {
				return blame[keys[i]] > blame[keys[j]]
			}
			return keys[i] < keys[j]
		})
		t.Logf("R38D CAUSAL ATTRIBUTION (resetting this axis cell makes the finding go away):")
		for _, k := range keys {
			t.Logf("   %-32s implicated in %d findings", k, blame[k])
		}
	}

	// Report findings grouped by verdict.
	if len(findings) > 0 {
		sort.Slice(findings, func(i, j int) bool {
			if findings[i].v != findings[j].v {
				return findings[i].v > findings[j].v
			}
			return findings[i].co.String() < findings[j].co.String()
		})
		for _, f := range findings {
			t.Logf("R38D %s: %s\n    %s", r38dVerdictName[f.v], f.co, f.note)
		}
	}

	// Per-axis-cell divergence tallies.
	keys := make([]string, 0, len(byAxisCell))
	for k := range byAxisCell {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		bi, bj := byAxisCell[keys[i]], byAxisCell[keys[j]]
		ni := bi[r38dDecline] + bi[r38dWrongServe] + bi[r38dWrongState]
		nj := bj[r38dDecline] + bj[r38dWrongServe] + bj[r38dWrongState]
		if ni != nj {
			return ni > nj
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		b := byAxisCell[k]
		if n := b[r38dDecline] + b[r38dWrongServe] + b[r38dWrongState]; n > 0 {
			t.Logf("   %-32s decline=%d wrongServe=%d wrongState=%d",
				k, b[r38dDecline], b[r38dWrongServe], b[r38dWrongState])
		}
	}

	// Gate: r38dOpen is tracked backlog; fail on new divergences or fixed cells.
	if os.Getenv("R38D_REPORT_ONLY") != "" || os.Getenv("R38D_AXES") != "" || os.Getenv("R38D_FULL") != "" {
		return // a restricted or widened walk visits different cells; the pin does not apply
	}
	seen := map[string]bool{}
	for _, f := range findings {
		k := f.co.String()
		seen[k] = true
		if _, tracked := r38dOpen[k]; !tracked {
			t.Errorf("R38D NEW %s, not in the tracked backlog: %s\n    %s\n"+
				"    If this is a real regression, fix it. If it is a newly measured gap, add it to\n"+
				"    r38dOpen with the rule it belongs to (see that map's doc comment for the buckets).",
				r38dVerdictName[f.v], k, f.note)
		}
	}
	for k, bucket := range r38dOpen {
		if !seen[k] {
			t.Errorf("R38D FIXED: %s\n    (%s) now agrees with the oracle. Delete its r38dOpen entry --\n"+
				"    a stale entry is what lets the NEXT regression in this cell pass unnoticed.", k, bucket)
		}
	}
}
