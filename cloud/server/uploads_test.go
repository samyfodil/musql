package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

// export reads a database's whole export from the stream.
func (e *env) export(org, name string, format v1.FileFormat) []byte {
	e.t.Helper()
	var out bytes.Buffer
	e.exportTo(&out, org, name, format)
	return out.Bytes()
}

// exportTo streams an export into w and returns how many messages it took.
func (e *env) exportTo(w io.Writer, org, name string, format v1.FileFormat) (messages int, size int64) {
	e.t.Helper()
	_, _, dbs := e.clients()
	stream := must(dbs.Export(context.Background(), auth(e.admin, &v1.ExportRequest{Org: org, Name: name, Format: format})))
	defer stream.Close()
	for stream.Receive() {
		m := stream.Msg()
		if messages == 0 {
			size = m.SizeBytes
		} else if m.SizeBytes != 0 {
			e.t.Fatalf("size_bytes on message %d", messages)
		}
		messages++
		if _, err := w.Write(m.Content); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := stream.Err(); err != nil {
		e.t.Fatal(err)
	}
	return messages, size
}

// upload sends content through the upload calls and returns the finished upload.
func (e *env) upload(org string, content []byte) *v1.FinishUploadResponse {
	e.t.Helper()
	return e.uploadFrom(org, bytes.NewReader(content), maxUploadChunk)
}

// uploadFrom sends r in pieces of piece bytes.
func (e *env) uploadFrom(org string, r io.Reader, piece int) *v1.FinishUploadResponse {
	e.t.Helper()
	ctx := context.Background()
	_, _, dbs := e.clients()
	up := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: org}))).Msg
	if up.Offset != 0 || up.MaxChunkBytes != maxUploadChunk {
		e.t.Fatalf("start: %+v", up)
	}
	h := sha256.New()
	buf := make([]byte, piece)
	var off int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			h.Write(buf[:n])
			got := must(dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: org, Upload: up.Upload, Offset: off, Content: buf[:n]}))).Msg
			off += int64(n)
			if got.Offset != off {
				e.t.Fatalf("after %d bytes the server holds %d", off, got.Offset)
			}
		}
		if err != nil {
			break
		}
	}
	return must(dbs.FinishUpload(ctx, auth(e.admin, &v1.FinishUploadRequest{Org: org, Upload: up.Upload, Sha256: h.Sum(nil)}))).Msg
}

func (e *env) uploadFiles() []string {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.s.cfg.DataDir, "uploads"))
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, f := range entries {
		names = append(names, f.Name())
	}
	return names
}

func sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func TestUploadsConfig(t *testing.T) {
	c := must(ParseConfig([]byte("listen: \":8080\"\n")))
	if c.Uploads.MaxBytes != 16<<30 || c.Uploads.ttl != 24*time.Hour {
		t.Fatalf("defaults: %+v", c.Uploads)
	}
	c = must(ParseConfig([]byte("uploads:\n  max_bytes: 1048576\n  ttl: 2d\n")))
	if c.Uploads.MaxBytes != 1<<20 || c.Uploads.ttl != 48*time.Hour {
		t.Fatalf("set: %+v", c.Uploads)
	}
	for _, bad := range []string{"max_bytes: -1", "ttl: 0", "ttl: soon", "ttl: -1h", "max_bytes: lots"} {
		if _, err := ParseConfig([]byte("uploads:\n  " + bad + "\n")); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// The example config says what the defaults are.
	example, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c := must(ParseConfig(example)); c.Uploads.MaxBytes != 16<<30 || c.Uploads.ttl != 24*time.Hour {
		t.Fatalf("example: %+v", c.Uploads)
	}
}

func TestUploadRejects(t *testing.T) {
	e := newEnv(t, "uploads:\n  max_bytes: 1048576\n")
	bob := e.setup()
	ctx := context.Background()
	_, orgs, dbs := e.clients()
	start := func(m *v1.StartUploadRequest) (*connect.Response[v1.StartUploadResponse], error) {
		return dbs.StartUpload(ctx, auth(e.admin, m))
	}
	chunk := func(id string, off int64, n int) (*connect.Response[v1.UploadChunkResponse], error) {
		return dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Offset: off, Content: bytes.Repeat([]byte{'x'}, n)}))
	}
	finish := func(id string, sha []byte) (*connect.Response[v1.FinishUploadResponse], error) {
		return dbs.FinishUpload(ctx, auth(e.admin, &v1.FinishUploadRequest{Org: "acme", Upload: id, Sha256: sha}))
	}

	// Who may upload.
	if _, err := dbs.StartUpload(ctx, auth(bob, &v1.StartUploadRequest{Org: "acme"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer: %v", err)
	}
	if _, err := dbs.StartUpload(ctx, auth("", &v1.StartUploadRequest{Org: "acme"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("no token: %v", err)
	}

	// A file over the limit is refused before a byte is sent.
	if _, err := start(&v1.StartUploadRequest{Org: "acme", SizeBytes: 1<<20 + 1}); code(err) != connect.CodeResourceExhausted {
		t.Fatalf("declared size over the limit: %v", err)
	}
	if _, err := start(&v1.StartUploadRequest{Org: "acme", SizeBytes: -1}); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("negative size: %v", err)
	}

	// An offset that is not where the upload stands is refused, and nothing is written.
	id := must(start(&v1.StartUploadRequest{Org: "acme"})).Msg.Upload
	if got := must(chunk(id, 0, 100)).Msg.Offset; got != 100 {
		t.Fatalf("first chunk: %d", got)
	}
	for _, off := range []int64{0, 50, 99, 101, 1 << 30} {
		if _, err := chunk(id, off, 10); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "continue from 100") {
			t.Fatalf("offset %d: %v", off, err)
		}
	}
	if _, err := chunk(id, -1, 10); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("negative offset: %v", err)
	}
	if got := must(start(&v1.StartUploadRequest{Org: "acme", Resume: id})).Msg.Offset; got != 100 {
		t.Fatalf("after refusals the upload holds %d", got)
	}

	// A chunk is not empty, and is over the chunk limit before it is over max_bytes.
	if _, err := chunk(id, 100, 0); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty chunk: %v", err)
	}
	if _, err := chunk(id, 100, maxUploadChunk+1); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("chunk over the chunk limit: %v", err)
	}

	// Growing past max_bytes is refused and the upload is dropped.
	if _, err := chunk(id, 100, 1<<20); code(err) != connect.CodeResourceExhausted {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := chunk(id, 100, 10); code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "unknown upload") {
		t.Fatalf("after the limit: %v", err)
	}
	if left := e.uploadFiles(); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}

	// A hash that is not the file's is refused, and the upload is dropped.
	id = must(start(&v1.StartUploadRequest{Org: "acme"})).Msg.Upload
	must(chunk(id, 0, 1000))
	if _, err := finish(id, sum([]byte("something else"))); code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("sha mismatch: %v", err)
	}
	if _, err := finish(id, sum(bytes.Repeat([]byte{'x'}, 1000))); code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "unknown upload") {
		t.Fatalf("finish after a mismatch: %v", err)
	}
	if left := e.uploadFiles(); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}

	// A hash that is not a SHA-256, an empty upload, an upload nobody started.
	id = must(start(&v1.StartUploadRequest{Org: "acme"})).Msg.Upload
	if _, err := finish(id, []byte("short")); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("short hash: %v", err)
	}
	if _, err := finish(id, sum(nil)); code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty upload: %v", err)
	}
	for _, bad := range []string{"nosuchupload", "../../etc/passwd", "a.part", ""} {
		if _, err := chunk(bad, 0, 10); code(err) != connect.CodeInvalidArgument {
			t.Fatalf("upload %q: %v", bad, err)
		}
		if _, err := finish(bad, sum(nil)); code(err) != connect.CodeInvalidArgument {
			t.Fatalf("finish %q: %v", bad, err)
		}
		if _, err := start(&v1.StartUploadRequest{Org: "acme", Resume: bad}); bad != "" && code(err) != connect.CodeInvalidArgument {
			t.Fatalf("resume %q: %v", bad, err)
		}
	}

	// An upload belongs to its org.
	must(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "other"})))
	id = must(start(&v1.StartUploadRequest{Org: "acme"})).Msg.Upload
	if _, err := dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "other", Upload: id, Content: []byte("x")})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("another org's chunk: %v", err)
	}
	if _, err := dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "other", Resume: id})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("another org's resume: %v", err)
	}
}

