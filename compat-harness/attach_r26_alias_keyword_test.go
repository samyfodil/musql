package compat

// ATTACH NAME position is an expression; bare keywords are decided by grammar.
// Test three outcome classes: rejected, name=literal, name=evaluated.

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// Run ATTACH and report rejected or name=<schema>.
func attachAliasOutcomeCGO(t *testing.T, dir, kw string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "cmain.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ATTACH '` + filepath.Join(dir, "c_"+kw+".db") + `' AS ` + kw); err != nil {
		return "rejected"
	}
	rows, err := db.Query(`SELECT name FROM pragma_database_list WHERE name<>'main' AND name<>'temp'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return "name=" + strings.Join(names, ",")
}

// attachAliasOutcomeEngine RUNS the statement rather than only parsing it:
// ATTACH's own semantic rules reject some names the parser resolves fine (AS
// TEMP is "database TEMP is already in use" on both engines), and a gate that
// stopped at the parse would book that agreement as a divergence. The name it
// then reports is the one ParseAttachStmt bound, which is what execAttach
// registered.
func attachAliasOutcomeEngine(t *testing.T, dir, kw string) string {
	t.Helper()
	edb, err := engine.Create(filepath.Join(dir, "emain_"+kw+".sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer edb.Close()
	if err := edb.Exec(`ATTACH '` + filepath.Join(dir, "e_"+kw+".db") + `' AS ` + kw); err != nil {
		return "rejected"
	}
	_, name, ok, perr := engine.ParseAttachStmt(`ATTACH '' AS ` + kw)
	if perr != nil || !ok {
		t.Fatalf("[%s] the engine ACCEPTED the ATTACH but its parser now refuses the same name", kw)
	}
	return "name=" + name
}

// TestAttachAliasKeywordParity is the gate. It compares only the two things
// this engine has to get right -- accepted-vs-rejected, and, when accepted,
// the NAME that got bound -- and it derives both from the oracle in the same
// run, so it can never assert a stale expectation.
func TestAttachAliasKeywordParity(t *testing.T) {
	var mismatch []string
	for _, kw := range sqlKeywords {
		dir := t.TempDir()
		want := attachAliasOutcomeCGO(t, dir, kw)
		got := attachAliasOutcomeEngine(t, dir, kw)
		// The three CURRENT_* keywords evaluate to a clock reading, so the
		// oracle's own answer is the only expectation there is; compare it
		// verbatim like every other one, but tolerate a second-boundary
		// crossing on CURRENT_TIME/CURRENT_TIMESTAMP by re-reading once.
		if got != want && strings.HasPrefix(kw, "CURRENT_") {
			want = attachAliasOutcomeCGO(t, dir, kw)
		}
		if got != want {
			mismatch = append(mismatch, kw+": engine "+got+", cgo "+want)
		}
	}
	if len(mismatch) > 0 {
		sort.Strings(mismatch)
		t.Fatalf("ATTACH alias-position keyword divergence (%d):\n  %s", len(mismatch), strings.Join(mismatch, "\n  "))
	}
}
