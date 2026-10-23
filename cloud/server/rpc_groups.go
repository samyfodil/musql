package server

import (
	"context"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

type groupService struct{ *Server }

func groupToProto(o *state.Org, g *state.Group) *v1.Group {
	return &v1.Group{Name: g.Name, Org: o.Slug, Primary: g.Primary, Locations: g.Locations, CreatedAt: stamp(g.CreatedAt), Type: typeToProto(g.Type)}
}

// locations are the deployment's: this node's and its peers'.
func (s *Server) locations() []*v1.Location {
	out := []*v1.Location{{Code: s.cfg.Node.Location, Node: s.cfg.Node.Name}}
	for _, p := range s.cfg.Peers {
		out = append(out, &v1.Location{Code: p.Location, Node: p.Name})
	}
	return out
}

func (s *Server) knownLocation(code string) error {
	for _, l := range s.locations() {
		if l.Code == code {
			return nil
		}
	}
	return invalid("unknown location " + code)
}

func (s groupService) List(ctx context.Context, req *connect.Request[v1.ListGroupsRequest]) (*connect.Response[v1.ListGroupsResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	gs, err := s.store.Groups(ctx, o.ID)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.ListGroupsResponse{}
	for _, g := range gs {
		resp.Groups = append(resp.Groups, groupToProto(o, g))
	}
	return connect.NewResponse(resp), nil
}

func (s groupService) Create(ctx context.Context, req *connect.Request[v1.CreateGroupRequest]) (*connect.Response[v1.CreateGroupResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := validName("group", req.Msg.Name); err != nil {
		return nil, err
	}
	loc := req.Msg.Location
	if loc == "" {
		loc = s.cfg.Node.Location
	}
	if err := s.knownLocation(loc); err != nil {
		return nil, err
	}
	typ, err := s.checkType(req.Msg.Type, s.store.DefaultType())
	if err != nil {
		return nil, err
	}
	g, err := s.store.CreateGroup(ctx, o.ID, req.Msg.Name, loc, typ)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.CreateGroupResponse{Group: groupToProto(o, g)}), nil
}

func (s groupService) Delete(ctx context.Context, req *connect.Request[v1.DeleteGroupRequest]) (*connect.Response[v1.DeleteGroupResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
	if err != nil {
		return nil, err
	}
	g, err := s.store.Group(ctx, o.ID, req.Msg.Name)
	if err != nil {
		return nil, rpcErr(err)
	}
	if err := s.store.DeleteGroup(ctx, g.ID); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.DeleteGroupResponse{}), nil
}

func (s groupService) AddLocation(ctx context.Context, req *connect.Request[v1.AddLocationRequest]) (*connect.Response[v1.AddLocationResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
	if err != nil {
		return nil, err
	}
	if err := s.knownLocation(req.Msg.Location); err != nil {
		return nil, err
	}
	g, err := s.store.Group(ctx, o.ID, req.Msg.Group)
	if err != nil {
		return nil, rpcErr(err)
	}
	if err := s.store.AddLocation(ctx, g.ID, req.Msg.Location); err != nil {
		return nil, rpcErr(err)
	}
	if g, err = s.store.GroupByID(ctx, g.ID); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.AddLocationResponse{Group: groupToProto(o, g)}), nil
}

func (s groupService) RemoveLocation(ctx context.Context, req *connect.Request[v1.RemoveLocationRequest]) (*connect.Response[v1.RemoveLocationResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
	if err != nil {
		return nil, err
	}
	g, err := s.store.Group(ctx, o.ID, req.Msg.Group)
	if err != nil {
		return nil, rpcErr(err)
	}
	if req.Msg.Location == g.Primary {
		return nil, invalid("the primary location cannot be removed")
	}
	if err := s.store.RemoveLocation(ctx, g.ID, req.Msg.Location); err != nil {
		return nil, rpcErr(err)
	}
	if g, err = s.store.GroupByID(ctx, g.ID); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.RemoveLocationResponse{Group: groupToProto(o, g)}), nil
}

func (s groupService) ListLocations(ctx context.Context, req *connect.Request[v1.ListLocationsRequest]) (*connect.Response[v1.ListLocationsResponse], error) {
	if _, err := s.caller(ctx, req.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ListLocationsResponse{Locations: s.locations()}), nil
}
