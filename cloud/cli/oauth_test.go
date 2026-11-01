package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDeviceFlowEndings: the device flow ends on a denial, an expired code, a
// refusal of any other kind, a code whose time ran out while pending, a
// context done, and a server that answers what is not JSON or not 200.
func TestDeviceFlowEndings(t *testing.T) {
	var mu sync.Mutex
	answer, status := "", http.StatusOK
	set := func(a string, s int) {
		mu.Lock()
		defer mu.Unlock()
		answer, status = a, s
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		a, s := answer, status
		mu.Unlock()
		w.WriteHeader(s)
		io.WriteString(w, a)
	}))
	defer srv.Close()
	ctx := context.Background()
	hc := srv.Client()
	d := &DeviceAuth{DeviceCode: "dc"}
	for reply, want := range map[string]string{
		`{"error":"access_denied"}`: "denied",
		`{"error":"expired_token"}`: "expired",
		`{"error":"invalid_grant"}`: "sign-in failed",
		`<html>`:                    "200 OK",
	} {
		set(reply, http.StatusOK)
		if _, err := PollDevice(ctx, hc, srv.URL, d); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", reply, err, want)
		}
	}
	set(`{"error":"authorization_pending"}`, http.StatusOK)
	if _, err := PollDevice(ctx, hc, srv.URL, &DeviceAuth{DeviceCode: "dc", ExpiresIn: 1}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("pending past the code's time: %v", err)
	}
	done, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := PollDevice(done, hc, srv.URL, d); err == nil {
		t.Error("polled on past its context")
	}

	set(`{}`, http.StatusBadRequest)
	if _, err := StartDevice(ctx, hc, srv.URL); err == nil || !strings.Contains(err.Error(), "device authorization failed") {
		t.Errorf("a device the server refused: %v", err)
	}
	if err := revoke(ctx, hc, srv.URL, "rt"); err == nil || !strings.Contains(err.Error(), "revoke") {
		t.Errorf("a revoke the server refused: %v", err)
	}
	gone := "http://127.0.0.1:1"
	if _, err := StartDevice(ctx, hc, gone); err == nil {
		t.Error("a device from no server")
	}
	if _, err := refresh(ctx, hc, gone, "rt"); err == nil {
		t.Error("a refresh from no server")
	}
	if err := revoke(ctx, hc, gone, "rt"); err == nil {
		t.Error("a revoke to no server")
	}
}

func TestConfigRefusesWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Error("read a config that is not JSON")
	}
	asDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(asDir, configFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(asDir); err == nil {
		t.Error("read a config that is a directory")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Config{}).Save(filepath.Join(file, "musql")); err == nil {
		t.Error("saved under a file")
	}
}
