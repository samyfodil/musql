package engine

import (
	"errors"
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
	// The engine cannot register a collation (sql_parser.go knows only BINARY,
	// NOCASE and RTRIM), so text_nocase does not exist.
	"text/45_01": "text_nocase collation",
	"text/45_02": "text_nocase collation",
	"text/45_03": "text_nocase collation",
	"text/45_04": "text_nocase collation",
	"text/45_05": "text_nocase collation",
	"text/45_06": "text_nocase collation",
	"text/45_07": "text_nocase collation",
	"text/45_08": "text_nocase collation",
	"text/45_09": "text_nocase collation",
	"text/45_10": "text_nocase collation",
	"text/45_11": "text_nocase collation",
	"text/45_12": "text_nocase collation",
	"text/45_13": "text_nocase collation",
	"text/45_14": "text_nocase collation",
	"text/45_15": "text_nocase collation",
	"text/45_16": "text_nocase collation",
	"text/71_41": "text_nocase collation",
	"text/71_42": "text_nocase collation",
	"text/71_43": "text_nocase collation",
	"text/71_44": "text_nocase collation",
	"text/71_45": "text_nocase collation",
}

var sqleanTrailingComment = regexp.MustCompile(`;\s+--.*$`)

// sqleanStatements splits a suite into its statements: lines up to one
// ending in ';', without comments and the shell's dot-commands.
func sqleanStatements(src string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if m := sqleanTrailingComment.FindString(trim); m != "" {
			line = strings.TrimSuffix(trim, m) + ";"
			trim = strings.TrimSpace(line)
		}
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
	withSQLean(t, "stats", "uuid", "crypto")
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
	// The schema and aggregate constraints as C SQLite answers them: the schema
	// one is taken and never tested again, the aggregate one is tested against
	// every row with NUMERIC affinity.
	for _, c := range []struct{ q, want string }{
		{"SELECT path, pagetype, pgoffset, pageno, schema, aggregate FROM dbstat('MAIN', 1) WHERE name = 'small'", "[NULL NULL NULL 1 'main' 1]"},
		{"SELECT count(*) FROM dbstat WHERE aggregate = 'yes'", "[0]"},
		{"SELECT count(*) FROM dbstat('main', 'yes')", "[0]"},
		{"SELECT count(*) FROM dbstat('main', '1')", "[4]"},
		{"SELECT count(*) FROM dbstat WHERE aggregate = '1'", "[4]"},
		{"SELECT count(*) FROM dbstat('main', NULL)", "[0]"},
		{"SELECT count(*) FROM dbstat('main', 2)", "[0]"},
		{"SELECT count(*) = (SELECT count(*) FROM dbstat) FROM dbstat WHERE schema = 'MAIN'", "[1]"},
		{"SELECT DISTINCT schema, aggregate FROM dbstat('Main')", "['main' 0]"},
		{"SELECT count(*) FROM dbstat WHERE schema = 'nope'", "[0]"},
		{"SELECT count(*) FROM dbstat('nope')", "[0]"},
	} {
		rows, err := sqleanQuery(t, db, c.q)
		if err != nil {
			t.Errorf("%s: %v", c.q, err)
			continue
		}
		if got := dbstatRender(rows); got != c.want {
			t.Errorf("%s = %s, want %s", c.q, got, c.want)
		}
	}
	for _, q := range []string{
		"SELECT count(*) FROM dbstat('temp')",
		"SELECT count(*) FROM dbstat WHERE schema = 'temp'",
	} {
		if _, err := sqleanQuery(t, db, q); !errors.Is(err, errVDBEUnsupported) {
			t.Errorf("%s: %v, want a decline", q, err)
		}
	}
	if err := db.Exec("CREATE VIRTUAL TABLE elsewhere USING dbstat(nope)"); err == nil {
		if _, err := sqleanQuery(t, db, "SELECT * FROM elsewhere"); err == nil || !strings.Contains(err.Error(), "no such database: nope") {
			t.Errorf("dbstat(nope): %v", err)
		}
	}
}

func dbstatRender(rows [][]Value) string {
	var out []string
	for _, r := range rows {
		var f []string
		for _, v := range r {
			switch v.Typ {
			case Null:
				f = append(f, "NULL")
			case Text:
				f = append(f, "'"+string(v.S)+"'")
			default:
				f = append(f, valueToText(v))
			}
		}
		out = append(out, "["+strings.Join(f, " ")+"]")
	}
	return strings.Join(out, " ")
}

