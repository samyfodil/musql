package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/replication"

	"github.com/samyfodil/musql/cloud/cluster"
	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

// stateTopic and dbTopic name the replicated files on the cluster's router.
const stateTopic = "musql-cloud/state"

func dbTopic(id string) string { return "musql-cloud/db/" + id }

// clusterSide is a cluster node's libp2p side and the crdt databases open
// here, which stay open while placed: their sync runs in the background.
type clusterSide struct {
	node *cluster.Node
	mu   sync.Mutex
	dbs  map[string]*crdtDB
}

type crdtDB struct {
	db   *sql.DB
	path string
	hist string
	// syncing is set from placement until the first sync with the peers has
	// completed: until then the file holds only part of the database, and it
	// is not served. It is kept on disk as path+syncingSuffix, so a node that
	// stops before then resumes it.
	syncing bool
}

const syncingSuffix = ".syncing"

// openState opens the state: with the plain driver standalone, replicated in
// CRDT mode over libp2p in a cluster.
func (s *Server) openState(ctx context.Context) (*state.Store, error) {
	path := filepath.Join(s.cfg.DataDir, "state.musq")
	if !s.cfg.Clustered() {
		return state.Open(path)
	}
	key, err := cluster.LoadKey(filepath.Join(s.cfg.DataDir, "node.key"))
	if err != nil {
		return nil, fmt.Errorf("node key: %w", err)
	}
	var peers []cluster.Peer
	for _, p := range s.cfg.Peers {
		peers = append(peers, cluster.Peer{ID: p.PeerID, Addrs: p.Addrs})
	}
	n, err := cluster.Start(key, s.cfg.Node.P2PListen, peers, s.log.Printf)
	if err != nil {
		return nil, err
	}
	db, err := replication.Open(ctx, path, replication.CRDT(), replication.WithSyncInterval(s.cfg.syncInterval), n.Sync(stateTopic))
	if err != nil {
		n.Close()
		return nil, fmt.Errorf("state: %w", err)
	}
	// One connection, as standalone: writes are serialized in-process.
	db.SetMaxOpenConns(1)
	st, err := state.OpenReplicated(db, s.cfg.Node.Name)
	if err != nil {
		db.Close()
		n.Close()
		return nil, err
	}
	s.cl = &clusterSide{node: n, dbs: map[string]*crdtDB{}}
	return st, nil
}

// runCluster keeps a cluster node in step with the state: held-out names
// renamed, other nodes' signing keys loaded, crdt databases placed.
func (s *Server) runCluster() {
	s.clusterPass(context.Background())
	s.every(s.cfg.syncInterval, "cluster", func(ctx context.Context) error {
		s.clusterPass(ctx)
		return nil
	})
}

func (s *Server) clusterPass(ctx context.Context) {
	if s.noAdmin.Load() {
		if s.requireAdmin(ctx) == nil {
			s.noAdmin.Store(false)
			s.log.Printf("the admin user arrived from a peer")
		} else if time.Since(s.noAdminLogged) >= time.Minute {
			s.log.Print(errNoAdminYet)
			s.noAdminLogged = time.Now()
		}
	}
	renamed, err := s.store.ResolveHeld(ctx)
	if err != nil {
		s.log.Printf("state conflicts: %v", err)
	}
	for _, r := range renamed {
		s.log.Printf("state conflict: %s %s claimed %q on two nodes; it is now %q", r.Table, r.ID, r.From, r.To)
	}
	if err := s.provider.ReloadKeys(ctx); err != nil {
		s.log.Printf("signing keys: %v", err)
	}
	if err := s.placeDatabases(ctx); err != nil {
		s.log.Printf("placement: %v", err)
	}
	s.crdtSynced()
}

// placedHere reports whether d's group lists this node's location.
func (s *Server) placedHere(ctx context.Context, d *state.Database) bool {
	g, err := s.store.GroupByID(ctx, d.GroupID)
	return err == nil && slices.Contains(g.Locations, s.cfg.Node.Location)
}

