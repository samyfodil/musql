package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/tursodatabase/libsql-client-go/libsql"
	"golang.org/x/term"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
	"github.com/samyfodil/musql/engine"
)

// shell opens a database over Hrana with a short-lived token and runs the
// SQL argument, a piped script, or the REPL.
func (a *app) shell(ctx context.Context, args []string) error {
	d, err := a.get(ctx, args[0])
	if err != nil {
		return err
	}
	tok, err := a.dbToken(ctx, d)
	if err != nil {
		return err
	}
	conn, err := libsql.NewConnector(d.Url, libsql.WithAuthToken(tok))
	if err != nil {
		return err
	}
	db := sql.OpenDB(conn)
	defer db.Close()
	return a.runShell(ctx, db, args[1:], fmt.Sprintf("Connected to %s/%s.", d.Org, d.Name))
}

// runShell runs the SQL argument, a piped script, or the REPL on db, the REPL
// greeting with banner.
func (a *app) runShell(ctx context.Context, db *sql.DB, script []string, banner string) error {
	sh := &Shell{DB: db, Out: a.env.Stdout, Mode: "table"}
	if a.json {
		sh.Mode = "json"
	}
	switch {
	case len(script) == 1:
		return sh.Script(ctx, script[0])
	case !a.env.Interactive:
		// Piped input is read as typed lines, dot-commands and all, and goes on
		// past an error as C's shell does; the exit status still says one failed.
		if _, err := sh.run(ctx, &readerLines{r: bufio.NewReader(a.env.Stdin)}, a.env.Stderr, true); err != nil {
			return err
		}
		if sh.failed > 0 {
			return fmt.Errorf("%d error(s) in the input", sh.failed)
		}
		return nil
	}
	old, err := term.MakeRaw(a.env.StdinFd)
	if err != nil {
		return err
	}
	defer term.Restore(a.env.StdinFd, old)
	defer consoleVT(a.env.StdoutFd)()
	t := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{a.env.Stdin, a.env.Stdout}, "")
	t.History = loadHistory(filepath.Join(a.env.Dir, "history"))
	sh.Out = t
	fmt.Fprintf(t, "%s End statements with ;  .help for commands.\n", banner)
	return sh.REPL(ctx, sizedTerminal{t, a.env.StdoutFd}, t)
}

// sizedTerminal is the line editor fitted to the terminal before each line:
// it wraps what is typed at the width it was given, 80 columns unless told
// (#101), and the window may have been resized since the last line.
type sizedTerminal struct {
	*term.Terminal
	fd int
}

func (t sizedTerminal) ReadLine() (string, error) {
	if w, h, err := term.GetSize(t.fd); err == nil {
		t.SetSize(w, h)
	}
	return t.Terminal.ReadLine()
}

// dbToken mints a one-hour token for d: full access when the caller's role
// allows it, else read-only.
func (a *app) dbToken(ctx context.Context, d *v1.Database) (string, error) {
	exp := timestamppb.New(time.Now().Add(time.Hour))
	req := &v1.CreateTokenRequest{Org: d.Org, Name: d.Name, Access: v1.Access_ACCESS_FULL, ExpiresAt: exp}
	r, err := a.services.db.CreateToken(ctx, connect.NewRequest(req))
	if connect.CodeOf(err) == connect.CodePermissionDenied {
		req.Access = v1.Access_ACCESS_READ_ONLY
		r, err = a.services.db.CreateToken(ctx, connect.NewRequest(req))
	}
	if err != nil {
		return "", err
	}
	return r.Msg.Token, nil
}

// LineReader is what the REPL reads from: a terminal with editing and
// history, or plain lines.
type LineReader interface {
	ReadLine() (string, error)
	SetPrompt(string)
}

// Shell runs SQL and dot-commands against a database and prints results.
type Shell struct {
	DB     *sql.DB
	Out    io.Writer
	Mode   string // table, csv or json
	Timer  bool   // print each statement's run time
	reads  int    // .read files open, one inside another
	failed int    // statements and dot-commands that reported an error
}

const (
	prompt     = "musql> "
	contPrompt = "   ...> "
)

// REPL reads statements until .quit or end of input; errors are printed to
// errw and do not stop it.
func (s *Shell) REPL(ctx context.Context, in LineReader, errw io.Writer) error {
	_, err := s.run(ctx, in, errw, false)
	return err
}

