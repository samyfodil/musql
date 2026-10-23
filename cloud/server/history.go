package server

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/samyfodil/musql/cloud/state"
	"github.com/samyfodil/musql/cloud/store"
)

// histories holds the open database histories (cloud/store), by database ID.
// A history stays open while the server runs: it must be open before any
// full-access connection is, and closing it with the pool's idle connections
// would only make the next open repeat the repair check.
type histories struct {
	mu sync.Mutex
	m  map[string]*store.History
}

// histDir is where a database's history is kept.
func (s *Server) histDir(d *state.Database) string {
	return filepath.Join(s.cfg.DataDir, "history", d.OrgID, d.ID)
}

// history is d's open history, opened (and its first base taken) on first use.
func (s *Server) history(d *state.Database) (*store.History, error) {
	s.hist.mu.Lock()
	defer s.hist.mu.Unlock()
	if h, ok := s.hist.m[d.ID]; ok {
		return h, nil
	}
	h, err := store.Open(s.dbPath(d), s.histDir(d))
	if err != nil {
		return nil, err
	}
	h.Logf = s.log.Printf
	if s.hist.m == nil {
		s.hist.m = map[string]*store.History{}
	}
	s.hist.m[d.ID] = h
	return h, nil
}

func (s *Server) closeHistory(id string) {
	s.hist.mu.Lock()
	h, ok := s.hist.m[id]
	delete(s.hist.m, id)
	s.hist.mu.Unlock()
	if ok {
		h.Close()
	}
}

func (s *Server) closeHistories() {
	s.hist.mu.Lock()
	all := s.hist.m
	s.hist.m = nil
	s.hist.mu.Unlock()
	for _, h := range all {
		h.Close()
	}
}

// snapshotPass takes a base of every database changed since its last one and
// drops history older than snapshots.keep, as of now.
//
// A replica takes no bases of its own (its log is the primary's records); it
// only prunes.
func (s *Server) snapshotPass(ctx context.Context, now time.Time) {
	pass := func(id string, h *store.History) {
		if s.replica == nil {
			if err := h.SnapshotIfChanged(); err != nil {
				s.log.Printf("snapshot %s: %v", id, err)
			}
		}
		if s.cfg.snapshotKeep > 0 {
			if err := h.Prune(now.Add(-s.cfg.snapshotKeep)); err != nil {
				s.log.Printf("prune %s: %v", id, err)
			}
		}
	}
	pass("state", s.stateHist)
	if s.replica != nil {
		s.replica.mu.Lock()
		fs := make(map[string]*follower, len(s.replica.followers))
		maps.Copy(fs, s.replica.followers)
		s.replica.mu.Unlock()
		for id, f := range fs {
			if id != "" {
				pass(id, f.h)
			}
		}
		return
	}
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		s.log.Printf("snapshots: %v", err)
		return
	}
	for _, d := range dbs {
		h, err := s.history(d)
		if errors.Is(err, os.ErrNotExist) {
			continue // being created or deleted
		}
		if err != nil {
			s.log.Printf("snapshots %s: %v", d.ID, err)
			continue
		}
		pass(d.ID, h)
	}
}

func (s *Server) runSnapshots(stop <-chan struct{}) {
	t := time.NewTicker(s.cfg.snapshotEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.snapshotPass(context.Background(), time.Now())
		}
	}
}

// copyAt writes src's state at `at` (now when at is zero) to dst, a new file;
// schemaOnly keeps its objects and none of its rows.
func (s *Server) copyAt(ctx context.Context, src *state.Database, at time.Time, schemaOnly bool, dst string) error {
	h, err := s.history(src)
	if err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now()
	}
	if !schemaOnly {
		return h.Restore(at, dst)
	}
	tmp, err := os.MkdirTemp(s.cfg.DataDir, "schema-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	full := filepath.Join(tmp, "db.musq")
	if err := h.Restore(at, full); err != nil {
		return err
	}
	return store.SchemaOnly(ctx, full, dst)
}

// restoreInPlace makes d its state at `at`. Its connections are closed and
// requests refused meanwhile. The restored file is written beside the current
// one and logged as a base of the same history; pointing d's record at it is
// the commit point, so a crash before that leaves d as it was (its history's
// repair then rolls it forward to the restored state, which the log holds).
func (s *Server) restoreInPlace(ctx context.Context, d *state.Database, at time.Time) error {
	h, err := s.history(d)
	if err != nil {
		return err
	}
	unblock := s.pool.block(d.ID)
	defer unblock()
	old := s.dbPath(d)
	nd := *d
	nd.File = d.ID + "-" + state.NewID()[:8] + ".musq"
	dst := s.dbPath(&nd)
	if err := h.Restore(at, dst); err != nil {
		return err
	}
	if err := h.Retarget(dst); err != nil {
		removeDBFiles(s.log, dst)
		return err
	}
	if err := s.store.SetDatabaseFile(ctx, d.ID, nd.File); err != nil {
		if rerr := h.Retarget(old); rerr != nil {
			s.log.Printf("restore %s: %v; and back: %v", d.ID, err, rerr)
		}
		removeDBFiles(s.log, dst)
		return err
	}
	removeDBFiles(s.log, old)
	*d = nd
	return nil
}
