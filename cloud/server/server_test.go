package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-jose/go-jose/v4/jwt"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/oidc"
)

type env struct {
	t     *testing.T
	s     *Server
	ts    *httptest.Server
	admin string
}

func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	dir := t.TempDir()
	cfg, err := ParseConfig([]byte(`
data_dir: ` + dir + `
node:
  location: fra
` + extra))
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(context.Background(), cfg, "admin@example.com", "admin-password"); err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	cfg.PublicURL = ts.URL
	s, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	h = s.Handler()
	t.Cleanup(func() {
		ts.Close()
		s.Close()
	})
	e := &env{t: t, s: s, ts: ts}
	e.admin = e.tokenFor("admin@example.com")
	return e
}

// tokenFor signs an access token for a user, as the OIDC flow would.
func (e *env) tokenFor(email string) string {
	e.t.Helper()
	u, err := e.s.Store().UserByEmail(context.Background(), email)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.s.Provider().Sign(&oidc.Claims{
		Claims: jwt.Claims{Subject: u.ID, Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))},
		Use:    oidc.UseAccess, ClientID: oidc.ClientConsole,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func auth[T any](tok string, msg *T) *connect.Request[T] {
	r := connect.NewRequest(msg)
	if tok != "" {
		r.Header().Set("Authorization", "Bearer "+tok)
	}
	return r
}

func code(err error) connect.Code { return connect.CodeOf(err) }

// must unwraps a call that is expected to succeed; a failure panics, which
// fails the test with the call's stack.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (e *env) clients() (cloudv1connect.UserServiceClient, cloudv1connect.OrgServiceClient, cloudv1connect.DatabaseServiceClient) {
	c := e.ts.Client()
	return cloudv1connect.NewUserServiceClient(c, e.ts.URL), cloudv1connect.NewOrgServiceClient(c, e.ts.URL), cloudv1connect.NewDatabaseServiceClient(c, e.ts.URL)
}

// setup makes org "acme" with database "app", and user bob as a viewer.
func (e *env) setup() (bob string) {
	ctx := context.Background()
	users, orgs, dbs := e.clients()
	must(users.Create(ctx, auth(e.admin, &v1.CreateUserRequest{Email: "bob@example.com", Name: "Bob", Password: "bob-password"})))
	must(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "acme", Name: "Acme"})))
	inv := must(orgs.Invite(ctx, auth(e.admin, &v1.InviteRequest{Org: "acme", Email: "bob@example.com", Role: v1.Role_ROLE_VIEWER})))
	bob = e.tokenFor("bob@example.com")
	must(orgs.AcceptInvite(ctx, auth(bob, &v1.AcceptInviteRequest{Id: inv.Msg.Invite.Id})))
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app"})))
	return bob
}

