package compat

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/samyfodil/musql/engine"
)

// The JIT lowering score's collection (scripts/jit-score). In a jitscore
// build, every statement TestTCLCorpus runs on the engine is recorded with
// what the engine counted for it (engine.JitScoreTake), under the key of its
// file, segment and position, and written to JITSCORE_OUT -- a directory, one
// file per test process -- when the test ends. Run once with the JIT and
// once without, the two are joined by key: a statement interpreted N
// instructions without the JIT and M with it had 1 - M/N of its work lowered.

var (
	tclJSMu   sync.Mutex
	tclJSKey  string // the statement about to run: file#segment|index
	tclJSRecs []string
)

// tclJSBegin clears the counts before a statement, so tclJSEnd's are its own.
func tclJSBegin() {
	if engine.JitScoreEnabled() {
		engine.JitScoreTake()
	}
}

// tclJSEnd records the statement's counts.
func tclJSEnd(stmt string) {
	if !engine.JitScoreEnabled() || tclJSKey == "" {
		return
	}
	st := engine.JitScoreTake()
	ops := make([]string, 0, len(st.Ops))
	for op, n := range st.Ops {
		ops = append(ops, fmt.Sprintf("%s=%d", op, n))
	}
	sort.Strings(ops)
	kind := strings.ToUpper(strings.Fields(strings.TrimLeft(stmt, " \t\r\n(") + " X")[0])
	switch kind {
	case "SELECT", "VALUES", "WITH", "INSERT", "REPLACE", "UPDATE", "DELETE":
	default:
		kind = "OTHER"
	}
	tclJSMu.Lock()
	defer func() { tclJSKey = "" }() // a statement outside the corpus loop records nothing
	tclJSRecs = append(tclJSRecs, fmt.Sprintf("%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s",
		tclJSKey, kind, st.Steps, st.Enters, st.KernelCalls, st.StaticNative, st.StaticAll, strings.Join(ops, ",")))
	tclJSMu.Unlock()
}

// tclJSWrite writes the records to JITSCORE_OUT, when set.
func tclJSWrite() error {
	dir := os.Getenv("JITSCORE_OUT")
	if dir == "" || !engine.JitScoreEnabled() {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, fmt.Sprintf("score-%d.tsv", os.Getpid())))
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	tclJSMu.Lock()
	for _, r := range tclJSRecs {
		fmt.Fprintln(w, r)
	}
	tclJSMu.Unlock()
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
