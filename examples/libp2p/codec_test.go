package libp2p

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	"github.com/samyfodil/musql/examples/libp2p/pb"
	"github.com/samyfodil/musql/replication"
)

func sameOps(t *testing.T, got, want []replication.Op) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d ops, want %d", len(got), len(want))
	}
	for i, g := range got {
		w := want[i]
		if g.Site != w.Site || g.Seq != w.Seq || g.HLC != w.HLC || g.Tbl != w.Tbl || !bytes.Equal(g.PK, w.PK) || g.Kind != w.Kind || len(g.Cells) != len(w.Cells) {
			t.Fatalf("op %d: %+v, want %+v", i, g, w)
		}
		for j, c := range g.Cells {
			if c.Col != w.Cells[j].Col || c.Type != w.Cells[j].Type || !bytes.Equal(c.Val, w.Cells[j].Val) {
				t.Fatalf("op %d cell %d: %+v, want %+v", i, j, c, w.Cells[j])
			}
		}
	}
}

// TestFramingIsProtodelim: a message is framed as protodelim frames it, so a
// peer that reads and writes with protodelim and proto interoperates.
func TestFramingIsProtodelim(t *testing.T) {
	ops := codecOps(40)
	ops[3].Cells, ops[3].Kind = nil, replication.OpDelete
	msg := getOpsPage(ops)

	var vt, old bytes.Buffer
	if err := writeMsg(&vt, msg); err != nil {
		t.Fatal(err)
	}
	if _, err := protodelim.MarshalTo(&old, msg); err != nil {
		t.Fatal(err)
	}
	var fromVT pb.Response
	if err := protodelim.UnmarshalFrom(bufio.NewReader(&vt), &fromVT); err != nil || !proto.Equal(&fromVT, msg) {
		t.Fatalf("protodelim reading writeMsg: %v", err)
	}
	for name, read := range map[string]func(*bufio.Reader, vtMessage, int) error{"readMsg": readMsg, "readOwnedMsg": readOwnedMsg} {
		for _, size := range []int{16, 4096, 1 << 20} {
			var got pb.Response
			if err := read(bufio.NewReaderSize(bytes.NewReader(old.Bytes()), size), &got, maxResponse); err != nil || !proto.Equal(&got, msg) {
				t.Fatalf("%s reading protodelim, buffer %d: %v", name, size, err)
			}
		}
	}
}

// TestReadRefusesOversizeAndTruncated: a message over the limit is refused
// before it is read, a cut one is an error, and a stream that ends between
// messages is io.EOF.
func TestReadRefusesOversizeAndTruncated(t *testing.T) {
	msg := getOpsPage(codecOps(3))
	var buf bytes.Buffer
	if err := writeMsg(&buf, msg); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(*bufio.Reader, vtMessage, int) error{"readMsg": readMsg, "readOwnedMsg": readOwnedMsg} {
		var m pb.Response
		if err := read(bufio.NewReader(bytes.NewReader(buf.Bytes())), &m, msg.SizeVT()-1); err == nil {
			t.Errorf("%s: an oversize message was read", name)
		}
		if err := read(bufio.NewReader(bytes.NewReader(buf.Bytes()[:buf.Len()-1])), &m, maxResponse); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: a truncated message: %v", name, err)
		}
		if err := read(bufio.NewReader(bytes.NewReader(nil)), &m, maxResponse); err != io.EOF {
			t.Errorf("%s: an empty stream: %v", name, err)
		}
	}
}

// TestDecodedOpsOwnTheirBytes: ops read with readOwnedMsg come back as they
// were sent, and an append to one's bytes or cells leaves the ops beside it,
// which share its buffer, as they were.
func TestDecodedOpsOwnTheirBytes(t *testing.T) {
	ops := codecOps(20)
	ops[5].Cells = nil
	var buf bytes.Buffer
	if err := writeMsg(&buf, getOpsPage(ops)); err != nil {
		t.Fatal(err)
	}
	var resp pb.Response
	if err := readOwnedMsg(bufio.NewReader(&buf), &resp, maxResponse); err != nil {
		t.Fatal(err)
	}
	got, err := opsFromPB(resp.GetGetOps().GetOps(), intern{})
	if err != nil {
		t.Fatal(err)
	}
	sameOps(t, got, ops)
	for i := range got {
		got[i].PK = append(got[i].PK, 0xee)
		for j := range got[i].Cells {
			got[i].Cells[j].Val = append(got[i].Cells[j].Val, 0xee)
		}
		got[i].Cells = append(got[i].Cells, replication.Cell{Col: "extra"})
		sameOps(t, got[i+1:], ops[i+1:])
	}
}
