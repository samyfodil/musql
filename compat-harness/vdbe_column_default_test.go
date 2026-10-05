// This file gates DEFAULT column value handling. Defaults are affinity-coerced
// and evaluated before NOT NULL/CHECK. Time and random functions are declined.
package compat

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// defaultValueScript tests affinity coercion of defaults by wrapping columns in typeof().
var defaultValueScript = []struct {
	stmts []string
	sel   string
}{
	// Every literal spelling term by term, including the two signed forms
	// and both boolean keywords.
	{[]string{
		`CREATE TABLE lit(k INTEGER PRIMARY KEY, i DEFAULT 5, neg DEFAULT -7, pos DEFAULT +8,
			r DEFAULT 1.5, s DEFAULT 'abc', b DEFAULT x'41', n DEFAULT NULL,
			tt DEFAULT TRUE, ff DEFAULT FALSE)`,
		`INSERT INTO lit(k) VALUES(1)`,
	}, `SELECT typeof(i),i,typeof(neg),neg,typeof(pos),pos,typeof(r),r,typeof(s),s,
			typeof(b),b,typeof(n),n,typeof(tt),tt,typeof(ff),ff FROM lit ORDER BY k`},

	// Affinity is applied to the default, exactly like to an inserted value:
	// TEXT DEFAULT 5 -> '5', INT DEFAULT '9' -> 9, REAL DEFAULT 3 -> 3.0,
	// BLOB (no affinity) DEFAULT 'q' -> the TEXT 'q', NUMERIC '7' -> 7.
	{[]string{
		`CREATE TABLE aff(k INTEGER PRIMARY KEY, t TEXT DEFAULT 5, i INT DEFAULT '9',
			r REAL DEFAULT 3, bl BLOB DEFAULT 'q', num NUMERIC DEFAULT '7')`,
		`INSERT INTO aff(k) VALUES(1)`,
	}, `SELECT typeof(t),t,typeof(i),i,typeof(r),r,typeof(bl),bl,typeof(num),num FROM aff ORDER BY k`},

	// A "DEFAULT id" is a TEXT literal of the identifier, in every spelling
	// SQLite's id nonterminal accepts -- and an INTEGER column's affinity
	// does not coerce it away, since 'abc' is not a number.
	{[]string{
		`CREATE TABLE ident(k INTEGER PRIMARY KEY, a DEFAULT abc, b DEFAULT [xy], c DEFAULT "NULL",
			d INTEGER DEFAULT abc)`,
		`INSERT INTO ident(k) VALUES(1)`,
	}, `SELECT typeof(a),a,typeof(b),b,typeof(c),c,typeof(d),d FROM ident ORDER BY k`},

	// Parenthesized constant expressions: arithmetic, concatenation, a
	// function call, CASE, a nested paren, a blob, a CAST, and 1/0 (NULL,
	// not an error).
	{[]string{
		`CREATE TABLE paren(k INTEGER PRIMARY KEY, a DEFAULT (1+2), b DEFAULT ('a'||'b'),
			c DEFAULT (abs(-3)), d DEFAULT (CASE WHEN 1 THEN 2 ELSE 3 END), e DEFAULT ((1)),
			f DEFAULT (x'41'), g DEFAULT (1/0), h DEFAULT (- 1), i DEFAULT (cast('5' AS INTEGER)))`,
		`INSERT INTO paren(k) VALUES(1)`,
	}, `SELECT typeof(a),a,typeof(b),b,typeof(c),c,typeof(d),d,typeof(e),e,
			typeof(f),f,typeof(g),g,typeof(h),h,typeof(i),i FROM paren ORDER BY k`},

	// Numeric literal edges. 9223372036854775808 overflows int64 and becomes
	// a REAL; the NEGATIVE form of the same magnitude is the exact INTEGER
	// math.MinInt64 (one grammar-level token sequence in SQLite). Hex, a
	// leading zero, an exponent and a bare ".5" round out the spellings.
	{[]string{
		`CREATE TABLE nums(k INTEGER PRIMARY KEY, big DEFAULT 9223372036854775808,
			negbig DEFAULT -9223372036854775808, hex DEFAULT 0x10, lead DEFAULT 07,
			exp DEFAULT 1e3, frac DEFAULT .5, negfrac DEFAULT -.5, empty INTEGER DEFAULT '')`,
		`INSERT INTO nums(k) VALUES(1)`,
	}, `SELECT typeof(big),big,typeof(negbig),negbig,typeof(hex),hex,typeof(lead),lead,
			typeof(exp),exp,typeof(frac),frac,typeof(negfrac),negfrac,typeof(empty),empty
			FROM nums ORDER BY k`},

	// Unary +/- over a non-numeric term: "+" is a pure no-op that does not
	// even coerce ('5' stays TEXT), while "-" numerifies first (-'abc' and
	// -x'41' are both the INTEGER 0, -NULL is NULL).
	{[]string{
		`CREATE TABLE signs(k INTEGER PRIMARY KEY, a DEFAULT +'5', b DEFAULT -'abc',
			c DEFAULT -NULL, d DEFAULT -x'41')`,
		`INSERT INTO signs(k) VALUES(1)`,
	}, `SELECT typeof(a),a,typeof(b),b,typeof(c),c,typeof(d),d FROM signs ORDER BY k`},

	// "INSERT INTO t DEFAULT VALUES": every column takes its default, and a
	// column without one takes NULL.
	{[]string{
		`CREATE TABLE dv(k INTEGER PRIMARY KEY, a DEFAULT 1, b DEFAULT 'x', c)`,
		`INSERT INTO dv DEFAULT VALUES`,
	}, `SELECT typeof(a),a,typeof(b),b,typeof(c),c FROM dv ORDER BY k`},

	// A DEFAULT on the INTEGER PRIMARY KEY column is IGNORED: the rowid is
	// auto-assigned as if the clause were not there at all.
	{[]string{
		`CREATE TABLE ipk(a INTEGER PRIMARY KEY DEFAULT 99, b DEFAULT 7)`,
		`INSERT INTO ipk(b) VALUES(1)`,
		`INSERT INTO ipk DEFAULT VALUES`,
	}, `SELECT typeof(a),a,b FROM ipk ORDER BY a`},

	// The DEFAULT clause's own tokens are not column constraints: a
	// "NOT NULL" inside a parenthesized default must not make the COLUMN
	// NOT NULL, and a COLLATE inside one must not become the column's
	// collation (so 'x' does NOT match 'X' here).
	{[]string{
		`CREATE TABLE span(k INTEGER PRIMARY KEY, a DEFAULT (nullif(1,2) IS NOT NULL),
			b TEXT DEFAULT ('x' COLLATE NOCASE))`,
		`INSERT INTO span(k) VALUES(1)`,
		`INSERT INTO span(k,a,b) VALUES(2,NULL,NULL)`,
	}, `SELECT typeof(a),a,typeof(b),b,(b='X'),(b IS NULL) FROM span ORDER BY k`},

	// NOT NULL is enforced against the DEFAULTED value, so a NOT NULL column
	// with a non-NULL default inserts fine when omitted.
	{[]string{
		`CREATE TABLE nn(k INTEGER PRIMARY KEY, a NOT NULL DEFAULT 3, b TEXT NOT NULL DEFAULT 9)`,
		`INSERT INTO nn(k) VALUES(1)`,
	}, `SELECT typeof(a),a,typeof(b),b FROM nn ORDER BY k`},

	// A STRICT table declines VDBE compilation outright (its per-row
	// datatype check has no opcode -- see compileInsertStmt), so this is the
	// same rule going through the OTHER row builder: affinity still converts
	// the default first, and only then is the RESULT type-checked, which is
	// why 5 lands in a TEXT column as '5' rather than being rejected.
	{[]string{
		`CREATE TABLE strict(k INTEGER PRIMARY KEY, i INT DEFAULT '9', t TEXT DEFAULT 5,
			r REAL DEFAULT 3, b BLOB DEFAULT x'41', any1 ANY DEFAULT 'abc') STRICT`,
		`INSERT INTO strict(k) VALUES(1)`,
		`INSERT INTO strict DEFAULT VALUES`,
	}, `SELECT k,typeof(i),i,typeof(t),t,typeof(r),r,typeof(b),b,typeof(any1),any1
			FROM strict ORDER BY k`},

	// A WITHOUT ROWID table's defaults work identically.
	{[]string{
		`CREATE TABLE wr(a TEXT PRIMARY KEY, b DEFAULT 9) WITHOUT ROWID`,
		`INSERT INTO wr(a) VALUES('x')`,
	}, `SELECT a,typeof(b),b FROM wr ORDER BY a`},

	// An INSERT ... SELECT source omitting the column defaults it too.
	{[]string{
		`CREATE TABLE fromsel(k INTEGER PRIMARY KEY, v DEFAULT 'dv')`,
		`INSERT INTO fromsel(k) SELECT 1`,
	}, `SELECT typeof(v),v FROM fromsel ORDER BY k`},

	// REPLACE INTO omitting the column re-defaults it (the replaced row's
	// value is NOT carried over).
	{[]string{
		`CREATE TABLE rp(k INTEGER PRIMARY KEY, v DEFAULT 9)`,
		`INSERT INTO rp VALUES(1,5)`,
		`REPLACE INTO rp(k) VALUES(1)`,
	}, `SELECT typeof(v),v FROM rp ORDER BY k`},

	// An UPSERT's "excluded" row carries the default for the omitted column.
	{[]string{
		`CREATE TABLE up(k INTEGER PRIMARY KEY, v DEFAULT 9)`,
		`INSERT INTO up VALUES(1,1)`,
		`INSERT INTO up(k) VALUES(1) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
	}, `SELECT typeof(v),v FROM up ORDER BY k`},

	// An index over the defaulted column is maintained with the default.
	{[]string{
		`CREATE TABLE ix(k INTEGER PRIMARY KEY, v DEFAULT 3)`,
		`CREATE INDEX ixv ON ix(v)`,
		`INSERT INTO ix(k) VALUES(1)`,
	}, `SELECT k FROM ix WHERE v=3 ORDER BY k`},

	// A trigger body's INSERT applies the target's defaults too.
	{[]string{
		`CREATE TABLE trg(k INTEGER PRIMARY KEY, v DEFAULT 'dflt')`,
		`CREATE TABLE src(x)`,
		`CREATE TRIGGER tr AFTER INSERT ON src BEGIN INSERT INTO trg(k) VALUES(new.x); END`,
		`INSERT INTO src VALUES(5)`,
	}, `SELECT k,typeof(v),v FROM trg ORDER BY k`},

	// A multi-row VALUES list defaults every row.
	{[]string{
		`CREATE TABLE multi(k INTEGER PRIMARY KEY, v DEFAULT 'd')`,
		`INSERT INTO multi(k) VALUES(1),(2),(3)`,
	}, `SELECT k,typeof(v),v FROM multi ORDER BY k`},

	// A DEFAULT plus an explicitly-supplied value for the SAME column: the
	// supplied one always wins, including an explicit NULL.
	{[]string{
		`CREATE TABLE explicit(k INTEGER PRIMARY KEY, v DEFAULT 'd')`,
		`INSERT INTO explicit(k,v) VALUES(1,'given')`,
		`INSERT INTO explicit(k,v) VALUES(2,NULL)`,
		`INSERT INTO explicit(k) VALUES(3)`,
	}, `SELECT k,typeof(v),v FROM explicit ORDER BY k`},

	// ALTER TABLE ADD COLUMN's literal default backfills existing rows AND
	// applies to later INSERTs that omit the new column.
	{[]string{
		`CREATE TABLE alt(a)`,
		`INSERT INTO alt VALUES(1)`,
		`ALTER TABLE alt ADD COLUMN b DEFAULT 5`,
		`ALTER TABLE alt ADD COLUMN c DEFAULT x'41'`,
		`ALTER TABLE alt ADD COLUMN d TEXT DEFAULT -3`,
		`INSERT INTO alt(a) VALUES(2)`,
	}, `SELECT a,typeof(b),b,typeof(c),c,typeof(d),d FROM alt ORDER BY a`},
}

func TestColumnDefaultValuesMatchCSQLite(t *testing.T) {
	pureDB, mattnDB, _, _ := openPair(t, "coldefault")
	for _, step := range defaultValueScript {
		for _, stmt := range step.stmts {
			if _, err := pureDB.Exec(stmt); err != nil {
				t.Fatalf("pure Exec(%s): %v", stmt, err)
			}
			if _, err := mattnDB.Exec(stmt); err != nil {
				t.Fatalf("mattn Exec(%s): %v", stmt, err)
			}
		}
		queryBoth(t, pureDB, mattnDB, "default-values", step.sel)
	}
}

// TestColumnDefaultErrorParity pins the cases where the DEFAULT itself makes
// the statement FAIL, with C SQLite's own exact wording: the default is
// materialized before NOT NULL, CHECK and STRICT's type check all run, so
// each of them judges it.
func TestColumnDefaultErrorParity(t *testing.T) {
	db, err := engine.Create(filepath.Join(t.TempDir(), "err.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)

	for _, stmt := range []string{
		// A NOT NULL column whose DEFAULT is NULL still fails when omitted.
		`CREATE TABLE nn(k INTEGER PRIMARY KEY, a NOT NULL DEFAULT 3, b NOT NULL DEFAULT NULL)`,
		`INSERT INTO nn(k) VALUES(1)`,
		// A CHECK sees the defaulted value.
		`CREATE TABLE ck(k INTEGER PRIMARY KEY, v DEFAULT 5 CHECK(v>10))`,
		`INSERT INTO ck(k) VALUES(1)`,
		// STRICT type-checks the default AFTER affinity: 'abc' does not
		// become an INT, but 5 does become the TEXT '5' and passes.
		`CREATE TABLE st(k INTEGER PRIMARY KEY, a INT DEFAULT 'abc', b TEXT DEFAULT 5) STRICT`,
		`INSERT INTO st(k) VALUES(1)`,
		`INSERT INTO st(k,a) VALUES(2,1)`,
		`CREATE TABLE st2(k INTEGER PRIMARY KEY, a INT DEFAULT 1.5) STRICT`,
		`INSERT INTO st2(k) VALUES(1)`,
		// A generated column may not carry a DEFAULT; SQLite's wording
		// depends on which of the two clauses it read first.
		`CREATE TABLE g1(a, b AS (a+1) DEFAULT 5)`,
		`CREATE TABLE g2(a, b DEFAULT 5 AS (a+1))`,
		// A DEFAULT may reference neither a column nor a bound parameter.
		`CREATE TABLE d1(x, y DEFAULT (x))`,
		`CREATE TABLE d2(x, y DEFAULT (nosuchcol))`,
		`CREATE TABLE d3(x, y DEFAULT (?))`,
	} {
		execPlainBoth(t, db, sdb, stmt)
	}
}

// TestColumnDefaultDeclinedShapes pins the shapes this write path
// deliberately refuses rather than approximates. C SQLite ACCEPTS every
// one of them; the value it stores is the wall clock or the RNG, which no
// separately-running replay could ever match -- and this engine folds a
// default once, at CREATE TABLE time, so such a value would additionally
// change across a reopen. The CREATE therefore still succeeds (exactly like
// C SQLite's) and only the INSERT that would have had to invent a value
// declines, so the failure is always an honest error and never a wrong row.
func TestColumnDefaultDeclinedShapes(t *testing.T) {
	db, err := engine.Create(filepath.Join(t.TempDir(), "declined.sqlite"))
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()

	// The CLOCK-reading defaults are no longer here: they are evaluated per
	// INSERT now, which is what C SQLite does, and gated cell-for-cell in
	// compat-harness/column_default_expr_test.go. What remains is the half
	// that must STAY declined -- an output that can never match an
	// independently-seeded oracle, and a function this engine cannot run at
	// all.
	for i, decl := range []string{
		`a DEFAULT (random())`,
		`a DEFAULT (randomblob(4))`,
		`a DEFAULT (abs(random()))`,
		// C SQLite defers an unknown function to INSERT time too
		// ("unknown function: nosuchfunc()"), so declining the INSERT --
		// not the CREATE -- is the matching shape of failure.
		`a DEFAULT (nosuchfunc(1))`,
	} {
		name := "decl" + string(rune('a'+i))
		if err := db.Exec(`CREATE TABLE ` + name + `(k INTEGER PRIMARY KEY, ` + decl + `)`); err != nil {
			t.Fatalf("CREATE TABLE %s(%s): %v", name, decl, err)
		}
		if err := db.Exec(`INSERT INTO ` + name + `(k) VALUES(1)`); err == nil {
			t.Errorf("[%s] INSERT omitting the column was ACCEPTED; this default is not reproducible and must decline", decl)
		}
		// Supplying the column explicitly never involves the default and
		// must keep working.
		if err := db.Exec(`INSERT INTO ` + name + `(k,a) VALUES(2,'x')`); err != nil {
			t.Errorf("[%s] INSERT supplying the column was rejected: %v", decl, err)
		}
	}
}

// TestColumnDefaultSurvivesReopen is the persistence half: a DEFAULT is
// recovered by re-parsing the table's stored CREATE TABLE text
// (engine/writer_open.go), never from any side table, so a database written
// by one session must default identically in the next one -- and real C
// SQLite must read the resulting file back with the same rows.
func TestColumnDefaultSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE t(k INTEGER PRIMARY KEY, a DEFAULT 5, b TEXT DEFAULT 7, c DEFAULT (2*3), d DEFAULT x'41')`,
		`INSERT INTO t(k) VALUES(1)`,
		`CREATE TABLE u(a)`,
		`INSERT INTO u VALUES(1)`,
		`ALTER TABLE u ADD COLUMN b DEFAULT -3`,
	} {
		if err := db.Exec(stmt); err != nil {
			t.Fatalf("Exec(%s): %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO t(k) VALUES(2)`,
		`INSERT INTO u(a) VALUES(2)`,
	} {
		if err := db2.Exec(stmt); err != nil {
			t.Fatalf("post-reopen Exec(%s): %v", stmt, err)
		}
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close2: %v", err)
	}

	// C SQLite reads the file back: the two rows written by the two
	// separate sessions must be byte-identical, and the file must verify.
	vdb, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatalf("sql.Open final: %v", err)
	}
	defer vdb.Close()
	var ic string
	if err := vdb.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if ic != "ok" {
		t.Fatalf("integrity_check = %q, want ok", ic)
	}
	var same int
	if err := vdb.QueryRow(`SELECT count(*) FROM t x JOIN t y ON x.k=1 AND y.k=2
		WHERE x.a IS y.a AND x.b IS y.b AND x.c IS y.c AND x.d IS y.d`).Scan(&same); err != nil {
		t.Fatalf("cross-session compare: %v", err)
	}
	if same != 1 {
		t.Fatalf("the row written after reopen differs from the one written before it")
	}
	var typA, typB, typC, typD string
	var a, b any
	var c int64
	var d []byte
	if err := vdb.QueryRow(`SELECT typeof(a),a,typeof(b),b,typeof(c),c,typeof(d),d FROM t WHERE k=2`).
		Scan(&typA, &a, &typB, &b, &typC, &c, &typD, &d); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if typA != "integer" || typB != "text" || typC != "integer" || c != 6 || typD != "blob" || string(d) != "A" {
		t.Fatalf("post-reopen defaults wrong: a=%s/%v b=%s/%v c=%s/%d d=%s/%q", typA, a, typB, b, typC, c, typD, d)
	}
	var ub int64
	var typUB string
	if err := vdb.QueryRow(`SELECT typeof(b),b FROM u WHERE a=2`).Scan(&typUB, &ub); err != nil {
		t.Fatalf("read back u: %v", err)
	}
	if typUB != "integer" || ub != -3 {
		t.Fatalf("post-reopen ADD COLUMN default wrong: %s/%d", typUB, ub)
	}
}
