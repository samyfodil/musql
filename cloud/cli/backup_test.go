package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/samyfodil/musql/cloud/cli"
)

// fakeS3 is an S3 server holding one bucket, "bkt", and the environment that
// reaches it.
func fakeS3(t *testing.T) (endpoint string, getenv func(string) string) {
	t.Helper()
	be := s3mem.New()
	if err := be.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(be).Server())
	t.Cleanup(srv.Close)
	env := map[string]string{"AWS_ACCESS_KEY_ID": "id", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_REGION": "us-east-1"}
	return srv.URL, func(k string) string { return env[k] }
}

func rows(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := db.Query(`SELECT a, b FROM t ORDER BY a`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out strings.Builder
	for r.Next() {
		var a int
		var b string
		if err := r.Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		out.WriteString(b + ";")
	}
	return out.String()
}

// TestBackupRestore: backups taken across row writes and a rewrite (CREATE
// INDEX) restore every point they were taken at, and the latest by default.
func TestBackupRestore(t *testing.T) {
	endpoint, getenv := fakeS3(t)
	dir := t.TempDir()
	run := func(args ...string) (string, error) {
		var out, errs bytes.Buffer
		err := cli.Run(context.Background(), cli.Env{Dir: t.TempDir(), Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errs, Getenv: getenv},
			append(args, "--endpoint", endpoint))
		if err != nil {
			t.Logf("%v: stderr %s", args, errs.String())
		}
		return out.String(), err
	}
	path := filepath.Join(dir, "app.musq")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	type point struct {
		at   time.Time
		want string
	}
	var points []point
	for i, q := range []string{
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`,
		`INSERT INTO t(b) VALUES ('one'), ('two')`,
		`UPDATE t SET b = 'TWO' WHERE a = 2`,
		`CREATE INDEX tb ON t(b)`,
		`INSERT INTO t(b) VALUES ('three')`,
		`DELETE FROM t WHERE a = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
		if _, err := run("backup", path, "s3://bkt/app"); err != nil {
			t.Fatal(err)
		}
		points = append(points, point{time.Now(), rows(t, path)})
		time.Sleep(5 * time.Millisecond)
	}
	for _, p := range points {
		dst := filepath.Join(t.TempDir(), "r.musq")
		if _, err := run("restore", "s3://bkt/app", dst, "--timestamp", p.at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if got := rows(t, dst); got != p.want {
			t.Errorf("restore to %v: %q, want %q", p.at, got, p.want)
		}
	}
	dst := filepath.Join(t.TempDir(), "latest.musq")
	if _, err := run("restore", "s3://bkt/app", dst); err != nil {
		t.Fatal(err)
	}
	if got, want := rows(t, dst), points[len(points)-1].want; got != want {
		t.Errorf("latest: %q, want %q", got, want)
	}
	if _, err := run("restore", "s3://bkt/app", dst); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("restore over a file: %v", err)
	}
	out, err := run("restore", "s3://bkt/app", "--list")
	if err != nil {
		t.Fatal(err)
	}
	// The first backup's snapshot, and the CREATE INDEX's.
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("--list: %d snapshots, want 2:\n%s", n, out)
	}

	// Another database is refused the prefix.
	other := filepath.Join(dir, "other.musq")
	odb, err := sql.Open("sqlite", other)
	if err != nil {
		t.Fatal(err)
	}
	defer odb.Close()
	if _, err := odb.Exec(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := run("backup", other, "s3://bkt/app"); err == nil || !strings.Contains(err.Error(), "another database") {
		t.Errorf("another database's prefix: %v", err)
	}
	if _, err := run("restore", "s3://bkt/none", filepath.Join(t.TempDir(), "x.musq")); err == nil || !strings.Contains(err.Error(), "no backup") {
		t.Errorf("an empty prefix: %v", err)
	}
}

// TestBackupStream: a streaming backup copies commits as they land, and the
// last of them once more when it is stopped.
func TestBackupStream(t *testing.T) {
	endpoint, getenv := fakeS3(t)
	path := filepath.Join(t.TempDir(), "app.musq")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(ctx, cli.Env{Dir: t.TempDir(), Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Getenv: getenv},
			[]string{"backup", path, "s3://bkt/app", "--stream", "--interval", "10ms", "--snapshot-every", "50ms", "--endpoint", endpoint})
	}()
	restore := func() string {
		dst := filepath.Join(t.TempDir(), "r.musq")
		if err := cli.Run(context.Background(), cli.Env{Dir: t.TempDir(), Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Getenv: getenv},
			[]string{"restore", "s3://bkt/app", dst, "--endpoint", endpoint}); err != nil {
			return "error: " + err.Error()
		}
		return rows(t, dst)
	}
	want := ""
	for _, b := range []string{"x", "y", "z"} {
		if _, err := db.Exec(`INSERT INTO t(b) VALUES (?)`, b); err != nil {
			t.Fatal(err)
		}
		want += b + ";"
		deadline := time.Now().Add(10 * time.Second)
		for got := restore(); got != want; got = restore() {
			if time.Now().After(deadline) {
				t.Fatalf("the stream did not copy %q: restore gives %q", b, got)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if _, err := db.Exec(`INSERT INTO t(b) VALUES ('last')`); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := restore(); got != want+"last;" {
		t.Errorf("after stopping: %q, want %q", got, want+"last;")
	}
}
