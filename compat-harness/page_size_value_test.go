package compat

import "testing"

// Tests PRAGMA page_size across valid and invalid values. Invalid values
// (like "abc" or out-of-range numbers) are silently ignored. The assignment
// takes effect immediately, even before the btree size is fixed.
func TestPageSizeValueSpace(t *testing.T) {
	for _, v := range []string{
		"512", "1024", "2048", "4096", "8192", "65536", // honored
		"131072", "256", "1000", "0", "-1", "4096.0", // out of range / not a power of two
		"ON", "DELETE", "DEFAULT", "KEY", "ABORT", "abc", // sqlite3Atoi -> 0
		"'4096'", "'abc'", "0x1000", "1024junk", // string literal / hex / trailing junk
	} {
		t.Run(v, func(t *testing.T) {
			differ(t, "page-size-fresh/"+v, []string{
				`PRAGMA page_size`,
				`PRAGMA page_size=` + v,
				`PRAGMA page_size`,
				`PRAGMA main.page_size`,
				`CREATE TABLE t(a)`,
				`PRAGMA page_size`,
				`INSERT INTO t VALUES(1)`,
				`SELECT count(*) FROM t`,
				`PRAGMA integrity_check`,
			})
			// ...and the same value against a database that already exists, where
			// BTS_PAGESIZE_FIXED makes the assignment a no-op for the file while
			// still recording db->nextPagesize for a later VACUUM.
			differ(t, "page-size-existing/"+v, []string{
				`CREATE TABLE t(a)`,
				`INSERT INTO t VALUES(1),(2)`,
				`PRAGMA page_size=` + v,
				`PRAGMA page_size`,
				`VACUUM`,
				`PRAGMA page_size`,
				`SELECT count(*) FROM t`,
				`PRAGMA integrity_check`,
			})
		})
	}
}
