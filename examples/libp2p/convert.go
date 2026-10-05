package libp2p

import (
	"fmt"

	"github.com/samyfodil/musql/examples/libp2p/pb"
	"github.com/samyfodil/musql/replication"
)

// cellToPB converts a replication.Cell to its protobuf representation.
func cellToPB(c replication.Cell) *pb.Cell {
	return &pb.Cell{Col: c.Col, Type: uint32(c.Type), Val: c.Val}
}

// cellFromPB converts a protobuf Cell back to replication.Cell.
func cellFromPB(c *pb.Cell) replication.Cell {
	return replication.Cell{Col: c.GetCol(), Type: byte(c.GetType()), Val: c.GetVal()}
}

// opToPB converts a replication.Op to its protobuf representation.
func opToPB(op replication.Op) *pb.Op {
	cells := make([]*pb.Cell, 0, len(op.Cells))
	for _, c := range op.Cells {
		cells = append(cells, cellToPB(c))
	}
	return &pb.Op{
		Site:  op.Site,
		Seq:   op.Seq,
		Hlc:   uint64(op.HLC),
		Tbl:   op.Tbl,
		Pk:    op.PK,
		Kind:  uint32(op.Kind),
		Cells: cells,
	}
}

// opFromPB converts a protobuf Op back to replication.Op, refusing one no replication node wrote:
// a kind or a cell type out of range would be read as something else.
func opFromPB(op *pb.Op) (replication.Op, error) {
	if op.GetSite() == "" || op.GetSeq() == 0 || op.GetKind() > uint32(replication.OpSchema) {
		return replication.Op{}, fmt.Errorf("examples/libp2p: malformed op %s/%d (kind %d)", op.GetSite(), op.GetSeq(), op.GetKind())
	}
	cells := make([]replication.Cell, 0, len(op.GetCells()))
	for _, c := range op.GetCells() {
		if c.GetType() > uint32(replication.TypeBlob) {
			return replication.Op{}, fmt.Errorf("examples/libp2p: op %s/%d: cell %q has type %d", op.GetSite(), op.GetSeq(), c.GetCol(), c.GetType())
		}
		cells = append(cells, cellFromPB(c))
	}
	return replication.Op{
		Site:  op.GetSite(),
		Seq:   op.GetSeq(),
		HLC:   replication.HLC(op.GetHlc()),
		Tbl:   op.GetTbl(),
		PK:    op.GetPk(),
		Kind:  replication.OpKind(op.GetKind()),
		Cells: cells,
	}, nil
}

// opsToPB converts a slice of replication.Op to their protobuf representation.
func opsToPB(ops []replication.Op) []*pb.Op {
	out := make([]*pb.Op, 0, len(ops))
	for _, op := range ops {
		out = append(out, opToPB(op))
	}
	return out
}

// opsFromPB converts a slice of protobuf Ops back to []replication.Op.
func opsFromPB(ops []*pb.Op) ([]replication.Op, error) {
	out := make([]replication.Op, 0, len(ops))
	for _, op := range ops {
		o, err := opFromPB(op)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// vvToPB converts a VersionVector into a slice of VVEntry.
func vvToPB(vv replication.VersionVector) []*pb.VVEntry {
	out := make([]*pb.VVEntry, 0, len(vv))
	for site, seq := range vv {
		out = append(out, &pb.VVEntry{Site: site, Seq: seq})
	}
	return out
}

// vvFromPB converts a slice of VVEntry back into a VersionVector.
func vvFromPB(entries []*pb.VVEntry) replication.VersionVector {
	vv := make(replication.VersionVector, len(entries))
	for _, e := range entries {
		vv[e.GetSite()] = e.GetSeq()
	}
	return vv
}

// snapshotToPB converts a snapshot page to its protobuf representation.
func snapshotToPB(p replication.SnapshotPage) *pb.GetSnapshotResponse {
	out := &pb.GetSnapshotResponse{Ops: opsToPB(p.Ops), Floors: vvToPB(p.Floors), Next: p.Next}
	for site, lo := range p.Ranges {
		out.Ranges = append(out.Ranges, &pb.RangeEntry{Site: site, Lo: lo})
	}
	for _, c := range p.Cells {
		out.Cells = append(out.Cells, &pb.ClockCell{Tbl: c.Tbl, Pk: c.PK, Col: c.Col, Hlc: uint64(c.HLC), Site: c.Site, Val: c.Val})
	}
	return out
}

// snapshotFromPB converts a protobuf snapshot page back.
func snapshotFromPB(r *pb.GetSnapshotResponse) (replication.SnapshotPage, error) {
	ops, err := opsFromPB(r.GetOps())
	if err != nil {
		return replication.SnapshotPage{}, err
	}
	p := replication.SnapshotPage{Ops: ops, Floors: vvFromPB(r.GetFloors()), Ranges: map[string]uint64{}}
	if len(r.GetNext()) > 0 {
		p.Next = r.GetNext()
	}
	for _, e := range r.GetRanges() {
		p.Ranges[e.GetSite()] = e.GetLo()
	}
	for _, c := range r.GetCells() {
		p.Cells = append(p.Cells, replication.ClockCell{Tbl: c.GetTbl(), PK: c.GetPk(), Col: c.GetCol(), HLC: replication.HLC(c.GetHlc()), Site: c.GetSite(), Val: c.GetVal()})
	}
	return p, nil
}

// acksToPB converts acks to their protobuf representation.
func acksToPB(acks replication.Acks) []*pb.AckEntry {
	out := make([]*pb.AckEntry, 0, len(acks))
	for site, a := range acks {
		out = append(out, &pb.AckEntry{Site: site, Hlc: uint64(a.HLC), Vv: vvToPB(a.VV)})
	}
	return out
}

// acksFromPB converts protobuf acks back.
func acksFromPB(entries []*pb.AckEntry) replication.Acks {
	out := make(replication.Acks, len(entries))
	for _, e := range entries {
		out[e.GetSite()] = replication.Ack{HLC: replication.HLC(e.GetHlc()), VV: vvFromPB(e.GetVv())}
	}
	return out
}
