package cli_test

import (
	"cmp"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/cloud/cli"
	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

// bigDB writes a musql file of about mib MiB of random blobs, in a directory
// of its own, and returns its path.
func bigDB(t *testing.T, mib int) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "work.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE blobs(id INTEGER PRIMARY KEY, b BLOB)`); err != nil {
		t.Fatal(err)
	}
	for range mib {
		if _, err := db.Exec(`INSERT INTO blobs(b) VALUES (randomblob(1048576))`); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "big.musq")
	if _, err := db.Exec(`VACUUM INTO '` + out + `'`); err != nil {
		t.Fatal(err)
	}
	return out
}

// content is a database's blobs as one comparable value.
func content(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT b FROM blobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	h := sha256.New()
	n := 0
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		h.Write(b)
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d rows, sha256 %x", n, h.Sum(nil))
}

func fileSum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// signedIn starts a server and signs the CLI in to a new org, the current one.
func signedIn(t *testing.T, extra string) *harness {
	t.Helper()
	h := startWith(t, extra)
	h.login()
	h.run("org", "create", "acme")
	h.run("org", "switch", "acme")
	return h
}

func TestTransfers(t *testing.T) {
	h := signedIn(t, "")
	src := bigDB(t, 9)
	want := content(t, src)

	// Several pieces up, with progress on stderr: a line per tenth passed
	// when it is not a terminal (0%, then after each of the three pieces).
	_, errs, err := h.tryEnv(cli.Env{}, "db", "import", src, "--name", "big")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(errs, "Uploading "+src); n != 4 || !strings.Contains(errs, "Uploading "+src+"    0%") || !strings.Contains(errs, "100%") {
		t.Fatalf("%d progress lines: %q", n, errs)
	}
	h.run("db", "create", "bigfile", "--from-file", src)

	// On a terminal the line is redrawn in place.
	_, errs, err = h.tryEnv(cli.Env{StderrIsTerminal: true}, "db", "create", "bigterm", "--from-file", src)
	if err != nil || !strings.Contains(errs, "\r") || strings.Contains(errs, "\n  ") || !strings.Contains(errs, "100%") {
		t.Fatalf("terminal progress: %q %v", errs, err)
	}

	// Several pieces down, to a file that appears only when it is whole.
	out := filepath.Join(t.TempDir(), "big-out.musq")
	_, errs, err = h.tryEnv(cli.Env{}, "db", "export", "big", "-o", out)
	if err != nil || !strings.Contains(errs, "Downloading "+out) || !strings.Contains(errs, "Wrote "+out) {
		t.Fatalf("export: %q %v", errs, err)
	}
	if got := content(t, out); got != want {
		t.Fatal("the exported rows differ from the uploaded ones")
	}
	if _, err := os.Stat(out + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial file stayed: %v", err)
	}

	// A C SQLite file goes up the same way and is converted by the server.
	lite := filepath.Join(t.TempDir(), "big.sqlite")
	h.run("db", "export", "big", "--sqlite", "-o", lite)
	if st, err := os.Stat(lite); err != nil || st.Size() < 9<<20 {
		t.Fatalf("sqlite export: %v", err)
	}
	h.run("db", "import", lite, "--name", "fromlite")
	back := filepath.Join(t.TempDir(), "back.musq")
	h.run("db", "export", "fromlite", "-o", back)
	if got := content(t, back); got != want {
		t.Fatal("the SQLite round trip changed the rows")
	}

	// A download that fails leaves nothing under the name asked for.
	missing := filepath.Join(t.TempDir(), "missing.musq")
	if _, err := h.try("db", "export", "nosuch", "-o", missing); err == nil {
		t.Fatal("exported a database that does not exist")
	}
	for _, p := range []string{missing, missing + ".part"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

// lossy lets every request through but one: the nth UploadChunk, which never
// reaches the server, or reaches it and loses its answer.
type lossy struct {
	mu     sync.Mutex
	n      int
	lose   int
	arrive bool
	lost   bool
	// call is the RPC whose nth request is lost: UploadChunk when empty.
	call string
}

func (l *lossy) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.URL.Path, "/"+cmp.Or(l.call, "UploadChunk")) {
		return http.DefaultTransport.RoundTrip(r)
	}
	l.mu.Lock()
	l.n++
	drop := l.n == l.lose
	l.lost = l.lost || drop
	l.mu.Unlock()
	if drop && !l.arrive {
		return nil, errors.New("connection reset by peer")
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if drop && err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, errors.New("connection reset by peer")
	}
	return resp, err
}

