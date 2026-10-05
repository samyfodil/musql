package replication

import (
	"context"
	"database/sql"
	"errors"
)

// Compaction: prune the op log per site to the frontier (least seq every
// member has acked). Acks are state (version vector + clock), not ops.
// Schema ops are never pruned; tombstones are never collected.

// Ack is a node's acknowledgement: its version vector and clock stamp.
type Ack struct {
	HLC HLC
	VV  VersionVector
}

// Acks is the latest ack this node knows of each site.
type Acks map[string]Ack

// Acks is every ack this node knows, its own current one included.
func (s *Store) Acks() Acks {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !vvEqual(s.selfAck.VV, s.vv) {
		s.selfAck = Ack{HLC: s.clock.Now(), VV: s.vv.Clone()}
	}
	out := Acks{s.site: s.selfAck}
	for site, a := range s.acks {
		out[site] = a
	}
	return out
}

// MergeAcks keeps, per site, the newer of the ack known here and the one in.
func (s *Store) MergeAcks(in Acks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for site, a := range in {
		if site == s.site {
			continue // this node's own is its version vector
		}
		if cur, ok := s.acks[site]; !ok || cur.HLC < a.HLC {
			s.acks[site] = Ack{HLC: a.HLC, VV: a.VV.Clone()}
		}
	}
}

func vvEqual(a, b VersionVector) bool {
	if len(a) != len(b) {
		return false
	}
	for site, n := range a {
		if b[site] != n {
			return false
		}
	}
	return true
}

// prune deletes, per site, the row and ack ops at or below the frontier, and
// raises the site's floor to it.
func (s *Store) prune(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	acks := map[string]VersionVector{s.site: s.vv.Clone()}
	for site, a := range s.acks {
		acks[site] = a.VV
	}

	members, err := s.members(ctx)
	if err != nil {
		return err
	}
	frontier := VersionVector{}
	for i, m := range members {
		vv, ok := acks[m]
		if !ok {
			return nil // a member that never acked: nothing is known to be everywhere
		}
		for site := range s.vv {
			if i == 0 || vv[site] < frontier[site] {
				frontier[site] = vv[site]
			}
		}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	raised := VersionVector{}
	for site, f := range frontier {
		if f <= s.floors[site] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM main._repl_oplog WHERE site=? AND seq<=? AND op<>?`, site, int64(f), int64(OpSchema)); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO main._repl_floors(site, seq) VALUES(?, ?)
			ON CONFLICT(site) DO UPDATE SET seq = excluded.seq`, site, int64(f)); err != nil {
			tx.Rollback()
			return err
		}
		raised[site] = f
	}
	if len(raised) == 0 {
		tx.Rollback()
		return nil
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for site, f := range raised {
		s.floors[site] = f
	}
	return nil
}

// members is the sites whose acks the frontier waits for: every site this node
// admitted, less the retired, and this node.
func (s *Store) members(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT site FROM main._repl_ranges ORDER BY site`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var site string
		if err := rows.Scan(&site); err != nil {
			return nil, err
		}
		if site != s.site && !s.cat.Gone[site] {
			out = append(out, site)
		}
	}
	// Itself always, retired or not: it may not prune past what it holds.
	return append(out, s.site), rows.Err()
}

// Retire stops db's node waiting for site's acks before it prunes: site's node
// is gone for good, and would otherwise hold pruning back forever. It
// replicates, so every node stops waiting. db is the *sql.DB Open returned.
//
// Nothing else changes: ops from site that still arrive are applied, and if
// the node does come back, it merges as before -- taking a snapshot if what it
// lacks has been pruned meanwhile.
func Retire(ctx context.Context, db *sql.DB, site string) error {
	v, ok := openDBs.Load(db)
	if !ok {
		return errors.New("replication: Retire needs the *sql.DB Open returned")
	}
	s := v.(*Syncer)
	if site == s.site {
		return errors.New("replication: a node cannot retire itself")
	}
	so := schemaOp{Act: actRetire, Name: site}
	if s.quorum == nil {
		return s.store.localSchemaOp(ctx, so)
	}
	// Every op of a Quorum database goes through the caller's log, which only
	// the writer proposes to.
	if !s.writer() {
		return ErrNotWriter
	}
	s.store.seqMu.Lock()
	defer s.store.seqMu.Unlock()
	var seq sql.NullInt64
	if err := s.store.db.QueryRowContext(ctx, maxSeqQuery, s.site, s.site).Scan(&seq); err != nil {
		return err
	}
	return s.propose(ctx, []Op{{Site: s.site, Seq: uint64(seq.Int64) + 1, HLC: s.clock.Now(), Kind: OpSchema, Cells: so.cells()}})
}
