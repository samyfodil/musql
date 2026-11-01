package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// uploadRetries is how many times one piece is sent again after a failure
// that looks like the network's.
const uploadRetries = 5

// progress reports a transfer on stderr: one redrawn line on a terminal, else
// a line at every tenth of the way.
type progress struct {
	a     *app
	label string
	total int64
	step  int64
	shown bool
}

func (a *app) progress(label string, total int64) *progress {
	return &progress{a: a, label: label, total: total, step: -1}
}

func (p *progress) update(done int64) {
	if p.total <= 0 {
		return
	}
	pct := done * 100 / p.total
	line := fmt.Sprintf("%s  %3d%%  %s of %s", p.label, pct, humanBytes(done), humanBytes(p.total))
	if p.a.env.StderrIsTerminal {
		fmt.Fprintf(p.a.env.Stderr, "\r%s\033[K", line)
		p.shown = true
		return
	}
	if tenth := pct / 10; tenth != p.step {
		p.step = tenth
		fmt.Fprintln(p.a.env.Stderr, line)
	}
}

// end finishes a redrawn line.
func (p *progress) end() {
	if p.shown {
		fmt.Fprintln(p.a.env.Stderr)
		p.shown = false
	}
}

// retryable reports whether a failed piece is worth asking the server about
// and sending again: anything but a refusal that sending again cannot change.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodePermissionDenied, connect.CodeUnauthenticated,
		connect.CodeNotFound, connect.CodeResourceExhausted, connect.CodeUnimplemented, connect.CodeCanceled:
		return false
	}
	return true
}

// upload sends the file at path in pieces, with progress on stderr, and returns
// the finished upload's id; resume continues an unfinished one. A piece that
// fails like the network is resent from the server's offset, and the server
// checks the SHA-256 at the end.
func (a *app) upload(ctx context.Context, org, path, resume string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()
	if size == 0 {
		return "", fmt.Errorf("%s is empty", path)
	}
	db := a.services.db
	start, err := db.StartUpload(ctx, connect.NewRequest(&v1.StartUploadRequest{Org: org, SizeBytes: size, Resume: resume}))
	if err != nil {
		return "", err
	}
	id, off := start.Msg.Upload, start.Msg.Offset
	if off > size {
		return "", fmt.Errorf("upload %s holds %s, more than %s is", id, humanBytes(off), path)
	}
	fail := func(err error) (string, error) {
		if ctx.Err() != nil || retryable(ctx, err) {
			fmt.Fprintf(a.env.Stderr, "Upload %s is kept on the server for a while: run the command again with --resume %s\n", id, id)
		}
		return "", err
	}

	h := sha256.New()
	hashed := int64(0)
	// catchUp feeds the hash what the server holds that the loop has not
	// sent: the part of a resumed upload, or a piece whose answer was lost.
	catchUp := func(to int64) error {
		_, err := io.Copy(h, io.NewSectionReader(f, hashed, to-hashed))
		hashed = to
		return err
	}
	if err := catchUp(off); err != nil {
		return fail(err)
	}

	prog := a.progress("Uploading "+path, size)
	defer prog.end()
	prog.update(off)
	buf := make([]byte, max(int(start.Msg.MaxChunkBytes), 1))
	for off < size {
		n, err := f.ReadAt(buf[:min(int64(len(buf)), size-off)], off)
		if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
			return fail(err)
		}
		next, err := a.sendPiece(ctx, org, id, size, off, buf[:n])
		if err != nil {
			return fail(err)
		}
		if next == off+int64(n) {
			h.Write(buf[:n])
			hashed = next
		} else if next < off || next > size {
			return fail(fmt.Errorf("the server holds %s of the upload, which is not where it left off", humanBytes(next)))
		} else if err := catchUp(next); err != nil {
			return fail(err)
		}
		off = next
		prog.update(off)
	}

	// Finishing is idempotent, so an answer that was lost is asked for again.
	for try := 0; ; try++ {
		fin, err := db.FinishUpload(ctx, connect.NewRequest(&v1.FinishUploadRequest{Org: org, Upload: id, Sha256: h.Sum(nil)}))
		if err == nil {
			return fin.Msg.Upload, nil
		}
		if try == uploadRetries || !retryable(ctx, err) {
			return fail(err)
		}
		if err := pause(ctx, try); err != nil {
			return fail(err)
		}
	}
}

// pause waits before try's successor, or until ctx is done.
func pause(ctx context.Context, try int) error {
	select {
	case <-time.After(time.Duration(try+1) * 500 * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendPiece sends piece at off and returns the offset the server holds after
// it. After a failure that may be the network's it asks the server where the
// upload stands: the piece may have arrived with its answer lost.
func (a *app) sendPiece(ctx context.Context, org, id string, size, off int64, piece []byte) (int64, error) {
	db := a.services.db
	for try := 0; ; try++ {
		r, err := db.UploadChunk(ctx, connect.NewRequest(&v1.UploadChunkRequest{Org: org, Upload: id, Offset: off, Content: piece}))
		if err == nil {
			return r.Msg.Offset, nil
		}
		if try == uploadRetries || !retryable(ctx, err) {
			return 0, err
		}
		if err := pause(ctx, try); err != nil {
			return 0, err
		}
		s, err := db.StartUpload(ctx, connect.NewRequest(&v1.StartUploadRequest{Org: org, SizeBytes: size, Resume: id}))
		if err != nil {
			if !retryable(ctx, err) {
				return 0, err
			}
			continue
		}
		if s.Msg.Offset != off {
			return s.Msg.Offset, nil
		}
	}
}

// download streams an export into the file at out: the pieces are written to
// out+".part" as they arrive, and the file is renamed into place when all of
// it has.
func (a *app) download(ctx context.Context, org, name string, format v1.FileFormat, out string) (int64, error) {
	stream, err := a.services.db.Export(ctx, connect.NewRequest(&v1.ExportRequest{Org: org, Name: name, Format: format}))
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	part := out + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	var prog *progress
	var done int64
	for stream.Receive() {
		m := stream.Msg()
		if prog == nil {
			prog = a.progress("Downloading "+out, m.SizeBytes)
			defer prog.end()
		}
		if _, err := f.Write(m.Content); err != nil {
			f.Close()
			os.Remove(part)
			return 0, err
		}
		done += int64(len(m.Content))
		prog.update(done)
	}
	if err := stream.Err(); err != nil {
		f.Close()
		os.Remove(part)
		return 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return 0, err
	}
	if err := os.Rename(part, out); err != nil {
		os.Remove(part)
		return 0, err
	}
	return done, nil
}
