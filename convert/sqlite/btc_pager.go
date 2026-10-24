package sqlite

import (
	"fmt"
	"os"

	"github.com/samyfodil/musql/internal/fsio"
)

// btPager is a page cache implementing SQLite pager semantics: zero-filled pages
// when fetching NOCONTENT, dontWrite suppression until savepoint release, and
// selective writes on commit.
type btPager struct {
	pageSize int

	// read fetches a page from the file.
	read func(pgno uint32, buf []byte) error

	dbOrigSize uint32 // Pager.dbOrigSize: pages in the file when the transaction began
	dbSize     uint32 // Pager.dbSize

	pages    map[uint32]*btPage
	modified bool // a page has been written: PAGER_WRITER_CACHEMOD or later

	// inJournal, mainJournal, and subJournal track pages for rollback and savepoints.
	inJournal   map[uint32]bool
	mainJournal []btJournalRec
	subJournal  []btJournalRec
	savepoints  []btSavepoint

	// maxPages is the cache size past which shrink writes pages out to file,
	// the database file being built, each at its own offset; 0 never does.
	// state holds, per page number, what a page out of the cache was
	// (btStSpilled and its flags) and whether its place in the file has been
	// written (btStWritten), which the commit must undo for a page whose
	// final state is not to be written.
	maxPages int
	file     *os.File
	state    []uint8
}

// btPager.state bits.
const (
	btStSpilled = 1 << iota
	btStDirty
	btStWriteable
	btStDontWrite
	btStWritten
)

func (pg *btPager) stateOf(pgno uint32) uint8 {
	if uint64(pgno) < uint64(len(pg.state)) {
		return pg.state[pgno]
	}
	return 0
}

func (pg *btPager) setState(pgno uint32, st uint8) {
	if uint64(pgno) >= uint64(len(pg.state)) {
		if st == 0 {
			return
		}
		pg.state = append(pg.state, make([]uint8, int(pgno)+1-len(pg.state))...)
	}
	pg.state[pgno] = st
}

func (pg *btPager) offset(pgno uint32) int64 { return int64(pgno-1) * int64(pg.pageSize) }

// btJournalRec is one journal record: a page number and the page it restores.
type btJournalRec struct {
	pgno uint32
	data []byte
}

// btSavepoint is PagerSavepoint (pager.c).
type btSavepoint struct {
	nOrig             uint32
	inSavepoint       map[uint32]bool
	truncateOnRelease bool
}

// btPage is a cached page with metadata.
type btPage struct {
	mem       memPage
	loaded    bool // content has been loaded or initialized
	dirty     bool
	writeable bool // PGHDR_WRITEABLE
	dontWrite bool
}

// newBtPager creates a pager for a write transaction.
func newBtPager(pageSize int, dbSize uint32, journal bool, read func(uint32, []byte) error) *btPager {
	pg := &btPager{pageSize: pageSize, read: read, dbOrigSize: dbSize, dbSize: dbSize, pages: map[uint32]*btPage{}}
	if journal {
		pg.inJournal = map[uint32]bool{}
	}
	return pg
}