// run is REPL, reporting whether a .quit ended it. With finish, a statement
// left unterminated at the end of input runs, as the last one of a file
// does.
func (s *Shell) run(ctx context.Context, in LineReader, errw io.Writer, finish bool) (quit bool, err error) {
	var buf strings.Builder
	in.SetPrompt(prompt)
	for {
		line, err := in.ReadLine()
		if errors.Is(err, io.EOF) {
			if finish && strings.TrimSpace(buf.String()) != "" {
				if err := s.Script(ctx, buf.String()); err != nil {
					s.report(errw, err)
				}
			}
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			quit, err := s.meta(ctx, strings.TrimSpace(line), errw)
			if err != nil {
				s.report(errw, err)
			}
			if quit {
				return true, nil
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		if !Complete(buf.String()) {
			if strings.TrimSpace(buf.String()) == "" {
				buf.Reset()
			} else {
				in.SetPrompt(contPrompt)
			}
			continue
		}
		if err := s.Script(ctx, buf.String()); err != nil {
			s.report(errw, err)
		}
		buf.Reset()
		in.SetPrompt(prompt)
	}
}

// Complete reports whether text ends with a finished statement: a ";" outside
// a literal, comment or trigger body, trailing comments allowed. The engine's
// splitter decides, so the shell and the server agree on boundaries.
func Complete(text string) bool {
	const probe = "musql_probe"
	without, err := engine.SplitStatements(text)
	if err != nil || len(without) == 0 {
		return false
	}
	with, err := engine.SplitStatements(text + "\n" + probe)
	if err != nil {
		return false
	}
	return len(with) == len(without)+1 && with[len(with)-1] == probe
}

// Script runs every statement in text, stopping at the first error.
func (s *Shell) Script(ctx context.Context, text string) error {
	stmts, err := engine.SplitStatements(text)
	if err != nil {
		return err
	}
	for _, st := range stmts {
		if err := s.Exec(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// Exec runs one statement and prints its rows, if it has any.
func (s *Shell) Exec(ctx context.Context, stmt string, args ...any) error {
	if s.Timer {
		defer func(start time.Time) {
			fmt.Fprintf(s.Out, "Run Time: real %.3f\n", time.Since(start).Seconds())
		}(time.Now())
	}
	rows, err := s.DB.QueryContext(ctx, stmt, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	var data [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		data = append(data, vals)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	return s.print(cols, data)
}

func (s *Shell) print(cols []string, data [][]any) error {
	switch s.Mode {
	case "csv":
		w := csv.NewWriter(s.Out)
		w.Write(cols)
		for _, r := range data {
			rec := make([]string, len(r))
			for i, v := range r {
				if v != nil {
					rec[i] = cell(v)
				}
			}
			w.Write(rec)
		}
		w.Flush()
		return w.Error()
	case "json":
		return writeRowsJSON(s.Out, cols, data)
	}
	rows := make([][]string, len(data))
	for i, r := range data {
		rows[i] = make([]string, len(r))
		for j, v := range r {
			rows[i][j] = cell(v)
		}
	}
	writeTable(s.Out, cols, rows)
	return nil
}

func cell(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case float64:
		return engine.FormatReal(v)
	case []byte:
		return string(v)
	case time.Time:
		return v.Format(time.RFC3339Nano)
	}
	return fmt.Sprint(v)
}

// writeRowsJSON prints rows as an array of objects, keys in column order.
func writeRowsJSON(w io.Writer, cols []string, data [][]any) error {
	var b strings.Builder
	b.WriteString("[")
	for i, r := range data {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n  {")
		for j, v := range r {
			if j > 0 {
				b.WriteString(", ")
			}
			k, err := json.Marshal(cols[j])
			if err != nil {
				return err
			}
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			val, err := json.Marshal(v)
			if err != nil {
				return err
			}
			b.Write(k)
			b.WriteString(": ")
			b.Write(val)
		}
		b.WriteString("}")
	}
	if len(data) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	_, err := io.WriteString(w, b.String())
	return err
}

const helpText = `.tables              list tables and views
.schema [table]      show CREATE statements
.mode table|csv|json output format
.timer on|off        print each statement's run time
.read FILE           run the SQL and dot-commands in FILE
.quit                leave the shell
`

// readsMax bounds .read files open inside one another, so a file that reads
// itself stops.
const readsMax = 16

// Meta runs a dot-command.
func (s *Shell) Meta(ctx context.Context, line string) (quit bool, err error) {
	return s.meta(ctx, line, s.Out)
}

// meta is Meta, the errors of a .read file's statements printed to errw.
func (s *Shell) meta(ctx context.Context, line string, errw io.Writer) (quit bool, err error) {
	f := strings.Fields(line)
	switch f[0] {
	case ".quit", ".exit", ".q":
		return true, nil
	case ".read":
		name := strings.TrimSpace(strings.TrimPrefix(line, ".read"))
		if len(name) >= 2 && (name[0] == '"' || name[0] == '\'') && name[len(name)-1] == name[0] {
			name = name[1 : len(name)-1]
		}
		if name == "" {
			return false, errors.New("usage: .read FILE")
		}
		return s.read(ctx, name, errw)
	case ".help":
		io.WriteString(s.Out, helpText)
	case ".tables":
		saved := s.Mode
		defer func() { s.Mode = saved }()
		s.Mode = "table"
		return false, s.listTables(ctx)
	case ".schema":
		q := `SELECT sql || ';' FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'`
		var args []any
		if len(f) > 1 {
			q += ` AND tbl_name = ?`
			args = append(args, f[1])
		}
		rows, err := s.DB.QueryContext(ctx, q+` ORDER BY tbl_name, type DESC, name`, args...)
		if err != nil {
			return false, err
		}
		defer rows.Close()
		for rows.Next() {
			var sqlText string
			if err := rows.Scan(&sqlText); err != nil {
				return false, err
			}
			fmt.Fprintln(s.Out, sqlText)
		}
		return false, rows.Err()
	case ".mode":
		if len(f) != 2 || (f[1] != "table" && f[1] != "csv" && f[1] != "json") {
			return false, errors.New("usage: .mode table|csv|json")
		}
		s.Mode = f[1]
	case ".timer":
		if len(f) != 2 || (f[1] != "on" && f[1] != "off") {
			return false, errors.New("usage: .timer on|off")
		}
		s.Timer = f[1] == "on"
	default:
		return false, fmt.Errorf("unknown command %s (.help lists them)", f[0])
	}
	return false, nil
}

// read runs the SQL and dot-commands in the file name, as typed at the
// prompt; a .quit there ends the shell.
func (s *Shell) read(ctx context.Context, name string, errw io.Writer) (quit bool, err error) {
	if s.reads >= readsMax {
		return false, fmt.Errorf(".read %s: more than %d files open one inside another", name, readsMax)
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return false, err
	}
	s.reads++
	defer func() { s.reads-- }()
	return s.run(ctx, &fileLines{lines: strings.Split(string(b), "\n")}, errw, true)
}

// fileLines reads a file's lines as the REPL reads typed ones.
type fileLines struct{ lines []string }

func (f *fileLines) ReadLine() (string, error) {
	if len(f.lines) == 0 {
		return "", io.EOF
	}
	line := strings.TrimSuffix(f.lines[0], "\r")
	f.lines = f.lines[1:]
	return line, nil
}

func (f *fileLines) SetPrompt(string) {}

// readerLines reads piped input's lines as the REPL reads typed ones.
type readerLines struct{ r *bufio.Reader }

func (l *readerLines) ReadLine() (string, error) {
	line, err := l.r.ReadString('\n')
	if err == io.EOF && line != "" {
		err = nil
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), err
}

func (l *readerLines) SetPrompt(string) {}

func (s *Shell) report(errw io.Writer, err error) {
	s.failed++
	fmt.Fprintf(errw, "Error: %v\n", err)
}

func (s *Shell) listTables(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) > 0 {
		fmt.Fprintln(s.Out, strings.Join(names, "  "))
	}
	return nil
}

// fileHistory is a terminal history kept in a file, newest last.
type fileHistory struct {
	path  string
	lines []string
}

const historyMax = 1000

func loadHistory(path string) *fileHistory {
	h := &fileHistory{path: path}
	f, err := os.Open(path)
	if err != nil {
		return h
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		h.lines = append(h.lines, sc.Text())
	}
	return h
}

func (h *fileHistory) Add(entry string) {
	if strings.TrimSpace(entry) == "" {
		return
	}
	h.lines = append(h.lines, entry)
	if len(h.lines) > historyMax {
		h.lines = h.lines[len(h.lines)-historyMax:]
	}
	// History is a convenience: failing to save it must not stop the shell.
	os.WriteFile(h.path, []byte(strings.Join(h.lines, "\n")+"\n"), 0o600)
}

func (h *fileHistory) Len() int { return len(h.lines) }

// At is 0 for the newest entry.
func (h *fileHistory) At(idx int) string { return h.lines[len(h.lines)-1-idx] }
