package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// runMain runs main with args as its command line, returning what it printed.
func runMain(t *testing.T, args ...string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	oldArgs, oldOut := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldOut }()
	os.Args, os.Stdout = append([]string{"musql"}, args...), w
	main()
	w.Close()
	return <-out
}

// TestCommandRunsTheCLI: the command runs the CLI with its configuration in
// MUSQL_CONFIG_DIR, or else in the user's configuration directory.
func TestCommandRunsTheCLI(t *testing.T) {
	t.Setenv("MUSQL_API_TOKEN", "")
	t.Setenv("MUSQL_CONFIG_DIR", t.TempDir())
	if out := runMain(t, "config", "show"); !strings.Contains(out, "signed in: no") {
		t.Fatalf("config show: %q", out)
	}
	home := t.TempDir()
	t.Setenv("MUSQL_CONFIG_DIR", "")
	for _, v := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(v, home)
	}
	if out := runMain(t, "config", "show"); !strings.Contains(out, "signed in: no") {
		t.Fatalf("config show in the user's directory: %q", out)
	}
}
