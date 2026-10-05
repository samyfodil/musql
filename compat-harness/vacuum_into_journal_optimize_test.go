// This file gates VACUUM INTO, PRAGMA journal_mode, and PRAGMA optimize
// by running against C SQLite and verifying both the results and internal state.
package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// vjKinds extracts the kind field from each statement result.
func vjKinds(res []map[string]any) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i], _ = r["kind"].(string)
	}
	return out
}

// vjCells renders the full result as JSON for comparison.
func vjCells(res []map[string]any) string {
	b, _ := json.Marshal(res)
	return string(b)
}

// vjRunBoth runs statements against both engines and returns the results.
func vjRunBoth(t *testing.T, stmts func(dir string) []string) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, eng := range engineOrder {
		dir := t.TempDir()
		out[eng] = runWithDSN(t, eng, filepath.Join(dir, "src.db"), stmts(dir))
	}
	return out
}

// ---- VACUUM INTO ----

// vjVacuumIntoPrograms tests VACUUM INTO with various table shapes.
var vjVacuumIntoPrograms = map[string][]string{
	"gappedRowids": {
		"CREATE TABLE r(a)",
		"INSERT INTO r VALUES('x'),('y'),('z'),('w')",
		"DELETE FROM r WHERE a IN ('y','z')",
	},
	"droppedTable": {
		"CREATE TABLE a(x)", "CREATE TABLE b(x)", "CREATE TABLE c(x)", "DROP TABLE b",
	},
	"indexed": {
		"CREATE TABLE t(a INTEGER PRIMARY KEY,b)",
		"CREATE INDEX ti ON t(b)",
		"INSERT INTO t VALUES(1,'aa'),(9,'bb'),(4,'cc')",
		"DELETE FROM t WHERE a=9",
	},
	"overflow": {
		"CREATE TABLE o(a)",
		"INSERT INTO o VALUES(hex(zeroblob(9000)))",
		"INSERT INTO o VALUES(hex(zeroblob(3000)))",
	},
	"withoutRowid": {
		"CREATE TABLE w(a TEXT PRIMARY KEY, b) WITHOUT ROWID",
		"INSERT INTO w VALUES('k1',1),('k2',2)",
	},
	"walSource": {
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE t1(a)", "INSERT INTO t1 VALUES(19)", "CREATE INDEX t1a ON t1(a)",
	},
	"utf16": {
		"PRAGMA encoding='UTF-16le'", "CREATE TABLE u(a)", "INSERT INTO u VALUES('hi')",
	},
	"headerScalars": {
		"CREATE TABLE h(a)", "INSERT INTO h VALUES(1)",
		"PRAGMA user_version=77", "PRAGMA application_id=1234",
	},
	"viewAndTrigger": {
		"CREATE TABLE t(a)",
		"CREATE VIEW v AS SELECT a FROM t",
		"CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET a=a; END",
		"INSERT INTO t VALUES(1)",
	},
}

// vjCopyReadback is what every reader is asked about a copy. It is deliberately
// the physical layout as well as the content: page_count and freelist_count are
// what make the copy a COMPACTION rather than a byte copy, schema_version is
// the one header field VACUUM INTO changes on purpose (source + 1), and
// integrity_check run by C SQLite over a file musql wrote is the claim
// that the copy is a well-formed SQLite database at all.
//
// The two COUNTS are asked only by C (vjCopyStorageReadback): musql reading its
// own copy answers with its own storage's number, which is not comparable (see
// storageOnlyColumns), while C over the export of that copy must match C over its
// own.
var vjCopyStorageReadback = []string{
	"PRAGMA page_count",
	"PRAGMA freelist_count",
}

var vjCopyReadback = []string{
	"PRAGMA page_size",
	"PRAGMA encoding",
	"PRAGMA schema_version",
	"PRAGMA user_version",
	"PRAGMA application_id",
	"PRAGMA journal_mode",
	"PRAGMA integrity_check",
	"SELECT type,name FROM sqlite_master ORDER BY type,name",
}

