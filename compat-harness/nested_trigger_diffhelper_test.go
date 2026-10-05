// This file provides differential testing for trigger scripts against C SQLite.
package compat

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// trigDiffRun runs stmts on one engine and returns a transcript of each statement's
// error (or ok) followed by the rows of each query in probes.
func trigDiffRun(t *testing.T, driver, dsn string, stmts, probes []string) string {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return "open: " + err.Error()
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var b strings.Builder
	for _, s := range stmts {
		if _, e := db.Exec(s); e != nil {
			fmt.Fprintf(&b, "%-50s -> ERR %v\n", s, trigDiffErr(e))
		} else {
			fmt.Fprintf(&b, "%-50s -> ok\n", s)
		}
	}
	for _, q := range probes {
		rows, e := db.Query(q)
		if e != nil {
			fmt.Fprintf(&b, "Q %-48s -> ERR %v\n", q, trigDiffErr(e))
			continue
		}
		cols, _ := rows.Columns()
		fmt.Fprintf(&b, "Q %-48s -> %v\n", q, cols)
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			out := make([]string, len(vals))
			for i, v := range vals {
				switch x := v.(type) {
				case []byte:
					out[i] = string(x)
				default:
					out[i] = fmt.Sprint(x)
				}
			}
			fmt.Fprintf(&b, "    [%s]\n", strings.Join(out, " | "))
		}
		rows.Close()
	}
	return b.String()
}

// trigDiffErr normalizes away two pieces of musql message decoration the
// oracle does not carry, so these batteries compare SEMANTICS -- which
// statement failed, with which SQLite error -- rather than wording.
//
// The first is the "engine: " prefix every error here has. The second is the
// statement-context fragment this engine puts in front of a constraint failure
// raised while storing a row: "INSERT into u: UNIQUE constraint failed: u.a"
// where the oracle says "UNIQUE constraint failed: u.a"
// (fmt.Sprintf("engine: INSERT into %s", ...), engine/vdbe_write.go, and its
// UPDATE/DELETE siblings). That difference is PRE-EXISTING and unrelated to any
// trigger work -- it shows identically for a plain "INSERT INTO u VALUES(1)"
// against a UNIQUE column, on both this engine's write routes -- so a battery
// about trigger cascades should neither fail on it nor pretend it is not there.
// Everything after the fragment, including the constraint kind and the column
// it names, is still compared exactly.
var trigDiffCtxPrefix = regexp.MustCompile(`^(?:INSERT into|UPDATE of|DELETE from) \S+: `)

func trigDiffErr(e error) string {
	s := strings.TrimPrefix(e.Error(), "engine: ")
	return trigDiffCtxPrefix.ReplaceAllString(s, "")
}

type trigDiffCase struct {
	name   string
	stmts  []string
	probes []string
}

func trigDiff(t *testing.T, cases []trigDiffCase) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := trigDiffRun(t, "sqlite3", filepath.Join(t.TempDir(), "o.db"), tc.stmts, tc.probes)
			got := trigDiffRun(t, "sqlite", filepath.Join(t.TempDir(), "m.db"), tc.stmts, tc.probes)
			if want != got {
				t.Errorf("DIVERGES\n--- oracle ---\n%s--- musql ---\n%s", want, got)
			} else {
				t.Logf("agree\n%s", want)
			}
		})
	}
}
