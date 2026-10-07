package engine

import (
	"cmp"
	"slices"
)

// The CATALOG a segment file has to carry to be a database rather than a pile of
// rows: every index, view and trigger, every AUTOINCREMENT counter, and the
// database values the PRAGMAs of the same names read. A file without them holds
// rows and no schema -- which is what a round trip once produced, the same rows
// and none of the indexes, views, triggers or counters, until
// TestRoundTripKeepsTheWholeCatalog compared the two catalogs.

// ConvertedObject is one non-table schema object, exactly as sqlite_master holds
// it.
type ConvertedObject struct {
	Type    string // "index", "view" or "trigger"
	Name    string
	TblName string
	SQL     string
	// Rank is ConvertedTable.Rank for a catalog object.
	Rank uint32
	// Rowid is ConvertedTable.Rowid for a catalog object.
	Rowid int64
}

// ConvertedSequence is one AUTOINCREMENT counter: the highest rowid the table has
// ever handed out, which is NOT derivable from the rows that remain.
//
// A table whose last row was deleted keeps its counter, so the next insert does
// not reuse that rowid. Re-inserting the surviving rows sets the counter to the
// largest of them, which is how a round trip handed back a rowid the source had
// already used: seq 3 came back as 2 (TestRoundTripKeepsAutoincrementState).
type ConvertedSequence struct {
	Table string
	Seq   int64
}

// ConvertedCatalog is everything about a database that is not a table's rows.
type ConvertedCatalog struct {
	Objects   []ConvertedObject
	Sequences []ConvertedSequence

	// SchemaVersion is the database's schema cookie -- what "PRAGMA
	// schema_version" answers, and what a client watches to notice that someone
	// else changed the schema.
	//
	// It has to live here because this format has no SQLite header to keep it
	// in, and without it every segment database answered 0 forever: a client
	// polling for schema changes would never see one, and C answered 1, 2, 4 for
	// the same three DDL statements.
	SchemaVersion uint32

	// UserVersion and ApplicationID are "PRAGMA user_version" and "PRAGMA
	// application_id": two 32-bit words an application owns, which C SQLite
	// keeps in the file header (offsets 60 and 68) and this format has to keep
	// here for the same reason SchemaVersion is here -- there is no header.
	//
	// They are the application's own data, so losing them is losing data:
	// "PRAGMA user_version=11" then reading it back answered 0 on this format,
	// and every consumer that stores a migration number there -- the standard use
	// -- would have re-run every migration on every open.
	UserVersion    uint32
	ApplicationID  uint32

	// Encoding is the database's TEXT ENCODING -- "PRAGMA encoding", which C
	// SQLite keeps in header byte 56 and which is fixed for a database's life
	// once it has a schema.
	//
	// It is here for the same reason SchemaVersion is: this format has no SQLite
	// header, and WITHOUT it the pragma had to be declined -- which is not a
	// property of the format, it is a field nobody had written yet. A database
	// created with "PRAGMA encoding='UTF-16le'" stores its text in UTF-16 here
	// exactly as C's does, and the values the engine hands back are in the
	// encoding this field names (see DB.encoding, utf16.go, which every value
	// path already takes as a parameter).
	//
	// 0 means UTF-8, which is what every file written before this field existed
	// meant.
	Encoding uint32

	// PageSize is "PRAGMA page_size": the size a database was CREATED at, which C
	// SQLite keeps in header bytes 16-17 and reports forever after.
	//
	// This format has no pages, but the VALUE is a property of the database like
	// any other and two things read it: the pragma's own getter, and fts3, whose
	// segment-spill budget is derived from it (so a database created at 512 spills
	// into %_segments exactly where C's does). 0 means the default 4096, which is
	// what every file written before this field existed meant.
	PageSize uint32

	// AutoVacuumPlus1 is "PRAGMA auto_vacuum" + 1, so the zero value means "not
	// recorded" and reads back as C's default of 0 (NONE).
	//
	// C keeps the mode in the file (header offset 52's largest-root-page slot
	// decides whether it is capable, meta 7 which mode), and the next connection
	// reads it back -- which is the whole reason this cannot be a per-connection
	// value here either. What the MODE means on this format is already true of it:
	// a rewrite re-lays the database and gives back everything churn retained, so
	// a segment database is always in the state FULL promises. The value is
	// recorded so the pragma answers what C answers.
	AutoVacuumPlus1 uint32

	// JournalWAL is 1 when the database is in "PRAGMA journal_mode=wal". In C that
	// is a property of the FILE (header bytes 18/19 = 2), which the next
	// connection reads back, so it is recorded here the same way. This format's
	// delta IS a write-ahead log -- an append-only record whose batch trailer is
	// the commit, replayed over the segments and folded in by a rewrite, which is
	// the checkpoint -- so the mode names a mechanism the file really has.
	JournalWAL uint32

	// RootEdits are the catalog rows whose rootpage a direct sqlite_schema write
	// pointed at storage other than their own -- C's catalog simply holds the
	// number, and its next schema load reads that b-tree. Root is the RENDERED
	// number (catalogRootpages), which the load resolves the same way a RESET
	// does (segmentRootpageEditPlan): an alias it can serve exactly, C's
	// "invalid rootpage" for a sibling index, a decline for the rest. -1 is a
	// value that is not an integer, which C's load calls an invalid rootpage.
	RootEdits []ConvertedRootEdit

	// CaptureGuard, when not empty, refuses writes from any connection that does
	// not capture its changes, with this text as the error (DB.SetCaptureGuard).
	// It is the file's own because the refusal has to hold for a program that
	// never links the capture's consumer: that program is exactly the one whose
	// writes would go unseen. The consumer chooses the text; the engine and the
	// driver know nothing of what it is for.
	CaptureGuard string
}