// placeDatabases opens every crdt database placed here and not open, and
// closes and removes every open one no longer placed here.
func (s *Server) placeDatabases(ctx context.Context) error {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, d := range dbs {
		if d.Type != state.TypeCRDT || !s.placedHere(ctx, d) {
			continue
		}
		want[d.ID] = true
		if err := s.openCRDT(ctx, d); err != nil {
			s.log.Printf("place %s: %v", d.ID, err)
		}
	}
	s.cl.mu.Lock()
	var gone []string
	for id := range s.cl.dbs {
		if !want[id] {
			gone = append(gone, id)
		}
	}
	s.cl.mu.Unlock()
	for _, id := range gone {
		// A database made here (an upload, a branch) is opened once its record
		// is written, which may be after the list above was read: only what the
		// state no longer places here goes.
		d, err := s.store.DatabaseByID(ctx, id)
		switch {
		case errors.Is(err, state.ErrNotFound):
		case err != nil:
			s.log.Printf("place %s: %v", id, err)
			continue
		case d.Type == state.TypeCRDT && s.placedHere(ctx, d):
			continue
		}
		s.unplace(id)
	}
	return nil
}

// openCRDT opens d's file through replication, creating it empty when
// missing: the peers fill it.
func (s *Server) openCRDT(ctx context.Context, d *state.Database) error {
	s.cl.mu.Lock()
	defer s.cl.mu.Unlock()
	if _, ok := s.cl.dbs[d.ID]; ok {
		return nil
	}
	path := s.dbPath(d)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path+syncingSuffix, nil, 0o600); err != nil {
			return err
		}
		if err := createEmpty(path); err != nil {
			return err
		}
	}
	_, err := os.Stat(path + syncingSuffix)
	syncing := err == nil
	if _, err := s.history(d); err != nil {
		return err
	}
	db, err := replication.Open(ctx, path+"?"+dsnMode+"=rw", replication.CRDT(), replication.WithSyncInterval(s.cfg.syncInterval), s.cl.node.Sync(dbTopic(d.ID)))
	if err != nil {
		return err
	}
	s.cl.dbs[d.ID] = &crdtDB{db: db, path: path, hist: s.histDir(d), syncing: syncing}
	return nil
}

// crdtSynced serves the databases whose first sync has completed since the
// last pass.
func (s *Server) crdtSynced() {
	s.cl.mu.Lock()
	defer s.cl.mu.Unlock()
	for id, c := range s.cl.dbs {
		if !c.syncing || !replication.Synced(c.db) {
			continue
		}
		if err := os.Remove(c.path + syncingSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Printf("synced %s: %v", id, err)
			continue
		}
		c.syncing = false
	}
}

// crdtSyncing reports whether d is open here and still in its first sync.
func (s *Server) crdtSyncing(id string) bool {
	if s.cl == nil {
		return false
	}
	s.cl.mu.Lock()
	defer s.cl.mu.Unlock()
	c, ok := s.cl.dbs[id]
	return ok && c.syncing
}

// crdtHandle is d's replicated handle, when d is open here.
func (s *Server) crdtHandle(id string) *sql.DB {
	if s.cl == nil {
		return nil
	}
	s.cl.mu.Lock()
	defer s.cl.mu.Unlock()
	if c, ok := s.cl.dbs[id]; ok {
		return c.db
	}
	return nil
}

// closeCRDT closes d's replicated handle, if open, and reports where its
// files were.
func (s *Server) closeCRDT(id string) *crdtDB {
	if s.cl == nil {
		return nil
	}
	s.cl.mu.Lock()
	c, ok := s.cl.dbs[id]
	delete(s.cl.dbs, id)
	s.cl.mu.Unlock()
	if !ok {
		return nil
	}
	s.pool.drop(id)
	if err := c.db.Close(); err != nil {
		s.log.Printf("close %s: %v", id, err)
	}
	return c
}

