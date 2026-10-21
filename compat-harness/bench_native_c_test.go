package compat

import (
	"bufio"
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The native baselines. mattn-C reaches SQLite from Go through cgo and
// database/sql, and tursogo reaches Turso through purego and database/sql --
// re-preparing the statement on every query, even a prepared *sql.Stmt's.
// Either costs microseconds per statement: invisible on a scan, most of the
// time on a point lookup. testdata/nativebench holds two C programs that run
// the same workloads, data and bind values straight against each library's C
// API, so "faster than C SQLite" or "than Turso" means the library itself.
//
// cbench is built from the amalgamation mattn bundles -- the oracle's exact
// SQLite (3.53.3) -- with mattn's compile options. tbench links the Rust
// library tursogo loads, from the same module version.

// nativeArgSets is how many bind-value sets each workload hands the C side,
// cycled through by its timing loop: the start of the same seeded sequence the
// Go arms use.
const nativeArgSets = 4096

type nativeWorkload struct {
	name, sql string
	args      func(*rand.Rand) []any
}

// nativeResult is one workload's per-query time and the checksum of the values
// its first bind set read, which two libraries agree on when they answered the
// same rows.
type nativeResult struct {
	d   time.Duration
	sum string
}

// nativeCTimes times the workloads through the SQLite C API, or returns nil
// (with the reason logged) when no C compiler is available.
func nativeCTimes(t *testing.T, dbPath string, ws []nativeWorkload) map[string]nativeResult {
	t.Helper()
	src := moduleDir(t, "github.com/mattn/go-sqlite3")
	return runNative(t, "cbench", dbPath, ws, "-I", src,
		"-DSQLITE_ENABLE_RTREE", "-DSQLITE_THREADSAFE=1", "-DHAVE_USLEEP=1",
		"-DSQLITE_ENABLE_FTS3", "-DSQLITE_ENABLE_FTS3_PARENTHESIS",
		"-DSQLITE_OMIT_DEPRECATED", "-DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1",
		"-DSQLITE_ENABLE_UPDATE_DELETE_LIMIT", "-Wno-deprecated-declarations",
		"testdata/nativebench/cbench.c", filepath.Join(src, "sqlite3-binding.c"),
		"-lpthread", "-lm", "-ldl")
}

// nativeTursoTimes times the workloads through Turso's C API, or returns nil
// (with the reason logged) when there is no C compiler or no prebuilt library
// for this platform.
func nativeTursoTimes(t *testing.T, dbPath string, ws []nativeWorkload) map[string]nativeResult {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Logf("native Turso baseline skipped on windows")
		return nil
	}
	lib := filepath.Join(moduleDir(t, "github.com/tursodatabase/turso-go-platform-libs"), "libs", runtime.GOOS+"_"+runtime.GOARCH)
	if _, err := os.Stat(lib); err != nil {
		t.Logf("native Turso baseline skipped: %v", err)
		return nil
	}
	// Turso locks its file exclusively and the Go arm still holds it, so the
	// harness reads a copy, its WAL included.
	cp := filepath.Join(t.TempDir(), "turso.db")
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(dbPath + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cp+suffix, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return runNative(t, "tbench", cp, ws, "testdata/nativebench/tbench.c",
		"-L", lib, "-lturso_sync_sdk_kit", "-Wl,-rpath,"+lib, "-lpthread", "-lm", "-ldl")
}

func moduleDir(t *testing.T, mod string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", mod).Output()
	if err != nil {
		t.Fatalf("locating %s: %v", mod, err)
	}
	return strings.TrimSpace(string(out))
}

func runNative(t *testing.T, name, dbPath string, ws []nativeWorkload, ccArgs ...string) map[string]nativeResult {
	t.Helper()
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	if _, err := exec.LookPath(cc); err != nil {
		t.Logf("native %s skipped: no C compiler (%v)", name, err)
		return nil
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	// -O2 as cgo uses.
	build := exec.Command(cc, append([]string{"-O2", "-std=gnu99", "-o", bin}, ccArgs...)...)
	if msg, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", name, err, msg)
	}

	var spec bytes.Buffer
	for _, w := range ws {
		fmt.Fprintf(&spec, "W\t%s\t%s\n", w.name, w.sql)
		rng := rand.New(rand.NewSource(benchSeed))
		for range nativeArgSets {
			parts := make([]string, 0, 2)
			for _, a := range w.args(rng) {
				parts = append(parts, fmt.Sprint(a))
			}
			fmt.Fprintf(&spec, "A\t%s\n", strings.Join(parts, ","))
		}
	}
	specPath := filepath.Join(dir, "workloads.tsv")
	if err := os.WriteFile(specPath, spec.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(bin, dbPath, specPath)
	cmd.Stderr = &stderr
	res, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, stderr.Bytes())
	}
	out := map[string]nativeResult{}
	sc := bufio.NewScanner(bytes.NewReader(res))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 {
			t.Fatalf("%s output %q", name, sc.Text())
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			t.Fatalf("%s output %q", name, sc.Text())
		}
		out[f[0]] = nativeResult{time.Duration(n), f[2]}
	}
	return out
}
