package hrana

import (
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
	opts   []Option

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
	return &Multi{dir: dir, create: create, opts: opts, dbs: map[string]*multiDB{}}
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
	srv, err := m.server(name)
	if err != nil {
		http.Error(w, `{"message":"no such database"}`, http.StatusNotFound)
		return
	}
	srv.ServeHTTP(w, r)
}

func (m *Multi) server(name string) (*Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.dbs[name]; ok {
		return d.srv, nil
	}
	path := filepath.Join(m.dir, name+".musq")
	if !m.create {
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
	d := &multiDB{db: db, srv: NewServer(db, m.opts...)}
	m.dbs[name] = d
	return d.srv, nil
}
