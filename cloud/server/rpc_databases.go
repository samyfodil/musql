package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/go-jose/go-jose/v4/jwt"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/oidc"
	"github.com/samyfodil/musql/cloud/state"
)

type databaseService struct {
	*Server
	cloudv1connect.UnimplementedDatabaseServiceHandler
}

var sqliteMagic = []byte("SQLite format 3\x00")

func (s *Server) dbToProto(ctx context.Context, o *state.Org, d *state.Database) *v1.Database {
	out := &v1.Database{
		Id: d.ID, Org: o.Slug, Name: d.Name, Parent: d.Parent, ParentAt: stamp(d.ParentAt),
		SchemaOnly: d.SchemaOnly, CreatedAt: stamp(d.CreatedAt),
		Url: s.cfg.PublicURL + "/db/" + o.Slug + "/" + d.Name + "/",
	}
	if g, err := s.store.GroupByID(ctx, d.GroupID); err == nil {
		out.Group = g.Name
	}
	if st, err := os.Stat(s.dbPath(d)); err == nil {
		out.SizeBytes = st.Size()
	}
	return out
}

func (s databaseService) List(ctx context.Context, req *connect.Request[v1.ListDatabasesRequest]) (*connect.Response[v1.ListDatabasesResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	var groupID string
	if req.Msg.Group != "" {
		g, err := s.store.Group(ctx, o.ID, req.Msg.Group)
		if err != nil {
			return nil, rpcErr(err)
		}
		groupID = g.ID
	}
	dbs, err := s.store.Databases(ctx, o.ID, groupID)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &v1.ListDatabasesResponse{}
	for _, d := range dbs {
		resp.Databases = append(resp.Databases, s.dbToProto(ctx, o, d))
	}
	return connect.NewResponse(resp), nil
}

// lookup resolves org/name with the caller holding at least min.
func (s databaseService) lookup(ctx context.Context, req connect.AnyRequest, org, name string, min state.Role) (*state.Org, *state.Database, state.Role, error) {
	_, o, r, err := s.org(ctx, req.Header(), org, min)
	if err != nil {
		return nil, nil, "", err
	}
	d, err := s.store.Database(ctx, o.ID, name)
	if err != nil {
		return nil, nil, "", rpcErr(err)
	}
	return o, d, r, nil
}

func (s databaseService) Get(ctx context.Context, req *connect.Request[v1.GetDatabaseRequest]) (*connect.Response[v1.GetDatabaseResponse], error) {
	o, d, _, err := s.lookup(ctx, req, req.Msg.Org, req.Msg.Name, state.RoleViewer)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetDatabaseResponse{Database: s.dbToProto(ctx, o, d)}), nil
}

func (s databaseService) Create(ctx context.Context, req *connect.Request[v1.CreateDatabaseRequest]) (*connect.Response[v1.CreateDatabaseResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleMember)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validName("database", m.Name); err != nil {
		return nil, err
	}
	if m.GetBranchOf() != "" || m.SchemaOnly || m.BranchAt != nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("branching is not implemented yet"))
	}
	group := m.Group
	if group == "" {
		group = "default"
	}
	g, err := s.store.Group(ctx, o.ID, group)
	if err != nil {
		return nil, rpcErr(err)
	}
	if _, err := s.store.Database(ctx, o.ID, m.Name); err == nil {
		return nil, rpcErr(state.ErrExists)
	}
	d := &state.Database{ID: state.NewID(), OrgID: o.ID, Name: m.Name, GroupID: g.ID}
	path := s.dbPath(d)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, rpcErr(err)
	}
	if up := m.GetUpload(); up != "" {
		err = s.fromUpload(o, up, path)
	} else {
		err = createEmpty(path)
	}
	if err != nil {
		s.removeFiles(d)
		return nil, err
	}
	if err := s.store.CreateDatabase(ctx, d); err != nil {
		s.removeFiles(d)
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.CreateDatabaseResponse{Database: s.dbToProto(ctx, o, d)}), nil
}