func TestUploadResumeAndFinishTwice(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	content := bytes.Repeat([]byte("0123456789"), 1000)
	id := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme", SizeBytes: int64(len(content))}))).Msg.Upload
	must(dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Content: content[:4000]})))

	// The same piece sent again after its answer was lost: the server says where it stands.
	_, err := dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Content: content[:4000]}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("a piece sent twice: %v", err)
	}
	if _, err := dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme", Resume: id, SizeBytes: 100})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("resume with a smaller size: %v", err)
	}
	r := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme", Resume: id, SizeBytes: int64(len(content))}))).Msg
	if r.Upload != id || r.Offset != 4000 {
		t.Fatalf("resume: %+v", r)
	}
	must(dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Offset: r.Offset, Content: content[r.Offset:]})))

	fin := &v1.FinishUploadRequest{Org: "acme", Upload: id, Sha256: sum(content)}
	first := must(dbs.FinishUpload(ctx, auth(e.admin, fin))).Msg
	again := must(dbs.FinishUpload(ctx, auth(e.admin, fin))).Msg
	if first.Upload != id || first.SizeBytes != int64(len(content)) || first.Format != v1.FileFormat_FILE_FORMAT_MUSQL || again.Upload != first.Upload || again.SizeBytes != first.SizeBytes {
		t.Fatalf("finish: %+v, again %+v", first, again)
	}
	if _, err := dbs.FinishUpload(ctx, auth(e.admin, &v1.FinishUploadRequest{Org: "acme", Upload: id, Sha256: sum(nil)})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a finished upload with another hash: %v", err)
	}
	if got := e.uploadFiles(); len(got) != 1 || strings.HasSuffix(got[0], partSuffix) {
		t.Fatalf("files: %v", got)
	}
	// A finished upload takes no more pieces; resuming it says it holds the whole file.
	if _, err := dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Offset: int64(len(content)), Content: []byte("x")})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a piece after the finish: %v", err)
	}
	if r := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme", Resume: id}))).Msg; r.Upload != id || r.Offset != int64(len(content)) {
		t.Fatalf("resume after the finish: %+v", r)
	}

	// A C SQLite header is told apart.
	lite := append([]byte("SQLite format 3\x00"), make([]byte, 100)...)
	if f := e.upload("acme", lite).Format; f != v1.FileFormat_FILE_FORMAT_SQLITE {
		t.Fatalf("format %v", f)
	}
}

func TestUploadPiecesAreSerialized(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	id := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme"}))).Msg.Upload
	piece := bytes.Repeat([]byte{'a'}, 1<<20)
	results := make([]error, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			_, results[i] = dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Offset: 0, Content: piece}))
		})
	}
	wg.Wait()
	took := 0
	for _, err := range results {
		switch {
		case err == nil:
			took++
		case code(err) != connect.CodeFailedPrecondition:
			t.Fatalf("a piece sent at once with the others: %v", err)
		}
	}
	if took != 1 {
		t.Fatalf("%d of the same piece were taken", took)
	}
	if r := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme", Resume: id}))).Msg; r.Offset != int64(len(piece)) {
		t.Fatalf("the upload holds %d bytes", r.Offset)
	}
}

func TestUploadSurvivesRestart(t *testing.T) {
	cfg := must(ParseConfig([]byte("data_dir: " + t.TempDir() + "\n")))
	ctx := context.Background()
	if err := Init(ctx, cfg, "admin@example.com", "admin-password"); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("abcdefgh"), 1000)

	s1 := must(New(ctx, cfg, nil))
	tok := (&env{t: t, s: s1}).tokenFor("admin@example.com")
	must(orgService{s1}.Create(ctx, auth(tok, &v1.CreateOrgRequest{Slug: "acme"})))
	id := must(databaseService{Server: s1}.StartUpload(ctx, auth(tok, &v1.StartUploadRequest{Org: "acme"}))).Msg.Upload
	must(databaseService{Server: s1}.UploadChunk(ctx, auth(tok, &v1.UploadChunkRequest{Org: "acme", Upload: id, Content: content[:3000]})))
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := must(New(ctx, cfg, nil))
	defer s2.Close()
	tok = (&env{t: t, s: s2}).tokenFor("admin@example.com")
	d2 := databaseService{Server: s2}
	r := must(d2.StartUpload(ctx, auth(tok, &v1.StartUploadRequest{Org: "acme", Resume: id}))).Msg
	if r.Offset != 3000 {
		t.Fatalf("after a restart the upload holds %d", r.Offset)
	}
	must(d2.UploadChunk(ctx, auth(tok, &v1.UploadChunkRequest{Org: "acme", Upload: id, Offset: r.Offset, Content: content[r.Offset:]})))
	fin := must(d2.FinishUpload(ctx, auth(tok, &v1.FinishUploadRequest{Org: "acme", Upload: id, Sha256: sum(content)}))).Msg
	if fin.SizeBytes != int64(len(content)) {
		t.Fatalf("finish: %+v", fin)
	}
}

