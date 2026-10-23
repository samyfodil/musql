// Package ai turns a schema and a request into SQL through one of several
// providers. It reads no environment variables: everything comes from Config.
package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Generator answers a system and a user message with text.
type Generator interface {
	Generate(ctx context.Context, system, user string) (string, error)
}

// Config selects and configures a provider.
type Config struct {
	Provider  string // jitllm | openai | anthropic | openrouter
	Model     string
	APIKey    string
	BaseURL   string
	ModelsDir string
	Embedded  bool // jitllm: run the model in-process instead of calling BaseURL
	MaxTokens int  // reply budget; 0 means 1024
}

const (
	defaultMaxTokens = 1024
	openRouterURL    = "https://openrouter.ai/api/v1"
)

// New builds the provider cfg names.
func New(cfg Config) (Generator, error) {
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaultMaxTokens
	}
	switch cfg.Provider {
	case "openai":
		return newOpenAI(cfg, cfg.BaseURL), nil
	case "openrouter":
		u := cfg.BaseURL
		if u == "" {
			u = openRouterURL
		}
		return newOpenAI(cfg, u), nil
	case "anthropic":
		return newAnthropic(cfg), nil
	case "jitllm":
		if cfg.Embedded {
			return newEmbedded(cfg)
		}
		if cfg.BaseURL == "" {
			return nil, errors.New("ai: jitllm without embedded needs base_url")
		}
		return newOpenAI(cfg, cfg.BaseURL), nil
	}
	return nil, fmt.Errorf("ai: unknown provider %q", cfg.Provider)
}

// CutSQL returns s up to and including its first statement-ending semicolon
// (one outside quotes and comments), and whether there was one.
func CutSQL(s string) (string, bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '[':
			if j := strings.IndexByte(s[i:], ']'); j > 0 {
				i += j
			}
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return s, false
			}
			i += j
		case c == ';':
			return s[:i+1], true
		}
	}
	return s, false
}

// ExtractSQL pulls the SQL out of a model reply: the first fenced block if
// there is one, else the text after any <think> section, cut at the end of
// the first statement.
func ExtractSQL(reply string) string {
	if i := strings.LastIndex(reply, "</think>"); i >= 0 {
		reply = reply[i+len("</think>"):]
	}
	if i := strings.Index(reply, "```"); i >= 0 {
		body := reply[i+3:]
		if nl := strings.IndexByte(body, '\n'); nl >= 0 && !strings.ContainsAny(body[:nl], " ;()") {
			body = body[nl+1:]
		}
		if j := strings.Index(body, "```"); j >= 0 {
			body = body[:j]
		}
		reply = body
	}
	reply = strings.TrimSpace(reply)
	if cut, ok := CutSQL(reply); ok {
		reply = cut
	}
	return strings.TrimSpace(reply)
}
