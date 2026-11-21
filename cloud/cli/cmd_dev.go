package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spf13/cobra"

	"github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/hrana"
)

// devMode is the DSN parameter that marks a connection serving dev's clients,
// "rw" or "ro", for the connection hook to confine it.
const devMode = "musqldev"

var devHookOnce sync.Once

func registerDevHook() {
	devHookOnce.Do(func() {
		driver.RegisterConnectionHook(func(c *driver.Conn, dsn string) error {
			_, query, ok := strings.Cut(dsn, "?")
			if !ok {
				return nil
			}
			v, err := url.ParseQuery(query)
			if err != nil {
				return nil
			}
			switch v.Get(devMode) {
			case "rw":
				c.Confine(false)
			case "ro":
				c.Confine(true)
			}
			return nil
		})
	})
}

// devCmd serves one database over Hrana on localhost for development, as
// turso dev does: no state, no sign-in, and no auth unless a JWT key is given.
func (a *app) devCmd() *cobra.Command {
	var (
		dbFile, keyFile string
		port            int
	)
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Serve a local database over Hrana for development",
		Long: `Serve one database over Hrana (HTTP and WebSocket) on localhost, for code
written against Turso or libSQL clients to run offline. Without --db-file the
database is temporary and gone when the server stops.`,
		Example: `  musql dev
  musql dev --db-file app.musq --port 8080
  musql dev --auth-jwt-key-file key.pem`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runDev(cmd.Context(), dbFile, keyFile, port)
		},
	}
	cmd.Flags().StringVarP(&dbFile, "db-file", "f", "", "the database file to serve (created if missing; default a temporary one)")
	cmd.Flags().IntVarP(&port, "port", "p", 8080, "the port to listen on")
	cmd.Flags().StringVarP(&keyFile, "auth-jwt-key-file", "a", "", "require JWTs signed by this Ed25519 public key: PEM, or the raw key in URL-safe base64")
	return cmd
}

func (a *app) runDev(ctx context.Context, dbFile, keyFile string, port int) error {
	var opts []hrana.Option
	if keyFile != "" {
		key, err := readJWTKey(keyFile)
		if err != nil {
			return err
		}
		opts = append(opts, hrana.WithAuth(jwtCheck(key)))
	}
	path := dbFile
	if path == "" {
		dir, err := os.MkdirTemp("", "musql-dev-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		path = filepath.Join(dir, "dev.musq")
	}
	registerDevHook()
	db, err := sql.Open("sqlite", path+"?"+devMode+"=rw")
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	ro, err := sql.Open("sqlite", path+"?"+devMode+"=ro")
	if err != nil {
		return err
	}
	defer ro.Close()
	srv := hrana.NewServer(db, append(opts, hrana.WithReadOnlyDB(ro))...)
	defer srv.Close()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- hs.Serve(ln) }()

	out := a.env.Stdout
	fmt.Fprintf(out, "musql dev listening on %s.\n", ln.Addr())
	fmt.Fprintf(out, "Use this URL in a libSQL client:\n\n    http://%s\n\n", ln.Addr())
	if keyFile != "" {
		fmt.Fprintf(out, "Clients need a JWT signed by the key in %s.\n", keyFile)
	} else {
		fmt.Fprintln(out, "No auth token is required; --auth-jwt-key-file requires one.")
	}
	if dbFile != "" {
		fmt.Fprintf(out, "Serving %s.\n", dbFile)
	} else {
		fmt.Fprintln(out, "The database is temporary and gone when this stops; --db-file keeps one.")
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		return err
	}
	return nil
}

// readJWTKey reads an Ed25519 public key as libSQL server takes it: a PKIX
// key in PEM, or the 32 raw bytes in URL-safe base64.
func readJWTKey(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if blk, _ := pem.Decode(b); blk != nil {
		k, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		pub, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an Ed25519 public key", path)
		}
		return pub, nil
	}
	s := strings.TrimRight(strings.TrimSpace(string(b)), "=")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s: not an Ed25519 public key in PEM or URL-safe base64", path)
	}
	return ed25519.PublicKey(raw), nil
}

// jwtCheck accepts a JWT signed by key and not expired; its "a" claim "ro"
// makes the client read-only, as libSQL server reads it.
func jwtCheck(key ed25519.PublicKey) func(string) (bool, error) {
	return func(tok string) (bool, error) {
		t, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.EdDSA})
		if err != nil {
			return false, err
		}
		var std jwt.Claims
		var c struct {
			A string `json:"a"`
		}
		if err := t.Claims(key, &std, &c); err != nil {
			return false, err
		}
		if err := std.ValidateWithLeeway(jwt.Expected{Time: time.Now()}, time.Minute); err != nil {
			return false, err
		}
		switch c.A {
		case "", "rw":
			return false, nil
		case "ro":
			return true, nil
		}
		return false, errors.New("unknown access")
	}
}
