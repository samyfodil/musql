package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/hrana"

	"github.com/samyfodil/musql/cloud/oidc"
	"github.com/samyfodil/musql/cloud/state"
)

// dsnMode is the DSN parameter that marks a connection serving Hrana
// clients: "rw" or "ro". The driver ignores parameters it does not know; the
// hook below reads this one and confines the connection.
const dsnMode = "musqlcloud"

var hookOnce sync.Once

func registerHook() {
	hookOnce.Do(func() {
		driver.RegisterConnectionHook(func(c *driver.Conn, dsn string) error {
			_, query, ok := strings.Cut(dsn, "?")
			if !ok {
				return nil
			}
			v, err := url.ParseQuery(query)
			if err != nil {
				return nil
			}
			switch v.Get(dsnMode) {
			case "rw":
				c.Confine(false)
			case "ro":
				c.Confine(true)
			}
			return nil
		})
	})
}

const idleClose = 5 * time.Minute

// errUnavailable is a database being restored in place.
var errUnavailable = errors.New("the database is being restored")

// errNotHere is a crdt database not placed at this node's location.
var errNotHere = errors.New("not placed here")

// errSyncing is a crdt database placed here whose first sync with the peers
// has not completed: this node holds only part of it.
var errSyncing = errors.New("this database was just placed here and is still syncing from its peers; it is served once that completes")

type entry struct {
	db       *sql.DB
	ro       *sql.DB
	srv      *hrana.Server
	shared   bool // db is a crdt database's replicated handle, not the entry's
	last     time.Time
	inflight int
}

// pool holds the open databases, opened on first use and closed when idle.
// Each database has two handles, a full-access one and a read-only one, behind
// one Hrana server that picks by the client's token.
type pool struct {
	open    func(d *state.Database) (*entry, error)
	mu      sync.Mutex
	m       map[string]*entry
	blocked map[string]bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

func newPool(open func(d *state.Database) (*entry, error)) *pool {
	registerHook()
	p := &pool{open: open, m: map[string]*entry{}, blocked: map[string]bool{}, stop: make(chan struct{})}
	p.wg.Add(1)
	go p.reaper()
	return p
}

// get returns a database's open entry, and a release to call when done with
// it.
func (p *pool) get(d *state.Database) (e *entry, release func(), err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blocked[d.ID] {
		return nil, nil, errUnavailable
	}
	e, ok := p.m[d.ID]
	if !ok {
		if e, err = p.open(d); err != nil {
			return nil, nil, err
		}
		p.m[d.ID] = e
	}
	e.inflight++
	e.last = time.Now()
	return e, func() {
		p.mu.Lock()
		e.inflight--
		e.last = time.Now()
		p.mu.Unlock()
	}, nil
}

func (e *entry) close() {
	e.srv.Close()
	if !e.shared {
		e.db.Close()
	}
	e.ro.Close()
}

// drop closes a database's servers, before its file is removed or replaced.
func (p *pool) drop(id string) {
	p.mu.Lock()
	e, ok := p.m[id]
	delete(p.m, id)
	p.mu.Unlock()
	if ok {
		e.close()
	}
}

// block closes a database's servers and keeps it closed until the returned
// unblock is called: every request meanwhile gets errUnavailable.
func (p *pool) block(id string) (unblock func()) {
	p.mu.Lock()
	p.blocked[id] = true
	p.mu.Unlock()
	p.drop(id)
	return func() {
		p.mu.Lock()
		delete(p.blocked, id)
		p.mu.Unlock()
	}
}

func (p *pool) reaper() {
	defer p.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
		cut := time.Now().Add(-idleClose)
		p.mu.Lock()
		var gone []*entry
		for k, e := range p.m {
			if e.inflight == 0 && e.last.Before(cut) {
				gone = append(gone, e)
				delete(p.m, k)
			}
		}
		p.mu.Unlock()
		for _, e := range gone {
			e.close()
		}
	}
}

func (p *pool) closeAll() {
	close(p.stop)
	p.wg.Wait()
	p.mu.Lock()
	all := p.m
	p.m = map[string]*entry{}
	p.mu.Unlock()
	for _, e := range all {
		e.close()
	}
}

// openEntry is the pool's open: a full-access and a read-only handle behind
// a Hrana server that checks each client's token and meters its statements.
// It opens only once the database's history is, so no commit goes unlogged.
func (s *Server) openEntry(d *state.Database) (*entry, error) {
	path := s.dbPath(d)
	if s.cl != nil && s.replHandle(d.ID) == nil {
		return nil, errNotHere
	}
	if s.cl != nil && s.replSyncing(d.ID) {
		return nil, errSyncing
	}
	// Opening creates a missing file; a database whose file is gone is gone.
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	if _, err := s.history(d); err != nil {
		return nil, err
	}
	// A crdt database is written through its replicated handle, which
	// outlives the entry; any other connection may only read it.
	db, shared := s.replHandle(d.ID), true
	if db == nil {
		var err error
		if db, err = openPinged(path + "?" + dsnMode + "=rw"); err != nil {
			return nil, err
		}
		shared = false
	}
	ro, err := openPinged(path + "?" + dsnMode + "=ro")
	if err != nil {
		if !shared {
			db.Close()
		}
		return nil, err
	}
	return &entry{db: db, ro: ro, shared: shared, srv: hrana.NewServer(db,
		hrana.WithReadOnlyDB(ro),
		hrana.WithAuth(s.hranaAuth(d.OrgID, d.ID)),
		hrana.WithObserver(s.meter.observer(d.ID, d.OrgID)))}, nil
}

