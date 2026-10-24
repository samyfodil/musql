package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The read plan cache keeps a compiled Program across commits -- this
// connection's and another's -- because a Program binds its rows when it runs.
// Every check below runs a statement twice: by its own text, which the cache
// serves, and by a text the cache has never held, which compiles afresh. The
// harness cannot see this: its differ prepares every statement anew.

type reuseQuery struct {
	q    string
	args []Value
}

type reuseProbe struct {
	t     *testing.T
	fresh int
}

// same runs q from the cache and from a fresh compile and fails unless both
// answer alike, column names and errors included.
func (r *reuseProbe) same(s *Session, q reuseQuery) {
	r.t.Helper()
	r.fresh++
	cols, rows, err := s.Query(q.q, q.args)
	fcols, frows, ferr := s.Query(q.q+strings.Repeat(" ", r.fresh), q.args)
	got, want := fmt.Sprint(cols, rows, err), fmt.Sprint(fcols, frows, ferr)
	if got != want {
		r.t.Fatalf("%s\n cached plan: %.400s\n fresh plan:  %.400s", q.q, got, want)
	}
}

func cachedPlanOf(s *Session, q string) *Program {
	if s.DB.plans == nil {
		return nil
	}
	return s.DB.plans.m[q]
}

// cachedPlans runs each query once and returns the plans the cache then
// holds. Most shapes are cached; the test is about those.
func cachedPlans(t *testing.T, s *Session, r *reuseProbe, qs []reuseQuery) map[string]*Program {
	t.Helper()
	plans := map[string]*Program{}
	for _, q := range qs {
		r.same(s, q)
		if p := cachedPlanOf(s, q.q); p != nil {
			plans[q.q] = p
		}
	}
	if len(plans) < len(qs)/2 {
		t.Fatalf("only %d of %d queries cached", len(plans), len(qs))
	}
	return plans
}

