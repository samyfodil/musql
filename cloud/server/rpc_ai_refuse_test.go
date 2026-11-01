package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
)

// TestGenerateSQLRefusals: what the AI service refuses before and after asking
// the model, the prose it keeps from a reply, and a schema too long to send
// whole.
func TestGenerateSQLRefusals(t *testing.T) {
	e := newEnv(t, "")
	e.setup()
	ctx := context.Background()
	ai := cloudv1connect.NewAIServiceClient(e.ts.Client(), e.ts.URL)
	gen := func(db, prompt string) (*connect.Response[v1.GenerateSQLResponse], error) {
		return ai.GenerateSQL(ctx, auth(e.admin, &v1.GenerateSQLRequest{Org: "acme", Database: db, Prompt: prompt}))
	}
	g := &fakeGen{replies: []string{"SELECT 1;"}}
	e.s.gen = g

	for name, tc := range map[string]struct {
		db, prompt string
		want       connect.Code
	}{
		"no such database":  {"nope", "x", connect.CodeNotFound},
		"an empty prompt":   {"app", "  ", connect.CodeInvalidArgument},
		"a prompt too long": {"app", strings.Repeat("x", maxAIPrompt+1), connect.CodeInvalidArgument},
	} {
		if _, err := gen(tc.db, tc.prompt); code(err) != tc.want {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}

	g.err = errors.New("the provider is down")
	if _, err := gen("app", "x"); code(err) != connect.CodeUnavailable {
		t.Fatalf("a provider error: %v", err)
	}
	g.err = nil
	g.replies = []string{"I cannot help with that."}
	if _, err := gen("app", "x"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("a reply without SQL: %v", err)
	}

	for reply, want := range map[string][2]string{
		"<think>hmm</think>Ones.\n```sql\nSELECT 1;": {"SELECT 1;", "Ones."},
		"SELECT 2;": {"SELECT 2;", ""},
	} {
		g.replies = []string{reply}
		res := must(gen("app", "x"))
		if res.Msg.Sql != want[0] || res.Msg.Explanation != want[1] {
			t.Errorf("%q: %q, %q", reply, res.Msg.Sql, res.Msg.Explanation)
		}
	}

	var ddl []string
	cols := make([]string, 90)
	for i := range cols {
		cols[i] = fmt.Sprintf("c%02d INTEGER", i)
	}
	for i := range 25 {
		ddl = append(ddl, fmt.Sprintf("CREATE TABLE wide%02d(%s)", i, strings.Join(cols, ", ")))
	}
	e.exec("/db/acme/app", e.admin, ddl...)
	g.replies, g.prompts = []string{"SELECT 1;"}, nil
	must(gen("app", "x"))
	if !strings.Contains(g.prompts[0], "-- schema truncated") || len(g.prompts[0]) > maxAISchema+maxAIPrompt {
		t.Fatalf("a long schema is sent whole: %d bytes", len(g.prompts[0]))
	}
}

// TestGenerateSQLThroughOpenAI: a configured provider is built on first use and
// asked over its API.
func TestGenerateSQLThroughOpenAI(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"finish_reason":"stop",`+
			`"message":{"role":"assistant","content":"Ones.\n`+"```sql\\nSELECT 1;\\n```"+`"}}]}`)
	}))
	defer fake.Close()
	e := newEnv(t, "ai:\n  provider: openai\n  model: m\n  api_key: k\n  base_url: "+fake.URL+"\n")
	e.setup()
	ai := cloudv1connect.NewAIServiceClient(e.ts.Client(), e.ts.URL)
	res := must(ai.GenerateSQL(context.Background(), auth(e.admin, &v1.GenerateSQLRequest{Org: "acme", Database: "app", Prompt: "one"})))
	if res.Msg.Sql != "SELECT 1;" || res.Msg.Explanation != "Ones." {
		t.Fatalf("%+v", res.Msg)
	}
}
