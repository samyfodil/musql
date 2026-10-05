package compat

// Gate for ATTACH statements that both engines reject.
// connection can answer is "is this ATTACH refused for a reason intrinsic to
// its ARGUMENT", and that is the only answer used: the reply is accepted only
// when the oracle's own error text is one of sqlite3ParseUri's / attachFunc's
// argument-level rejections. Every SESSION-dependent rejection -- a name
// already in use, the attachment limit, a lock conflict, an encoding mismatch
// -- is deliberately NOT in that set, because a virgin connection would answer
// it differently from the real one and booking it here would hide a real gap
// rather than measure one. The first two of those already have their own
// probes (tclOracleAttachNameInUse, tclOracleAttachAtLimit), which is the
// pattern this follows.
//
// It runs on cgoStmt -- the oracle's own, already-relocated statement text
// (tclIsolateAttach) -- so a relative path resolves to the same file the real
// oracle would have opened, and it restores the attach directory to exactly the
// files it held before, so a probe that happens to SUCCEED leaves nothing
// behind for a later statement to find.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// r35bAttachArgumentErrors are the oracle's argument-level ATTACH rejections:
// sqlite3ParseUri's three "no such <kind>: <value>" / "<kind> mode not allowed"
// forms (main.c) and attachFunc's open failure (attach.c, which surfaces as the
// VFS's own "unable to open database file" or as the message naming the raw
// argument). A rejection whose text is none of these is treated as unmeasured.
var r35bAttachArgumentErrors = []string{
	"unable to open database",
	"no such vfs:",
	"no such access mode:",
	"no such cache mode:",
	"access mode not allowed:",
	"cache mode not allowed:",
}

// tclR35BAttachFailsIntrinsically reports whether cgoStmt is an ATTACH the
// oracle refuses for a reason intrinsic to its argument. dir is the segment's
// oracle-side attach directory, restored to its prior contents on the way out.
func tclR35BAttachFailsIntrinsically(t *testing.T, stmt, cgoStmt, dir string) bool {
	t.Helper()
	fields := strings.Fields(stmt)
	if len(fields) == 0 || strings.ToUpper(fields[0]) != "ATTACH" {
		return false
	}
	before := r35bDirEntries(dir)
	defer func() {
		for name := range r35bDirEntries(dir) {
			if !before[name] {
				os.Remove(filepath.Join(dir, name))
			}
		}
	}()

	probe, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "r35bprobe.db"))
	if err != nil {
		return false
	}
	defer probe.Close()
	if _, err := probe.Exec("CREATE TABLE r35b_probe(x)"); err != nil {
		return false // the throwaway connection is not usable; answer nothing
	}
	_, aerr := probe.Exec(cgoStmt)
	if aerr == nil {
		return false
	}
	msg := strings.ToLower(aerr.Error())
	for _, want := range r35bAttachArgumentErrors {
		if strings.Contains(msg, want) {
			return true
		}
	}
	return false
}

// r35bDirEntries is the set of names directly under dir ("" or an unreadable
// directory gives an empty set, which makes the cleanup a no-op).
func r35bDirEntries(dir string) map[string]bool {
	out := map[string]bool{}
	if dir == "" {
		return out
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		out[e.Name()] = true
	}
	return out
}