func TestRoles(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	users, orgs, dbs := e.clients()

	if _, err := dbs.List(ctx, auth("", &v1.ListDatabasesRequest{Org: "acme"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("no token: %v", err)
	}
	if _, err := dbs.List(ctx, auth("garbage", &v1.ListDatabasesRequest{Org: "acme"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("bad token: %v", err)
	}
	list := must(dbs.List(ctx, auth(bob, &v1.ListDatabasesRequest{Org: "acme"})))
	if len(list.Msg.Databases) != 1 || list.Msg.Databases[0].Url != e.ts.URL+"/db/acme/app/" || list.Msg.Databases[0].Group != "default" {
		t.Fatalf("viewer list: %v", list.Msg)
	}
	if _, err := dbs.Create(ctx, auth(bob, &v1.CreateDatabaseRequest{Org: "acme", Name: "x"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer create: %v", err)
	}
	if _, err := dbs.Delete(ctx, auth(bob, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer delete: %v", err)
	}
	if _, err := dbs.CreateToken(ctx, auth(bob, &v1.CreateTokenRequest{Org: "acme", Name: "app", Access: v1.Access_ACCESS_FULL})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer full token: %v", err)
	}
	must(dbs.CreateToken(ctx, auth(bob, &v1.CreateTokenRequest{Org: "acme", Name: "app", Access: v1.Access_ACCESS_READ_ONLY})))
	if _, err := users.List(ctx, auth(bob, &v1.ListUsersRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("non-admin user list: %v", err)
	}
	if _, err := orgs.Invite(ctx, auth(bob, &v1.InviteRequest{Org: "acme", Email: "c@example.com", Role: v1.Role_ROLE_MEMBER})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer invite: %v", err)
	}
	// An org bob is not in does not exist for him.
	must(orgs.Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "secret"})))
	if _, err := dbs.List(ctx, auth(bob, &v1.ListDatabasesRequest{Org: "secret"})); code(err) != connect.CodeNotFound {
		t.Fatalf("foreign org: %v", err)
	}
	// The last owner stays.
	ms := must(orgs.ListMembers(ctx, auth(e.admin, &v1.ListMembersRequest{Org: "acme"})))
	var owner string
	for _, m := range ms.Msg.Members {
		if m.Role == v1.Role_ROLE_OWNER {
			owner = m.User.Id
		}
	}
	if _, err := orgs.UpdateMember(ctx, auth(e.admin, &v1.UpdateMemberRequest{Org: "acme", UserId: owner, Role: v1.Role_ROLE_MEMBER})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("demote last owner: %v", err)
	}
	// Promoted to member, bob may create.
	bobID := must(e.s.Store().UserByEmail(ctx, "bob@example.com")).ID
	must(orgs.UpdateMember(ctx, auth(e.admin, &v1.UpdateMemberRequest{Org: "acme", UserId: bobID, Role: v1.Role_ROLE_MEMBER})))
	must(dbs.Create(ctx, auth(bob, &v1.CreateDatabaseRequest{Org: "acme", Name: "bobs"})))
	// Disabled, bob's token stops working.
	disabled := true
	must(users.Update(ctx, auth(e.admin, &v1.UpdateUserRequest{Id: bobID, Disabled: &disabled})))
	if _, err := dbs.List(ctx, auth(bob, &v1.ListDatabasesRequest{Org: "acme"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("disabled user: %v", err)
	}
	// A viewer may not restore.
	must(users.Create(ctx, auth(e.admin, &v1.CreateUserRequest{Email: "vic@example.com", Name: "Vic", Password: "vic-password"})))
	inv := must(orgs.Invite(ctx, auth(e.admin, &v1.InviteRequest{Org: "acme", Email: "vic@example.com", Role: v1.Role_ROLE_VIEWER})))
	viewer := e.tokenFor("vic@example.com")
	must(orgs.AcceptInvite(ctx, auth(viewer, &v1.AcceptInviteRequest{Id: inv.Msg.Invite.Id})))
	if _, err := dbs.Restore(ctx, auth(viewer, &v1.RestoreRequest{Org: "acme", Name: "app"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("restore by a viewer: %v", err)
	}
}

func TestPlatformToken(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	a := cloudv1connect.NewAuthServiceClient(e.ts.Client(), e.ts.URL)
	created := must(a.CreatePlatformToken(ctx, auth(e.admin, &v1.CreatePlatformTokenRequest{Name: "ci"})))
	who := must(a.WhoAmI(ctx, auth(created.Msg.Secret, &v1.WhoAmIRequest{})))
	if who.Msg.User.Email != "admin@example.com" || len(who.Msg.Memberships) != 1 {
		t.Fatalf("whoami: %v", who.Msg)
	}
	must(a.RevokePlatformToken(ctx, auth(e.admin, &v1.RevokePlatformTokenRequest{Id: created.Msg.Token.Id})))
	if _, err := a.WhoAmI(ctx, auth(created.Msg.Secret, &v1.WhoAmIRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked: %v", err)
	}
}

type result struct {
	Type     string `json:"type"`
	Response struct {
		Result struct {
			Rows [][]struct {
				Type  string `json:"type"`
				Value any    `json:"value"`
			} `json:"rows"`
		} `json:"result"`
	} `json:"response"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// pipeline runs statements over Hrana's HTTP v2 pipeline, returning the HTTP
// status and each statement's result.
func (e *env) pipeline(path, tok string, stmts ...string) (int, []result) {
	e.t.Helper()
	var reqs []map[string]any
	for _, s := range stmts {
		reqs = append(reqs, map[string]any{"type": "execute", "stmt": map[string]any{"sql": s}})
	}
	reqs = append(reqs, map[string]any{"type": "close"})
	body, _ := json.Marshal(map[string]any{"requests": reqs})
	req, _ := http.NewRequest("POST", e.ts.URL+path+"/v2/pipeline", bytes.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	var out struct {
		Results []result `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, out.Results[:len(stmts)]
}

func TestHrana(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	full := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	ro := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app", Access: v1.Access_ACCESS_READ_ONLY}))).Msg.Token

	status, res := e.pipeline("/db/acme/app", full, "CREATE TABLE t(a)", "INSERT INTO t VALUES (1), (2)", "SELECT sum(a) FROM t")
	if status != 200 || res[0].Type != "ok" || res[1].Type != "ok" || res[2].Response.Result.Rows[0][0].Value != "3" {
		t.Fatalf("full: %d %+v", status, res)
	}

	if status, _ := e.pipeline("/db/acme/app", "", "SELECT 1"); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	other := must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "other"})))
	if status, _ := e.pipeline("/db/acme/other", full, "SELECT 1"); status != http.StatusUnauthorized {
		t.Fatalf("token for another database: %d (%s)", status, other.Msg.Database.Name)
	}

	status, res = e.pipeline("/db/acme/app", ro,
		"SELECT count(*) FROM t",
		"INSERT INTO t VALUES (3)",
		"CREATE TABLE u(a)",
		"PRAGMA query_only = 0",
		"DELETE FROM t",
		"ATTACH '"+filepath.Join(t.TempDir(), "x.musq")+"' AS x",
		"SELECT count(*) FROM t")
	if status != 200 || res[0].Type != "ok" || res[0].Response.Result.Rows[0][0].Value != "2" {
		t.Fatalf("read-only read: %d %+v", status, res)
	}
	for i := 1; i <= 5; i++ {
		if res[i].Type != "error" {
			t.Errorf("read-only statement %d was not refused: %+v", i, res[i])
		}
	}
	if res[6].Response.Result.Rows[0][0].Value != "2" {
		t.Fatalf("read-only token changed the data: %+v", res[6])
	}

	// A full token may not reach files on the server either.
	_, res = e.pipeline("/db/acme/app", full, "VACUUM INTO '"+filepath.Join(t.TempDir(), "y.musq")+"'")
	if res[0].Type != "error" {
		t.Fatalf("VACUUM INTO allowed: %+v", res[0])
	}

	// A viewer's own token reads but does not write.
	_, res = e.pipeline("/db/acme/app", bob, "SELECT count(*) FROM t", "INSERT INTO t VALUES (9)")
	if res[0].Type != "ok" || res[1].Type != "error" {
		t.Fatalf("viewer token: %+v", res)
	}

	// Renaming keeps tokens; invalidating ends them.
	must(dbs.Rename(ctx, auth(e.admin, &v1.RenameDatabaseRequest{Org: "acme", Name: "app", NewName: "app2"})))
	if status, _ := e.pipeline("/db/acme/app2", full, "SELECT 1"); status != 200 {
		t.Fatalf("after rename: %d", status)
	}
	must(dbs.InvalidateTokens(ctx, auth(e.admin, &v1.InvalidateTokensRequest{Org: "acme", Name: "app2"})))
	if status, _ := e.pipeline("/db/acme/app2", full, "SELECT 1"); status != http.StatusUnauthorized {
		t.Fatalf("invalidated token: %d", status)
	}

	// Group tokens follow the group's generation.
	grp := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Group: "default"}))).Msg.Token
	if status, _ := e.pipeline("/db/acme/app2", grp, "SELECT 1"); status != 200 {
		t.Fatalf("group token: %d", status)
	}
	must(dbs.InvalidateTokens(ctx, auth(e.admin, &v1.InvalidateTokensRequest{Org: "acme", Group: "default"})))
	if status, _ := e.pipeline("/db/acme/app2", grp, "SELECT 1"); status != http.StatusUnauthorized {
		t.Fatalf("invalidated group token: %d", status)
	}

	// A deleted database's token reaches nothing, not even a new database
	// of the same name.
	fresh := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app2"}))).Msg.Token
	must(dbs.Delete(ctx, auth(e.admin, &v1.DeleteDatabaseRequest{Org: "acme", Name: "app2"})))
	if status, _ := e.pipeline("/db/acme/app2", fresh, "SELECT 1"); status != http.StatusNotFound {
		t.Fatalf("deleted database: %d", status)
	}
	must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "app2"})))
	if status, _ := e.pipeline("/db/acme/app2", fresh, "SELECT 1"); status != http.StatusUnauthorized {
		t.Fatalf("old token on a recreated database: %d", status)
	}
	// An expired token is refused.
	exp := must(e.s.Provider().Sign(&oidc.Claims{Claims: jwt.Claims{Subject: "x", Expiry: jwt.NewNumericDate(time.Now().Add(-time.Hour))}, Use: oidc.UseDB, Access: accessFull}))
	if status, _ := e.pipeline("/db/acme/app2", exp, "SELECT 1"); status != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", status)
	}
}

