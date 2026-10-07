package compat

import (
	"bufio"
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The native-C baseline. mattn-C reaches SQLite from Go through cgo and
// database/sql, which costs microseconds per statement: invisible on a scan,
// most of the time on a point lookup. testdata/nativebench/cbench.c runs the
// same workloads, data and bind values straight against the SQLite C API, so
// "faster than C SQLite" can mean the C library itself.
//
// It is built from the amalgamation mattn bundles -- the oracle's exact
// SQLite (3.53.3) -- with mattn's compile options, so the only thing that
// differs from the mattn-C arm is the path from the caller to sqlite3_step.

// nativeArgSets is how many bind-value sets each workload hands the C side,
// cycled through by its timing loop: the start of the same seeded sequence the
// Go arms use.
const nativeArgSets = 4096

type nativeWorkload struct {
	name, sql string
	args      func(*rand.Rand) []any
}

// nativeCTimes returns each workload's per-query time from the C harness, or
// nil (with the reason logged) when no C compiler is available.
func nativeCTimes(t *testing.T, dbPath string, ws []nativeWorkload) map[string]time.Duration {
	t.Helper()
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	if _, err := exec.LookPath(cc); err != nil {
		t.Logf("native C baseline skipped: no C compiler (%v)", err)
		return nil
	}
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/mattn/go-sqlite3").Output()
	if err != nil {
		t.Fatalf("locating mattn's SQLite source: %v", err)
	}
	src := strings.TrimSpace(string(out))
	dir := t.TempDir()
	bin := filepath.Join(dir, "cbench")
	// Header from mattn's tree, and the program compiled together with its
	// amalgamation: one translation unit per file, -O2 as cgo uses.
	build := exec.Command(cc, "-O2", "-std=gnu99", "-I", src,
		"-DSQLITE_ENABLE_RTREE", "-DSQLITE_THREADSAFE=1", "-DHAVE_USLEEP=1",
		"-DSQLITE_ENABLE_FTS3", "-DSQLITE_ENABLE_FTS3_PARENTHESIS",
		"-DSQLITE_OMIT_DEPRECATED", "-DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1",
		"-DSQLITE_ENABLE_UPDATE_DELETE_LIMIT", "-Wno-deprecated-declarations",
		"-o", bin, "testdata/nativebench/cbench.c", filepath.Join(src, "sqlite3-binding.c"),
		"-lpthread", "-lm", "-ldl")
	if msg, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the native C harness: %v\n%s", err, msg)
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
	res, err := exec.Command(bin, dbPath, specPath).Output()
	if err != nil {
		t.Fatalf("native C harness: %v", err)
	}
	times := map[string]time.Duration{}
	sc := bufio.NewScanner(bytes.NewReader(res))
	for sc.Scan() {
		name, ns, ok := strings.Cut(sc.Text(), "\t")
		n, err := strconv.ParseInt(ns, 10, 64)
		if !ok || err != nil {
			t.Fatalf("native C harness output %q", sc.Text())
		}
		times[name] = time.Duration(n)
	}
	return times
}
