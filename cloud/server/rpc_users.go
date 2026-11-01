package server

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/state"
)

type authService struct{ *Server }

func (s authService) WhoAmI(ctx context.Context, req *connect.Request[v1.WhoAmIRequest]) (*connect.Response[v1.WhoAmIResponse], error) {
	u, err := s.caller(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ms, err := s.store.UserOrgs(ctx, u.ID)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.WhoAmIResponse{User: userToProto(u)}
	for _, m := range ms {
		resp.Memberships = append(resp.Memberships, &v1.OrgRole{Org: orgToProto(m.Org), Role: roleToProto(m.Role)})
	}
	return connect.NewResponse(resp), nil
}

func (s authService) ListPlatformTokens(ctx context.Context, req *connect.Request[v1.ListPlatformTokensRequest]) (*connect.Response[v1.ListPlatformTokensResponse], error) {
	u, err := s.caller(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ts, err := s.store.PlatformTokens(ctx, u.ID)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.ListPlatformTokensResponse{}
	for _, t := range ts {
		resp.Tokens = append(resp.Tokens, &v1.PlatformToken{Id: t.ID, Name: t.Name, CreatedAt: stamp(t.CreatedAt), LastUsedAt: stamp(t.LastUsedAt)})
	}
	return connect.NewResponse(resp), nil
}

func (s authService) CreatePlatformToken(ctx context.Context, req *connect.Request[v1.CreatePlatformTokenRequest]) (*connect.Response[v1.CreatePlatformTokenResponse], error) {
	u, err := s.caller(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" || len(name) > 100 {
		return nil, invalid("a token needs a name of at most 100 characters")
	}
	t, secret, err := s.store.CreatePlatformToken(ctx, u.ID, name)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.CreatePlatformTokenResponse{
		Token:  &v1.PlatformToken{Id: t.ID, Name: t.Name, CreatedAt: stamp(t.CreatedAt)},
		Secret: secret,
	}), nil
}

func (s authService) RevokePlatformToken(ctx context.Context, req *connect.Request[v1.RevokePlatformTokenRequest]) (*connect.Response[v1.RevokePlatformTokenResponse], error) {
	u, err := s.caller(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if err := s.store.RevokePlatformToken(ctx, u.ID, req.Msg.Id); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.RevokePlatformTokenResponse{}), nil
}

type userService struct{ *Server }

func (s userService) List(ctx context.Context, req *connect.Request[v1.ListUsersRequest]) (*connect.Response[v1.ListUsersResponse], error) {
	if _, err := s.admin(ctx, req.Header()); err != nil {
		return nil, err
	}
	us, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.ListUsersResponse{}
	for _, u := range us {
		resp.Users = append(resp.Users, userToProto(u))
	}
	return connect.NewResponse(resp), nil
}

func validPassword(p string) error {
	if len(p) < 8 || len(p) > 256 {
		return invalid("a password has 8 to 256 characters")
	}
	return nil
}

func (s userService) Create(ctx context.Context, req *connect.Request[v1.CreateUserRequest]) (*connect.Response[v1.CreateUserResponse], error) {
	if _, err := s.admin(ctx, req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	email := state.NormalizeEmail(m.Email)
	if !strings.Contains(email, "@") || len(email) > 254 {
		return nil, invalid("a valid email is required")
	}
	if err := validPassword(m.Password); err != nil {
		return nil, err
	}
	u, err := s.store.CreateUser(ctx, email, strings.TrimSpace(m.Name), state.HashPassword(m.Password), m.Admin)
	if err != nil {
		return nil, rpcErr(err)
	}
	if err := s.store.EnsureGroupLocation(ctx, s.cfg.Node.Location); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.CreateUserResponse{User: userToProto(u)}), nil
}

func (s userService) Update(ctx context.Context, req *connect.Request[v1.UpdateUserRequest]) (*connect.Response[v1.UpdateUserResponse], error) {
	me, err := s.admin(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	u, err := s.store.UserByID(ctx, m.Id)
	if err != nil {
		return nil, rpcErr(err)
	}
	if m.Name != nil {
		u.Name = strings.TrimSpace(*m.Name)
	}
	if m.Disabled != nil {
		u.Disabled = *m.Disabled
	}
	if m.Admin != nil {
		u.Admin = *m.Admin
	}
	if u.ID == me.ID && (u.Disabled || !u.Admin) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("an admin cannot disable or demote itself"))
	}
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return nil, rpcErr(err)
	}
	if u.Disabled {
		if err := s.store.RevokeUserRefreshTokens(ctx, u.ID); err != nil {
			return nil, rpcErr(err)
		}
	}
	return connect.NewResponse(&v1.UpdateUserResponse{User: userToProto(u)}), nil
}

func (s userService) ResetPassword(ctx context.Context, req *connect.Request[v1.ResetPasswordRequest]) (*connect.Response[v1.ResetPasswordResponse], error) {
	if _, err := s.admin(ctx, req.Header()); err != nil {
		return nil, err
	}
	if err := validPassword(req.Msg.Password); err != nil {
		return nil, err
	}
	if err := s.store.SetPassword(ctx, req.Msg.Id, state.HashPassword(req.Msg.Password)); err != nil {
		return nil, rpcErr(err)
	}
	if err := s.store.RevokeUserRefreshTokens(ctx, req.Msg.Id); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.ResetPasswordResponse{}), nil
}

func (s userService) Delete(ctx context.Context, req *connect.Request[v1.DeleteUserRequest]) (*connect.Response[v1.DeleteUserResponse], error) {
	me, err := s.admin(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	if req.Msg.Id == me.ID {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("an admin cannot delete itself"))
	}
	if err := s.store.DeleteUser(ctx, req.Msg.Id); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.DeleteUserResponse{}), nil
}
