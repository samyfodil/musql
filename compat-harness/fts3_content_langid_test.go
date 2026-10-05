// This file tests FTS4 "content=" and "languageid=" module options used together.
package compat

import "testing"

func TestFts3ContentLanguageidDiff(t *testing.T) {
	cases := []struct {
		name      string
		stmts     []string
		nAccepted int
	}{
		// The mined shape itself: columns DERIVED from the content table have
		// the langid column (l) removed, leaving x and y -- not l, x, y.
		{"derived columns exclude the langid column", []string{
			`CREATE TABLE t3_data(l, x, y)`,
			`INSERT INTO t3_data(rowid, l, x, y) VALUES(1, 3, 'alpha beta', 'gamma')`,
			`INSERT INTO t3_data(rowid, l, x, y) VALUES(2, 7, 'delta', 'epsilon')`,
			`CREATE VIRTUAL TABLE t2 USING fts4(content=t3_data, languageid=l)`,
			`SELECT type, name, sql FROM sqlite_master WHERE name LIKE 't2%' ORDER BY name`,
			`SELECT * FROM t2`,
			// The langid value comes back off the CONTENT table's own column,
			// not a %_content slot that does not exist for this table.
			`SELECT docid, l, x, y FROM t2 ORDER BY docid`,
			`SELECT rowid, x, y, l FROM t2 ORDER BY rowid`,
		}, 8},

		// A content table whose own column does NOT share the langid's name
		// (t8c has no "langid" column) needs no exclusion: both its columns
		// are kept.
		{"no exclusion when no column of that name exists", []string{
			`CREATE TABLE t8c(a, b)`,
			`CREATE VIRTUAL TABLE t8 USING fts4(content=t8c, languageid=langid)`,
			`SELECT type, name, sql FROM sqlite_master WHERE name LIKE 't8%' ORDER BY name`,
		}, 3},

		// Explicit columns need no derivation at all, so the content table is
		// not even consulted for the CREATE -- "nosuchtable" not existing is
		// not an error until something actually reads it.
		{"explicit columns skip content-table derivation entirely", []string{
			`CREATE VIRTUAL TABLE t9 USING fts4(x, y, languageid=l, content=nosuchtable)`,
			`SELECT type, name, sql FROM sqlite_master WHERE name LIKE 't9%' ORDER BY name`,
		}, 2},

		// DELETE/UPDATE against such a table read the row's language off the
		// SAME content-table column the CREATE derived (or, for explicit
		// columns, the declared one) -- fts3_write.go's mutation path, which
		// must place delete markers and re-inserted terms at the RIGHT
		// per-language %_segdir level rather than defaulting to language 0.
		{"DELETE/UPDATE read langid from the content table", []string{
			`CREATE TABLE t3_data(l, x, y)`,
			`INSERT INTO t3_data(rowid, l, x, y) VALUES(1, 2, 'alpha beta', 'gamma')`,
			`INSERT INTO t3_data(rowid, l, x, y) VALUES(2, 2, 'alpha two', 'delta')`,
			`INSERT INTO t3_data(rowid, l, x, y) VALUES(3, 0, 'zeta', 'eta')`,
			`CREATE VIRTUAL TABLE t2 USING fts4(content=t3_data, languageid=l)`,
			`INSERT INTO t2(docid,x,y,l) VALUES(1,'alpha beta','gamma',2)`,
			`INSERT INTO t2(docid,x,y,l) VALUES(2,'alpha two','delta',2)`,
			`INSERT INTO t2(docid,x,y,l) VALUES(3,'zeta','eta',0)`,
			`SELECT level, idx, quote(root) FROM t2_segdir ORDER BY level, idx`,
			`DELETE FROM t2 WHERE docid=1`,
			`SELECT level, idx, quote(root) FROM t2_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t2_stat ORDER BY id`,
			`UPDATE t2 SET x='omega' WHERE docid=2`,
			`SELECT level, idx, quote(root) FROM t2_segdir ORDER BY level, idx`,
			`SELECT id, quote(value) FROM t2_stat ORDER BY id`,
		}, 8},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, c.stmts, c.nAccepted)
		})
	}
}

