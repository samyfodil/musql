// This file tests fts4 MATCH queries over content tables that don't need to
// be read (e.g., content=nosuchtable, or CONTENTLESS mode).
package compat

import "testing"

func TestFts3ContentUnneededMatchOverUnreadableContentTable(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
	}{
		// fts4content.test 7.1.1-7.1.2 verbatim.
		{"content=nosuchtable, MATCH with a bare docid projection", []string{
			`CREATE VIRTUAL TABLE ft8 USING fts4(content=nosuchtable, x)`,
			`INSERT INTO ft8(docid, x) VALUES(13, 'U O N X G')`,
			`INSERT INTO ft8(docid, x) VALUES(14, 'C J J U B')`,
			`INSERT INTO ft8(docid, x) VALUES(15, 'N J Y G X')`,
			`INSERT INTO ft8(docid, x) VALUES(16, 'R Y D O R')`,
			`INSERT INTO ft8(docid, x) VALUES(17, 'I Y T Q O')`,
			`SELECT docid FROM ft8 WHERE ft8 MATCH 'N'`,
			`SELECT count(*) FROM ft8 WHERE ft8 MATCH 'N'`,
			`SELECT rowid FROM ft8 WHERE ft8 MATCH 'N'`,
			// No MATCH at all: the row source is the (nonexistent) content
			// table alone, exactly as before this fix -- still an error.
			`SELECT docid FROM ft8`,
		}},

		// fts4content.test 7.2.1-7.2.4 verbatim: CONTENTLESS (content=""), plus
		// the one shape that must STILL decline (a real column read, "SELECT
		// *") to confirm the fix is not over-broad.
		{"contentless, MATCH with a bare docid projection, star-select still declines", []string{
			`CREATE VIRTUAL TABLE ft9 USING fts4(content=, x)`,
			`INSERT INTO ft9(docid, x) VALUES(13, 'U O N X G')`,
			`INSERT INTO ft9(docid, x) VALUES(14, 'C J J U B')`,
			`INSERT INTO ft9(docid, x) VALUES(15, 'N J Y G X')`,
			`INSERT INTO ft9(docid, x) VALUES(16, 'R Y D O R')`,
			`INSERT INTO ft9(docid, x) VALUES(17, 'I Y T Q O')`,
			`SELECT docid FROM ft9 WHERE ft9 MATCH 'N'`,
			`SELECT name FROM sqlite_master WHERE name LIKE 'ft9_%' ORDER BY name`,
			`SELECT * FROM ft9 WHERE ft9 MATCH 'N'`,
			`SELECT x FROM ft9 WHERE ft9 MATCH 'N'`,
		}},

		// A WITHOUT ROWID content table gets the identical benefit for free
		// (fts3WithoutRowidContentErr is just another error fts3ContentSourceOf
		// can return, swallowed the same way) -- this file's own doc comment
		// notes it explicitly.
		{"WITHOUT ROWID content table, MATCH with a bare docid projection", []string{
			`CREATE TABLE wr(a TEXT PRIMARY KEY, b) WITHOUT ROWID`,
			`INSERT INTO wr VALUES('k1','U O N X G')`,
			`INSERT INTO wr VALUES('k2','C J J U B')`,
			`CREATE VIRTUAL TABLE fw USING fts4(content=wr, b)`,
			`INSERT INTO fw(docid, b) VALUES(1, 'U O N X G')`,
			`INSERT INTO fw(docid, b) VALUES(2, 'C J J U B')`,
			`SELECT docid FROM fw WHERE fw MATCH 'N'`,
			`SELECT count(*) FROM fw WHERE fw MATCH 'N'`,
			// A real column read still fails exactly as fts3WithoutRowidContentErr
			// documents.
			`SELECT * FROM fw WHERE fw MATCH 'N'`,
			`SELECT * FROM fw`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}