func reuseExec(t *testing.T, s *Session, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if err := s.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func reuseCommit(t *testing.T, s *Session) {
	t.Helper()
	if _, err := s.Commit(); err != nil {
		t.Fatal(err)
	}
}

func reuseSchema(t *testing.T, s *Session) {
	t.Helper()
	reuseExec(t, s,
		`CREATE TABLE t(a INTEGER PRIMARY KEY, b TEXT, c REAL)`,
		`CREATE INDEX t_b ON t(b)`,
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
	)
	reuseCommit(t, s)
}

func reuseQueries() []reuseQuery {
	return []reuseQuery{
		{`SELECT b, c FROM t WHERE a = ?`, []Value{{Typ: Int, I: 5}}},
		{`SELECT count(*), sum(a), min(b), max(c) FROM t`, nil},
		{`SELECT a, b FROM t WHERE b BETWEEN 'k1' AND 'k3' ORDER BY b, a LIMIT 40`, nil},
		{`SELECT t.a, u.v FROM t JOIN u ON u.k = t.a WHERE t.a % 7 = 0 ORDER BY t.a LIMIT 40`, nil},
		{`SELECT substr(b, 1, 2) AS p, count(*) FROM t GROUP BY p ORDER BY p`, nil},
		{`SELECT a FROM t WHERE a IN (SELECT k FROM u WHERE v > 3) ORDER BY a LIMIT 20`, nil},
		{`WITH x AS (SELECT a, c FROM t WHERE c > 10) SELECT count(*), avg(c) FROM x`, nil},
		{`SELECT a, sum(c) OVER (ORDER BY a ROWS 2 PRECEDING) FROM t WHERE a < 30 ORDER BY a`, nil},
		{`SELECT a FROM t WHERE b = 'k5' OR a = 7 ORDER BY a`, nil},
		{`SELECT a, (SELECT count(*) FROM u WHERE u.k <= t.a) FROM t WHERE a < 20 ORDER BY a`, nil},
		{`SELECT a FROM t WHERE c < 5`, nil},
		{`SELECT count(*) FROM t WHERE c > 100.5`, nil},
		{`SELECT sum(c), count(b) FROM t WHERE a > 10`, nil},
		{`SELECT * FROM t WHERE a BETWEEN 2 AND 4`, nil},
		{`SELECT (SELECT max(a) FROM t), (SELECT count(*) FROM u), EXISTS (SELECT 1 FROM u WHERE v = 7)`, nil},
		{`SELECT a FROM t WHERE a = (SELECT max(k) FROM u)`, nil},
		{`SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u WHERE v = 1) OFFSET (SELECT count(*) FROM u WHERE v = 2)`, nil},
		{`SELECT count(*) FROM (SELECT a FROM t LIMIT (SELECT count(*) FROM u WHERE v = 1))`, nil},
		{`SELECT (SELECT group_concat(a) FROM (SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u WHERE v = 2)))`, nil},
		{`SELECT k FROM u WHERE k IN (SELECT a FROM t ORDER BY a LIMIT (SELECT count(*) FROM u WHERE v = 3)) ORDER BY k`, nil},
		{`SELECT a FROM t UNION SELECT k FROM u ORDER BY 1 LIMIT (SELECT count(*) FROM u WHERE v = 4)`, nil},
		{`SELECT a, row_number() OVER (ORDER BY a) FROM t ORDER BY a LIMIT (SELECT count(*) FROM u WHERE v = 5)`, nil},
		{`SELECT b, count(*) FROM t GROUP BY b HAVING count(*) > 0 ORDER BY b LIMIT (SELECT count(*) FROM u WHERE v = 6)`, nil},
		{`SELECT count(*) FROM (SELECT DISTINCT v FROM u) AS d`, nil},
		{`SELECT max(a) FROM t UNION ALL SELECT min(k) FROM u`, nil},
	}
}

// reuseWrites is one round of writes per step: a table that grows from empty
// across the sizes and storage layouts a commit can leave it in (one row, the
// change log, a bulk load, updates, deletes).
func reuseWrites(t *testing.T, s *Session, step int, next *int) {
	t.Helper()
	insert := func(n int) {
		for i := 0; i < n; i++ {
			*next++
			a := *next
			reuseExec(t, s,
				fmt.Sprintf(`INSERT INTO t VALUES(%d, 'k%d', %d.5)`, a, a%97, a%211),
				fmt.Sprintf(`INSERT INTO u VALUES(%d, %d)`, a, a%11))
		}
	}
	switch step {
	case 0:
		insert(1)
	case 1:
		insert(9)
	case 2:
		insert(1000)
	case 3:
		for i := 0; i < 30; i++ {
			insert(1)
			reuseCommit(t, s)
		}
	case 4:
		reuseExec(t, s, `UPDATE t SET c = c + 100 WHERE a % 2 = 0`, `UPDATE u SET v = v * 2 WHERE k % 3 = 0`)
	case 5:
		reuseExec(t, s, `DELETE FROM t WHERE a % 3 = 1`, `DELETE FROM u WHERE k % 5 = 0`)
	case 6:
		insert(6000)
	case 7:
		reuseExec(t, s, `DELETE FROM t WHERE a > 5000`)
	}
	reuseCommit(t, s)
}

const reuseSteps = 8

func TestPlanReuseAcrossOwnCommits(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "own.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	reuseSchema(t, s)
	r := &reuseProbe{t: t}
	qs := reuseQueries()
	plans := cachedPlans(t, s, r, qs)
	next := 0
	for step := 0; step < reuseSteps; step++ {
		reuseWrites(t, s, step, &next)
		if step == 6 {
			if _, err := s.CompactIfLargerThan(0); err != nil {
				t.Fatal(err)
			}
		}
		for _, q := range qs {
			r.same(s, q)
			if p := plans[q.q]; p != nil && cachedPlanOf(s, q.q) != p {
				t.Fatalf("step %d: %s was compiled again after a commit that changed no schema", step, q.q)
			}
		}
	}
}

func TestPlanReuseAcrossOtherConnectionsCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.musq")
	a, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	reuseSchema(t, a)
	b, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	r := &reuseProbe{t: t}
	qs := reuseQueries()
	plans := cachedPlans(t, a, r, qs)
	refresh := func(s *Session) {
		t.Helper()
		if _, err := s.RefreshIfStale(); err != nil {
			t.Fatal(err)
		}
	}
	next := 0
	for step := 0; step < reuseSteps; step++ {
		refresh(b)
		reuseWrites(t, b, step, &next)
		refresh(a)
		for _, q := range qs {
			r.same(a, q)
			if p := plans[q.q]; p != nil && cachedPlanOf(a, q.q) != p {
				t.Fatalf("step %d: %s was compiled again after another connection's commit that changed no schema", step, q.q)
			}
		}
	}
	// A schema change by the other connection is seen, whatever it does to a
	// plan this one holds: an index it used is dropped, one it may now use
	// appears, a column joins "SELECT *", and statistics arrive.
	for _, ddl := range []string{
		`CREATE INDEX t_c ON t(c)`,
		`ALTER TABLE t ADD COLUMN d DEFAULT 'dd'`,
		`DROP INDEX t_b`,
		`ANALYZE`,
		`DROP TABLE u`,
		`CREATE TABLE u(k INTEGER PRIMARY KEY, v)`,
		`INSERT INTO u SELECT a, a % 4 FROM t`,
	} {
		refresh(b)
		reuseExec(t, b, ddl)
		reuseCommit(t, b)
		refresh(a)
		for _, q := range qs {
			r.same(a, q)
		}
	}
}

