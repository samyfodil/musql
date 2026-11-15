package rpc

import (
	"fmt"

	"github.com/samyfodil/musql/replication"
	v1 "github.com/samyfodil/musql/replication/rpc/gen/replication/v1"
	"google.golang.org/protobuf/proto"
)

// OpsToProto converts ops to their messages.
func OpsToProto(ops []replication.Op) []*v1.Op {
	out := make([]*v1.Op, len(ops))
	for i, op := range ops {
		cells := make([]*v1.Cell, len(op.Cells))
		for j, c := range op.Cells {
			cells[j] = &v1.Cell{Col: c.Col, Type: uint32(c.Type), Val: c.Val}
		}
		out[i] = &v1.Op{Site: op.Site, Seq: op.Seq, Hlc: uint64(op.HLC), Tbl: op.Tbl, Pk: op.PK, Kind: uint32(op.Kind), Cells: cells}
	}
	return out
}

// OpsFromProto converts op messages back, refusing one no node wrote: a kind
// or a cell type out of range would be read as something else.
func OpsFromProto(ops []*v1.Op) ([]replication.Op, error) {
	out := make([]replication.Op, len(ops))
	for i, op := range ops {
		if op.GetSite() == "" || op.GetSeq() == 0 || op.GetKind() > uint32(replication.OpSchema) {
			return nil, fmt.Errorf("rpc: malformed op %s/%d (kind %d)", op.GetSite(), op.GetSeq(), op.GetKind())
		}
		cells := make([]replication.Cell, len(op.GetCells()))
		for j, c := range op.GetCells() {
			if c.GetType() > uint32(replication.TypeBlob) {
				return nil, fmt.Errorf("rpc: op %s/%d: cell %q has type %d", op.GetSite(), op.GetSeq(), c.GetCol(), c.GetType())
			}
			cells[j] = replication.Cell{Col: c.GetCol(), Type: byte(c.GetType()), Val: c.GetVal()}
		}
		out[i] = replication.Op{Site: op.GetSite(), Seq: op.GetSeq(), HLC: replication.HLC(op.GetHlc()), Tbl: op.GetTbl(),
			PK: op.GetPk(), Kind: replication.OpKind(op.GetKind()), Cells: cells}
	}
	return out, nil
}

// VersionVectorToProto converts a version vector to its entries.
func VersionVectorToProto(vv replication.VersionVector) []*v1.VVEntry {
	out := make([]*v1.VVEntry, 0, len(vv))
	for site, seq := range vv {
		out = append(out, &v1.VVEntry{Site: site, Seq: seq})
	}
	return out
}

// VersionVectorFromProto converts version vector entries back.
func VersionVectorFromProto(entries []*v1.VVEntry) replication.VersionVector {
	vv := make(replication.VersionVector, len(entries))
	for _, e := range entries {
		vv[e.GetSite()] = e.GetSeq()
	}
	return vv
}

// AcksToProto converts acks to their entries.
func AcksToProto(acks replication.Acks) []*v1.AckEntry {
	out := make([]*v1.AckEntry, 0, len(acks))
	for site, a := range acks {
		out = append(out, &v1.AckEntry{Site: site, Hlc: uint64(a.HLC), Vv: VersionVectorToProto(a.VV)})
	}
	return out
}

// AcksFromProto converts ack entries back.
func AcksFromProto(entries []*v1.AckEntry) replication.Acks {
	acks := make(replication.Acks, len(entries))
	for _, e := range entries {
		acks[e.GetSite()] = replication.Ack{HLC: replication.HLC(e.GetHlc()), VV: VersionVectorFromProto(e.GetVv())}
	}
	return acks
}

func rangesToProto(ranges map[string]uint64) []*v1.RangeEntry {
	out := make([]*v1.RangeEntry, 0, len(ranges))
	for site, lo := range ranges {
		out = append(out, &v1.RangeEntry{Site: site, Lo: lo})
	}
	return out
}

func rangesFromProto(entries []*v1.RangeEntry) map[string]uint64 {
	ranges := make(map[string]uint64, len(entries))
	for _, e := range entries {
		ranges[e.GetSite()] = e.GetLo()
	}
	return ranges
}