func TestUploadChunkLimit(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	id := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme"}))).Msg.Upload
	put := func(off int64, n int) error {
		_, err := dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Offset: off, Content: make([]byte, n)}))
		return err
	}
	if err := put(0, maxUploadChunk); err != nil {
		t.Fatalf("a chunk of the limit: %v", err)
	}
	if err := put(maxUploadChunk, maxUploadChunk+1); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a chunk over the limit: %v", err)
	}
	// A message past what the API reads at all is refused before it is held.
	if err := put(maxUploadChunk, maxRequestBytes+1); code(err) != connect.CodeResourceExhausted {
		t.Fatalf("a message over the read limit: %v", err)
	}

	// The console speaks JSON, where a full chunk is base64: a third larger.
	js := cloudv1connect.NewDatabaseServiceClient(e.ts.Client(), e.ts.URL, connect.WithProtoJSON())
	chunk := auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Offset: maxUploadChunk, Content: make([]byte, maxUploadChunk)})
	if r, err := js.UploadChunk(ctx, chunk); err != nil || r.Msg.Offset != 2*maxUploadChunk {
		t.Fatalf("a full chunk as JSON: %v", err)
	}
}

func TestUploadSweep(t *testing.T) {
	e := newEnv(t, "uploads:\n  ttl: 1h\n")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	startWith := func(n int) string {
		id := must(dbs.StartUpload(ctx, auth(e.admin, &v1.StartUploadRequest{Org: "acme"}))).Msg.Upload
		if n > 0 {
			must(dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: id, Content: make([]byte, n)})))
		}
		return id
	}
	age := func(id string) {
		old := time.Now().Add(-2 * time.Hour)
		for _, name := range e.uploadFiles() {
			if strings.Contains(name, id) {
				if err := os.Chtimes(filepath.Join(e.s.cfg.DataDir, "uploads", name), old, old); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	has := func(id string) bool {
		for _, name := range e.uploadFiles() {
			if strings.Contains(name, id) {
				return true
			}
		}
		return false
	}

	abandoned, fresh, busy := startWith(10), startWith(10), startWith(10)
	finished := e.upload("acme", []byte("not used")).Upload
	age(abandoned)
	age(finished)
	age(busy)
	unlock := e.s.uploads.tryLock(busy)
	if unlock == nil {
		t.Fatal("the upload was locked")
	}
	if err := e.s.sweepUploads(time.Now()); err != nil {
		t.Fatal(err)
	}
	if has(abandoned) || has(finished) {
		t.Fatalf("stale uploads kept: %v", e.uploadFiles())
	}
	if !has(fresh) || !has(busy) {
		t.Fatalf("an upload in use or fresh was removed: %v", e.uploadFiles())
	}
	unlock()
	if err := e.s.sweepUploads(time.Now()); err != nil {
		t.Fatal(err)
	}
	if has(busy) || !has(fresh) {
		t.Fatalf("after the call let go: %v", e.uploadFiles())
	}
	// A write keeps an upload alive.
	age(fresh)
	must(dbs.UploadChunk(ctx, auth(e.admin, &v1.UploadChunkRequest{Org: "acme", Upload: fresh, Offset: 10, Content: []byte("more")})))
	if err := e.s.sweepUploads(time.Now()); err != nil {
		t.Fatal(err)
	}
	if !has(fresh) {
		t.Fatal("an upload written to was removed")
	}
}

// heapGrowth runs f and returns how far the heap rose above where it began,
// sampled while f runs, with the collector kept tight so garbage does not count.
func heapGrowth(f func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	base := m.HeapAlloc
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	f()
	close(stop)
	<-done
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}

// bigDatabase makes a single-file database of about mib MiB of random blobs.
func bigDatabase(t *testing.T, mib int) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "src.musq"))
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

