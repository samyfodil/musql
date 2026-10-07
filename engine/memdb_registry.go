// Process-wide registry for shared memdb ATTACH URIs, porting memdb_g.apMemStore[]
// from memdb.c. See attach.go's resolveAttachURIPath for the rules this implements.
//
// # What C SQLite does (memdb.c), and what this ports
//
// memdb.c:97-100's memdb_g.apMemStore[] is a single, process-wide,
// mutex-protected array of MemStore objects, looked up/created by an exact
// string compare on the leading-'/' (or '\\') name (memdbOpen,
// memdb.c:556-566: `strcmp(memdb_g.apMemStore[i]->zFName,zName)==0`). A
// second ATTACH (or a second, wholly independent connection, even in a
// different OS thread) naming the same string finds the SAME store and
// bumps its refcount (nRef++) rather than copying anything; the store's
// backing buffer is freed only when the LAST handle closes (memdbClose,
// memdb.c:210-247: `p->nRef--; if (p->nRef<=0) { free... }`).
//
// A shared store's content is backed by a real temp file, reusing the existing
// file backend (lock.go, WAL, encoding, page size, etc.). The only new mechanism
// is the name -> refcounted-store lookup. Shared stores are freed when the last
// handle closes; a new ATTACH of the same name gets a fresh store.
//
// Two aliases of one store in one session enforce lock arbitration: a new
// SHARED request is refused while another handle holds RESERVED, giving
// "database is locked" reads on a transaction that wrote through the other
// alias. Read pagers are held for the life of the binding, so reads don't
// refresh after detached sessions write to the shared store.
package engine

import (
	"fmt"
	"os"
	"sync"
)

// memdbRegistry is the process-wide (package-level, so shared by every
// engine.DB opened anywhere in this Go process) name -> shared-store table,
// mirroring memdb_g.apMemStore[] plus its SQLITE_MUTEX_STATIC_VFS1 guard
// mutex (memdb.c:94-100).
var memdbRegistry = struct {
	mu     sync.Mutex
	stores map[string]*memdbSharedStore
}{stores: make(map[string]*memdbSharedStore)}

// memdbSharedStore is one shared memdb store, keyed by its exact leading-'/'
// (or '\\') name string. path is the real temp file backing its content (see
// this file's package comment); refCount mirrors MemStore.nRef.
type memdbSharedStore struct {
	path     string
	refCount int
}

// memdbAcquire finds-or-creates the shared store named name (the exact
// leading-'/'/'\\' text between "file:" and "?" in the ATTACH URI, e.g.
// "/one") and returns the real file backing its content, bumping the
// store's refcount -- memdbOpen's own p->nRef++ on a hit, or a fresh
// nRef==1 store on a miss (memdb.c:562-599). A fresh store's backing file is
// a new, EMPTY temp file (mirroring the ':memory:' convention execAttach's
// own isMem branch already uses): the caller still runs it through
// ensureDatabaseFile exactly as any other freshly-attached path, so
// bootstrap and page-size/encoding handling are unchanged from the existing
// single-file ATTACH path.
//
// Every successful call must be paired with exactly one later
// memdbRelease(name) (attachedDB.close, attach.go) -- mirroring memdbClose's
// own nRef-- on every close, shared or not.
func memdbAcquire(name string) (path string, err error) {
	memdbRegistry.mu.Lock()
	defer memdbRegistry.mu.Unlock()
	if s, ok := memdbRegistry.stores[name]; ok {
		s.refCount++
		return s.path, nil
	}
	f, ferr := os.CreateTemp("", "musql-memdb-*.musq")
	if ferr != nil {
		return "", fmt.Errorf("engine: ATTACH vfs=memdb: %w", ferr)
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("engine: ATTACH vfs=memdb: %w", cerr)
	}
	memdbRegistry.stores[name] = &memdbSharedStore{path: f.Name(), refCount: 1}
	return f.Name(), nil
}

