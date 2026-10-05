// This file implements fts3/fts4's automatic incremental merge -- what C's
// xSync (fts3SyncMethod, fts3.c:3529) does after flushing pending terms once
// 'automerge=N' is on (fts3Automerge).
//
// # The trigger (fts3.c:3529-3568)
//
//	rc = sqlite3Fts3PendingTermsFlush(p);                    // the ordinary flush
//	if( rc==SQLITE_OK
//	 && p->nLeafAdd>(nMinMerge/16)                            // nMinMerge=64, so >4
//	 && p->nAutoincrmerge && p->nAutoincrmerge!=0xff
//	){
//	  rc = sqlite3Fts3MaxLevel(p, &mxLevel);                  // SQL_SELECT_MXLEVEL
//	  A = p->nLeafAdd * mxLevel;
//	  A += (A/2);
//	  if( A>(int)nMinMerge ) rc = sqlite3Fts3Incrmerge(p, A, p->nAutoincrmerge);
//	}
//
// p->nLeafAdd is per table, per transaction (fts3Int.h:272), reset at xBegin
// (fts3.c:3606) and bumped by the segment writer's two leaf flushes
// (fts3_write.c:2320, 2435). fts3SegmentImage.nLeaves reproduces that count,
// and fts3AllocateSegdirIdx/fts3MergeLevel thread a cascade's own leaf writes
// back to the caller.
//
// # Where the xSync boundary is
//
// C flushes once per autocommit statement or COMMIT. This write path rewrites
// a transaction's segment eagerly per statement (fts3_txn.go), so its
// equivalents are an autocommit write's own commit tail (insertIntoFts3,
// fts3Mutation.commit, via fts3AutocommitFlushAndAutomerge) and an explicit
// transaction's COMMIT (fts3AutomergeCommitCheck).
//
// # Atomicity
//
// No statement may leave partial state behind on a decline. fts3IncrmergeRun
// already simulates against cloned %_segdir/%_segments, but the ordinary flush
// before it writes directly. So when automerge is on (one %_stat lookup; off
// costs nothing), the flush also runs against a clone, the trigger is decided
// from the clone, and the real rows are replaced in one shot only once
// everything fits.
//
// # Still declined
//
//   - a write inside a transaction that could raise mxLevel (see
//     fts3DeclineAutomergeInTxn): COMMIT cannot be refused after the writes
//     happened, so the decline stays at the write.
//   - a statement that flushes more than one segment of its own (fts3_txn.go's
//     mid-flushed chunks: a multi-language INSERT, a docid-changing UPDATE):
//     their leaf counts would have to be summed into one decision. Declined
//     before any chunk is flushed.
//   - the command channel's writes (merge=, optimize, rebuild) do not chain
//     into an automerge. 'rebuild' clears %_stat first, so it cannot be
//     affected; the others would need shapes fts3_incrmerge.go already
//     declines.
package engine

import "fmt"

// fts3ReadAutoincrmerge resolves name's automerge=N the way C resolves
// p->nAutoincrmerge: read once per connection (the 0xff sentinel, fts3.c:1434)
// from %_stat row 2 (fts3_write.c:3357-3370), then sticky.
// db.fts3AutomergeCache holds the resolved value. A later wipe
// (fts3DeleteAll, from an emptying DELETE or 'rebuild') clears the %_stat row
// but never the in-memory value (fts3_write.c:1051), so the cache must not
// forget it either. Only the automerge= command overwrites it, as
// fts3DoAutoincrmerge does (fts3_write.c:5193).
//
// The "==1 -> 8" clamp (fts3_write.c:3364) matters only for a hand-written
// %_stat row; automerge= already clamps before storing.
func (db *DB) fts3ReadAutoincrmerge(name string) int64 {
	if v, ok := db.fts3AutomergeCache[name]; ok {
		return v
	}
	var n int64
	if stat := db.findTableMeta(name + "_stat"); stat != nil {
		if row, ok := stat.rows.get(fts3StatAutoincrmergeID); ok && len(row) >= 2 {
			n = valueToInt64Trunc(row[1])
			if n == 1 {
				n = 8
			}
		}
	}
	db.setFts3AutomergeCache(name, n)
	return n
}