// TestVacuumIntoCopyIsWhatCSQLiteWrites verifies that both engines produce
// readable copies and cross-engine reading produces identical results.
func TestVacuumIntoCopyIsWhatCSQLiteWrites(t *testing.T) {
	for name, prog := range vjVacuumIntoPrograms {
		prog := prog
		t.Run(name, func(t *testing.T) {
			copies := map[string]string{}
			for _, writer := range engineOrder {
				dir := t.TempDir()
				cp := filepath.Join(dir, "copy.db")
				stmts := append(append([]string{}, prog...), "VACUUM INTO '"+cp+"'")
				res := runWithDSN(t, writer, filepath.Join(dir, "src.db"), stmts)
				if k := vjKinds(res); k[len(k)-1] != "ok" && k[len(k)-1] != "rows" {
					t.Fatalf("%s refused VACUUM INTO: %v", writer, k)
				}
				if _, err := os.Stat(cp); err != nil {
					t.Fatalf("%s wrote no copy: %v", writer, err)
				}
				copies[writer] = cp
			}
			// Every read answers vjCopyReadback; C's reads also answer the storage
			// counts, compared among themselves.
			baseline, baselineFrom := map[string]string{}, map[string]string{}
			check := func(key, from, got string) {
				if baseline[key] == "" {
					baseline[key], baselineFrom[key] = got, from
				} else if got != baseline[key] {
					t.Errorf("VACUUM INTO copies disagree\n  %s: %s\n  %s: %s", baselineFrom[key], baseline[key], from, got)
				}
			}
			for _, writer := range engineOrder {
				for _, reader := range engineOrder {
					path := pathForReader(t, reader, copies[writer])
					from := "copy-by-" + writer + "/read-by-" + reader
					check("all", from, vjCells(runWithDSN(t, reader, path, vjCopyReadback)))
					if reader == "cgo" {
						check("storage", from, vjCells(runWithDSN(t, reader, path, vjCopyStorageReadback)))
					}
				}
			}
		})
	}
}

// vjHeaderVersionField is the version number offset in the header.
const vjHeaderVersionField = 96

// TestVacuumIntoCopyIsByteIdentical verifies that copies are byte-identical
// except for the version field.
func TestVacuumIntoCopyIsByteIdentical(t *testing.T) {
	for name, prog := range vjVacuumIntoPrograms {
		prog := prog
		t.Run(name, func(t *testing.T) {
			var files [][]byte
			for _, writer := range engineOrder {
				dir := t.TempDir()
				cp := filepath.Join(dir, "copy.db")
				stmts := append(append([]string{}, prog...), "VACUUM INTO '"+cp+"'")
				runWithDSN(t, writer, filepath.Join(dir, "src.db"), stmts)
				b, err := os.ReadFile(exportedForOracle(t, cp))
				if err != nil {
					t.Fatalf("%s: %v", writer, err)
				}
				if len(b) >= vjHeaderVersionField+4 {
					copy(b[vjHeaderVersionField:vjHeaderVersionField+4], []byte{0, 0, 0, 0})
				}
				files = append(files, b)
			}
			if !bytes.Equal(files[0], files[1]) {
				t.Errorf("VACUUM INTO copy bytes differ (%s): %s=%d bytes, %s=%d bytes, first diff at %d",
					name, engineOrder[0], len(files[0]), engineOrder[1], len(files[1]), vjFirstDiff(files[0], files[1]))
			}
		})
	}
}

func vjFirstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// TestVacuumIntoDoesNotRenumberRowidsButVacuumDoes verifies that VACUUM INTO
// preserves rowids while VACUUM does not.
func TestVacuumIntoDoesNotRenumberRowidsButVacuumDoes(t *testing.T) {
	setup := []string{
		"CREATE TABLE r(a)",
		"INSERT INTO r VALUES('x'),('y'),('z'),('w')",
		"DELETE FROM r WHERE a IN ('y','z')",
	}
	var baseline, baselineFrom string
	for _, writer := range engineOrder {
		dir := t.TempDir()
		cp := filepath.Join(dir, "copy.db")
		src := filepath.Join(dir, "src.db")
		runWithDSN(t, writer, src, append(append([]string{}, setup...), "VACUUM INTO '"+cp+"'"))
		copyRowids := vjCells(runWithDSN(t, writer, cp, []string{"SELECT rowid,a FROM r ORDER BY rowid"}))
		// ...and the SOURCE is untouched by the copy, then renumbered by a
		// plain VACUUM of its own.
		after := vjCells(runWithDSN(t, writer, src, []string{
			"SELECT rowid,a FROM r ORDER BY rowid",
			"VACUUM",
			"SELECT rowid,a FROM r ORDER BY rowid",
		}))
		got := copyRowids + " | " + after
		if baseline == "" {
			baseline, baselineFrom = got, writer
		} else if got != baseline {
			t.Errorf("rowid handling differs\n  %s: %s\n  %s: %s", baselineFrom, baseline, writer, got)
		}
		if !strings.Contains(copyRowids, `["I:4","T:w"]`) {
			t.Errorf("%s: VACUUM INTO copy did not PRESERVE rowid 4: %s", writer, copyRowids)
		}
	}
}

// TestVacuumIntoAppliesAPendingAutoVacuum verifies that a deferred auto_vacuum
// mode is applied to the copy but not the source.
func TestVacuumIntoAppliesAPendingAutoVacuum(t *testing.T) {
	res := vjRunBoth(t, func(dir string) []string {
		return []string{
			"CREATE TABLE t(a)", "INSERT INTO t VALUES(1),(2)",
			"PRAGMA auto_vacuum=1",
			"VACUUM INTO '" + filepath.Join(dir, "copy.db") + "'",
			"PRAGMA auto_vacuum", // the SOURCE stays as it was
		}
	})
	if a, b := vjCells(res["cgo"]), vjCells(res["musql"]); a != b {
		t.Errorf("the source's own auto_vacuum differs after the copy\n  cgo:    %s\n  musql: %s", a, b)
	}
	for _, eng := range engineOrder {
		dir := t.TempDir()
		cp := filepath.Join(dir, "copy.db")
		runWithDSN(t, eng, filepath.Join(dir, "src.db"), []string{
			"CREATE TABLE t(a)", "INSERT INTO t VALUES(1),(2)",
			"PRAGMA auto_vacuum=2",
			"VACUUM INTO '" + cp + "'",
		})
		got := vjCells(runWithDSN(t, eng, cp, []string{"PRAGMA auto_vacuum"}))
		if !strings.Contains(got, "I:2") {
			t.Errorf("%s: the copy was not built in the pending incremental mode: %s", eng, got)
		}
	}
}

// TestVacuumIntoLeavesTheSourceAlone verifies that the source database is
// unchanged after VACUUM INTO.
func TestVacuumIntoLeavesTheSourceAlone(t *testing.T) {
	res := vjRunBoth(t, func(dir string) []string {
		return []string{
			"CREATE TABLE a(x)", "CREATE TABLE b(x)", "CREATE TABLE c(x)",
			"INSERT INTO a VALUES(1),(2),(3)", "DELETE FROM a WHERE x=2", "DROP TABLE b",
			"PRAGMA user_version=9",
			"PRAGMA page_count", "PRAGMA freelist_count", // 7, 8
			"PRAGMA schema_version",
			"SELECT rowid,x FROM a ORDER BY rowid",
			"VACUUM INTO '" + filepath.Join(dir, "copy.db") + "'",
			"PRAGMA page_count", "PRAGMA freelist_count", // 12, 13
			"PRAGMA schema_version",
			"PRAGMA user_version",
			"SELECT rowid,x FROM a ORDER BY rowid",
		}
	})
	counts := map[int]bool{7: true, 8: true, 12: true, 13: true}
	others := map[string][]map[string]any{}
	for _, eng := range engineOrder {
		r := res[eng]
		if before, after := vjCells(r[7:9]), vjCells(r[12:14]); before != after {
			t.Errorf("%s: VACUUM INTO changed the source's page/freelist counts: %s -> %s", eng, before, after)
		}
		for i, row := range r {
			if !counts[i] {
				others[eng] = append(others[eng], row)
			}
		}
	}
	if a, b := vjCells(others["cgo"]), vjCells(others["musql"]); a != b {
		t.Errorf("VACUUM INTO changed the source differently\n  cgo:    %s\n  musql: %s", a, b)
	}
}

