// This file implements HOT-JOURNAL DETECTION and recovery (playback) for a C
// SQLite file the converter is about to read (openPager, pager.go),
// exactly as SQLite recovers a hot journal before its first read: if the file's
// last writer crashed after fsyncing the journal but before deleting it, the
// journal is "hot" and its original pages are rolled back into the db,
// restoring the pre-transaction state an import must see.
package sqlite

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/samyfodil/musql/internal/filelock"
)

// The journal format (pager.c):
//
//	journal header @ offset 0, big-endian, padded to sectorSize (512) bytes:
//	  [0:8]   magic   d9 d5 05 f9 20 a1 63 d7
//	  [8:12]  nRec    the REAL record count (never 0, never 0xffffffff)
//	  [12:16] cksumInit  a nonce
//	  [16:20] dbSize  original page count -- rollback truncates the db to this
//	  [20:24] sectorSize  512
//	  [24:28] pageSize
//	page records START at offset == sectorSize (512), each:
//	  u32 pgno (1-based), then pageSize bytes of the ORIGINAL page, then u32 cksum
//	cksum = cksumInit; for i := pageSize-200; i>0; i -= 200 { cksum += data[i] }
//
// The rollback-journal modes differ only in what happens to the journal file
// at the commit point:
//
//	delete    the file is REMOVED                         (absent)
//	truncate  the file is TRUNCATED to zero length        (size 0)
//	persist   the file is KEPT with its HEADER ZEROED     (size kept, [0:28]=0)
//	memory    no file is ever created                     (absent)
//	off       no file is ever created                     (absent)
//
// A persisted journal is not hot because its header is zeroed: condition (4)
// below, first byte nonzero, is what tells "finalized" from "a writer crashed".

// journalSuffix is appended to the database path to form the rollback journal
// path, exactly as SQLite names it.
const journalSuffix = "-journal"

// journalMagic is the 8-byte rollback-journal magic (SQLite's aJournalMagic).
var journalMagic = []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// journalCksum is SQLite's pager_cksum: an additive checksum over the bytes at
// data[pageSize-200], data[pageSize-400], ... down to the front of the page.
func journalCksum(cksumInit uint32, data []byte, pageSize uint32) uint32 {
	cksum := cksumInit
	for i := int(pageSize) - 200; i > 0; i -= 200 {
		cksum += uint32(data[i])
	}
	return cksum
}

// syncDir fsyncs the directory containing path so a create/delete of the
// journal's directory entry is itself durable. Best-effort: some filesystems
// and platforms do not permit fsync on a directory handle.
func syncDir(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil
	}
	defer d.Close()
	d.Sync() // best-effort, as above
	return nil
}

// journalHeaderSize is the number of meaningful bytes at the start of the
// journal header (magic + five u32s). The header is padded to sectorSize on
// disk; records begin at the sector boundary.
const journalHeaderSize = 28

