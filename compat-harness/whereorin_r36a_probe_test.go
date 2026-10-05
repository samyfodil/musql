package compat

// R36a hand probe: R36A_SQL='stmt;stmt;...' prints both engines' answers.
// Measurement scaffolding for the OR/IN port; skipped unless asked.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestR36aProbe(t *testing.T) {
	sql := os.Getenv("R36A_SQL")
	if sql == "" {
		t.Skip("set R36A_SQL")
	}
	var stmts []string
	for _, s := range strings.Split(r36Fixture+sql, ";") {
		if s = strings.TrimSpace(s); s != "" {
			stmts = append(stmts, s)
		}
	}
	m := run(t, "musql", stmts)
	cg := run(t, "cgo", stmts)
	for i := range stmts {
		mb, _ := json.Marshal(m[i])
		cb, _ := json.Marshal(cg[i])
		if string(mb) == string(cb) {
			continue
		}
		fmt.Printf("R36A PROBE sql: %s\n  cgo:    %s\n  musql: %s\n", stmts[i], cb, mb)
	}
}
