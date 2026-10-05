// This file tests SLT files with duplicate column names in self-joins,
// testing proper column name disambiguation per C SQLite rules.
package compat

import (
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

// r32mSLTDupNameFiles are the test files relative to sltTestdataDir.
var r32mSLTDupNameFiles = []struct {
	rel     string
	inShort bool
}{
	{"index/random/scale1000_slt_good_0.test", false},
	{"index/random/scale1000_slt_good_5.test", false},
	{"random/aggregates/slt_good_0.test", true},
	{"random/aggregates/slt_good_129.test", true},
	{"random/expr/slt_good_1.test", true},
	{"random/expr/slt_good_32.test", true},
	{"random/select/slt_good_124.test", true},
}

// TestR32MSLTDuplicateColumnNameFiles replays each SLT file against the oracle.
func TestR32MSLTDuplicateColumnNameFiles(t *testing.T) {
	r32mSkipUntilRenamingLanded(t)
	for _, f := range r32mSLTDupNameFiles {
		if testing.Short() && !f.inShort {
			continue
		}
		rel := f.rel
		t.Run(rel, func(t *testing.T) {
			stmts, err := parseSLTFile(filepath.Join(sltTestdataDir, rel))
			if err != nil {
				t.Fatalf("parsing %s: %v", rel, err)
			}
			if len(stmts) == 0 {
				t.Fatalf("no records survived the sqlite filter in %s", rel)
			}
			differ(t, rel, stmts)
		})
	}
}