func TestPlanReuseRollbackAndSavepoints(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "rb.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	reuseSchema(t, s)
	next := 0
	reuseWrites(t, s, 1, &next)
	r := &reuseProbe{t: t}
	qs := reuseQueries()
	all := func() {
		t.Helper()
		for _, q := range qs {
			r.same(s, q)
		}
	}
	all()
	reuseExec(t, s, `BEGIN`, `INSERT INTO t VALUES(100, 'k5', 3.5)`, `DELETE FROM u WHERE k = 3`)
	all()
	reuseExec(t, s, `ROLLBACK`)
	all()
	reuseExec(t, s, `BEGIN`, `SAVEPOINT a`, `UPDATE t SET b = 'k1' WHERE a < 5`, `ALTER TABLE t ADD COLUMN d DEFAULT 'x'`)
	all()
	reuseExec(t, s, `ROLLBACK TO a`)
	all()
	reuseExec(t, s, `SAVEPOINT b`, `CREATE INDEX t_c ON t(c)`, `ANALYZE`)
	all()
	reuseExec(t, s, `RELEASE b`, `COMMIT`)
	all()
}

// bm25, rank, snippet and highlight compile against the corpus statistics of
// the MATCH (fts5AuxState), so their answers must follow every write to the
// table: inside a transaction, after its commit, and after another
// connection's.
func TestPlanReuseFollowsFts5Corpus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.musq")
	a, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	reuseExec(t, a, `CREATE VIRTUAL TABLE ft USING fts5(x)`, `INSERT INTO ft VALUES('a b c'), ('a a d'), ('e f g')`)
	reuseCommit(t, a)
	r := &reuseProbe{t: t}
	qs := []reuseQuery{
		{q: `SELECT rowid, bm25(ft) FROM ft WHERE ft MATCH 'a' ORDER BY rowid`},
		{q: `SELECT rowid FROM ft WHERE ft MATCH 'a OR q' ORDER BY rank`},
		{q: `SELECT highlight(ft, 0, '[', ']') FROM ft WHERE ft MATCH 'a' ORDER BY rank, rowid`},
	}
	all := func(s *Session) {
		t.Helper()
		for _, q := range qs {
			r.same(s, q)
		}
	}
	all(a)
	reuseExec(t, a, `BEGIN`, `INSERT INTO ft VALUES('h i j'), ('k l m'), ('n o p'), ('a q r')`)
	all(a)
	reuseExec(t, a, `COMMIT`)
	all(a)
	b, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	reuseExec(t, b, `INSERT INTO ft VALUES('q q q'), ('a'), ('s t u')`, `DELETE FROM ft WHERE rowid = 2`)
	reuseCommit(t, b)
	if _, err := a.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	all(a)
}

