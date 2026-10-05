package engine

import (
	"strings"
	"testing"
)

// analyzeAttachedDB creates a test database with attached file.
func analyzeAttachedDB(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()
	db, err := Create(dir + "/m.musq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"ATTACH '" + dir + "/aux.db' AS aux1",
		"CREATE TABLE main.mt(a)",
		"CREATE INDEX main.mi ON mt(a)",
		"INSERT INTO main.mt VALUES(1),(2)",
		"CREATE TABLE aux1.t(a,b)",
		"CREATE INDEX aux1.i ON t(a)",
		"INSERT INTO aux1.t VALUES(1,2),(3,4)",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return db
}

// analyzeStat1Of reads sqlite_stat1 from a database.
func analyzeStat1Of(t *testing.T, db *Session, qualifier string) []string {
	t.Helper()
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	_, rows, qerr := p.Query("SELECT tbl,idx,stat FROM " + qualifier + ".sqlite_stat1")
	if qerr != nil {
		return []string{"ERR: " + qerr.Error()}
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		cells := make([]string, len(r))
		for j, v := range r {
			cells[j] = string(v.S)
		}
		out[i] = strings.Join(cells, "|")
	}
	return out
}

// TestBareAnalyzeCoversAttachedDatabases tests that bare ANALYZE covers all attached databases.
func TestBareAnalyzeCoversAttachedDatabases(t *testing.T) {
	db := analyzeAttachedDB(t)
	if err := db.Exec("ANALYZE"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	if got, want := analyzeStat1Of(t, db, "aux1"), "t|i|2 1"; len(got) != 1 || got[0] != want {
		t.Errorf("aux1.sqlite_stat1 = %q, want [%q]", got, want)
	}
	if got, want := analyzeStat1Of(t, db, "main"), "mt|mi|2 1"; len(got) != 1 || got[0] != want {
		t.Errorf("main.sqlite_stat1 = %q, want [%q]", got, want)
	}
}

// TestAnalyzeNamesAnAttachedDatabase tests ANALYZE with attached database names.
func TestAnalyzeNamesAnAttachedDatabase(t *testing.T) {
	for _, tc := range []struct {
		stmt      string
		wantErr   string
		wantAux   string
		wantMainN int
	}{
		{stmt: "ANALYZE aux1", wantAux: "t|i|2 1", wantMainN: 0},
		{stmt: "ANALYZE aux1.t", wantAux: "t|i|2 1", wantMainN: 0},
		{stmt: "ANALYZE aux1.i", wantAux: "t|i|2 1", wantMainN: 0},
		{stmt: "ANALYZE main", wantAux: "", wantMainN: 1},
		{stmt: "ANALYZE aux1.nosuch", wantErr: "no such table: aux1.nosuch"},
		{stmt: "ANALYZE nosuchdb", wantErr: "no such table: nosuchdb"},
	} {
		t.Run(tc.stmt, func(t *testing.T) {
			db := analyzeAttachedDB(t)
			err := db.Exec(tc.stmt)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%s: err = %v, want %q", tc.stmt, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", tc.stmt, err)
			}
			aux := analyzeStat1Of(t, db, "aux1")
			switch {
			case tc.wantAux == "" && len(aux) != 0 && !strings.HasPrefix(aux[0], "ERR:"):
				t.Errorf("%s: aux1.sqlite_stat1 = %q, want it untouched", tc.stmt, aux)
			case tc.wantAux != "" && (len(aux) != 1 || aux[0] != tc.wantAux):
				t.Errorf("%s: aux1.sqlite_stat1 = %q, want [%q]", tc.stmt, aux, tc.wantAux)
			}
			if main := analyzeStat1Of(t, db, "main"); tc.wantMainN == 0 {
				if len(main) != 0 && !strings.HasPrefix(main[0], "ERR:") {
					t.Errorf("%s: main.sqlite_stat1 = %q, want it untouched", tc.stmt, main)
				}
			} else if len(main) != tc.wantMainN {
				t.Errorf("%s: main.sqlite_stat1 = %q, want %d row(s)", tc.stmt, main, tc.wantMainN)
			}
		})
	}
}

// TestBareAnalyzeTakesTheAliasLockLadder is why routing ANALYZE through the
// ordinary attached-write path matters beyond tidiness: under
// "PRAGMA locking_mode=exclusive" two aliases of one file each hold a
// persistent SHARED lock, so the write ANALYZE makes into either conflicts,
// and C SQLite answers "database is locked". Measured against the oracle
// on exactly this sequence; this engine answered SUCCESS before.
func TestBareAnalyzeTakesTheAliasLockLadder(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(dir + "/m.musq")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"PRAGMA locking_mode=EXCLUSIVE",
		"ATTACH '" + dir + "/a.db' AS aux1",
		"ATTACH '" + dir + "/a.db' AS aux2",
		"CREATE TABLE main.t1(x)",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	err = db.Exec("ANALYZE")
	if err == nil {
		t.Fatal("ANALYZE over two aliases under locking_mode=exclusive: answered, want \"database is locked\"")
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("ANALYZE: %v\n  want a \"database is locked\" busy error", err)
	}
}
