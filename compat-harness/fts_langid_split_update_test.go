// FTS4 UPDATE with language ID split between delete and insert.
// engine/fts3_write.go's fts3Mutation.mid/midFlushed/noteMutationOp) to also
// carry and compare a langid field, widening the trigger that creates mu.mid
// to any "languageid=" table's UPDATE, not just a docid-changing one -- see
// those files' own doc comments for the full citation trail.
//
// Everything here reads the SHADOW TABLES directly (not just MATCH or
// %_content), because a wrong segment split is a corrupt FILE, not merely a
// wrong query answer -- this engine answers a non-MATCH query straight off
// %_content, so a right row or right MATCH result proves nothing about the
// index bytes underneath (same discipline as fts_langid_split_insert_test.go,
// this fix's INSERT-side sibling).
package compat

import (
	"testing"
)

func TestFtsLangidSplitUpdateDiff(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
		dump  []string
	}{
		// fts4langid.test's own 6.0-6.2 block, verbatim.
		{"fts4langid.test 6.0-6.2 verbatim", []string{
			`CREATE VIRTUAL TABLE vt0 USING fts4(c0, languageid="lid")`,
			`INSERT INTO vt0 VALUES ('a'), ('b')`,
			`BEGIN`,
			`UPDATE vt0 SET lid = 1 WHERE lid=0`,
			`INSERT INTO vt0(vt0) VALUES('integrity-check')`,
			`COMMIT`,
			`INSERT INTO vt0(vt0) VALUES('integrity-check')`,
		}, ftsLangidSplitDump("vt0", "c0, lid")},

		// The same shape without the enclosing transaction (autocommit): each
		// of the UPDATE's 4 sub-operations still flushes into its own
		// segment (there being no accumulating transaction segment to fold
		// the LAST one into instead), and 'optimize' afterwards must still
		// see a clean, mergeable index across both languages.
		{"autocommit, followed by optimize", []string{
			`CREATE VIRTUAL TABLE vt0 USING fts4(c0, languageid="lid")`,
			`INSERT INTO vt0 VALUES ('a'), ('b')`,
			`UPDATE vt0 SET lid = 1 WHERE lid=0`,
			`INSERT INTO vt0(vt0) VALUES('integrity-check')`,
			`INSERT INTO vt0(vt0) VALUES('optimize')`,
			`INSERT INTO vt0(vt0) VALUES('integrity-check')`,
		}, append([]string{
			`SELECT docid FROM vt0 WHERE vt0 MATCH 'a' AND lid=1`,
			`SELECT docid FROM vt0 WHERE vt0 MATCH 'b' AND lid=1`,
			`SELECT docid FROM vt0 WHERE vt0 MATCH 'a' AND lid=0`,
		}, ftsLangidSplitDump("vt0", "c0, lid")...)},

		// A same-language UPDATE (the overwhelmingly common case) must be
		// completely unaffected by widening mu.mid's trigger to every
		// "languageid=" table's UPDATE: the langid clause never fires when
		// every operation shares one language, so this must still write
		// exactly ONE segment, matching pre-fix behavior byte for byte.
		{"same-language UPDATE is unaffected (no split)", []string{
			`CREATE VIRTUAL TABLE vt0 USING fts4(c0, languageid="lid")`,
			`INSERT INTO vt0(c0, lid) VALUES ('a', 5), ('b', 5)`,
			`UPDATE vt0 SET c0 = c0 || ' x' WHERE lid=5`,
		}, ftsLangidSplitDump("vt0", "c0, lid")},

		// A language-tracking UPDATE that is NOT the first write of its
		// transaction must still flush at the right point against what the
		// transaction is already accumulating -- fts3TxnPeekLastLangid's own
		// seeding, symmetric to fts3TxnPeekLastDocid's pre-existing one for a
		// docid-changing UPDATE. Two single-row, same-language (7) INSERTs
		// first (which alone would leave ONE accumulating segment for
		// language 7), then a same-language-7 UPDATE that changes one row's
		// language to 9 mid-statement.
		{"language-tracking UPDATE is not the first write of its transaction", []string{
			`CREATE VIRTUAL TABLE vt0 USING fts4(c0, languageid="lid")`,
			`BEGIN`,
			`INSERT INTO vt0(c0, lid) VALUES ('un', 7)`,
			`INSERT INTO vt0(c0, lid) VALUES ('deux', 7)`,
			`UPDATE vt0 SET lid = 9 WHERE c0 = 'deux'`,
			`INSERT INTO vt0(vt0) VALUES('integrity-check')`,
			`COMMIT`,
			`INSERT INTO vt0(vt0) VALUES('integrity-check')`,
		}, ftsLangidSplitDump("vt0", "c0, lid")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			differAllAccepted(t, c.name, append(append([]string{}, c.stmts...), c.dump...), len(c.stmts))
		})
	}
}