// An fts5 table's "rank" row decides the function a compiled ORDER BY rank
// calls, and the row is data: the plan must follow it through a ROLLBACK and
// through another connection's commit, neither of which moves the schema.
func TestPlanReuseFollowsFts5Rank(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rank.musq")
	a, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	reuseExec(t, a,
		`CREATE VIRTUAL TABLE tt USING fts5(x, y)`,
		`INSERT INTO tt VALUES('w', 'z z z z z z z z')`,
		`INSERT INTO tt VALUES('z z z z z z z z', 'w')`,
		`INSERT INTO tt VALUES('w z', 'z')`,
		`INSERT INTO tt VALUES('z', 'w z z')`,
	)
	reuseCommit(t, a)
	r := &reuseProbe{t: t}
	q := reuseQuery{q: `SELECT rowid FROM tt('w') ORDER BY rank`}
	r.same(a, q)
	reuseExec(t, a, `BEGIN`, `INSERT INTO tt(tt, rank) VALUES('rank', 'bm25(1000.0, 0.001)')`)
	r.same(a, q)
	reuseExec(t, a, `ROLLBACK`)
	r.same(a, q)

	b, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	reuseExec(t, b, `INSERT INTO tt(tt, rank) VALUES('rank', 'bm25(0.001, 1000.0)')`)
	reuseCommit(t, b)
	if _, err := a.RefreshIfStale(); err != nil {
		t.Fatal(err)
	}
	r.same(a, q)
}

// Each flag a compile reads keeps its cached plan apart, with commits in
// between that leave the schema alone.
func TestPlanReuseFollowsPragmas(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "pragma.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	reuseSchema(t, s)
	reuseExec(t, s,
		`INSERT INTO t VALUES(1, 'Abc', 1.5), (2, 'abd', 2.5), (3, 'ABE', 3.5), (4, 'xyz', 4.5)`,
		`INSERT INTO u VALUES(1, 1), (2, 2)`,
		`CREATE VIRTUAL TABLE ft USING fts5(body)`,
		`INSERT INTO ft VALUES('alpha beta'), ('beta gamma')`,
		`CREATE VIEW vm AS SELECT rowid FROM ft WHERE ft MATCH 'beta'`,
	)
	reuseCommit(t, s)
	r := &reuseProbe{t: t}
	qs := []reuseQuery{
		{`SELECT a FROM t WHERE b LIKE 'ab%' ORDER BY a`, nil},
		{`SELECT t.a, u.v FROM t, u WHERE t.a = u.k`, nil},
		{`SELECT a FROM t`, nil},
		{`SELECT a FROM t WHERE b > 'a' AND c > 0`, nil},
		{`SELECT * FROM vm`, nil},
	}
	n := 10
	for _, pragma := range []string{
		`PRAGMA case_sensitive_like = ON`, `PRAGMA case_sensitive_like = OFF`,
		`PRAGMA full_column_names = ON`, `PRAGMA short_column_names = OFF`,
		`PRAGMA full_column_names = OFF`, `PRAGMA short_column_names = ON`,
		`PRAGMA reverse_unordered_selects = ON`, `PRAGMA reverse_unordered_selects = OFF`,
		`PRAGMA automatic_index = OFF`, `PRAGMA automatic_index = ON`,
		`PRAGMA trusted_schema = OFF`, `PRAGMA trusted_schema = ON`,
	} {
		for _, q := range qs {
			r.same(s, q)
		}
		reuseExec(t, s, pragma)
		n++
		reuseExec(t, s, fmt.Sprintf(`INSERT INTO u VALUES(%d, 0)`, n))
		reuseCommit(t, s)
		for _, q := range qs {
			r.same(s, q)
		}
	}
}

