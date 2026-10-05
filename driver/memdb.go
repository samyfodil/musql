package driver

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// In-memory databases are backed by temp files since the engine always reads
// and writes files. ":memory:" and "?mode=memory" DSNs are private per connection
// or shared by name, refcounted and removed when the last connection closes.

// memBacked reports whether path is a memory-backed database file, either the
// main database or an attached in-memory database.
func (c *Conn) memBacked(path string) bool {
	if (c.memPrivate || c.memShared != "") && path == c.path {
		return true
	}
	for _, a := range c.attached {
		if a.isMem && a.path == path {
			return true
		}
	}
	return false
}

type memEntry struct {
	path string
	refs int
}

var sharedMem struct {
	mu sync.Mutex
	m  map[string]*memEntry // shared-cache name -> backing file
}

// memBacking decides how a DSN's in-memory database is backed. It returns the
// file path to use, whether that file is private to this connection (and so
// should be removed when it closes), and the shared-cache name it took a
// reference on (released by releaseSharedMem). ok is false for an ordinary
// file DSN.
func memBacking(dsn string) (path string, private bool, shared string, ok bool) {
	base := splitDSN(dsn)
	query := ""
	if i := strings.IndexByte(dsn, '?'); i >= 0 {
		query = dsn[i+1:]
	}
	v, _ := url.ParseQuery(query)
	isMem := strings.EqualFold(base, ":memory:") || strings.EqualFold(v.Get("mode"), "memory")
	if !isMem {
		return "", false, "", false
	}
	name := base
	if strings.EqualFold(name, ":memory:") {
		name = ""
	}
	if name != "" && strings.EqualFold(v.Get("cache"), "shared") {
		return sharedMemPath(name), false, name, true
	}
	f, err := os.CreateTemp("", "musql-mem-*.db")
	if err != nil {
		// Nothing sensible to fall back to; let the caller open the literal
		// path and report the real error from there.
		return base, false, "", true
	}
	p := f.Name()
	f.Close()
	return p, true, "", true
}

// sharedMemPath returns the temp file backing the shared in-memory database
// called name, creating it on first use, and takes a reference on it (released
// by releaseSharedMem when the connection closes).
func sharedMemPath(name string) string {
	sharedMem.mu.Lock()
	defer sharedMem.mu.Unlock()
	if sharedMem.m == nil {
		sharedMem.m = map[string]*memEntry{}
	}
	e, ok := sharedMem.m[name]
	if !ok {
		dir, err := os.MkdirTemp("", "musql-mem-shared-*")
		if err != nil {
			dir = os.TempDir()
		}
		e = &memEntry{path: filepath.Join(dir, "db")}
		sharedMem.m[name] = e
	}
	e.refs++
	return e.path
}

// releaseSharedMem drops one reference to a shared in-memory database, deleting
// its backing file once no connection is left.
func releaseSharedMem(name string) {
	sharedMem.mu.Lock()
	defer sharedMem.mu.Unlock()
	e, ok := sharedMem.m[name]
	if !ok {
		return
	}
	e.refs--
	if e.refs > 0 {
		return
	}
	delete(sharedMem.m, name)
	os.RemoveAll(filepath.Dir(e.path))
}
