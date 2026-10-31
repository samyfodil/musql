package hrana

import (
	"bytes"
	"io"
	"net/http"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
)

// vtMessage is a message with the methods protoc-gen-go-vtproto generates
// (gen/**/*_vtproto.pb.go), which marshal without reflection.
type vtMessage interface {
	SizeVT() int
	MarshalToSizedBufferVT([]byte) (int, error)
}

// bufPool holds the buffers request bodies are read into and responses are
// marshalled into. A body is done with once it is decoded (UnmarshalVT and
// encoding/json copy what they keep), and a response once it is written.
var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// maxPooledBuf keeps one huge request or response from pinning its buffer.
const maxPooledBuf = 1 << 20

func getBuf() *bytes.Buffer {
	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putBuf(b *bytes.Buffer) {
	if b.Cap() <= maxPooledBuf {
		bufPool.Put(b)
	}
}

// readBody reads r's body into a pooled buffer. The buffer grows only with
// what arrives, never from Content-Length, which the client chooses.
func readBody(r *http.Request) (*bytes.Buffer, error) {
	b := getBuf()
	_, err := b.ReadFrom(r.Body)
	return b, err
}

// marshal marshals m into b's free space, after a size prefix when delimited
// (protodelim's framing, which the cursor endpoint uses).
func marshal(b *bytes.Buffer, m vtMessage, delimited bool) ([]byte, error) {
	n := m.SizeVT()
	b.Reset()
	b.Grow(protowire.SizeVarint(uint64(n)) + n)
	out := b.AvailableBuffer()
	if delimited {
		out = protowire.AppendVarint(out, uint64(n))
	}
	k := len(out)
	if _, err := m.MarshalToSizedBufferVT(out[k : k+n]); err != nil {
		return nil, err
	}
	return out[:k+n], nil
}

// writeMsg writes m, marshalled through a pooled buffer.
func writeMsg(w io.Writer, m vtMessage) error {
	b := getBuf()
	defer putBuf(b)
	out, err := marshal(b, m, false)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}

// delimWriter writes delimited messages, each through the same buffer.
type delimWriter struct {
	w io.Writer
	b *bytes.Buffer
}

func (d delimWriter) write(m vtMessage) error {
	out, err := marshal(d.b, m, true)
	if err != nil {
		return err
	}
	_, err = d.w.Write(out)
	return err
}