// TestVacuumIntoRefusalsMatchCSQLite verifies that both engines refuse the
// same invalid VACUUM INTO statements.
func TestVacuumIntoRefusalsMatchCSQLite(t *testing.T) {
	cases := map[string]func(dir string) []string{
		// The target already holds a database.
		"targetExists": func(dir string) []string {
			cp := filepath.Join(dir, "copy.db")
			return []string{"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)",
				"VACUUM INTO '" + cp + "'", "VACUUM INTO '" + cp + "'"}
		},
		// ...including when it is this very database.
		"targetIsSource": func(dir string) []string {
			return []string{"CREATE TABLE t(a)", "VACUUM INTO '" + filepath.Join(dir, "src.db") + "'"}
		},
		// A schema qualifier resolved at prepare time beats everything else.
		"unknownSchema": func(dir string) []string {
			return []string{"CREATE TABLE t(a)", "VACUUM nosuch INTO '" + filepath.Join(dir, "c.db") + "'"}
		},
		// ...and inside a transaction, the transaction check beats every rule
		// about the target: a target that already exists still reports
		// "cannot VACUUM from within a transaction".
		"insideTransaction": func(dir string) []string {
			cp := filepath.Join(dir, "copy.db")
			return []string{"CREATE TABLE t(a)", "VACUUM INTO '" + cp + "'",
				"BEGIN", "VACUUM INTO '" + filepath.Join(dir, "c2.db") + "'",
				"VACUUM INTO '" + cp + "'", "COMMIT"}
		},
		// A directory that does not exist cannot be created.
		"missingDirectory": func(dir string) []string {
			return []string{"CREATE TABLE t(a)", "VACUUM INTO '" + filepath.Join(dir, "nodir", "c.db") + "'"}
		},
		// ...and the same transaction rule for PLAIN VACUUM, which this file
		// reaches through the DRIVER: a SQL "BEGIN" run as text keeps one
		// engine session open from BEGIN to COMMIT without the engine ever
		// seeing a BEGIN statement, so the engine's own txActive stays false.
		// C SQLite refuses a VACUUM there all the same, and this engine used
		// to ACCEPT it -- the one direction that is scored wrong.
		"plainVacuumInsideTransaction": func(dir string) []string {
			return []string{"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)",
				"BEGIN", "VACUUM", "COMMIT", "VACUUM"}
		},
	}
	for name, mk := range cases {
		mk := mk
		t.Run(name, func(t *testing.T) {
			res := vjRunBoth(t, mk)
			a, b := vjKinds(res["cgo"]), vjKinds(res["musql"])
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("refusals differ\n  cgo:    %v\n  musql: %v", a, b)
			}
			if !strings.Contains(fmt.Sprint(a), "error") {
				t.Fatalf("case %s no longer refuses anything on the ORACLE (%v) -- the rule it pins is gone", name, a)
			}
		})
	}
}

// TestVacuumIntoRefusesANonDatabaseTarget verifies that non-database files
// are rejected while zero-length files are accepted.
func TestVacuumIntoRefusesANonDatabaseTarget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []byte
		refuse  bool
	}{
		{"garbage", []byte("hello"), true},
		{"zeroLength", []byte{}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var baseline string
			for _, eng := range engineOrder {
				dir := t.TempDir()
				cp := filepath.Join(dir, "target.db")
				if err := os.WriteFile(cp, tc.content, 0o644); err != nil {
					t.Fatal(err)
				}
				got := vjKinds(runWithDSN(t, eng, filepath.Join(dir, "src.db"), []string{
					"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)", "VACUUM INTO '" + cp + "'",
				}))
				last := got[len(got)-1]
				if (last == "error") != tc.refuse {
					t.Errorf("%s: VACUUM INTO onto a %s target: kind=%s, want refuse=%v", eng, tc.name, last, tc.refuse)
				}
				if baseline == "" {
					baseline = fmt.Sprint(got)
				} else if fmt.Sprint(got) != baseline {
					t.Errorf("engines differ on a %s target\n  %s\n  %s", tc.name, baseline, got)
				}
			}
		})
	}
}

