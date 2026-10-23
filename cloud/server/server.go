package server

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/oidc"
	"github.com/samyfodil/musql/cloud/state"
)

// maxUpload bounds a request body to the API; an upload carries a whole
// database file.
const maxUpload = 256 << 20

// Server is a running musql cloud node.
type Server struct {
	cfg      *Config
	store    *state.Store
	provider *oidc.Provider
	pool     *pool
	handler  http.Handler
	log      *log.Logger
}

// New opens the state in cfg.DataDir, makes sure the configured admin can
// sign in, and builds the handler.
func New(ctx context.Context, cfg *Config, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "musql-cloud: ", log.LstdFlags)
	}
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "databases"), filepath.Join(cfg.DataDir, "uploads")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	store, err := state.Open(filepath.Join(cfg.DataDir, "state.musq"))
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: store, log: logger}
	if err := s.ensureAdmin(ctx); err != nil {
		store.Close()
		return nil, err
	}
	if err := store.EnsureGroupLocation(ctx, cfg.Node.Location); err != nil {
		store.Close()
		return nil, err
	}
	if s.provider, err = oidc.New(ctx, store, cfg.PublicURL, oidc.WithConsoleOrigins(cfg.Console.CORSOrigins),
		oidc.WithLoginLimits(cfg.OIDC.LoginPerIP, cfg.OIDC.LoginPerEmail, cfg.OIDC.loginWindow)); err != nil {
		store.Close()
		return nil, err
	}
	s.pool = newPool()
	s.handler = s.routes()
	return s, nil
}

// Handler serves everything.
func (s *Server) Handler() http.Handler { return s.handler }

// Provider is the OIDC provider.
func (s *Server) Provider() *oidc.Provider { return s.provider }

// Store is the state database.
func (s *Server) Store() *state.Store { return s.store }

// Close closes every open database and the state.
func (s *Server) Close() error {
	s.pool.closeAll()
	return s.store.Close()
}

func (s *Server) ensureAdmin(ctx context.Context) error {
	hash := s.cfg.Admin.PasswordHash
	u, err := s.store.UserByEmail(ctx, s.cfg.Admin.Email)
	if errors.Is(err, state.ErrNotFound) {
		if hash == "" {
			hash = state.HashPassword(s.cfg.Admin.Password)
		}
		_, err = s.store.CreateUser(ctx, s.cfg.Admin.Email, "Admin", hash, true)
		return err
	}
	if err != nil {
		return err
	}
	// The config is the way back in: its password always works, and the
	// admin it names is always an enabled admin.
	if hash == "" && !state.VerifyPassword(u.PasswordHash, s.cfg.Admin.Password) {
		hash = state.HashPassword(s.cfg.Admin.Password)
	}
	if hash != "" && hash != u.PasswordHash {
		if err := s.store.SetPassword(ctx, u.ID, hash); err != nil {
			return err
		}
	}
	if !u.Admin || u.Disabled {
		u.Admin, u.Disabled = true, false
		return s.store.UpdateUser(ctx, u)
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	opts := []connect.HandlerOption{connect.WithReadMaxBytes(maxUpload)}
	mux.Handle(cloudv1connect.NewAuthServiceHandler(authService{s}, opts...))
	mux.Handle(cloudv1connect.NewUserServiceHandler(userService{s}, opts...))
	mux.Handle(cloudv1connect.NewOrgServiceHandler(orgService{s}, opts...))
	mux.Handle(cloudv1connect.NewGroupServiceHandler(groupService{s}, opts...))
	mux.Handle(cloudv1connect.NewDatabaseServiceHandler(databaseService{Server: s}, opts...))
	mux.Handle(cloudv1connect.NewAIServiceHandler(cloudv1connect.UnimplementedAIServiceHandler{}, opts...))
	mux.Handle("/db/", http.HandlerFunc(s.serveDB))
	op := s.provider.Handler()
	mux.Handle("/.well-known/openid-configuration", op)
	mux.Handle("/oidc/", op)
	mux.Handle("/login", op)
	mux.Handle("/console/", s.console())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusFound)
	})
	return s.cors(mux)
}

// oidcCORSPaths are the OIDC endpoints a console on another origin calls.
var oidcCORSPaths = []string{"/oidc/token", "/oidc/userinfo", "/oidc/logout", "/oidc/revoke"}

// cors lets the configured origins call the API, Hrana and the OIDC token
// endpoints from a browser.
// Credentials travel in the Authorization header, never in cookies, so no
// cookie-bearing request is allowed across origins.
func (s *Server) cors(next http.Handler) http.Handler {
	origins := s.cfg.Console.CORSOrigins
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		api := strings.HasPrefix(r.URL.Path, "/cloud.v1.") || strings.HasPrefix(r.URL.Path, "/db/") || slices.Contains(oidcCORSPaths, r.URL.Path)
		if origin != "" && api && (slices.Contains(origins, "*") || slices.Contains(origins, origin)) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Connect-Content-Encoding")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Connect-Accept-Encoding, Connect-Content-Encoding, X-User-Agent, X-Grpc-Web, Grpc-Timeout")
				h.Set("Access-Control-Max-Age", "7200")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// console serves the console's build, with index.html for any path that is
// not a file (the console routes on the client).
func (s *Server) console() http.Handler {
	if s.cfg.Console.Dir == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "the console is not configured (console.dir)", http.StatusNotFound)
		})
	}
	root := os.DirFS(s.cfg.Console.Dir)
	files := http.FileServerFS(root)
	return http.StripPrefix("/console", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" {
			if st, err := fs.Stat(root, name); err == nil && !st.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, root, "index.html")
	}))
}
