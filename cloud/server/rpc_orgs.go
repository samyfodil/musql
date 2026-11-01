package server

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

type orgService struct{ *Server }

func validName(kind, name string) error {
	if !state.ValidName.MatchString(name) {
		return invalid(kind + " names are 1-63 lower-case letters, digits and dashes, starting with a letter or digit")
	}
	return nil
}

func (s orgService) List(ctx context.Context, req *connect.Request[v1.ListOrgsRequest]) (*connect.Response[v1.ListOrgsResponse], error) {
	u, err := s.caller(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	resp := &v1.ListOrgsResponse{}
	if u.Admin {
		os, err := s.store.AllOrgs(ctx)
		if err != nil {
			return nil, rpcErr(err)
		}
		for _, o := range os {
			resp.Orgs = append(resp.Orgs, orgToProto(o))
		}
		return connect.NewResponse(resp), nil
	}
	ms, err := s.store.UserOrgs(ctx, u.ID)
	if err != nil {
		return nil, rpcErr(err)
	}
	for _, m := range ms {
		resp.Orgs = append(resp.Orgs, orgToProto(m.Org))
	}
	return connect.NewResponse(resp), nil
}

func (s orgService) Create(ctx context.Context, req *connect.Request[v1.CreateOrgRequest]) (*connect.Response[v1.CreateOrgResponse], error) {
	u, err := s.caller(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if err := validName("org", req.Msg.Slug); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		name = req.Msg.Slug
	}
	o, err := s.store.CreateOrg(ctx, req.Msg.Slug, name, u.ID, s.cfg.Node.Location)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.CreateOrgResponse{Org: orgToProto(o)}), nil
}

func (s orgService) Update(ctx context.Context, req *connect.Request[v1.UpdateOrgRequest]) (*connect.Response[v1.UpdateOrgResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		return nil, invalid("a name is required")
	}
	if err := s.store.UpdateOrgName(ctx, o.ID, name); err != nil {
		return nil, rpcErr(err)
	}
	o.Name = name
	return connect.NewResponse(&v1.UpdateOrgResponse{Org: orgToProto(o)}), nil
}

func (s orgService) Delete(ctx context.Context, req *connect.Request[v1.DeleteOrgRequest]) (*connect.Response[v1.DeleteOrgResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleOwner)
	if err != nil {
		return nil, err
	}
	if o.Personal {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a personal org goes with its user"))
	}
	dbs, err := s.store.Databases(ctx, o.ID, "")
	if err != nil {
		return nil, rpcErr(err)
	}
	if err := s.store.DeleteOrg(ctx, o.ID); err != nil {
		return nil, rpcErr(err)
	}
	for _, d := range dbs {
		s.removeFiles(d)
	}
	return connect.NewResponse(&v1.DeleteOrgResponse{}), nil
}

