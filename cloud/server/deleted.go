package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

// purgeGrace is how long past its retention a cluster keeps a deleted
// database's record, so every node holding its files sees it expire and
// removes them before the record goes.
const purgeGrace = 24 * time.Hour

// retention is how long org's deleted databases are kept.
func (s *Server) retention(ctx context.Context, o *state.Org) time.Duration {
	if s.deletedRetention != nil && o != nil {
		return s.deletedRetention(ctx, o)
	}
	return s.cfg.deletedRetention
}

// closeDB closes a deleted database and its history, keeping the files.
func (s *Server) closeDB(id string) {
	s.pool.drop(id)
	s.closeReplicated(id)
	s.closeHistory(id)
}

// sweepDeleted removes the files and history of every deleted database past
// its org's retention as of now, then its record, and the records of deleted
// groups past theirs. In a cluster each node removes its own files, and the
// records stay purgeGrace longer for the nodes that have yet to.
func (s *Server) sweepDeleted(ctx context.Context, now time.Time) error {
	dbs, err := s.store.DeletedDatabases(ctx, "")
	if err != nil {
		return err
	}
	groups, err := s.store.DeletedGroups(ctx, "")
	if err != nil {
		return err
	}
	// An org deleted since keeps nothing: its records expired when it went.
	keep := map[string]time.Duration{}
	expiry := func(orgID string, deleted time.Time) (time.Time, error) {
		r, ok := keep[orgID]
		if !ok {
			o, err := s.store.OrgByID(ctx, orgID)
			switch {
			case err == nil:
				r = s.retention(ctx, o)
			case errors.Is(err, state.ErrNotFound):
				r = 0
			default:
				return time.Time{}, err
			}
			keep[orgID] = r
		}
		return deleted.Add(r), nil
	}
	purge := func(at time.Time) bool { return s.cl == nil || now.After(at.Add(purgeGrace)) }
	for _, d := range dbs {
		at, err := expiry(d.OrgID, d.DeletedAt)
		if err != nil {
			return err
		}
		if now.Before(at) {
			continue
		}
		s.removeFiles(&d.Database)
		if !purge(at) {
			continue
		}
		if err := s.store.PurgeDeletedDatabase(ctx, d.ID); err != nil && !errors.Is(err, state.ErrNotFound) {
			return err
		}
		s.log.Printf("removed deleted database %s (%s), kept since %s", d.ID, d.Name, d.DeletedAt.Format(time.RFC3339))
	}
	for _, g := range groups {
		at, err := expiry(g.OrgID, g.DeletedAt)
		if err != nil {
			return err
		}
		if now.Before(at) || !purge(at) {
			continue
		}
		if err := s.store.PurgeDeletedGroup(ctx, g.ID); err != nil && !errors.Is(err, state.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (s databaseService) Update(ctx context.Context, req *connect.Request[v1.UpdateDatabaseRequest]) (*connect.Response[v1.UpdateDatabaseResponse], error) {
	o, d, _, err := s.lookup(ctx, req, req.Msg.Org, req.Msg.Name, state.RoleMember)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := s.store.UpdateDatabase(ctx, d.ID, m.DeleteProtection, m.BlockReads, m.BlockWrites); err != nil {
		return nil, rpcErr(err)
	}
	if d, err = s.store.DatabaseByID(ctx, d.ID); err != nil {
		return nil, rpcErr(err)
	}
	s.pool.refresh(d)
	return connect.NewResponse(&v1.UpdateDatabaseResponse{Database: s.dbToProto(ctx, o, d)}), nil
}

func (s databaseService) ListDeleted(ctx context.Context, req *connect.Request[v1.ListDeletedRequest]) (*connect.Response[v1.ListDeletedResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	dbs, err := s.store.DeletedDatabases(ctx, o.ID)
	if err != nil {
		return nil, rpcErr(err)
	}
	groups, err := s.store.DeletedGroups(ctx, o.ID)
	if err != nil {
		return nil, rpcErr(err)
	}
	keep, now := s.retention(ctx, o), time.Now()
	resp := &v1.ListDeletedResponse{}
	for _, d := range dbs {
		if exp := d.DeletedAt.Add(keep); now.Before(exp) {
			resp.Databases = append(resp.Databases, &v1.DeletedDatabase{Database: s.deletedToProto(ctx, o, d),
				DeletedAt: stamp(d.DeletedAt), ExpiresAt: stamp(exp), DeletedWithGroup: d.DeletedWith})
		}
	}
	for _, g := range groups {
		if exp := g.DeletedAt.Add(keep); now.Before(exp) {
			resp.Groups = append(resp.Groups, &v1.DeletedGroup{Group: groupToProto(o, &g.Group), DeletedAt: stamp(g.DeletedAt), ExpiresAt: stamp(exp)})
		}
	}
	return connect.NewResponse(resp), nil
}

// deletedToProto is a deleted database as ListDeleted shows it: no URL, since
// nothing serves it, and its group's name even when the group is deleted too.
func (s *Server) deletedToProto(ctx context.Context, o *state.Org, d *state.DeletedDatabase) *v1.Database {
	out := s.dbToProto(ctx, o, &d.Database)
	out.Url = ""
	if out.Group == "" {
		if g, err := s.store.DeletedGroupByID(ctx, d.GroupID); err == nil {
			out.Group = g.Name
		}
	}
	return out
}

var errExpired = connect.NewError(connect.CodeNotFound, errors.New("deleted too long ago to restore"))

func (s databaseService) RestoreDeleted(ctx context.Context, req *connect.Request[v1.RestoreDeletedRequest]) (*connect.Response[v1.RestoreDeletedResponse], error) {
	_, o, r, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleMember)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.NewName != "" {
		if err := validName("name", m.NewName); err != nil {
			return nil, err
		}
	}
	keep, now := s.retention(ctx, o), time.Now()
	d, err := s.store.DeletedDatabaseByID(ctx, m.Id)
	switch {
	case err == nil && d.OrgID == o.ID:
		if !now.Before(d.DeletedAt.Add(keep)) {
			return nil, errExpired
		}
		nd, err := s.store.RestoreDatabase(ctx, d.ID, m.NewName)
		if err != nil {
			return nil, s.restoreDeletedErr(ctx, err, d.GroupID)
		}
		s.reopen(ctx, nd)
		return connect.NewResponse(&v1.RestoreDeletedResponse{Databases: []*v1.Database{s.dbToProto(ctx, o, nd)}}), nil
	case err != nil && !errors.Is(err, state.ErrNotFound):
		return nil, rpcErr(err)
	}
	g, err := s.store.DeletedGroupByID(ctx, m.Id)
	if err != nil || g.OrgID != o.ID {
		if err == nil || errors.Is(err, state.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no such deleted database or group"))
		}
		return nil, rpcErr(err)
	}
	// A group is deleted and made by an org's admins.
	if !r.AtLeast(state.RoleAdmin) {
		return nil, errDenied
	}
	if !now.Before(g.DeletedAt.Add(keep)) {
		return nil, errExpired
	}
	ng, dbs, err := s.store.RestoreGroup(ctx, g.ID, m.NewName)
	if err != nil {
		return nil, s.restoreDeletedErr(ctx, err, "")
	}
	resp := &v1.RestoreDeletedResponse{Group: groupToProto(o, ng)}
	for _, d := range dbs {
		s.reopen(ctx, d)
		resp.Databases = append(resp.Databases, s.dbToProto(ctx, o, d))
	}
	return connect.NewResponse(resp), nil
}

// reopen opens a restored database's replicated handle on a cluster node it
// is placed on; elsewhere placement does, and standalone the pool opens it at
// its first request.
func (s *Server) reopen(ctx context.Context, d *state.Database) {
	if s.cl == nil || !s.placedHere(ctx, d) {
		return
	}
	if d.Type != state.TypeCRDT {
		g, err := s.store.GroupByID(ctx, d.GroupID)
		if err != nil {
			return
		}
		if err := s.groupElection(g); err != nil {
			s.log.Printf("restore %s: group election: %v", d.ID, err)
			return
		}
	}
	if err := s.openReplicated(ctx, d); err != nil {
		s.log.Printf("restore %s: %v", d.ID, err)
	}
}

func (s *Server) restoreDeletedErr(ctx context.Context, err error, groupID string) error {
	switch {
	case errors.Is(err, state.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("the name is taken: restore it under new_name"))
	case errors.Is(err, state.ErrGroupDeleted):
		name := groupID
		if g, gerr := s.store.DeletedGroupByID(ctx, groupID); gerr == nil {
			name = g.Name
		}
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("its group %s is deleted: restore the group first", name))
	}
	return rpcErr(err)
}

func (s groupService) Update(ctx context.Context, req *connect.Request[v1.UpdateGroupRequest]) (*connect.Response[v1.UpdateGroupResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
	if err != nil {
		return nil, err
	}
	g, err := s.store.Group(ctx, o.ID, req.Msg.Name)
	if err != nil {
		return nil, rpcErr(err)
	}
	if req.Msg.DeleteProtection != nil {
		if err := s.store.SetGroupProtection(ctx, g.ID, *req.Msg.DeleteProtection); err != nil {
			return nil, rpcErr(err)
		}
		if g, err = s.store.GroupByID(ctx, g.ID); err != nil {
			return nil, rpcErr(err)
		}
	}
	return connect.NewResponse(&v1.UpdateGroupResponse{Group: groupToProto(o, g)}), nil
}
