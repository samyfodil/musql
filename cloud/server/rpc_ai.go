package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/cloud/ai"
	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/cloud/gen/cloud/v1/cloudv1connect"
	"github.com/samyfodil/musql/cloud/state"
)

const (
	maxAIPrompt  = 4000
	maxAISchema  = 24000
	aiTimeout    = 3 * time.Minute
	defaultModel = "slm-sql-1.5b"
)

const aiSystem = `You write SQLite queries. Reply with exactly one SQL statement in a sql code fence and nothing else. Use only the tables and columns of the schema. Prefer a read-only SELECT unless the request asks to change data.`

type aiService struct {
	*Server
	cloudv1connect.UnimplementedAIServiceHandler
}

// generator builds the configured provider on first use, so a server whose
// provider cannot start (an embedded model that is missing) still serves
// everything else.
func (s *Server) generator() (ai.Generator, error) {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	if s.gen != nil {
		return s.gen, nil
	}
	c := s.cfg.AI
	if c.Provider == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("AI SQL is not configured on this server"))
	}
	model := c.Model
	if model == "" && c.Provider == "jitllm" {
		model = defaultModel
	}
	embedded := c.JITLLM.Embedded == nil || *c.JITLLM.Embedded
	g, err := ai.New(ai.Config{
		Provider: c.Provider, Model: model, APIKey: c.APIKey, BaseURL: c.BaseURL,
		ModelsDir: c.JITLLM.ModelsDir, Embedded: embedded,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	s.gen = g
	return g, nil
}

func (s aiService) GenerateSQL(ctx context.Context, req *connect.Request[v1.GenerateSQLRequest]) (*connect.Response[v1.GenerateSQLResponse], error) {
	m := req.Msg
	_, o, _, err := s.org(ctx, req.Header(), m.Org, state.RoleMember)
	if err != nil {
		return nil, err
	}
	d, err := s.store.Database(ctx, o.ID, m.Database)
	if err != nil {
		return nil, rpcErr(err)
	}
	prompt := strings.TrimSpace(m.Prompt)
	if prompt == "" {
		return nil, invalid("prompt is empty")
	}
	if len(prompt) > maxAIPrompt {
		return nil, invalid("prompt is too long")
	}
	gen, err := s.generator()
	if err != nil {
		return nil, err
	}
	e, release, err := s.pool.get(d)
	if err != nil {
		return nil, restoreErr(err)
	}
	defer release()
	schema, err := schemaText(ctx, e.ro)
	if err != nil {
		return nil, rpcErr(err)
	}
	ctx, cancel := context.WithTimeout(ctx, aiTimeout)
	defer cancel()

	user := "Database schema:\n" + schema + "\nRequest: " + prompt
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		reply, err := gen.Generate(ctx, aiSystem, user)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("AI provider: %w", err))
		}
		q := ai.ExtractSQL(reply)
		if q == "" {
			lastErr = errors.New("the model returned no SQL")
			continue
		}
		writes, err := checkSQL(ctx, e.ro, q)
		if err != nil {
			lastErr = fmt.Errorf("%w: %s", err, q)
			user += "\nYour previous answer failed: " + err.Error() + "\nAnswer: " + q + "\nFix it."
			continue
		}
		return connect.NewResponse(&v1.GenerateSQLResponse{Sql: q, Explanation: explanation(reply, q), Writes: writes}), nil
	}
	return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("generated SQL does not compile: %w", lastErr))
}

// schemaText is the database's CREATE statements, nothing else: no rows.
func schemaText(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		AND name NOT IN (SELECT name FROM pragma_table_list WHERE type = 'shadow') ORDER BY rowid`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			return "", err
		}
		if b.Len()+len(q) > maxAISchema {
			b.WriteString("-- schema truncated\n")
			break
		}
		b.WriteString(q)
		b.WriteString(";\n")
	}
	return b.String(), rows.Err()
}

// checkSQL compiles q with EXPLAIN on a read-only handle, so nothing runs and
// nothing can be written, and reports whether q would write.
func checkSQL(ctx context.Context, ro *sql.DB, q string) (writes bool, err error) {
	if cut, ok := ai.CutSQL(q); ok && strings.TrimSpace(q[len(cut):]) != "" {
		return false, errors.New("more than one statement")
	}
	rows, err := ro.QueryContext(ctx, "EXPLAIN "+strings.TrimSuffix(strings.TrimSpace(q), ";"))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return false, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return false, err
		}
		if len(vals) > 3 && fmt.Sprint(vals[1]) == "Transaction" && fmt.Sprint(vals[3]) != "0" {
			writes = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return writes || !readsOnly(q), nil
}

// readsOnly is the keyword check behind the label: the opcodes catch what
// writes, this catches statements that change state without a transaction.
func readsOnly(q string) bool {
	f := strings.Fields(strings.ToUpper(q))
	if len(f) == 0 {
		return true
	}
	switch strings.TrimLeft(f[0], "(") {
	case "SELECT", "WITH", "VALUES", "EXPLAIN":
		return true
	}
	return false
}

// explanation is the prose a model wrote around its SQL, if any.
func explanation(reply, q string) string {
	if i := strings.LastIndex(reply, "</think>"); i >= 0 {
		reply = reply[i+len("</think>"):]
	}
	if i := strings.Index(reply, "```"); i >= 0 {
		rest := reply[i+3:]
		if j := strings.Index(rest, "```"); j >= 0 {
			reply = reply[:i] + rest[j+3:]
		} else {
			reply = reply[:i]
		}
	} else {
		reply = strings.Replace(reply, q, "", 1)
	}
	return strings.TrimSpace(reply)
}
