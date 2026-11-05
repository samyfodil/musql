package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/hrana"
	"github.com/samyfodil/musql/replication"

	"github.com/samyfodil/musql/cloud/cluster"
	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

// stateTopic and dbTopic name the replicated files on the cluster's router.
// The state's topic changed when it moved from CRDT to Leader mode, so a node
// of either kind never takes the other's ops.
const stateTopic = "musql-cloud/state/leader"

func dbTopic(id string) string { return "musql-cloud/db/" + id }

// clusterSide is a cluster node's libp2p side: the elections it runs, and
// the databases open here, which stay open while placed: their sync runs in
// the background.
type clusterSide struct {
	node *cluster.Node
	// client reaches the other nodes over libp2p.
	client *http.Client
	// state elects the state's master, among every node.
	state *cluster.Election
	// starting lets this node write the state on its own while it starts:
	// its schema statements, its signing key, the built-in clients. Each is
	// the same on every node or this node's alone, so the writes merge.
	starting atomic.Bool
	mu       sync.Mutex
	dbs      map[string]*replDB
	// groups elects each group's master, among the nodes it is placed on.
	groups map[string]*cluster.Election
	// importFrom is a state this node held before, its records not yet
	// imported into the cluster's.
	importFrom string
	// stateDB is the state's replicated handle, once open.
	stateDB atomic.Pointer[sql.DB]
}

type replDB struct {
	db   *sql.DB
	path string
	hist string
	// syncing is set from placement until the first sync with the peers, while
	// the file holds only part of the database and is not served; kept on disk
	// as path+syncingSuffix so a restart resumes it.
	syncing bool
}

const syncingSuffix = ".syncing"

// settle is how long a newly elected master waits before it writes: two
// anti-entropy rounds, so the previous master's last commits reach it first.
func (s *Server) settle() time.Duration { return 2 * s.cfg.syncInterval }

// members are the peer IDs of the nodes at locations, this node's included.
func (s *Server) members(locations []string) []string {
	var out []string
	if slices.Contains(locations, s.cfg.Node.Location) {
		out = append(out, s.cl.node.ID())
	}
	for _, p := range s.cfg.Peers {
		if slices.Contains(locations, p.Location) {
			out = append(out, p.PeerID)
		}
	}
	return out
}

// allLocations is every node's location.
func (s *Server) allLocations() []string {
	out := []string{s.cfg.Node.Location}
	for _, p := range s.cfg.Peers {
		out = append(out, p.Location)
	}
	return out
}

// openState opens the state: with the plain driver standalone; in a cluster
// replicated in Leader mode, its master elected among every node, a follower
// reaching the master's over libp2p for every call.
func (s *Server) openState(ctx context.Context) (*state.Store, error) {
	path := filepath.Join(s.cfg.DataDir, "state.musq")
	if !s.cfg.Clustered() {
		return state.Open(path)
	}
	// A state no replication has opened is a standalone node's, joining a
	// cluster: its databases must all be ones a cluster can replicate before
	// any of them is turned into one. Its records then join the cluster's
	// state by import, as a CRDT state's do (openLeaderState).
	if _, err := os.Stat(path); err == nil {
		replicated, err := replication.Replicated(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		if !replicated {
			if err := s.preflightJoin(ctx, path); err != nil {
				return nil, err
			}
			engine.RemoveDatabaseFiles(path + importSuffix)
			if err := moveDB(path, path+importSuffix); err != nil {
				return nil, err
			}
		}
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
	s.cl = &clusterSide{node: n, client: n.HTTPClient(), dbs: map[string]*replDB{}, groups: map[string]*cluster.Election{}}
	fail := func(err error) (*state.Store, error) {
		if s.cl.state != nil {
			s.cl.state.Close()
		}
		n.Close()
		s.cl = nil
		return nil, err
	}
	// Init writes the admin on this node alone, before it runs: no election.
	here := func() bool { return true }
	if !s.initOnly {
		e, err := n.Elect(filepath.Join(s.cfg.DataDir, "raft", "state"), "state", s.members(s.allLocations()), s.settle(), s.cfg.election)
		if err != nil {
			return fail(fmt.Errorf("state election: %w", err))
		}
		s.cl.state = e
		// A master writes, and answers the others, from its copy: one that
		// has not synced with a peer yet may miss what they hold.
		here = func() bool {
			db := s.cl.stateDB.Load()
			return e.Writer() && db != nil && replication.Synced(db)
		}
	}
	elected := here
	here = func() bool { return s.cl.starting.Load() || elected() }
	s.cl.starting.Store(true)
	db, pending, err := openLeaderState(ctx, path, here, s.cfg.syncInterval, n.Sync(stateTopic))
	if err != nil {
		return fail(fmt.Errorf("state: %w", err))
	}
	// One connection, as standalone: writes are serialized in-process.
	db.SetMaxOpenConns(1)
	s.cl.stateDB.Store(db)
	m := &state.Master{Here: here, Client: s.cl.client, URL: func() (string, error) {
		id := s.cl.state.Leader()
		if id == "" || id == n.ID() {
			return "", state.ErrNoMaster
		}
		return cluster.PeerURL(id) + "/state/v3-protobuf/pipeline", nil
	}}
	st, err := state.OpenReplicated(db, s.cfg.Node.Name, m)
	if err != nil {
		db.Close()
		return fail(err)
	}
	s.cl.importFrom = pending
	if !s.initOnly {
		// The other nodes reach this one's state, and the requests they
		// forward, here; only the configured peers can open the protocol.
		mux := http.NewServeMux()
		stateHrana := http.StripPrefix("/state", hrana.NewServer(db))
		mux.Handle("/state/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Elected but not yet one to answer from (settling, or its copy
			// not synced): the asker uses its own copy meanwhile.
			if !elected() {
				http.Error(w, state.ErrNoMaster.Error(), http.StatusServiceUnavailable)
				return
			}
			stateHrana.ServeHTTP(w, r)
		}))
		mux.Handle("/fwd/", http.StripPrefix("/fwd", http.HandlerFunc(s.serveForwarded)))
		mux.HandleFunc("/usage/flush", s.serveFlushUsage)
		if err := n.ServeHTTP(mux); err != nil {
			st.Close()
			return fail(err)
		}
	}
	return st, nil
}

