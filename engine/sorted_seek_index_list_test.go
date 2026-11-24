package engine

import "testing"

// TestSortedSeekOverIndexSeekList: after ANALYZE, a join seeks its inner table
// on (dsk, moy, yr) where the constant terms also seek it, and an automatic
// index order waits on it. OpAutoIndexOrder positioned the constant seek's
// rows as a list the segments serve, which OpSortedSeek cannot sort, so every
// outer row walked the whole list (TPC-DS q71, 30 s against C's 1). The seek
// now builds those rows and searches them. The answers are C SQLite's.
func TestSortedSeekOverIndexSeekList(t *testing.T) {
	p := newSegPair(t,
		`CREATE TABLE d(dsk BIGINT, yr BIGINT, moy BIGINT, x BIGINT)`,
		`CREATE TABLE s(dt BIGINT, v BIGINT, item BIGINT)`,
		`INSERT INTO d SELECT value, 1900 + value / 365, 1 + (value % 365) / 31, value FROM generate_series(1, 73049)`,
		`INSERT INTO s SELECT 36000 + (value * 7919) % 2500, value, value % 9000 FROM generate_series(1, 360000)`,
		`ANALYZE`)
	_, rows, err := p.fast(`SELECT v, item, d.x FROM s, d WHERE d.dsk = s.dt AND d.moy = 11 AND d.yr = 1999`)
	if err != nil {
		t.Fatal(err)
	}
	var n, sv, sx int64
	for _, r := range rows {
		n, sv, sx = n+1, sv+r[0].I, sx+r[1].I*r[2].I
	}
	if n != 4464 || sv != 803345760 || sx != 731306494080 {
		t.Errorf("count %d, sum(v) %d, sum(item*x) %d; want 4464, 803345760, 731306494080", n, sv, sx)
	}
}