// TestVacuumIntoExpressionTarget verifies that the VACUUM INTO target can be
// an expression, including subqueries.
func TestVacuumIntoExpressionTarget(t *testing.T) {
	t.Run("subqueryTargetReadsBackCorrectly", func(t *testing.T) {
		for _, writer := range engineOrder {
			dir := t.TempDir()
			cp := filepath.Join(dir, "copy.db")
			runWithDSN(t, writer, filepath.Join(dir, "src.db"), []string{
				"CREATE TABLE t(a)", "INSERT INTO t VALUES(1),(2)",
				"CREATE TABLE names(n)", "INSERT INTO names VALUES('" + cp + "')",
				"VACUUM INTO (SELECT n FROM names)",
			})
			got := vjCells(runWithDSN(t, writer, cp, []string{"SELECT a FROM t ORDER BY a", "SELECT n FROM names"}))
			want := `[{"cols":["a"],"kind":"rows","rows":[["I:1"],["I:2"]]},{"cols":["n"],"kind":"rows","rows":[["T:` + cp + `"]]}]`
			if got != want {
				t.Errorf("%s: subquery-target copy content wrong\n  got:  %s\n  want: %s", writer, got, want)
			}
		}
	})

	cases := map[string]func(dir string) []string{
		"nonTextNumber": func(dir string) []string {
			return []string{"CREATE TABLE t(a)", "VACUUM INTO 5"}
		},
		"nonTextNull": func(dir string) []string {
			return []string{"CREATE TABLE t(a)", "VACUUM INTO NULL"}
		},
		"concatExpression": func(dir string) []string {
			return []string{"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)",
				"VACUUM INTO '" + filepath.Join(dir, "c") + "' || '.db'"}
		},
		// The transaction check wins over the target's own validity -- a
		// non-text target inside a transaction still reports the
		// transaction error, not "non-text filename".
		"nonTextInsideTransactionStaysTransactionError": func(dir string) []string {
			return []string{"CREATE TABLE t(a)", "BEGIN", "VACUUM INTO 5", "ROLLBACK"}
		},
	}
	for name, mk := range cases {
		mk := mk
		t.Run(name, func(t *testing.T) {
			res := vjRunBoth(t, mk)
			a, b := vjKinds(res["cgo"]), vjKinds(res["musql"])
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("kinds differ\n  cgo:    %v\n  musql: %v", a, b)
			}
		})
	}
}

// TestVacuumIntoMemoryAndEmptyTargetsAreNoOps verifies that :memory: and
// empty string targets are no-ops that leave the source unchanged.
func TestVacuumIntoMemoryAndEmptyTargetsAreNoOps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stmts func(target string) []string
	}{
		{"literal", func(target string) []string {
			return []string{
				"CREATE TABLE t2(name TEXT)",
				"INSERT INTO t2 VALUES('" + target + "')",
				"VACUUM main INTO '" + target + "'",
				"SELECT name FROM t2",
			}
		}},
		{"subquery", func(target string) []string {
			return []string{
				"CREATE TABLE t2(name TEXT)",
				"INSERT INTO t2 VALUES('" + target + "')",
				"VACUUM main INTO (SELECT name FROM t2)",
				"SELECT name FROM t2",
			}
		}},
	} {
		for _, target := range []string{":memory:", ""} {
			tc, target := tc, target
			wantRow := fmt.Sprintf(`[{"cols":["name"],"kind":"rows","rows":[["T:%s"]]}]`, target)
			t.Run(tc.name+"/"+target, func(t *testing.T) {
				for _, eng := range engineOrder {
					dir := t.TempDir()
					got := runWithDSN(t, eng, filepath.Join(dir, "src.db"), tc.stmts(target))
					if k := vjKinds(got); strings.Contains(fmt.Sprint(k), "error") {
						t.Fatalf("%s: statement refused: %v", eng, k)
					}
					if cells := vjCells(got[len(got)-1:]); cells != wantRow {
						t.Errorf("%s: source row did not read back unchanged\n  got:  %s\n  want: %s", eng, cells, wantRow)
					}
					// VACUUM INTO %q must leave nothing else in the directory:
					// see execVacuumInto's doc comment -- the private btree
					// behind ':memory:'/'' is discarded, never written to disk.
					ents, rerr := os.ReadDir(dir)
					if rerr != nil {
						t.Fatal(rerr)
					}
					// The source's own files are not strays: a segment database is
					// src.db plus its delta and lock files.
					var stray bool
					for _, e := range ents {
						switch e.Name() {
						case "src.db", "src.db.delta", "src.db.lock":
						default:
							stray = true
						}
					}
					if stray {
						var names []string
						for _, e := range ents {
							names = append(names, e.Name())
						}
						t.Errorf("%s: VACUUM INTO %q left a stray directory entry: %v", eng, target, names)
					}
				}
			})
		}
	}
}