func openPinged(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// dbDir is the directory an org's database files are in.
func (s *Server) dbDir(d *state.Database) string {
	return filepath.Join(s.cfg.DataDir, "databases", d.OrgID)
}

// dbPath is where a database's file lives: named by IDs, so renaming an org
// or a database moves nothing.
func (s *Server) dbPath(d *state.Database) string {
	if d.File != "" {
		return filepath.Join(s.dbDir(d), d.File)
	}
	return filepath.Join(s.dbDir(d), d.ID+".musq")
}

// removeFiles closes a database and deletes its file, any sidecars and its
// history.
func (s *Server) removeFiles(d *state.Database) {
	s.pool.drop(d.ID)
	s.closeReplicated(d.ID)
	s.closeHistory(d.ID)
	removeDBFiles(s.log, s.dbPath(d))
	if err := os.RemoveAll(s.histDir(d)); err != nil {
		s.log.Printf("remove %s: %v", s.histDir(d), err)
	}
}

// removeDBFiles deletes a database file and the engine's files beside it.
func removeDBFiles(l interface{ Printf(string, ...any) }, p string) {
	matches, err := filepath.Glob(p + "*")
	if err != nil {
		l.Printf("remove %s: %v", p, err)
		return
	}
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			l.Printf("remove %s: %v", m, err)
		}
	}
}

func hranaError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(`{"message":"` + msg + `"}`))
}

// serveDB serves Hrana at /db/<org>/<name>/..., for a database token or a
// user's token.
func (s *Server) serveDB(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/db/")
	orgSlug, rest, _ := strings.Cut(rest, "/")
	name, sub, _ := strings.Cut(rest, "/")
	ctx := r.Context()
	o, err := s.store.OrgBySlug(ctx, orgSlug)
	if err != nil {
		hranaError(w, http.StatusNotFound, "no such database")
		return
	}
	d, err := s.store.Database(ctx, o.ID, name)
	if err != nil {
		hranaError(w, http.StatusNotFound, "no such database")
		return
	}
	// A regular database in a cluster is served by its group's master, reads
	// included: a stream's statements run where its transaction is.
	if base, err := s.masterURL(ctx, d); err != nil {
		hranaError(w, http.StatusServiceUnavailable, errNoMaster.Error())
		return
	} else if base != "" {
		s.proxy(w, r, base)
		return
	}
	e, release, err := s.pool.get(d)
	// A copy in its first sync is a moment from whole, most of all a new
	// database's on the master its group just elected: the request waits
	// for it, as for the election, before it is refused.
	for deadline := time.Now().Add(masterWait); errors.Is(err, errSyncing) && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
		e, release, err = s.pool.get(d)
	}
	if errors.Is(err, errUnavailable) {
		hranaError(w, http.StatusServiceUnavailable, "the database is being restored")
		return
	}
	if errors.Is(err, errNotHere) {
		hranaError(w, http.StatusNotFound, s.notHere(ctx, d).Error())
		return
	}
	if errors.Is(err, errSyncing) {
		hranaError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err != nil {
		s.log.Printf("open %s/%s: %v", o.Slug, d.Name, err)
		hranaError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	defer release()
	r2 := r.Clone(ctx)
	r2.URL.Path = "/" + sub
	r2.URL.RawPath = ""
	e.srv.ServeHTTP(w, r2)
}

// hranaAuth is the token check of a database's Hrana server: HTTP requests
// and WebSocket hellos alike. It looks the database up at each call, so a
// rename, an invalidation or a role change takes effect at once.
func (s *Server) hranaAuth(orgID, dbID string) func(string) (bool, error) {
	return func(tok string) (bool, error) {
		ctx := context.Background()
		o, err := s.store.OrgByID(ctx, orgID)
		if err != nil {
			return false, errNoAccess
		}
		d, err := s.store.DatabaseByID(ctx, dbID)
		if err != nil {
			return false, errNoAccess
		}
		return s.dbAccess(ctx, tok, o, d)
	}
}

var errNoAccess = errors.New("no access")

// dbAccess decides what a bearer token may do to d: (readOnly, nil) when it
// may connect at all.
func (s *Server) dbAccess(ctx context.Context, tok string, o *state.Org, d *state.Database) (bool, error) {
	if tok == "" {
		return false, errNoAccess
	}
	if c, err := s.provider.Verify(tok, oidc.UseDB); err == nil {
		switch {
		case c.DB != "":
			if c.DB != d.ID || c.Gen != d.TokenGen {
				return false, errNoAccess
			}
		case c.Group != "":
			g, err := s.store.GroupByID(ctx, c.Group)
			if err != nil || c.Group != d.GroupID || c.Gen != g.TokenGen {
				return false, errNoAccess
			}
		default:
			return false, errNoAccess
		}
		switch c.Access {
		case accessFull:
			return false, nil
		case accessReadOnly:
			return true, nil
		}
		return false, errNoAccess
	}
	u, err := s.userFromToken(ctx, tok)
	if err != nil {
		return false, errNoAccess
	}
	r, err := s.role(ctx, u, o)
	if err != nil || r == "" {
		return false, errNoAccess
	}
	return !r.AtLeast(state.RoleMember), nil
}

const (
	accessFull     = "full"
	accessReadOnly = "read-only"
)
