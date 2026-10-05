package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// A page holds at most 512 ops/cells, stops at 16MB to stay within message limits.
const (
	snapshotPageLen   = 512
	snapshotPageBytes = 16 << 20
)

// SnapshotPage serves one page of part after cursor (nil: the first).
func (s *Store) SnapshotPage(ctx context.Context, part int, cursor []byte) (SnapshotPage, error) {
	var page SnapshotPage
	switch part {
	case SnapshotOps:
		var after struct {
			Site string
			Seq  uint64
		}
		if cursor != nil {
			if err := json.Unmarshal(cursor, &after); err != nil {
				return page, err
			}
		}
		rows, err := s.db.QueryContext(ctx, `SELECT site, seq, hlc, tbl, pk, op, cells FROM main._repl_oplog
			WHERE site > ? OR (site = ? AND seq > ?) ORDER BY site, seq LIMIT ?`,
			after.Site, after.Site, int64(after.Seq), snapshotPageLen)
		if err != nil {
			return page, err
		}
		defer rows.Close()
		size, full := 0, false
		for rows.Next() {
			if size >= snapshotPageBytes {
				full = true
				break
			}
			var op Op
			var hlc, kind int64
			var cells []byte
			if err := rows.Scan(&op.Site, &op.Seq, &hlc, &op.Tbl, &op.PK, &kind, &cells); err != nil {
				return page, err
			}
			op.HLC, op.Kind = HLC(hlc), OpKind(kind)
			if op.Cells, err = decodeCells(cells); err != nil {
				return page, err
			}
			size += len(op.Site) + len(op.Tbl) + len(op.PK) + len(cells) + 32
			page.Ops = append(page.Ops, op)
		}
		if err := rows.Err(); err != nil {
			return page, err
		}
		if full || len(page.Ops) == snapshotPageLen {
			last := page.Ops[len(page.Ops)-1]
			after.Site, after.Seq = last.Site, last.Seq
			page.Next, _ = json.Marshal(after) // two plain fields cannot fail
		}
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
		if err := rows.Err(); err != nil {
			return page, err
		}
	case SnapshotClock:
		var after struct {
			Tbl string
			PK  []byte
			Col string
		}
		if cursor != nil {
			if err := json.Unmarshal(cursor, &after); err != nil {
				return page, err
			}
		}
		rows, err := s.db.QueryContext(ctx, `SELECT tbl, pk, col, hlc, site, val FROM main._repl_clock
			WHERE tbl > ? OR (tbl = ? AND (pk > ? OR (pk = ? AND col > ?))) ORDER BY tbl, pk, col LIMIT ?`,
			after.Tbl, after.Tbl, after.PK, after.PK, after.Col, snapshotPageLen)
		if err != nil {
			return page, err
		}
		defer rows.Close()
		size, full := 0, false
		for rows.Next() {
			if size >= snapshotPageBytes {
				full = true
				break
			}
			var c ClockCell
			var hlc int64
			if err := rows.Scan(&c.Tbl, &c.PK, &c.Col, &hlc, &c.Site, &c.Val); err != nil {
				return page, err
			}
			c.HLC = HLC(hlc)
			size += len(c.Tbl) + len(c.PK) + len(c.Col) + len(c.Site) + len(c.Val) + 16
			page.Cells = append(page.Cells, c)
		}
		if err := rows.Err(); err != nil {
			return page, err
		}
		if full || len(page.Cells) == snapshotPageLen {
			last := page.Cells[len(page.Cells)-1]
			after.Tbl, after.PK, after.Col = last.Tbl, last.PK, last.Col
			page.Next, _ = json.Marshal(after)
		}
	default:
		return page, fmt.Errorf("replication: unknown snapshot part %d", part)
	}
	return page, nil
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
		for _, op := range page.Ops {
			if err := n.store.Ingest(ctx, op, false); err != nil {
				return fmt.Errorf("replication: snapshot op %s/%d: %w", op.Site, op.Seq, err)
			}
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
	for _, c := range cells {
		s.clock.Merge(c.HLC)
		if err = s.mergeCell(ctx, c.Tbl, c.PK, c.Col, c.HLC, c.Site, c.Val); err != nil {
			break
		}
	}
	s.q = s.db
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
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
		if err := s.advanceVV(ctx, site); err != nil {
			return err
		}
	}
	return nil
}

// isPruned reports whether err is a peer's ErrPruned.
func isPruned(err error) bool { return errors.Is(err, ErrPruned) }
