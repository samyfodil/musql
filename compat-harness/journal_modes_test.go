// This file gates rollback journal modes (delete, truncate, persist, memory, off).
// The modes differ only in the post-commit journal file state, verified by comparing
// both SQL results and on-disk artifacts between C SQLite and musql.
package compat

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/samyfodil/musql/engine"
)

// jmProgram is a statement list that (a) puts the connection in mode, (b)
// creates a database, and (c) commits several separate transactions over an
// EXISTING file -- autocommit INSERTs plus one explicit transaction, so the
// journal is written and finalized more than once. The trailing reads are what
// differ()-style comparison checks.
func jmProgram(mode string) []string {
	return []string{
		`PRAGMA journal_mode=` + mode,
		`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t VALUES(1,'one')`,
		`INSERT INTO t VALUES(2,'two')`,
		`BEGIN`,
		`INSERT INTO t VALUES(3,'three')`,
		`UPDATE t SET v='ONE' WHERE id=1`,
		`COMMIT`,
		`BEGIN`,
		`INSERT INTO t VALUES(4,'four')`,
		`ROLLBACK`,
		`DELETE FROM t WHERE id=2`,
		`PRAGMA journal_mode`,
		`PRAGMA integrity_check`,
		`SELECT id,v FROM t ORDER BY id`,
	}
}

// jmArtifact is the state of "<db>-journal" after a commit: whether the file is
// there, how long it is, and whether its first journalHeaderSize bytes are all
// zero (which is what stops either engine from treating it as HOT).
type jmArtifact struct {
	exists     bool
	size       int64
	headerZero bool
}

// jmJournalHeaderSize mirrors engine/recovery.go's journalHeaderSize: the magic
// plus five u32s. PERSIST mode zeroes exactly these bytes (SQLite's
// zeroJournalHdr writes a 28-byte zeroHdr).
const jmJournalHeaderSize = 28

func jmStat(t *testing.T, dbPath string) jmArtifact {
	t.Helper()
	fi, err := os.Stat(dbPath + "-journal")
	if err != nil {
		if os.IsNotExist(err) {
			return jmArtifact{}
		}
		t.Fatalf("stat %s-journal: %v", dbPath, err)
	}
	a := jmArtifact{exists: true, size: fi.Size()}
	if fi.Size() >= jmJournalHeaderSize {
		b, rerr := os.ReadFile(dbPath + "-journal")
		if rerr != nil {
			t.Fatalf("read %s-journal: %v", dbPath, rerr)
		}
		a.headerZero = true
		for _, c := range b[:jmJournalHeaderSize] {
			if c != 0 {
				a.headerZero = false
				break
			}
		}
	}
	return a
}

func (a jmArtifact) String() string {
	if !a.exists {
		return "absent"
	}
	return fmt.Sprintf("present size=%d headerZero=%v", a.size, a.headerZero)
}

// jmWantArtifact checks the post-commit journal state each mode must leave.
// For C SQLite: delete and memory leave no file; truncate leaves an empty file;
// persist leaves a file with the header zeroed. For musql: no journal file in any mode.
func jmWantArtifact(t *testing.T, eng, mode string, got jmArtifact) {
	t.Helper()
	if eng == "musql" {
		if got.exists {
			t.Errorf("journal_mode=%s: musql left a journal file (%s); this format keeps none", mode, got)
		}
		return
	}
	switch mode {
	case "delete", "memory":
		if got.exists {
			t.Errorf("journal_mode=%s: journal is %s after commit, want absent", mode, got)
		}
	case "truncate":
		if !got.exists || got.size != 0 {
			t.Errorf("journal_mode=%s: journal is %s after commit, want present with size 0", mode, got)
		}
	case "persist":
		if !got.exists || got.size == 0 {
			t.Errorf("journal_mode=%s: journal is %s after commit, want present and non-empty", mode, got)
		} else if !got.headerZero {
			t.Errorf("journal_mode=%s: journal is %s after commit, want its first %d bytes zeroed (an un-zeroed header reads as HOT and would be rolled back on the next open)", mode, got, jmJournalHeaderSize)
		}
	case "off":
		// "off" writes no journal file at all, same shape as memory -- see
		// engine/journal.go's package comment.
		if got.exists {
			t.Errorf("journal_mode=off: journal is %s after commit, want absent", got)
		}
	default:
		t.Fatalf("unknown mode %q", mode)
	}
}

