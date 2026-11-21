package cli_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/tursodatabase/libsql-client-go/libsql"

	"github.com/samyfodil/musql/cloud/cli"
)

// startDev runs musql dev on a free port and returns the URL it prints.
func startDev(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		env := cli.Env{Dir: t.TempDir(), Stdin: strings.NewReader(""), Stdout: pw, Stderr: io.Discard}
		done <- cli.Run(ctx, env, append([]string{"dev", "--port", "0"}, args...))
		pw.Close()
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("musql dev: %v", err)
		}
	})
	urls := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if u, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "http://"); ok {
				urls <- "http://" + u
			}
		}
	}()
	select {
	case u := <-urls:
		return u
	case err := <-done:
		t.Fatalf("musql dev exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("musql dev printed no URL")
	}
	return ""
}

func devClient(t *testing.T, url, token string) *sql.DB {
	t.Helper()
	var opts []libsql.Option
	if token != "" {
		opts = append(opts, libsql.WithAuthToken(token))
	}
	conn, err := libsql.NewConnector(url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	t.Cleanup(func() { db.Close() })
	return db
}

// TestDevRoundTrip: musql dev serves a database file to a libSQL client, and
// what the client wrote is in the file.
func TestDevRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.musq")
	url := startDev(t, "--db-file", file)
	for _, u := range []string{url, strings.Replace(url, "http://", "ws://", 1)} {
		db := devClient(t, u, "")
		for _, q := range []string{"CREATE TABLE IF NOT EXISTS people(id INTEGER, name TEXT)", "INSERT INTO people VALUES (42, 'John')"} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", u, err)
			}
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM people WHERE name = ?", "John").Scan(&n); err != nil || n == 0 {
			t.Fatalf("%s: %d %v", u, n, err)
		}
	}
	local, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	var n int
	if err := local.QueryRow("SELECT count(*) FROM people").Scan(&n); err != nil || n != 2 {
		t.Fatalf("file: %d %v", n, err)
	}
}

// TestDevTemporary: without --db-file the database is a temporary one that
// works for the server's life.
func TestDevTemporary(t *testing.T) {
	db := devClient(t, startDev(t), "")
	var v int
	if _, err := db.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT x FROM t").Scan(&v); err != nil || v != 7 {
		t.Fatalf("%d %v", v, err)
	}
}

// TestDevJWT: with --auth-jwt-key-file a client needs a JWT the key signed;
// "a": "ro" reads only, as libSQL server takes it. The key is read in both
// forms libSQL server reads.
func TestDevJWT(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pemFile, rawFile := filepath.Join(dir, "key.pem"), filepath.Join(dir, "key.b64")
	if err := os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawFile, []byte(base64.RawURLEncoding.EncodeToString(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sign := func(k ed25519.PrivateKey, claims map[string]any) string {
		sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: k}, nil)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := jwt.Signed(sig).Claims(claims).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).Unix()
	for _, key := range []string{pemFile, rawFile} {
		url := startDev(t, "--auth-jwt-key-file", key)
		rw := devClient(t, url, sign(priv, map[string]any{"exp": exp}))
		if _, err := rw.Exec("CREATE TABLE t(x); INSERT INTO t VALUES (1)"); err != nil {
			t.Fatalf("%s: full access: %v", key, err)
		}
		ro := devClient(t, url, sign(priv, map[string]any{"a": "ro"}))
		var n int
		if err := ro.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: read-only read: %d %v", key, n, err)
		}
		if _, err := ro.Exec("INSERT INTO t VALUES (2)"); err == nil {
			t.Fatalf("%s: a read-only token wrote", key)
		}
		for name, tok := range map[string]string{
			"none":        "",
			"other key":   sign(other, nil),
			"expired":     sign(priv, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}),
			"not a token": "secret",
		} {
			if err := devClient(t, url, tok).QueryRow("SELECT 1").Scan(&n); err == nil {
				t.Fatalf("%s: %s accepted", key, name)
			}
		}
	}
}
