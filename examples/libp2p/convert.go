package libp2p

import (
	"fmt"
	"strings"

	"github.com/samyfodil/musql/examples/libp2p/pb"
	"github.com/samyfodil/musql/replication"
)

// A response is read with readOwnedMsg, so its strings and bytes point into its
// buffer. Converting it interns every string kept and caps every byte slice,
// so what outlives it pins the buffer only through bytes it was sent, and an
// append reallocates.

// intern copies each distinct string of one message once.
type intern map[string]string

func (in intern) str(s string) string {
	if v, ok := in[s]; ok {
		return v
	}
	v := strings.Clone(s)
	in[v] = v
	return v
}

func capped(b []byte) []byte { return b[:len(b):len(b)] }

// opsToPB converts ops to their protobuf representation, the ops in one
// allocation and their cells in another rather than one each.
func opsToPB(ops []replication.Op) []*pb.Op {
	n := 0
	for _, op := range ops {
		n += len(op.Cells)
	}
	msgs, out := make([]pb.Op, len(ops)), make([]*pb.Op, len(ops))
	cells, cellPtrs := make([]pb.Cell, n), make([]*pb.Cell, n)
	for i, op := range ops {
		k := len(op.Cells)
		m := &msgs[i]
		m.Site, m.Seq, m.Hlc, m.Tbl, m.Pk, m.Kind = op.Site, op.Seq, uint64(op.HLC), op.Tbl, op.PK, uint32(op.Kind)
		m.Cells = cellPtrs[:k:k]
		for j, c := range op.Cells {
			pc := &cells[j]
			pc.Col, pc.Type, pc.Val = c.Col, uint32(c.Type), c.Val
			m.Cells[j] = pc
		}
		cells, cellPtrs = cells[k:], cellPtrs[k:]
		out[i] = m
	}
	return out
}

// opDecoder converts protobuf ops back, their cells in one allocation.
type opDecoder struct {
	in    intern
	cells []replication.Cell
}

func newOpDecoder(ops []*pb.Op, in intern) *opDecoder {
	n := 0
	for _, op := range ops {
		n += len(op.GetCells())
	}
	return &opDecoder{in: in, cells: make([]replication.Cell, n)}
}

// op converts one of the ops the decoder was made for, refusing one no
// replication node wrote: a kind or a cell type out of range would be read as
// something else.
func (d *opDecoder) op(op *pb.Op) (replication.Op, error) {
	if op.GetSite() == "" || op.GetSeq() == 0 || op.GetKind() > uint32(replication.OpSchema) {
		return replication.Op{}, fmt.Errorf("examples/libp2p: malformed op %s/%d (kind %d)", op.GetSite(), op.GetSeq(), op.GetKind())
	}
	k := len(op.GetCells())
	cells := d.cells[:k:k]
	d.cells = d.cells[k:]
	for i, c := range op.GetCells() {
		if c.GetType() > uint32(replication.TypeBlob) {
			return replication.Op{}, fmt.Errorf("examples/libp2p: op %s/%d: cell %q has type %d", op.GetSite(), op.GetSeq(), c.GetCol(), c.GetType())
		}
		cells[i] = replication.Cell{Col: d.in.str(c.GetCol()), Type: byte(c.GetType()), Val: capped(c.GetVal())}
	}
	return replication.Op{
		Site:  d.in.str(op.GetSite()),
		Seq:   op.GetSeq(),
		HLC:   replication.HLC(op.GetHlc()),
		Tbl:   d.in.str(op.GetTbl()),
		PK:    capped(op.GetPk()),
		Kind:  replication.OpKind(op.GetKind()),
		Cells: cells,
	}, nil
}

