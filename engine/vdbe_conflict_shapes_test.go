package engine

// This file gates conflict-handling compilation in the write path.

import (
	"strings"
	"testing"
)

type conflictShapeCase struct {
	setup []string
	stmt  string
}

// conflictShapesCompiled: statements whose promotion this batch is about.
var conflictShapesCompiled = []conflictShapeCase{
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`UPDATE OR IGNORE t SET a=1`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`UPDATE t SET a=1`},
	// UPDATE, explicit OR-clause.
	{[]string{`CREATE TABLE t(a,b UNIQUE)`}, `UPDATE OR REPLACE t SET b=b+10`},
	{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE)`}, `UPDATE OR IGNORE t SET b=b+10`},
	{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE)`}, `UPDATE OR FAIL t SET b=b+10`},
	{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE)`}, `UPDATE OR ABORT t SET b=b+10`},
	{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE)`}, `UPDATE OR ROLLBACK t SET b=b+10`},

	// UPDATE, per-constraint declared ON CONFLICT default.
	{[]string{`CREATE TABLE t(a,b UNIQUE ON CONFLICT REPLACE)`}, `UPDATE t SET b=b+10`},
	{[]string{`CREATE TABLE t(a,b INTEGER UNIQUE ON CONFLICT FAIL)`}, `UPDATE t SET b=b+10`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`}, `UPDATE t SET a=1`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`}, `UPDATE OR REPLACE t SET a=1`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT ABORT,b)`}, `UPDATE OR IGNORE t SET a=1`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT REPLACE,b)`}, `UPDATE OR ABORT t SET a=1`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE, b UNIQUE ON CONFLICT REPLACE)`}, `UPDATE t SET b=10 WHERE a=3`},
	{[]string{`CREATE TABLE t(a UNIQUE, b NOT NULL ON CONFLICT IGNORE)`}, `UPDATE t SET a=1`},

	// NOT NULL / CHECK under a conflict action.
	{[]string{`CREATE TABLE t(a,b NOT NULL)`}, `UPDATE OR FAIL t SET b = CASE a WHEN 3 THEN NULL ELSE b||'!' END`},
	{[]string{`CREATE TABLE t(a,b NOT NULL)`}, `UPDATE OR IGNORE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`},
	{[]string{`CREATE TABLE t(a,b NOT NULL ON CONFLICT IGNORE)`}, `UPDATE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`},
	{[]string{`CREATE TABLE t(a,b NOT NULL ON CONFLICT REPLACE DEFAULT 'dd')`}, `UPDATE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END`},
	{[]string{`CREATE TABLE t(a, b INT NOT NULL ON CONFLICT REPLACE DEFAULT '5')`}, `UPDATE t SET b = CASE a WHEN 2 THEN NULL ELSE b END`},
	{[]string{`CREATE TABLE t(a, c NOT NULL ON CONFLICT REPLACE DEFAULT 'ccc', g AS (c||'!'))`}, `UPDATE t SET c = CASE a WHEN 2 THEN NULL ELSE c END`},
	{[]string{`CREATE TABLE t(a,b NOT NULL)`}, `UPDATE OR REPLACE t SET b = NULL WHERE a=2`},
	{[]string{`CREATE TABLE t(a,b, CHECK(b<10))`}, `UPDATE OR IGNORE t SET b=b+1`},
	{[]string{`CREATE TABLE t(a,b, CHECK(b<10))`}, `UPDATE OR REPLACE t SET b=b+1`},
	{[]string{`CREATE TABLE t(a,b, CHECK(b<10))`}, `UPDATE OR FAIL t SET b=b+1`},

	// Index shapes the conflict probe has to see.
	{[]string{`CREATE TABLE t(a,b)`, `CREATE UNIQUE INDEX i1 ON t(a)`, `CREATE UNIQUE INDEX i2 ON t(b)`}, `UPDATE OR REPLACE t SET a=2,b=20 WHERE a=3`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE UNIQUE INDEX i1 ON t(a)`, `CREATE UNIQUE INDEX i2 ON t(b)`}, `UPDATE OR REPLACE t SET a=1,b=20 WHERE a=3`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE UNIQUE INDEX px ON t(a) WHERE b>0`}, `UPDATE OR IGNORE t SET a=1`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE UNIQUE INDEX ex ON t(abs(a))`}, `UPDATE OR REPLACE t SET a=-1 WHERE b='z'`},
	{[]string{`CREATE TABLE t(a,b)`, `CREATE UNIQUE INDEX ux ON t(a DESC)`}, `UPDATE OR REPLACE t SET a=1 WHERE b='y'`},
	{[]string{`CREATE TABLE t(a,b,c, UNIQUE(a,b))`}, `UPDATE OR REPLACE t SET a=1 WHERE c='r'`},
	{[]string{`CREATE TABLE t(a TEXT COLLATE NOCASE UNIQUE ON CONFLICT IGNORE, b)`}, `UPDATE t SET a='a'`},
	{[]string{`CREATE TABLE t(a, g AS (a*2) UNIQUE ON CONFLICT IGNORE, b)`}, `UPDATE t SET a=1`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `UPDATE OR IGNORE t SET a=NULL`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `UPDATE OR REPLACE t SET b='q' WHERE a=1`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`}, `UPDATE t SET b=b||'!'`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v UNIQUE) WITHOUT ROWID`}, `UPDATE OR REPLACE t SET v=1 WHERE k='c'`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v UNIQUE) WITHOUT ROWID`}, `UPDATE OR IGNORE t SET v=1`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v INTEGER UNIQUE) WITHOUT ROWID`}, `UPDATE OR REPLACE t SET v=v+10`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v UNIQUE ON CONFLICT IGNORE) WITHOUT ROWID`}, `UPDATE t SET v=1`},

	// STRICT x conflict-aware: main's OpTypeCheck promotion and this one meet
	// on the same statement. Measured against the oracle in the harness
	// battery's "STRICT x conflict-aware" section.
	{[]string{`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`}, `UPDATE OR REPLACE t SET a=1`},
	{[]string{`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`}, `UPDATE OR IGNORE t SET a=1`},
	{[]string{`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`}, `UPDATE OR IGNORE t SET a='abc'`},
	{[]string{`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`}, `UPDATE OR REPLACE t SET b=x'0102'`},
	{[]string{`CREATE TABLE t(a INT UNIQUE ON CONFLICT IGNORE, b TEXT) STRICT`}, `UPDATE t SET a=1`},
	{[]string{`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`}, `UPDATE OR FAIL t SET a=1`},
	{[]string{`CREATE TABLE g(a INT, b BLOB AS (a), c INT UNIQUE) STRICT`}, `UPDATE OR IGNORE g SET a=1`},

	// The conflict clause combined with the rest of the UPDATE grammar.
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `UPDATE OR IGNORE t SET a=1 RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `UPDATE OR REPLACE t SET a=1 WHERE b='y' RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE,b)`}, `UPDATE t SET a=1 RETURNING a,b`},
	{[]string{`CREATE TABLE t(a,b, CHECK(b<10))`}, `UPDATE OR IGNORE t SET b=b+1 RETURNING a,b`},
	{[]string{`CREATE TABLE t(a,b NOT NULL)`}, `UPDATE OR IGNORE t SET b = CASE a WHEN 2 THEN NULL ELSE b||'!' END RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE INDEX ix ON t(b)`}, `UPDATE OR IGNORE t INDEXED BY ix SET a=1 WHERE b='y'`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `UPDATE OR REPLACE main.t SET a=1 WHERE b='y'`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE s(x)`}, `UPDATE OR IGNORE t SET a=1 WHERE b IN (SELECT 'y' FROM s)`},
	{[]string{`CREATE TABLE t(a TEXT UNIQUE, b)`}, `UPDATE OR REPLACE t SET a=1 WHERE b='y'`},
	{[]string{`CREATE TABLE t(a INTEGER UNIQUE, b)`}, `UPDATE OR IGNORE t SET a='1' WHERE b='y'`},
	{[]string{`PRAGMA foreign_keys=ON`, `CREATE TABLE p(k UNIQUE)`, `CREATE TABLE c(x REFERENCES p(k))`}, `UPDATE OR REPLACE p SET k=1 WHERE k=2`},

	// INSERT ... VALUES ... RETURNING carrying an OR-clause / a declared default.
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `INSERT OR IGNORE INTO t VALUES(1,'y') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `INSERT OR IGNORE INTO t VALUES(1,'y'),(2,'z'),(1,'w'),(3,'q') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `INSERT OR REPLACE INTO t VALUES(1,'y'),(2,'z') RETURNING a,b,rowid`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `INSERT OR ABORT INTO t VALUES(2,'z'),(1,'y') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `INSERT OR FAIL INTO t VALUES(2,'z'),(1,'y'),(3,'q') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a NOT NULL, b)`}, `INSERT OR IGNORE INTO t VALUES(NULL,'y'),(2,'z') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a CHECK(a>0), b)`}, `INSERT OR IGNORE INTO t VALUES(-1,'y'),(2,'z') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a NOT NULL DEFAULT 7, b)`}, `INSERT OR REPLACE INTO t VALUES(NULL,'y'),(2,'z') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT IGNORE, b)`}, `INSERT INTO t VALUES(1,'y') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE ON CONFLICT REPLACE, b)`}, `INSERT INTO t VALUES(1,'y'),(3,'z') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `INSERT OR IGNORE INTO t VALUES(1,'y'),(2,'z') RETURNING *`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`}, `INSERT OR IGNORE INTO t VALUES(1,'y'),(2,'z') RETURNING k,b`},
	{[]string{`CREATE TABLE t(a UNIQUE,b)`}, `INSERT OR REPLACE INTO t VALUES(1,'y') RETURNING a+100 AS z, upper(b)`},
	{[]string{`CREATE TABLE t(a UNIQUE, g AS (a*2), b)`}, `INSERT OR IGNORE INTO t(a,b) VALUES(1,'y'),(2,'z') RETURNING a,g,b`},

	// A non-integer rowid under an OR-clause. These COMPILE and must ABORT the
	// statement -- see the OpMustBeInt emission in emitInsertRowBody for the
	// insert.c:1534 citation. They are listed here because the abort has to
	// come from the COMPILED program, and a differential gate cannot tell which
	// route aborted.
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`}, `INSERT OR IGNORE INTO t VALUES('abc','y'),(2,'z') RETURNING k,b`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`}, `INSERT OR IGNORE INTO t VALUES(1.5,'y'),(2,'z') RETURNING k,b`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`}, `INSERT OR IGNORE INTO t VALUES(x'0102','y'),(2,'z') RETURNING k,b`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`}, `INSERT OR IGNORE INTO t VALUES('abc','y')`},
	{[]string{`CREATE TABLE t(a,b)`}, `INSERT OR IGNORE INTO t(rowid,a,b) VALUES('abc',1,'y') RETURNING a,b`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`}, `INSERT OR FAIL INTO t VALUES(1,'x'),('abc','y') RETURNING k,b`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`}, `INSERT OR REPLACE INTO t VALUES(1,'x'),('abc','y') RETURNING k,b`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`}, `INSERT OR ROLLBACK INTO t VALUES(1,'x'),('abc','y') RETURNING k,b`},

	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`, `CREATE TABLE s(k,b)`}, `INSERT OR IGNORE INTO t SELECT k,b FROM s`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, b)`, `CREATE TABLE s(k,b)`}, `INSERT OR FAIL INTO t SELECT k,b FROM s`},
	{[]string{`CREATE TABLE t(a UNIQUE, b)`, `CREATE TABLE s(a,b)`}, `INSERT OR IGNORE INTO t SELECT a,b FROM s`},

	{[]string{`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`}, `INSERT OR IGNORE INTO t VALUES(1,'y'),(2,'z') RETURNING a,b`},
	{[]string{`CREATE TABLE t(a INT UNIQUE, b TEXT) STRICT`}, `INSERT OR IGNORE INTO t VALUES('nope','y') RETURNING a,b`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`}, `INSERT OR IGNORE INTO t VALUES('a',9),('b',2) RETURNING k,v`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`}, `INSERT OR REPLACE INTO t VALUES('a',9),('b',2) RETURNING k,v`},

	// A conflict-resolving UPDATE that MOVES THE KEY THE LOOP RE-SEEKS BY. Every
	// one of these was in the DECLINED table below, on the note "this compiler
	// scans a frozen materialization and would re-apply the SET to the stale
	// image" -- which was a true description of the loop and a false conclusion
	// about the fix: emitting the re-seek update.c:875-878 codes for EVERY
	// ONEPASS_OFF statement (and update.c:868-869's PRIMARY KEY spelling for a
	// keyless table, reseekRowStoreByPK) is the fix, and the triggered path had
	// already been given it. See compileUpdateStmt.
	//
	// They were LIVE WRONG ANSWERS while they went unlowered, not merely
	// missing: applyPendingUpdatesConflict builds its pendings list from the
	// pre-statement image too. Measured against the 3.53.3 oracle in
	// compat-harness/conflict_identity_move_test.go, which is the differential
	// half of these rows.
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`}, `UPDATE OR REPLACE t SET k=k+1`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY,b)`}, `UPDATE OR IGNORE t SET k='abc' WHERE b='y'`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY, v) WITHOUT ROWID`}, `UPDATE OR REPLACE t SET k=k+1`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`}, `UPDATE OR REPLACE t SET k=char(unicode(k)+1)`},
	{[]string{`CREATE TABLE t(k INTEGER PRIMARY KEY ON CONFLICT REPLACE, v) WITHOUT ROWID`}, `UPDATE t SET k=k+1`},
	{[]string{`CREATE TABLE t(x,y,v, PRIMARY KEY(x,y)) WITHOUT ROWID`}, `UPDATE OR REPLACE t SET y=y+1`},
	{[]string{`CREATE TABLE t(x,y,v, PRIMARY KEY(x,y)) WITHOUT ROWID`}, `UPDATE OR IGNORE t SET x=x+1`},

	// A REPLACE victim's implicit delete firing the table's own DELETE triggers
	// under "PRAGMA recursive_triggers=ON" -- the UPDATE half of the shape whose
	// INSERT half already compiled. compileReplaceVictimDeletePlans compiles the
	// programs and opUpdateRow fires them, with insert.c's post-cascade
	// uniqueness RE-CHECK and its OP_CursorLock pin (writeCtx.pinnedTable).
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a UNIQUE,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER td AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE OR REPLACE t SET a=1 WHERE b='y'`},
	{[]string{`PRAGMA recursive_triggers=ON`, `CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tbd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('b'||old.a); END`,
		`CREATE TRIGGER tad AFTER DELETE ON t BEGIN INSERT INTO log VALUES(old.a); END`},
		`UPDATE OR REPLACE t SET a=a+1`},

	// "INSERT ... ON CONFLICT ... DO UPDATE" against a table with UPDATE
	// triggers: the DO UPDATE branch IS an UPDATE (upsert.c:325-326), so it
	// fires them. Declined by every write path once, and the decline outlived
	// the reason for it, which is why this shape went unlowered so long.
	{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.a); END`},
		`INSERT INTO t(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`},
	{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tb BEFORE UPDATE ON t BEGIN INSERT INTO log VALUES(old.c); END`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.c); END`},
		`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
	{[]string{`CREATE TABLE t(a INTEGER PRIMARY KEY,b)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(old.a||'->'||new.a); END`},
		`INSERT INTO t VALUES(1,'y') ON CONFLICT(a) DO UPDATE SET a=9`},
	{[]string{`CREATE TABLE t(k TEXT PRIMARY KEY, v) WITHOUT ROWID`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.v); END`},
		`INSERT INTO t VALUES('a',9) ON CONFLICT(k) DO UPDATE SET v=excluded.v`},
	{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t WHEN new.c > 50 BEGIN INSERT INTO log VALUES(new.c); END`},
		`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c`},
	{[]string{`CREATE TABLE t(a UNIQUE,b,c)`, `CREATE TABLE log(x)`,
		`CREATE TRIGGER tu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES('au'); END`},
		`INSERT INTO t(a,b,c) VALUES(1,2,99) ON CONFLICT(a) DO UPDATE SET c=excluded.c WHERE c<50`},
}