// setFts3AutomergeCache resolves (or overwrites, for the automerge= command
// itself -- see fts3_command.go's fts3Automerge) name's cached value,
// allocating db.fts3AutomergeCache lazily. Never touched by a wipe
// (fts3_write.go's wipe / fts3_command.go's fts3Rebuild): see
// fts3ReadAutoincrmerge's own doc comment for why that is exactly the point.
func (db *DB) setFts3AutomergeCache(name string, val int64) {
	if db.fts3AutomergeCache == nil {
		db.fts3AutomergeCache = map[string]int64{}
	}
	db.fts3AutomergeCache[name] = val
}

// SetFts3AutomergeCache installs the map automerge resolutions are read from
// and written into, in place of db's own. The driver passes one per connection
// so the sticky resolution (fts3ReadAutoincrmerge) outlives a session; being a
// map, writes land in the caller's copy directly.
func (db *DB) SetFts3AutomergeCache(m map[string]int64) { db.fts3AutomergeCache = m }

// fts3SegdirMxLevel is SQL_SELECT_MXLEVEL (fts3_write.c:396/399):
// "SELECT max( level % 1024 ) FROM %_segdir" -- one maximum over the whole
// table, 0 when empty.
//
// A negative level (only reachable through a hand-written shadow table) is
// skipped rather than folded through Go's truncating %, as
// fts3IncrmergeSelectLevel does. That can only make mxLevel smaller, and so A
// smaller, so it cannot invent a merge C would not make.
func fts3SegdirMxLevel(segdir *tableMeta) (int64, error) {
	if segdir == nil {
		return 0, nil
	}
	rows, err := fts3SegdirRowsOf(segdir)
	if err != nil {
		return 0, err
	}
	var mx int64
	for _, r := range rows {
		if r.level < 0 {
			continue
		}
		rel := r.level % fts3SegdirMaxLevel
		if rel > mx {
			mx = rel
		}
	}
	return mx, nil
}

// fts3MaybeAutomerge is fts3SyncMethod's own automerge decision
// (fts3.c:3555-3568), given this boundary's already-resolved nAuto (see
// fts3ReadAutoincrmerge) and nLeafAdd (this boundary's own total leaf-write
// count -- see this file's header comment). segdir/segments may be a CLONE
// (see fts3AutocommitFlushAndAutomerge) or the real shadow tables; this
// function itself neither knows nor cares which, since fts3IncrmergeRun is
// already safe to call against either (fts3_incrmerge.go's own "never
// partially applied" guarantee).
func (db *DB) fts3MaybeAutomerge(name string, sch fts3Schema, segdir, segments *tableMeta, nAuto, nLeafAdd int64) error {
	// fts3.c:3558-3559: "p->nLeafAdd>(nMinMerge/16)" with nMinMerge=64, i.e.
	// nLeafAdd>4; "p->nAutoincrmerge" -- nAuto's own 0-means-off (the 0xff
	// "unknown" sentinel does not apply to a fresh read, which always
	// resolves to a concrete value).
	if nAuto == 0 || nLeafAdd <= 4 {
		return nil
	}
	mx, err := fts3SegdirMxLevel(segdir)
	if err != nil {
		return err
	}
	// fts3.c:3566-3568: A = nLeafAdd*mxLevel; A += A/2; if A>nMinMerge(64) merge.
	a := nLeafAdd * mx
	a += a / 2
	if a <= 64 {
		return nil
	}
	s := &fts3Shadows{segdir: segdir, segments: segments, stat: db.findTableMeta(name + "_stat")}
	return db.fts3IncrmergeRun(name, s, sch, int(a), int(nAuto))
}

