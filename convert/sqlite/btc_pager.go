package sqlite

import (
	"fmt"
	"os"
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

	// maxPages is the cache size before spilling to a scratch file.
	maxPages  int
	spillFile *os.File
	spillEnd  int64
	spilled   map[uint32]btSpilled
	slots     map[uint32]int64 // scratch file offsets for spilled pages
}

// btSpilled tracks a page spilled to the scratch file.
type btSpilled struct {
	off                         int64
	dirty, writeable, dontWrite bool
}

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
		if s, ok := pg.spilled[pgno]; ok {
			delete(pg.spilled, pgno)
			p.dirty, p.writeable, p.dontWrite = s.dirty, s.writeable, s.dontWrite
			if !noContent {
				if _, err := pg.spillFile.ReadAt(p.mem.aData, s.off); err != nil {
					return nil, fmt.Errorf("engine: reading a spilled page: %w", err)
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

// commitInto applies dirty pages to a file buffer (pager_write_pagelist, pager.c).
func (pg *btPager) commitInto(image []byte) ([]byte, error) {
	size := int(pg.dbSize) * pg.pageSize
	if len(image) < size {
		image = append(image, make([]byte, size-len(image))...)
	}
	image = image[:size]
	for pgno, p := range pg.pages {
		if !p.dirty || p.dontWrite || pgno > pg.dbSize {
			continue
		}
		off := int(pgno-1) * pg.pageSize
		if off+pg.pageSize > len(image) {
			return nil, fmt.Errorf("engine: page %d past the end of a %d-page image", pgno, pg.dbSize)
		}
		copy(image[off:off+pg.pageSize], p.mem.aData)
	}
	for pgno, s := range pg.spilled {
		if !s.dirty || s.dontWrite || pgno > pg.dbSize {
			continue
		}
		off := int(pgno-1) * pg.pageSize
		if _, err := pg.spillFile.ReadAt(image[off:off+pg.pageSize], s.off); err != nil {
			return nil, fmt.Errorf("engine: reading a spilled page: %w", err)
		}
	}
	return image, nil
}

// shrink evicts pages to a scratch file when cache exceeds maxPages.
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
	if pg.spillFile == nil {
		f, err := os.CreateTemp("", "musql-pages-*")
		if err != nil {
			return fmt.Errorf("engine: page spill: %w", err)
		}
		os.Remove(f.Name()) // the open handle keeps it until the pager is gone
		pg.spillFile, pg.spilled, pg.slots = f, map[uint32]btSpilled{}, map[uint32]int64{}
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
			off, ok := pg.slots[pgno]
			if !ok {
				off = pg.spillEnd
				pg.spillEnd += int64(pg.pageSize)
				pg.slots[pgno] = off
			}
			if _, err := pg.spillFile.WriteAt(p.mem.aData, off); err != nil {
				return fmt.Errorf("engine: page spill: %w", err)
			}
			pg.spilled[pgno] = btSpilled{off, p.dirty, p.writeable, p.dontWrite}
		}
		delete(pg.pages, pgno)
	}
	return nil
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
	delete(pg.spilled, pgno)
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