// blobSums is what the table holds, as one comparable string.
func blobSums(t *testing.T, path string) string {
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
	var n, total int
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		h.Write(b)
		n++
		total += len(b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d rows, %d bytes, sha256 %x", n, total, h.Sum(nil))
}

func TestLargeUploadAndExport(t *testing.T) {
	const mib = 64
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	src := bigDatabase(t, mib)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var up *v1.FinishUploadResponse
	grew := heapGrowth(func() { up = e.uploadFrom("acme", f, 1<<20) })
	t.Logf("uploading %d MiB grew the heap by %d MiB", st.Size()>>20, grew>>20)
	if up.SizeBytes != st.Size() || up.Format != v1.FileFormat_FILE_FORMAT_MUSQL {
		t.Fatalf("finish: %+v for %d bytes", up, st.Size())
	}
	if limit := uint64(st.Size()) / 3; grew > limit {
		t.Fatalf("the heap grew by %d MiB uploading %d MiB (limit %d MiB)", grew>>20, st.Size()>>20, limit>>20)
	}

	grew = heapGrowth(func() {
		must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "big", Source: &v1.CreateDatabaseRequest_Upload{Upload: up.Upload}})))
	})
	t.Logf("creating from it grew the heap by %d MiB", grew>>20)
	if limit := uint64(st.Size()) / 3; grew > limit {
		t.Fatalf("the heap grew by %d MiB creating from %d MiB (limit %d MiB)", grew>>20, st.Size()>>20, limit>>20)
	}
	if left := e.uploadFiles(); len(left) != 0 {
		t.Fatalf("the upload was not consumed: %v", left)
	}

	out := filepath.Join(t.TempDir(), "out.musq")
	w, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	var msgs int
	var size int64
	grew = heapGrowth(func() { msgs, size = e.exportTo(w, "acme", "big", v1.FileFormat_FILE_FORMAT_MUSQL) })
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("exporting grew the heap by %d MiB in %d messages", grew>>20, msgs)
	got, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if msgs < mib || size != got.Size() {
		t.Fatalf("%d messages, first says %d bytes, %d arrived", msgs, size, got.Size())
	}
	if limit := uint64(got.Size()) / 2; grew > limit {
		t.Fatalf("the heap grew by %d MiB exporting %d MiB (limit %d MiB)", grew>>20, got.Size()>>20, limit>>20)
	}
	if blobSums(t, out) != blobSums(t, src) {
		t.Fatal("the exported rows differ from the uploaded ones")
	}
	if entries, _ := filepath.Glob(filepath.Join(e.s.cfg.DataDir, "export-*")); len(entries) != 0 {
		t.Fatalf("export left %v", entries)
	}
}

func TestExportStreams(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	full := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	e.exec("/db/acme/app", full, "CREATE TABLE t(a BLOB)", "INSERT INTO t VALUES (randomblob(2500000))")

	for _, f := range []v1.FileFormat{v1.FileFormat_FILE_FORMAT_UNSPECIFIED, v1.FileFormat_FILE_FORMAT_MUSQL, v1.FileFormat_FILE_FORMAT_SQLITE} {
		var out bytes.Buffer
		msgs, size := e.exportTo(&out, "acme", "app", f)
		if msgs < 3 || size != int64(out.Len()) || out.Len() < 2500000 {
			t.Fatalf("%v: %d messages, first says %d, %d bytes arrived", f, msgs, size, out.Len())
		}
		if lite := bytes.HasPrefix(out.Bytes(), sqliteMagic); lite != (f == v1.FileFormat_FILE_FORMAT_SQLITE) {
			t.Fatalf("%v: SQLite header %v", f, lite)
		}
	}

	stream := must(dbs.Export(ctx, auth(e.admin, &v1.ExportRequest{Org: "acme", Name: "app", Format: 99})))
	for stream.Receive() {
	}
	if code(stream.Err()) != connect.CodeInvalidArgument {
		t.Fatalf("unknown format: %v", stream.Err())
	}
	stream.Close()
	stream = must(dbs.Export(ctx, auth(bob, &v1.ExportRequest{Org: "acme", Name: "app"})))
	for stream.Receive() {
	}
	if code(stream.Err()) != connect.CodePermissionDenied {
		t.Fatalf("viewer: %v", stream.Err())
	}
	stream.Close()
	stream = must(dbs.Export(ctx, auth(e.admin, &v1.ExportRequest{Org: "acme", Name: "nosuch"})))
	for stream.Receive() {
	}
	if code(stream.Err()) != connect.CodeNotFound {
		t.Fatalf("missing database: %v", stream.Err())
	}
	stream.Close()
	if entries, _ := filepath.Glob(filepath.Join(e.s.cfg.DataDir, "export-*")); len(entries) != 0 {
		t.Fatalf("export left %v", entries)
	}
}

func TestStaleExportsRemovedAtStart(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "export-123456")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, "db.musq"), []byte("half an export"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := must(ParseConfig([]byte("data_dir: " + dir + "\n")))
	ctx := context.Background()
	if err := Init(ctx, cfg, "admin@example.com", "admin-password"); err != nil {
		t.Fatal(err)
	}
	must(New(ctx, cfg, nil)).Close()
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("the export of a stopped server is still there: %v", err)
	}
}
