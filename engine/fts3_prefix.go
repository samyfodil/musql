// Package engine implements fts4's "prefix=" module option: PREFIX INDEXES
// alongside the term index for fast prefix seeks ("abc*" queries). All indexes
// share the same %_segdir/%_segments pair, partitioned by level number.
// Prefix lengths are byte counts, not character counts (may split UTF-8).
// The list is positional, zero-dropped, not sorted or deduplicated.
package engine

import (
	"fmt"
	"strings"
)

// fts3SegdirMaxLevel is the level space partition size per index (1024).
const fts3SegdirMaxLevel = 1024

// fts3MaxPrefixLen is the ceiling a prefix length saturates at (10000000).
const fts3MaxPrefixLen = 10000000

// fts3PrefixParameter parses "prefix=" option value into byte lengths per index.
// It is a comma-separated list of decimal integers; zeroes are dropped. The
// parsing is permissive: non-digit characters are skipped after each number,
// which allows "1.5,2" to parse as [1, 5].
func fts3PrefixParameter(val string) ([]int, error) {
	if val == "" {
		return nil, nil
	}
	var out []int
	p := 0
	for k := strings.Count(val, ",") + 1; k > 0; k-- {
		start, n := p, 0
		for p < len(val) && val[p] >= '0' && val[p] <= '9' {
			n = n*10 + int(val[p]-'0')
			if n > fts3MaxPrefixLen {
				n = fts3MaxPrefixLen
			}
			p++
		}
		if p == start {
			return nil, fmt.Errorf("error parsing prefix parameter: %s", val)
		}
		if n != 0 {
			out = append(out, n)
		}
		// Skip separator character.
		p++
	}
	return out, nil
}

// fts3PendingSet is one flush's pending terms for EVERY index of the table:
// slot 0 is the term index and slot i>0 the prefix index of length
// prefixes[i-1]. Each slot is an ordinary fts3PendingTerms, so a prefix index's
// segment goes through the same encoder, the same spill and the same merge.
type fts3PendingSet struct {
	prefixes []int
	pts      []*fts3PendingTerms
	// langid is the LANGUAGE these terms belong to, which shifts every index's
	// %_segdir level by langid*nIndex*1024 (fts3_langid.go). 0 for a table
	// without "languageid=", where it changes nothing.
	langid int64
	// desc is the table's "order=desc" bit, carried so a merge this flush
	// cascades into encodes the same way (fts3_index.go).
	desc bool
}

func newFts3PendingSet(prefixes []int, desc bool) *fts3PendingSet {
	s := &fts3PendingSet{prefixes: prefixes, pts: make([]*fts3PendingTerms, len(prefixes)+1), desc: desc}
	for i := range s.pts {
		s.pts[i] = newFts3PendingTerms(desc)
	}
	return s
}

// levelOf is getAbsoluteLevel(p, s.langid, i, 0) for index i of this set.
func (s *fts3PendingSet) levelOf(i int) int64 {
	return fts3LevelBase(s.langid, len(s.pts), i)
}

// add records term at (docid, col, pos) in the term index and in every prefix
// index the term is long enough for -- see this file's comment for why the
// truncation is by BYTES.
func (s *fts3PendingSet) add(term string, docid int64, col, pos int) {
	s.pts[0].add(term, docid, col, pos)
	for i, n := range s.prefixes {
		if len(term) < n {
			continue
		}
		s.pts[i+1].add(term[:n], docid, col, pos)
	}
}

// addDelete records a delete marker in every index the term is in. Verified
// against the oracle: deleting a row from "fts4(a,b,prefix=2)" writes a marker
// segment at level 0 AND one at level 1024, with the same doclist shape.
func (s *fts3PendingSet) addDelete(term string, docid int64) {
	s.pts[0].addDelete(term, docid)
	for i, n := range s.prefixes {
		if len(term) < n {
			continue
		}
		s.pts[i+1].addDelete(term[:n], docid)
	}
}

// terms is the TERM index's pending doclists -- the only ones anything outside
// this file inspects (a prefix index is derived from them).
func (s *fts3PendingSet) terms() map[string]*fts3Doclist { return s.pts[0].terms }

// merge folds another flush's pending terms into this one, index by index --
// what a transaction's accumulating segment does after every statement
// (fts3_txn.go).
func (s *fts3PendingSet) merge(other *fts3PendingSet) {
	for i := range s.pts {
		if i < len(other.pts) {
			s.pts[i].merge(other.pts[i])
		}
	}
}

// fts3SameSegdirIndex reports whether two %_segdir levels belong to the same
// index, i.e. to the same 1024-level block.
func fts3SameSegdirIndex(a, b int64) bool {
	return a/fts3SegdirMaxLevel == b/fts3SegdirMaxLevel
}

// fts3IsTermIndexLevel reports whether a %_segdir level belongs to the TERM
// index (index 0) of the language whose term index starts at base, which is the
// only one the read path may look at: a prefix index holds truncated terms, so
// folding it into the term index would make "MATCH 'al'" find a row whose only
// token is "alpha", and another LANGUAGE's index is a different index entirely
// (fts3_langid.go). base is 0 for every table without "languageid=". A negative
// level is a hand-written shadow row; treat it as foreign rather than guessing.
func fts3IsTermIndexLevel(level, base int64) bool {
	return level >= base && level < base+fts3SegdirMaxLevel
}

// fts3FlushPendingSet writes one segment per non-empty index, IN INDEX ORDER --
// which is fts3PendingTermsFlush's own loop, and what makes the %_segments
// block ids come out in C fts3's order when more than one index spills in
// the same statement.
//
// leavesWritten is the total number of LEAF NODES this call wrote, across
// every index and any cascade merge it triggered -- p->nLeafAdd's own
// contribution from one fts3PendingTermsFlush call (fts3_write.c:2320/2435;
// see fts3SegmentImage.nLeaves and fts3AllocateSegdirIdx's own doc comment).
// Every caller that does not need it (every one except
// engine/fts3_automerge.go) simply discards it -- the ordinary flush itself
// is completely unaffected by this return value's existence.
func (db *DB) fts3FlushPendingSet(m fts3Module, segdir, segments *tableMeta, set *fts3PendingSet) (leavesWritten int64, err error) {
	for i, pt := range set.pts {
		if pt.empty() {
			continue
		}
		level := set.levelOf(i)
		idx, cascadeLeaves, err := db.fts3AllocateSegdirIdx(segdir, segments, m, level, 0, set.desc)
		if err != nil {
			return leavesWritten, err
		}
		leavesWritten += cascadeLeaves
		img := pt.encodeSegment(int(db.pageSize)-fts3NodeOverhead, fts3NextBlockID(segments))
		if err := fts3StoreSegment(segdir, segments, level, idx, img); err != nil {
			return leavesWritten, err
		}
		leavesWritten += img.nLeaves
		// fts3_write.c:3330 -- this is fts3SegmentMerge's own
		// "iLevel==FTS3_SEGCURSOR_PENDING" branch, an ORDINARY pending-terms
		// flush, so the promotion call is UNCONDITIONAL (see
		// engine/fts3_promote.go).
		fts3PromoteSegments(segdir, level, img.nLeafData)
	}
	return leavesWritten, nil
}
