package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// THE TEMP DATABASE'S FILE.
//
// The temp database is a segment file and delta in the temp directory, written by
// the same code as main's, with committed rows read through the same row store.
// Commits after main's: appending changes or rewriting on catalog changes.
// Failed main commits leave the temp file untouched (atomic). The connection claims
// and removes the file; otherwise it's removed at session close.

// tempStoreDirForTest overrides temp file location for tests; empty uses system temp.
var tempStoreDirForTest string

// tempPair is the TEMP database's file pair.
type tempPair struct {
	path string
	// src is the committed pair, read by every temp table's row store.
	src *ReadOnlyPager
	// tableIndex and ipkOf are the file's table directory.
	tableIndex map[string]int
	ipkOf      map[string]int
	// fp is the temp catalog; commit rewrites if it differs.
	fp string
	// Identity and append point, as Session keeps main's.
	baseCtr, endCtr uint32
	appendAt        SegDeltaAppendState
	// claimed: connection owns this pair and removes it.
	claimed bool
}

// tempInFile reports whether TEMP is a file (true) or in-memory (false).
func (db *DB) tempInFile() bool { return db.tempStore != 2 }

// takeTempChanges removes the TEMP tables' changes from the change log, leaving
// main's in order, and returns them -- the temp commit's input.
func (db *DB) takeTempChanges() []RowChange {
	var temp []RowChange
	kept := db.changeLog[:0]
	for _, ch := range db.changeLog {
		if ch.Temp {
			ch.Temp = false // a record of the TEMP file names its table plainly
			temp = append(temp, ch)
			continue
		}
		kept = append(kept, ch)
	}
	db.changeLog = kept
	return temp
}

// tempCatalogFingerprint is the TEMP catalog as a string: every temp object and
// the temp header. The file is rewritten whenever it moves.
func (db *DB) tempCatalogFingerprint() string {
	var b strings.Builder
	h := db.tempHdr()
	b.WriteString("C" + strconv.FormatUint(uint64(h.cookie), 10) + "U" + strconv.FormatUint(uint64(h.userVersion), 10) +
		"A" + strconv.FormatUint(uint64(h.applicationID), 10))
	for _, r := range db.segmentSchemaRows() {
		if r.Temp {
			b.WriteString("\x00" + r.Type + "\x01" + r.Name + "\x01" + r.SQL)
		}
	}
	return b.String()
}

// tempMutatedOutsideChangeLog is mutatedOutsideChangeLog for the TEMP tables: a
// temp row written without a change record cannot become a delta record.
func (db *DB) tempMutatedOutsideChangeLog(changes []RowChange) bool {
	described := map[string]bool{}
	for _, c := range changes {
		described[strings.ToLower(c.Table)] = true
	}
	for _, t := range db.tables {
		if t.isTemp && t.rowsWrittenSinceCommit && t.rows != nil && !described[strings.ToLower(t.name)] {
			return true
		}
	}
	return false
}

// commitTempFile writes this commit's TEMP changes to the temp file. It runs after
// main's commit succeeded.
func (db *DB) commitTempFile(changes []RowChange) error {
	if !db.tempInFile() || (db.tempPair == nil && !db.holdsAnyTempObject()) {
		return nil
	}
	defer func() {
		for _, t := range db.tables {
			if t.isTemp {
				t.rowsWrittenSinceCommit = false
			}
		}
	}()
	fp := db.tempCatalogFingerprint()
	tp := db.tempPair
	if tp == nil || tp.fp != fp || db.tempMutatedOutsideChangeLog(changes) {
		return db.rewriteTempFile(fp)
	}
	recs, err := segDeltaRecordsInto(nil, changes, tp.tableIndex, tp.ipkOf, db.storeOf(true))
	if err != nil || len(recs) == 0 {
		return err
	}
	tp.endCtr++
	// No fsync: temp database doesn't outlive the connection.
	if aerr := appendSegmentDeltaAt(tp.path, tp.baseCtr, 0, recs, tp.endCtr, 0, &tp.appendAt, syncNone); aerr != nil {
		tp.endCtr--
		tp.fp = "" // the next commit rewrites the file from the live rows
		return fmt.Errorf("engine: TEMP database: %w", aerr)
	}
	return nil
}

// rewriteTempFile writes the whole TEMP database to its file and rebases every
// temp table onto it: their committed rows are the file's now, and nothing of
// theirs is held in memory.
func (db *DB) rewriteTempFile(fp string) error {
	tp := db.tempPair
	if tp == nil {
		f, cerr := os.CreateTemp(tempStoreDirForTest, "musql-temp-*.musq")
		if cerr != nil {
			return fmt.Errorf("engine: TEMP database: %w", cerr)
		}
		f.Close()
		tp = &tempPair{path: f.Name()}
		db.tempPair = tp
	}
	w, werr := newSegFileWriter(filepath.Dir(tp.path))
	if werr != nil {
		return fmt.Errorf("engine: TEMP database: %w", werr)
	}
	defer w.discard()
	tables, cat, err := db.segmentFileContentsOf(true, w)
	if err != nil {
		return err
	}
	ctr := tp.endCtr + 1
	tmp := tp.path + ".new"
	if werr := w.finish(tmp, tables, cat, ctr, 0); werr != nil {
		os.Remove(tmp)
		tp.fp = ""
		return fmt.Errorf("engine: TEMP database: %w", werr)
	}
	if rerr := os.Rename(tmp, tp.path); rerr != nil {
		os.Remove(tmp)
		tp.fp = ""
		return fmt.Errorf("engine: TEMP database: %w", rerr)
	}
	os.Remove(segDeltaPath(tp.path))
	src, oerr := openSegmentsUnlocked(tp.path)
	if oerr != nil {
		tp.fp = ""
		return fmt.Errorf("engine: TEMP database: %w", oerr)
	}
	tableIndex, ipkOf := SegmentFileTableIndex(src.segs.file)
	for _, t := range db.tables {
		if !t.isTemp || t.aliasOf != "" {
			continue
		}
		if ti, ok := tableIndex[t.name]; ok {
			t.rows = newSegRowStoreAt(src, src.segs.file.RootOf(ti), t)
		}
	}
	old := tp.src
	tp.src, tp.tableIndex, tp.ipkOf, tp.fp = src, tableIndex, ipkOf, fp
	tp.baseCtr, tp.endCtr, tp.appendAt = ctr, ctr, SegDeltaAppendState{}
	if old != nil {
		old.Close()
	}
	return nil
}

// removeTempFile deletes the TEMP database's files.
func (tp *tempPair) remove() {
	if tp == nil {
		return
	}
	if tp.src != nil {
		tp.src.Close()
		tp.src = nil
	}
	os.Remove(tp.path)
	os.Remove(segDeltaPath(tp.path))
	os.Remove(tp.path + ".new")
}

// Close removes the connection's TEMP database file -- the connection is
// closing, and C deletes its temp database with it.
func (t *TempDatabase) Close() {
	if t != nil && t.pair != nil {
		t.pair.remove()
		t.pair = nil
	}
}

// dropTempFileIfUnclaimed removes the temp file of a session closing with no
// connection to hand it to.
func (db *DB) dropTempFileIfUnclaimed() {
	if db.tempPair != nil && !db.tempPair.claimed {
		db.tempPair.remove()
		db.tempPair = nil
	}
}
