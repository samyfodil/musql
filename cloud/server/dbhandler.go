package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/samyfodil/musql/hrana"

	"github.com/samyfodil/musql/cloud/state"
)

// DatabaseRequest is what a WithDatabaseHandler handler gets with a request
// the server has let through.
type DatabaseRequest struct {
	Org      *state.Org
	Database *state.Database
	// File is the database's file on this node. Open it read-only: writes
	// go through Hrana, which logs them in the database's history.
	File string
	// Dir is the handler's own directory for this database, made before the
	// handler is called and removed with the database.
	Dir string
	// Access is what the request's token grants, read-only too while the
	// database's blocks or the access policy refuse writes. A token with
	// permissions (Access.NewAuthorizer set) may see only some tables.
	Access hrana.Access
}

// WithDatabaseHandler serves pattern under each database's URL beside Hrana,
// /db/<org>/<name>/info for pattern "GET /info": a net/http ServeMux pattern
// over the path after the database's name, taking precedence over Hrana's.
// Each request is handled as a Hrana request is before h is called: the
// database is looked up and, in a cluster, the request goes to the node that
// serves it; the bearer token is checked, as by Hrana, and refused with 401;
// a database whose blocks or access policy refuse reads is refused with 403
// and the reason. h is called for each request let through.
func WithDatabaseHandler(pattern string, h func(DatabaseRequest) http.Handler) Option {
	return func(s *Server) {
		if s.dbMux == nil {
			s.dbMux = http.NewServeMux()
		}
		s.dbMux.Handle(pattern, dbRoute(h))
	}
}

// dbRoute is a WithDatabaseHandler handler as its pattern is registered.
type dbRoute func(DatabaseRequest) http.Handler

func (h dbRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h(r.Context().Value(dbRequestKey{}).(DatabaseRequest)).ServeHTTP(w, r)
}

type dbRequestKey struct{}

// dbHandles reports whether a WithDatabaseHandler pattern matches r. The
// mux's own redirects (a path one slash short of a "/files/" pattern, say)
// do not count: they would send the client to the path after the
// database's name, which is not where the database is.
func (s *Server) dbHandles(r *http.Request) bool {
	if s.dbMux == nil {
		return false
	}
	h, _ := s.dbMux.Handler(r)
	_, ok := h.(dbRoute)
	return ok
}

// serveDBHandler checks r as Hrana would before handing it to the handler
// its pattern names. The mux serves it, so the pattern's wildcards are the
// request's PathValues.
func (s *Server) serveDBHandler(w http.ResponseWriter, r *http.Request, o *state.Org, d *state.Database, e *entry) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	acc, err := s.dbAccess(r.Context(), tok, o, d)
	switch {
	case errors.Is(err, hrana.ErrUnavailable):
		hranaError(w, http.StatusServiceUnavailable, "the token cannot be checked now")
		return
	case err != nil:
		hranaError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	e.setBlock(d) // as hranaAuth's refresh does: d is the record as it is now
	b := e.blocks()
	if b.Reads {
		hranaError(w, http.StatusForbidden, b.Reason)
		return
	}
	acc.ReadOnly = acc.ReadOnly || b.Writes
	dir := s.handlerDir(d)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Printf("handler dir %s: %v", dir, err)
		hranaError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	req := DatabaseRequest{Org: o, Database: d, File: s.dbPath(d), Dir: dir, Access: acc}
	s.dbMux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), dbRequestKey{}, req)))
}

// handlerDir is a database's WithDatabaseHandler directory.
func (s *Server) handlerDir(d *state.Database) string {
	return filepath.Join(s.cfg.DataDir, "handlers", d.OrgID, d.ID)
}
