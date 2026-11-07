package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	_ "github.com/samyfodil/musql/driver"
)

func TestWriteTable(t *testing.T) {
	var b bytes.Buffer
	writeTable(&b, []string{"NAME", "GROUP"}, [][]string{{"app", "default"}, {"a-longer-name", "eu"}})
	want := "NAME           GROUP\napp            default\na-longer-name  eu\n"
	if b.String() != want {
		t.Fatalf("got\n%q\nwant\n%q", b.String(), want)
	}
}

func TestParseExpiration(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d":  now.AddDate(0, 0, 7),
		"12h": now.Add(12 * time.Hour),
	} {
		got, err := parseExpiration(in, now)
		if err != nil || !got.AsTime().Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "never"} {
		if got, err := parseExpiration(in, now); got != nil || err != nil {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"0d", "-1h", "xd", "7days", "soon"} {
		if _, err := parseExpiration(in, now); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestParseRole(t *testing.T) {
	for _, s := range []string{"viewer", "Member", "ADMIN", "owner"} {
		if _, err := parseRole(s); err != nil {
			t.Error(s, err)
		}
	}
	for _, s := range []string{"", "unspecified", "root"} {
		if _, err := parseRole(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestComplete(t *testing.T) {
	for text, want := range map[string]bool{
		"SELECT 1;":         true,
		"SELECT 1":          false,
		"":                  false,
		"SELECT ';":         false,
		"SELECT ';';":       true,
		"SELECT 1; -- done": true,
		"CREATE TRIGGER t AFTER INSERT ON x BEGIN SELECT 1;":      false,
		"CREATE TRIGGER t AFTER INSERT ON x BEGIN SELECT 1; END;": true,
		"SELECT 1;\nSELECT 2;":                                    true,
		"/* ; */":                                                 false,
	} {
		if got := Complete(text); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 3 << 20: "3.0 MiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}

func TestRowsJSON(t *testing.T) {
	var b bytes.Buffer
	if err := writeRowsJSON(&b, []string{"b", "a"}, [][]any{{int64(1), nil}, {[]byte("x"), 2.5}}); err != nil {
		t.Fatal(err)
	}
	want := "[\n  {\"b\": 1, \"a\": null},\n  {\"b\": \"x\", \"a\": 2.5}\n]\n"
	if b.String() != want {
		t.Fatalf("got %q", b.String())
	}
}

func TestConfigIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "musql")
	c := &Config{URL: "http://x", RefreshToken: "secret"}
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	got, err := LoadConfig(dir)
	if err != nil || got.RefreshToken != "secret" || got.URL != "http://x" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestExplain(t *testing.T) {
	for err, want := range map[error]string{
		errors.New("plain"): "plain",
		connect.NewError(connect.CodeUnimplemented, errors.New("x")):           "this server does not support that yet",
		connect.NewError(connect.CodeUnauthenticated, errors.New("x")):         errNotSignedIn.Error(),
		connect.NewError(connect.CodeNotFound, errors.New("no such database")): "no such database",
	} {
		if got := explain(err); got.Error() != want {
			t.Errorf("%v: %q", err, got)
		}
	}
	if explain(nil) != nil {
		t.Error("no error explained as one")
	}
}

func TestCell(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	// A REAL is shown as SQLite renders it, not as Go's shortest form
	// (issue #116): 2^81 is ...584e+24 there, ...583e+24 in Go.
	for v, want := range map[any]string{nil: "NULL", "x": "x", int64(7): "7", at: "2026-01-02T03:04:05.000000006Z",
		float64(1 << 81): "2.4178516392292584e+24", 0.30000000000000004: "0.30000000000000004", 3.0: "3.0", 49.47: "49.47"} {
		if got := cell(v); got != want {
			t.Errorf("%v: %q", v, got)
		}
	}
	if got := cell([]byte("b")); got != "b" {
		t.Errorf("bytes: %q", got)
	}
}

func TestMeta(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	s := &Shell{Out: &out}
	if quit, err := s.Meta(ctx, ".help"); quit || err != nil || out.String() != helpText {
		t.Fatalf(".help: %v %v %q", quit, err, out.String())
	}
	if _, err := s.Meta(ctx, ".nope"); err == nil || !strings.Contains(err.Error(), ".help lists them") {
		t.Fatalf("an unknown command: %v", err)
	}
	if quit, err := s.Meta(ctx, ".exit"); !quit || err != nil {
		t.Fatalf(".exit: %v %v", quit, err)
	}
}

func TestFileHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	h := loadHistory(path)
	h.Add("SELECT 1;")
	h.Add("  ")
	h.Add("SELECT 2;")
	if h.Len() != 2 || h.At(0) != "SELECT 2;" || h.At(1) != "SELECT 1;" {
		t.Fatalf("history: %q", h.lines)
	}
	again := loadHistory(path)
	if again.Len() != 2 || again.At(0) != "SELECT 2;" {
		t.Fatalf("reloaded: %q", again.lines)
	}
	for i := range historyMax {
		again.Add(fmt.Sprint(i))
	}
	if again.Len() != historyMax || again.At(historyMax-1) != "0" {
		t.Fatalf("trimmed to %d, oldest %q", again.Len(), again.At(again.Len()-1))
	}
}

func TestProgressAndRetryable(t *testing.T) {
	var errs bytes.Buffer
	a := &app{env: Env{Stderr: &errs}}
	a.progress("nothing", 0).update(5)
	if errs.Len() != 0 {
		t.Fatalf("progress of no total: %q", errs.String())
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if retryable(done, errors.New("x")) {
		t.Error("retried with the context done")
	}
	ctx := context.Background()
	if retryable(ctx, connect.NewError(connect.CodeInvalidArgument, errors.New("x"))) || !retryable(ctx, connect.NewError(connect.CodeUnavailable, errors.New("x"))) {
		t.Error("retried a refusal, or not a network failure")
	}
}

// failingReader fails its first read.
type failingReader struct{}

func (failingReader) ReadLine() (string, error) { return "", errors.New("the terminal went away") }
func (failingReader) SetPrompt(string)          {}

// lineReader is lines, then the end of input.
type lineReader []string

func (l *lineReader) ReadLine() (string, error) {
	if len(*l) == 0 {
		return "", io.EOF
	}
	s := (*l)[0]
	*l = (*l)[1:]
	return s, nil
}

func (*lineReader) SetPrompt(string) {}

func TestREPLEndsAndFails(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out, errs bytes.Buffer
	sh := &Shell{DB: db, Out: &out, Mode: "table"}
	ctx := context.Background()
	in := &lineReader{"", "  ", "SELECT 1 AS one;"}
	if err := sh.REPL(ctx, in, &errs); err != nil || out.String() != "one\n1\n" {
		t.Fatalf("to the end of input: %q %v", out.String(), err)
	}
	if err := sh.REPL(ctx, failingReader{}, &errs); err == nil {
		t.Fatal("a reader that failed ended the REPL cleanly")
	}
	if err := sh.Script(ctx, "SELECT 'unterminated"); err == nil {
		t.Fatal("ran a statement that does not end")
	}
}

func TestShellReportsFailures(t *testing.T) {
	var b bytes.Buffer
	if err := writeRowsJSON(&b, []string{"x"}, [][]any{{math.NaN()}}); err == nil {
		t.Error("wrote NaN as JSON")
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.musq"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	ctx := context.Background()
	s := &Shell{DB: db, Out: &b, Mode: "table"}
	for _, line := range []string{".schema", ".tables"} {
		if _, err := s.Meta(ctx, line); err == nil {
			t.Errorf("%s on a closed database", line)
		}
	}
	if err := s.Exec(ctx, "SELECT 1"); err == nil {
		t.Error("ran a statement on a closed database")
	}
}
