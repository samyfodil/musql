// This file implements fts3/fts4's command channel: an INSERT naming the table
// itself as the column carries a command ("INSERT INTO t1(t1)
// VALUES('optimize')"). fts3's command set is not fts5's
// (fts5CommandInsert).
//
// # What makes a command
//
// fts3 declares a hidden column named for the table (table_xinfo on "USING
// fts4(a)" reports a, t HIDDEN, docid HIDDEN, __langid HIDDEN), and xUpdate
// treats an INSERT as a command when that value is not NULL
// ("sqlite3_value_type(apVal[p->nColumn+2])!=SQLITE_NULL"). So:
//
//   - "INSERT INTO t(t) VALUES(NULL)" inserts an all-NULL row;
//   - other columns of a command INSERT are ignored ("INSERT INTO
//     t(t,docid) VALUES('optimize',99)" adds no row);
//   - this is unambiguous because fts3 refuses a column named for its table
//     ("vtable constructor failed: tt"), as createShadowTables does here.
//
// The name is matched case-insensitively over a fixed-length prefix with no
// trimming (fts3SpecialInsert): 'OPTIMIZE' works, ' optimize' and 'optimize '
// fail, and a non-text value fails.
//
// # optimize
//
// Merges every segment into one at the greatest level present, idx 0:
//
//	3 level-0 segments            -> one segment at L0 idx 0
//	18 INSERTs (L0 idx 0,1 + L1)  -> one segment at L1 idx 0
//
// Delete markers are dropped (bIgnoreEmpty), so a term with no document left
// disappears; %_content, %_docsize and %_stat are untouched. A table with one
// segment and nothing pending is left alone ("csr.nSegment==1 && !pending"
// returns SQLITE_DONE, which the INSERT form treats as success).
//
// # rebuild
//
// fts3DoRebuild is fts3DeleteAll(bContent=0) plus a re-scan of %_content:
// %_segdir, %_segments and %_docsize are rebuilt and %_stat recomputed. The
// result is one segment at L0 idx 0 with block ids restarting at 1; it repairs
// an index corrupted behind fts3's back; %_stat's other rows (e.g. automerge's
// id=2) are deleted; an empty fts4 table still gets %_stat row 0 as zeroes and
// an empty %_segdir.
//
// # integrity-check
//
// fts3IntegrityCheck checksums every index entry against a re-tokenization of
// %_content (SQLITE_CORRUPT_VTAB on mismatch); here the term sets are
// compared directly:
//
//	%_content row the index does not know about        -> ERROR
//	index entry for a docid %_content no longer holds  -> ERROR
//	a correctly-masking delete marker                  -> ok
//	%_stat corrupted (X'FF') / %_docsize emptied       -> ok (not checked)
//
// # merge=X,Y
//
// Runs fts3's incremental merge for the subset fts3_incrmerge.go reproduces,
// declining the rest. When nothing qualifies (nMerge 0, or no level with nMin
// segments and no %_stat hint), sqlite3Fts3Incrmerge's loop
//
//	SQL_FIND_MERGE_LEVEL   SELECT level, count(*) AS cnt FROM %_segdir
//	                       GROUP BY level HAVING cnt>=?    -- ? is MAX(2,nMin)
//
// breaks at once and the INSERT succeeds having written nothing -- except that
// fts3DoIncrmerge first creates %_stat on an fts3 table without one ("if(
// !p->bHasStat ) sqlite3Fts3CreateStatTable(p)").
//
// Parameters go through fts3Getint, not strtol: leading digits only and the
// rest must be exhausted, so 'merge=2x' and 'merge=1,2,3' are errors, and
// 'merge=' never gets here (fts3SpecialInsert needs nVal>6). B defaults to
// MergeCount(p)/2 = 8; B<2 is an error.
//
// # automerge=N
//
// Writes %_stat id=2 (FTS_STAT_AUTOINCRMERGE) as an integer, clamped:
// 'automerge=4' stores 4, 17 and 1 store 8, 'automerge=x' stores 0; an fts3
// table gets %_stat created. It turns on the automatic merge for later writes,
// which fts3_automerge.go reproduces.
package engine

