package libp2p

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/samyfodil/musql/examples/libp2p/pb"
	"github.com/samyfodil/musql/replication"
)

// codecOps is n insert ops of four cells each, from one site, the shape a
// GetOps page or a commit's push carries.
func codecOps(n int) []replication.Op {
	const site = "12D3KooWQYhTNQdmr3ArTeUHRYzFg94BKyTkoWBDWez9kSCVe2Xo"
	ops := make([]replication.Op, n)
	for i := range ops {
		ops[i] = replication.Op{Site: site, Seq: uint64(i + 1), HLC: replication.HLC(1<<40 + i), Tbl: "items",
			PK: binary.BigEndian.AppendUint64([]byte{1}, uint64(i)), Kind: replication.OpInsert,
			Cells: []replication.Cell{
				{Col: "id", Type: replication.TypeInt, Val: binary.BigEndian.AppendUint64(nil, uint64(i))},
				{Col: "name", Type: replication.TypeText, Val: fmt.Appendf(nil, "item name %08d", i)},
				{Col: "price", Type: replication.TypeFloat, Val: binary.BigEndian.AppendUint64(nil, uint64(i)<<3)},
				{Col: "data", Type: replication.TypeBlob, Val: bytes.Repeat([]byte{byte(i)}, 64)},
			}}
	}
	return ops
}

// getOpsPage builds the response handleStream answers a GetOps with.
func getOpsPage(ops []replication.Op) *pb.Response {
	out := &pb.GetOpsResponse{}
	size := 0
	for _, op := range opsToPB(ops) {
		n := op.SizeVT() + binary.MaxVarintLen64 + 1
		if size+n > maxResponse-binary.MaxVarintLen64 {
			break
		}
		size += n
		out.Ops = append(out.Ops, op)
	}
	return &pb.Response{Msg: &pb.Response_GetOps{GetOps: out}}
}

func BenchmarkCodecGetOps(b *testing.B) {
	ops := codecOps(maxOpsPerResponse)
	var page bytes.Buffer
	if err := writeMsg(&page, getOpsPage(ops)); err != nil {
		b.Fatal(err)
	}
	b.Run("serve", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := writeMsg(io.Discard, getOpsPage(ops)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("read", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var resp pb.Response
			if err := readOwnedMsg(bufio.NewReader(bytes.NewReader(page.Bytes())), &resp, maxResponse); err != nil {
				b.Fatal(err)
			}
			got, err := opsFromPB(resp.GetGetOps().GetOps(), intern{})
			if err != nil || len(got) != len(ops) {
				b.Fatal(len(got), err)
			}
		}
		b.ReportMetric(float64(page.Len()), "page-bytes")
	})
}

func BenchmarkCodecPush(b *testing.B) {
	for _, n := range []int{1, 100} {
		ops := codecOps(n)
		var msgs [][]byte
		encode := func() {
			msgs = msgs[:0]
			msg, size := &pb.PushMessage{}, 0
			flush := func() {
				if len(msg.Ops) == 1 {
					msg.Op, msg.Ops = msg.Ops[0], nil
				}
				data, err := msg.MarshalVT()
				if err != nil {
					b.Fatal(err)
				}
				msgs = append(msgs, data)
				msg, size = &pb.PushMessage{}, 0
			}
			for _, op := range opsToPB(ops) {
				n := op.SizeVT()
				if len(msg.Ops) > 0 && size+n > maxPushBytes {
					flush()
				}
				msg.Ops = append(msg.Ops, op)
				size += n
			}
			if len(msg.Ops) > 0 {
				flush()
			}
		}
		b.Run(fmt.Sprintf("encode/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				encode()
			}
		})
		encode()
		b.Run(fmt.Sprintf("decode/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, data := range msgs {
					var pm pb.PushMessage
					if err := pm.UnmarshalVTUnsafe(data); err != nil {
						b.Fatal(err)
					}
					pops := pm.GetOps()
					if pm.GetOp() != nil {
						pops = append([]*pb.Op{pm.GetOp()}, pops...)
					}
					d := newOpDecoder(pops, intern{})
					for _, o := range pops {
						if _, err := d.op(o); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
		})
	}
}