// fts3AutocommitFlushAndAutomerge performs one autocommit statement's fts3
// segment write and, when automerge is on, runs the trigger once afterward
// with the statement's total leaf count. Used by insertIntoFts3 and
// fts3Mutation.commit's non-transactional tails.
//
// hadMidFlush must be true when the statement already flushed earlier segments
// of its own; callers check before flushing any, so a decline here never
// follows a real write.
//
// With automerge off this is just fts3FlushPendingSet on the real tables.
func (db *DB) fts3AutocommitFlushAndAutomerge(m fts3Module, name string, sch fts3Schema, segdir, segments *tableMeta, hadMidFlush bool, final *fts3PendingSet) error {
	nAuto := db.fts3ReadAutoincrmerge(name)
	if nAuto == 0 {
		_, err := db.fts3FlushPendingSet(m, segdir, segments, final)
		return err
	}
	if hadMidFlush {
		return fmt.Errorf("engine: %s table %s: a single statement that flushes more than one %%_segdir write of its own (e.g. a multi-language INSERT into a \"languageid=\" table, or an UPDATE that changes docids or scans an \"order=desc\" table) is not supported by this write path while automerge is enabled (see engine/fts3_automerge.go)", m.name, name)
	}
	// See this file's header comment ("Atomicity") for why the ordinary flush
	// itself runs against a clone here: fts3IncrmergeRun is already safe to
	// call and decline with nothing written, but the plain fts3FlushPendingSet
	// call below is not normally staged that way, and a decline from the
	// automerge check that follows it must still leave NOTHING written.
	cSegdir := fts3CloneRowsOnly(segdir)
	cSegments := fts3CloneRowsOnly(segments)
	leaves, err := db.fts3FlushPendingSet(m, cSegdir, cSegments, final)
	if err != nil {
		return err
	}
	if err := db.fts3MaybeAutomerge(name, sch, cSegdir, cSegments, nAuto, leaves); err != nil {
		return err
	}
	// Everything fit: commit the clone (as fts3IncrmergeSim.commit does).
	// replaceRows resets the key-set caches and marks the table mutated so
	// the write survives Close -- without it, the original pages were copied
	// back over the flushed segments. segments is nil for a table that never
	// spilled into %_segments.
	segdir.replaceRows(cSegdir.rows)
	if segments != nil {
		segments.replaceRows(cSegments.rows)
	}
	return nil
}

// fts3DeclineAutomergeInTxn is the guard vtab_fts3.go's insertIntoFts3 and
// fts3_write.go's fts3MutationCommon both call BEFORE staging anything, when
// this statement would write to name INSIDE an explicit transaction or
// savepoint (db.inTransaction() || len(db.savepoints)>0) and name has
// automerge enabled -- see this file's header comment ("What remains
// declined") for why the transactional path is not wired to the trigger.
// nil when automerge is off for name (the overwhelming common case) or this
// statement is not inside a transaction at all, in which case the caller
// proceeds exactly as it did before this file existed.
func (db *DB) fts3DeclineAutomergeInTxn(m fts3Module, name string) error {
	if !db.inTransaction() && len(db.savepoints) == 0 {
		return nil
	}
	// Every write this guard LETS THROUGH is recorded, including one made
	// while automerge is still off: nLeafAdd counts every leaf this
	// transaction writes, so a later "automerge=N" in the same transaction
	// makes those count too. See fts3AutomergeCommitCheck. A write this guard
	// DECLINES is not recorded -- it writes nothing, adds nothing to nLeafAdd,
	// and recording it made the COMMIT decline as well, leaving open a
	// transaction C SQLite commits.
	if db.fts3ReadAutoincrmerge(name) == 0 {
		db.noteFts3AutomergeTouched(name)
		return nil
	}
	// A table whose every segment is at relative level 0 cannot be merged by
	// the trigger whatever this transaction writes:
	//
	//	A = p->nLeafAdd * mxLevel;
	//	A += (A/2);
	//	if( A>(int)nMinMerge ) rc = sqlite3Fts3Incrmerge(p, A, ...);
	//
	// (fts3.c:3566-3568) with mxLevel 0 makes A 0. So the write is allowed, and
	// fts3AutomergeCommitCheck re-asks at COMMIT.
	//
	// ...provided this write cannot itself raise mxLevel. Inside a transaction
	// that needs a cascade: a level reaching FTS3_MERGE_COUNT (16) segments. One
	// statement opens at most two level-0 segments (its own and a statement
	// sub-transaction's flush), so a write is allowed only while every index's
	// level 0 holds 13 or fewer. Anything else is declined at the write.
	if mx, l0, ok := db.fts3TableLevelShape(name); ok && mx == 0 && l0 <= 13 {
		db.noteFts3AutomergeTouched(name)
		return nil
	}
	return fmt.Errorf("engine: %s table %s: a write inside an explicit transaction or savepoint to a table with automerge enabled is not supported by this write path yet (see engine/fts3_automerge.go)", m.name, name)
}

