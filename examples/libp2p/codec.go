package libp2p

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protowire"
)

// vtMessage is a message with the methods protoc-gen-go-vtproto generates
// (pb/transport_vtproto.pb.go), which marshal without reflection.
type vtMessage interface {
	SizeVT() int
	MarshalToSizedBufferVT([]byte) (int, error)
	UnmarshalVT([]byte) error
	UnmarshalVTUnsafe([]byte) error
}

// writeMsg writes m framed as protodelim frames a message: its size as a
// varint, then the message, in one write.
func writeMsg(w io.Writer, m vtMessage) error {
	n := m.SizeVT()
	buf := protowire.AppendVarint(make([]byte, 0, protowire.SizeVarint(uint64(n))+n), uint64(n))
	if _, err := m.MarshalToSizedBufferVT(buf[len(buf) : len(buf)+n]); err != nil {
		return err
	}
	_, err := w.Write(buf[:len(buf)+n])
	return err
}

// readMsg reads one message writeMsg framed into m, refusing one over max
// bytes before reading it. A message that fits r's buffer is decoded from it
// in place: UnmarshalVT copies what it keeps.
func readMsg(r *bufio.Reader, m vtMessage, max int) error {
	size, err := readSize(r, max)
	if err != nil {
		return err
	}
	if size <= r.Size() {
		if b, err := r.Peek(size); err == nil {
			err = m.UnmarshalVT(b)
			r.Discard(size)
			return err
		}
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return m.UnmarshalVT(b)
}

// readOwnedMsg is readMsg into a buffer of the message's own that m's
// strings and bytes point into rather than copy. The buffer is never written
// again, but it lives as long as anything taken from m: copy or intern what
// outlives the message.
func readOwnedMsg(r *bufio.Reader, m vtMessage, max int) error {
	size, err := readSize(r, max)
	if err != nil {
		return err
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return m.UnmarshalVTUnsafe(b)
}

func readSize(r *bufio.Reader, max int) (int, error) {
	size, err := binary.ReadUvarint(r)
	if err != nil {
		return 0, err
	}
	if size > uint64(max) {
		return 0, fmt.Errorf("examples/libp2p: a message of %d bytes is over the %d allowed", size, max)
	}
	return int(size), nil
}
