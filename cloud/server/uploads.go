package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

// An upload is a file in data_dir/uploads named for its org and id: ".part"
// while pieces are still arriving, and without it once FinishUpload has
// checked the whole file. The part's size is the offset the next piece must
// start at, so an upload has no state beyond its file and survives a restart.

const (
	// maxUploadChunk is the largest piece one UploadChunk takes.
	maxUploadChunk = 4 << 20
	// maxRequestBytes bounds a request message to the API. The console speaks
	// JSON, which carries a piece as base64, a third larger; a megabyte covers
	// the message's other fields.
	maxRequestBytes = maxUploadChunk/3*4 + 1<<20
	partSuffix      = ".part"
)

// uploadGate serializes the calls on one upload: a piece, a finish, a create
// from it, and the sweep that removes it. The zero value is ready.
type uploadGate struct {
	mu   sync.Mutex
	busy map[string]chan struct{}
}

// take locks id, or reports the channel that closes when its holder lets go.
func (g *uploadGate) take(id string) (unlock func(), wait <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if held, ok := g.busy[id]; ok {
		return nil, held
	}
	if g.busy == nil {
		g.busy = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	g.busy[id] = done
	return func() {
		g.mu.Lock()
		delete(g.busy, id)
		g.mu.Unlock()
		close(done)
	}, nil
}

func (g *uploadGate) lock(ctx context.Context, id string) (func(), error) {
	for {
		unlock, wait := g.take(id)
		if unlock != nil {
			return unlock, nil
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
	}
}

// tryLock is lock that gives up, returning nil, when id is in use.
func (g *uploadGate) tryLock(id string) func() {
	unlock, _ := g.take(id)
	return unlock
}

func (s *Server) partPath(o *state.Org, id string) (string, error) {
	p, err := s.uploadPath(o, id)
	return p + partSuffix, err
}

func tooLarge(limit int64) error {
	return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("the upload is over the server's limit of %d bytes (uploads.max_bytes)", limit))
}

// diskErr logs a failure of the uploads directory and gives the caller what
// it can act on.
func (s *Server) diskErr(what string, err error) error {
	s.log.Printf("%s: %v", what, err)
	if errors.Is(err, syscall.ENOSPC) {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("the server is out of disk space"))
	}
	return rpcErr(err)
}

func (s databaseService) StartUpload(ctx context.Context, req *connect.Request[v1.StartUploadRequest]) (*connect.Response[v1.StartUploadResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleMember)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	switch {
	case m.SizeBytes < 0:
		return nil, invalid("size_bytes is negative")
	case m.SizeBytes > s.cfg.Uploads.MaxBytes:
		return nil, tooLarge(s.cfg.Uploads.MaxBytes)
	}
	resp := &v1.StartUploadResponse{MaxChunkBytes: maxUploadChunk}
	if m.Resume != "" {
		part, err := s.partPath(o, m.Resume)
		if err != nil {
			return nil, err
		}
		unlock, err := s.uploads.lock(ctx, m.Resume)
		if err != nil {
			return nil, err
		}
		defer unlock()
		// A finished upload holds the whole file: a client whose finish went
		// unanswered learns that, and finishes again.
		path := part
		st, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			path = strings.TrimSuffix(part, partSuffix)
			st, err = os.Stat(path)
		}
		if err != nil {
			return nil, invalid("unknown upload")
		}
		if m.SizeBytes > 0 && st.Size() > m.SizeBytes {
			return nil, invalid("the upload holds more than size_bytes")
		}
		// The client may spend a while on its side before the next piece.
		now := time.Now()
		os.Chtimes(path, now, now)
		resp.Upload, resp.Offset = m.Resume, st.Size()
		return connect.NewResponse(resp), nil
	}
	id := state.NewID()
	part, err := s.partPath(o, id)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, s.diskErr("start upload", err)
	}
	f.Close()
	resp.Upload = id
	return connect.NewResponse(resp), nil
}

