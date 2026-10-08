// Cross-process variant of N4 concurrency tests. Runs concurrent writes from
// separate OS processes to verify locking and transaction isolation work
// correctly across process boundaries.
package engine_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

const n4CrossProcessEnvVar = "MUSQL_N4_CROSSPROCESS_HELPER"

// TestN4CrossProcessHelperProcess is not a real test: when
// n4CrossProcessEnvVar is unset (the normal `go test` invocation) it returns
// immediately. TestN4CrossProcess re-execs this same test binary with that
// variable set and a subcommand after a literal "--" argument -- identical
// shape to compat-harness/concurrency_lock_test.go's TestConcurrencyHelperProcess.
func TestN4CrossProcessHelperProcess(t *testing.T) {
	if os.Getenv(n4CrossProcessEnvVar) == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "n4 crossprocess helper: missing -- separator")
		os.Exit(2)
	}
	args = args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "n4 crossprocess helper: no subcommand")
		os.Exit(2)
	}
	var code int
	switch args[0] {
	case "writer":
		code = n4CrossProcessWriter(args[1:])
	case "reader":
		code = n4CrossProcessReader(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "n4 crossprocess helper: unknown subcommand %q\n", args[0])
		code = 2
	}
	os.Exit(code)
}

// n4CrossProcessWriter: usage: writer <path> <writerID> <n>. Runs
// n4GenWriterOps(writerID, n) sequentially via ordinary autocommit
// statements, tracks which ids actually end up alive (independent of what
// the generator assumed), and prints one line to stdout on success:
// "ALIVE <space-separated ids>".
func n4CrossProcessWriter(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "writer: usage: writer <path> <writerID> <n>")
		return 2
	}
	path := args[0]
	writerID, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "writer: bad writerID %q: %v\n", args[1], err)
		return 2
	}
	n, err := strconv.Atoi(args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "writer: bad n %q: %v\n", args[2], err)
		return 2
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "writer: sql.Open: %v\n", err)
		return 1
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()

	alive := map[int64]bool{}
	for _, op := range n4GenWriterOps(writerID, n) {
		_, err := db.ExecContext(ctx, op.sql, op.args...)
		if err != nil {
			continue // ErrBusy-class: this attempt simply did not land, exactly like the in-process harness
		}
		switch {
		case strings.HasPrefix(op.sql, "INSERT"):
			alive[op.args[0].(int64)] = true
		case strings.HasPrefix(op.sql, "DELETE"):
			delete(alive, op.args[len(op.args)-1].(int64))
			// UPDATE changes v, never aliveness.
		}
	}

	ids := make([]int64, 0, len(alive))
	for id := range alive {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var sb strings.Builder
	sb.WriteString("ALIVE")
	for _, id := range ids {
		fmt.Fprintf(&sb, " %d", id)
	}
	fmt.Println(sb.String())
	return 0
}

// n4CrossProcessReader: usage: reader <path> <durationMillis>. Loops
// ordinary autocommit SELECTs against path for the given duration, then
// prints "READS <n> ERRORS <m>" and exits 0 regardless of individual query
// outcomes (a reader hitting ErrBusy under a writer's EXCLUSIVE hold is an
// accepted outcome, not a failure of this helper).
func n4CrossProcessReader(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "reader: usage: reader <path> <durationMillis>")
		return 2
	}
	path := args[0]
	ms, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "reader: bad durationMillis %q: %v\n", args[1], err)
		return 2
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reader: sql.Open: %v\n", err)
		return 1
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()

	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	var reads, errs int
	for time.Now().Before(deadline) {
		rows, err := db.QueryContext(ctx, `SELECT count(*) FROM t`)
		if err != nil {
			errs++
			continue
		}
		for rows.Next() {
		}
		rows.Close()
		reads++
	}
	fmt.Printf("READS %d ERRORS %d\n", reads, errs)
	return 0
}

// n4RunCrossProcessHelper re-execs this test binary as a genuinely separate
// OS process running TestN4CrossProcessHelperProcess with the given
// subcommand args, and returns its captured stdout. On a non-zero exit it
// returns a non-nil error (with stderr folded in) instead of calling
// t.Fatalf itself: Go's testing package does not allow Fatal/Fatalf to be
// called from any goroutine other than the one running the test (calling it
// from a spawned goroutine risks "panic: Fail in goroutine after TestXxx has
// completed", which takes down the ENTIRE test binary, not just this test) --
// so every call site that runs this from its own goroutine must funnel the
// (string, error) result back through a channel and only the goroutine
// actually running the *testing.T may act on it. t is accepted solely for
// t.Helper() bookkeeping, matching this file's other helpers; it must never
// be used to fail the test from here.
func n4RunCrossProcessHelper(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"-test.run=^TestN4CrossProcessHelperProcess$", "--"}, args...)
	cmd := exec.Command(os.Args[0], full...)
	cmd.Env = append(os.Environ(), n4CrossProcessEnvVar+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("n4 crossprocess helper %v failed: %w (stderr: %s)", args, err, stderr.String())
	}
	return stdout.String(), nil
}

// n4ParseReaderOutput parses a reader subprocess's "READS <n> ERRORS <m>"
// stdout line (n4CrossProcessReader above). Returns an error if the line is
// missing or malformed -- the parent test must be able to make a real
// assertion on what a reader subprocess actually observed, not merely that
// the subprocess exited 0.
func n4ParseReaderOutput(out string) (reads, errs int, err error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		var n, m int
		if _, serr := fmt.Sscanf(line, "READS %d ERRORS %d", &n, &m); serr == nil {
			return n, m, nil
		}
	}
	return 0, 0, fmt.Errorf("no \"READS <n> ERRORS <m>\" line found in reader output %q", out)
}

