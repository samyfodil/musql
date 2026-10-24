package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Package replication implements snapshot bootstrap for nodes whose peers have
// pruned (ErrPruned). Pages are fetched in parts: ops, floors/ranges, clock.

// The parts of a snapshot, fetched in this order.
const (
	SnapshotOps   = 1
	SnapshotMeta  = 2
	SnapshotClock = 3
)

// ClockCell is one _repl_clock row.
type ClockCell struct {
	Tbl  string
	PK   []byte
	Col  string
	HLC  HLC
	Site string
	Val  []byte
}

// SnapshotPage is one page of one part; Next is the cursor for the following page.
type SnapshotPage struct {
	Ops    []Op
	Floors VersionVector
	Ranges map[string]uint64 // range owners: site -> its range's lo
	Cells  []ClockCell
	Next   []byte
}

// A page holds at most snapshotPageLen ops or cells, and stops at 16MB to stay
// within message limits.
const (
	snapshotPageLen   = 8192
	snapshotPageBytes = 16 << 20
)

// snapshotCursor is where the next page of a part starts: after the row of
// this rowid. A page is the rows after it in rowid order, which a rowid table
// is stored in, so serving one is a seek rather than a sort of the table.
// A cursor of an older format decodes as the start, and the part is sent again
// from there: every row of it merges idempotently.
type snapshotCursor struct {
	Rowid int64 `json:"r"`
}

// SnapshotPage serves one page of part after cursor (nil: the first).
func (s *Store) SnapshotPage(ctx context.Context, part int, cursor []byte) (SnapshotPage, error) {
	var page SnapshotPage
	var after snapshotCursor
	if cursor != nil {
		if err := json.Unmarshal(cursor, &after); err != nil {
			return page, err
		}
	}
	var err error
	switch part {
	case SnapshotOps:
		page.Next, err = s.pageAfter(ctx, `site, seq, hlc, tbl, pk, op, cells FROM main._repl_oplog`, after.Rowid,
			func() { page.Ops = page.Ops[:0] },
			func(scan func(...any) error) (int, error) {
				var op Op
				var hlc, kind int64
				var cells []byte
				if err := scan(&op.Site, &op.Seq, &hlc, &op.Tbl, &op.PK, &kind, &cells); err != nil {
					return 0, err
				}
				op.HLC, op.Kind = HLC(hlc), OpKind(kind)
				var err error
				if op.Cells, err = decodeCells(cells); err != nil {
					return 0, err
				}
				page.Ops = append(page.Ops, op)
				return len(op.Site) + len(op.Tbl) + len(op.PK) + len(cells) + 32, nil
			})
	case SnapshotMeta:
		page.Floors, page.Ranges = VersionVector{}, map[string]uint64{}
		rows, err := s.db.QueryContext(ctx, `SELECT site, seq FROM main._repl_floors`)
		if err != nil {
			return page, err
		}
		for rows.Next() {
			var site string
			var seq uint64
			if err := rows.Scan(&site, &seq); err != nil {
				rows.Close()
				return page, err
			}
			page.Floors[site] = seq
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return page, err
		}
		rows, err = s.db.QueryContext(ctx, `SELECT site, lo FROM main._repl_ranges`)
		if err != nil {
			return page, err
		}
		defer rows.Close()
		for rows.Next() {
			var site string
			var lo int64
			if err := rows.Scan(&site, &lo); err != nil {
				return page, err
			}
			page.Ranges[site] = uint64(lo)
		}
		return page, rows.Err()
	case SnapshotClock:
		page.Next, err = s.pageAfter(ctx, `tbl, pk, col, hlc, site, val FROM main._repl_clock`, after.Rowid,
			func() { page.Cells = page.Cells[:0] },
			func(scan func(...any) error) (int, error) {
				var c ClockCell
				var hlc int64
				if err := scan(&c.Tbl, &c.PK, &c.Col, &hlc, &c.Site, &c.Val); err != nil {
					return 0, err
				}
				c.HLC = HLC(hlc)
				page.Cells = append(page.Cells, c)
				return len(c.Tbl) + len(c.PK) + len(c.Col) + len(c.Site) + len(c.Val) + 16, nil
			})
	default:
		return page, fmt.Errorf("replication: unknown snapshot part %d", part)
	}
	return page, err
}

// pageAfter takes one page of "SELECT rowid, <from>": the rows after rowid
// after, in rowid order, each handed to take, which scans the columns after
// the rowid and reports the row's size. It returns the cursor of the next
// page, nil when this one is the last.
//
// The table's own order serves the page without sorting it. A page that comes
// back out of rowid order is read again sorted (reset discards what take kept):
// the cursor must be the largest rowid of every row the page could hold.
func (s *Store) pageAfter(ctx context.Context, from string, after int64, reset func(), take func(scan func(...any) error) (int, error)) ([]byte, error) {
	for _, order := range []string{``, ` ORDER BY rowid`} {
		last, more, sorted, err := s.readPage(ctx, `SELECT rowid, `+from+` WHERE rowid > ?`+order+` LIMIT ?`, after, take)
		if err != nil {
			return nil, err
		}
		if !sorted {
			reset()
			continue
		}
		if !more {
			return nil, nil
		}
		next, err := json.Marshal(snapshotCursor{Rowid: last})
		return next, err
	}
	return nil, errors.New("replication: a snapshot page sorted by rowid came back out of rowid order")
}

