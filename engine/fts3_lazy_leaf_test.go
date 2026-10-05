package engine

import (
	"fmt"
	"testing"
)

var fts3LazyLeafTestNode = []byte{
	0x00,
	0x02, 'a', 'a',
	0x04,
	0x01, 0x02, 0x00, 0x00,
	0x00,
	0x02, 'b', 'b',
	0x05,
}

func TestFts3LazyLeafSkipsUnneededCorruptTerm(t *testing.T) {
	wanted := &fts3WantedTerms{exact: []string{"aa"}}
	byTerm := map[string]fts3TermPostings{}
	seen := map[string]bool{}
	sw := wanted.openForSegment()
	err := fts3DecodeLeafNode(fts3LazyLeafTestNode, func(term string, doclist []byte, nDoclist int) (bool, error) {
		seen[term] = true
		if sw.consume(term) {
			if err := fts3MergeDoclist(byTerm, term, doclist, nDoclist, false, true); err != nil {
				return false, err
			}
		}
		return sw.resolved(), nil
	})
	if err != nil {
		t.Fatalf("wanted={aa}: expected success (bb is never reached), got: %v", err)
	}
	if seen["bb"] {
		t.Fatalf("wanted={aa}: term \"bb\" was visited at all -- the lazy skip did not fire")
	}
	if _, ok := byTerm["aa"]; !ok {
		t.Fatalf("wanted={aa}: term \"aa\" was not decoded: %v", byTerm)
	}
}

// TestFts3LazyLeafStillValidatesTermsOnThePathToItsTarget is the soundness
// counterpart: wanting a term that does NOT come first (or does not exist at
// all, but sorts past the node's only corrupt term) must NOT let the walk
// "jump to" it -- C fts3's positioning do-while (fts3_write.c:2790-2801)
// structurally parses every term from a node's start up to and including the
// first one that is lexically at-or-past the target, corrupt or not. Both
// sub-cases here must see the SAME "bb" corruption, because both require
// walking through "aa" and then attempting "bb".
func TestFts3LazyLeafStillValidatesTermsOnThePathToItsTarget(t *testing.T) {
	cases := []struct {
		name   string
		wanted *fts3WantedTerms
	}{
		// "bb" itself is the target: present in the node, but its own header
		// is what is corrupt, so walking TO it is what fails.
		{"exact target is the corrupt term", &fts3WantedTerms{exact: []string{"bb"}}},
		// "cc" is not present anywhere in this node, but it sorts after
		// "bb" -- proving the walk cannot determine "not found" without
		// first structurally passing every term up to where it WOULD sort,
		// which still means attempting "bb"'s corrupt header.
		{"absent target sorting past the corrupt term", &fts3WantedTerms{exact: []string{"cc"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			byTerm := map[string]fts3TermPostings{}
			sw := tc.wanted.openForSegment()
			err := fts3DecodeLeafNode(fts3LazyLeafTestNode, func(term string, doclist []byte, nDoclist int) (bool, error) {
				if sw.consume(term) {
					if err := fts3MergeDoclist(byTerm, term, doclist, nDoclist, false, true); err != nil {
						return false, err
					}
				} else if !fts3DoclistTerminatorOK(doclist, nDoclist) {
					return false, fmt.Errorf("fts3: corrupt doclist (declared doclist does not end in 0x00)")
				}
				return sw.resolved(), nil
			})
			if err == nil {
				t.Fatalf("expected a decline (term \"bb\"'s header genuinely overruns the node), got success: byTerm=%v", byTerm)
			}
			t.Logf("declined as required: %v", err)
		})
	}
}

// TestFts3LazyLeafPrefixReadsItsOwnClosingTerm is the prefix-specific
// soundness case: a prefix token does NOT get the bLookup shortcut
// (fts3.c:3156 sets bLookup only for a non-prefix token in this engine's
// no-prefix-index configuration -- see fts3LoadIndex's doc comment). Once it
// matches "aa", C fts3 must still read ONE further term to discover the
// range has closed (fts3_write.c:2938's continuation call, checked against
// fts3_write.c:2959-2966) -- and that closing term is "bb", the corrupt one.
// So unlike the exact-match case above, a prefix match on the node's FIRST
// term must NOT let the walk stop early.
func TestFts3LazyLeafPrefixReadsItsOwnClosingTerm(t *testing.T) {
	wanted := &fts3WantedTerms{prefixes: []string{"a"}}
	byTerm := map[string]fts3TermPostings{}
	sw := wanted.openForSegment()
	err := fts3DecodeLeafNode(fts3LazyLeafTestNode, func(term string, doclist []byte, nDoclist int) (bool, error) {
		if sw.consume(term) {
			if err := fts3MergeDoclist(byTerm, term, doclist, nDoclist, false, true); err != nil {
				return false, err
			}
		} else if !fts3DoclistTerminatorOK(doclist, nDoclist) {
			return false, fmt.Errorf("fts3: corrupt doclist (declared doclist does not end in 0x00)")
		}
		return sw.resolved(), nil
	})
	if err == nil {
		t.Fatalf("prefix \"a\": expected a decline (the range's closing term \"bb\" is the corrupt one), got success: byTerm=%v", byTerm)
	}
	if _, ok := byTerm["aa"]; !ok {
		t.Fatalf("prefix \"a\": term \"aa\" should have been decoded before the closing-term read failed: %v", byTerm)
	}
	t.Logf("declined as required, after decoding the matching term first: %v", err)
}

// TestFts3LazyLeafNilWantedStaysEager is the eager-caller regression guard:
// fts3IndexOnlyDocids and fts3AuxRows pass wanted=nil and must see the
// ORIGINAL whole-node behavior, unaffected by this change -- including still
// declining on this node's corrupt second term.
func TestFts3LazyLeafNilWantedStaysEager(t *testing.T) {
	byTerm := map[string]fts3TermPostings{}
	err := fts3DecodeLeafNode(fts3LazyLeafTestNode, func(term string, doclist []byte, nDoclist int) (bool, error) {
		return false, fts3MergeDoclist(byTerm, term, doclist, nDoclist, false, true)
	})
	if err == nil {
		t.Fatalf("wanted=nil: expected the eager walk to still decline on term \"bb\", got success: byTerm=%v", byTerm)
	}
	if _, ok := byTerm["aa"]; !ok {
		t.Fatalf("wanted=nil: term \"aa\" should have decoded before \"bb\" was reached: %v", byTerm)
	}
	t.Logf("declined as required (eager path unaffected): %v", err)
}
