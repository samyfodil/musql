package compat

import "testing"

// TestUTF16SurrogatePairsCompare pins TEXT comparison in a UTF-16 database
// against the oracle for characters past the BMP: 😀 and 😁 share their high
// surrogate, so only the low halves tell them apart, in every comparison,
// grouping and dedup.
func TestUTF16SurrogatePairsCompare(t *testing.T) {
	for _, enc := range []string{"UTF-16le", "UTF-16be"} {
		setup := []string{
			"PRAGMA encoding='" + enc + "'",
			"CREATE TABLE e(x)",
			"INSERT INTO e VALUES('😀'),('😁'),('😀'),('😀a'),('😁a'),('z'),('é')",
		}
		for _, q := range []string{
			"SELECT '😀' = '😁', '😀' < '😁', '😁' > '😀a', char(55357, 56832) = '😀', length(char(55357, 56832))",
			"SELECT count(DISTINCT x), count(*) FROM e",
			"SELECT x, count(*) FROM e GROUP BY x ORDER BY x",
			"SELECT DISTINCT x FROM e ORDER BY x DESC",
			"SELECT count(*) FROM (SELECT x FROM e UNION SELECT x FROM e)",
			"SELECT min(x), max(x) FROM e",
			"SELECT x FROM e WHERE x = '😁'",
		} {
			aggOrderCheck(t, aggOrderCase{enc + ": " + q, append(append([]string(nil), setup...), q), true})
		}
	}
}
