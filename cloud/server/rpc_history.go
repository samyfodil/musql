package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
	"github.com/samyfodil/musql/cloud/store"
)

// restoreErr maps a history error to the API's: a declined restore says why,
// a database mid-restore is unavailable.
func restoreErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrBeforeHistory), errors.Is(err, store.ErrFuture):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, errUnavailable):
		return connect.NewError(connect.CodeUnavailable, err)
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return rpcErr(err)
}

func snapshotToProto(sn store.Snapshot) *v1.Snapshot {
	return &v1.Snapshot{Id: sn.ID(), TakenAt: stamp(sn.At), SizeBytes: sn.Size, Manual: sn.Manual}
}

func (s databaseService) ListSnapshots(ctx context.Context, req *connect.Request[v1.ListSnapshotsRequest]) (*connect.Response[v1.ListSnapshotsResponse], error) {
	_, d, _, err := s.lookup(ctx, req, req.Msg.Org, req.Msg.Name, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	if c, err := s.atMaster(ctx, d); c != nil || err != nil {
		if err != nil {
			return nil, err
		}
		return forward(ctx, req, c.ListSnapshots)
	}
	if err := s.localCopy(ctx, d); err != nil {
		return nil, err
	}
	h, err := s.history(d)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.ListSnapshotsResponse{}
	for _, sn := range h.Snapshots() {
		resp.Snapshots = append(resp.Snapshots, snapshotToProto(sn))
	}
	return connect.NewResponse(resp), nil
}

func (s databaseService) CreateSnapshot(ctx context.Context, req *connect.Request[v1.CreateSnapshotRequest]) (*connect.Response[v1.CreateSnapshotResponse], error) {
	_, d, _, err := s.lookup(ctx, req, req.Msg.Org, req.Msg.Name, state.RoleMember)
	if err != nil {
		return nil, err
	}
	if c, err := s.atMaster(ctx, d); c != nil || err != nil {
		if err != nil {
			return nil, err
		}
		return forward(ctx, req, c.CreateSnapshot)
	}
	if err := s.localCopy(ctx, d); err != nil {
		return nil, err
	}
	h, err := s.history(d)
	if err != nil {
		return nil, rpcErr(err)
	}
	sn, err := h.Snapshot(true)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.CreateSnapshotResponse{Snapshot: snapshotToProto(sn)}), nil
}

func (s databaseService) Restore(ctx context.Context, req *connect.Request[v1.RestoreRequest]) (*connect.Response[v1.RestoreResponse], error) {
	m := req.Msg
	o, d, _, err := s.lookup(ctx, req, m.Org, m.Name, state.RoleMember)
	if err != nil {
		return nil, err
	}
	// A regular database's master holds the history to restore from, and is
	// the one that writes it.
	if c, err := s.atMaster(ctx, d); c != nil || err != nil {
		if err != nil {
			return nil, err
		}
		return forward(ctx, req, c.Restore)
	}
	if err := s.localCopy(ctx, d); err != nil {
		return nil, err
	}
	at := time.Now()
	if m.At != nil {
		at = m.At.AsTime()
	}
	if m.InPlace {
		if m.NewName != "" {
			return nil, invalid("in_place and new_name exclude each other")
		}
		restore := s.restoreInPlace
		if s.cl != nil {
			restore = s.restoreReplicatedInPlace
		}
		if err := restore(ctx, d, at); err != nil {
			return nil, restoreErr(err)
		}
		return connect.NewResponse(&v1.RestoreResponse{Database: s.dbToProto(ctx, o, d)}), nil
	}
	name := m.NewName
	if name == "" {
		name = d.Name + "-" + at.UTC().Format("20060102-150405")
	}
	if err := validName("database", name); err != nil {
		return nil, err
	}
	if _, err := s.store.Database(ctx, o.ID, name); err == nil {
		return nil, rpcErr(state.ErrExists)
	}
	nd := &state.Database{ID: state.NewID(), OrgID: o.ID, Name: name, GroupID: d.GroupID, Parent: d.ID, ParentAt: at, Type: d.Type, Durability: d.Durability}
	if err := os.MkdirAll(filepath.Dir(s.dbPath(nd)), 0o700); err != nil {
		return nil, rpcErr(err)
	}
	err = restoreErr(s.copyAt(ctx, d, at, false, s.dbPath(nd)))
	if err == nil && s.cl != nil {
		err = s.checkReplicable(ctx, nd, s.dbPath(nd))
	}
	if err != nil {
		s.removeFiles(nd)
		return nil, err
	}
	if err := s.host(ctx, nd); err != nil {
		return nil, err
	}
	if s.cl != nil {
		if err := s.openReplicated(ctx, nd); err != nil {
			s.log.Printf("open %s: %v", nd.ID, err)
		}
	}
	return connect.NewResponse(&v1.RestoreResponse{Database: s.dbToProto(ctx, o, nd)}), nil
}

