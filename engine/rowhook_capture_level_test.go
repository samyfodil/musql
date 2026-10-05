package engine

import (
	"path/filepath"
	"testing"
)

// TestRowCaptureLevels verifies that delta-level and full-level capture
// record the fields they claim to record.
func TestRowCaptureLevels(t *testing.T) {
	seed := func(t *testing.T, full bool) []RowChange {
		t.Helper()
		sess, err := Create(filepath.Join(t.TempDir(), "c.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Discard()
		db := sess.DB
		if full {
			db.EnableRowChangeCapture()
		}
		for _, s := range []string{
			`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
			`INSERT INTO t VALUES(1,'one')`,
			`UPDATE t SET b='ONE' WHERE a=1`,
		} {
			if e := sess.Exec(s); e != nil {
				t.Fatalf("%s: %v", s, e)
			}
		}
		return db.PeekRowChanges()
	}

	// Delta-level capture is always on for write sessions.
	light := seed(t, false)
	if len(light) == 0 {
		t.Fatal("a write session records nothing at all; the segment delta is built from this")
	}
	for _, ch := range light {
		if ch.Table == "" || ch.Rowid == 0 {
			t.Fatalf("delta-level capture lost a field segDeltaRecordsInto reads: %+v", ch)
		}
		if ch.Kind != RowDelete && len(ch.New) == 0 {
			t.Fatalf("delta-level capture lost New, which IS the delta record: %+v", ch)
		}
		if ch.Old != nil || ch.Cols != nil {
			t.Fatalf("delta-level capture recorded what only full capture reads: %+v", ch)
		}
	}

	// Full capture also journals CREATE statements.
	var full []RowChange
	for _, ch := range seed(t, true) {
		if ch.Kind != RowSchema {
			full = append(full, ch)
		}
	}
	if len(full) != len(light) {
		t.Fatalf("full capture recorded %d changes, delta-level %d: the LEVEL must not change WHICH rows are recorded", len(full), len(light))
	}
	sawOld := false
	for _, ch := range full {
		if len(ch.Cols) == 0 {
			t.Fatalf("full capture lost Cols: %+v", ch)
		}
		if ch.Kind == RowUpdate || ch.Kind == RowDelete {
			if len(ch.Old) == 0 {
				t.Fatalf("full capture lost Old on a %v, which the CRDT layer reads: %+v", ch.Kind, ch)
			}
			sawOld = true
		}
	}
	if !sawOld {
		t.Fatal("the UPDATE produced no change with an OLD image, so this test proves nothing about Old")
	}
}
