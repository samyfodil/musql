package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/samyfodil/musql/cloud/state"
)

// TestNewAndInitRefuse: the server will not start or initialize on a data
// directory that is a file, a state database that is a directory, or a state
// whose tables it cannot read; and init makes an existing user the admin.
func TestNewAndInitRefuse(t *testing.T) {
	ctx := context.Background()
	cfgAt := func(dir string) *Config {
		t.Helper()
		cfg, err := ParseConfig([]byte("data_dir: " + dir + "\nnode:\n  location: fra\n"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.PublicURL = "http://cloud.test"
		return cfg
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(stateDir, "state.musq"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"a data directory that is a file": file, "a state that is a directory": stateDir} {
		if s, err := New(ctx, cfgAt(dir), nil); err == nil {
			s.Close()
			t.Errorf("%s: started", name)
		}
		if err := Init(ctx, cfgAt(dir), "admin@example.com", "admin-password"); err == nil {
			t.Errorf("%s: initialized", name)
		}
	}

	broken := func(table string) string {
		t.Helper()
		dir := t.TempDir()
		if err := Init(ctx, cfgAt(dir), "admin@example.com", "admin-password"); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(dir, "state.musq"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_broken`); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, table := range []string{"users", "group_locations", "signing_keys"} {
		if s, err := New(ctx, cfgAt(broken(table)), nil); err == nil {
			s.Close()
			t.Errorf("started without %s", table)
		}
	}
	if err := Init(ctx, cfgAt(broken("users")), "admin@example.com", "admin-password"); err == nil {
		t.Error("initialized without users")
	}

	dir := t.TempDir()
	if err := Init(ctx, cfgAt(dir), "admin@example.com", "admin-password"); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.musq"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "bob@example.com", "Bob", state.HashPassword("bob-password"), false); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := Init(ctx, cfgAt(dir), "bob@example.com", "bob-password-2"); err != nil {
		t.Fatal(err)
	}
	st, err = state.Open(filepath.Join(dir, "state.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if u, err := st.UserByEmail(ctx, "bob@example.com"); err != nil || !u.Admin {
		t.Fatalf("init made %+v the admin: %v", u, err)
	}
}
