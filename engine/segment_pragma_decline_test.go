package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// This file tests pragmas that decline at the session level.
func TestPragmaDeclineEngineDirect(t *testing.T) {
	nw, err := Create(filepath.Join(t.TempDir(), "p.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Discard()
	for _, s := range []string{`PRAGMA max_page_count=2`} {
		err := nw.Exec(s)
		if err == nil {
			t.Errorf("%s: accepted", s)
			continue
		}
		if !strings.Contains(err.Error(), "no meaning on this format") {
			t.Errorf("%s: %v", s, err)
		}
	}
	if err := nw.Exec(`CREATE TABLE t(a)`); err != nil {
		t.Errorf("an ordinary statement must still run: %v", err)
	}
	// ...and the two page COUNTS are served -- this checks only that they stop
	// declining, because Exec discards rows; the numbers themselves (page_count is
	// the file plus its delta in this database's own page size, freelist_count is 0
	// because a rewrite gives back everything churn retained) are probed through
	// the driver, and the differential probe is compat-harness's pragma diff.
	for _, s := range []string{`PRAGMA page_count`, `PRAGMA freelist_count`} {
		if err := nw.Exec(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	// ...and the three properties the catalog records ARE served, with C's own
	// answers (the differential probe for them is compat-harness's pragma diff).
	for _, s := range []string{`PRAGMA page_size=512`, `PRAGMA encoding='UTF-16le'`, `pragma auto_vacuum=1`} {
		if err := nw.Exec(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	// ...and every journal mode is SERVED, with C's answer: the four rollback
	// modes differ in C only in their journal file, off is the in-memory undo
	// rule journalOffUndoDisabled implements, and wal is recorded in the catalog
	// with the delta as its log (see execJournalMode's segment branch).
	for _, s := range []string{`PRAGMA journal_mode = DELETE`, `PRAGMA journal_mode=truncate`, `PRAGMA journal_mode=persist`,
		`PRAGMA journal_mode=memory`, `PRAGMA journal_mode=off`, `PRAGMA journal_mode=wal`, `PRAGMA journal_mode=delete`} {
		if err := nw.Exec(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}
