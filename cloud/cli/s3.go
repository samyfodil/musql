package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/samyfodil/musql/cloud/store"
)

// bucket is one prefix of an S3-compatible bucket, holding a copy of a
// database's history (store.Files): its snapshot files by name, and its log
// as chunks named by the log offset they start at, each a run of whole
// records, so that the log is their concatenation in offset order.
type bucket struct {
	c            *minio.Client
	name, prefix string
}

// s3Bucket connects to the bucket an s3://bucket/prefix URL names. The
// endpoint is --endpoint, else $AWS_ENDPOINT_URL, else AWS; credentials are
// $AWS_ACCESS_KEY_ID and $AWS_SECRET_ACCESS_KEY, else the shared credentials
// file, else the instance's role.
func (a *app) s3Bucket(raw, endpoint string) (*bucket, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("%q is not an s3://bucket/prefix URL", raw)
	}
	prefix := strings.Trim(u.Path, "/")
	if prefix == "" {
		return nil, fmt.Errorf("%s names no prefix: give the backup a folder of its own, s3://%s/<name>", raw, u.Host)
	}
	getenv := a.env.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if endpoint == "" {
		endpoint = getenv("AWS_ENDPOINT_URL")
	}
	secure := true
	host := "s3.amazonaws.com"
	if endpoint != "" {
		e, err := url.Parse(endpoint)
		if err != nil || e.Host == "" {
			return nil, fmt.Errorf("endpoint %q is not a URL", endpoint)
		}
		host, secure = e.Host, e.Scheme != "http"
	}
	var creds *credentials.Credentials
	if id := getenv("AWS_ACCESS_KEY_ID"); id != "" {
		creds = credentials.NewStaticV4(id, getenv("AWS_SECRET_ACCESS_KEY"), getenv("AWS_SESSION_TOKEN"))
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.FileAWSCredentials{Profile: getenv("AWS_PROFILE")},
			&credentials.IAM{},
		})
	}
	region := getenv("AWS_REGION")
	if region == "" {
		region = getenv("AWS_DEFAULT_REGION")
	}
	c, err := minio.New(host, &minio.Options{Creds: creds, Secure: secure, Region: region})
	if err != nil {
		return nil, err
	}
	return &bucket{c: c, name: u.Host, prefix: prefix}, nil
}

func (b *bucket) key(name string) string { return b.prefix + "/" + name }

func (b *bucket) String() string { return "s3://" + b.name + "/" + b.prefix }

const logChunks = "log/"

// chunk is one piece of the log in the bucket.
type chunk struct {
	from, n int64
	key     string
}

// list reads what the bucket holds: the snapshot files by name, and the log
// chunks in order, which must join up from offset 0.
func (b *bucket) list(ctx context.Context) (map[string]bool, []chunk, error) {
	files := map[string]bool{}
	var chunks []chunk
	for o := range b.c.ListObjects(ctx, b.name, minio.ListObjectsOptions{Prefix: b.prefix + "/", Recursive: true}) {
		if o.Err != nil {
			return nil, nil, o.Err
		}
		name := strings.TrimPrefix(o.Key, b.prefix+"/")
		if c, ok := strings.CutPrefix(name, logChunks); ok {
			from, err := strconv.ParseInt(c, 10, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %s is not a log chunk", b, o.Key)
			}
			chunks = append(chunks, chunk{from: from, n: o.Size, key: o.Key})
			continue
		}
		files[name] = true
	}
	slices.SortFunc(chunks, func(x, y chunk) int { return int(x.from - y.from) })
	var end int64
	for _, c := range chunks {
		if c.from != end {
			return nil, nil, fmt.Errorf("%s: the log has a gap or an overlap at byte %d (a chunk starts at %d)", b, end, c.from)
		}
		end += c.n
	}
	return files, chunks, nil
}