import (
	"fmt"
	"sort"
	"strings"
)

// fts3CommandSlot returns the index in names of the column named after the
// table itself -- fts3's command channel -- or -1. Case-insensitive, exactly as
// column resolution is.
func fts3CommandSlot(tableName string, names []string) int {
	for i, n := range names {
		if strings.EqualFold(n, tableName) {
			return i
		}
	}
	return -1
}

// fts3CommandInsert runs the command carried in slot cmdSlot of each of an
// INSERT's rows. handled is false when EVERY row holds NULL there, which is an
// ordinary INSERT of an all-NULL row rather than a command (see this file's
// comment); a statement that MIXES the two is declined rather than guessed at.
func (db *DB) fts3CommandInsert(vm *vtabMeta, m fts3Module, table string, valueRows [][]Value, cmdSlot int) (bool, int, error) {
	nCmd := 0
	for _, row := range valueRows {
		if row[cmdSlot].Typ != Null {
			nCmd++
		}
	}
	if nCmd == 0 {
		return false, 0, nil
	}
	if nCmd != len(valueRows) {
		return true, 0, fmt.Errorf("engine: INSERT into %s: a multi-row INSERT that mixes %s's command channel with ordinary rows is not supported by this write path", table, m.name)
	}
	for _, row := range valueRows {
		if err := db.fts3RunCommand(vm, m, valueToText(row[cmdSlot])); err != nil {
			return true, 0, err
		}
	}
	// Real fts3 reports changes()==1 for a command -- verified after a
	// two-row and a three-row INSERT, where it reads 1 either way.
	return true, 1, nil
}

// fts3RunCommand dispatches one command by name. The comparison is
// case-insensitive but does NO trimming, because C fts3's is a fixed-length
// sqlite3_strnicmp against the whole value (see this file's comment).
func (db *DB) fts3RunCommand(vm *vtabMeta, m fts3Module, name string) error {
	var err error
	switch {
	case strings.EqualFold(name, "optimize"):
		// fts3SpecialInsert calls fts3DoOptimize with bReturnDone 0, so the
		// command channel SWALLOWS the SQLITE_DONE the function form reports as
		// 'Index already optimal' -- seenDone is discarded here.
		_, _, err = db.fts3Optimize(vm, m)
	case strings.EqualFold(name, "rebuild"):
		_, err = db.fts3Rebuild(vm, m)
	case strings.EqualFold(name, "integrity-check"):
		err = db.fts3IntegrityCheck(vm, m)
	case len(name) > 6 && strings.EqualFold(name[:6], "merge="):
		// Returns WITHOUT the seal below: fts3SpecialInsert calls
		// fts3DoIncrmerge directly and nothing on that path flushes the
		// pending terms, so "BEGIN; INSERT; merge=...; INSERT; COMMIT" leaves
		// C fts3 with ONE segment for the two INSERTs, not two.
		return db.fts3Incrmerge(vm, m, name)
	case len(name) > 10 && strings.EqualFold(name[:10], "automerge="):
		_, err = db.fts3Automerge(vm, m, name)
	default:
		// Every other spelling C fts3 rejects too (its SQLITE_ERROR), so
		// the two engines already agree -- including 'nodesize=' and
		// 'maxpending=', which exist only in an SQLITE_TEST build.
		return fmt.Errorf("engine: %s: unknown command %q", m.name, name)
	}
	if err != nil {
		return err
	}
	// Real fts3 clears its pending terms as part of every command that flushes
	// them, so a later statement in the same transaction opens a FRESH segment.
	// This engine writes the transaction's segment eagerly (fts3_txn.go) rather
	// than holding pending terms, so the equivalent is to stop accumulating
	// into the segment the command just consumed. Without this, "BEGIN; INSERT;
	// optimize; INSERT; COMMIT" would fold the second INSERT back into the
	// segment optimize produced (ONE %_segdir row) where C fts3 leaves TWO.
	db.fts3TxnSealTable(vm.name)
	return nil
}