// jmAcceptedModes are the five rollback modes this engine implements.
var jmAcceptedModes = []string{"delete", "truncate", "persist", "memory", "off"}

// TestJournalModeSQLAndArtifactMatch is the core gate: for each accepted mode,
// both engines must agree on every SQL result AND leave the same journal
// artifact on disk.
func TestJournalModeSQLAndArtifactMatch(t *testing.T) {
	for _, mode := range jmAcceptedModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			stmts := jmProgram(mode)
			results := map[string][]map[string]any{}
			arts := map[string]jmArtifact{}
			for _, eng := range engineOrder {
				dsn := filepath.Join(t.TempDir(), eng+".db")
				results[eng] = runWithDSN(t, eng, dsn, stmts)
				arts[eng] = jmStat(t, dsn)
			}
			// The oracle's own artifact is the specification; assert it matches
			// what jmWantArtifact documents, so a change in the oracle build
			// surfaces here rather than silently rewriting the expectation.
			jmWantArtifact(t, "cgo", mode, arts["cgo"])
			jmWantArtifact(t, "musql", mode, arts["musql"])
			for i := range results["cgo"] {
				c := fmt.Sprint(results["cgo"][i])
				g := fmt.Sprint(results["musql"][i])
				if c != g {
					t.Errorf("journal_mode=%s: stmt #%d %q DIVERGES\n  cgo:    %s\n  musql: %s", mode, i, stmts[i], c, g)
				}
			}
			t.Logf("journal_mode=%s: cgo journal %s | musql journal %s", mode, arts["cgo"], arts["musql"])
		})
	}
}

// TestJournalModeSetterAndGetter checks that the pragma returns the mode in effect
// after the statement, lower-cased, silently ignoring unrecognized names.
func TestJournalModeSetterAndGetter(t *testing.T) {
	for _, from := range jmAcceptedModes {
		for _, to := range jmAcceptedModes {
			from, to := from, to
			t.Run(from+"-to-"+to, func(t *testing.T) {
				differ(t, "journal-mode-"+from+"-to-"+to, []string{
					`PRAGMA journal_mode=` + from,
					`CREATE TABLE t(a)`,
					`INSERT INTO t VALUES(1)`,
					`PRAGMA journal_mode`,
					`PRAGMA journal_mode=` + to,
					`PRAGMA journal_mode`,
					`PRAGMA main.journal_mode`,
					// An unrecognized name is ignored, not an error, and the
					// CURRENT mode comes back.
					`PRAGMA journal_mode=persistent`,
					`PRAGMA journal_mode=xxx`,
					// Case is folded on the way out.
					`PRAGMA journal_mode=` + upper(to),
					`INSERT INTO t VALUES(2)`,
					`SELECT count(*) FROM t`,
				})
			})
		}
	}
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}

