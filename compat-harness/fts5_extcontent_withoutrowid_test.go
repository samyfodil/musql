//go:build sqlite_fts5

package compat

import "testing"

// TestFts5ExternalContentWithoutRowidFromSegments: an fts5 external-content
// table whose content table is WITHOUT ROWID, content_rowid naming its key
// (fts5content.test 4.x). Read engine-direct -- the corpus's path, where the
// content table's rows come from segments -- the reader walked an INDEX b-tree
// at the segment table's synthetic root and failed "page N out of range";
// segment rows are read as the row store they are. Replayed through the
// corpus's own runTCLSegment, since the driver path reads the rows elsewhere.
func TestFts5ExternalContentWithoutRowidFromSegments(t *testing.T) {
	stmts := []string{
		`CREATE VIRTUAL TABLE t4 USING fts5(x)`,
		`CREATE TABLE x2(a, "key col" PRIMARY KEY, b, c) WITHOUT ROWID`,
		`INSERT INTO x2 VALUES('a b', 1, 'c d', 'e f')`,
		`INSERT INTO x2 VALUES('x y', -40, 'z z', 'y x')`,
		`CREATE VIRTUAL TABLE t2 USING fts5(a, c, content=x2, content_rowid='key col')`,
		`INSERT INTO t2(t2) VALUES('rebuild')`,
		`SELECT rowid FROM t2`,
		`SELECT rowid FROM t2 WHERE t2 MATCH 'a'`,
		`SELECT rowid, a, c FROM t2 WHERE t2 MATCH 'x'`,
		`SELECT rowid FROM t2 WHERE t2 MATCH 'c'`,
	}
	tally := runTCLSegment(t, "fts5-extcontent-wr", stmts, map[string]int{})
	if tally.unsupported != 0 || tally.wrong != 0 || tally.panics != 0 {
		t.Fatalf("tally %+v: want no declines, wrong answers or panics", tally)
	}
}