// fts3CommandShadows resolves the shadow tables a command needs.
func (db *DB) fts3CommandShadows(vm *vtabMeta, m fts3Module) (*fts3Shadows, fts3Schema, error) {
	sch, err := m.schemaOf(db, vm)
	if err != nil {
		return nil, fts3Schema{}, err
	}
	s := &fts3Shadows{
		content:  db.findTableMeta(vm.name + "_content"),
		segdir:   db.findTableMeta(vm.name + "_segdir"),
		segments: db.findTableMeta(vm.name + "_segments"),
	}
	// A "content=" table has none (fts3_content.go); 'optimize' works on one
	// all the same, since it only ever reads %_segdir and %_segments.
	if (s.content == nil && !sch.hasContent) || s.segdir == nil {
		return nil, fts3Schema{}, fmt.Errorf("engine: %s table %s is missing its shadow tables", m.name, vm.name)
	}
	if err := fts3ResolveFts4Shadows(db, vm.name, m, sch, s); err != nil {
		return nil, fts3Schema{}, err
	}
	return s, sch, nil
}

// fts3SegdirNewestFirst orders %_segdir rows the way fts3SegReaderCursor adds
// its readers: LEVEL ASCENDING, then IDX DESCENDING. A higher level holds older
// data (it was merged down from a full level below it) and within a level a
// higher idx is the newer segment, so this is newest first -- which is the
// order fts3MergeDoclist's newest-wins resolution needs. The read path derives
// the same order independently (fts3_search.go's fts3LoadIndex).
func fts3SegdirNewestFirst(rows []fts3SegdirRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].level != rows[j].level {
			return rows[i].level < rows[j].level
		}
		return rows[i].idx > rows[j].idx
	})
}

// fts3Optimize is fts3DoOptimize: merge every segment into one. It reports
// whether it wrote anything, and seenDone -- fts3DoOptimize's own bSeenDone,
// the OR over every (langid, index) of "that index's merge returned
// SQLITE_DONE". Only the FUNCTION form looks at seenDone (bReturnDone 1); the
// command channel passes 0 and swallows it.
func (db *DB) fts3Optimize(vm *vtabMeta, m fts3Module) (wrote, seenDone bool, err error) {
	s, sch, err := db.fts3CommandShadows(vm, m)
	if err != nil {
		return false, false, err
	}
	// fts3DoOptimize merges EACH INDEX of EACH LANGUAGE separately, in
	// (language, index) order, swallowing the SQLITE_DONE a single-segment one
	// returns. Verified against the oracle on "fts4(x, prefix=5)" holding
	// 'ab','cd','abcdef': the three level-0 segments merge while the ONE prefix
	// segment at level 1024 is left BYTE IDENTICAL; and on a "languageid="
	// table holding languages 2 and 7, where each of the six (language, index)
	// blocks comes back as exactly one segment.
	//
	// The language list is SQL_SELECT_ALL_LANGID's (fts3_langid.go), which is
	// {0} plus every language %_segdir actually holds a segment for -- so a
	// table without the option iterates language 0 alone, exactly as before.
	rows, err := fts3SegdirRowsOf(s.segdir)
	if err != nil {
		return false, false, err
	}
	nIndex := sch.nIndex()
	for _, lang := range fts3SegdirLangids(rows, nIndex) {
		for i := 0; i < nIndex; i++ {
			did, done, oerr := db.fts3OptimizeIndex(s, fts3LevelBase(lang, nIndex, i), sch.descIdx)
			if oerr != nil {
				return false, false, oerr
			}
			wrote = wrote || did
			seenDone = seenDone || done
		}
	}
	return wrote, seenDone, nil
}