func TestUploadRetriesFromTheServersOffset(t *testing.T) {
	h := signedIn(t, "")
	src := bigDB(t, 10)
	want := content(t, src)

	// The second piece arrives and its answer is lost: the CLI asks where the
	// upload stands and goes on from there.
	l := &lossy{lose: 2, arrive: true}
	_, errs, err := h.tryEnv(cli.Env{HTTP: &http.Client{Transport: l}}, "db", "import", src, "--name", "retried")
	if err != nil || !l.lost {
		t.Fatalf("%v\n%s", err, errs)
	}
	out := filepath.Join(t.TempDir(), "retried.musq")
	h.run("db", "export", "retried", "-o", out)
	if got := content(t, out); got != want {
		t.Fatal("the retried upload changed the rows")
	}

	// A piece that never arrives is sent again.
	l = &lossy{lose: 1}
	if _, errs, err := h.tryEnv(cli.Env{HTTP: &http.Client{Transport: l}}, "db", "import", src, "--name", "retried2"); err != nil || !l.lost {
		t.Fatalf("%v\n%s", err, errs)
	}
	out = filepath.Join(t.TempDir(), "retried2.musq")
	h.run("db", "export", "retried2", "-o", out)
	if got := content(t, out); got != want {
		t.Fatal("the second retried upload changed the rows")
	}

	// The answer to the finish is lost: finishing is idempotent, so it is asked again.
	l = &lossy{call: "FinishUpload", lose: 1, arrive: true}
	if _, errs, err := h.tryEnv(cli.Env{HTTP: &http.Client{Transport: l}}, "db", "import", src, "--name", "retried3"); err != nil || !l.lost {
		t.Fatalf("%v\n%s", err, errs)
	}
	out = filepath.Join(t.TempDir(), "retried3.musq")
	h.run("db", "export", "retried3", "-o", out)
	if got := content(t, out); got != want {
		t.Fatal("the upload with a repeated finish changed the rows")
	}
}