// recoverHotJournal detects and, if hot, replays the rollback journal for the
// database at dbPath, then deletes it. It is a no-op when there is no journal
// or it is not hot.
//
// A journal is hot iff all of:
//  1. "<dbPath>-journal" exists;
//  2. no other writer holds RESERVED -- enforced by taking RESERVED for the
//     whole recovery: a journal belonging to a commit in flight is live, and
//     rolling it back would undo that commit;
//  3. the db file is non-empty (otherwise the journal is stale and deleted);
//  4. the journal's first byte is nonzero (finalized journals are zeroed).
//
// A header validation failure (bad magic, out-of-range sizes) is a no-op, as
// in SQLite.
//
// # Locking
//
// Everything the replay decision relies on is read after RESERVED is held. A
// live commit's journal is byte-identical to a hot one, so a reader that read
// it unlocked, then took RESERVED after the writer finished, would replay a
// stale snapshot over committed data (TestHotJournalRecoveryRaceDestroysCommittedRows).
// Lengths derived from an earlier stat could also panic if the journal shrank
// (TestRecoverHotJournalShrinkRaceDoesNotPanic).
//
// Before taking the lock there is one unlocked, read-only content filter over
// the journal's own bytes (size, first byte, magic, page/sector sizes), as C's
// hasHotJournal (pager.c:5180-5262) never opens the db for write just because
// a journal exists. A "not a candidate" verdict returns without opening dbPath
// read-write, so persist/truncate artifacts on a read-only database still
// read (TestRecoverHotJournalReadOnlyDBWithInertJournal). Only a plausible
// candidate escalates to O_RDWR + RESERVED, after which everything is
// re-derived under the lock.
//
// The filter is safe: its read can only be racy while a writer is live, and
// then RESERVED fails and the result is a no-op anyway. With no writer the
// bytes are static, and it applies the same checks the locked pass does. A
// false positive costs one harmless lock attempt, as C's own unlocked
// pre-check allows (ticket #3883, pager.c:5201-5208).
//
// C escalates to EXCLUSIVE before playback (pager.c:5339-5354); this takes
// RESERVED as the liveness test instead. A C writer holds RESERVED from before
// any journal content until its commit point, so RESERVED held elsewhere means
// the journal is live.
//
// Not closed: a reader already holding SHARED from before recovery starts can
// see the rollback mid-flight, because playback is RESERVED-scoped rather than
// EXCLUSIVE-scoped. Closing it needs the caller's lock state threaded in.
func recoverHotJournal(dbPath string) error {
	jpath := dbPath + journalSuffix

	// Fast path ONLY: an unlocked existence check. This is exactly the
	// status C SQLite gives its own sqlite3OsAccess heuristic
	// (pager.c:5196) -- the C explicitly licenses this precise kind of false
	// positive (pager.c:5201-5208) BECAUSE everything that follows is
	// properly locked. If the journal does not exist at all, there is
	// nothing further to do; any other outcome (it exists now, or comes and
	// goes before RESERVED is acquired below) is re-decided entirely under
	// the lock, never trusted from here.
	if _, err := os.Stat(jpath); err != nil {
		if os.IsNotExist(err) {
			return nil // (1) no journal
		}
		return err
	}

	// Fast path, part two, still entirely read-only and still NEVER trusted
	// for the replay decision itself (see D1-round-2 above): a CONTENT
	// FILTER over the journal's own header bytes, deciding whether this is
	// even a plausible hot-journal candidate before dbPath is ever opened
	// for write. journal_mode=persist's zeroed header and journal_mode=
	// truncate's 0-byte post-commit artifact both fail this filter and
	// return here -- no O_RDWR open of dbPath, so a read-only-permissioned
	// db with either artifact stays readable, matching C SQLite
	// (pager.c:5230-5240 opens the journal READONLY for exactly this) and
	// pre-D1 main.
	hdr, err := peekJournalHeader(jpath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // deleted between the two stats above -- nothing to do
		}
		return err
	}
	if !looksLikeHotJournalCandidate(hdr) {
		return nil // provably inert by the SAME checks step 3 below performs
	}

	dbf, err := os.OpenFile(dbPath, os.O_RDWR, 0644)
	if err != nil {
		if os.IsNotExist(err) {
			// Journal but no database file to open at all: nothing to roll
			// back to, and nothing to take a lock ON either (a byte-range
			// lock needs an open fd onto the file it protects). A writer
			// never creates a journal before the db file it belongs to
			// already exists, so this is not a live-commit shape RESERVED
			// needs to guard -- drop the stale journal.
			if rerr := os.Remove(jpath); rerr != nil && !os.IsNotExist(rerr) {
				return rerr
			}
			return nil
		}
		return err
	}

	// Condition (2), and the gate for everything below. Taking RESERVED both
	// tests liveness (failing immediately if a writer holds it) and holds the
	// exclusion across the page writes, truncate and fsync. Failing to
	// acquire is not an error: the journal is not hot.
	//
	// RESERVED is held through the final delete too: dbf stays open until
	// return, and closing it would drop the OFD lock. Releasing before the
	// delete let another writer take RESERVED and create its own journal at
	// this path, which the delayed delete then removed mid-commit
	// (TestRecoverHotJournalFinalDeleteDoesNotDestroyLiveWriter). C likewise
	// deletes a stale journal only while holding RESERVED (pager.c:5224-5231).
	// Since a live journal's writer must hold RESERVED, anything at jpath
	// while we hold it is safe to delete.
	//
	// A zero timeout keeps this non-blocking: opening a database must not
	// stall behind another connection's commit to find nothing to recover.
	ok, err := filelock.LockRange(dbf, filelock.ReservedByte, 1, true)
	if err != nil || !ok {
		dbf.Close()
		return err // !ok: a live journal, owned by an in-flight commit
	}
	defer filelock.UnlockRange(dbf, filelock.ReservedByte, 1)

	// Everything below re-derives its own facts from here on -- the db
	// size, the journal's existence/size, and the journal's actual content
	// -- rather than trusting anything observed before RESERVED was held.

	dbInfo, err := dbf.Stat()
	if err != nil {
		dbf.Close()
		return err
	}
	if dbInfo.Size() == 0 {
		// (3) empty db -> not hot; the journal is stale. Closing X1b: real
		// SQLite takes RESERVED before this exact delete too
		// ("if( pagerLockDb(pPager, RESERVED_LOCK)==SQLITE_OK ){
		// sqlite3OsDelete(...) }", pager.c:5224-5231) -- unlike the
		// EXCLUSIVE-scoped playback path D1 above explains musql cannot
		// literally port, THIS specific delete's own liveness test is
		// RESERVED in C SQLite too, which is exactly what musql already
		// holds by the time it reaches here. D2 (above): the delete runs
		// BEFORE dbf.Close(), not after -- RESERVED stays held across it, so
		// no other writer can have created a fresh, live journal at jpath in
		// between.
		if err := os.Remove(jpath); err != nil && !os.IsNotExist(err) {
			dbf.Close()
			return err
		}
		return dbf.Close()
	}

	// Re-stat the journal UNDER the lock: it may have legitimately been
	// deleted by its owning writer between the fast-path check above and
	// RESERVED being granted here -- that is now a clean no-op (real
	// SQLite's own normal case, pager.c:5366-5369), not an error.
	jinfo, err := os.Stat(jpath)
	if err != nil {
		dbf.Close()
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if jinfo.Size() < journalHeaderSize {
		// Too small to hold a header; treat as not a usable journal (no-op).
		dbf.Close()
		return nil
	}

	jbuf, err := os.ReadFile(jpath)
	if err != nil {
		dbf.Close()
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Closing X3: re-validate against the length of the buffer JUST READ,
	// not jinfo.Size() above -- this is what actually prevents the
	// jbuf[0]/jbuf[0:8] accesses below from running off the end of the real
	// buffer, regardless of what any earlier stat claimed.
	if len(jbuf) < journalHeaderSize {
		dbf.Close()
		return nil
	}
	// (4) first byte nonzero. A zeroed header means the journal was finalized;
	// leave it (a later write will clean it) and do NOT roll back.
	if jbuf[0] == 0 {
		dbf.Close()
		return nil
	}
	if !bytesEqual(jbuf[0:8], journalMagic) {
		dbf.Close()
		return nil // bad magic -> no-op
	}

	nRec := binary.BigEndian.Uint32(jbuf[8:12])
	cksumInit := binary.BigEndian.Uint32(jbuf[12:16])
	dbSize := binary.BigEndian.Uint32(jbuf[16:20])
	sectorSize := binary.BigEndian.Uint32(jbuf[20:24])
	pageSize := binary.BigEndian.Uint32(jbuf[24:28])

	if !isPowerOfTwo(pageSize) || pageSize < 512 || pageSize > 65536 {
		dbf.Close()
		return nil
	}
	if !isPowerOfTwo(sectorSize) || sectorSize < 32 || sectorSize > 65536 {
		dbf.Close()
		return nil
	}

	recSize := 4 + int(pageSize) + 4
	for i := uint32(0); i < nRec; i++ {
		off := int(sectorSize) + int(i)*recSize
		if off+recSize > len(jbuf) {
			break // journal truncated mid-record -> stop replay
		}
		rec := jbuf[off : off+recSize]
		pgno := binary.BigEndian.Uint32(rec[0:4])
		data := rec[4 : 4+pageSize]
		cksum := binary.BigEndian.Uint32(rec[4+pageSize:])
		if journalCksum(cksumInit, data, pageSize) != cksum {
			break // bad checksum -> the record was not fully written; stop
		}
		if pgno == 0 {
			continue
		}
		if _, err := dbf.WriteAt(data, int64(pgno-1)*int64(pageSize)); err != nil {
			dbf.Close()
			return fmt.Errorf("engine: hot-journal recovery: restoring page %d: %w", pgno, err)
		}
	}

	// Truncate the db back to its original page count and fsync the restored
	// content before removing the journal. dbf stays OPEN (RESERVED stays
	// HELD) through the delete and syncDir below -- see D2 above -- and is
	// only closed at this function's very end.
	if err := dbf.Truncate(int64(dbSize) * int64(pageSize)); err != nil {
		dbf.Close()
		return err
	}
	if err := dbf.Sync(); err != nil {
		dbf.Close()
		return err
	}

	// Deleting the journal completes recovery (mirrors the writer's commit
	// point in reverse). RESERVED is STILL HELD here (D2 above: dbf has not
	// been closed yet), which is what makes this delete SAFE unconditionally
	// -- no live writer can hold a journal at jpath while we hold RESERVED
	// (a writer takes RESERVED before writing any journal content and keeps it
	// through the commit). os.Remove still tolerates ENOENT,
	// unchanged from before D2: an EARLIER, already-abandoned writer's own
	// journal.finish() may have already deleted this exact path (a
	// completely ordinary, non-hazardous case), and that remains a success
	// condition, not an error.
	if err := os.Remove(jpath); err != nil && !os.IsNotExist(err) {
		dbf.Close()
		return err
	}
	// syncDir opens and syncs the PARENT directory, not jpath itself (see
	// syncDir, journal.go) -- it is unaffected by whether jpath's dirent was
	// just removed by us or by an earlier, already-abandoned writer, and
	// already tolerates a missing directory by returning nil. No related
	// failure mode reachable here needs handling beyond what it already
	// does. dbf.Close() -- and with it, the release of RESERVED -- comes
	// LAST, after both the delete and the directory sync have completed.
	if err := syncDir(jpath); err != nil {
		dbf.Close()
		return err
	}
	return dbf.Close()
}

// peekJournalHeader opens jpath READ-ONLY (never O_RDWR, and never touching
// dbPath) and returns up to journalHeaderSize bytes from its start. A short
// read (fewer than journalHeaderSize bytes, including zero) is NOT an error:
// it is returned as a short slice, which looksLikeHotJournalCandidate treats
// as "not a candidate" -- exactly matching the len(jbuf) < journalHeaderSize
// no-op step 3 performs on the buffer it reads later under RESERVED. Only a
// genuine I/O error (including the journal having been deleted since the
// caller's existence check -- os.IsNotExist) is returned as err.
func peekJournalHeader(jpath string) ([]byte, error) {
	f, err := os.Open(jpath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, journalHeaderSize)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// looksLikeHotJournalCandidate applies the SAME header-level checks step 3 of
// recoverHotJournal performs under RESERVED (size >= journalHeaderSize,
// unfinalized first byte, valid magic, sane page/sector sizes) to bytes read
// UNLOCKED, purely to decide whether escalating to an O_RDWR open of dbPath
// is even worth attempting. See D1-round-2 above for why reusing the exact
// same criteria here can never produce a false negative for a genuinely
// static hot journal, and why a false positive is harmless (RESERVED
// acquisition then re-derives everything from scratch and may cleanly no-op
// there instead).
func looksLikeHotJournalCandidate(hdr []byte) bool {
	if len(hdr) < journalHeaderSize {
		return false
	}
	if hdr[0] == 0 {
		return false // finalized/zeroed header (journal_mode=persist's artifact)
	}
	if !bytesEqual(hdr[0:8], journalMagic) {
		return false
	}
	sectorSize := binary.BigEndian.Uint32(hdr[20:24])
	pageSize := binary.BigEndian.Uint32(hdr[24:28])
	if !isPowerOfTwo(pageSize) || pageSize < 512 || pageSize > 65536 {
		return false
	}
	if !isPowerOfTwo(sectorSize) || sectorSize < 32 || sectorSize > 65536 {
		return false
	}
	return true
}
