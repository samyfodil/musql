package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/samyfodil/musql/cloud/cli"
)

// TestConsoleLineEditing (#93): in a Windows console the line editor's
// backspace and arrow keys move the cursor instead of printing "^[[D". The
// shell runs in a child of this test inside a pseudo-console, the keys are
// typed into it, and what the console renders is read back.
func TestConsoleLineEditing(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.musq")
	t.Setenv("MUSQL_CONSOLE_CHILD_DB", db)
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		t.Fatal(err)
	}
	var pc windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: 100, Y: 30}, inR, outW, 0, &pc); err != nil {
		t.Skip("no pseudo-console here:", err)
	}
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatal(err)
	}
	defer attrs.Delete()
	// The attribute's value is the console handle itself.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&pc)), unsafe.Sizeof(pc)); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := windows.UTF16PtrFromString(`"` + exe + `" -test.run=^TestConsoleChild$`)
	if err != nil {
		t.Fatal(err)
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	// No handles of this process: the child talks to its console.
	si.Flags = windows.STARTF_USESTDHANDLES
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, cmd, nil, nil, false, windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &si.StartupInfo, &pi); err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)

	var mu sync.Mutex
	var screen strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			var n uint32
			if windows.ReadFile(outR, buf, &n, nil) != nil || n == 0 {
				return
			}
			mu.Lock()
			screen.Write(buf[:n])
			mu.Unlock()
		}
	}()
	shown := func() string {
		mu.Lock()
		defer mu.Unlock()
		return screen.String()
	}
	wait := func(s string) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !strings.Contains(shown(), s); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q on the console:\n%q", s, shown())
			}
		}
	}
	typ := func(s string) {
		t.Helper()
		var n uint32
		if err := windows.WriteFile(inW, []byte(s), &n, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	wait("End statements with ;")
	typ(".modx")
	typ("\x7f")
	typ("\x1b[D\x1b[C")
	typ("e csv\r")
	typ("SELECT 41 + 1 AS a, 'x' AS b;\r")
	wait("42,x")
	typ(".quit\r")
	windows.WaitForSingleObject(pi.Process, 20000)
	windows.ClosePseudoConsole(pc)
	got := shown()
	// The line editor's moves reach the screen as moves: a console printing
	// them would show their text, the ESC as a glyph and "[D" after it.
	if strings.Contains(strings.ReplaceAll(got, "\x1b[", ""), "[D") || strings.Contains(got, "←[") {
		t.Errorf("escape sequences printed as text:\n%q", got)
	}
}

// TestConsoleChild is the shell TestConsoleLineEditing types into, run in its
// pseudo-console.
func TestConsoleChild(t *testing.T) {
	db := os.Getenv("MUSQL_CONSOLE_CHILD_DB")
	if db == "" {
		t.Skip("run by TestConsoleLineEditing")
	}
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A console window starts with escape sequences off (a pseudo-console
	// has them on): as the shell finds one.
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(out.Fd()), &mode); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetConsoleMode(windows.Handle(out.Fd()), mode&^windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		t.Fatal(err)
	}
	err = cli.Run(context.Background(), cli.Env{Dir: t.TempDir(), Stdin: in, Stdout: out, Stderr: out,
		Interactive: true, StdinFd: int(in.Fd()), StdoutFd: int(out.Fd())}, []string{"open", db})
	if err != nil {
		t.Fatal(err)
	}
}
