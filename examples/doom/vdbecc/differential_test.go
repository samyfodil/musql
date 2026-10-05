package vdbecc

// Every C program in testdata/c is compiled twice -- natively by clang, and to
// VDBE bytecode through clang's IR -- and the two results must match. Each
// exposes int test_main(void). Skipped where clang is not installed.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// irTarget is the target Doom's IR is built for: on it va_list is a plain
// pointer and va_arg a pointer bump over 8-byte slots, the model va_start
// implements. x86-64's register-save-area va_list is not supported.
const irTarget = "arm64-apple-macosx15.0.0"

func TestDifferentialC(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not installed")
	}
	files, err := filepath.Glob("testdata/c/*.c")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test programs: %v", err)
	}
	tmp := t.TempDir()
	wrapper := filepath.Join(tmp, "wrapper.c")
	if err := os.WriteFile(wrapper, []byte("#include <stdio.h>\nextern int test_main(void);\nint main(void){ printf(\"%d\\n\", test_main()); return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			bin := filepath.Join(tmp, "native")
			if out, err := exec.Command("clang", "-O1", "-o", bin, f, wrapper).CombinedOutput(); err != nil {
				t.Fatalf("native build: %v\n%s", err, out)
			}
			out, err := exec.Command(bin).Output()
			if err != nil {
				t.Fatal(err)
			}
			native, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			ll, err := exec.Command("clang", "-O1", "-S", "-emit-llvm", "--target="+irTarget, "-o", "-", f).Output()
			if err != nil {
				t.Fatal(err)
			}
			m, err := ParseModule(string(ll))
			if err != nil {
				t.Fatal(err)
			}
			c, err := Compile(m, Options{Memory: 1 << 20, Entry: "test_main"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Run()
			if err != nil {
				t.Fatal(err)
			}
			if got != native {
				t.Fatalf("MISCOMPILE: native=%d vdbe=%d", native, got)
			}
		})
	}
}
