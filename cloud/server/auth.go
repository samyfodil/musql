package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/oidc"
	"github.com/samyfodil/musql/cloud/state"
)

var (
	errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	errDenied          = connect.NewError(connect.CodePermissionDenied, errors.New("your role does not allow this"))
)

func bearer(h http.Header) string {
	tok, _ := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(tok)
}

// userFromToken resolves a user access token or a platform token.
func (s *Server) userFromToken(ctx context.Context, tok string) (*state.User, error) {
	if tok == "" {
		return nil, oidc.ErrInvalidToken
	}
	if strings.HasPrefix(tok, state.PlatformTokenPrefix) {
		u, err := s.store.PlatformTokenUser(ctx, tok)
		switch {
		case errors.Is(err, state.ErrNotFound):
			return nil, oidc.ErrInvalidToken
		case err != nil:
			return nil, err
		}
		if u.Disabled {
			return nil, oidc.ErrInvalidToken
		}
		return u, nil
	}
	return s.provider.VerifyAccess(ctx, tok)
}

// caller is the signed-in user making an RPC.
func (s *Server) caller(ctx context.Context, h http.Header) (*state.User, error) {
	u, err := s.userFromToken(ctx, bearer(h))
	switch {
	case errors.Is(err, oidc.ErrInvalidToken):
		return nil, errUnauthenticated
	case err != nil:
		// The state is out of reach, as around a master's election: the
		// token may well be good.
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("checking the token: %w", err))
	}
	return u, nil
}

// role is u's role in org; a server admin is an owner everywhere.
func (s *Server) role(ctx context.Context, u *state.User, org *state.Org) (state.Role, error) {
	if u.Admin {
		return state.RoleOwner, nil
	}
	return s.store.MemberRole(ctx, org.ID, u.ID)
}

// org resolves an org by slug and checks that the caller holds at least min
// in it. An org the caller is not in is reported as not found, so names do
// not leak.
func (s *Server) org(ctx context.Context, h http.Header, slug string, min state.Role) (*state.User, *state.Org, state.Role, error) {
	u, err := s.caller(ctx, h)
	if err != nil {
		return nil, nil, "", err
	}
	o, err := s.store.OrgBySlug(ctx, slug)
	if err != nil {
		return nil, nil, "", rpcErr(err)
	}
	r, err := s.role(ctx, u, o)
	if err != nil {
		return nil, nil, "", rpcErr(err)
	}
	if r == "" {
		return nil, nil, "", connect.NewError(connect.CodeNotFound, errors.New("no such org"))
	}
	if !r.AtLeast(min) {
		return nil, nil, "", errDenied
	}
	return u, o, r, nil
}

func (s *Server) admin(ctx context.Context, h http.Header) (*state.User, error) {
	u, err := s.caller(ctx, h)
	if err != nil {
		return nil, err
	}
	if !u.Admin {
		return nil, errDenied
	}
	return u, nil
}

// rpcErr maps state errors onto Connect codes; anything else is internal and
// its text is not shown.
func rpcErr(err error) error {
	var ce *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return err
	case errors.Is(err, state.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	case errors.Is(err, state.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("already exists"))
	case errors.Is(err, state.ErrInUse):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("still in use"))
	}
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func invalid(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

func stamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func userToProto(u *state.User) *v1.User {
	return &v1.User{Id: u.ID, Email: u.Email, Name: u.Name, Disabled: u.Disabled, Admin: u.Admin, CreatedAt: stamp(u.CreatedAt)}
}

func orgToProto(o *state.Org) *v1.Org {
	return &v1.Org{Id: o.ID, Slug: o.Slug, Name: o.Name, Personal: o.Personal, CreatedAt: stamp(o.CreatedAt)}
}

func roleToProto(r state.Role) v1.Role {
	switch r {
	case state.RoleViewer:
		return v1.Role_ROLE_VIEWER
	case state.RoleMember:
		return v1.Role_ROLE_MEMBER
	case state.RoleAdmin:
		return v1.Role_ROLE_ADMIN
	case state.RoleOwner:
		return v1.Role_ROLE_OWNER
	}
	return v1.Role_ROLE_UNSPECIFIED
}

func roleFromProto(r v1.Role) (state.Role, error) {
	switch r {
	case v1.Role_ROLE_VIEWER:
		return state.RoleViewer, nil
	case v1.Role_ROLE_MEMBER:
		return state.RoleMember, nil
	case v1.Role_ROLE_ADMIN:
		return state.RoleAdmin, nil
	case v1.Role_ROLE_OWNER:
		return state.RoleOwner, nil
	}
	return "", invalid("a role is required")
}
