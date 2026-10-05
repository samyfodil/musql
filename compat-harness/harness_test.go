package compat

import (
	"strings"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// engines built once (TestMain), each a worker binary linked against one driver.
var workerBin = map[string]string{}

// engineOrder is the comparison order; "cgo" (C SQLite) is the oracle.
var engineOrder = []string{"cgo", "musql"}

var buildTag = map[string]string{
	"cgo":    "cgoengine",
	"musql": "musql",
}

// workerExt is each engine's own file extension: C's file and ours.
var workerExt = map[string]string{
	"cgo":    ".db",
	"musql": ".musq",
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "compat-workers-")
	if err != nil {
		panic(err)
	}
	for eng, tag := range buildTag {
		bin := filepath.Join(dir, "w-"+eng)
		cmd := exec.Command("go", "build", "-tags", tag, "-o", bin, "./worker")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1") // cgo worker needs it; harmless for the rest
		if out, err := cmd.CombinedOutput(); err != nil {
			panic("build " + eng + ": " + err.Error() + "\n" + string(out))
		}
		workerBin[eng] = bin
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run executes stmts against one engine and returns its normalized JSON results.
func run(t *testing.T, engine string, stmts []string) []map[string]any {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stmts-*.json")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := json.Marshal(stmts)
	f.Write(enc)
	f.Close()

	cmd := exec.Command(workerBin[engine], f.Name())
	cmd.Env = append(os.Environ(),
		"COMPAT_DSN="+filepath.Join(t.TempDir(), engine+workerExt[engine]),
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s worker failed: %v", engine, err)
	}
	var res []map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("%s: bad worker output: %v\n%s", engine, err, out)
	}
	normalizeRandomColumnSuffixes(res)
	blankStorageOnlyColumns(res)
	return res
}

// storageOnlyColumns are columns that vary by implementation storage, not data.
var storageOnlyColumns = []string{"rootpage", "page_count", "freelist_count"}

// blankStorageOnlyColumns blanks storage-only columns for comparison.
func blankStorageOnlyColumns(res []map[string]any) {
	for _, r := range res {
		cols, ok := r["cols"].([]any)
		if !ok {
			continue
		}
		var blank []int
		for i, c := range cols {
			name, _ := c.(string)
			lower := strings.ToLower(name)
			for _, s := range storageOnlyColumns {
				if lower == s || strings.HasSuffix(lower, "."+s) {
					blank = append(blank, i)
					break
				}
			}
		}
		if len(blank) == 0 {
			continue
		}
		rows, ok := r["rows"].([]any)
		if !ok {
			continue
		}
		for _, raw := range rows {
			cells, ok := raw.([]any)
			if !ok {
				continue
			}
			for _, i := range blank {
				if i < len(cells) {
					cells[i] = "<storage>"
				}
			}
		}
	}
}

// randomColumnSuffixRe matches a result-column name's ":<digits>" suffix.
var randomColumnSuffixRe = regexp.MustCompile(`^(.*):([0-9]+)$`)

// normalizeRandomColumnSuffixes normalizes random column name suffixes for comparison.
func normalizeRandomColumnSuffixes(res []map[string]any) {
	for _, r := range res {
		raw, ok := r["cols"].([]any)
		if !ok {
			continue
		}
		for i, c := range raw {
			name, ok := c.(string)
			if !ok {
				continue
			}
			m := randomColumnSuffixRe.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			if n, err := strconv.Atoi(m[2]); err == nil && n > 4 {
				raw[i] = m[1] + ":?"
			}
		}
	}
}

// differ runs stmts through every engine and fails if any diverges from the
// C-SQLite oracle. Returns true on agreement (used by the fuzzer to keep going).
func differ(t *testing.T, name string, stmts []string) bool {
	t.Helper()
	got := map[string][]byte{}
	for _, eng := range engineOrder {
		res := run(t, eng, stmts)
		b, _ := json.Marshal(res)
		got[eng] = b
	}
	oracle := got["cgo"]
	ok := true
	for _, eng := range []string{"musql"} {
		if string(got[eng]) != string(oracle) {
			ok = false
			t.Errorf("[%s] %s DIVERGES from C SQLite\n  sql:     %v\n  cgo:     %s\n  %-7s %s",
				name, eng, stmts, oracle, eng+":", got[eng])
		}
	}
	return ok
}

// corpus: curated SQL exercising types, coercion, aggregates, joins, dates,
// transactions, constraints, and error paths. Each case is a statement sequence
// run on one connection.
var corpus = []struct {
	name  string
	stmts []string
}{
	{"types", []string{
		"CREATE TABLE t(i INTEGER, r REAL, s TEXT, b BLOB, n)",
		"INSERT INTO t VALUES(1, 2.5, 'hi', x'0a0b', NULL)",
		"INSERT INTO t VALUES(9223372036854775807, -0.0, '', x'', 0)",
		"SELECT i,r,s,b,n, typeof(i),typeof(r),typeof(s),typeof(b),typeof(n) FROM t",
	}},
	{"affinity-coercion", []string{
		"CREATE TABLE a(x INTEGER, y TEXT)",
		"INSERT INTO a VALUES('42','42'),(3.0,3.0),('3.5x', 9)",
		"SELECT x,y,typeof(x),typeof(y) FROM a ORDER BY rowid",
	}},
	{"arithmetic", []string{
		"SELECT 7/2, 7.0/2, 7%3, -7%3, 5&3, 5|2, 1<<10, abs(-3), round(2.5), round(3.5), round(2.345,2)",
	}},
	{"strings", []string{
		"SELECT upper('AbÇ'), lower('AbÇ'), length('héllo'), substr('hello',2,3), replace('aaa','a','bb'), trim('  x  '), instr('abcabc','bc'), hex(x'deadbeef')",
	}},
	{"null-logic", []string{
		"SELECT NULL=NULL, NULL IS NULL, 1=NULL, NULL AND 0, NULL OR 1, coalesce(NULL,NULL,3), ifnull(NULL,'d')",
	}},
	{"aggregates-group", []string{
		"CREATE TABLE s(g TEXT, v INTEGER)",
		"INSERT INTO s VALUES('a',1),('a',2),('b',10),('b',NULL),('c',5)",
		"SELECT g, count(*), count(v), sum(v), avg(v), min(v), max(v), group_concat(v,'-') FROM s GROUP BY g ORDER BY g",
		"SELECT total(v), sum(v) FROM s WHERE g='x'",
	}},
	{"joins-subquery", []string{
		"CREATE TABLE u(id INTEGER PRIMARY KEY, name TEXT)",
		"CREATE TABLE o(uid INTEGER, amt INTEGER)",
		"INSERT INTO u VALUES(1,'a'),(2,'b'),(3,'c')",
		"INSERT INTO o VALUES(1,10),(1,20),(2,5)",
		"SELECT u.name, coalesce(sum(o.amt),0) FROM u LEFT JOIN o ON o.uid=u.id GROUP BY u.id ORDER BY u.id",
		"SELECT name FROM u WHERE id IN (SELECT uid FROM o WHERE amt>8) ORDER BY name",
	}},
	{"order-limit-distinct", []string{
		"CREATE TABLE p(v INTEGER)",
		"INSERT INTO p VALUES(3),(1),(2),(3),(1),(NULL)",
		"SELECT DISTINCT v FROM p ORDER BY v",
		"SELECT v FROM p ORDER BY v DESC NULLS LAST LIMIT 3",
	}},
	{"transactions", []string{
		"CREATE TABLE tx(v INTEGER)",
		"BEGIN",
		"INSERT INTO tx VALUES(1)",
		"ROLLBACK",
		"INSERT INTO tx VALUES(2)",
		"SELECT count(*), sum(v) FROM tx",
	}},
	{"constraint-errors", []string{
		"CREATE TABLE c(id INTEGER PRIMARY KEY, u TEXT UNIQUE, nn TEXT NOT NULL)",
		"INSERT INTO c VALUES(1,'a','x')",
		"INSERT INTO c VALUES(1,'b','y')",   // PK conflict -> error
		"INSERT INTO c VALUES(2,'a','z')",   // UNIQUE conflict -> error
		"INSERT INTO c(id,u) VALUES(3,'c')", // NOT NULL -> error
		"SELECT count(*) FROM c",
	}},
	{"dates", []string{
		"SELECT date('2024-02-29'), datetime('2024-01-01 12:00:00','+1 day','-2 hours'), strftime('%Y-%W','2024-06-15'), julianday('2000-01-01')",
	}},
	{"cast", []string{
		"SELECT CAST('123abc' AS INTEGER), CAST('3.14' AS REAL), CAST(3.99 AS INTEGER), CAST(65 AS TEXT), CAST(x'41' AS TEXT)",
	}},
	// The following four cases pin PRAGMA recursive_triggers=ON firing a
	// REPLACE conflict's implicit victim-row DELETE triggers (engine/
	// trigger.go's replaceDeleteTriggers/fireReplaceVictimDelete), and the
	// post-trigger uniqueness recheck (replaceRecheckHit, conflict.go) that
	// aborts the whole statement when the trigger reintroduces a conflict.
	// See engine/trigger_replace_delete_test.go for the same shapes pinned
	// directly against musql, with the exact error wording/oracle evidence
	// each one was verified against.
	{"replace-victim-delete-trigger-fires", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)",
		"CREATE TABLE log(kind TEXT, a INTEGER)",
		"INSERT INTO t VALUES(1,'x')",
		"CREATE TRIGGER bd BEFORE DELETE ON t BEGIN INSERT INTO log VALUES('bdel', old.a); END",
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO log VALUES('del', old.a); END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(2,'x')", // UNIQUE-index conflict on b, different rowid
		"SELECT * FROM t ORDER BY a",
		"SELECT kind,a FROM log ORDER BY rowid",
	}},
	{"replace-victim-delete-trigger-recheck-aborts", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)",
		"INSERT INTO t VALUES(1,'x')",
		"INSERT INTO t VALUES(3,'z')",
		// An AFTER (not BEFORE, to avoid re-colliding with the not-yet-deleted
		// victim and recursing -- see TestReplaceVictimOrconfInherited)
		// DELETE trigger updates a SIBLING row to the value the candidate is
		// about to take; the post-trigger recheck must catch it and abort the
		// WHOLE statement, undoing the trigger's own sibling-row UPDATE too.
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN UPDATE t SET b='x' WHERE a=3; END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(2,'x')", // errors on both engines
		"SELECT * FROM t ORDER BY a",             // unchanged: (1,x),(3,z)
	}},
	{"replace-victim-delete-trigger-multi-table-rollback", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b UNIQUE)",
		"CREATE TABLE other(x INTEGER)",
		"INSERT INTO t VALUES(1,'x')",
		"INSERT INTO t VALUES(3,'z')",
		"CREATE TRIGGER ad AFTER DELETE ON t BEGIN INSERT INTO other VALUES(old.a); UPDATE t SET b='x' WHERE a=3; END",
		"PRAGMA recursive_triggers=ON",
		"INSERT OR REPLACE INTO t VALUES(2,'x')", // errors; other's own insert must roll back too
		"SELECT * FROM t ORDER BY a",
		"SELECT * FROM other",
	}},
	{"replace-victim-delete-trigger-raise-ignore-rowid", []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b)",
		"INSERT INTO t VALUES(1,'one')",
		"CREATE TRIGGER bd BEFORE DELETE ON t BEGIN SELECT RAISE(IGNORE); END",
		"PRAGMA recursive_triggers=ON",
		// The victim survives (RAISE(IGNORE)); C SQLite's recheck
		// explicitly re-tests rowid EXISTENCE rather than letting the final
		// INSERT silently overwrite it -- errors on both engines.
		"INSERT OR REPLACE INTO t VALUES(1,'three')",
		"SELECT * FROM t",
	}},
}

func TestCorpus(t *testing.T) {
	for _, c := range corpus {
		t.Run(c.name, func(t *testing.T) {
			differ(t, c.name, c.stmts)
		})
	}
}