// usageRange is a request's [from, to) in whole hours: the last 24 hours by
// default.
func usageRange(from, to interface{ AsTime() time.Time }, hasFrom, hasTo bool) (time.Time, time.Time) {
	t := time.Now()
	if hasTo {
		t = to.AsTime()
	}
	f := t.Add(-24 * time.Hour)
	if hasFrom {
		f = from.AsTime()
	}
	return f.Truncate(time.Hour), t.Truncate(time.Hour).Add(time.Hour)
}

func pointOf(h *state.UsageHour) *v1.UsagePoint {
	return &v1.UsagePoint{Hour: stamp(h.Hour), RowsRead: h.RowsRead, RowsWritten: h.RowsWritten, Statements: h.Statements,
		Errors: h.Errors, BytesStored: h.BytesStored, P50Ms: h.Latency.Quantile(0.5), P95Ms: h.Latency.Quantile(0.95)}
}

func (s databaseService) Usage(ctx context.Context, req *connect.Request[v1.UsageRequest]) (*connect.Response[v1.UsageResponse], error) {
	m := req.Msg
	_, o, _, err := s.org(ctx, req.Header(), m.Org, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	from, to := usageRange(m.From, m.To, m.From != nil, m.To != nil)
	if !from.Before(to) {
		return nil, invalid("from is not before to")
	}
	// What is counted but not yet flushed belongs in the answer, every node's.
	if err := s.flushUsage(ctx); err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.UsageResponse{}
	if m.Name != "" {
		d, err := s.store.Database(ctx, o.ID, m.Name)
		if err != nil {
			return nil, rpcErr(err)
		}
		hs, err := s.store.UsageHours(ctx, o.ID, d.ID, from, to)
		if err != nil {
			return nil, rpcErr(err)
		}
		for _, h := range hs {
			resp.Points = append(resp.Points, pointOf(h))
		}
		return connect.NewResponse(resp), nil
	}
	dbs, err := s.store.Databases(ctx, o.ID, "")
	if err != nil {
		return nil, rpcErr(err)
	}
	names := map[string]string{}
	for _, d := range dbs {
		names[d.ID] = d.Name
	}
	hs, err := s.store.UsageHours(ctx, o.ID, "", from, to)
	if err != nil {
		return nil, rpcErr(err)
	}
	per := map[string]*v1.DatabaseUsage{}
	totals := map[int64]*state.UsageHour{}
	for _, h := range hs {
		if name, ok := names[h.DBID]; ok {
			du := per[h.DBID]
			if du == nil {
				du = &v1.DatabaseUsage{Name: name}
				per[h.DBID] = du
				resp.Databases = append(resp.Databases, du)
			}
			du.Points = append(du.Points, pointOf(h))
		}
		t := totals[h.Hour.Unix()]
		if t == nil {
			t = &state.UsageHour{Hour: h.Hour}
			totals[h.Hour.Unix()] = t
		}
		t.Statements += h.Statements
		t.RowsRead += h.RowsRead
		t.RowsWritten += h.RowsWritten
		t.Errors += h.Errors
		t.BytesStored += h.BytesStored
		t.Latency.Merge(&h.Latency)
	}
	hours := make([]int64, 0, len(totals))
	for k := range totals {
		hours = append(hours, k)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
	for _, k := range hours {
		resp.Points = append(resp.Points, pointOf(totals[k]))
	}
	sort.Slice(resp.Databases, func(i, j int) bool { return resp.Databases[i].Name < resp.Databases[j].Name })
	return connect.NewResponse(resp), nil
}

func (s databaseService) Stats(ctx context.Context, req *connect.Request[v1.StatsRequest]) (*connect.Response[v1.StatsResponse], error) {
	m := req.Msg
	_, d, _, err := s.lookup(ctx, req, m.Org, m.Name, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	from, to := usageRange(m.From, m.To, m.From != nil, m.To != nil)
	limit := int(m.Limit)
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 200)
	if err := s.flushUsage(ctx); err != nil {
		return nil, rpcErr(err)
	}
	qs, err := s.store.TopQueries(ctx, d.ID, from, to, limit)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.StatsResponse{}
	for _, q := range qs {
		resp.TopQueries = append(resp.TopQueries, &v1.TopQuery{Sql: q.SQL, Count: q.Count, TotalMs: float64(q.TotalUS) / 1000,
			RowsRead: q.RowsRead, RowsWritten: q.RowsWritten})
	}
	return connect.NewResponse(resp), nil
}