// createEmpty writes a new, empty database at path.
func createEmpty(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return rpcErr(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		return rpcErr(err)
	}
	if _, err := os.Stat(path); err != nil {
		return rpcErr(err)
	}
	return nil
}

func (s *Server) uploadPath(o *state.Org, id string) (string, error) {
	if !state.ValidName.MatchString(id) {
		return "", invalid("unknown upload")
	}
	return filepath.Join(s.cfg.DataDir, "uploads", o.ID+"-"+id), nil
}

// fromUpload makes the database at path from an upload, converting a C
// SQLite file, and checks the result before it is served.
func (s *Server) fromUpload(o *state.Org, id, path string) error {
	src, err := s.uploadPath(o, id)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return invalid("unknown upload")
	}
	head := make([]byte, len(sqliteMagic))
	n, err := io.ReadFull(f, head)
	f.Close()
	if err != nil && n == 0 {
		return invalid("the upload is empty")
	}
	if bytes.Equal(head[:n], sqliteMagic) {
		if err := sqliteconv.Import(src, path, sqliteconv.ImportOptions{}); err != nil {
			return invalid("the SQLite file could not be imported: " + err.Error())
		}
	} else if err := copyFile(src, path); err != nil {
		return rpcErr(err)
	}
	if err := integrityOK(path); err != nil {
		return invalid("the uploaded database is not usable: " + err.Error())
	}
	os.Remove(src)
	return nil
}

func integrityOK(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New(res)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (s databaseService) Upload(ctx context.Context, req *connect.Request[v1.UploadRequest]) (*connect.Response[v1.UploadResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleMember)
	if err != nil {
		return nil, err
	}
	if len(req.Msg.Content) == 0 {
		return nil, invalid("the upload is empty")
	}
	id := state.NewID()
	p, err := s.uploadPath(o, id)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, req.Msg.Content, 0o600); err != nil {
		return nil, rpcErr(err)
	}
	format := v1.FileFormat_FILE_FORMAT_MUSQL
	if bytes.HasPrefix(req.Msg.Content, sqliteMagic) {
		format = v1.FileFormat_FILE_FORMAT_SQLITE
	}
	return connect.NewResponse(&v1.UploadResponse{Upload: id, Format: format}), nil
}

func (s databaseService) Delete(ctx context.Context, req *connect.Request[v1.DeleteDatabaseRequest]) (*connect.Response[v1.DeleteDatabaseResponse], error) {
	_, d, _, err := s.lookup(ctx, req, req.Msg.Org, req.Msg.Name, state.RoleMember)
	if err != nil {
		return nil, err
	}
	// The record goes first: from then on no token reaches the file.
	if err := s.store.DeleteDatabase(ctx, d.ID); err != nil {
		return nil, rpcErr(err)
	}
	s.removeFiles(d)
	return connect.NewResponse(&v1.DeleteDatabaseResponse{}), nil
}

func (s databaseService) Rename(ctx context.Context, req *connect.Request[v1.RenameDatabaseRequest]) (*connect.Response[v1.RenameDatabaseResponse], error) {
	o, d, _, err := s.lookup(ctx, req, req.Msg.Org, req.Msg.Name, state.RoleMember)
	if err != nil {
		return nil, err
	}
	if err := validName("database", req.Msg.NewName); err != nil {
		return nil, err
	}
	if err := s.store.RenameDatabase(ctx, d.ID, req.Msg.NewName); err != nil {
		return nil, rpcErr(err)
	}
	d.Name = req.Msg.NewName
	return connect.NewResponse(&v1.RenameDatabaseResponse{Database: s.dbToProto(ctx, o, d)}), nil
}