// conflictShapesDeclined: conflict shapes this compiler REFUSES, each for a
// reason the differential harness cannot observe on its own. Asserting they
// still decline is what stops a later "just widen the gate" change from
// compiling them by accident.
var conflictShapesDeclined = []struct {
	why string
	conflictShapeCase
}{
	// EMPTY, and each entry left for a different reason in a different slice.
	// The table stays: it is where a future decline goes, and an empty one is
	// still worth reading, because what emptied it is the same mistake three
	// times over -- a note that described this emitter accurately and then drew
	// a conclusion the C does not support.
	//
	//   - "conflict clause on a table with UPDATE triggers", and its DECLARED
	//     default twin, were the constraint/BEFORE emission ORDER. update.c
	//     fires BEFORE UPDATE triggers ahead of the checks and says so in its
	//     own words (:978-980); compileUpdateStmt now emits emitUpdateNotNull,
	//     emitCheckConstraintsAction and OpMakeRecord below the fire and the
	//     row-gone guard, which is :1030's position.
	//   - the IDENTITY-MOVE family -- an OR-clause UPDATE reassigning the rowid,
	//     or a WITHOUT ROWID PRIMARY KEY (INTEGER, TEXT, the declared-default
	//     spelling and either column of a composite one) -- said this compiler
	//     "scans a frozen materialization and would re-apply the SET to the
	//     stale image". True of the loop, false as a conclusion:
	//     update.c:875-878 RE-SEEKS the real table at the top of every
	//     ONEPASS_OFF iteration, and the triggered path here already had that
	//     seek. Emitting it for the conflict path too is the promotion.
	//   - "REPLACE victim delete firing DELETE triggers (recursive_triggers ON)"
	//     needed the same seek, plus insert.c:2611-2618's OP_CursorLock pin.
	//
	// All of them are in conflictShapesCompiled above now.
}

