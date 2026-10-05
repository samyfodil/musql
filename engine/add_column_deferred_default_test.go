package engine_test

import (
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/engine"
)

// TestAddColumnNonConstantDefaultSurvivesReopen verifies that non-constant
// column defaults remain correct across Close/reopen.
func TestAddColumnNonConstantDefaultSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "addcol.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE t(x)",
		"ALTER TABLE t ADD COLUMN ts DEFAULT CURRENT_TIMESTAMP",
		"ALTER TABLE t ADD COLUMN e DEFAULT (1+2)",
		"INSERT INTO t(x) VALUES(1)",
	} {
		if _, _, err := db.ExecArgs(s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := engine.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if _, _, err := db2.ExecArgs("INSERT INTO t(x) VALUES(2)", nil); err != nil {
		t.Fatalf("second-session INSERT: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close (2nd): %v", err)
	}

	p, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Expression defaults should fold identically across sessions.
	_, rows, err := p.Query("SELECT x, e, length(ts) FROM t ORDER BY x")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for i, r := range rows {
		if r[1].Typ != engine.Int || r[1].I != 3 {
			t.Errorf("row %d: e = %+v, want integer 3", i, r[1])
		}
		if r[2].Typ != engine.Int || r[2].I != 19 {
			t.Errorf("row %d: length(ts) = %+v, want 19", i, r[2])
		}
	}

	// Clock defaults should be fresh, not frozen at ALTER time.
	_, chk, err := p.Query("SELECT ts = strftime('%Y-%m-%d %H:%M:%S', ts) FROM t")
	if err != nil {
		t.Fatalf("shape Query: %v", err)
	}
	for i, r := range chk {
		if r[0].Typ != engine.Int || r[0].I != 1 {
			t.Errorf("row %d: ts is not a 'YYYY-MM-DD HH:MM:SS' timestamp: %+v", i, r[0])
		}
	}

	// The stored schema is what a reopen reads, so it must carry the clause
	// verbatim.
	_, sq, err := p.Query("SELECT sql FROM sqlite_master WHERE name='t'")
	if err != nil {
		t.Fatalf("schema Query: %v", err)
	}
	got := string(sq[0][0].S)
	want := "CREATE TABLE t(x, ts DEFAULT CURRENT_TIMESTAMP, e DEFAULT (1+2))"
	if got != want {
		t.Errorf("stored schema = %q, want %q", got, want)
	}
}