// TestJournalModeIsNotPersistedInTheFile pins where the mode LIVES. A rollback
// mode is CONNECTION state that SQLite writes nowhere: verified directly, a
// second connection to a file the first put in truncate/persist/memory mode
// reads back "delete", and the header's (read, write) version pair stays (1,1).
// Only WAL persists, as version (2,2). Getting this backwards -- stamping the
// mode into the header -- would make a musql database announce a mode to C
// SQLite that C SQLite never announces back.
func TestJournalModeIsNotPersistedInTheFile(t *testing.T) {
	for _, mode := range jmAcceptedModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			for _, eng := range engineOrder {
				dsn := filepath.Join(t.TempDir(), eng+".db")
				runWithDSN(t, eng, dsn, []string{
					`PRAGMA journal_mode=` + mode,
					`CREATE TABLE t(a)`,
					`INSERT INTO t VALUES(1)`,
				})
				// A brand-new worker process == a brand-new connection.
				got := runWithDSN(t, eng, dsn, []string{`PRAGMA journal_mode`})
				assertOneRowCell(t, eng+": journal_mode on a NEW connection (the mode is per-connection, not stored in the file)", got[0], "T:delete")
				hdr := jmHeaderVersions(t, dsn)
				if hdr != [2]byte{1, 1} {
					t.Errorf("%s: header (read,write) version = %v after journal_mode=%s, want (1,1)", eng, hdr, mode)
				}
			}
		})
	}
}

// jmHeaderVersions reads a database's header version bytes 18/19 -- for a musql
// database, of its EXPORT, which is where this format's journal mode meets C's.
func jmHeaderVersions(t *testing.T, path string) [2]byte {
	path = exportedForOracle(t, path)
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	b := make([]byte, 20)
	if _, err := f.ReadAt(b, 0); err != nil {
		t.Fatalf("read header of %s: %v", path, err)
	}
	return [2]byte{b[18], b[19]}
}

// TestJournalArtifactIsNotHotForTheOtherEngine checks that a finalized journal
// file left by one engine is not mistaken for a hot journal by the other.
func TestJournalArtifactIsNotHotForTheOtherEngine(t *testing.T) {
	for _, mode := range []string{"truncate", "persist"} {
		for _, writer := range engineOrder {
			mode, writer := mode, writer
			t.Run(mode+"/written-by-"+writer, func(t *testing.T) {
				dsn := filepath.Join(t.TempDir(), "jm.db")
				runWithDSN(t, writer, dsn, jmProgram(mode))
				art := jmStat(t, dsn)
				if writer == "cgo" && !art.exists {
					t.Fatalf("%s left no journal artifact in %s mode", writer, mode)
				}
				jmWantArtifact(t, writer, mode, art)
				// jmProgram commits (1,'ONE') and (3,'three') and deletes id 2.
				const want = "[[I:1 T:ONE] [I:3 T:three]]"
				for _, reader := range engineOrder {
					// The other engine's file through the converter: C's leftover
					// journal must not read as hot to the IMPORT either.
					got := runWithDSN(t, reader, pathForReader(t, reader, dsn), []string{
						`PRAGMA integrity_check`,
						`SELECT id,v FROM t ORDER BY id`,
					})
					assertOneRowCell(t, fmt.Sprintf("%s reading a %s-mode database written by %s: integrity_check", reader, mode, writer), got[0], "T:ok")
					if rows, _ := got[1]["rows"].([]any); fmt.Sprint(rows) != want {
						t.Errorf("%s reading a %s-mode database written by %s: rows = %v, want %s -- the leftover journal (%s) was treated as HOT and rolled the commit back", reader, mode, writer, rows, want, art)
					}
				}
			})
		}
	}
}