// preflightJoin checks, before a standalone node's data is replicated, that
// every database it holds can be: it names each one a cluster would refuse
// (a trigger, a virtual table, a WITHOUT ROWID table), so the node stops
// rather than serve some of its databases and not others.
func (s *Server) preflightJoin(ctx context.Context, path string) error {
	st, err := state.Open(path)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	defer st.Close()
	dbs, err := st.AllDatabases(ctx)
	if err != nil {
		return err
	}
	var refused []string
	for _, d := range dbs {
		err := replication.Check(ctx, s.dbPath(d))
		if err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}
		name := d.Name
		if o, oerr := st.OrgByID(ctx, d.OrgID); oerr == nil {
			name = o.Slug + "/" + d.Name
		}
		refused = append(refused, fmt.Sprintf("  %s: %v", name, err))
	}
	if len(refused) > 0 {
		return fmt.Errorf("this node's databases cannot all join a cluster; start it without peers and drop what is named, or export the database and delete it:\n%s", strings.Join(refused, "\n"))
	}
	return nil
}

// openLeaderState opens the state in Leader mode. A state this node kept in
// CRDT mode, before the state moved to Leader mode, is not converted in
// place: genesis would record its tables by their final text, and the
// cluster's other nodes by the statements that made them, two schemas whose
// rows would not merge. It is set aside (state.musq.crdt), a plain copy of it
// left for Import (state.musq.import, its name returned), and the node starts
// from a state built as every node's is.
func openLeaderState(ctx context.Context, path string, isWriter func() bool, every time.Duration, sync replication.Option) (*sql.DB, string, error) {
	open := func() (*sql.DB, error) {
		return replication.Open(ctx, path, replication.Leader(isWriter), replication.WithSyncInterval(every), sync)
	}
	pending := path + importSuffix
	db, err := open()
	if !errors.Is(err, replication.ErrMode) {
		if _, serr := os.Stat(pending); serr != nil {
			pending = ""
		}
		return db, pending, err
	}
	engine.RemoveDatabaseFiles(pending)
	if err := replication.PlainCopy(ctx, path, pending); err != nil {
		return nil, "", fmt.Errorf("moving the state to Leader mode: %w", err)
	}
	old := path + ".crdt"
	engine.RemoveDatabaseFiles(old)
	if err := moveDB(path, old); err != nil {
		return nil, "", err
	}
	db, err = open()
	return db, pending, err
}

// importSuffix names a state this node held before, whose records are
// waiting to be imported into the cluster's.
const importSuffix = ".import"

// importState writes the records of the state this node held before into the
// cluster's, through its master, and marks the file done (".imported"). It
// waits for a master: until one is reached it fails, and the next pass tries
// again.
func (s *Server) importState(ctx context.Context) error {
	src, err := sql.Open("sqlite", s.cl.importFrom)
	if err != nil {
		return err
	}
	n, err := s.store.Import(ctx, src)
	src.Close()
	if err != nil {
		return err
	}
	// A standalone node's groups may name the location it had alone: placed
	// nowhere in the cluster, placement would never open their databases.
	g, err := s.store.AdoptGroups(ctx, s.allLocations(), s.cfg.Node.Location)
	if err != nil {
		return err
	}
	if g > 0 {
		s.log.Printf("placed %d groups placed at no node's location at %s", g, s.cfg.Node.Location)
	}
	if err := moveDB(s.cl.importFrom, strings.TrimSuffix(s.cl.importFrom, importSuffix)+".imported"); err != nil {
		return err
	}
	s.log.Printf("imported %d rows of this node's earlier state into the cluster's", n)
	s.cl.importFrom = ""
	return nil
}

