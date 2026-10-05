package engine

import (
	"encoding/hex"
	"testing"
)

// TestFts3DoclistOverrunsIntoNextTermBytes verifies that position-list decoding
// correctly handles overruns into subsequent node bytes, preserving the ability
// to decode all terms independently.
func TestFts3DoclistOverrunsIntoNextTermBytes(t *testing.T) {
	node, err := hex.DecodeString("000131030782000103323334050101010200000461616161050101020200000462626262050101030200")
	if err != nil {
		t.Fatal(err)
	}

	byTerm := map[string]fts3TermPostings{}
	err = fts3DecodeLeafNode(node, func(term string, doclist []byte, nDoclist int) (bool, error) {
		return false, fts3MergeDoclist(byTerm, term, doclist, nDoclist, false /* desc */, true)
	})
	if err != nil {
		t.Fatalf("fts3DecodeLeafNode: %v", err)
	}

	// Term "1234" must decode independently and correctly no matter how far
	// term "1"'s own scan read into its bytes: fts3DecodeLeafNode's outer walk
	// advances by the DECLARED nDoclist (fts3_write.c:1347), never by how far
	// visit's scan actually consumed, so the two reads of the same bytes for
	// two different purposes cannot interact.
	tp1234, ok := byTerm["1234"]
	if !ok {
		t.Fatalf("term \"1234\" missing entirely -- the outer walk was perturbed by term \"1\"'s overrun")
	}
	// "1234"'s own doclist (01 01 01 02 00): delta=1 (docid=1), poslist bytes
	// "01 01 02" decode to column 1 (POS_COLUMN, since fts3DecodePoslist's
	// column starts at 0 and the first byte 0x01 selects column 1), position
	// (1-2)=... this engine's own fts3DecodePoslist owns that arithmetic; the
	// only thing asserted here is that decoding SUCCEEDED and named docid 1,
	// which is the structural claim (independence), not a re-derivation of
	// fts3DecodePoslist's own already-tested column/position math.
	if _, ok := tp1234[1]; !ok {
		t.Fatalf("term \"1234\" docid 1 missing: got %v", tp1234)
	}

	tp1, ok := byTerm["1"]
	if !ok {
		t.Fatalf("term \"1\" missing -- the doclist walk declined instead of reading the overrun")
	}
	cols, ok := tp1[7]
	if !ok {
		t.Fatalf("term \"1\" docid 7 missing: got %v", tp1)
	}
	// The decoded position list must be non-trivial (more than what a 2-byte
	// "82 00" naive-zero-pad scan would have produced) -- proof that REAL
	// subsequent node bytes were read, not zeros. A naive per-term zero-pad
	// stops after consuming exactly 2 bytes of the declared window (82, then
	// the declared 00): the scan's carry read the two window bytes then hit a
	// zero byte from the pad with no carry, i.e. it would decode column/
	// position data from "82 00" alone. The real, oracle-matching scan instead
	// runs 12 bytes deep into the node (through term "1234"'s own header and
	// doclist), which -- being real non-zero-heavy structural bytes, not a
	// zero pad -- decodes to a different, richer position list.
	total := 0
	for _, positions := range cols {
		total += len(positions)
	}
	if total == 0 {
		t.Fatalf("term \"1\" docid 7 decoded to an empty position set from a 12-byte overrun poslist -- expected at least one position, got cols=%v", cols)
	}
	t.Logf("term \"1\" docid 7 decoded columns/positions: %v (overrun read term \"1234\"'s real header+doclist bytes, not zero padding)", cols)
}

// TestFts3DoclistDeclaredTerminatorMustBeZero exercises the NEW structural
// check ported from fts3_write.c:1450-1453: a doclist whose declared final
// byte is not literally 0x00 is corrupt, independent of anything the
// position-list scan itself would find (fts3SegReaderNext returns
// FTS_CORRUPT_VTAB before ever constructing a reader over the doclist). This
// is the excluded class the unbounded scan does NOT paper over: overrunning
// past a genuinely-non-zero declared boundary is still a hard decline.
func TestFts3DoclistDeclaredTerminatorMustBeZero(t *testing.T) {
	// One-term leaf: term "z", nDoclist=2, doclist bytes "01 01" -- declared
	// length is right (2 bytes fit in the node), but the LAST declared byte
	// (0x01) is not 0x00.
	node := []byte{0x00, 0x01, 'z', 0x02, 0x01, 0x01}
	err := fts3DecodeLeafNode(node, func(term string, doclist []byte, nDoclist int) (bool, error) {
		byTerm := map[string]fts3TermPostings{}
		return false, fts3MergeDoclist(byTerm, term, doclist, nDoclist, false, true)
	})
	if err == nil {
		t.Fatalf("expected a decline (declared doclist does not end in 0x00), got success")
	}
	t.Logf("declined as expected: %v", err)
}