// TestJournalModeOffUndoSemantics checks the two behaviors where "off" differs:
// ROLLBACK TO a savepoint undoes nothing, and partial statement failures keep their rows.
func TestJournalModeOffUndoSemantics(t *testing.T) {
	// (1) The oracle: ROLLBACK TO a savepoint undoes NOTHING under off.
	t.Run("oracle-savepoint-rollback-does-nothing", func(t *testing.T) {
		for _, tc := range []struct{ mode, wantTables, wantRows string }{
			{"delete", "keep", "1"},
			{"memory", "keep", "1"},
			{"off", "gone,keep", "1,2"},
		} {
			path := filepath.Join(t.TempDir(), "off.db")
			db := jmOpenCGO(t, path)
			jmExec(t, db, `PRAGMA journal_mode=`+tc.mode)
			jmExec(t, db, `CREATE TABLE keep(x)`)
			jmExec(t, db, `INSERT INTO keep VALUES(1)`)
			jmExec(t, db, `SAVEPOINT s`)
			jmExec(t, db, `CREATE TABLE gone(y)`)
			jmExec(t, db, `INSERT INTO keep VALUES(2)`)
			jmExec(t, db, `ROLLBACK TO s`)
			if got := jmScalar(t, db, `SELECT group_concat(name) FROM (SELECT name FROM sqlite_master ORDER BY name)`); got != tc.wantTables {
				t.Errorf("journal_mode=%s: tables after ROLLBACK TO = %q, want %q", tc.mode, got, tc.wantTables)
			}
			if got := jmScalar(t, db, `SELECT group_concat(x) FROM (SELECT x FROM keep ORDER BY x)`); got != tc.wantRows {
				t.Errorf("journal_mode=%s: keep after ROLLBACK TO = %q, want %q", tc.mode, got, tc.wantRows)
			}
			db.Close()
		}
	})

	// (2) The oracle: a statement that fails part way through INSIDE an explicit
	// transaction keeps the rows it already wrote under off (no sub-journal).
	t.Run("oracle-statement-rollback-does-nothing-in-a-txn", func(t *testing.T) {
		for _, tc := range []struct {
			mode string
			want string
		}{
			{"delete", "1"},
			{"memory", "1"},
			{"off", "500"},
		} {
			path := filepath.Join(t.TempDir(), "off.db")
			db := jmOpenCGO(t, path)
			jmExec(t, db, `PRAGMA journal_mode=`+tc.mode)
			jmExec(t, db, `CREATE TABLE src(a INTEGER)`)
			jmExec(t, db, `CREATE TABLE t(a INTEGER PRIMARY KEY)`)
			jmExec(t, db, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<600) INSERT INTO src SELECT n FROM c`)
			jmExec(t, db, `INSERT INTO t VALUES(500)`)
			jmExec(t, db, `BEGIN`)
			if _, err := db.Exec(`INSERT INTO t SELECT a FROM src`); err == nil {
				t.Fatalf("journal_mode=%s: the INSERT..SELECT was expected to fail on the UNIQUE at row 500", tc.mode)
			}
			if got := jmScalar(t, db, `SELECT count(*) FROM t`); got != tc.want {
				t.Errorf("journal_mode=%s: count after the failed statement = %q, want %q", tc.mode, got, tc.want)
			}
			jmExec(t, db, `ROLLBACK`)
			db.Close()
		}
	})

	// (3) musql now reproduces (1) end-to-end, both engines compared live.
	t.Run("differ-savepoint-rollback-does-nothing", func(t *testing.T) {
		differ(t, "journal-off-savepoint-noop", []string{
			`PRAGMA journal_mode=off`,
			`CREATE TABLE keep(x)`,
			`INSERT INTO keep VALUES(1)`,
			`BEGIN`,
			`SAVEPOINT s`,
			`CREATE TABLE gone(y)`,
			`INSERT INTO keep VALUES(2)`,
			`ROLLBACK TO s`,
			`SELECT name FROM sqlite_master ORDER BY name`,
			`SELECT x FROM keep ORDER BY x`,
			// A full ROLLBACK, unlike ROLLBACK TO, still undoes everything --
			// including what the no-op ROLLBACK TO above left behind.
			`ROLLBACK`,
			`SELECT name FROM sqlite_master ORDER BY name`,
			`SELECT x FROM keep ORDER BY x`,
			`PRAGMA integrity_check`,
		})
	})

	// The second behavior is not tested end-to-end here; see the engine-direct
	// test in journal_mode_off_test.go instead.
}

func jmOpenCGO(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", exportedForOracle(t, path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // one connection: journal_mode is per-connection state
	return db
}

func jmExec(t *testing.T, db *sql.DB, sqlText string) {
	t.Helper()
	if _, err := db.Exec(sqlText); err != nil {
		t.Fatalf("cgo %q: %v", sqlText, err)
	}
}

func jmScalar(t *testing.T, db *sql.DB, sqlText string) string {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(sqlText).Scan(&v); err != nil {
		t.Fatalf("cgo %q: %v", sqlText, err)
	}
	return v.String
}

// TestJournalModeRollbackIsUnaffected checks that all rollback paths produce
// identical SQL results across all modes.
func TestJournalModeRollbackIsUnaffected(t *testing.T) {
	for _, mode := range jmAcceptedModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			differ(t, "journal-rollback-"+mode, []string{
				`PRAGMA journal_mode=` + mode,
				`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
				`INSERT INTO t VALUES(1,'one')`,
				`BEGIN`,
				`INSERT INTO t VALUES(2,'two')`,
				`UPDATE t SET b='CHANGED' WHERE a=1`,
				`ROLLBACK`,
				`SELECT a,b FROM t ORDER BY a`,
				// A DDL rollback.
				`BEGIN`,
				`CREATE TABLE gone(y)`,
				`ROLLBACK`,
				`SELECT name FROM sqlite_master ORDER BY name`,
				// A constraint failure in autocommit undoes the whole statement.
				`INSERT INTO t VALUES(9,'nine')`,
				`INSERT INTO t SELECT a+8, b FROM t`,
				`SELECT a,b FROM t ORDER BY a`,
				`PRAGMA integrity_check`,
				`PRAGMA journal_mode`,
			})
		})
	}
}