func (b *bucket) get(ctx context.Context, key string) ([]byte, error) {
	o, err := b.c.GetObject(ctx, b.name, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer o.Close()
	return io.ReadAll(o)
}

// mirror keeps a bucket a copy of one local history.
type mirror struct {
	b     *bucket
	dir   string
	files map[string]bool
	end   int64 // the log bytes the bucket holds
}

// startMirror checks the bucket holds a copy of this history, or nothing: its
// log must be a prefix of the local one. Its first chunk tells another
// database's history (its first record names another base), its last one a
// history that went on differently.
func startMirror(ctx context.Context, b *bucket, h *store.History, dir string) (*mirror, error) {
	files, chunks, err := b.list(ctx)
	if err != nil {
		return nil, err
	}
	m := &mirror{b: b, dir: dir, files: files}
	if len(chunks) == 0 {
		return m, nil
	}
	last := chunks[len(chunks)-1]
	m.end = last.from + last.n
	logEnd := h.Files().LogEnd
	same := func(c chunk) (bool, error) {
		n := min(c.n, logEnd-c.from)
		remote, err := b.get(ctx, c.key)
		if err != nil {
			return false, err
		}
		local, err := readAt(filepath.Join(dir, store.LogName), c.from, n)
		if err != nil {
			return false, err
		}
		return bytes.Equal(remote[:n], local), nil
	}
	if ok, err := same(chunks[0]); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%s holds another database's history: back up to another prefix", b)
	}
	if m.end > logEnd {
		return nil, fmt.Errorf("%s holds a longer history than %s: restore from it, or back up to another prefix", b, dir)
	}
	if ok, err := same(last); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%s holds another database's history: back up to another prefix", b)
	}
	return m, nil
}

// push copies what the history holds that the bucket lacks: snapshot files
// first, so the bucket's log never names one it does not have, then the log
// from where the bucket's ends. It reports the log bytes it copied.
func (m *mirror) push(ctx context.Context, h *store.History) (int64, error) {
	f := h.Files()
	for _, name := range f.Snapshots {
		if m.files[name] {
			continue
		}
		if _, err := m.b.c.FPutObject(ctx, m.b.name, m.b.key(name), filepath.Join(m.dir, name), minio.PutObjectOptions{}); err != nil {
			return 0, err
		}
		m.files[name] = true
	}
	if f.LogEnd <= m.end {
		return 0, nil
	}
	data, err := readAt(filepath.Join(m.dir, store.LogName), m.end, f.LogEnd-m.end)
	if err != nil {
		return 0, err
	}
	key := m.b.key(logChunks + fmt.Sprintf("%020d", m.end))
	if _, err := m.b.c.PutObject(ctx, m.b.name, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{}); err != nil {
		return 0, err
	}
	m.end = f.LogEnd
	return int64(len(data)), nil
}

// fetch copies the bucket's log into dir, opens it, and returns it with the
// bucket's file names; the caller fetches the snapshot files it needs.
func fetch(ctx context.Context, b *bucket, dir string) (*store.History, map[string]bool, error) {
	files, chunks, err := b.list(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(chunks) == 0 {
		return nil, nil, fmt.Errorf("%s holds no backup", b)
	}
	f, err := os.Create(filepath.Join(dir, store.LogName))
	if err != nil {
		return nil, nil, err
	}
	werr := func() error {
		for _, c := range chunks {
			o, err := b.c.GetObject(ctx, b.name, c.key, minio.GetObjectOptions{})
			if err != nil {
				return err
			}
			n, err := io.Copy(f, o)
			o.Close()
			if err != nil {
				return err
			}
			if n != c.n {
				return fmt.Errorf("%s: %s read %d bytes, listed %d", b, path.Base(c.key), n, c.n)
			}
		}
		return nil
	}()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, nil, werr
	}
	h, err := store.OpenLog(dir)
	if err != nil {
		return nil, nil, err
	}
	return h, files, nil
}

func (b *bucket) fetchFile(ctx context.Context, files map[string]bool, name, dir string) error {
	if !files[name] {
		return errors.New(b.String() + " is missing " + name + ", a snapshot its log names")
	}
	return b.c.FGetObject(ctx, b.name, b.key(name), filepath.Join(dir, name), minio.GetObjectOptions{})
}

func readAt(p string, off, n int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}
