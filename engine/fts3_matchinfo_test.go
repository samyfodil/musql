package engine

import (
	"encoding/hex"
	"path/filepath"
	"testing"
)

// fts3MatchinfoInts runs a matchinfo query and decodes the result into uint32s.
func fts3MatchinfoInts(t *testing.T, setup []string, query string) []uint32 {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "f.musq"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range setup {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	p, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, rows, err := p.QueryArgs(query, nil)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if len(rows) != 1 {
		t.Fatalf("%s: got %d rows, want 1", query, len(rows))
	}
	raw, err := hex.DecodeString(string(rows[0][0].S))
	if err != nil {
		t.Fatalf("decoding matchinfo hex: %v", err)
	}
	if len(raw)%4 != 0 {
		t.Fatalf("matchinfo blob length %d not a multiple of 4", len(raw))
	}
	out := make([]uint32, len(raw)/4)
	for i := range out {
		out[i] = uint32(raw[i*4]) | uint32(raw[i*4+1])<<8 | uint32(raw[i*4+2])<<16 | uint32(raw[i*4+3])<<24
	}
	return out
}

var fts3MatchinfoSetup = []string{
	`CREATE VIRTUAL TABLE t0 USING fts3(col0 INTEGER PRIMARY KEY, col1 VARCHAR(8), col2 BINARY, col3 BINARY)`,
	`INSERT INTO t0 VALUES (1, '1234', 'aaaa', 'bbbb')`,
}

// TestFts3MatchinfoYSuppressedByDeadAndSibling pins fts3ExprLHitGather's gate
// (fts3_snippet.c:911, "pExpr->bEof==0 && pExpr->iDocid==p->pCursor->iPrevId"):
// once an AND/NEAR/NOT node's own doclist iterator is permanently exhausted
// (fts3.c:5354's fts3EvalNextRow sets AND/NEAR's bEof = left.bEof||right.bEof),
// sqlite3Fts3ExprLHitGather never descends into it again for ANY row -- even
// for a sibling phrase that DOES occur in this row. Minimized from
// fts3matchinfo2.test's mined MATCH blob: "(s AND 1) OR 1" over a row where
// "s" matches nothing anywhere but "1" matches col0 (the docid). Verified
// against mattn/go-sqlite3 3.53.3: x[phrase1][col0][0] (hits in this row) is
// 1, but y[phrase1][col0] is 0 -- the naive y==x[...][0] shortcut this file
// used before is wrong for exactly this shape.
func TestFts3MatchinfoYSuppressedByDeadAndSibling(t *testing.T) {
	ints := fts3MatchinfoInts(t, fts3MatchinfoSetup,
		`SELECT hex(matchinfo(t0,'yxy')) FROM t0 WHERE t0 MATCH '(s AND 1) OR 1'`)
	// 3 phrases (s, 1, 1) x 4 columns x (y + 3x + y) = 60 ints.
	if len(ints) != 60 {
		t.Fatalf("got %d ints, want 60 (layout assumption broken)", len(ints))
	}
	// phrase1 ("1" inside the dead "s AND 1") at col0: y (index 4), x-local
	// (index 12+4*3=24), and the second y (index 48+4=52).
	if y1 := ints[4]; y1 != 0 {
		t.Errorf("first y-block[phrase1][col0] = %d, want 0 (suppressed by the dead AND sibling)", y1)
	}
	if xLocal := ints[24]; xLocal != 1 {
		t.Errorf("x-block[phrase1][col0][local] = %d, want 1 ('1' does occur in col0 of this row)", xLocal)
	}
	if y2 := ints[52]; y2 != 0 {
		t.Errorf("second y-block[phrase1][col0] = %d, want 0 (suppressed by the dead AND sibling)", y2)
	}
	// phrase2 (the "1" after OR, NOT under the dead AND) must NOT be
	// suppressed: y == x[...][0] == 1 there.
	if y1 := ints[8]; y1 != 1 {
		t.Errorf("first y-block[phrase2][col0] = %d, want 1 (not under the dead AND)", y1)
	}
}

// TestFts3MatchinfoYSuppressedByPartialCoOccurrence pins fts3NodeCandidateSet
// (fts3_search.go): the case TestFts3MatchinfoYSuppressedByDeadAndSibling
// above does NOT cover, where neither AND operand is globally dead (each has
// real hits somewhere) and the AND itself is not globally dead either (it
// really does match docid 1) -- it is dead only PAST docid 1, because bravo's
// doclist has nothing left beyond it. docid 1: alpha bravo. docid 2: alpha
// charlie. "(alpha AND bravo) OR charlie" matches both docids -- docid 1
// through the AND, docid 2 through charlie alone. At docid 2, alpha has a
// real local hit (column a) but the AND node's own candidate set is
// {1}-only, so alpha's y/b bits must still read 0 there. Verified byte-exact
// against mattn/go-sqlite3 3.53.3 (compat-harness/fts3_lhit_gather_test.go
// runs the live oracle differential this pins offline).
func TestFts3MatchinfoYSuppressedByPartialCoOccurrence(t *testing.T) {
	setup := []string{
		`CREATE VIRTUAL TABLE t USING fts3(a,b)`,
		`INSERT INTO t(docid,a,b) VALUES(1,'alpha bravo','x')`,
		`INSERT INTO t(docid,a,b) VALUES(2,'alpha charlie','x')`,
		`INSERT INTO t(docid,a,b) VALUES(3,'delta charlie','x')`,
	}
	ints := fts3MatchinfoInts(t, setup,
		`SELECT hex(matchinfo(t,'xyb')) FROM t WHERE t MATCH '(alpha AND bravo) OR charlie' AND docid=2`)
	// 3 phrases (alpha, bravo, charlie) x 2 columns x 3 (local,gX,gY) for x,
	// + 3 phrases x 2 columns for y, + 3 phrases x 1 word for b = 18+6+3=27.
	if len(ints) != 27 {
		t.Fatalf("got %d ints, want 27 (layout assumption broken)", len(ints))
	}
	if xLocal := ints[0]; xLocal != 1 {
		t.Errorf("x-block[alpha][col a][local] = %d, want 1 (alpha does occur in col a of docid 2)", xLocal)
	}
	if yAlpha := ints[18]; yAlpha != 0 {
		t.Errorf("y-block[alpha][col a] = %d, want 0 (AND's own candidate set is {1}-only, docid 2 not in it)", yAlpha)
	}
	if yCharlie := ints[22]; yCharlie != 1 {
		t.Errorf("y-block[charlie][col a] = %d, want 1 (charlie is a real, unsuppressed match at docid 2)", yCharlie)
	}
	if bAlpha := ints[24]; bAlpha != 0 {
		t.Errorf("b-block[alpha] = %d, want 0 (suppressed, mirrors y)", bAlpha)
	}
	if bCharlie := ints[26]; bCharlie != 1 {
		t.Errorf("b-block[charlie] = %d, want 1 (bit 0 set, col a)", bCharlie)
	}
}