func TestExportAndUpload(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	_, _, dbs := e.clients()
	full := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: "app"}))).Msg.Token
	e.pipeline("/db/acme/app", full, "CREATE TABLE t(a TEXT)", "INSERT INTO t VALUES ('hello')")

	for _, f := range []v1.FileFormat{v1.FileFormat_FILE_FORMAT_MUSQL, v1.FileFormat_FILE_FORMAT_SQLITE} {
		content := e.export("acme", "app", f)
		isSQLite := bytes.HasPrefix(content, sqliteMagic)
		if isSQLite != (f == v1.FileFormat_FILE_FORMAT_SQLITE) {
			t.Fatalf("%v: wrong file format", f)
		}
		up := e.upload("acme", content)
		if up.Format != f || up.SizeBytes != int64(len(content)) {
			t.Fatalf("%v: finished as %v, %d bytes", f, up.Format, up.SizeBytes)
		}
		name := "copy-" + strings.ToLower(strings.TrimPrefix(f.String(), "FILE_FORMAT_"))
		must(dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: name, Source: &v1.CreateDatabaseRequest_Upload{Upload: up.Upload}})))
		tok := must(dbs.CreateToken(ctx, auth(e.admin, &v1.CreateTokenRequest{Org: "acme", Name: name}))).Msg.Token
		_, res := e.pipeline("/db/acme/"+name, tok, "SELECT a FROM t")
		if res[0].Type != "ok" || res[0].Response.Result.Rows[0][0].Value != "hello" {
			t.Fatalf("%s: %+v", name, res)
		}
		// Creating consumed the upload.
		if _, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: name + "-again", Source: &v1.CreateDatabaseRequest_Upload{Upload: up.Upload}})); code(err) != connect.CodeInvalidArgument {
			t.Fatalf("%s: an upload used twice: %v", name, err)
		}
	}

	up := e.upload("acme", []byte("not a database"))
	if _, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "acme", Name: "junk", Source: &v1.CreateDatabaseRequest_Upload{Upload: up.Upload}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("junk upload: %v", err)
	}
	if _, err := dbs.Get(ctx, auth(e.admin, &v1.GetDatabaseRequest{Org: "acme", Name: "junk"})); code(err) != connect.CodeNotFound {
		t.Fatalf("junk database recorded: %v", err)
	}
	// An upload belongs to its org.
	must((cloudv1connect.NewOrgServiceClient(e.ts.Client(), e.ts.URL)).Create(ctx, auth(e.admin, &v1.CreateOrgRequest{Slug: "other"})))
	up = e.upload("acme", e.export("acme", "app", v1.FileFormat_FILE_FORMAT_MUSQL))
	if _, err := dbs.Create(ctx, auth(e.admin, &v1.CreateDatabaseRequest{Org: "other", Name: "stolen", Source: &v1.CreateDatabaseRequest_Upload{Upload: up.Upload}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("cross-org upload: %v", err)
	}
}

func TestInit(t *testing.T) {
	dir := t.TempDir()
	cfg := must(ParseConfig([]byte("data_dir: " + dir + "\n")))
	ctx := context.Background()
	if _, err := New(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), "no admin: run musql-cloud init --admin-email") {
		t.Fatalf("started without an admin: %v", err)
	}
	if err := Init(ctx, cfg, "root@example.com", ""); err == nil {
		t.Fatal("accepted an empty password")
	}
	if err := Init(ctx, cfg, "Root@example.com", "first-password"); err != nil {
		t.Fatal(err)
	}
	s := must(New(ctx, cfg, nil))
	u := must(s.Store().UserByEmail(ctx, "root@example.com"))
	if !u.Admin {
		t.Fatalf("not an admin: %+v", u)
	}
	u.Admin, u.Disabled = false, true
	if err := s.Store().UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if err := Init(ctx, cfg, "root@example.com", "second-password"); err != nil {
		t.Fatal(err)
	}
	s = must(New(ctx, cfg, nil))
	defer s.Close()
	if _, err := s.Provider().Authenticate(ctx, "root@example.com", "second-password"); err != nil {
		t.Fatalf("reset password: %v", err)
	}
	if _, err := s.Provider().Authenticate(ctx, "root@example.com", "first-password"); err == nil {
		t.Fatal("old password still works")
	}
	if u := must(s.Store().UserByEmail(ctx, "root@example.com")); !u.Admin || u.Disabled {
		t.Fatalf("admin not restored: %+v", u)
	}
}