// SnapshotPageToProto converts a snapshot page to its message.
func SnapshotPageToProto(p replication.SnapshotPage) *v1.SnapshotPage {
	cells := make([]*v1.ClockCell, len(p.Cells))
	for i, c := range p.Cells {
		cells[i] = &v1.ClockCell{Tbl: c.Tbl, Pk: c.PK, Col: c.Col, Hlc: uint64(c.HLC), Site: c.Site, Val: c.Val}
	}
	return &v1.SnapshotPage{Ops: OpsToProto(p.Ops), Floors: VersionVectorToProto(p.Floors), Ranges: rangesToProto(p.Ranges), Cells: cells, Next: p.Next}
}

// SnapshotPageFromProto converts a snapshot page message back.
func SnapshotPageFromProto(m *v1.SnapshotPage) (replication.SnapshotPage, error) {
	ops, err := OpsFromProto(m.GetOps())
	if err != nil {
		return replication.SnapshotPage{}, err
	}
	p := replication.SnapshotPage{Ops: ops, Floors: VersionVectorFromProto(m.GetFloors()), Ranges: rangesFromProto(m.GetRanges())}
	if len(m.GetNext()) > 0 {
		p.Next = m.GetNext()
	}
	if len(m.GetCells()) > 0 {
		p.Cells = make([]replication.ClockCell, len(m.GetCells()))
	}
	for i, c := range m.GetCells() {
		p.Cells[i] = replication.ClockCell{Tbl: c.GetTbl(), PK: c.GetPk(), Col: c.GetCol(), HLC: replication.HLC(c.GetHlc()), Site: c.GetSite(), Val: c.GetVal()}
	}
	return p, nil
}

// ImageRequestToProto converts an image request to its message.
func ImageRequestToProto(r replication.ImageRequest) *v1.ImageRequest {
	return &v1.ImageRequest{Layout: r.Layout, FileVersion: r.FileVersion, MinFileVersion: r.MinFileVersion, DeltaVersion: r.DeltaVersion}
}

// ImageRequestFromProto converts an image request message back.
func ImageRequestFromProto(m *v1.ImageRequest) replication.ImageRequest {
	return replication.ImageRequest{Layout: m.GetLayout(), FileVersion: m.GetFileVersion(), MinFileVersion: m.GetMinFileVersion(), DeltaVersion: m.GetDeltaVersion()}
}

// ImageManifestToProto converts an image manifest to its message.
func ImageManifestToProto(m replication.ImageManifest) *v1.ImageManifest {
	return &v1.ImageManifest{Site: m.Site, FileVersion: m.FileVersion, DeltaVersion: m.DeltaVersion, Layout: m.Layout, Catalog: m.Catalog,
		Size: uint64(m.Size), DeltaSize: uint64(m.DeltaSize), Crc32C: m.Checksum, Vv: VersionVectorToProto(m.VV),
		Floors: VersionVectorToProto(m.Floors), Ranges: rangesToProto(m.Ranges), MaxHlc: uint64(m.MaxHLC)}
}

// ImageManifestFromProto converts an image manifest message back.
func ImageManifestFromProto(m *v1.ImageManifest) replication.ImageManifest {
	return replication.ImageManifest{Site: m.GetSite(), FileVersion: m.GetFileVersion(), DeltaVersion: m.GetDeltaVersion(), Layout: m.GetLayout(),
		Catalog: m.GetCatalog(), Size: int64(m.GetSize()), DeltaSize: int64(m.GetDeltaSize()), Checksum: m.GetCrc32C(),
		VV: VersionVectorFromProto(m.GetVv()), Floors: VersionVectorFromProto(m.GetFloors()), Ranges: rangesFromProto(m.GetRanges()),
		MaxHLC: replication.HLC(m.GetMaxHlc())}
}

// MarshalQuorumState encodes a Quorum's frontier (replication.Quorum.Frontier)
// as a QuorumState, for a Leader-mode node's raft snapshot.
func MarshalQuorumState(frontier replication.VersionVector) ([]byte, error) {
	return proto.Marshal(&v1.QuorumState{Frontier: VersionVectorToProto(frontier)})
}

// UnmarshalQuorumState decodes what MarshalQuorumState encoded.
func UnmarshalQuorumState(b []byte) (replication.VersionVector, error) {
	var m v1.QuorumState
	if err := proto.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("rpc: quorum state: %w", err)
	}
	return VersionVectorFromProto(m.GetFrontier()), nil
}