// TestVacuumIntoMemoryCaseVariantWritesARealFile verifies that case variants
// of :memory: are treated as real file paths.
func TestVacuumIntoMemoryCaseVariantWritesARealFile(t *testing.T) {
	for _, target := range []string{":MEMORY:", ":Memory:"} {
		target := target
		t.Run(target, func(t *testing.T) {
			for _, eng := range engineOrder {
				dir := t.TempDir()
				old, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chdir(dir); err != nil {
					t.Fatal(err)
				}
				got := runWithDSN(t, eng, filepath.Join(dir, "src.db"), []string{
					"CREATE TABLE t(a)",
					"INSERT INTO t VALUES(1)",
					"VACUUM main INTO '" + target + "'",
				})
				if cerr := os.Chdir(old); cerr != nil {
					t.Fatal(cerr)
				}
				if k := vjKinds(got); strings.Contains(fmt.Sprint(k), "error") {
					t.Fatalf("%s: VACUUM INTO %q refused: %v", eng, target, k)
				}
				full := filepath.Join(dir, target)
				if _, serr := os.Stat(full); serr != nil {
					t.Errorf("%s: VACUUM INTO %q did not create a file named %q: %v", eng, target, target, serr)
				}
			}
		})
	}
}

// ---- the journal file a journal_mode switch leaves behind ----

// TestJournalModeSwitchDropsTheOldModesJournal verifies that switching journal
// modes deletes the previous mode's journal file.
func TestJournalModeSwitchDropsTheOldModesJournal(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		{"truncate", "memory"}, {"persist", "memory"},
		{"truncate", "delete"}, {"persist", "delete"},
		{"persist", "truncate"}, {"truncate", "persist"},
	} {
		tc := tc
		t.Run(tc.from+"-to-"+tc.to, func(t *testing.T) {
			arts := map[string]jmArtifact{}
			for _, eng := range engineOrder {
				dsn := filepath.Join(t.TempDir(), "jm.db")
				runWithDSN(t, eng, dsn, []string{
					"PRAGMA journal_mode=" + tc.from,
					"CREATE TABLE t(a)",
					"INSERT INTO t VALUES(1)",
					"PRAGMA journal_mode=" + tc.to, // nothing after it: the switch is the whole event
				})
				arts[eng] = jmStat(t, dsn)
			}
			// musql keeps no journal in any mode (see jmWantArtifact), so for it
			// the whole event is "none appears". C's file is logged: between two
			// file-keeping modes it is the OLD mode's until the next commit.
			jmWantArtifact(t, "musql", tc.to, arts["musql"])
			t.Logf("%s -> %s: cgo journal %s", tc.from, tc.to, arts["cgo"])
		})
	}
}

// ---- PRAGMA journal_mode inside a transaction ----