// TestJournalModeInsideACleanTransactionTakes checks that inside a clean transaction,
// a mode switch takes effect and the commit uses the new mode's journal artifact.
func TestJournalModeInsideACleanTransactionTakes(t *testing.T) {
	for _, from := range jmAcceptedModes {
		for _, to := range jmAcceptedModes {
			if from == to {
				continue
			}
			from, to := from, to
			t.Run(from+"-to-"+to, func(t *testing.T) {
				program := []string{
					`PRAGMA journal_mode=` + from,
					`CREATE TABLE t(a)`,
					`INSERT INTO t VALUES(1)`,
					`BEGIN`,
					`PRAGMA journal_mode=` + to, // #4: the pager is still clean
					`PRAGMA journal_mode`,       // #5: already `to`
					`INSERT INTO t VALUES(2)`,
					`COMMIT`,
					`PRAGMA journal_mode`, // #8: survives the commit
					`INSERT INTO t VALUES(3)`,
				}
				var baseline string
				for _, eng := range engineOrder {
					dsn := filepath.Join(t.TempDir(), "jm.db")
					got := runWithDSN(t, eng, dsn, program)
					assertOneRowCell(t, eng+": journal_mode inside the transaction", got[5], "T:"+to)
					assertOneRowCell(t, eng+": journal_mode after COMMIT", got[8], "T:"+to)
					// ...and the commit really finalizes the file the NEW mode's
					// way, which is the assertion a mode that only LOOKED
					// changed would not survive.
					jmWantArtifact(t, eng, to, jmStat(t, dsn))
					if baseline == "" {
						baseline = fmt.Sprint(got)
					} else if fmt.Sprint(got) != baseline {
						t.Errorf("engines differ\n  cgo:    %s\n  musql: %s", baseline, got)
					}
				}
			})
		}
	}
}