// ConvertedRootEdit is one ConvertedCatalog.RootEdits entry.
type ConvertedRootEdit struct {
	Name string
	Root int64
}

// SegmentFileJournalWAL reports whether the segment file at path records
// "journal_mode=wal" (ConvertedCatalog.JournalWAL) -- what the driver asks of a
// SQLite file's header bytes 18/19, for a file that has none.
func SegmentFileJournalWAL(path string) bool {
	f, err := OpenSegmentFile(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return f.Catalog().JournalWAL != 0
}

// catalogRanks turns each catalog entry's schemaSeq into its Rank: its 1-based
// position when every entry is sorted by creation.
func catalogRanks(seqs []uint64) []uint32 {
	order := make([]int, len(seqs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(seqs[a], seqs[b]) })
	ranks := make([]uint32, len(seqs))
	for pos, i := range order {
		ranks[i] = uint32(pos + 1)
	}
	return ranks
}

// catalogOrderKey places one sqlite_schema row: its entry's Rank, and for an
// automatic index -- which the file does not carry, the table's own DDL
// rebuilds it -- the table's Rank and its constraint ordinal. C inserts those
// rows right after the table's own, in constraint order: sqlite3StartTable
// takes the table's rowid for a placeholder row first (build.c:1377), then each
// constraint's sqlite3CreateIndex inserts its own (build.c:4460).
type catalogOrderKey struct {
	rank uint32
	sub  int
}

// sortCatalogRows orders rows by keys, or leaves them as they are when any
// entry has no Rank (a file written before the field existed).
func sortCatalogRows(rows []SchemaRow, keys []catalogOrderKey) []SchemaRow {
	for _, k := range keys {
		if k.rank == 0 {
			return rows
		}
	}
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		ka, kb := keys[a], keys[b]
		if c := cmp.Compare(ka.rank, kb.rank); c != 0 {
			return c
		}
		return cmp.Compare(ka.sub, kb.sub)
	})
	out := make([]SchemaRow, len(rows))
	for i, j := range idx {
		out[i] = rows[j]
	}
	return out
}