// unplace closes a database no longer placed here (or deleted) and removes
// its file and history.
func (s *Server) unplace(id string) {
	c := s.closeCRDT(id)
	if c == nil {
		return
	}
	s.closeHistory(id)
	removeDBFiles(s.log, c.path)
	if err := os.RemoveAll(c.hist); err != nil {
		s.log.Printf("remove %s: %v", c.hist, err)
	}
}

// closeCluster closes the crdt databases and the libp2p host, after the
// state.
func (s *Server) closeCluster() {
	s.cl.mu.Lock()
	ids := make([]string, 0, len(s.cl.dbs))
	for id := range s.cl.dbs {
		ids = append(ids, id)
	}
	s.cl.mu.Unlock()
	for _, id := range ids {
		s.closeCRDT(id)
	}
}

// PeerID is this node's libp2p ID, "" standalone.
func (s *Server) PeerID() string {
	if s.cl == nil {
		return ""
	}
	return s.cl.node.ID()
}

// P2PAddrs are the addresses this node's libp2p host listens on.
func (s *Server) P2PAddrs() []string {
	if s.cl == nil {
		return nil
	}
	return s.cl.node.Addrs()
}

// checkType resolves the type a new database or group gets and declines the
// ones this deployment cannot keep.
func (s *Server) checkType(t v1.DatabaseType, def string) (string, error) {
	typ := def
	switch t {
	case v1.DatabaseType_DATABASE_TYPE_PLAIN:
		typ = state.TypePlain
	case v1.DatabaseType_DATABASE_TYPE_CRDT:
		typ = state.TypeCRDT
	case v1.DatabaseType_DATABASE_TYPE_RAFT:
		typ = state.TypeRaft
	}
	switch {
	case typ == state.TypeRaft:
		return "", connect.NewError(connect.CodeUnimplemented, errors.New("raft databases are not yet supported"))
	case typ == state.TypeCRDT && !s.cfg.Clustered():
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("crdt databases need a cluster: this node has no peers"))
	case typ == state.TypePlain && s.cfg.Clustered():
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("plain databases need a standalone node: this one is in a cluster, use crdt"))
	}
	return typ, nil
}

func typeToProto(t string) v1.DatabaseType {
	switch t {
	case state.TypeCRDT:
		return v1.DatabaseType_DATABASE_TYPE_CRDT
	case state.TypeRaft:
		return v1.DatabaseType_DATABASE_TYPE_RAFT
	}
	return v1.DatabaseType_DATABASE_TYPE_PLAIN
}

// notHere is the error for a database whose files are not on this node.
func (s *Server) notHere(ctx context.Context, d *state.Database) error {
	locs := "none"
	if g, err := s.store.GroupByID(ctx, d.GroupID); err == nil && len(g.Locations) > 0 {
		locs = strings.Join(g.Locations, ", ")
	}
	return fmt.Errorf("this database is not placed at %s (this node's location); its locations: %s", s.cfg.Node.Location, locs)
}

// localCopy declines, for a crdt database not open here or still in its
// first sync, what reads this node's copy or history.
func (s *Server) localCopy(ctx context.Context, d *state.Database) error {
	switch {
	case d.Type != state.TypeCRDT:
		return nil
	case s.crdtHandle(d.ID) == nil:
		return connect.NewError(connect.CodeFailedPrecondition, s.notHere(ctx, d))
	case s.crdtSyncing(d.ID):
		return connect.NewError(connect.CodeUnavailable, errSyncing)
	}
	return nil
}

// checkCRDT opens the file at path through replication, without peers, so a
// database holding what replication refuses (a trigger, a virtual table, a
// WITHOUT ROWID table) is declined before it is recorded. Its rows become
// this node's first ops.
func checkCRDT(ctx context.Context, path string) error {
	db, err := replication.Open(ctx, path, replication.CRDT())
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("the database cannot be a crdt database: %w", err))
	}
	return db.Close()
}