func (s databaseService) UploadChunk(ctx context.Context, req *connect.Request[v1.UploadChunkRequest]) (*connect.Response[v1.UploadChunkResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleMember)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	switch {
	case len(m.Content) == 0:
		return nil, invalid("the chunk is empty")
	case len(m.Content) > maxUploadChunk:
		return nil, invalid(fmt.Sprintf("a chunk is at most %d bytes", maxUploadChunk))
	case m.Offset < 0:
		return nil, invalid("offset is negative")
	}
	part, err := s.partPath(o, m.Upload)
	if err != nil {
		return nil, err
	}
	unlock, err := s.uploads.lock(ctx, m.Upload)
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, err := os.OpenFile(part, os.O_WRONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, invalid("unknown upload")
	}
	if err != nil {
		return nil, s.diskErr("upload chunk", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, s.diskErr("upload chunk", err)
	}
	end := m.Offset + int64(len(m.Content))
	switch {
	case m.Offset != st.Size():
		f.Close()
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the upload holds %d bytes, not %d: continue from %d", st.Size(), m.Offset, st.Size()))
	case end > s.cfg.Uploads.MaxBytes:
		f.Close()
		os.Remove(part)
		return nil, tooLarge(s.cfg.Uploads.MaxBytes)
	}
	if _, err = f.WriteAt(m.Content, m.Offset); err != nil {
		// A piece is all there or not there, so the size stays the offset to continue from.
		f.Truncate(m.Offset)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, s.diskErr("upload chunk", err)
	}
	return connect.NewResponse(&v1.UploadChunkResponse{Offset: end}), nil
}

func (s databaseService) FinishUpload(ctx context.Context, req *connect.Request[v1.FinishUploadRequest]) (*connect.Response[v1.FinishUploadResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleMember)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if len(m.Sha256) != sha256.Size {
		return nil, invalid("sha256 must be the 32 bytes of the file's SHA-256")
	}
	part, err := s.partPath(o, m.Upload)
	if err != nil {
		return nil, err
	}
	done, err := s.uploadPath(o, m.Upload)
	if err != nil {
		return nil, err
	}
	unlock, err := s.uploads.lock(ctx, m.Upload)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// A finish whose answer was lost is asked again: a finished upload answers
	// the same as long as the hash is the file's.
	path, finished := part, false
	if _, err := os.Stat(part); errors.Is(err, fs.ErrNotExist) {
		path, finished = done, true
	}
	sum, err := hashFile(ctx, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, invalid("unknown upload")
	}
	if err != nil {
		return nil, s.diskErr("finish upload", err)
	}
	if !bytes.Equal(sum, m.Sha256) {
		if !finished {
			os.Remove(part)
		}
		return nil, invalid("the SHA-256 does not match the upload: it arrived damaged, send the file again")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, s.diskErr("finish upload", err)
	}
	if st.Size() == 0 {
		os.Remove(path)
		return nil, invalid("the upload is empty")
	}
	format, err := fileFormat(path)
	if err != nil {
		return nil, s.diskErr("finish upload", err)
	}
	if !finished {
		// The hash was read from the page cache: have the file on disk before
		// it is named as checked.
		if err := syncFile(part); err != nil {
			return nil, s.diskErr("finish upload", err)
		}
		if err := os.Rename(part, done); err != nil {
			return nil, s.diskErr("finish upload", err)
		}
	}
	return connect.NewResponse(&v1.FinishUploadResponse{Upload: m.Upload, Format: format, SizeBytes: st.Size()}), nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// fileFormat tells a C SQLite file from a musql one by its header.
func fileFormat(path string) (v1.FileFormat, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	head := make([]byte, len(sqliteMagic))
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, err
	}
	if bytes.Equal(head[:n], sqliteMagic) {
		return v1.FileFormat_FILE_FORMAT_SQLITE, nil
	}
	return v1.FileFormat_FILE_FORMAT_MUSQL, nil
}

// hashFile is the SHA-256 of the file at path, read in pieces and abandoned
// when ctx is done.
func hashFile(ctx context.Context, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, ctxReader{ctx, f}, make([]byte, 1<<20)); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// sweepUploads removes the uploads nothing has written to for cfg.Uploads.TTL,
// finished or not, except one in use.
func (s *Server) sweepUploads(now time.Time) error {
	dir := filepath.Join(s.cfg.DataDir, "uploads")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	stale := func(path string) bool {
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular() && now.Sub(st.ModTime()) > s.cfg.Uploads.ttl
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		_, id, ok := strings.Cut(strings.TrimSuffix(e.Name(), partSuffix), "-")
		if !ok || !stale(path) {
			continue
		}
		unlock := s.uploads.tryLock(id)
		if unlock == nil {
			continue
		}
		// The call that held it may have written to it or finished it.
		if stale(path) {
			if err := os.Remove(path); err == nil {
				s.log.Printf("removed upload %s: unused for %s", e.Name(), s.cfg.Uploads.ttl)
			}
		}
		unlock()
	}
	return nil
}

// removeStaleExports removes the export-* directories a stopped server left
// behind. Nothing is exporting yet when the server starts.
func (s *Server) removeStaleExports() {
	dirs, err := filepath.Glob(filepath.Join(s.cfg.DataDir, "export-*"))
	if err != nil {
		return
	}
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			s.log.Printf("remove %s: %v", d, err)
		}
	}
}

// runUploadSweep removes stale uploads every so often until stop is closed.
func (s *Server) runUploadSweep() {
	every := min(s.cfg.Uploads.ttl, 10*time.Minute)
	s.every(every, "uploads", func(context.Context) error { return s.sweepUploads(time.Now()) })
}