// TestN4CrossProcess is the second-tier confirmation: 3 writer subprocesses
// + 2 reader subprocesses, genuinely separate OS processes, sharing one
// database file, run concurrently via os/exec (not goroutines). After they
// all exit, the survivor file must (a) pass CheckStructuralIntegrity + N3,
// exactly like the primary harness, and (b) contain EXACTLY the union of
// every writer subprocess's own reported alive-id set -- no lost updates,
// no phantom rows, across a REAL process boundary.
func TestN4CrossProcess(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip("js/wasm: no subprocesses (pipe is not implemented), and no other process can open the file")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "n4-crossprocess.db")
	ctx := context.Background()

	seedDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("N4 crossprocess: sql.Open (seed): %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, n4CreateTable); err != nil {
		t.Fatalf("N4 crossprocess: CREATE TABLE: %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, n4CreateIndex); err != nil {
		t.Fatalf("N4 crossprocess: CREATE INDEX: %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("N4 crossprocess: seed Close: %v", err)
	}

	const nWriters = 3
	const writesPerWriter = 15
	const nReaders = 2
	const readerDurationMillis = 4000

	type result struct {
		writerID int
		out      string
		err      error
	}
	results := make(chan result, nWriters)
	for w := 0; w < nWriters; w++ {
		go func(id int) {
			full := []string{"-test.run=^TestN4CrossProcessHelperProcess$", "--", "writer", path, strconv.Itoa(id), strconv.Itoa(writesPerWriter)}
			cmd := exec.Command(os.Args[0], full...)
			cmd.Env = append(os.Environ(), n4CrossProcessEnvVar+"=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err != nil {
				results <- result{id, "", fmt.Errorf("%v (stderr: %s)", err, stderr.String())}
				return
			}
			results <- result{id, stdout.String(), nil}
		}(w)
	}
	// Readers report their own outcome back through a channel rather than
	// calling t.Fatalf directly from their own goroutine (see
	// n4RunCrossProcessHelper's doc comment) -- matching the writer
	// goroutines' own `results` channel pattern immediately above, which
	// already did this correctly.
	type readerResult struct {
		readerID int
		reads    int
		errs     int
		err      error
	}
	readerResults := make(chan readerResult, nReaders)
	for r := 0; r < nReaders; r++ {
		go func(id int) {
			out, err := n4RunCrossProcessHelper(t, "reader", path, strconv.Itoa(readerDurationMillis))
			if err != nil {
				readerResults <- readerResult{readerID: id, err: err}
				return
			}
			reads, errs, perr := n4ParseReaderOutput(out)
			if perr != nil {
				readerResults <- readerResult{readerID: id, err: perr}
				return
			}
			readerResults <- readerResult{readerID: id, reads: reads, errs: errs}
		}(r)
	}

	expectedAlive := map[int64]bool{}
	for w := 0; w < nWriters; w++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("N4 crossprocess: writer %d subprocess failed: %v", res.writerID, res.err)
		}
		sc := bufio.NewScanner(strings.NewReader(res.out))
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "ALIVE") {
				continue
			}
			for _, f := range strings.Fields(line)[1:] {
				id, err := strconv.ParseInt(f, 10, 64)
				if err != nil {
					t.Fatalf("N4 crossprocess: writer %d printed unparseable id %q", res.writerID, f)
				}
				expectedAlive[id] = true
			}
		}
	}
	// Wait for both reader subprocesses to finish their own fixed duration
	// and report back, and inspect what each one actually observed -- not
	// just that its subprocess exited 0. A reader hitting ErrBusy under a
	// writer's EXCLUSIVE hold is an accepted, expected outcome (see
	// n4CrossProcessReader's own doc comment); a reader subprocess failing
	// to launch/exit cleanly, or printing unparseable output, is not, and is
	// exactly the class of finding this channel plumbing exists to surface
	// on the MAIN test goroutine rather than crashing the whole binary from
	// a spawned one.
	for r := 0; r < nReaders; r++ {
		res := <-readerResults
		if res.err != nil {
			t.Fatalf("N4 crossprocess: reader %d subprocess failed: %v", res.readerID, res.err)
		}
		t.Logf("N4 crossprocess: reader %d performed %d reads (%d errors) concurrently with the writers", res.readerID, res.reads, res.errs)
	}

	rp, err := engine.Open(path)
	if err != nil {
		t.Fatalf("N4 crossprocess post-run: Open: %v", err)
	}
	rowids, _, err := rp.Rows("t")
	rp.Close()
	if err != nil {
		t.Fatalf("N4 crossprocess post-run: Rows: %v", err)
	}
	actual := map[int64]bool{}
	for _, id := range rowids {
		actual[int64(id)] = true
	}
	var missing, extra []int64
	for id := range expectedAlive {
		if !actual[id] {
			missing = append(missing, id)
		}
	}
	for id := range actual {
		if !expectedAlive[id] {
			extra = append(extra, id)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("STOP-AND-RECONSIDER (N4 crossprocess): survivor file disagrees with the union of every writer subprocess's own reported alive-id set -- missing (expected, not present): %v; extra (present, not expected by any writer): %v", missing, extra)
	}
	t.Logf("N4 crossprocess: %d writer subprocess(es), %d reader subprocess(es), %d rows reconciled exactly", nWriters, nReaders, len(actual))

	checkStructuralAndN3(t, path)
	t.Log("N4 crossprocess post-run: CheckStructuralIntegrity + N3 index cross-check both clean")
}