// TestJournalModeInsideADirtiedTransactionIsIgnored checks that inside a dirtied
// transaction, mode switches are ignored and the mode survives the commit.
func TestJournalModeInsideADirtiedTransactionIsIgnored(t *testing.T) {
	tos := append(append([]string{}, jmAcceptedModes...), "wal")
	for _, from := range jmAcceptedModes {
		for _, to := range tos {
			if from == to {
				continue
			}
			from, to := from, to
			t.Run(from+"-to-"+to, func(t *testing.T) {
				program := []string{
					`PRAGMA journal_mode=` + from,
					`CREATE TABLE t(a)`,
					`BEGIN`,
					`INSERT INTO t VALUES(1)`,   // dirties a page of main
					`PRAGMA journal_mode=` + to, // #4: silently ignored, reports `from`
					`PRAGMA journal_mode`,       // #5: still `from`
					`COMMIT`,
					`PRAGMA journal_mode`, // #7: still `from`
					`INSERT INTO t VALUES(2)`,
				}
				var baseline string
				for _, eng := range engineOrder {
					dsn := filepath.Join(t.TempDir(), "jm.db")
					got := runWithDSN(t, eng, dsn, program)
					assertOneRowCell(t, eng+": ignored setter's own report", got[4], "T:"+from)
					assertOneRowCell(t, eng+": journal_mode inside the dirtied transaction", got[5], "T:"+from)
					assertOneRowCell(t, eng+": journal_mode after COMMIT", got[7], "T:"+from)
					jmWantArtifact(t, eng, from, jmStat(t, dsn))
					if baseline == "" {
						baseline = fmt.Sprint(got)
					} else if fmt.Sprint(got) != baseline {
						t.Errorf("engines differ\n  cgo:    %s\n  musql: %s", baseline, got)
					}
				}
			})
		}
	}
}

// TestJournalModeDirtiedByDelete checks that DELETE also dirtied transactions ignore mode switches.
func TestJournalModeDirtiedByDelete(t *testing.T) {
	program := []string{
		`PRAGMA journal_mode=delete`,
		`CREATE TABLE t(a)`,
		`INSERT INTO t VALUES(1)`,
		`BEGIN`,
		`DELETE FROM t`,
		`PRAGMA journal_mode=truncate`, // ignored, reports delete
		`PRAGMA journal_mode`,
		`COMMIT`,
		`PRAGMA journal_mode`,
	}
	var baseline string
	for _, eng := range engineOrder {
		dsn := filepath.Join(t.TempDir(), "jm.db")
		got := runWithDSN(t, eng, dsn, program)
		assertOneRowCell(t, eng+": ignored setter's own report", got[5], "T:delete")
		assertOneRowCell(t, eng+": after COMMIT", got[8], "T:delete")
		if baseline == "" {
			baseline = fmt.Sprint(got)
		} else if fmt.Sprint(got) != baseline {
			t.Errorf("engines differ\n  cgo:    %s\n  musql: %s", baseline, got)
		}
	}
}