func TestUploadResume(t *testing.T) {
	h := signedIn(t, "")
	src := bigDB(t, 10)
	want := content(t, src)

	secret := strings.TrimSpace(h.run("auth", "api-tokens", "mint", "resume"))
	dbs := cloudv1connect.NewDatabaseServiceClient(http.DefaultClient, h.url)
	// partial leaves an upload unfinished, with the first 4 MiB of path in it.
	partial := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		begin := connect.NewRequest(&v1.StartUploadRequest{Org: "acme"})
		begin.Header().Set("Authorization", "Bearer "+secret)
		up, err := dbs.StartUpload(t.Context(), begin)
		if err != nil {
			t.Fatal(err)
		}
		piece := connect.NewRequest(&v1.UploadChunkRequest{Org: "acme", Upload: up.Msg.Upload, Content: b[:4<<20]})
		piece.Header().Set("Authorization", "Bearer "+secret)
		if _, err := dbs.UploadChunk(t.Context(), piece); err != nil {
			t.Fatal(err)
		}
		return up.Msg.Upload
	}

	// The CLI goes on from where the server stands, not from the start.
	_, errs, err := h.tryEnv(cli.Env{}, "db", "import", src, "--name", "resumed", "--resume", partial(src))
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	if strings.Contains(errs, "Uploading "+src+"    0%") || !strings.Contains(errs, "100%") {
		t.Fatalf("a resumed upload starts where the server stands: %q", errs)
	}
	out := filepath.Join(t.TempDir(), "resumed.musq")
	h.run("db", "export", "resumed", "-o", out)
	if got := content(t, out); got != want {
		t.Fatal("the resumed upload changed the rows")
	}

	// An upload that is finished holds the whole file: resuming it finishes it again.
	small := bigDB(t, 1)
	sb, err := os.ReadFile(small)
	if err != nil {
		t.Fatal(err)
	}
	begin := connect.NewRequest(&v1.StartUploadRequest{Org: "acme"})
	begin.Header().Set("Authorization", "Bearer "+secret)
	whole, err := dbs.StartUpload(t.Context(), begin)
	if err != nil {
		t.Fatal(err)
	}
	piece := connect.NewRequest(&v1.UploadChunkRequest{Org: "acme", Upload: whole.Msg.Upload, Content: sb})
	piece.Header().Set("Authorization", "Bearer "+secret)
	finish := connect.NewRequest(&v1.FinishUploadRequest{Org: "acme", Upload: whole.Msg.Upload, Sha256: fileSum(sb)})
	finish.Header().Set("Authorization", "Bearer "+secret)
	if _, err := dbs.UploadChunk(t.Context(), piece); err != nil {
		t.Fatal(err)
	}
	if _, err := dbs.FinishUpload(t.Context(), finish); err != nil {
		t.Fatal(err)
	}
	if _, errs, err := h.tryEnv(cli.Env{}, "db", "import", small, "--name", "finished", "--resume", whole.Msg.Upload); err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	out = filepath.Join(t.TempDir(), "finished.musq")
	h.run("db", "export", "finished", "-o", out)
	if got := content(t, out); got != content(t, small) {
		t.Fatal("the upload finished again changed the rows")
	}

	// An upload that is not there is refused; so is one holding another
	// file's start, which the server finds by the hash, and drops.
	if _, err := h.try("db", "import", src, "--name", "nope", "--resume", "doesnotexist"); err == nil || !strings.Contains(err.Error(), "unknown upload") {
		t.Fatalf("resume of an unknown upload: %v", err)
	}
	_, errs, err = h.tryEnv(cli.Env{}, "db", "import", src, "--name", "mixed", "--resume", partial(bigDB(t, 5)))
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("a start of another file: %v", err)
	}
	if strings.Contains(errs, "--resume") {
		t.Fatalf("offered to resume an upload the server dropped: %q", errs)
	}
	if _, err := h.try("db", "import", small, "--name", "shorter", "--resume", partial(src)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("a resume of an upload holding more than the file: %v", err)
	}
	if _, err := h.try("db", "create", "x", "--resume", "abc"); err == nil || !strings.Contains(err.Error(), "--resume needs --from-file") {
		t.Fatalf("resume without a file: %v", err)
	}
}

func TestUploadRefusedBeforeItStarts(t *testing.T) {
	h := signedIn(t, "uploads:\n  max_bytes: 1048576\n")
	_, errs, err := h.tryEnv(cli.Env{}, "db", "import", bigDB(t, 3))
	if err == nil || !strings.Contains(err.Error(), "uploads.max_bytes") {
		t.Fatalf("an upload over the limit: %v", err)
	}
	if strings.Contains(errs, "Uploading") || strings.Contains(errs, "--resume") {
		t.Fatalf("sent bytes, or offered to resume, an upload the server will never take: %q", errs)
	}
	empty := filepath.Join(t.TempDir(), "empty.musq")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.try("db", "import", empty); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("an empty file: %v", err)
	}
	if _, err := h.try("db", "import", filepath.Join(t.TempDir(), "nosuch.musq")); err == nil {
		t.Fatal("imported a file that is not there")
	}
}
