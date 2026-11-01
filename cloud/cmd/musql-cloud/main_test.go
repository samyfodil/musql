//go:build unix

package main

import (
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/samyfodil/musql/cloud/cluster"
)

// runMain runs main with args as its command line and stdin as its input,
// returning what it printed.
func runMain(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte(stdin), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	oldArgs, oldIn, oldOut := os.Args, os.Stdin, os.Stdout
	defer func() { os.Args, os.Stdin, os.Stdout = oldArgs, oldIn, oldOut }()
	flag.CommandLine = flag.NewFlagSet("musql-cloud", flag.ExitOnError)
	os.Args, os.Stdin, os.Stdout = append([]string{"musql-cloud"}, args...), f, w
	main()
	w.Close()
	return <-out
}

// TestCommandInitsAndServes: init makes the admin from a password on stdin,
// -peer-id prints the node's ID, and the server serves until interrupted.
func TestCommandInitsAndServes(t *testing.T) {
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+filepath.Join(dir, "data")+"\nlisten: "+addr+"\nnode:\n  location: fra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runMain(t, "admin-password\n", "init", "-config", cfg, "--admin-email", "admin@example.com")

	id := strings.TrimSpace(runMain(t, "", "-config", cfg, "-peer-id"))
	key, err := cluster.LoadKey(filepath.Join(dir, "data", "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	if want, err := cluster.PeerID(key); err != nil || id != want {
		t.Fatalf("peer id %q, want %q (%v)", id, want, err)
	}

	served := make(chan bool, 1)
	go func() {
		ok := false
		for deadline := time.Now().Add(30 * time.Second); !ok && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if resp, err := http.Get("http://" + addr + "/"); err == nil {
				resp.Body.Close()
				ok = true
			}
		}
		served <- ok
		syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	runMain(t, "", "-config", cfg)
	if !<-served {
		t.Fatal("the server never answered")
	}
}
