package engine

import "testing"

// TestFts3PoslistByteLengthMatchSentinel exercises fts3MergeDoclist's
// allowByteLengthMatch on fts3corrupt4.test's "excepteur" doclist: docid 7 has
// position bytes "01", a bare POS_COLUMN cut off before its column number, so
// it decodes to zero positions. C SQLite matches it for 'e*' but not for 'a:e*'.
func TestFts3PoslistByteLengthMatchSentinel(t *testing.T) {
	doclist := []byte{0x07, 0x01, 0x00} // delta=7, poslist="01", terminator
	// fts3MergeDoclist expects input padded past nDoclist, as fts3DecodeLeafNode
	// provides.
	padded := make([]byte, len(doclist)+fts3NodePadding)
	copy(padded, doclist)

	t.Run("write path declines", func(t *testing.T) {
		byTerm := map[string]fts3TermPostings{}
		err := fts3MergeDoclist(byTerm, "excepteur", padded, len(doclist), false, false)
		if err == nil {
			t.Fatalf("allowByteLengthMatch=false (the write/merge path) must still decline this doclist -- fts3PendingTermsFrom has no way to re-encode a position it cannot decode, and silently accepting it here would let a segment MERGE/optimize/rebuild silently DROP the document instead of refusing to touch it")
		}
	})

	t.Run("read path stores the sentinel", func(t *testing.T) {
		byTerm := map[string]fts3TermPostings{}
		if err := fts3MergeDoclist(byTerm, "excepteur", padded, len(doclist), false, true); err != nil {
			t.Fatalf("allowByteLengthMatch=true (the read/query path): %v", err)
		}
		tp, ok := byTerm["excepteur"]
		if !ok {
			t.Fatalf("term \"excepteur\" missing entirely")
		}
		cols, ok := tp[7]
		if !ok {
			t.Fatalf("docid 7 missing: got %v", tp)
		}
		if len(cols) != 1 {
			t.Fatalf("want exactly one sentinel column entry, got %v", cols)
		}
		ps, ok := cols[fts3ByteLengthMatchCol]
		if !ok {
			t.Fatalf("sentinel key fts3ByteLengthMatchCol missing: got %v", cols)
		}
		if len(ps) != 0 {
			t.Fatalf("sentinel entry must carry no real position, got %v", ps)
		}

		// ix.lookup: an unfiltered (no column restriction) lookup must still
		// report docid 7 as a match...
		ix := &fts3Index{terms: []string{"excepteur"}, post: []fts3TermPostings{tp}}
		nCol := 1
		unfiltered := ix.lookup(fts3QueryToken{term: "excepteur"}, nCol /* no filter */, nCol)
		if _, ok := unfiltered[7]; !ok {
			t.Fatalf("unfiltered lookup must include docid 7 (a byte-length match), got %v", unfiltered)
		}
		// ...but a column filter must exclude it: there is no real position.
		filtered := ix.lookup(fts3QueryToken{term: "excepteur"}, 0 /* column 0 only */, nCol)
		if _, ok := filtered[7]; ok {
			t.Fatalf("column-filtered lookup must exclude docid 7 (no real column data), got %v", filtered)
		}
	})
}