// get fetches or creates a page, loading it if required (pager.c:5581).
func (pg *btPager) get(bt *btShared, pgno uint32, noContent bool) (*memPage, error) {
	if pgno == 0 {
		return nil, errBtCorrupt
	}
	p := pg.pages[pgno]
	if p == nil {
		p = &btPage{}
		p.mem.aData = make([]byte, pg.pageSize)
		pg.pages[pgno] = p
		if st := pg.stateOf(pgno); st&btStSpilled != 0 {
			pg.setState(pgno, st&btStWritten)
			p.dirty, p.writeable, p.dontWrite = st&btStDirty != 0, st&btStWriteable != 0, st&btStDontWrite != 0
			if !noContent {
				if _, err := fsio.ReadAt(pg.file, p.mem.aData, pg.offset(pgno)); err != nil {
					return nil, fmt.Errorf("sqlite: export: reading back page %d: %w", pgno, err)
				}
				p.loaded = true
			}
		}
	}
	m := &p.mem
	if m.pgno != pgno {
		m.pgno, m.bt = pgno, bt
		m.hdrOffset = 0
		if pgno == 1 {
			m.hdrOffset = HeaderSize
		}
	}
	if p.loaded && !noContent {
		return m, nil
	}
	p.loaded = true
	if pg.dbSize < pgno || noContent {
		if noContent {
			if pgno <= pg.dbOrigSize && pg.inJournal != nil {
				pg.inJournal[pgno] = true
			}
			pg.addToSavepoints(pgno)
		}
		clear(m.aData)
		return m, nil
	}
	if pgno > pg.dbOrigSize {
		// Past the end of the file: initialize as zeros.
		clear(m.aData)
		return m, nil
	}
	if err := pg.read(pgno, m.aData); err != nil {
		return nil, err
	}
	return m, nil
}

// write marks a page dirty and journals its first state (pager.c:6280).
func (pg *btPager) write(m *memPage) {
	p := pg.pages[m.pgno]
	if p.writeable && pg.dbSize >= m.pgno {
		pg.subjournalIfRequired(m)
		return
	}
	p.dirty = true
	p.dontWrite = false
	pg.modified = true
	if pg.inJournal != nil && !pg.inJournal[m.pgno] && m.pgno <= pg.dbOrigSize {
		pg.mainJournal = append(pg.mainJournal, btJournalRec{m.pgno, append([]byte(nil), m.aData...)})
		pg.inJournal[m.pgno] = true
		pg.addToSavepoints(m.pgno)
	}
	p.writeable = true
	pg.subjournalIfRequired(m)
	pg.dbSize = max(pg.dbSize, m.pgno)
}

// addToSavepoints records a page as modified within active savepoints (pager.c:1820).
func (pg *btPager) addToSavepoints(pgno uint32) {
	for i := range pg.savepoints {
		if sp := &pg.savepoints[i]; pgno <= sp.nOrig {
			sp.inSavepoint[pgno] = true
		}
	}
}

// subjournalIfRequired journals a page for savepoint release (pager.c:4628).
func (pg *btPager) subjournalIfRequired(m *memPage) {
	for i := range pg.savepoints {
		sp := &pg.savepoints[i]
		if sp.nOrig >= m.pgno && !sp.inSavepoint[m.pgno] {
			for j := i + 1; j < len(pg.savepoints); j++ {
				pg.savepoints[j].truncateOnRelease = false
			}
			pg.subJournal = append(pg.subJournal, btJournalRec{m.pgno, append([]byte(nil), m.aData...)})
			pg.addToSavepoints(m.pgno)
			return
		}
	}
}

// dontWrite marks a page to be excluded from commit (pager.c:6329).
func (pg *btPager) dontWrite(m *memPage) {
	if p := pg.pages[m.pgno]; p.dirty && len(pg.savepoints) == 0 {
		p.dontWrite = true
		p.writeable = false
	}
}

// btSQLiteVersionNumber is stamped at offset 96 of page 1 on commit.
const btSQLiteVersionNumber = 3053003