// memdbRelease drops one reference on the shared store named name, deleting
// its backing temp file once the LAST reference releases -- memdbClose's own
// `p->nRef--; if (p->nRef<=0) { ...free... }` (memdb.c:232-238). A name not
// currently registered is a silent no-op: it means the matching
// memdbAcquire itself failed (execAttach never sets attachedDB.isMemdb in
// that case), so there is nothing to release.
func memdbRelease(name string) {
	memdbRegistry.mu.Lock()
	defer memdbRegistry.mu.Unlock()
	s, ok := memdbRegistry.stores[name]
	if !ok {
		return
	}
	s.refCount--
	if s.refCount <= 0 {
		delete(memdbRegistry.stores, name)
		os.Remove(s.path)
	}
}

// memdbPathSentinel prefixes the string resolveAttachURIPath (attach.go)
// returns for a SHARED "file:/name?vfs=memdb" attachment. It is NOT a
// filesystem path -- it carries the memdb NAME (e.g. "/one") that
// execAttach must resolve through memdbAcquire above rather than opening
// directly. A leading NUL byte can never appear in any path this parser's
// own grammar produces (attachExprLiteral only ever binds a plain
// string/identifier/numeric/CTIME literal token, none of which can embed
// one -- SQL has no string escape for it, and a BLOB literal is a token
// kind attachExprLiteral does not resolve at all), so this can never
// collide with a genuinely resolved filesystem path. IsMemdbAttachName is
// the matching reader, exported for driver's own ATTACH parsing (which
// shares ParseAttachStmt, per attach.go's package comment) to check BEFORE
// ever treating a resolved "path" as literal.
const memdbPathSentinel = "\x00memdb:"

// IsMemdbAttachName reports whether path (as returned by ParseAttachStmt) is
// the shared-memdb sentinel form, and if so returns the underlying memdb
// name. A caller that does not resolve it through memdbAcquire (today,
// driver's own ATTACH -- see driver/attach.go) must check this
// BEFORE treating path as a literal filesystem path: the raw memdb name
// (e.g. "/one") is an absolute-looking string this engine never intends to
// open directly, and passing it to os.Open/os.Create would try to touch
// that literal path on the real filesystem.
func IsMemdbAttachName(path string) (name string, ok bool) {
	if len(path) < len(memdbPathSentinel) || path[:len(memdbPathSentinel)] != memdbPathSentinel {
		return "", false
	}
	return path[len(memdbPathSentinel):], true
}

// sharedMemPathSentinel is memdbPathSentinel's twin for a
// "file:X?mode=memory&cache=shared" ATTACH (see resolveAttachURIPath's
// "cache" handling, attach.go): a DIFFERENT feature -- C SQLite's named
// in-memory shared-cache database (btree.c:2594-2649's isMemdb +
// SQLITE_OPEN_SHAREDCACHE branch, keyed by the exact zFilename text) rather
// than memdb.c's own separate MemStore table -- that happens to need the
// exact same shape of process-wide, refcounted, name -> backing-file lookup
// this file already provides for vfs=memdb. Rather than duplicate
// memdbAcquire/memdbRelease, a cache=shared attachment reuses them under
// sharedMemRegistryKey's namespaced key, so the two features' names can never
// collide in the one shared map even though a cache=shared name has none of
// vfs=memdb's leading-'/'-or-'\\' shape requirement (attach.test's own mined
// case is a 9000-digit hex string with no leading slash at all).
const sharedMemPathSentinel = "\x00sharedmem:"

// IsSharedMemAttachName is sharedMemPathSentinel's reader, mirroring
// IsMemdbAttachName exactly.
func IsSharedMemAttachName(path string) (name string, ok bool) {
	if len(path) < len(sharedMemPathSentinel) || path[:len(sharedMemPathSentinel)] != sharedMemPathSentinel {
		return "", false
	}
	return path[len(sharedMemPathSentinel):], true
}

// sharedMemRegistryKey namespaces a cache=shared name before it reaches
// memdbAcquire/memdbRelease, so it can never collide with a genuine vfs=memdb
// name in the SAME map -- a NUL byte can never appear in either (attachExprLiteral
// only ever binds a plain string/identifier/numeric/CTIME literal token, and a
// percent-escaped URI component is declined outright, per this file's and
// attach.go's own package comments), so this prefix can never be produced by
// a real name from either namespace.
func sharedMemRegistryKey(name string) string {
	return "cache-shared\x00" + name
}