// fts3OptimizeIndex is fts3Optimize for the one index whose levels start at
// base. done is fts3SegmentMerge's SQLITE_DONE, which it returns for
// "csr.nSegment==1 && !fts3SegReaderIsPending(csr.apSegment[0])" and NOT for a
// cursor with no segments at all -- that one falls out of the "csr.nSegment==0"
// goto above it with rc still SQLITE_OK. The distinction is observable through
// the function form: on "fts4(x, prefix=5)" holding three term segments and one
// prefix segment the answer is 'Index already optimal' (the prefix index's
// single segment is DONE) while the term index really does merge three rows
// into one, and on the same table's EMPTY prefix index it would not be.
func (db *DB) fts3OptimizeIndex(s *fts3Shadows, base int64, desc bool) (wrote, done bool, err error) {
	all, err := fts3SegdirRowsOf(s.segdir)
	if err != nil {
		return false, false, err
	}
	rows := make([]fts3SegdirRow, 0, len(all))
	for _, r := range all {
		if fts3SameSegdirIndex(r.level, base) {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return false, false, nil
	}
	if len(rows) == 1 {
		// SQLITE_DONE: nothing to merge, and the INSERT form reports success.
		return false, true, nil
	}
	fts3SegdirNewestFirst(rows)
	byTerm := map[string]fts3TermPostings{}
	for _, in := range rows {
		if rerr := fts3ReadSegmentInto(s.segments, in, byTerm, desc); rerr != nil {
			return false, false, rerr
		}
	}
	outLevel := base
	for _, r := range rows {
		if r.level > outLevel {
			outLevel = r.level
		}
	}
	// Encode BEFORE freeing the inputs, so the output's %_segments blocks are
	// numbered above theirs -- the same ordering fts3_merge.go verified for a
	// level merge, and the reason the next spilling segment continues from the
	// output's last block rather than reusing a freed one.
	pt := fts3PendingTermsFrom(byTerm, true, desc)
	var img *fts3SegmentImage
	if !pt.empty() {
		img = pt.encodeSegment(int(db.pageSize)-fts3NodeOverhead, fts3NextBlockID(s.segments))
	}
	// Real fts3 deletes the input %_segdir rows (fts3DeleteSegdir) BEFORE
	// writing the output's own row, so the output lands on the rowid the
	// emptied table hands out next.
	for _, in := range rows {
		s.segdir.dropRow(in.rowid)
		if s.segments == nil || in.startBlock == 0 {
			continue
		}
		for b := in.startBlock; b <= in.endBlock; b++ {
			s.segments.dropRow(uint64(b))
		}
	}
	if img != nil {
		if serr := fts3StoreSegment(s.segdir, s.segments, outLevel, 0, img); serr != nil {
			return false, false, serr
		}
	}
	return true, false, nil
}

// fts3Getint is fts3Getint: leading decimal digits only, stopping once the
// accumulator would pass 214748363 (the C loop tests that BEFORE consuming the
// next digit, so a long run of digits is truncated rather than overflowed). It
// returns the value and the unconsumed remainder.
func fts3Getint(z string) (int, string) {
	i, k := 0, 0
	for k < len(z) && z[k] >= '0' && z[k] <= '9' && i < 214748363 {
		i = 10*i + int(z[k]-'0')
		k++
	}
	return i, z[k:]
}

// fts3ParseMergeParam reads merge='s "A,B" exactly as fts3DoIncrmerge does. ok
// is false for the two spellings it answers SQLITE_ERROR to: a remainder the
// digit scan did not consume, and a B below 2. B defaults to MergeCount(p)/2.
func fts3ParseMergeParam(param string) (nMerge, nMin int, ok bool) {
	nMin = fts3MergeCount / 2
	nMerge, rest := fts3Getint(param)
	// "z[0]==',' && z[1]!='\0'": a TRAILING comma is not a separator, it is
	// the unconsumed remainder that makes the whole parameter an error.
	if len(rest) > 1 && rest[0] == ',' {
		nMin, rest = fts3Getint(rest[1:])
	}
	if rest != "" || nMin < 2 {
		return 0, 0, false
	}
	return nMerge, nMin, true
}

// fts3Incrmerge is fts3DoIncrmerge. Parses the command's parameters and
// resolves shadows, then hands off to fts3IncrmergeRun (fts3_incrmerge.go)
// for the actual algorithm -- see that file's comment for exactly what it
// does and does not reproduce.
func (db *DB) fts3Incrmerge(vm *vtabMeta, m fts3Module, name string) error {
	param := name[6:]
	nMerge, nMin, ok := fts3ParseMergeParam(param)
	if !ok {
		// SQLITE_ERROR, which surfaces on the INSERT as a bare "SQL logic
		// error"; the wording here is this engine's.
		return fmt.Errorf("engine: %s: malformed %q command parameter %q (expected \"A\" or \"A,B\" with B at least 2)", m.name, name, param)
	}
	s, sch, err := db.fts3CommandShadows(vm, m)
	if err != nil {
		return err
	}
	if err := db.fts3IncrmergeRun(vm.name, s, sch, nMerge, nMin); err != nil {
		return err
	}
	// Nothing merged, so nothing but fts3DoIncrmerge's own %_stat creation is
	// left to reproduce. It is guarded by !p->bHasStat, which is 1 for every
	// fts4 table, so only an fts3 one can be missing it.
	if !m.isFts4 && db.findTableMeta(vm.name+"_stat") == nil {
		return db.CreateTable(fmt.Sprintf("CREATE TABLE %s(id INTEGER PRIMARY KEY, value BLOB)", fts3QuoteName(vm.name+"_stat")))
	}
	return nil
}

// fts3StatAutoincrmergeID is FTS_STAT_AUTOINCRMERGE (fts3_write.c:75): the
// %_stat row id automerge=N's own setting lives at.
const fts3StatAutoincrmergeID = 2

// fts3Automerge is fts3DoAutoincrmerge (fts3_write.c:5182-5208): parse with
// fts3Getint (no remainder check, so 'automerge=x' is 0), clamp 1 or anything
// above fts3MergeCount to 8 (5193-5195), create %_stat for a bare fts3 table,
// and REPLACE %_stat row 2 with the integer (5202-5205, SQL_REPLACE_STAT).
//
// Unlike merge=, it is dispatched through fts3RunCommand's switch, since
// fts3SpecialInsert treats it like every other command (fts3_write.c:5474-
// 5475): it flushes pending terms and seals the transaction's segment as
// optimize/rebuild do. Turning it on has no other immediate effect; later
// writes consult fts3ReadAutoincrmerge.
func (db *DB) fts3Automerge(vm *vtabMeta, m fts3Module, name string) (bool, error) {
	val, _ := fts3Getint(name[10:])
	if val == 1 || val > fts3MergeCount {
		val = 8
	}
	if !m.isFts4 && db.findTableMeta(vm.name+"_stat") == nil {
		if err := db.CreateTable(fmt.Sprintf("CREATE TABLE %s(id INTEGER PRIMARY KEY, value BLOB)", fts3QuoteName(vm.name+"_stat"))); err != nil {
			return false, err
		}
	}
	stat := db.findTableMeta(vm.name + "_stat")
	if stat == nil {
		return false, fmt.Errorf("engine: %s table %s is missing its %%_stat shadow table", m.name, vm.name)
	}
	// %_stat's own convention (fts3_write.go's fts3StagedRow/putRow usage
	// throughout this package) is an INTEGER PRIMARY KEY table whose rowid
	// carries the id, so the stored record's first slot is NULL.
	stat.putRow(fts3StatAutoincrmergeID, []Value{{Typ: Null}, {Typ: Int, I: int64(val)}})
	// Real fts3DoAutoincrmerge sets p->nAutoincrmerge directly in this SAME
	// call (fts3_write.c:5193), not merely %_stat -- so this must too,
	// otherwise a table whose automerge value was already cached (e.g. a
	// SECOND automerge= command, changing it) would keep answering the OLD
	// value until something else forced a re-resolve. See
	// engine/fts3_automerge.go's fts3ReadAutoincrmerge for the cache itself.
	db.setFts3AutomergeCache(vm.name, int64(val))
	return true, nil
}

// fts3StatHintNonEmpty reports whether %_stat row 1 holds a hint with any bytes
// in it. fts3IncrmergeHintLoad reads it with sqlite3_column_blob(), which
// renders ANY non-NULL value as bytes -- an INTEGER hint stored by hand is its
// decimal text -- so only NULL and a zero-length blob/text leave hint.n at 0.
func fts3StatHintNonEmpty(v Value) bool {
	switch v.Typ {
	case Null:
		return false
	case Blob, Text:
		return len(v.S) > 0
	default:
		return true
	}
}

// fts3Rebuild is fts3DoRebuild: throw the index away and rebuild it from
// %_content, recomputing FTS4's %_docsize and %_stat with it.
//
// Like every other statement here it builds ONE segment. Real fts3 flushes its
// pending terms early if they pass FTS3_MAX_PENDING_DATA (1MB), which would
// leave more than one -- the same unreproduced limit an oversized
// "INSERT ... SELECT" already has (vtab_fts3.go), not a new one.
func (db *DB) fts3Rebuild(vm *vtabMeta, m fts3Module) (bool, error) {
	s, sch, err := db.fts3CommandShadows(vm, m)
	if err != nil {
		return false, err
	}
	nCol := len(sch.cols)
	// 'rebuild' re-reads whatever fts3ReadExprList names -- %_content for an
	// ordinary table and the CONTENT TABLE for a "content=" one, which is the
	// whole point of the command there. A CONTENTLESS table names "", so this
	// is where its "SQL logic error" comes from (verified: "INSERT INTO
	// ft9(ft9) VALUES('rebuild')" on "fts4(content=, x)" is exactly that).
	docids, rows, err := db.fts3MutationRows(sch, s)
	if err != nil {
		return false, err
	}

	// fts3DoRebuild re-scans %_content in docid order through
	// fts3PendingTermsDocid, which FLUSHES whenever the LANGUAGE changes
	// (fts3_langid.go). So the rebuild of a "languageid=" table writes one
	// segment per index per CONTIGUOUS RUN of one language, not one per index;
	// a table without the option has exactly one run and is unchanged.
	var runs []*fts3PendingSet
	var cur *fts3PendingSet
	st := &fts4Stat{colSizes: make([]int64, nCol)}
	sizes := make([][]int, len(docids))
	for i, docid := range docids {
		langid := fts3RowLangid(sch, rows[i])
		if cur == nil || cur.langid != langid {
			cur = newFts3PendingSet(sch.prefixes, sch.descIdx)
			cur.langid = langid
			runs = append(runs, cur)
		}
		sz := make([]int, nCol)
		for c := 0; c < nCol; c++ {
			v := rows[i][c+1]
			if v.Typ == Null || !sch.indexed(c) {
				// sqlite3_column_text() is NULL, so fts3PendingTermsAdd returns
				// early and sqlite3_column_bytes() contributes 0; a notindexed=
				// column is skipped the same way (fts3DoRebuild's own
				// abNotindexed check).
				continue
			}
			text := valueToText(v)
			st.nByte += int64(len(text))
			terms := sch.tok.tokenize(text)
			sz[c] = len(terms)
			for pos, term := range terms {
				cur.add(term, docid, c, pos)
			}
		}
		for c, n := range sz {
			st.colSizes[c] += int64(n)
		}
		st.nDoc++
		sizes[i] = sz
	}

	// fts3DeleteAll(bContent=0): every shadow table but %_content is emptied,
	// which is also what restarts the %_segments block ids at 1.
	fts3ClearTable(s.segdir)
	fts3ClearTable(s.segments)
	fts3ClearTable(s.docsize)
	fts3ClearTable(s.stat)
	// One segment per index, in index order -- %_segments was just emptied, so
	// the block ids restart at 1 and climb across them. The leaf count is
	// discarded: 'rebuild' clears %_stat (including automerge's own row 2)
	// BEFORE this loop runs, so automerge cannot be active for any of these
	// writes regardless (see engine/fts3_automerge.go's own file comment on
	// why the command channel is out of scope for the automatic trigger).
	for _, run := range runs {
		if _, ferr := db.fts3FlushPendingSet(m, s.segdir, s.segments, run); ferr != nil {
			return false, ferr
		}
	}
	if s.docsize != nil {
		for i, docid := range docids {
			s.docsize.putRow(uint64(docid), []Value{{Typ: Null}, {Typ: Blob, S: encodeFts4Docsize(sizes[i])}})
		}
	}
	if m.isFts4 {
		s.stat.putRow(0, []Value{{Typ: Null}, {Typ: Blob, S: st.encode()}})
	}
	return true, nil
}

// fts3IntegrityCheck is fts3DoIntegrityCheck: the live index must describe
// exactly the terms a fresh tokenization of %_content produces.
//
// Real fts3 compares two 64-bit checksums over the (term, docid, column,
// position, langid) tuples; comparing the tuple sets themselves is the same
// test without the hash, and cannot be fooled by a collision.
func (db *DB) fts3IntegrityCheck(vm *vtabMeta, m fts3Module) error {
	s, sch, err := db.fts3CommandShadows(vm, m)
	if err != nil {
		return err
	}
	all, err := fts3SegdirRowsOf(s.segdir)
	if err != nil {
		return err
	}

	// The expected side covers EVERY index: fts3IntegrityCheck folds each
	// prefix entry into the checksum too, keyed by the index number, so an
	// intact term index over a corrupted prefix index is still corrupt. It is
	// also keyed by the LANGUAGE (fts3ChecksumEntry takes iLangid), so a row
	// indexed under the wrong language is corruption too.
	nCol := len(sch.cols)
	nIndex := sch.nIndex()
	// Same source as 'rebuild': fts3IntegrityCheck re-tokenizes
	// "SELECT %s" % p->zReadExprlist, so a "content=" table is checked against
	// its CONTENT TABLE (verified -- editing that table behind the index makes
	// the check report "database disk image is malformed") and a CONTENTLESS
	// one is "SQL logic error", the command having nothing to compare against.
	docids, content, err := db.fts3MutationRows(sch, s)
	if err != nil {
		return err
	}
	expect := map[int64]*fts3PendingSet{}
	expectFor := func(langid int64) *fts3PendingSet {
		if e, ok := expect[langid]; ok {
			return e
		}
		e := newFts3PendingSet(sch.prefixes, sch.descIdx)
		e.langid = langid
		expect[langid] = e
		return e
	}
	for i, docid := range docids {
		e := expectFor(fts3RowLangid(sch, content[i]))
		for c := 0; c < nCol; c++ {
			v := content[i][c+1]
			if v.Typ == Null || !sch.indexed(c) {
				continue
			}
			for pos, term := range sch.tok.tokenize(valueToText(v)) {
				e.add(term, docid, c, pos)
			}
		}
	}

	for _, lang := range fts3SegdirLangids(all, nIndex) {
		expectFor(lang)
	}
	for lang, e := range expect {
		for i := 0; i < nIndex; i++ {
			base := fts3LevelBase(lang, nIndex, i)
			rows := make([]fts3SegdirRow, 0, len(all))
			for _, r := range all {
				if fts3SameSegdirIndex(r.level, base) {
					rows = append(rows, r)
				}
			}
			fts3SegdirNewestFirst(rows)
			byTerm := map[string]fts3TermPostings{}
			for _, in := range rows {
				if rerr := fts3ReadSegmentInto(s.segments, in, byTerm, sch.descIdx); rerr != nil {
					return rerr
				}
			}
			if !fts3PendingTermsEqual(fts3PendingTermsFrom(byTerm, true, sch.descIdx), e.pts[i]) {
				// SQLITE_CORRUPT_VTAB there; the wording is this engine's.
				return fmt.Errorf("engine: %s table %s: database disk image is malformed (its index does not match %s_content)", m.name, vm.name, vm.name)
			}
		}
	}
	return nil
}

// fts3PendingTermsEqual reports whether two term sets hold the same
// (term, docid, column, position) tuples. Both sides are built docid-ascending
// and, within a document, in (column, position) order, so the hit slices
// compare elementwise.
func fts3PendingTermsEqual(a, b *fts3PendingTerms) bool {
	if len(a.terms) != len(b.terms) {
		return false
	}
	for term, da := range a.terms {
		db, ok := b.terms[term]
		if !ok || len(da.docids) != len(db.docids) {
			return false
		}
		for i, docid := range da.docids {
			if db.docids[i] != docid {
				return false
			}
			ha, hb := da.hits[docid], db.hits[docid]
			if len(ha) != len(hb) {
				return false
			}
			for j := range ha {
				if ha[j] != hb[j] {
					return false
				}
			}
		}
	}
	return true
}