// moveDB renames a database's file and, when it has one, its delta.
func moveDB(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if err := os.Rename(from+".delta", to+".delta"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// runCluster keeps a cluster node in step with the state: held-out names
// renamed, other nodes' signing keys loaded, databases placed and their
// groups' elections run.
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
	if s.cl.importFrom != "" {
		if err := s.importState(ctx); err != nil {
			s.log.Printf("import of this node's earlier state: %v", err)
		}
	}
	// Two masters can overlap for a moment around an election, and both
	// claim one name; the master renames the row held out.
	if s.cl.state.Writer() {
		if err := s.cl.state.SetMembers(s.members(s.allLocations())); err != nil {
			s.log.Printf("state election: members: %v", err)
		}
		renamed, err := s.store.ResolveHeld(ctx)
		if err != nil {
			s.log.Printf("state conflicts: %v", err)
		}
		for _, r := range renamed {
			s.log.Printf("state conflict: %s %s claimed %q on two masters; it is now %q", r.Table, r.ID, r.From, r.To)
		}
	}
	if err := s.provider.ReloadKeys(ctx); err != nil {
		s.log.Printf("signing keys: %v", err)
	}
	if err := s.placeDatabases(ctx); err != nil {
		s.log.Printf("placement: %v", err)
	}
	s.replSynced()
}

// placedHere reports whether d's group lists this node's location.
func (s *Server) placedHere(ctx context.Context, d *state.Database) bool {
	g, err := s.store.GroupByID(ctx, d.GroupID)
	return err == nil && slices.Contains(g.Locations, s.cfg.Node.Location)
}

// placeDatabases opens every database placed here and not open, and closes
// and removes every open one no longer placed here. A group with a regular
// database placed here has an election among the nodes it is placed on, run
// while it has one.
func (s *Server) placeDatabases(ctx context.Context) error {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return err
	}
	groups := map[string]*state.Group{}
	group := func(id string) *state.Group {
		g, ok := groups[id]
		if !ok {
			if g, err = s.store.GroupByID(ctx, id); err != nil {
				s.log.Printf("place: group %s: %v", id, err)
			}
			groups[id] = g
		}
		return g
	}
	want, elect := map[string]bool{}, map[string]*state.Group{}
	for _, d := range dbs {
		g := group(d.GroupID)
		if g == nil || !slices.Contains(g.Locations, s.cfg.Node.Location) {
			continue
		}
		want[d.ID] = true
		if d.Type != state.TypeCRDT {
			elect[g.ID] = g
			if err := s.groupElection(g); err != nil {
				s.log.Printf("place %s: group election: %v", d.ID, err)
				continue
			}
		}
		if err := s.openReplicated(ctx, d); err != nil {
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
		case s.placedHere(ctx, d):
			continue
		}
		s.unplace(id)
	}
	s.cl.mu.Lock()
	var ended []string
	for id, e := range s.cl.groups {
		if g, ok := elect[id]; ok {
			if err := e.SetMembers(s.members(g.Locations)); err != nil {
				s.log.Printf("group %s: members: %v", id, err)
			}
			continue
		}
		ended = append(ended, id)
	}
	s.cl.mu.Unlock()
	for _, id := range ended {
		s.endGroupElection(id)
	}
	return nil
}

// groupElection starts g's election here, unless it runs already.
func (s *Server) groupElection(g *state.Group) error {
	s.cl.mu.Lock()
	defer s.cl.mu.Unlock()
	if _, ok := s.cl.groups[g.ID]; ok {
		return nil
	}
	e, err := s.cl.node.Elect(s.groupRaftDir(g.ID), "group/"+g.ID, s.members(g.Locations), s.settle(), s.cfg.election)
	if err != nil {
		return err
	}
	s.cl.groups[g.ID] = e
	return nil
}

func (s *Server) groupRaftDir(id string) string {
	return filepath.Join(s.cfg.DataDir, "raft", "group-"+id)
}

// endGroupElection leaves a group no regular database here is in any more,
// and forgets its raft state: placed here again, it joins as a new member.
func (s *Server) endGroupElection(id string) {
	s.cl.mu.Lock()
	e := s.cl.groups[id]
	delete(s.cl.groups, id)
	s.cl.mu.Unlock()
	if e == nil {
		return
	}
	if err := e.Close(); err != nil {
		s.log.Printf("group %s: %v", id, err)
	}
	if err := os.RemoveAll(s.groupRaftDir(id)); err != nil {
		s.log.Printf("group %s: %v", id, err)
	}
}