// opsFromPB converts protobuf ops back to []replication.Op.
func opsFromPB(ops []*pb.Op, in intern) ([]replication.Op, error) {
	d := newOpDecoder(ops, in)
	out := make([]replication.Op, 0, len(ops))
	for _, op := range ops {
		o, err := d.op(op)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// vvToPB converts a VersionVector into a slice of VVEntry.
func vvToPB(vv replication.VersionVector) []*pb.VVEntry {
	entries, out := make([]pb.VVEntry, len(vv)), make([]*pb.VVEntry, 0, len(vv))
	for site, seq := range vv {
		e := &entries[len(out)]
		e.Site, e.Seq = site, seq
		out = append(out, e)
	}
	return out
}

// vvFromPB converts a slice of VVEntry back into a VersionVector.
func vvFromPB(entries []*pb.VVEntry, in intern) replication.VersionVector {
	vv := make(replication.VersionVector, len(entries))
	for _, e := range entries {
		vv[in.str(e.GetSite())] = e.GetSeq()
	}
	return vv
}

// snapshotToPB converts a snapshot page to its protobuf representation.
func snapshotToPB(p replication.SnapshotPage) *pb.GetSnapshotResponse {
	out := &pb.GetSnapshotResponse{Ops: opsToPB(p.Ops), Floors: vvToPB(p.Floors), Next: p.Next}
	ranges := make([]pb.RangeEntry, len(p.Ranges))
	for site, lo := range p.Ranges {
		r := &ranges[len(out.Ranges)]
		r.Site, r.Lo = site, lo
		out.Ranges = append(out.Ranges, r)
	}
	cells := make([]pb.ClockCell, len(p.Cells))
	out.Cells = make([]*pb.ClockCell, len(p.Cells))
	for i, c := range p.Cells {
		pc := &cells[i]
		pc.Tbl, pc.Pk, pc.Col, pc.Hlc, pc.Site, pc.Val = c.Tbl, c.PK, c.Col, uint64(c.HLC), c.Site, c.Val
		out.Cells[i] = pc
	}
	return out
}

// snapshotFromPB converts a protobuf snapshot page back.
func snapshotFromPB(r *pb.GetSnapshotResponse) (replication.SnapshotPage, error) {
	in := intern{}
	ops, err := opsFromPB(r.GetOps(), in)
	if err != nil {
		return replication.SnapshotPage{}, err
	}
	p := replication.SnapshotPage{Ops: ops, Floors: vvFromPB(r.GetFloors(), in), Ranges: make(map[string]uint64, len(r.GetRanges()))}
	if len(r.GetNext()) > 0 {
		p.Next = capped(r.GetNext())
	}
	for _, e := range r.GetRanges() {
		p.Ranges[in.str(e.GetSite())] = e.GetLo()
	}
	if len(r.GetCells()) > 0 {
		p.Cells = make([]replication.ClockCell, len(r.GetCells()))
	}
	for i, c := range r.GetCells() {
		p.Cells[i] = replication.ClockCell{Tbl: in.str(c.GetTbl()), PK: capped(c.GetPk()), Col: in.str(c.GetCol()), HLC: replication.HLC(c.GetHlc()), Site: in.str(c.GetSite()), Val: capped(c.GetVal())}
	}
	return p, nil
}

// acksToPB converts acks to their protobuf representation.
func acksToPB(acks replication.Acks) []*pb.AckEntry {
	entries, out := make([]pb.AckEntry, len(acks)), make([]*pb.AckEntry, 0, len(acks))
	for site, a := range acks {
		e := &entries[len(out)]
		e.Site, e.Hlc, e.Vv = site, uint64(a.HLC), vvToPB(a.VV)
		out = append(out, e)
	}
	return out
}

// acksFromPB converts protobuf acks back.
func acksFromPB(entries []*pb.AckEntry, in intern) replication.Acks {
	out := make(replication.Acks, len(entries))
	for _, e := range entries {
		out[in.str(e.GetSite())] = replication.Ack{HLC: replication.HLC(e.GetHlc()), VV: vvFromPB(e.GetVv(), in)}
	}
	return out
}

// manifestToPB converts an image manifest to its protobuf representation.
func manifestToPB(m replication.ImageManifest) *pb.ImageManifest {
	out := &pb.ImageManifest{Site: m.Site, FileVersion: m.FileVersion, DeltaVersion: m.DeltaVersion, Layout: m.Layout,
		Catalog: m.Catalog, Size: uint64(m.Size), DeltaSize: uint64(m.DeltaSize), Crc32C: m.Checksum, Vv: vvToPB(m.VV), Floors: vvToPB(m.Floors), MaxHlc: uint64(m.MaxHLC)}
	for site, lo := range m.Ranges {
		out.Ranges = append(out.Ranges, &pb.RangeEntry{Site: site, Lo: lo})
	}
	return out
}

// manifestFromPB converts a protobuf image manifest back.
func manifestFromPB(m *pb.ImageManifest) replication.ImageManifest {
	in := intern{}
	out := replication.ImageManifest{Site: m.GetSite(), FileVersion: m.GetFileVersion(), DeltaVersion: m.GetDeltaVersion(), Layout: m.GetLayout(),
		Catalog: m.GetCatalog(), Size: int64(m.GetSize()), DeltaSize: int64(m.GetDeltaSize()), Checksum: m.GetCrc32C(), VV: vvFromPB(m.GetVv(), in), Floors: vvFromPB(m.GetFloors(), in),
		Ranges: map[string]uint64{}, MaxHLC: replication.HLC(m.GetMaxHlc())}
	for _, e := range m.GetRanges() {
		out.Ranges[in.str(e.GetSite())] = e.GetLo()
	}
	return out
}