// commit writes the database out to file (pager_write_pagelist, pager.c): a
// page whose final state is dirty and not dontWrite holds its content, every
// other page up to dbSize zeros, and the file ends at dbSize.
func (pg *btPager) commit() error {
	zero := make([]byte, pg.pageSize)
	for pgno, p := range pg.pages {
		if pgno > pg.dbSize {
			continue
		}
		var err error
		switch {
		case p.dirty && !p.dontWrite:
			_, err = fsio.WriteAt(pg.file, p.mem.aData, pg.offset(pgno))
			pg.setState(pgno, btStWritten)
		case pg.stateOf(pgno)&btStWritten != 0:
			_, err = fsio.WriteAt(pg.file, zero, pg.offset(pgno))
			pg.setState(pgno, 0)
		}
		if err != nil {
			return fmt.Errorf("sqlite: export: writing page %d: %w", pgno, err)
		}
	}
	for i := 1; i < len(pg.state) && uint64(i) <= uint64(pg.dbSize); i++ {
		pgno := uint32(i)
		st := pg.state[i]
		if st&btStWritten == 0 || pg.pages[pgno] != nil {
			continue
		}
		if st&btStSpilled != 0 && st&btStDirty != 0 && st&btStDontWrite == 0 {
			continue
		}
		if _, err := fsio.WriteAt(pg.file, zero, pg.offset(pgno)); err != nil {
			return fmt.Errorf("sqlite: export: writing page %d: %w", pgno, err)
		}
	}
	return pg.file.Truncate(int64(pg.dbSize) * int64(pg.pageSize))
}

// shrink writes pages out of a cache past maxPages, dirty ones to their place
// in file, until a quarter of it is free. Only a cursor's pages and page 1
// stay: it runs between operations, when nothing else holds a page.
func (pg *btPager) shrink(bt *btShared) error {
	if pg.maxPages == 0 || len(pg.pages) <= pg.maxPages {
		return nil
	}
	held := map[uint32]bool{1: true}
	for _, c := range bt.cursors {
		for i := 0; i < c.iPage; i++ {
			held[c.apPage[i].pgno] = true
		}
		if c.iPage >= 0 && c.page != nil {
			held[c.page.pgno] = true
		}
	}
	target := pg.maxPages * 3 / 4
	for pgno, p := range pg.pages {
		if len(pg.pages) <= target {
			break
		}
		if held[pgno] {
			continue
		}
		if p.loaded && p.dirty {
			if _, err := fsio.WriteAt(pg.file, p.mem.aData, pg.offset(pgno)); err != nil {
				return fmt.Errorf("sqlite: export: writing page %d: %w", pgno, err)
			}
			st := uint8(btStSpilled | btStDirty | btStWritten)
			if p.writeable {
				st |= btStWriteable
			}
			if p.dontWrite {
				st |= btStDontWrite
			}
			pg.setState(pgno, st)
		}
		delete(pg.pages, pgno)
	}
	return nil
}

// lookup is btreePageLookup: page pgno if the cache holds it, else nil. A page
// shrink wrote out counts as held, as a cache that never shrinks holds it.
func (pg *btPager) lookup(bt *btShared, pgno uint32) (*memPage, error) {
	if p := pg.pages[pgno]; p != nil && p.loaded {
		return &p.mem, nil
	}
	if pg.stateOf(pgno)&btStSpilled != 0 {
		return pg.get(bt, pgno, false)
	}
	return nil, nil
}

// movePage reassigns a page's number, dropping any cached page at the new number (pager.c:7204).
func (pg *btPager) movePage(m *memPage, pgno uint32) {
	p := pg.pages[m.pgno]
	// Dirty pages are subjournaled under their old number first.
	if p.dirty {
		pg.subjournalIfRequired(m)
	}
	delete(pg.pages, m.pgno)
	pg.pages[pgno] = p
	pg.setState(pgno, pg.stateOf(pgno)&btStWritten)
	m.pgno = pgno
	p.dirty = true
	p.dontWrite = false
}

// rekey swaps page numbers between two pages (btree.c:8726-8737).
func (pg *btPager) rekey(a, b *memPage) {
	pa, pb := pg.pages[a.pgno], pg.pages[b.pgno]
	pg.pages[a.pgno], pg.pages[b.pgno] = pb, pa
	a.pgno, b.pgno = b.pgno, a.pgno
	pa.dirty, pb.dirty = pb.dirty, pa.dirty
	pa.dontWrite, pb.dontWrite = pb.dontWrite, pa.dontWrite
}
