package compat

// Scratch probe for stream r36d. Not a gate: it prints both engines' answers for
// an ad-hoc statement list so a shape-space coordinate can be reproduced by hand.
//
//	R36D_SQL='CREATE TABLE ...;;SELECT ...' go test -run TestR36DProbe -v ./
//
// Statements are separated by ";;" so a statement may itself contain ';'.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestR36DProbe(t *testing.T) {
	raw := os.Getenv("R36D_SQL")
	if raw == "" {
		t.Skip("set R36D_SQL")
	}
	var stmts []string
	for _, s := range strings.Split(raw, ";;") {
		if s = strings.TrimSpace(s); s != "" {
			stmts = append(stmts, s)
		}
	}
	m := run(t, "musql", stmts)
	c := run(t, "cgo", stmts)
	for i := range stmts {
		mb, _ := json.Marshal(m[i])
		cb, _ := json.Marshal(c[i])
		mark := "  "
		if string(mb) != string(cb) {
			mark = "!!"
		}
		t.Logf("%s [%d] %s\n   cgo: %s\n   mus: %s", mark, i, stmts[i], cb, mb)
	}
}