// vjJournalModeInTxnCases tests PRAGMA journal_mode inside transactions.
var vjJournalModeInTxnCases = map[string][]string{
	// Clean: nothing written at all since BEGIN.
	"cleanDeferred": {"BEGIN", "PRAGMA journal_mode=truncate", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	"cleanAfterSelect": {"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)",
		"BEGIN", "SELECT * FROM t", "PRAGMA journal_mode=persist", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	// A write lock alone does not dirty a page.
	"cleanImmediate": {"CREATE TABLE t(a)",
		"BEGIN IMMEDIATE", "PRAGMA journal_mode=truncate", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	"cleanExclusive": {"CREATE TABLE t(a)",
		"BEGIN EXCLUSIVE", "PRAGMA journal_mode=persist", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	// Two assignments in a row, neither of which dirties anything.
	"cleanTwice": {"CREATE TABLE t(a)",
		"BEGIN", "PRAGMA journal_mode=delete", "PRAGMA journal_mode=truncate", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	// Dirty: a write really happened, so C SQLite SILENTLY IGNORES the
	// assignment and the mode survives the transaction unchanged.
	"dirtyInsert": {"CREATE TABLE t(a)", "PRAGMA journal_mode=truncate",
		"BEGIN", "INSERT INTO t VALUES(1)", "PRAGMA journal_mode=persist", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	"dirtyHeaderWrite": {"CREATE TABLE t(a)", "PRAGMA journal_mode=truncate",
		"BEGIN", "PRAGMA user_version=3", "PRAGMA journal_mode=persist", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	// A zero-row write dirties NOTHING in C SQLite, so the assignment takes
	// -- the case an "any DML" rule would get wrong. musql declines it (an
	// over-decline is mirrored onto both engines and costs only coverage), so
	// this case is here to keep the ORACLE's answer recorded and to fail if
	// musql ever starts ANSWERING it the other way.
	"zeroRowUpdate": {"CREATE TABLE t(a)", "INSERT INTO t VALUES(1)", "PRAGMA journal_mode=truncate",
		"BEGIN", "UPDATE t SET a=a WHERE 0", "PRAGMA journal_mode=persist", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	// Entering or leaving WAL from inside a clean transaction is an ERROR in
	// C SQLite, not a silent no-op and not a change.
	"cleanIntoWAL": {"CREATE TABLE t(a)",
		"BEGIN", "PRAGMA journal_mode=wal", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	"cleanOutOfWAL": {"PRAGMA journal_mode=wal", "CREATE TABLE t(a)",
		"BEGIN", "PRAGMA journal_mode=delete", "PRAGMA journal_mode", "COMMIT", "PRAGMA journal_mode"},
	// An assignment to the mode the connection is ALREADY in is a no-op
	// wherever it appears -- the case that must not start erroring.
	"sameModeDirty": {"CREATE TABLE t(a)", "PRAGMA journal_mode=truncate",
		"BEGIN", "INSERT INTO t VALUES(1)", "PRAGMA journal_mode=truncate", "PRAGMA journal_mode", "COMMIT"},
	// The journal mode is CONNECTION state, not transaction state: a change
	// taken inside a transaction survives that transaction's ROLLBACK.
	"cleanThenRollback": {"CREATE TABLE t(a)",
		"BEGIN", "PRAGMA journal_mode=truncate", "ROLLBACK", "PRAGMA journal_mode"},
	// The SAVEPOINT forms are deliberately absent. A SAVEPOINT in autocommit
	// opens a transaction of its own in the ENGINE (engine/txn.go's
	// openSavepoint), and both halves of the rule apply inside it -- verified
	// against the oracle: "SAVEPOINT s; PRAGMA journal_mode=truncate" TAKES,
	// while "SAVEPOINT s; INSERT ...; ROLLBACK TO s; PRAGMA journal_mode=..."
	// is still IGNORED, so rolling back to the savepoint does NOT make the
	// pager clean again. Neither can be replayed HERE, because driver
	// declines SAVEPOINT/ROLLBACK TO/RELEASE outright: through this worker the
	// statements simply error and the pragma runs in plain autocommit, which
	// tests the driver's gap rather than the rule. The engine's own flag covers
	// it by construction -- SAVEPOINT is in the safe set and nothing clears the
	// flag short of the transaction ENDING (clearTxnState).
}

// TestJournalModeInsideATransaction verifies that both engines handle journal
// mode changes inside transactions correctly.
func TestJournalModeInsideATransaction(t *testing.T) {
	for name, prog := range vjJournalModeInTxnCases {
		prog := prog
		t.Run(name, func(t *testing.T) {
			res := vjRunBoth(t, func(string) []string { return prog })
			cgo, mush := res["cgo"], res["musql"]
			for i, stmt := range prog {
				isGetter := strings.EqualFold(strings.TrimSpace(stmt), "PRAGMA journal_mode")
				isSetter := strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stmt)), "PRAGMA JOURNAL_MODE=")
				if isSetter && vjKinds(mush)[i] == "error" && vjKinds(cgo)[i] != "error" {
					// A declined setter. The corpus mirrors that onto both
					// engines; here it means the mode did NOT move on musql,
					// so the getters below legitimately diverge -- stop.
					t.Logf("%s: musql declines %q (statement %d); oracle answered %s", name, stmt, i, vjCells(cgo[i:i+1]))
					return
				}
				a, b := vjCells(cgo[i:i+1]), vjCells(mush[i:i+1])
				if a == b {
					continue
				}
				if isGetter || isSetter {
					t.Errorf("%s: statement %d %q\n  cgo:    %s\n  musql: %s", name, i, stmt, a, b)
				}
			}
		})
	}
}

// ---- PRAGMA optimize ----

// TestPragmaOptimizeStaysDeclinedAndWhy verifies PRAGMA optimize behavior
// by testing when it re-analyzes indexes.
func TestPragmaOptimizeStaysDeclinedAndWhy(t *testing.T) {
	prog := []string{
		"CREATE TABLE t1(x)", "CREATE INDEX i1 ON t1(x)", "INSERT INTO t1 VALUES(1),(2),(3)",
		"CREATE TABLE t2(y)", "CREATE INDEX i2 ON t2(y)", "INSERT INTO t2 VALUES(1),(2),(3)",
		"PRAGMA optimize", // case 1: no stats anywhere -> analyzes both
		"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl",
		"INSERT INTO t1 SELECT 1 FROM (WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<300) SELECT i FROM s)",
		"PRAGMA optimize", // grown 100x but never queried -> nothing
		"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl",
		"SELECT count(*) FROM t1", // a full SCAN is not enough either
		"PRAGMA optimize",
		"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl",
		"SELECT * FROM t1 WHERE x=1", // ...an INDEX-using query is
		"PRAGMA optimize",
		"SELECT tbl,idx,stat FROM sqlite_stat1 ORDER BY tbl",
	}
	dir := t.TempDir()
	got := runWithDSN(t, "cgo", filepath.Join(dir, "o.db"), prog)
	stat := func(i int) string { return vjCells(got[i : i+1]) }
	const (
		afterFirst  = 7
		afterGrowth = 10
		afterScan   = 13
		afterSeek   = 16
	)
	if !strings.Contains(stat(afterFirst), "t1") || !strings.Contains(stat(afterFirst), "t2") {
		t.Fatalf("oracle rule 1 changed: a bare PRAGMA optimize no longer analyzes never-analyzed indexes: %s", stat(afterFirst))
	}
	if stat(afterGrowth) != stat(afterFirst) || stat(afterScan) != stat(afterFirst) {
		t.Fatalf("oracle rule 2 changed: PRAGMA optimize re-analyzed without an index-using query\n  first: %s\n  grown: %s\n  scan:  %s",
			stat(afterFirst), stat(afterGrowth), stat(afterScan))
	}
	if stat(afterSeek) == stat(afterFirst) {
		t.Fatalf("oracle rule 2 changed: PRAGMA optimize did NOT re-analyze after an index-using query: %s", stat(afterSeek))
	}

	// musql answers rule 1 exactly: the result shape, and the sqlite_stat1 it
	// leaves behind.
	differ(t, "pragma optimize rule 1", prog[:8])

	// Rule 2 needs TF_MaybeReanalyze to outlive the statement that set it, as it
	// does on a connection, and a driver session lasts one autocommit statement
	// (pragma_optimize_track.go keeps it per *DB). Until the connection holds
	// it, musql declines a PRAGMA optimize whose statistics have moved rather
	// than guess which tables the planner used.
	mush := runWithDSN(t, "musql", filepath.Join(t.TempDir(), "m.db"), prog[:10])
	if k := vjKinds(mush); k[9] != "error" {
		t.Errorf("musql now answers PRAGMA optimize after growth without an index-using query (%v) -- differ the whole program against the oracle instead", k)
	}
}
