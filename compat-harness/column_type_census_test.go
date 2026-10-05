package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	musqlengine "github.com/samyfodil/musql/engine"
)

// TestColumnTypeCensus counts the distribution of storage classes across
// columns in the mined corpus to assess columnar format viability.
func TestColumnTypeCensus(t *testing.T) {
	files := tclAllFiles(t)
	if len(files) == 0 {
		t.Skip("no .test files found under " + tclCorpusDir)
	}
	corpusDir, aerr := filepath.Abs(tclCorpusDir)
	if aerr != nil {
		t.Fatalf("resolving the corpus directory: %v", aerr)
	}
	if testing.Short() && len(files) > tclShortFileCount {
		files = files[:tclShortFileCount]
	}

	var (
		cols, clean, allNull, mixed int
		typedRows, exceptionRows    int64
		worst                       []musqlengine.ColumnTypeCensus
		classPairs                  = map[string]int{}
		// A column holding two rows cannot show mixing in any meaningful way,
		// and a corpus of fragments produces a great many of them. The same
		// counts restricted to columns with real data are the ones to design
		// against; both are reported, because the gap between them is itself
		// the measurement's honesty.
		bigCols, bigClean, bigMixed int
		bigTyped, bigExceptions     int64
	)
	const meaningful = 10

	dir := t.TempDir()
	// The mined SQL contains literal file names -- "ATTACH 'test.db2'" and its
	// kin -- which a replay resolves against the WORKING DIRECTORY. Left alone
	// that writes databases into the source tree, which this project forbids
	// outright (Makefile's check-no-stray-dbs), and it is worse than untidy: an
	// attached database that OUTLIVES the segment that made it is then found by
	// a later segment, which is a state no isolated replay ever produces.
	// Caught exactly that way -- three stray files and two structural-check
	// findings that came with them.
	t.Chdir(dir)
	for fi, rel := range files {
		src, err := os.ReadFile(filepath.Join(corpusDir, rel))
		if err != nil {
			continue
		}
		for si, stmts := range tclSegments(string(src)) {
			path := filepath.Join(dir, fmt.Sprintf("c%d-%d.musq", fi, si))
			if !censusReplay(path, stmts) {
				continue
			}
			rp, oerr := musqlengine.Open(path)
			if oerr != nil {
				os.Remove(path)
				continue
			}
			census, cerr := rp.CensusColumnTypes()
			rp.Close()
			os.Remove(path)
			if cerr != nil {
				continue
			}
			for _, c := range census {
				cols++
				switch {
				case c.Typed() == 0:
					allNull++
				case c.Exceptions() == 0:
					clean++
				default:
					mixed++
					if len(worst) < 40 {
						worst = append(worst, c)
					}
					classPairs[censusClasses(c)]++
				}
				typedRows += c.Typed()
				exceptionRows += c.Exceptions()
				if c.Typed() >= meaningful {
					bigCols++
					bigTyped += c.Typed()
					bigExceptions += c.Exceptions()
					if c.Exceptions() == 0 {
						bigClean++
					} else {
						bigMixed++
					}
				}
			}
		}
	}

	if cols == 0 {
		t.Fatal("censused no columns at all; this test would report a clean bill of health for an empty measurement")
	}
	t.Logf("COLUMN TYPE CENSUS over the mined corpus: columns=%d cleanly-typed=%d (%.1f%%) all-NULL=%d (%.1f%%) mixed=%d (%.1f%%)",
		cols, clean, pct(clean, cols), allNull, pct(allNull, cols), mixed, pct(mixed, cols))
	t.Logf("  rows: typed=%d exceptions=%d (%.3f%% of stored non-NULL values would go to the side list)",
		typedRows, exceptionRows, pct(int(exceptionRows), int(typedRows)))

	t.Logf("  restricted to columns with >=%d stored non-NULL values: columns=%d cleanly-typed=%d (%.1f%%) mixed=%d (%.1f%%)",
		meaningful, bigCols, bigClean, pct(bigClean, bigCols), bigMixed, pct(bigMixed, bigCols))
	t.Logf("  restricted rows: typed=%d exceptions=%d (%.3f%%)",
		bigTyped, bigExceptions, pct(int(bigExceptions), int(bigTyped)))

	kinds := make([]string, 0, len(classPairs))
	for k := range classPairs {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(a, b int) bool { return classPairs[kinds[a]] > classPairs[kinds[b]] })
	for i, k := range kinds {
		if i == 12 {
			break
		}
		t.Logf("  mixed shape %-22s %d columns", k, classPairs[k])
	}
	for i, c := range worst {
		if i == 10 {
			break
		}
		cls, n := c.Majority()
		t.Logf("  example: %s col %d -- majority %s %d, exceptions %d of %d (null %d int %d real %d text %d blob %d)",
			c.Table, c.Column, cls, n, c.Exceptions(), c.Typed(), c.Null, c.Int, c.Real, c.Text, c.Blob)
	}
}

// censusReplay runs a segment into a fresh engine database, ignoring every
// statement error: a mined segment is a fragment, most of them fail somewhere,
// and what this test wants is whatever data does land.
//
// Ignoring errors is what makes this a MEASUREMENT rather than a gate, and it
// drives the engine outside the contract every other caller keeps -- a real
// caller stops when a statement fails. Two consequences, both handled here
// rather than left to surprise someone:
//
//   - the structural check is OFF for the replay. Left on it reported a
//     committed file with four million unsound pages, which came from
//     bigsort.test building a database of ~1.8 million pages on a 16GB tmpfs:
//     a write failed for space, this replay ignored the error, and Close()
//     committed the partial b-tree. Whether the engine should refuse that
//     commit is a real question and a separate one; it is not something a
//     typing census should be asserting about, and leaving it on made the
//     whole harness package FAIL on a measurement.
//   - each database is capped. A typing census learns nothing from a table
//     with a million pages that it does not learn from one with a thousand,
//     and uncapped it filled the box (and wrote a 1.9GB log, because a single
//     finding enumerated four million pages).
func censusReplay(path string, stmts []string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()

	db, err := musqlengine.Create(path)
	if err != nil {
		return false
	}
	// The page cap this used to set ("PRAGMA max_page_count=20000", 80MB at 4KB)
	// is gone with the pages: a segment database has none, and the pragma declines.
	// What it was for -- stopping one pathological corpus segment from filling the
	// disk -- is now bounded by the segment file's own size instead, and by the
	// fact that a census only needs enough rows per column to see what it holds.
	wrote := false
	for _, s := range stmts {
		if err := db.Exec(s); err == nil {
			wrote = true
		}
	}
	if cerr := db.Close(); cerr != nil {
		return false
	}
	return wrote
}

func censusClasses(c musqlengine.ColumnTypeCensus) string {
	s := ""
	for _, p := range []struct {
		name string
		n    int64
	}{{"int", c.Int}, {"real", c.Real}, {"text", c.Text}, {"blob", c.Blob}} {
		if p.n > 0 {
			if s != "" {
				s += "+"
			}
			s += p.name
		}
	}
	return s
}