// TestRegexpReplaceEmptyMatches: regexp_replace replaces matches as
// pcre2_substitute's global loop finds them (the answers are Python's re.sub,
// which loops the same way), an empty match right after another match
// included; where PCRE2 would retry an empty match's place for a non-empty one
// that exists, it declines.
func TestRegexpReplaceEmptyMatches(t *testing.T) {
	withSQLean(t, "regexp")
	db, err := Create(filepath.Join(t.TempDir(), "r.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, c := range [][3]string{
		{"abc", "b*", "-a--c-"},
		{"abxd", "x*", "-a-b--d-"},
		{"", "x*", "-"},
		{"aaa", "a*", "--"},
		{"ab cd", `\b`, "-ab- -cd-"},
		{"héllo", "l*", "-h-é--o-"},
		{"a\nb", "(?m)^", "-a\n-b"},
		{"xay", "a|", "-x--y-"},
		{"abc", "b", "a-c"},
	} {
		q := fmt.Sprintf("SELECT regexp_replace(replace('%s', '\\n', char(10)), '%s', '-')", c[0], c[1])
		rows, err := sqleanQuery(t, db, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if got, want := string(rows[0][0].S), strings.ReplaceAll(c[2], `\n`, "\n"); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	if rows, err := sqleanQuery(t, db, `SELECT regexp_replace('abc', '(b*)', '<$1>')`); err != nil || string(rows[0][0].S) != "<>a<b><>c<>" {
		t.Errorf("groups of empty matches: %v %v", rows, err)
	}
	for _, c := range [][2]string{{"b", "|b"}, {"ab", "b??"}} {
		q := fmt.Sprintf("SELECT regexp_replace('%s', '%s', '-')", c[0], c[1])
		if _, err := sqleanQuery(t, db, q); !errors.Is(err, errVDBEUnsupported) {
			t.Errorf("%s: %v, want a decline", q, err)
		}
	}
}

// TestRegisteredAggregateOrderRisk: where the compiler could not prove the
// rows arrive in C SQLite's order (orderStrict), a registered aggregate whose
// result depends on that order declines once two contributions differ, and
// one that sorts its rows does not.
func TestRegisteredAggregateOrderRisk(t *testing.T) {
	withSQLean(t, "crypto", "stats")
	a, b := Value{Typ: Text, S: []byte("a")}, Value{Typ: Text, S: []byte("b")}
	one, two := Value{Typ: Int, I: 1}, Value{Typ: Int, I: 2}
	for _, c := range []struct {
		name  string
		vals  []Value
		risky bool
	}{
		{"md5_agg", []Value{a, a, a}, false},
		{"md5_agg", []Value{a, b}, true},
		{"sha256_agg", []Value{b, a}, true},
		{"stddev", []Value{one, two}, true},
		{"stddev", []Value{two, two}, false},
		{"median", []Value{two, one, two}, false},
	} {
		it := &aggItem{aggItemPlan: &aggItemPlan{kind: aggUDF, udaf: udafs[c.name], orderStrict: true}}
		it.rowRegs[aggExprArg] = 1
		for _, v := range c.vals {
			if err := it.stepUDF(nil, []Value{v}); err != nil {
				t.Fatal(err)
			}
		}
		if err := it.orderRiskErr(); (err != nil) != c.risky {
			t.Errorf("%s over %v: %v, want risky=%v", c.name, c.vals, err, c.risky)
		}
	}
}

// TestRegisterSQLeanTakenName: a family whose function name is already taken
// registers nothing, of it or of the families named with it, so registering
// without it afterwards succeeds.
func TestRegisterSQLeanTakenName(t *testing.T) {
	sf, nc, uf, ua := maps.Clone(supportedFuncs), maps.Clone(nonConstantFuncs), maps.Clone(udfs), maps.Clone(udafs)
	reg, rows, mods := maps.Clone(sqleanRegistered), sqleanFuncRows, sqleanModuleNames
	t.Cleanup(func() {
		for _, m := range sqleanModuleNames[len(mods):] {
			unregisterVtabModule(m)
		}
		supportedFuncs, nonConstantFuncs, udfs, udafs = sf, nc, uf, ua
		sqleanRegistered, sqleanFuncRows, sqleanModuleNames = reg, rows, mods
	})
	if err := RegisterFunction(ScalarFunction{Name: "md5", NArg: 1, Fn: func([]Value) (Value, error) { return Value{}, nil }}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterSQLean("stats", "crypto"); err == nil || !strings.Contains(err.Error(), "md5 is already a function") {
		t.Fatalf("RegisterSQLean(stats, crypto) = %v", err)
	}
	if udafs["median"] != nil || supportedFuncs["sqlean_version"] || len(sqleanFuncRows) != len(rows) {
		t.Fatal("a failed RegisterSQLean registered some of its functions")
	}
	if err := RegisterSQLean("stats"); err != nil {
		t.Fatalf("RegisterSQLean(stats) after the failure: %v", err)
	}
	if udafs["median"] == nil || !supportedFuncs["sqlean_version"] {
		t.Fatal("RegisterSQLean(stats) registered nothing")
	}
}