// An attached database is read through its own pager, and a commit to it by
// another connection is seen on the next statement.
func TestPlanReuseWithAttachments(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(filepath.Join(dir, "main.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	reuseSchema(t, a)
	aux, err := Create(filepath.Join(dir, "aux.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer aux.Close()
	reuseExec(t, aux, `CREATE TABLE x(k INTEGER PRIMARY KEY, w)`, `INSERT INTO x VALUES(1, 'one')`)
	reuseCommit(t, aux)
	reuseExec(t, a, fmt.Sprintf(`ATTACH %q AS aux`, filepath.Join(dir, "aux.musq")))
	r := &reuseProbe{t: t}
	qs := []reuseQuery{
		{`SELECT k, w FROM aux.x ORDER BY k`, nil},
		{`SELECT count(*) FROM x`, nil},
		{`SELECT t.a, x.w FROM t JOIN aux.x ON x.k = t.a ORDER BY t.a`, nil},
		{`SELECT count(*) FROM t`, nil},
	}
	for i := 2; i < 8; i++ {
		for _, q := range qs {
			r.same(a, q)
		}
		reuseExec(t, aux, fmt.Sprintf(`INSERT INTO x VALUES(%d, 'w%d')`, i, i))
		reuseCommit(t, aux)
		reuseExec(t, a, fmt.Sprintf(`INSERT INTO t VALUES(%d, 'k', 1.5)`, i))
		reuseCommit(t, a)
		if _, err := a.RefreshIfStale(); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range qs {
		r.same(a, q)
	}
}

// A write program whose LIMIT holds a subquery folds the count it compiled
// with, so the write cache must not keep it: the cached statement and a fresh
// compile of the same statement, run against twin tables, must stay in step.
func TestWriteProgramLimitFoldIsNotCached(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "wfold.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	reuseExec(t, s,
		`CREATE TABLE t1(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE t2(a INTEGER PRIMARY KEY, b)`,
		`CREATE TABLE u(k INTEGER PRIMARY KEY)`,
		`INSERT INTO t1 VALUES(1, 1), (2, 2), (3, 3), (4, 4)`,
		`INSERT INTO t2 SELECT * FROM t1`,
	)
	const w = `UPDATE %s SET b = b + 10 WHERE a IN (SELECT a FROM %[1]s ORDER BY a LIMIT (SELECT count(*) FROM u))`
	for i := 1; i <= 3; i++ {
		// The write cache keys on the trimmed text, so the fresh twin differs by a comment.
		reuseExec(t, s, fmt.Sprintf(w, "t1"), fmt.Sprintf(w, "t2")+fmt.Sprintf(" /* %d */", i))
		reuseExec(t, s, fmt.Sprintf(`INSERT INTO u VALUES(%d)`, i))
		reuseCommit(t, s)
		_, cached, _ := s.Query(`SELECT a, b FROM t1 ORDER BY a`, nil)
		_, fresh, _ := s.Query(`SELECT a, b FROM t2 ORDER BY a`, nil)
		if fmt.Sprint(cached) != fmt.Sprint(fresh) {
			t.Fatalf("round %d: the cached program left %v, a fresh compile %v", i, cached, fresh)
		}
	}
}

// A plan now outlives many commits, so it must not hold the pager it was
// compiled against: that pins the statement's whole read source.
func TestCachedPlanHoldsNoPager(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "pin.musq"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	reuseSchema(t, s)
	next := 0
	reuseWrites(t, s, 1, &next)
	for _, q := range []string{
		`SELECT a FROM t WHERE b = 'k5' OR a = 7 ORDER BY a`,
		`SELECT k, (SELECT max(u.v + t.a) OVER () FROM t LIMIT 1) FROM u ORDER BY k`,
	} {
		if _, _, err := s.Query(q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		prog := cachedPlanOf(s, q)
		if prog == nil {
			t.Fatalf("%s: not cached", q)
		}
		if path := pagerRefIn(reflect.ValueOf(prog), "plan", map[uintptr]bool{}); path != "" {
			t.Fatalf("%s: the cached plan holds a pager at %s", q, path)
		}
	}
}

var pagerType = reflect.TypeOf((*ReadOnlyPager)(nil))

// pagerRefIn returns the path to the first *ReadOnlyPager reachable from v.
func pagerRefIn(v reflect.Value, path string, seen map[uintptr]bool) string {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return ""
		}
		if v.Type() == pagerType {
			return path
		}
		if seen[v.Pointer()] {
			return ""
		}
		seen[v.Pointer()] = true
		return pagerRefIn(v.Elem(), path, seen)
	case reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return pagerRefIn(v.Elem(), path, seen)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if p := pagerRefIn(v.Field(i), path+"."+v.Type().Field(i).Name, seen); p != "" {
				return p
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if p := pagerRefIn(v.Index(i), path+"[]", seen); p != "" {
				return p
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if p := pagerRefIn(it.Value(), path+"{}", seen); p != "" {
				return p
			}
		}
	}
	return ""
}
