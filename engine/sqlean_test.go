package engine

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// withSQLean registers the sqlean families for the length of the test and
// restores the tables RegisterSQLean adds to afterwards, so the other tests
// in this package keep seeing a build without them.
func withSQLean(t *testing.T, families ...string) {
	t.Helper()
	sf, nc, uf, ua := maps.Clone(supportedFuncs), maps.Clone(nonConstantFuncs), maps.Clone(udfs), maps.Clone(udafs)
	reg, rows, mods := maps.Clone(sqleanRegistered), sqleanFuncRows, sqleanModuleNames
	if err := RegisterSQLean(families...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, m := range sqleanModuleNames[len(mods):] {
			unregisterVtabModule(m)
		}
		supportedFuncs, nonConstantFuncs, udfs, udafs = sf, nc, uf, ua
		sqleanRegistered, sqleanFuncRows, sqleanModuleNames = reg, rows, mods
	})
}

// TestSQLeanSuites runs sqlean's own SQL test suites (testdata/sqlean, from
// github.com/nalgeon/sqlean's test/, MIT License): every case is
// "select '<label>', <boolean>;" and must answer 1. The other statements set
// up their tables and must succeed. sqleanSkip lists the cases this engine
// does not reproduce, each with its reason.
func TestSQLeanSuites(t *testing.T) {
	withSQLean(t)
	files, err := filepath.Glob("testdata/sqlean/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no suites: %v", err)
	}
	for _, file := range files {
		suite := strings.TrimSuffix(filepath.Base(file), ".sql")
		t.Run(suite, func(t *testing.T) {
			db, err := Create(filepath.Join(t.TempDir(), "s.musq"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range sqleanStatements(string(src)) {
				label := sqleanLabel(stmt)
				if label == "" {
					if err := db.Exec(stmt); err != nil {
						t.Fatalf("%s: %v", stmt, err)
					}
					continue
				}
				if reason, ok := sqleanSkip[suite+"/"+label]; ok {
					t.Logf("skip %s: %s", label, reason)
					continue
				}
				pg, err := db.SnapshotPager()
				if err != nil {
					t.Fatal(err)
				}
				_, rows, qerr := pg.QueryArgs(stmt, nil)
				pg.Close()
				switch {
				case qerr != nil:
					t.Errorf("%s: %v", stmt, qerr)
				case len(rows) != 1 || len(rows[0]) != 2:
					t.Errorf("%s: %v", stmt, rows)
				case rows[0][1].Typ != Int || rows[0][1].I != 1:
					t.Errorf("%s = %v, want 1", stmt, rows[0][1])
				}
			}
		})
	}
}

// sqleanSkip is the suite cases not reproduced, by "<suite>/<label>".
var sqleanSkip = map[string]string{
	"stats/08_24": "group_concat over a correlated table-valued function declines: its row order is not provably C's",

	// PCRE2 syntax Go's RE2 does not have (sqlean_regexp.go).
	"regexp/01_05": "lookahead",
	"regexp/09_06": "backreference",
	"regexp/09_11": "lookahead",
	"regexp/09_16": "negative lookahead",
	"regexp/09_17": "lookbehind",
	"regexp/09_18": "atomic group",
	"regexp/09_23": "$ before a trailing newline",
	"regexp/09_24": "(?x) extended mode",
	"regexp/10_04": "\\b between Unicode word characters",
	"regexp/10_05": "\\b between Unicode word characters",
}

// sqleanStatements splits a suite into its statements: lines up to one
// ending in ';', without comments and the shell's dot-commands.
func sqleanStatements(src string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if cur.Len() == 0 && (trim == "" || strings.HasPrefix(trim, "--") || strings.HasPrefix(trim, ".")) {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(trim, ";") {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	return out
}

// sqleanLabel is a test case's label, "" for a setup statement.
func sqleanLabel(stmt string) string {
	m := sqleanLabelRE.FindStringSubmatch(stmt)
	if m == nil {
		return ""
	}
	return m[1]
}

var sqleanLabelRE = regexp.MustCompile(`(?m)^(?:with .*\n)?select '(\d+_\d+[a-z]?)',`)
