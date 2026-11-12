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

// shellOnPTY runs the shell on a pseudo-terminal cols wide, types typed into
// it and returns what it printed once it quits.
func shellOnPTY(t *testing.T, cols uint16, typed string) (dir, out string) {
	h := start(t)
	h.login()
	h.run("org", "create", "acme")
	h.run("config", "set", "org", "acme")
	h.run("db", "create", "app")
	master, slave := openPTY(t)
	ws := [4]uint16{24, cols}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		t.Fatal(e)
	}
	buf := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- cli.Run(context.Background(), cli.Env{Dir: h.dir, Stdin: slave, Stdout: buf, Stderr: io.Discard,
			Interactive: true, StdinFd: int(slave.Fd()), StdoutFd: int(slave.Fd())}, []string{"db", "shell", "app"})
	}()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(buf.String(), "Connected to acme/app"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no greeting: %q", buf.String())
		}
	}
	if _, err := master.Write([]byte(typed)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the shell did not quit: %q", buf.String())
	}
	return h.dir, buf.String()
}

// TestInteractiveShell: on a terminal the shell edits lines in raw mode, runs
// what is typed and keeps it in its history.
func TestInteractiveShell(t *testing.T) {
	dir, out := shellOnPTY(t, 80, "SELECT 41 + 1 AS n;\r.quit\r")
	if !strings.Contains(out, "42") {
		t.Fatalf("output: %q", out)
	}
	if b, err := os.ReadFile(dir + "/history"); err != nil || !strings.Contains(string(b), "SELECT 41 + 1 AS n;") {
		t.Fatalf("history: %q %v", b, err)
	}
}

// TestInteractiveShellWidth: a line longer than 80 columns is echoed on one
// line of a terminal wide enough for it, not wrapped at the 80th (#101).
func TestInteractiveShellWidth(t *testing.T) {
	stmt := "SELECT '" + strings.Repeat("a", 120) + "' AS s;"
	_, out := shellOnPTY(t, 200, stmt+"\r.quit\r")
	if !strings.Contains(out, "musql> "+stmt) {
		t.Fatalf("the line was wrapped: %q", out)
	}
}

// TestInteractiveShellHomeEnd: Home and End move to the line's ends in every
// spelling a terminal sends them (#132), not type an "H" or an "F".
func TestInteractiveShellHomeEnd(t *testing.T) {
	for _, keys := range [][2]string{{"\x1bOH", "\x1bOF"}, {"\x1b[1~", "\x1b[4~"}, {"\x1b[H", "\x1b[F"}} {
		_, out := shellOnPTY(t, 80, "41 + 1 AS n"+keys[0]+"SELECT "+keys[1]+";\r.quit\r")
		if !strings.Contains(out, "42") {
			t.Fatalf("%q: output %q", keys, out)
		}
	}
}

// TestInteractiveShellInterrupt: Ctrl+C stops the running statement, as in
// C's shell, and the shell goes on to the next one (#188).
func TestInteractiveShellInterrupt(t *testing.T) {
	master, slave := openPTY(t)
	buf := &syncBuf{}
	done := make(chan error, 1)
	path := t.TempDir() + "/i.musq"
	go func() {
		done <- cli.Run(context.Background(), cli.Env{Dir: t.TempDir(), Stdin: slave, Stdout: buf, Stderr: io.Discard,
			Interactive: true, StdinFd: int(slave.Fd()), StdoutFd: int(slave.Fd())}, []string{"open", path})
	}()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(buf.String(), "Opened"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no greeting: %q", buf.String())
		}
	}
	type_ := func(s string) {
		if _, err := master.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	type_("SELECT sum(a.value * b.value) FROM generate_series(1, 100000) a, generate_series(1, 100000) b;\r")
	time.Sleep(300 * time.Millisecond)
	type_("\x03")
	type_("SELECT 41 + 1 AS n;\r.quit\r")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the statement was not interrupted: %q", buf.String())
	}
	if out := buf.String(); !strings.Contains(out, "interrupted") || !strings.Contains(out, "42") {
		t.Fatalf("output: %q", out)
	}
}
