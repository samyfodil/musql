package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLikelihoodCheckedAtCodeTimeNotParseTime: likelihood argument validation timing.
func TestLikelihoodCheckedAtCodeTimeNotParseTime(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "lk.musq"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer db.Close()

	// Storing a view body with a bad argument must SUCCEED.
	for _, s := range []string{
		`CREATE VIEW v3 AS SELECT likelihood(1, 9)`,
		`CREATE VIEW c AS SELECT NULL INTERSECT SELECT NULL ORDER BY likelihood(NULL, (d, (SELECT c)))`,
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v (the oracle accepts this; the body is never prepared)", s, err)
		}
	}

	pg, perr := db.SnapshotPager()
	if perr != nil {
		t.Fatalf("SnapshotPager: %v", perr)
	}
	// Using it must FAIL, and so must a direct call.
	for _, q := range []string{
		`SELECT * FROM v3`,
		`SELECT likelihood(1, 9)`,
	} {
		_, _, qerr := pg.QueryArgs(q, nil)
		if qerr == nil {
			t.Errorf("%s: accepted, want the likelihood range error", q)
			continue
		}
		if !strings.Contains(qerr.Error(), "second argument to likelihood()") {
			t.Errorf("%s: %v, want the likelihood range error", q, qerr)
		}
	}
	// A valid one still runs, and the spelling is echoed as written.
	if _, _, qerr := pg.QueryArgs(`SELECT likelihood(1, 0.5)`, nil); qerr != nil {
		t.Errorf("SELECT likelihood(1, 0.5): %v", qerr)
	}
	_, _, uerr := pg.QueryArgs(`SELECT LIKELIHOOD(1, 9)`, nil)
	if uerr == nil || !strings.Contains(uerr.Error(), "LIKELIHOOD()") {
		t.Errorf("upper-case call: %v, want the message to echo LIKELIHOOD()", uerr)
	}
}
