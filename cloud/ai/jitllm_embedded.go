//go:build jitllm

package ai

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jitllm/jitllm/engine/model"
)

type embedded struct {
	path string
	max  int

	mu   sync.Mutex
	m    *model.Model
	err  error
	once sync.Once
}

func newEmbedded(cfg Config) (Generator, error) {
	p := cfg.Model
	if !strings.HasSuffix(p, ".jlm") {
		p += ".jlm"
	}
	if !filepath.IsAbs(p) {
		if _, err := os.Stat(p); err != nil && cfg.ModelsDir != "" {
			p = filepath.Join(cfg.ModelsDir, p)
		}
	}
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("ai: jitllm model: %w", err)
	}
	return &embedded{path: p, max: cfg.MaxTokens}, nil
}

func (e *embedded) open() (*model.Model, error) {
	e.once.Do(func() { e.m, e.err = model.Open(e.path) })
	return e.m, e.err
}

// done reports whether text holds a whole statement, ignoring a <think>
// section still being written.
func done(text string) bool {
	if i := strings.LastIndex(text, "</think>"); i >= 0 {
		text = text[i+len("</think>"):]
	} else if strings.Contains(text, "<think>") {
		return false
	}
	if i := strings.Index(text, "```"); i >= 0 {
		text = text[i+3:]
		if strings.Contains(text, "```") {
			return true
		}
	}
	_, ok := CutSQL(text)
	return ok
}

func (e *embedded) Generate(ctx context.Context, system, user string) (string, error) {
	m, err := e.open()
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ids, err := m.ChatIDs([]model.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}, true)
	if err != nil {
		return "", err
	}
	budget := len(ids) + e.max
	st := m.NewState(budget)
	defer st.Close()
	logits, err := st.Prefill(ids)
	var out []int32
	for err == nil && st.Pos() < budget {
		if err = ctx.Err(); err != nil {
			break
		}
		id := model.Greedy(logits)
		if m.Vocab.IsEOG(id) {
			break
		}
		out = append(out, id)
		if done(m.Vocab.Decode(out)) {
			break
		}
		logits, err = st.Forward(id)
	}
	if err != nil {
		return "", err
	}
	return m.Vocab.Decode(out), nil
}