func (s databaseService) CreateToken(ctx context.Context, req *connect.Request[v1.CreateTokenRequest]) (*connect.Response[v1.CreateTokenResponse], error) {
	m := req.Msg
	access := accessFull
	min := state.RoleMember
	switch m.Access {
	case v1.Access_ACCESS_FULL, v1.Access_ACCESS_UNSPECIFIED:
	case v1.Access_ACCESS_READ_ONLY:
		access, min = accessReadOnly, state.RoleViewer
	default:
		return nil, invalid("unknown access")
	}
	u, o, _, err := s.org(ctx, req.Header(), m.Org, min)
	if err != nil {
		return nil, err
	}
	c := &oidc.Claims{Claims: jwt.Claims{Subject: u.ID, ID: state.NewID()}, Use: oidc.UseDB, Access: access}
	switch {
	case m.Name != "":
		d, err := s.store.Database(ctx, o.ID, m.Name)
		if err != nil {
			return nil, rpcErr(err)
		}
		c.DB, c.Gen = d.ID, d.TokenGen
	case m.Group != "":
		g, err := s.store.Group(ctx, o.ID, m.Group)
		if err != nil {
			return nil, rpcErr(err)
		}
		c.Group, c.Gen = g.ID, g.TokenGen
	default:
		return nil, invalid("a database name or a group is required")
	}
	if m.ExpiresAt != nil {
		exp := m.ExpiresAt.AsTime()
		if !exp.After(time.Now()) {
			return nil, invalid("expires_at is in the past")
		}
		c.Expiry = jwt.NewNumericDate(exp)
	}
	tok, err := s.provider.Sign(c)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.CreateTokenResponse{Token: tok}), nil
}

func (s databaseService) InvalidateTokens(ctx context.Context, req *connect.Request[v1.InvalidateTokensRequest]) (*connect.Response[v1.InvalidateTokensResponse], error) {
	_, o, _, err := s.org(ctx, req.Header(), req.Msg.Org, state.RoleMember)
	if err != nil {
		return nil, err
	}
	switch {
	case req.Msg.Name != "":
		d, err := s.store.Database(ctx, o.ID, req.Msg.Name)
		if err != nil {
			return nil, rpcErr(err)
		}
		err = s.store.BumpDatabaseTokens(ctx, d.ID)
	case req.Msg.Group != "":
		g, err := s.store.Group(ctx, o.ID, req.Msg.Group)
		if err != nil {
			return nil, rpcErr(err)
		}
		err = s.store.BumpGroupTokens(ctx, g.ID)
	default:
		return nil, invalid("a database name or a group is required")
	}
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.InvalidateTokensResponse{}), nil
}

func (s databaseService) Export(ctx context.Context, req *connect.Request[v1.ExportRequest]) (*connect.Response[v1.ExportResponse], error) {
	_, d, _, err := s.lookup(ctx, req, req.Msg.Org, req.Msg.Name, state.RoleMember)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(s.cfg.DataDir, "export-")
	if err != nil {
		return nil, rpcErr(err)
	}
	defer os.RemoveAll(tmp)
	// VACUUM INTO takes a consistent copy while the database stays live.
	snap := filepath.Join(tmp, "db.musq")
	db, err := sql.Open("sqlite", s.dbPath(d))
	if err != nil {
		return nil, rpcErr(err)
	}
	_, err = db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(snap, "'", "''")+`'`)
	db.Close()
	if err != nil {
		return nil, rpcErr(err)
	}
	out := snap
	switch req.Msg.Format {
	case v1.FileFormat_FILE_FORMAT_MUSQL, v1.FileFormat_FILE_FORMAT_UNSPECIFIED:
	case v1.FileFormat_FILE_FORMAT_SQLITE:
		out = filepath.Join(tmp, "db.sqlite")
		if err := sqliteconv.Export(snap, out, 0); err != nil {
			return nil, rpcErr(err)
		}
	default:
		return nil, invalid("unknown format")
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.ExportResponse{Content: b}), nil
}
