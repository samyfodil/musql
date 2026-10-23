package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractSQL(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1; SELECT 2;":                       "SELECT 1;",
		"```sql\nSELECT ';' AS x;\n```\nthat is it": "SELECT ';' AS x;",
		"<think>a; b</think>\n```\nSELECT 3\n```":   "SELECT 3",
		"  SELECT 4 -- c;\n;":                       "SELECT 4 -- c;\n;",
	} {
		if got := ExtractSQL(in); got != want {
			t.Errorf("ExtractSQL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenAIShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"SELECT 1;"}}]}`))
	}))
	defer srv.Close()
	for _, p := range []string{"openai", "openrouter", "jitllm"} {
		g, err := New(Config{Provider: p, Model: "m", APIKey: "k", BaseURL: srv.URL + "/v1"})
		if err != nil {
			t.Fatal(err)
		}
		out, err := g.Generate(context.Background(), "sys", "usr")
		if err != nil || out != "SELECT 1;" {
			t.Fatalf("%s: %q %v", p, out, err)
		}
		msgs := got["messages"].([]any)
		if got["model"] != "m" || len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "sys" {
			t.Errorf("%s: request %v", p, got)
		}
	}
}

func TestAnthropicShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("bad request %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"SELECT 2;"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	g, err := New(Config{Provider: "anthropic", Model: "m", APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.Generate(context.Background(), "sys", "usr")
	if err != nil || out != "SELECT 2;" {
		t.Fatalf("%q %v", out, err)
	}
	if got["model"] != "m" || got["max_tokens"] == nil || !strings.Contains(string(mustJSON(got["system"])), "sys") {
		t.Errorf("request %v", got)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestUnknownAndEmbeddedOff(t *testing.T) {
	if _, err := New(Config{Provider: "x"}); err == nil {
		t.Error("unknown provider accepted")
	}
	if _, err := New(Config{Provider: "jitllm", Model: "m"}); err == nil {
		t.Error("jitllm without base_url accepted")
	}
}