func compileShape(t *testing.T, tc conflictShapeCase) (*Program, error) {
	t.Helper()
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
	return db.compileWrite(tc.stmt)
}

func TestConflictShapesCompileToBytecode(t *testing.T) {
	for _, tc := range conflictShapesCompiled {
		if _, err := compileShape(t, tc); err != nil {
			t.Errorf("RULE #1: %q should COMPILE, got error: %v.\n"+
				"The differential gate in compat-harness/conflict_codegen_diff_test.go cannot see this -- "+
				"which is exactly why this assertion exists.", tc.stmt, err)
		}
	}
}

func TestDeclinedConflictShapesStayDeclined(t *testing.T) {
	for _, tc := range conflictShapesDeclined {
		if _, err := compileShape(t, tc.conflictShapeCase); err != nil {
			continue // the decline, which is the required outcome
		}
		t.Errorf("%q now COMPILES, but it is declined on purpose (%s).\n"+
			"Compiling it needs its own oracle evidence first -- see the note on this case.", tc.stmt, tc.why)
	}
}

// TestOrAbortReportsTheSameTextAsPlainAbort pins the one BEHAVIOUR CHANGE this
// promotion makes to a statement that already compiled: "UPDATE OR ABORT"
// carries an explicit OR-clause, so it is now conflictAware and reports its
// UNIQUE violation through the pre-store conflict resolver
// (uniqueConflictError, conflict.go) instead of the post-store whole-table
// re-validation (uniqueIndexConflictError, index_write.go). The two are
// deliberately the same wording -- uniqueIndexConflictError's own doc comment
// says so -- and a constraint message is a conformance surface, so the
// equivalence is asserted rather than assumed.
func TestOrAbortReportsTheSameTextAsPlainAbort(t *testing.T) {
	cases := []struct {
		setup []string
		rows  []string
		stmt  string // %s is replaced by "" or " OR ABORT"
	}{
		{[]string{`CREATE TABLE t(a,b UNIQUE)`}, []string{`INSERT INTO t VALUES(1,1),(2,2)`}, `UPDATE%s t SET b=1 WHERE a=2`},
		{[]string{`CREATE TABLE t(a,b)`, `CREATE UNIQUE INDEX ux ON t(b)`}, []string{`INSERT INTO t VALUES(1,1),(2,2)`}, `UPDATE%s t SET b=1 WHERE a=2`},
		{[]string{`CREATE TABLE t(a,b)`, `CREATE UNIQUE INDEX ex ON t(abs(b))`}, []string{`INSERT INTO t VALUES(1,1),(2,2)`}, `UPDATE%s t SET b=-1 WHERE a=2`},
		{[]string{`CREATE TABLE t(a,b)`, `CREATE UNIQUE INDEX px ON t(b) WHERE a>0`}, []string{`INSERT INTO t VALUES(1,1),(2,2)`}, `UPDATE%s t SET b=1 WHERE a=2`},
		{[]string{`CREATE TABLE t(a,b,c, UNIQUE(a,b))`}, []string{`INSERT INTO t VALUES(1,1,'p'),(2,2,'q')`}, `UPDATE%s t SET a=1,b=1 WHERE c='q'`},
	}
	for _, tc := range cases {
		var texts []string
		for _, orClause := range []string{"", " OR ABORT"} {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			for _, s := range append(append([]string{}, tc.setup...), tc.rows...) {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			stmt := strings.Replace(tc.stmt, "%s", orClause, 1)
			err = db.Exec(stmt)
			if err == nil {
				t.Fatalf("%q: expected a UNIQUE violation, got none", stmt)
			}
			texts = append(texts, err.Error())
			db.Discard()
		}
		if texts[0] != texts[1] {
			t.Errorf("%q: OR ABORT changed the reported constraint message\n  plain:    %s\n  OR ABORT: %s",
				tc.stmt, texts[0], texts[1])
		}
	}
}

// TestUpsertDoUpdateHonoursOfColumnList pins the OF-list filter on the COMPILED
// upsert DO UPDATE path, engine-direct.
//
// The compiled tail built its UPDATE trigger plans with the UNFILTERED builder,
// on the stated grounds that "triggersCompilable refuses an OF-list outright".
// It does not -- that function looks only at a trigger's event, table,
// temp-ness and insteadOf flag, and has no notion of a column list -- so every
// UPDATE trigger on the table fired whatever it was declared OF, and the upsert
// grew a SPURIOUS side effect. The ROW was correct throughout; only the log was
// wrong, which is exactly why the differential test that caught it asserts the
// trigger log (compat-harness/upsert_update_triggers_test.go).
//
// C asks checkColumnOverlap both where the trigger list is walked
// ("&& checkColumnOverlap(p->pColumns, pChanges)", trigger.c:1502) and where
// triggersReallyExist builds the BEFORE/AFTER mask (trigger.c:837).
func TestUpsertDoUpdateHonoursOfColumnList(t *testing.T) {
	for _, tc := range []struct {
		name string
		stmt string
		want []string
	}{
		{"SET c fires only OF c", `INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET c=c+1`, []string{"of-c"}},
		{"SET b fires only OF b", `INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET b=b+1`, []string{"of-b"}},
		{"SET both fires both", `INSERT INTO t1(a,b) VALUES(1,2) ON CONFLICT(a) DO UPDATE SET b=b+1, c=c+1`, []string{"of-b", "of-c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Create(t.TempDir() + "/x.musq")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Discard()
			for _, s := range []string{
				`CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c DEFAULT 0)`,
				`CREATE TABLE log(m)`,
				`CREATE TRIGGER tc AFTER UPDATE OF c ON t1 BEGIN INSERT INTO log VALUES('of-c'); END`,
				`CREATE TRIGGER tbb AFTER UPDATE OF b ON t1 BEGIN INSERT INTO log VALUES('of-b'); END`,
				`INSERT INTO t1(a,b) VALUES(1,2)`,
			} {
				if err := db.Exec(s); err != nil {
					t.Fatalf("setup %q: %v", s, err)
				}
			}
			if err := db.Exec(tc.stmt); err != nil {
				t.Fatalf("%q: %v", tc.stmt, err)
			}
			got := rvdRowStrings(rvdQuery(t, db, `SELECT m FROM log ORDER BY rowid`))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("%q fired %v, want %v -- an UPDATE trigger declared OF a column the SET "+
					"list does not assign must not fire (checkColumnOverlap, trigger.c:1502)",
					tc.stmt, got, tc.want)
			}
		})
	}
}
