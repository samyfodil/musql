package engine

// Scanning a segment: docs/format-design.md's stage 3, the half that does not
// need the VDBE.
//
// A predicate over a cleanly-typed column is the case the whole format exists
// for, and it has two paths that must agree exactly:
//
//   - the FAST path reads the column as a plain []int64 -- no decode, no NULL
//     test, no bounds check per row -- which is only legal when the column is
//     fixed-width int64, has no NULL and has no exception. Measured at 2.1ns
//     per row against the b-tree's 581.8 and C SQLite's 128 for the whole query.
//   - the GENERAL path goes through segment.Value, which honours the NULL
//     bitmap and the exception list and works for every column and every type.
//     Measured at 36.6ns per row, which is still 3.5x faster than C.
//
// The fast path is an OPTIMISATION OF the general one, never a second
// implementation of it: segmentCountGreater runs whichever applies and
// TestSegmentScanPathsAgree pins them against each other over data built to
// make every reason to decline reachable. A fast path that silently disagreed
// with the general one would be the exact failure mode this project cannot
// have -- a wrong answer that is also faster.

// segScanKind says which path a column's scan can take, and is worth naming
// because "why did this column not take the fast path" is the question a
// performance investigation always asks first.
type segScanKind uint8

const (
	segScanSlice  segScanKind = iota // zero-copy []int64
	segScanValues                    // through segment.Value
)

// scanKind reports how column c would be scanned, and why.
func (s *segment) scanKind(c int) (segScanKind, string) {
	if c < 0 || c >= len(s.cols) {
		return segScanValues, "no such column"
	}
	if s.cols[c].phys != PhysInt64 {
		return segScanValues, "column is " + s.cols[c].phys.String() + ", not a fixed-width int64 block"
	}
	if s.cols[c].nullOff != 0 {
		return segScanValues, "column has NULLs, which the bitmap must be consulted for"
	}
	if s.hasException(c) {
		return segScanValues, "column has values in the exception list"
	}
	if _, ok := s.Int64Column(c); !ok {
		return segScanValues, "block is unaligned or this machine is not little-endian"
	}
	return segScanSlice, ""
}

// hasException reports whether any row of column c missed its physical type.
func (s *segment) hasException(c int) bool {
	if len(s.exc) == 0 {
		return false
	}
	nCols := uint64(len(s.cols))
	for k := range s.exc {
		if k%nCols == uint64(c) {
			return true
		}
	}
	return false
}

// segmentCountGreater answers "count the rows whose column c exceeds bound",
// which is workload 4's shape, taking whichever path the column allows.
//
// A NULL never satisfies a comparison, which is SQLite's own rule and the
// reason the fast path may only run on a column with no NULL at all: reading
// one as its zero cell would count it, and counting a NULL as 0 > -1 is a wrong
// answer rather than a slow one.
func segmentCountGreater(s *segment, c int, bound int64) int {
	if kind, _ := s.scanKind(c); kind == segScanSlice {
		col, _ := s.Int64Column(c)
		count := 0
		for _, x := range col {
			if x > bound {
				count++
			}
		}
		return count
	}
	// compareValues, NOT a hand-rolled integer test. SQLite orders values
	// ACROSS storage classes -- NULL below every number, every number below
	// every TEXT, TEXT below BLOB -- so a stray text in a mostly-integer column
	// is GREATER than any integer bound, and a test that only counted Int
	// values silently dropped it. Found by TestSegmentScanAgreesWithTheEngine
	// on exactly one row out of 1,500: the fast path cannot reach a mixed
	// column, so the bug lived only where the exception list does, which is
	// precisely where a hand-rolled comparison is least likely to be checked.
	probe := Value{Typ: Int, I: bound}
	count := 0
	for i := 0; i < s.nRows; i++ {
		if compareValues(s.Value(c, i), probe) > 0 {
			count++
		}
	}
	return count
}