// TestFts3ContentLanguageidMatchMissingColumn closes fts4langid.test#3.4's own
// mined statement, and pins the language-partition fix fts3ContentMatchLangid
// needed alongside it (fts3_content.go). See this file's package comment.
func TestFts3ContentLanguageidMatchMissingColumn(t *testing.T) {
	cases := []struct {
		name      string
		stmts     []string
		nAccepted int
	}{
		// The mined shape itself. t8c has no "langid" column, and the INSERT
		// never touches it (content= tables never write to content), so
		// docid -1 is an INDEX-ONLY row: fts3IndexOnlyDocids' all-NULL row,
		// whose langid slot used to have nothing to read and hard-errored.
		{"MATCH answers the langid column from its own resolved language", []string{
			`CREATE TABLE t8c(a, b)`,
			`CREATE VIRTUAL TABLE t8 USING fts4(content=t8c, languageid=langid)`,
			`INSERT INTO t8(docid, a, b) VALUES(-1, 'one two three', 'x y z')`,
			`SELECT docid FROM t8 WHERE t8 MATCH 'one x' AND langid=0`,
		}, 4},

		// The latent bug this fix's own implementation caught along the way
		// (fts3IndexOnlyDocids used to hardcode language 0's %_segdir level
		// range for EVERY call): two index-only docids at DIFFERENT
		// languages, each holding a term the other does not. Before the
		// langid parameter existed, a langid=3 MATCH would have read
		// language 0's index range and answered wrongly (missing docid -2,
		// or worse, cross-contaminating with language 0's own terms).
		{"a nonzero-language MATCH reads its OWN language's index, not language 0's", []string{
			`CREATE TABLE t8c(a, b)`,
			`CREATE VIRTUAL TABLE t8 USING fts4(content=t8c, languageid=langid)`,
			`INSERT INTO t8(docid, a, b, langid) VALUES(-1, 'one two three', 'x y z', 0)`,
			`INSERT INTO t8(docid, a, b, langid) VALUES(-2, 'quattro cinque', 'sei sette', 3)`,
			`SELECT docid FROM t8 WHERE t8 MATCH 'quattro' AND langid=3`,
			`SELECT docid FROM t8 WHERE t8 MATCH 'quattro' AND langid=0`,
			`SELECT docid FROM t8 WHERE t8 MATCH 'one' AND langid=0`,
			`SELECT docid FROM t8 WHERE t8 MATCH 'one' AND langid=3`,
		}, 8},

		// A SECOND latent bug, found the same way as the level-range one
		// above (by testing a shape the mined statement itself did not
		// exercise): when the content table DOES have a column of the
		// declared name, a MATCH-driven read must still answer langid from
		// the MATCH's OWN resolved language, not content's stored value for
		// that row -- fts3ColumnMethod's "if (pCsr->pExpr)" check
		// (fts3.c:3462-3499) fires before any content-seeking branch, full
		// stop, so content's own column is never even consulted for it under
		// a MATCH. Content's row here stores langid=99, deliberately
		// disagreeing with the language actually searched (0); a/b DO come
		// from content (proving this is a content-BACKED row, not an
		// index-only stand-in -- the previous case's shape), while langid
		// does not.
		{"MATCH overrides content's OWN langid column too, not just a missing one", []string{
			`CREATE TABLE t8c(a, b, langid)`,
			`INSERT INTO t8c(rowid, a, b, langid) VALUES(1, 'placeholder-content', 'placeholder', 99)`,
			`CREATE VIRTUAL TABLE t8 USING fts4(content=t8c, languageid=langid)`,
			`INSERT INTO t8(docid, a, b, langid) VALUES(1, 'one two three', 'x y z', 0)`,
			`SELECT docid, a, b, langid FROM t8 WHERE t8 MATCH 'one' AND langid=0`,
			`SELECT docid, a, b, langid FROM t8`,
		}, 6},

		// Excluded class this fix's soundness depends on: a NON-MATCH read
		// still gets fts3ProjectContent's original hard error, because
		// fts3ContentMatchLangid finds no MATCH on this table and leaves
		// matchLangid nil -- C fts3 agrees once the content table holds
		// an actual row (fts3CursorSeek's own per-row content lookup then
		// genuinely fails to find the "langid" column, "SQL logic error" on
		// the oracle too, verified directly): plain agreement is the right
		// assertion, not a one-sided "still declines" check.
		{"a non-MATCH read still hard-errors on a missing column", []string{
			`CREATE TABLE t8c(a, b)`,
			`INSERT INTO t8c(rowid, a, b) VALUES(7, 'zzz', 'www')`,
			`CREATE VIRTUAL TABLE t8 USING fts4(content=t8c, languageid=langid)`,
			`SELECT docid FROM t8`,
		}, 3},

		// Same excluded class, the WRITE path: fts3ContentSourceIn
		// (DELETE/UPDATE/'rebuild') always passes matchLangid nil and keeps
		// erroring too -- C fts3's own 'rebuild' reads languageid off
		// content unconditionally (fts3ReadExprList's zContentTbl!=0
		// branch), never from a cursor's iLangid, so it cannot use this
		// fix's MATCH-driven exception either. Verified against the oracle:
		// DELETE/UPDATE/rebuild are each "SQL logic error" there too.
		{"DELETE/UPDATE/rebuild still hard-error on a missing column", []string{
			`CREATE TABLE t8c(a, b)`,
			`INSERT INTO t8c(rowid, a, b) VALUES(7, 'zzz', 'www')`,
			`CREATE VIRTUAL TABLE t8 USING fts4(content=t8c, languageid=langid)`,
			`DELETE FROM t8 WHERE docid=7`,
			`UPDATE t8 SET a='new' WHERE docid=7`,
			`INSERT INTO t8(t8) VALUES('rebuild')`,
		}, 3},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, c.stmts, c.nAccepted)
		})
	}
}