// fts3TableLevelShape reports name's max relative level (SQL_SELECT_MXLEVEL)
// and the largest number of segments any one index holds at relative level 0.
// ok is false when there is no %_segdir to read or it does not decode, which
// every caller treats as "cannot prove it safe", i.e. declines.
func (db *DB) fts3TableLevelShape(name string) (mx int64, level0 int, ok bool) {
	segdir := db.findTableMeta(name + "_segdir")
	if segdir == nil {
		return 0, 0, false
	}
	rows, err := fts3SegdirRowsOf(segdir)
	if err != nil {
		return 0, 0, false
	}
	// Level 0 is PER INDEX: a prefix= or languageid= table keeps one slice of
	// 1024 levels per index (fts3_prefix.go), each cascading on its own.
	perIndex := map[int64]int{}
	for _, r := range rows {
		if r.level < 0 {
			continue
		}
		rel := r.level % fts3SegdirMaxLevel
		if rel > mx {
			mx = rel
		}
		if rel == 0 {
			perIndex[r.level]++
			level0 = max(level0, perIndex[r.level])
		}
	}
	return mx, level0, true
}

// noteFts3AutomergeTouched records name in fts3AutomergeTouched (writer.go).
func (db *DB) noteFts3AutomergeTouched(name string) {
	if db.fts3AutomergeTouched == nil {
		db.fts3AutomergeTouched = map[string]bool{}
	}
	db.fts3AutomergeTouched[name] = true
}

// fts3TableMxLevel is SQL_SELECT_MXLEVEL over name's live %_segdir. ok is
// false when there is no %_segdir to read (a table this session has not
// loaded) or it does not decode, and every caller treats that as "cannot
// prove the level is 0", i.e. declines.
func (db *DB) fts3TableMxLevel(name string) (int64, bool) {
	segdir := db.findTableMeta(name + "_segdir")
	if segdir == nil {
		return 0, false
	}
	mx, err := fts3SegdirMxLevel(segdir)
	if err != nil {
		return 0, false
	}
	return mx, true
}

// fts3AutomergeCommitCheck is fts3DeclineAutomergeInTxn's backstop at the
// point C decides: xSync at COMMIT (fts3.c:3529-3573). The write-time guard
// allowed the write because mxLevel was 0; it can only have risen through a
// cascade (fts3AllocateSegdirIdx). fts3PromoteSegments (fts3_write.c:3145)
// only moves segments down, so it never raises mxLevel.
//
// This engine's %_segdir at COMMIT equals C's after xSync's flush: fts3_txn.go
// opens a fresh segment at exactly C's flush points. So mxLevel here is C's.
//
// Declining leaves the transaction open, as a failed deferred FK check does
// (fkCommitCheck runs just before); ROLLBACK still undoes it. A table dropped
// before COMMIT has no trigger to run (fts5savepoint.test 3.1).
func (db *DB) fts3AutomergeCommitCheck() error {
	for name := range db.fts3AutomergeTouched {
		if db.findVtabMeta(name) == nil {
			continue // dropped since: nothing left for xSync to merge
		}
		if db.fts3ReadAutoincrmerge(name) == 0 {
			continue
		}
		if mx, ok := db.fts3TableMxLevel(name); ok && mx == 0 {
			continue
		}
		return fmt.Errorf("engine: fts3/fts4 table %s: cannot COMMIT a transaction that wrote into it with automerge enabled once its index has a segment above relative level 0 -- C fts3 may run its automatic incremental merge at this COMMIT, and this write path reproduces that trigger only for autocommit statements (see engine/fts3_automerge.go); the transaction is still open", name)
	}
	return nil
}
