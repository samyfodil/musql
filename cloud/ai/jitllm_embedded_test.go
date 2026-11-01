//go:build jitllm

package ai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestEmbeddedGenerate runs the real model; it needs MUSQL_AI_MODEL, the path
// of a converted SLM-SQL .jlm.
func TestEmbeddedGenerate(t *testing.T) {
	path := os.Getenv("MUSQL_AI_MODEL")
	if path == "" {
		t.Skip("MUSQL_AI_MODEL is not set")
	}
	g, err := New(Config{Provider: "jitllm", Embedded: true, Model: path, MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	out, err := g.Generate(ctx,
		"You write SQLite queries. Reply with exactly one SQL statement in a sql code fence and nothing else.",
		"Database schema:\nCREATE TABLE employees(id INTEGER PRIMARY KEY, name TEXT, dept TEXT, salary REAL);\nRequest: average salary per department")
	if err != nil {
		t.Fatal(err)
	}
	sql := ExtractSQL(out)
	t.Logf("%v\nreply: %q\nsql: %s", time.Since(start), out, sql)
	if u := strings.ToUpper(sql); !strings.HasPrefix(u, "SELECT") || !strings.Contains(u, "AVG") || !strings.Contains(u, "GROUP BY") {
		t.Fatalf("unexpected SQL: %s", sql)
	}
}
