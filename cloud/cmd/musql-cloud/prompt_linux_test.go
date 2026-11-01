//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
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

// TestPromptPasswordOnATerminal: on a terminal the password is asked twice,
// and two that differ or an empty one are refused.
func TestPromptPasswordOnATerminal(t *testing.T) {
	master, slave := openPTY(t)
	for typed, want := range map[string]string{
		"pw-one\npw-one\n": "pw-one",
		"pw-one\npw-two\n": "do not match",
		"\n\n":             "empty",
	} {
		if _, err := master.Write([]byte(typed)); err != nil {
			t.Fatal(err)
		}
		pw, err := promptPassword(slave, io.Discard)
		if !strings.Contains(pw+fmt.Sprint(err), want) {
			t.Errorf("%q: %q %v, want %q", typed, pw, err, want)
		}
	}
}
