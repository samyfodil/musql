package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
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
