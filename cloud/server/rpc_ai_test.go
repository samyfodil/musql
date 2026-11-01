package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

type fakeGen struct {
	replies []string
	prompts []string
	err     error
}

func (f *fakeGen) Generate(_ context.Context, system, user string) (string, error) {
	f.prompts = append(f.prompts, user)
	if f.err != nil {
		return "", f.err
	}
	r := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return r, nil
}

func TestGenerateSQL(t *testing.T) {
	e := newEnv(t, "")
	bob := e.setup()
	ctx := context.Background()
	ai := cloudv1connect.NewAIServiceClient(e.ts.Client(), e.ts.URL)
	req := func(tok, prompt string) *connect.Request[v1.GenerateSQLRequest] {
		return auth(tok, &v1.GenerateSQLRequest{Org: "acme", Database: "app", Prompt: prompt})
	}

	if _, err := ai.GenerateSQL(ctx, req(e.admin, "x")); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("not configured: %v", err)
	}

	e.exec("/db/acme/app", e.admin, "CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT)", "INSERT INTO t(name) VALUES ('secret-row')")
	g := &fakeGen{replies: []string{"Counts them.\n```sql\nSELECT count(*) FROM t;\n```"}}
	e.s.gen = g

	if _, err := ai.GenerateSQL(ctx, req(bob, "count rows")); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer: %v", err)
	}
	res := must(ai.GenerateSQL(ctx, req(e.admin, "count rows")))
	if res.Msg.Sql != "SELECT count(*) FROM t;" || res.Msg.Writes || res.Msg.Explanation != "Counts them." {
		t.Fatalf("read: %+v", res.Msg)
	}
	if !strings.Contains(g.prompts[0], "CREATE TABLE t(") || strings.Contains(g.prompts[0], "secret-row") {
		t.Fatalf("prompt: %q", g.prompts[0])
	}

	g.replies = []string{"```sql\nDELETE FROM t WHERE id = 1;\n```"}
	res = must(ai.GenerateSQL(ctx, req(e.admin, "remove one")))
	if !res.Msg.Writes {
		t.Fatalf("delete not labelled: %+v", res.Msg)
	}
	if got := e.exec("/db/acme/app", e.admin, "SELECT count(*) FROM t"); got[0].Response.Result.Rows[0][0].Value != "1" {
		t.Fatalf("generated SQL ran: %+v", got)
	}

	g.replies = []string{"```sql\nSELECT nope FROM t;\n```", "```sql\nSELECT name FROM t;\n```"}
	g.prompts = nil
	res = must(ai.GenerateSQL(ctx, req(e.admin, "names")))
	if res.Msg.Sql != "SELECT name FROM t;" || len(g.prompts) != 2 || !strings.Contains(g.prompts[1], "failed") {
		t.Fatalf("repair: %+v %q", res.Msg, g.prompts)
	}

	g.replies = []string{"```sql\nSELECT nope FROM t;\n```"}
	if _, err := ai.GenerateSQL(ctx, req(e.admin, "bad")); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("invalid SQL: %v", err)
	}
}

func TestAIEmbeddedUnavailable(t *testing.T) {
	e := newEnv(t, "ai:\n  provider: jitllm\n")
	e.setup()
	ai := cloudv1connect.NewAIServiceClient(e.ts.Client(), e.ts.URL)
	_, err := ai.GenerateSQL(context.Background(), auth(e.admin, &v1.GenerateSQLRequest{Org: "acme", Database: "app", Prompt: "x"}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("embedded without the tag: %v", err)
	}
}