func TestConfigRejectsAdminBlock(t *testing.T) {
	_, err := ParseConfig([]byte("admin: {email: a@b.c, password: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "run musql-cloud init") {
		t.Fatalf("%v", err)
	}
}

func TestConfig(t *testing.T) {
	for _, bad := range []string{
		"unknown_key: 1",
		"public_url: ftp://x",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	c := must(ParseConfig([]byte(`
ai:
  provider: jitllm
  model: slm-sql-1.5b
  jitllm: {models_dir: ./models, embedded: true}
`)))
	if c.Listen != ":8080" || c.PublicURL != "http://localhost:8080" || c.AI.Provider != "jitllm" || c.AI.JITLLM.Embedded == nil || !*c.AI.JITLLM.Embedded {
		t.Fatalf("%+v", c)
	}
}

func TestConsoleAndCORS(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>console</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("js"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, "console:\n  dir: "+dir+"\n  cors_origins: [\"http://localhost:5173\"]\n")
	get := func(p string) string {
		resp := must(e.ts.Client().Get(e.ts.URL + p))
		defer resp.Body.Close()
		return string(must(io.ReadAll(resp.Body)))
	}
	if get("/console/app.js") != "js" || get("/console/dbs/acme") != "<html>console</html>" {
		t.Fatal("console serving")
	}
	pre := func(origin string) string {
		req, _ := http.NewRequest("OPTIONS", e.ts.URL+"/cloud.v1.AuthService/WhoAmI", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		resp := must(e.ts.Client().Do(req))
		resp.Body.Close()
		return resp.Header.Get("Access-Control-Allow-Origin")
	}
	if pre("http://localhost:5173") != "http://localhost:5173" || pre("https://evil.example") != "" {
		t.Fatal("CORS")
	}
}