// groupMaster is the election of d's group when it runs here, nil when d is
// a crdt database (every node writes) or its group has none here.
func (s *Server) groupMaster(d *state.Database) *cluster.Election {
	if s.cl == nil || d.Type == state.TypeCRDT {
		return nil
	}
	s.cl.mu.Lock()
	defer s.cl.mu.Unlock()
	return s.cl.groups[d.GroupID]
}

// mode is how d replicates: a crdt database merges every node's writes; a
// regular one is written by its group's master alone.
func (s *Server) mode(d *state.Database) replication.Option {
	if d.Type == state.TypeCRDT {
		return replication.CRDT()
	}
	return replication.Leader(func() bool {
		e := s.groupMaster(d)
		return e != nil && e.Writer()
	})
}

// openReplicated opens d's file through replication, creating it empty when
// missing: its first sync installs a peer's image of it (replication's
// image.go), and pulls what came after.
func (s *Server) openReplicated(ctx context.Context, d *state.Database) error {
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
	db, err := replication.Open(ctx, path+"?"+dsnMode+"=rw", s.mode(d), replication.WithSyncInterval(s.cfg.syncInterval), s.cl.node.Sync(dbTopic(d.ID)))
	if err != nil {
		return err
	}
	s.cl.dbs[d.ID] = &replDB{db: db, path: path, hist: s.histDir(d), syncing: syncing}
	return nil
}

// replSynced serves the databases whose first sync has completed since the
// last pass.
func (s *Server) replSynced() {
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

// replSyncing reports whether d is open here and still in its first sync.
func (s *Server) replSyncing(id string) bool {
	if s.cl == nil {
		return false
	}
	s.cl.mu.Lock()
	defer s.cl.mu.Unlock()
	c, ok := s.cl.dbs[id]
	return ok && c.syncing
}

// replHandle is d's replicated handle, when d is open here.
func (s *Server) replHandle(id string) *sql.DB {
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

// closeReplicated closes d's replicated handle, if open, and reports where its
// files were.
func (s *Server) closeReplicated(id string) *replDB {
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
	c := s.closeReplicated(id)
	if c == nil {
		return
	}
	s.closeHistory(id)
	removeDBFiles(s.log, c.path)
	if err := os.RemoveAll(c.hist); err != nil {
		s.log.Printf("remove %s: %v", c.hist, err)
	}
}

// leaveElections leaves the groups' elections and the state's.
func (s *Server) leaveElections() {
	s.cl.mu.Lock()
	groups := s.cl.groups
	s.cl.groups = map[string]*cluster.Election{}
	s.cl.mu.Unlock()
	for id, e := range groups {
		if err := e.Close(); err != nil {
			s.log.Printf("group %s: %v", id, err)
		}
	}
	if err := s.cl.state.Close(); err != nil {
		s.log.Printf("state election: %v", err)
	}
}

// closeCluster closes the replicated databases.
func (s *Server) closeCluster() {
	s.cl.mu.Lock()
	ids := make([]string, 0, len(s.cl.dbs))
	for id := range s.cl.dbs {
		ids = append(ids, id)
	}
	s.cl.mu.Unlock()
	for _, id := range ids {
		s.closeReplicated(id)
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
	// A raft database is a regular one: in a cluster its group's master,
	// elected by raft, writes it.
	if typ == state.TypeRaft {
		typ = state.TypePlain
	}
	if typ == state.TypeCRDT && !s.cfg.Clustered() {
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("crdt databases need a cluster: this node has no peers"))
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

// localCopy declines, for a cluster's database not open here or still in
// its first sync, what reads this node's copy or history.
func (s *Server) localCopy(ctx context.Context, d *state.Database) error {
	switch {
	case s.cl == nil:
		return nil
	case s.replHandle(d.ID) == nil:
		return connect.NewError(connect.CodeFailedPrecondition, s.notHere(ctx, d))
	case s.replSyncing(d.ID):
		return connect.NewError(connect.CodeUnavailable, errSyncing)
	}
	return nil
}

// checkReplicable opens the file at path through replication in d's mode,
// without peers, so a database holding what replication refuses (a trigger,
// a virtual table, a WITHOUT ROWID table) is declined before it is recorded.
// Its rows become this node's first ops.
func (s *Server) checkReplicable(ctx context.Context, d *state.Database, path string) error {
	db, err := replication.Open(ctx, path, s.mode(d))
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("the database cannot be replicated in a cluster: %w", err))
	}
	return db.Close()
}
