package hrana

import (
	"crypto/subtle"
	"database/sql"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Multi serves every database in a directory, choosing one per request from the
// first label of the request's host, the way Turso names databases
// (<db>.<host>): "app.example.com" serves <dir>/app.musq. Hrana itself has no
// way to select a database, so the address is the selector.
type Multi struct {
	dir    string
	create bool
	config // for Multi's own auth check and for each database's Server

	mu  sync.Mutex
	dbs map[string]*multiDB
}

type multiDB struct {
	db  *sql.DB
	srv *Server
}

var dbName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// NewMulti serves the databases in dir. With create, a name that has no file
// yet gets a new empty database; without it, such a request is a 404.
func NewMulti(dir string, create bool, opts ...Option) *Multi {
	return &Multi{dir: dir, create: create, config: newConfig(opts), dbs: map[string]*multiDB{}}
}

// Close closes every open database.
func (m *Multi) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, d := range m.dbs {
		d.srv.Close()
		d.db.Close()
		delete(m.dbs, name)
	}
	return nil
}

// nameOf is the database a host names: its first label, unless the host is a
// bare IP address or a single label (such as "localhost").
func nameOf(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if net.ParseIP(host) != nil {
		return ""
	}
	label, rest, ok := strings.Cut(host, ".")
	if !ok || rest == "" {
		return ""
	}
	return label
}

func (m *Multi) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := nameOf(r.Host)
	if !dbName.MatchString(name) {
		http.Error(w, `{"message":"no database in the host name; use <db>.<host>"}`, http.StatusNotFound)
		return
	}
	// A request must be authorized before it can open, let alone create, a
	// database file. A WebSocket client authenticates in its hello message,
	// after the upgrade, so without a valid header it may only reach a
	// database that already exists; creating one takes an authorized request.
	authed := m.authToken == "" || bearerOK(r, m.authToken)
	if !authed && !isWebSocket(r) {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	srv, err := m.server(name, authed)
	if err != nil {
		http.Error(w, `{"message":"no such database"}`, http.StatusNotFound)
		return
	}
	srv.ServeHTTP(w, r)
}

// bearerOK reports whether r carries "Authorization: Bearer <token>".
func bearerOK(r *http.Request, token string) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1
}

// server is name's server. With create, a name with no file yet gets a new
// empty database; create is false for a request not yet authorized.
func (m *Multi) server(name string, create bool) (*Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.dbs[name]; ok {
		return d.srv, nil
	}
	path := filepath.Join(m.dir, name+".musq")
	if !m.create || !create {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	d := &multiDB{db: db, srv: newServer(db, m.config)}
	m.dbs[name] = d
	return d.srv, nil
}
