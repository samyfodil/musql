package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPasswordLine(t *testing.T) {
	for in, want := range map[string]string{"pw\n": "pw", "pw": "pw", "a b\r\nrest": "a b"} {
		if got, err := readPasswordLine(strings.NewReader(in)); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "\n"} {
		if _, err := readPasswordLine(strings.NewReader(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

// TestInitRefuses: init needs an admin email that is not blank, a config it
// can read and parse, and a password.
func TestInitRefuses(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+filepath.Join(dir, "data")+"\nnode:\n  location: fra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("listen: [not, a, string]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"-config", cfg, "--admin-email", "a@example.com", "--admin-password", "pw-pw-pw-pw"}); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no email":          {"-config", cfg},
		"no config":         {"-config", filepath.Join(dir, "none.yaml"), "--admin-email", "a@example.com"},
		"a config not YAML": {"-config", bad, "--admin-email", "a@example.com"},
		"a blank email":     {"-config", cfg, "--admin-email", " ", "--admin-password", "pw-pw-pw-pw"},
	} {
		if err := runInit(args); err == nil {
			t.Errorf("%s: initialized", name)
		}
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	old := os.Stdin
	defer func() { os.Stdin = old }()
	os.Stdin = devNull
	if err := runInit([]string{"-config", cfg, "--admin-email", "b@example.com"}); err == nil {
		t.Error("initialized without a password on stdin")
	}
}