// TestJournalModeAttachedCleanPagerFollowsUnqualifiedSwitch checks that an
// unqualified mode switch in a dirtied-main transaction still applies to untouched
// attached pagers. Tests with C SQLite directly and against the engine.
func TestJournalModeAttachedCleanPagerFollowsUnqualifiedSwitch(t *testing.T) {
	dir := t.TempDir()
	auxPath := filepath.Join(dir, "aux.db")

	// Test C SQLite (oracle): main stays "truncate" (dirtied, ignored) while aux switches.
	cgodb, err := sql.Open("sqlite3", filepath.Join(dir, "cgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	cgodb.SetMaxOpenConns(1)
	defer cgodb.Close()
	cgoAuxPath := filepath.Join(dir, "cgo-aux.db")
	cgoSetup := []string{
		`ATTACH '` + cgoAuxPath + `' AS aux`,
		`CREATE TABLE main.u(a)`,
		`CREATE TABLE aux.v(a)`,
		`PRAGMA journal_mode=truncate`,
		`BEGIN`,
		`INSERT INTO u VALUES(1)`, // dirties main only
	}
	for _, s := range cgoSetup {
		jmExec(t, cgodb, s)
	}
	// Mode switch mid-transaction, main dirtied: reports main's unchanged mode.
	var reported string
	if err := cgodb.QueryRow(`PRAGMA journal_mode=persist`).Scan(&reported); err != nil {
		t.Fatalf("cgo journal_mode setter: %v", err)
	}
	if reported != "truncate" {
		t.Fatalf("oracle sanity check: unqualified setter reported %q inside the dirtied transaction, want truncate (main ignored)", reported)
	}
	jmExec(t, cgodb, `COMMIT`)
	if got := jmScalar(t, cgodb, `PRAGMA journal_mode`); got != "truncate" {
		t.Fatalf("oracle sanity check: main mode after COMMIT = %q, want truncate", got)
	}
	// aux was untouched, so the switch took; verify with a write.
	jmExec(t, cgodb, `INSERT INTO aux.v VALUES(2)`)
	if !jmStat(t, cgoAuxPath).exists {
		t.Fatalf("oracle sanity check: aux left no journal artifact after a persist-mode write -- aux's own switch did not take as expected")
	}

	// Test musql engine-direct.
	goMainPath := filepath.Join(dir, "go-main.db")
	godb, err := engine.Create(goMainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer godb.Discard()
	goProgram := []string{
		`ATTACH '` + auxPath + `' AS aux`,
		`CREATE TABLE main.u(a)`,
		`CREATE TABLE aux.v(a)`,
		`PRAGMA journal_mode=truncate`,
		`BEGIN`,
		`INSERT INTO u VALUES(1)`,
		`PRAGMA journal_mode=persist`,
		`COMMIT`,
		`INSERT INTO aux.v VALUES(2)`,
	}
	for _, s := range goProgram {
		if _, _, err := godb.ExecArgs(s, nil); err != nil {
			t.Fatalf("musql %q: %v", s, err)
		}
	}
	if got := godb.JournalMode(); got != "truncate" {
		t.Errorf("musql: main journal mode = %q, want truncate (dirtied, ignored)", got)
	}
	// Read the switch back from aux itself.
	if got := godb.AttachedJournalModeForTest("aux"); got != "persist" {
		t.Errorf("musql: aux's journal mode = %q, want persist -- the untouched attachment's own switch did not take", got)
	}
	if got := jmScalar(t, cgodb, `PRAGMA aux.journal_mode`); got != "persist" {
		t.Fatalf("oracle sanity check: PRAGMA aux.journal_mode = %q, want persist", got)
	}
}

// TestJournalModeWALTransitions checks that entering and leaving WAL works
// from and to each rollback mode, and WAL persists across reopens.
func TestJournalModeWALTransitions(t *testing.T) {
	for _, mode := range jmAcceptedModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			stmts := []string{
				`PRAGMA journal_mode=` + mode,
				`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
				`INSERT INTO t VALUES(1,'one')`,
				`PRAGMA journal_mode=wal`,
				`PRAGMA journal_mode`,
				`INSERT INTO t VALUES(2,'two')`,
				`PRAGMA journal_mode=` + mode,
				`PRAGMA journal_mode`,
				`INSERT INTO t VALUES(3,'three')`,
				`PRAGMA integrity_check`,
				`SELECT a,b FROM t ORDER BY a`,
			}
			for _, eng := range engineOrder {
				dsn := filepath.Join(t.TempDir(), eng+".db")
				runWithDSN(t, eng, dsn, stmts)
				// Back out of WAL: the side files are gone and the header says
				// "rollback journal" again.
				for _, side := range []string{"-wal", "-shm"} {
					if _, err := os.Stat(dsn + side); err == nil {
						t.Errorf("%s: %s survived the switch back to %s", eng, side, mode)
					}
				}
				if hdr := jmHeaderVersions(t, dsn); hdr != [2]byte{1, 1} {
					t.Errorf("%s: header (read,write) = %v after leaving WAL for %s, want (1,1)", eng, hdr, mode)
				}
				jmWantArtifact(t, eng, mode, jmStat(t, dsn))
			}
			differ(t, "journal-wal-roundtrip-"+mode, stmts)
		})
	}
}