func (s orgService) ListMembers(ctx context.Context, req *connect.Request[v1.ListMembersRequest]) (*connect.Response[v1.ListMembersResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	ms, err := s.store.Members(ctx, o.ID)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.ListMembersResponse{}
	for _, m := range ms {
		resp.Members = append(resp.Members, &v1.Member{User: userToProto(m.User), Role: roleToProto(m.Role)})
	}
	return connect.NewResponse(resp), nil
}

func inviteToProto(inv *state.Invite) *v1.Invite {
	return &v1.Invite{Id: inv.ID, Org: inv.OrgSlug, Email: inv.Email, Role: roleToProto(inv.Role), CreatedAt: stamp(inv.CreatedAt)}
}

// grantable checks that a caller with role mine may give or take away role r:
// only an owner deals in owners.
func grantable(mine, r state.Role) error {
	if r == state.RoleOwner && mine != state.RoleOwner {
		return errDenied
	}
	return nil
}

func (s orgService) Invite(ctx context.Context, req *connect.Request[v1.InviteRequest]) (*connect.Response[v1.InviteResponse], error) {
	_, o, mine, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
	if err != nil {
		return nil, err
	}
	r, err := roleFromProto(req.Msg.Role)
	if err != nil {
		return nil, err
	}
	if err := grantable(mine, r); err != nil {
		return nil, err
	}
	if !strings.Contains(req.Msg.Email, "@") {
		return nil, invalid("a valid email is required")
	}
	inv, err := s.store.CreateInvite(ctx, o.ID, req.Msg.Email, r)
	if err != nil {
		return nil, rpcErr(err)
	}
	inv.OrgSlug = o.Slug
	return connect.NewResponse(&v1.InviteResponse{Invite: inviteToProto(inv)}), nil
}

func (s orgService) UpdateMember(ctx context.Context, req *connect.Request[v1.UpdateMemberRequest]) (*connect.Response[v1.UpdateMemberResponse], error) {
	_, o, mine, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
	if err != nil {
		return nil, err
	}
	r, err := roleFromProto(req.Msg.Role)
	if err != nil {
		return nil, err
	}
	cur, err := s.store.MemberRole(ctx, o.ID, req.Msg.UserId)
	if err != nil {
		return nil, rpcErr(err)
	}
	if cur == "" {
		return nil, rpcErr(state.ErrNotFound)
	}
	if err := grantable(mine, r); err != nil {
		return nil, err
	}
	if err := grantable(mine, cur); err != nil {
		return nil, err
	}
	if cur == state.RoleOwner && r != state.RoleOwner {
		if err := s.keepAnOwner(ctx, o.ID); err != nil {
			return nil, err
		}
	}
	if err := s.store.SetMember(ctx, o.ID, req.Msg.UserId, r); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.UpdateMemberResponse{}), nil
}

func (s orgService) keepAnOwner(ctx context.Context, orgID string) error {
	n, err := s.store.CountOwners(ctx, orgID)
	if err != nil {
		return rpcErr(err)
	}
	if n <= 1 {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("an org keeps at least one owner"))
	}
	return nil
}

func (s orgService) RemoveMember(ctx context.Context, req *connect.Request[v1.RemoveMemberRequest]) (*connect.Response[v1.RemoveMemberResponse], error) {
	u, o, mine, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	// Anyone may leave; removing someone else takes an admin.
	if req.Msg.UserId != u.ID && !mine.AtLeast(state.RoleAdmin) {
		return nil, errDenied
	}
	cur, err := s.store.MemberRole(ctx, o.ID, req.Msg.UserId)
	if err != nil {
		return nil, rpcErr(err)
	}
	if cur == "" {
		return nil, rpcErr(state.ErrNotFound)
	}
	if req.Msg.UserId != u.ID {
		if err := grantable(mine, cur); err != nil {
			return nil, err
		}
	}
	if cur == state.RoleOwner {
		if err := s.keepAnOwner(ctx, o.ID); err != nil {
			return nil, err
		}
	}
	if err := s.store.RemoveMember(ctx, o.ID, req.Msg.UserId); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.RemoveMemberResponse{}), nil
}

func (s orgService) ListInvites(ctx context.Context, req *connect.Request[v1.ListInvitesRequest]) (*connect.Response[v1.ListInvitesResponse], error) {
	var invs []*state.Invite
	if req.Msg.Org == "" {
		u, err := s.caller(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		if invs, err = s.store.EmailInvites(ctx, u.Email); err != nil {
			return nil, rpcErr(err)
		}
	} else {
		_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleAdmin)
		if err != nil {
			return nil, err
		}
		if invs, err = s.store.OrgInvites(ctx, o.ID); err != nil {
			return nil, rpcErr(err)
		}
	}
	resp := &v1.ListInvitesResponse{}
	for _, inv := range invs {
		resp.Invites = append(resp.Invites, inviteToProto(inv))
	}
	return connect.NewResponse(resp), nil
}

func (s orgService) AcceptInvite(ctx context.Context, req *connect.Request[v1.AcceptInviteRequest]) (*connect.Response[v1.AcceptInviteResponse], error) {
	u, err := s.caller(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	inv, err := s.store.AcceptInvite(ctx, req.Msg.Id, u)
	if err != nil {
		return nil, rpcErr(err)
	}
	o, err := s.store.OrgByID(ctx, inv.OrgID)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.AcceptInviteResponse{Org: orgToProto(o)}), nil
}
