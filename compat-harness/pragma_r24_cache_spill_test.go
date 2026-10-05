// This file tests PRAGMA cache_spill against C SQLite. The behavior depends on
// whether cache_size is positive or negative, affecting when the actual spill
// threshold value can be determined exactly vs approximated.
package compat

import (
	"path/filepath"
	"testing"
)

// tempDSN is one throwaway database path per engine per case.
func tempDSN(t *testing.T, name string) string { return filepath.Join(t.TempDir(), name) }

var pragmaR24CacheSpillCases = []struct {
	name  string
	stmts []string
	// declines is how many of the statements musql is expected to refuse.
	// Compared exactly: a decline that disappears is as interesting as one
	// that appears.
	declines int
}{
	// With positive cache_size, all getters are exact
	{"pragma2-prefix", []string{
		`PRAGMA main.cache_size=2000`,
		`PRAGMA temp.cache_size=2000`,
		`PRAGMA cache_spill`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
		`PRAGMA cache_spill=OFF`,
		`PRAGMA cache_spill`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
	}, 0},

	// Each database's threshold is independent; bare form reports main
	{"per-database-and-the-bare-form-is-main", []string{
		`PRAGMA main.cache_size=1000`,
		`PRAGMA temp.cache_size=77`,
		`PRAGMA cache_spill`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
		`PRAGMA cache_spill=100000`,
		`PRAGMA cache_spill`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
	}, 0},

	// Spill below cache size doesn't change the reported value
	{"a-spill-below-the-cache-size-is-invisible", []string{
		`PRAGMA main.cache_size=2000`,
		`PRAGMA temp.cache_size=300`,
		`PRAGMA cache_spill=25`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
		`PRAGMA main.cache_spill=7`,
		`PRAGMA main.cache_spill`,
		`PRAGMA cache_spill`,
		`PRAGMA temp.cache_spill=9`,
		`PRAGMA temp.cache_spill`,
	}, 0},

	// Negative spill is bounded but may still be exact vs cache size
	{"a-negative-spill-below-the-bound", []string{
		`PRAGMA main.cache_size=2000`,
		`PRAGMA cache_spill(-25)`,
		`PRAGMA main.cache_spill`,
		`PRAGMA cache_spill`,
	}, 0},

	// Flag is connection-wide; moves both databases though thresholds are separate
	{"the-flag-is-connection-wide", []string{
		`PRAGMA main.cache_size=1000`,
		`PRAGMA temp.cache_size=77`,
		`PRAGMA cache_spill=OFF`,
		`PRAGMA cache_spill`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
		`PRAGMA cache_spill=ON`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
		`PRAGMA cache_spill=0`,
		`PRAGMA main.cache_spill`,
		`PRAGMA cache_spill=ON`,
		`PRAGMA main.cache_spill`,
	}, 0},

	// OFF makes the answer 0 regardless of cache size
	{"off-is-exact-even-on-a-fresh-connection", []string{
		`PRAGMA cache_spill=OFF`,
		`PRAGMA cache_spill`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
	}, 0},

	// ON with negative cache_size is computed exactly when page size is known,
	// including on fresh connections
	{"a-negative-cache-size-on-a-fresh-connection", []string{
		`PRAGMA cache_spill`,
		`PRAGMA main.cache_spill`,
		`PRAGMA temp.cache_spill`,
	}, 0},

	// Page size set before page creation, with negative cache_spill
	{"pragma2-test-5.1-through-5.3", []string{
		`PRAGMA page_size=16384`,
		`CREATE TABLE t1(x)`,
		`PRAGMA cache_size=2`,
		`PRAGMA cache_spill=YES`,
		`PRAGMA cache_spill`,
		`PRAGMA cache_spill=NO`,
		`PRAGMA cache_spill`,
		`PRAGMA cache_spill(-51)`,
		`PRAGMA cache_spill`,
	}, 0},

	// Cache size of 0 still floors to default spill of 1
	{"cache-size-zero-floors-to-the-default-spill", []string{
		`PRAGMA page_size=4096`,
		`CREATE TABLE t1(x)`,
		`PRAGMA cache_size=0`,
		`PRAGMA cache_spill`,
	}, 0},

	// Default cache size with existing page on disk
	{"default-cache-size-geometry-at-4096", []string{
		`PRAGMA page_size=4096`,
		`CREATE TABLE t1(x)`,
		`PRAGMA cache_spill`,
	}, 0},
	{"default-cache-size-geometry-at-1024", []string{
		`PRAGMA page_size=1024`,
		`CREATE TABLE t1(x)`,
		`PRAGMA cache_spill`,
	}, 0},
}

func TestPragmaR24CacheSpillMatchesCSQLite(t *testing.T) {
	for _, tc := range pragmaR24CacheSpillCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cgo := runWithDSN(t, "cgo", tempDSN(t, "o.db"), tc.stmts)
			mush := runWithDSN(t, "musql", tempDSN(t, "m.db"), tc.stmts)
			declines := 0
			for i, s := range tc.stmts {
				if vjKinds(mush)[i] == "error" {
					declines++
					if vjKinds(cgo)[i] == "error" {
						t.Errorf("stmt #%d %q: BOTH engines refused it -- this case's premise is wrong", i, s)
					}
					continue
				}
				if got, want := vjCells(mush[i:i+1]), vjCells(cgo[i:i+1]); got != want {
					t.Errorf("stmt #%d %q DIVERGES\n  cgo:    %s\n  musql: %s", i, s, want, got)
				}
			}
			if declines != tc.declines {
				t.Errorf("musql declined %d statement(s), expected %d", declines, tc.declines)
			}
		})
	}
}