// readPage runs q for the rows after after, handing each to take until the
// page is full. sorted reports whether they came in rowid order, more whether
// a next page may hold rows, and last is the rowid of the last row taken.
func (s *Store) readPage(ctx context.Context, q string, after int64, take func(scan func(...any) error) (int, error)) (last int64, more, sorted bool, err error) {
	rows, err := s.db.QueryContext(ctx, q, after, snapshotPageLen)
	if err != nil {
		return 0, false, false, err
	}
	defer rows.Close()
	var rowid int64
	scan := func(dest ...any) error { return rows.Scan(append([]any{&rowid}, dest...)...) }
	last = after
	n, size := 0, 0
	for rows.Next() {
		if size >= snapshotPageBytes {
			return last, true, true, nil
		}
		k, err := take(scan)
		if err != nil {
			return 0, false, false, err
		}
		if rowid <= last {
			return 0, false, false, nil
		}
		last, size, n = rowid, size+k, n+1
	}
	return last, n == snapshotPageLen, true, rows.Err()
}

// bootstrap takes a snapshot from peer and merges it.
func (n *Node) bootstrap(ctx context.Context, peer PeerID) error {
	if err := n.snapshotOps(ctx, peer); err != nil {
		return err
	}
	meta, err := n.tr.GetSnapshot(ctx, peer, SnapshotMeta, nil)
	if err != nil {
		return err
	}
	if err := n.snapshotOps(ctx, peer); err != nil {
		return err
	}
	var cursor []byte
	for {
		page, err := n.tr.GetSnapshot(ctx, peer, SnapshotClock, cursor)
		if err != nil {
			return err
		}
		if err := n.store.mergeCells(ctx, page.Cells); err != nil {
			return err
		}
		if cursor = page.Next; cursor == nil {
			break
		}
	}
	return n.store.finishSnapshot(ctx, meta.Floors, meta.Ranges)
}

// snapshotOps applies every op peer retains.
func (n *Node) snapshotOps(ctx context.Context, peer PeerID) error {
	var cursor []byte
	for {
		page, err := n.tr.GetSnapshot(ctx, peer, SnapshotOps, cursor)
		if err != nil {
			return err
		}
		if k, err := n.store.ingestAll(ctx, page.Ops); k >= 0 && err != nil {
			return fmt.Errorf("replication: snapshot op %s/%d: %w", page.Ops[k].Site, page.Ops[k].Seq, err)
		} else if err != nil {
			return fmt.Errorf("replication: snapshot ops: %w", err)
		}
		if cursor = page.Next; cursor == nil {
			return nil
		}
	}
}

// mergeCells merges a peer's clock cells, each only where it is the newer
// writer.
func (s *Store) mergeCells(ctx context.Context, cells []ClockCell) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	s.q = tx
	err = s.mergeCellsIn(ctx, cells)
	s.q = s.db
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// cellsPerUpsert is how many cells one statement of mergeCellsIn writes.
const cellsPerUpsert = 64

// mergeCellsIn merges cells into the clock, each only where it is a strictly
// newer writer than the cell there by (hlc, site), as mergeCell does one: an
// upsert per few hundred cells, whose rows apply in order, so a cell later in
// cells is merged against an earlier one as a run of mergeCell calls would.
func (s *Store) mergeCellsIn(ctx context.Context, cells []ClockCell) error {
	for len(cells) > 0 {
		chunk := cells[:min(len(cells), cellsPerUpsert)]
		cells = cells[len(chunk):]
		args := make([]any, 0, 6*len(chunk))
		for _, c := range chunk {
			s.clock.Merge(c.HLC)
			args = append(args, c.Tbl, c.PK, c.Col, int64(c.HLC), c.Site, c.Val)
		}
		if _, err := s.q.ExecContext(ctx, `INSERT INTO main._repl_clock(tbl,pk,col,hlc,site,val) VALUES `+
			strings.TrimSuffix(strings.Repeat(`(?,?,?,?,?,?),`, len(chunk)), ",")+
			` ON CONFLICT(tbl,pk,col) DO UPDATE SET hlc=excluded.hlc, site=excluded.site, val=excluded.val
			WHERE excluded.hlc > hlc OR (excluded.hlc = hlc AND excluded.site > site)`, args...); err != nil {
			return err
		}
	}
	return nil
}

// finishSnapshot raises this node's floors to the peer's -- every op at or
// below them is in the clock now -- takes the peer's range owners where it has
// none, and rebuilds every table from the merged clock.
func (s *Store) finishSnapshot(ctx context.Context, floors VersionVector, ranges map[string]uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	s.q = tx
	err = func() error {
		for site, lo := range ranges {
			if _, err := s.q.ExecContext(ctx, `INSERT INTO main._repl_ranges(lo, site) VALUES(?, ?) ON CONFLICT(lo) DO NOTHING`, int64(lo), site); err != nil {
				return err
			}
		}
		for site, f := range floors {
			if f <= s.floors[site] {
				continue
			}
			if _, err := s.q.ExecContext(ctx, `INSERT INTO main._repl_floors(site, seq) VALUES(?, ?)
				ON CONFLICT(site) DO UPDATE SET seq = excluded.seq`, site, int64(f)); err != nil {
				return err
			}
		}
		if err := s.syncCatalog(ctx); err != nil {
			return err
		}
		for _, o := range s.cat.Objs {
			if o.Type == "table" {
				if err := s.rebuildTable(ctx, o); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	s.q = s.db
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for site, f := range floors {
		if f > s.floors[site] {
			s.floors[site] = f
		}
		if f > s.vv[site] {
			s.vv[site] = f
		}
		if err := s.advanceVV(ctx, site, nil); err != nil {
			return err
		}
	}
	return nil
}

// isPruned reports whether err is a peer's ErrPruned.
func isPruned(err error) bool { return errors.Is(err, ErrPruned) }
