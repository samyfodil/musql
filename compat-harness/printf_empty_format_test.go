package compat

// This file tests that printf()/format() with an empty format string
// returns NULL, not an empty string.

import (
	"encoding/json"
	"testing"
)

func TestPrintfEmptyFormatIsNull(t *testing.T) {
	for _, c := range []struct{ name, sql string }{
		{"empty-fmt-1arg", `SELECT printf('', 1), typeof(printf('', 1))`},
		{"empty-fmt-0arg", `SELECT printf(''), typeof(printf(''))`},
		{"pct-s-empty", `SELECT printf('%s',''), typeof(printf('%s',''))`},
		{"pct-.0s", `SELECT printf('%.0s','abc'), typeof(printf('%.0s','abc'))`},
		{"format-empty", `SELECT format('',1), typeof(format('',1))`},
		{"nonempty", `SELECT printf('x'), typeof(printf('x'))`},
		{"null-fmt", `SELECT printf(NULL,1), typeof(printf(NULL,1))`},
		{"empty-concat", `SELECT printf('')||'z'`},
		{"empty-length", `SELECT length(printf(''))`},
		{"blob-empty", `SELECT printf('%s', x'')`},
	} {
		m := run(t, "musql", []string{c.sql})
		cg := run(t, "cgo", []string{c.sql})
		mj, _ := json.Marshal(m[0])
		cj, _ := json.Marshal(cg[0])
		if string(mj) != string(cj) {
			t.Errorf("%s DIVERGES\n  sql:    %s\n  cgo:    %s\n  musql: %s", c.name, c.sql, cj, mj)
		}
	}
}
