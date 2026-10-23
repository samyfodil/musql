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

type entry struct {
	db       *sql.DB
	ro       *sql.DB
	srv      *hrana.Server
	last     time.Time
	inflight int
}

// pool holds the open databases, opened on first use and closed when idle.
// Each database has two handles, a full-access one and a read-only one, behind
// one Hrana server that picks by the client's token.
type pool struct {
	mu   sync.Mutex
	m    map[string]*entry
	stop chan struct{}
	wg   sync.WaitGroup
}

func newPool() *pool {
	registerHook()
	p := &pool{m: map[string]*entry{}, stop: make(chan struct{})}
	p.wg.Add(1)
	go p.reaper()
	return p
}

// get returns the Hrana server for a database file, and a release to call
// when the request is done. check authenticates its clients; it is used only
// when the server is first opened.
func (p *pool) get(id, path string, check func(token string) (bool, error)) (*hrana.Server, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[id]
	if !ok {
		if _, err := os.Stat(path); err != nil {
			return nil, nil, err
		}
		db, err := sql.Open("sqlite", path+"?"+dsnMode+"=rw")
		if err != nil {
			return nil, nil, err
		}
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, nil, err
		}
		ro, err := sql.Open("sqlite", path+"?"+dsnMode+"=ro")
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		if err := ro.Ping(); err != nil {
			ro.Close()
			db.Close()
			return nil, nil, err
		}
		e = &entry{db: db, ro: ro, srv: hrana.NewServer(db, hrana.WithReadOnlyDB(ro), hrana.WithAuth(check))}
		p.m[id] = e
	}
	e.inflight++
	e.last = time.Now()
	return e.srv, func() {
		p.mu.Lock()
		e.inflight--
		e.last = time.Now()
		p.mu.Unlock()
	}, nil
}

func (e *entry) close() {
	e.srv.Close()
	e.db.Close()
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

// dbPath is where a database's file lives: named by IDs, so renaming an org
// or a database moves nothing.
func (s *Server) dbPath(d *state.Database) string {
	return filepath.Join(s.cfg.DataDir, "databases", d.OrgID, d.ID+".musq")
}

// removeFiles closes a database and deletes its file and any sidecars.
func (s *Server) removeFiles(d *state.Database) {
	s.pool.drop(d.ID)
	p := s.dbPath(d)
	matches, err := filepath.Glob(p + "*")
	if err != nil {
		s.log.Printf("remove %s: %v", p, err)
		return
	}
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			s.log.Printf("remove %s: %v", m, err)
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
	srv, release, err := s.pool.get(d.ID, s.dbPath(d), s.hranaAuth(o.ID, d.ID))
	if err != nil {
		s.log.Printf("open %s/%s: %v", o.Slug, d.Name, err)
		hranaError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	defer release()
	r2 := r.Clone(ctx)
	r2.URL.Path = "/" + sub
	r2.URL.RawPath = ""
	srv.ServeHTTP(w, r2)
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
