//go:build linux

package cli_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/samyfodil/musql/cloud/cli"
)

// openPTY is a pseudo-terminal: what is written to master is typed at slave.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pseudo-terminal here:", err)
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatal(e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatal(e)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		m.Close()
	})
	return m, s
}

// TestInteractiveShell: on a terminal the shell edits lines in raw mode, runs
// what is typed and keeps it in its history.
func TestInteractiveShell(t *testing.T) {
	h := start(t)
	h.login()
	h.run("org", "create", "acme")
	h.run("config", "set", "org", "acme")
	h.run("db", "create", "app")
	master, slave := openPTY(t)
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(context.Background(), cli.Env{Dir: h.dir, Stdin: slave, Stdout: out, Stderr: io.Discard,
			Interactive: true, StdinFd: int(slave.Fd())}, []string{"db", "shell", "app"})
	}()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "Connected to acme/app"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no greeting: %q", out.String())
		}
	}
	if _, err := master.Write([]byte("SELECT 41 + 1 AS n;\r.quit\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the shell did not quit: %q", out.String())
	}
	if !strings.Contains(out.String(), "42") {
		t.Fatalf("output: %q", out.String())
	}
	if b, err := os.ReadFile(h.dir + "/history"); err != nil || !strings.Contains(string(b), "SELECT 41 + 1 AS n;") {
		t.Fatalf("history: %q %v", b, err)
	}
}
