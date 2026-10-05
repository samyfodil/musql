package compat

import (
	"fmt"
	"testing"
)

// TestRowValueInListCollation: a row-value IN list is "IN (VALUES ...)" to C,
// tested against one ephemeral index whose key takes each field's collating
// sequence from the LHS element or else the LAST row's (expr.c:3750-3753), so
// an explicit COLLATE on an earlier row compares nothing. The desugared OR of
// equalities compared each row under its own: over a NOCASE s, "(s, n) IN
// (('b' COLLATE binary, 3), ('c', 4))" matched 1,3,4 where C matches 1,2,3,4.
func TestRowValueInListCollation(t *testing.T) {
	for _, idx := range []string{"", "CREATE INDEX ws ON w(s, n)"} {
		base := []string{
			"CREATE TABLE w(a INTEGER, s TEXT COLLATE NOCASE, n INTEGER, u TEXT)",
			"INSERT INTO w VALUES(1,'b',3,'b'),(2,'B',3,'B'),(3,'c',4,'c'),(4,'C',4,'C'),(5,'b',4,'b')",
		}
		if idx != "" {
			base = append(base, idx)
		}
		for i, w := range []string{
			"(s, n) IN (('b' COLLATE binary, 3), ('c', 4))",
			"(s, n) IN (('b', 3), ('c' COLLATE binary, 4))",
			"(s, n) IN (('B' COLLATE nocase, 3), ('c', 4), ('C' COLLATE binary, 4))",
			"(u, n) IN (('b', 3), ('c' COLLATE nocase, 4))",
			"(u, n) IN (('B' COLLATE nocase, 3), ('c', 4))",
			"(u COLLATE nocase, n) IN (('B' COLLATE binary, 3), ('c', 4))",
			"(s, n) NOT IN (('b' COLLATE binary, 3), ('c', 4))",
		} {
			for _, out := range []string{"SELECT a FROM w WHERE %s ORDER BY a", "SELECT count(*) FROM w WHERE %s"} {
				differ(t, fmt.Sprintf("row-in coll %d idx=%v %s", i, idx != "", out[:12]), append(append([]string{}, base...), fmt.Sprintf(out, w)))
			}
		}
	}
}

// TestRowValueInNestedCollationStaysDeclined: an earlier row's COLLATE below its
// element's top still decides C's key, EP_Collate carrying it up, and cannot be
// taken off the desugared comparison, so the IN declines (rowInKeyCollations).
func TestRowValueInNestedCollationStaysDeclined(t *testing.T) {
	stmts := []string{
		"CREATE TABLE w(a INTEGER, s TEXT COLLATE NOCASE, n INTEGER)",
		"INSERT INTO w VALUES(1,'b',3),(2,'B',3),(3,'c',4)",
		"SELECT count(*) FROM w WHERE (s, n) IN (('b' || '' COLLATE binary, 3), ('c', 4))",
	}
	if got := run(t, "cgo", stmts)[len(stmts)-1]; got["kind"] != "rows" {
		t.Fatalf("test premise wrong: oracle answered %v", got)
	}
	if got := run(t, "musql", stmts)[len(stmts)-1]["kind"]; got != "error" {
		t.Errorf("expected a decline, got kind=%v", got)
	}
}
