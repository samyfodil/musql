package engine

import (
	"fmt"
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

	// The *_agg hash aggregates (crypto.sql sections 27-29).
	"crypto/27_01": "crypto_*_agg aggregates are not implemented",
	"crypto/27_02": "crypto_*_agg aggregates are not implemented",
	"crypto/27_03": "crypto_*_agg aggregates are not implemented",
	"crypto/27_04": "crypto_*_agg aggregates are not implemented",
	"crypto/27_05": "crypto_*_agg aggregates are not implemented",
	"crypto/27_06": "crypto_*_agg aggregates are not implemented",
	"crypto/27_07": "crypto_*_agg aggregates are not implemented",
	"crypto/27_08": "crypto_*_agg aggregates are not implemented",
	"crypto/27_09": "crypto_*_agg aggregates are not implemented",
	"crypto/27_10": "crypto_*_agg aggregates are not implemented",
	"crypto/27_11": "crypto_*_agg aggregates are not implemented",
	"crypto/27_12": "crypto_*_agg aggregates are not implemented",
	"crypto/27_13": "crypto_*_agg aggregates are not implemented",
	"crypto/27_14": "crypto_*_agg aggregates are not implemented",
	"crypto/27_15": "crypto_*_agg aggregates are not implemented",
	"crypto/27_16": "crypto_*_agg aggregates are not implemented",
	"crypto/27_17": "crypto_*_agg aggregates are not implemented",
	"crypto/27_18": "crypto_*_agg aggregates are not implemented",
	"crypto/28_01": "crypto_*_agg aggregates are not implemented",
	"crypto/28_02": "crypto_*_agg aggregates are not implemented",
	"crypto/28_03": "crypto_*_agg aggregates are not implemented",
	"crypto/28_04": "crypto_*_agg aggregates are not implemented",
	"crypto/28_05": "crypto_*_agg aggregates are not implemented",
	"crypto/28_06": "crypto_*_agg aggregates are not implemented",
	"crypto/28_07": "crypto_*_agg aggregates are not implemented",
	"crypto/28_08": "crypto_*_agg aggregates are not implemented",
	"crypto/28_09": "crypto_*_agg aggregates are not implemented",
	"crypto/28_10": "crypto_*_agg aggregates are not implemented",
	"crypto/29_01": "crypto_*_agg aggregates are not implemented",
	"crypto/29_02": "crypto_*_agg aggregates are not implemented",
	"crypto/29_03": "crypto_*_agg aggregates are not implemented",
	"crypto/29_04": "crypto_*_agg aggregates are not implemented",
	"crypto/29_05": "crypto_*_agg aggregates are not implemented",
	"crypto/29_06": "crypto_*_agg aggregates are not implemented",
	"crypto/29_07": "crypto_*_agg aggregates are not implemented",
	"crypto/29_08": "crypto_*_agg aggregates are not implemented",
	"crypto/29_09": "crypto_*_agg aggregates are not implemented",
	"crypto/29_10": "crypto_*_agg aggregates are not implemented",

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

func sqleanQuery(t *testing.T, db *Session, q string) ([][]Value, error) {
	t.Helper()
	pg, err := db.SnapshotPager()
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	_, rows, err := pg.QueryArgs(q, nil)
	return rows, err
}

// TestSQLeanRegistration covers what RegisterSQLean adds besides the
// functions' bodies: the arity errors, the pragma lists, and that a
// family is opt-in.
func TestSQLeanRegistration(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := sqleanQuery(t, db, "SELECT median(1)"); err == nil || !strings.Contains(err.Error(), "no such function") {
		t.Fatalf("median before RegisterSQLean: %v", err)
	}
	if err := RegisterSQLean("nope"); err == nil {
		t.Fatal("RegisterSQLean(nope) succeeded")
	}
	withSQLean(t, "stats", "uuid")
	if err := RegisterSQLean("stats"); err != nil {
		t.Fatalf("registering a family twice: %v", err)
	}
	for q, want := range map[string]string{
		"SELECT uuid7(1, 2)":           "wrong number of arguments to function uuid7()",
		"SELECT median(1, 2)":          "wrong number of arguments to function median()",
		"SELECT percentile(1)":         "wrong number of arguments to function percentile()",
		"SELECT percentile(1, 101)":    "percentile should be a number between 0 and 100",
		"SELECT regexp_like('a', 'a')": "no such function",
		"SELECT abs(1) OVER ()":        "may not be used as a window function",
		"SELECT median(value) FILTER (WHERE value > 1) FROM stats_seq(1, 4)": "",
	} {
		_, err := sqleanQuery(t, db, q)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
	rows, err := sqleanQuery(t, db, "SELECT type, narg, flags FROM pragma_function_list WHERE name IN ('median', 'uuid7', 'uuid_str') ORDER BY name, narg")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || string(rows[0][0].S) != "w" || rows[3][2].I != 2048 || rows[1][2].I != 0 {
		t.Errorf("function_list rows %v, want median/1 (w), uuid7/0, uuid7/1 (not deterministic), uuid_str/1", rows)
	}
	rows, err = sqleanQuery(t, db, "SELECT name FROM pragma_module_list WHERE name = 'stats_seq'")
	if err != nil || len(rows) != 1 {
		t.Errorf("module_list stats_seq: %v %v", rows, err)
	}
	rows, err = sqleanQuery(t, db, "SELECT g, median(v), stats_var_pop(v) FROM (SELECT value % 2 g, value v FROM stats_seq(1, 6)) GROUP BY g ORDER BY g")
	if err != nil || fmt.Sprint(rows) != fmt.Sprint([][]Value{
		{{Typ: Int, I: 0}, {Typ: Float, F: 4}, {Typ: Float, F: 8.0 / 3}},
		{{Typ: Int, I: 1}, {Typ: Float, F: 3}, {Typ: Float, F: 8.0 / 3}},
	}) {
		t.Errorf("grouped stats: %v %v", rows, err)
	}
}

// TestDBStat checks dbstat's shape and that its sizes follow the data.
func TestDBStat(t *testing.T) {
	RegisterDBStat()
	t.Cleanup(func() { unregisterVtabModule("dbstat") })
	db, err := Create(filepath.Join(t.TempDir(), "d.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE small(a INTEGER PRIMARY KEY, b TEXT)",
		"CREATE TABLE big(a, b)",
		"CREATE INDEX big_b ON big(b)",
		"CREATE VIEW v AS SELECT 1",
		"INSERT INTO small VALUES (1, 'x')",
		"INSERT INTO big SELECT value, printf('%.*c', 100, 'y') FROM generate_series(1, 50)",
	} {
		if err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	rows, err := sqleanQuery(t, db, "SELECT name, ncell, pgsize > (SELECT pgsize FROM dbstat WHERE name = 'small') FROM dbstat ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s:%d:%d", r[0].S, r[1].I, r[2].I))
	}
	if want := "big:50:1 big_b:50:1 small:1:0 sqlite_schema:4:1"; strings.Join(got, " ") != want {
		t.Errorf("dbstat = %v, want %s", got, want)
	}
	if rows, err := sqleanQuery(t, db, "SELECT count(*) FROM dbstat('main', 1)"); err != nil || rows[0][0].I != 4 {
		t.Errorf("dbstat('main', 1): %v %v", rows, err)
	}
	if _, err := sqleanQuery(t, db, "SELECT name FROM pragma_module_list WHERE name = 'dbstat'"); err != nil {
		t.Error(err)
	}
}
