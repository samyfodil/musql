package compat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sqliteconv "github.com/samyfodil/musql/convert/sqlite"
)

// TestTursoSqltests replays Turso's SQLite-compatibility corpus
// (testdata/turso-sqltests, 356 .sqltest files) against musql and C SQLite and
// fails wherever musql answers differently from C. The oracle is C SQLite, not
// the expect block Turso recorded: those blocks are only compared with C's
// answer, to report where Turso's expectation and C 3.53.3 disagree (logged,
// never a failure).
//
// Each test is its own subtest, run once per @database the file declares, on a
// fresh database: the file's setups, then the test's SQL, statement by
// statement through the same worker binaries the other differential gates use.
// What is not run is counted with its reason in the summary: EXPLAIN snapshots
// (Turso's own plans), the features neither engine under test has, fixtures
// that are corrupt by design, files the parser does not understand.
//
// A divergence is classed as
//
//	wrong       musql answers differently from C, or accepts what C rejects
//	decline     musql errors where C answers
//	error-text  both reject, with different words
//	row-order   the same rows in another order (a defect only under ORDER BY)
//	crash       the musql worker died or timed out
//	export      the file musql leaves is not accepted by C after Export
//
// and a test fails on any of them unless tsKnownDivergences names it.
func TestTursoSqltests(t *testing.T) {
	root := filepath.Join("testdata", "turso-sqltests")
	var files []string
	err := filepath.WalkDir(filepath.Join(root, "sqlite-sqltests"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".sqltest") {
			files = append(files, p)
		}
		return err
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("corpus not vendored under %s: %v", root, err)
	}
	sort.Strings(files)
	if testing.Short() {
		files = tsShortSubset(files)
	}

	st := &tsStats{counts: map[string]int{}, skips: map[string]int{}}
	fix := &tsFixtures{root: root}
	sem := make(chan struct{}, max(2, runtime.NumCPU()))
	t.Cleanup(func() {
		fix.cleanup()
		st.report(t)
		if run := flag.Lookup("test.run"); !testing.Short() && (run == nil || run.Value.String() == "") {
			for _, e := range tsKnownDivergences {
				if e.hits.Load() == 0 {
					t.Errorf("tsKnownDivergences entry %q (%s) matches no diverging test: remove it", e.pattern, e.reason)
				}
			}
		}
	})
	t.Run("files", func(t *testing.T) {
		for _, path := range files {
			rel, _ := filepath.Rel(filepath.Join(root, "sqlite-sqltests"), path)
			t.Run(rel, func(t *testing.T) {
				t.Parallel()
				sem <- struct{}{}
				defer func() { <-sem }()
				tsRunFile(t, rel, path, fix, st)
			})
		}
	})
}

// tsShortSubset keeps the cheap, broad files for go test -short: the files
// that are small enough to run in seconds, spread across the directories.
func tsShortSubset(files []string) []string {
	var out []string
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.Size() <= 24<<10 {
			out = append(out, f)
		}
	}
	return out
}

type tsStats struct {
	mu          sync.Mutex
	counts      map[string]int // ran, pass, wrong, ...
	skips       map[string]int // reason -> count
	details     []string       // one line per divergence
	disagree    []string       // Turso's expectation vs C
	parseFailed []string
}

func (s *tsStats) add(k string) {
	s.mu.Lock()
	s.counts[k]++
	s.mu.Unlock()
}

func (s *tsStats) skip(reason string) {
	s.mu.Lock()
	s.skips[reason]++
	s.mu.Unlock()
}

func (s *tsStats) report(t *testing.T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "Turso sqltests: %d ran, %d agree with C", s.counts["ran"], s.counts["pass"])
	for _, k := range []string{"wrong", "decline", "error-text", "row-order", "crash", "export", "c-bug", "known", "known-wrong", "known-decline", "known-error-text", "known-row-order", "known-crash"} {
		if s.counts[k] > 0 {
			fmt.Fprintf(&b, ", %d %s", s.counts[k], k)
		}
	}
	fmt.Fprintf(&b, "\n  C vs Turso's recorded expectation: %d disagree of %d compared", s.counts["turso-disagree"], s.counts["turso-compared"])
	var reasons []string
	total := 0
	for r, n := range s.skips {
		reasons = append(reasons, r)
		total += n
	}
	sort.Strings(reasons)
	fmt.Fprintf(&b, "\n  skipped %d:", total)
	for _, r := range reasons {
		fmt.Fprintf(&b, "\n    %5d  %s", s.skips[r], r)
	}
	t.Log(b.String())
	if p := os.Getenv("TURSO_SQLTESTS_REPORT"); p != "" {
		sort.Strings(s.details)
		sort.Strings(s.disagree)
		sort.Strings(s.parseFailed)
		var r strings.Builder
		r.WriteString(b.String())
		r.WriteString("\n\n== divergences from C ==\n" + strings.Join(s.details, "\n"))
		r.WriteString("\n\n== Turso expectation disagrees with C ==\n" + strings.Join(s.disagree, "\n"))
		r.WriteString("\n\n== files not parsed ==\n" + strings.Join(s.parseFailed, "\n") + "\n")
		os.WriteFile(p, []byte(r.String()), 0o644)
	}
}

// ---- fixtures ----

// tsFixtures builds, once, the databases a file can ask for: Turso generates
// :default: and :default-no-rowidalias: from a seeded random generator in Rust,
// which cannot be reproduced here, so the same schema is filled with
// deterministic data of this harness's own. Both engines see the same rows;
// Turso's recorded answers for those files do not apply and are not compared.
// Every fixture exists twice: the C file, and musql's, converted from it.
type tsFixtures struct {
	root    string
	once    sync.Once // dir and conv
	defOnce sync.Once // the generated :default: databases
	dir     string
	err     error
	mu      sync.Mutex
	conv    map[string]string // C file -> converted musql file
}

func (f *tsFixtures) cleanup() {
	if f.dir != "" {
		os.RemoveAll(f.dir)
	}
}

func (f *tsFixtures) setup() error {
	f.once.Do(func() {
		f.conv = map[string]string{}
		f.dir, f.err = os.MkdirTemp("", "turso-sqltests-fixtures-")
	})
	return f.err
}

func (f *tsFixtures) init() error {
	if err := f.setup(); err != nil {
		return err
	}
	var err error
	f.defOnce.Do(func() {
		for _, noAlias := range []bool{false, true} {
			name := "default.db"
			if noAlias {
				name = "default-no-rowidalias.db"
			}
			if err = tsGenerateDefaultDB(filepath.Join(f.dir, name), noAlias); err != nil {
				f.mu.Lock()
				f.err = err
				f.mu.Unlock()
				return
			}
		}
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// musql returns the musql copy of a C-format fixture, converting it once.
func (f *tsFixtures) musql(cdb string) (string, error) {
	if err := f.setup(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.conv[cdb]; ok {
		return m, nil
	}
	m := filepath.Join(f.dir, fmt.Sprintf("conv-%d.musq", len(f.conv)))
	if err := sqliteconv.Import(cdb, m, sqliteconv.ImportOptions{}); err != nil {
		return "", err
	}
	f.conv[cdb] = m
	return m, nil
}

func (f *tsFixtures) defaultDB(kind tsDBKind) string {
	if kind == tsDBDefaultNoRowid {
		return filepath.Join(f.dir, "default-no-rowidalias.db")
	}
	return filepath.Join(f.dir, "default.db")
}

var (
	tsFirstNames = []string{"Alice", "Bob", "Carol", "Dave", "Erin", "Frank", "Grace", "Heidi", "Ivan", "Judy", "Mallory", "Niaj", "Olivia", "Peggy", "Rupert", "Sybil", "Trent", "Victor", "Walter", "Zoe"}
	tsLastNames  = []string{"Smith", "Jones", "Brown", "Taylor", "Wilson", "Davies", "Evans", "Thomas", "Johnson", "Roberts", "Walker", "Wright", "Robinson", "Thompson", "White", "Hughes", "Edwards", "Green", "Hall", "Wood"}
	tsCities     = []string{"Springfield", "Riverton", "Lakeside", "Fairview", "Georgetown", "Clinton", "Salem", "Madison", "Franklin", "Greenville"}
	tsStates     = []string{"CA", "NY", "TX", "WA", "FL", "IL", "OH", "GA", "NC", "MI"}
	tsProducts   = []string{"hat", "cap", "shirt", "sweater", "sweatshirt", "shorts", "jeans", "sneakers", "boots", "coat", "accessories"}
)

// tsGenerateDefaultDB writes the users/products database of Turso's :default:
// kinds with C SQLite: same schema, 10000 deterministic users, 11 products.
func tsGenerateDefaultDB(path string, noAlias bool) error {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	pk := "INTEGER PRIMARY KEY"
	if noAlias {
		pk = "INT PRIMARY KEY"
	}
	stmts := []string{
		"CREATE TABLE users (id " + pk + ", first_name TEXT, last_name TEXT, email TEXT, phone_number TEXT, address TEXT, city TEXT, state TEXT, zipcode TEXT, age INTEGER)",
		"CREATE TABLE IF NOT EXISTS products (id " + pk + ", name TEXT, price REAL)",
	}
	if !noAlias {
		stmts = append(stmts, "CREATE INDEX age_idx ON users (age)")
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	rng := rand.New(rand.NewPCG(42, 42))
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for i := 1; i <= 10000; i++ {
		first := tsFirstNames[rng.IntN(len(tsFirstNames))]
		last := tsLastNames[rng.IntN(len(tsLastNames))]
		args := []any{
			first, last,
			strings.ToLower(first) + "." + strings.ToLower(last) + strconv.Itoa(i) + "@example.com",
			fmt.Sprintf("%03d-%03d-%04d", 200+rng.IntN(700), rng.IntN(1000), rng.IntN(10000)),
			fmt.Sprintf("%d %s Street", 1+rng.IntN(9999), tsLastNames[rng.IntN(len(tsLastNames))]),
			tsCities[rng.IntN(len(tsCities))], tsStates[rng.IntN(len(tsStates))],
			fmt.Sprintf("%05d", rng.IntN(100000)), 1 + rng.IntN(100),
		}
		if noAlias {
			_, err = tx.Exec("INSERT INTO users (id, first_name, last_name, email, phone_number, address, city, state, zipcode, age) VALUES (?,?,?,?,?,?,?,?,?,?)", append([]any{i}, args...)...)
		} else {
			_, err = tx.Exec("INSERT INTO users (first_name, last_name, email, phone_number, address, city, state, zipcode, age) VALUES (?,?,?,?,?,?,?,?,?)", args...)
		}
		if err != nil {
			return err
		}
	}
	for i, name := range tsProducts {
		price := 1 + rng.Float64()*99
		if noAlias {
			_, err = tx.Exec("INSERT INTO products (id, name, price) VALUES (?,?,?)", i+1, name, price)
		} else {
			_, err = tx.Exec("INSERT INTO products (name, price) VALUES (?,?)", name, price)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- running a case ----

type tsStmtRes struct {
	Kind string
	Cols []string
	Rows [][]string
	Err  string
}

type tsEngineRun struct {
	res   []tsStmtRes
	crash string // the worker died, timed out or returned garbage
}

func tsExecWorker(engine string, stmts []string, dsn, dir string) tsEngineRun {
	in := filepath.Join(dir, engine+"-stmts.json")
	enc, _ := json.Marshal(stmts)
	if err := os.WriteFile(in, enc, 0o644); err != nil {
		return tsEngineRun{crash: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, workerBin[engine], in)
	cmd.Env = append(os.Environ(), "COMPAT_DSN="+dsn, "COMPAT_ERRTEXT=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := err.Error()
		if ctx.Err() != nil {
			msg = "timeout after 3m"
		}
		if e := strings.TrimSpace(stderr.String()); e != "" {
			msg += ": " + tsTrunc(e, 600)
		}
		return tsEngineRun{crash: msg}
	}
	var raw []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return tsEngineRun{crash: "bad worker output: " + err.Error()}
	}
	normalizeRandomColumnSuffixes(raw)
	blankStorageOnlyColumns(raw)
	out := make([]tsStmtRes, len(raw))
	for i, r := range raw {
		out[i].Kind, _ = r["kind"].(string)
		out[i].Err, _ = r["err"].(string)
		if cols, ok := r["cols"].([]any); ok {
			for _, c := range cols {
				s, _ := c.(string)
				out[i].Cols = append(out[i].Cols, s)
			}
		}
		if rows, ok := r["rows"].([]any); ok {
			for _, row := range rows {
				var cells []string
				cs, _ := row.([]any)
				for _, c := range cs {
					s, _ := c.(string)
					cells = append(cells, s)
				}
				out[i].Rows = append(out[i].Rows, cells)
			}
		}
	}
	return tsEngineRun{res: out}
}

func tsTrunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// tsDeclineText marks a musql error as the compiler declining a statement
// rather than rejecting it for a reason C also has.
func tsDeclineText(e string) bool {
	return strings.Contains(e, "VDBE-only") || strings.Contains(e, "not compilable") ||
		strings.Contains(e, "unsupported") || strings.Contains(e, "not supported") ||
		strings.Contains(e, "not implemented")
}

// tsCompare returns "" when the two engines agree on every statement, else
// "<class>: <detail>" for the first statement that differs.
func tsCompare(stmts []string, c, m []tsStmtRes, loose bool) string {
	if len(c) != len(m) {
		return fmt.Sprintf("wrong: %d results from C, %d from musql", len(c), len(m))
	}
	for i := range c {
		a, b := c[i], m[i]
		where := tsTrunc(strings.Join(strings.Fields(stmts[i]), " "), 200)
		switch {
		case a.Kind == "error" && b.Kind == "error":
			if a.Err != b.Err {
				return fmt.Sprintf("error-text: stmt %d %q\n    C:     %s\n    musql: %s", i, where, a.Err, b.Err)
			}
		case a.Kind == "error":
			return fmt.Sprintf("wrong: stmt %d %q: C rejects (%s), musql answers %s", i, where, a.Err, tsShow(b))
		case b.Kind == "error":
			return fmt.Sprintf("decline: stmt %d %q: C answers %s, musql errors (%s)", i, where, tsShow(a), b.Err)
		case tsFirstWord(stmts[i]) == "explain":
			// Plans and bytecode are each engine's own; only whether the
			// statement is accepted is comparable.
		case loose:
			// A test whose expectation is a pattern is non-deterministic by
			// design (random(), sqlite_version()); the shape must still agree.
			if a.Kind != b.Kind || len(a.Cols) != len(b.Cols) || len(a.Rows) != len(b.Rows) {
				return fmt.Sprintf("wrong: stmt %d %q: shape differs\n    C:     %s\n    musql: %s", i, where, tsShow(a), tsShow(b))
			}
		case len(a.Rows) == 0 && len(b.Rows) == 0 && tsDDLWord(tsFirstWord(stmts[i])):
			// C hands back a column header for some DDL (ALTER ... ADD COLUMN
			// runs a check query); there is no result to compare.
		case !slices.Equal(a.Cols, b.Cols) || a.Kind != b.Kind:
			return fmt.Sprintf("wrong: stmt %d %q: columns/kind differ\n    C:     %s %v\n    musql: %s %v", i, where, a.Kind, a.Cols, b.Kind, b.Cols)
		case !tsRowsEqual(tsReconcileTimes(a.Rows, b.Rows), b.Rows):
			class := "wrong"
			if tsSameRowSet(a.Rows, b.Rows) && !strings.Contains(strings.ToLower(stmts[i]), "order by") {
				class = "row-order"
			}
			return fmt.Sprintf("%s: stmt %d %q\n    C:     %s\n    musql: %s", class, i, where, tsShow(a), tsShow(b))
		}
	}
	return ""
}

// tsFirstWord is the statement's leading keyword, lower-cased.
func tsFirstWord(stmt string) string {
	s := strings.TrimSpace(stmt)
	for strings.HasPrefix(s, "--") || strings.HasPrefix(s, "/*") {
		if strings.HasPrefix(s, "--") {
			_, rest, _ := strings.Cut(s, "\n")
			s = strings.TrimSpace(rest)
		} else {
			_, rest, _ := strings.Cut(s, "*/")
			s = strings.TrimSpace(rest)
		}
	}
	end := strings.IndexFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') })
	if end < 0 {
		end = len(s)
	}
	return strings.ToLower(s[:end])
}

func tsDDLWord(w string) bool {
	switch w {
	case "alter", "create", "drop", "attach", "detach", "vacuum", "reindex", "analyze":
		return true
	}
	return false
}

// tsReconcileTimes undoes the mattn driver's conversion of a column declared
// date/datetime/timestamp into time.Time (the worker prints those "?:<time>"),
// the way tclReconcileDriverTimes does for the other gates: a C cell that is the
// same instant as musql's text is replaced by musql's. It returns c's rows so
// adjusted.
func tsReconcileTimes(c, m [][]string) [][]string {
	if len(c) != len(m) {
		return c
	}
	out := make([][]string, len(c))
	for r := range c {
		out[r] = slices.Clone(c[r])
		if len(c[r]) != len(m[r]) {
			continue
		}
		for i, cell := range c[r] {
			rest, ok := strings.CutPrefix(cell, "?:")
			if !ok {
				continue
			}
			want, err := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", rest)
			if err == nil && tclDriverTimeMatches(m[r][i], want) {
				out[r][i] = m[r][i]
			}
		}
	}
	return out
}

func tsRowsEqual(a, b [][]string) bool {
	return slices.EqualFunc(a, b, slices.Equal[[]string])
}

func tsSameRowSet(a, b [][]string) bool {
	key := func(rs [][]string) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = strings.Join(r, "\x00")
		}
		sort.Strings(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

func tsShow(r tsStmtRes) string {
	if r.Kind == "error" {
		return "error: " + r.Err
	}
	var rows []string
	for i, row := range r.Rows {
		if i == 6 {
			rows = append(rows, fmt.Sprintf("... %d rows", len(r.Rows)))
			break
		}
		rows = append(rows, tsTrunc(strings.Join(row, "|"), 160))
	}
	return fmt.Sprintf("%d rows %v", len(r.Rows), rows)
}

// ---- Turso's recorded expectation, compared with C ----

// tsRenderCell renders a worker-normalised cell the way a SQL shell prints it:
// NULL empty, reals with 15 significant digits and always a decimal point or
// exponent, blobs as their bytes.
func tsRenderCell(c string) string {
	if c == "N" {
		return ""
	}
	if len(c) < 2 {
		return c
	}
	switch c[:2] {
	case "I:", "T:":
		return c[2:]
	case "X:":
		b, _ := hex.DecodeString(c[2:])
		return strings.ToValidUTF8(string(b), "�")
	case "F:":
		f, err := strconv.ParseFloat(c[2:], 64)
		if err != nil {
			return c[2:]
		}
		switch {
		case f > 1.7976931348623157e308:
			return "Inf"
		case f < -1.7976931348623157e308:
			return "-Inf"
		}
		s := strconv.FormatFloat(f, 'g', 15, 64)
		if mant, exp, ok := strings.Cut(s, "e"); ok {
			if !strings.Contains(mant, ".") {
				mant += ".0"
			}
			return mant + "e" + exp
		}
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	return c
}

// tsUnrenderable reports results whose cells this harness cannot print the way
// Turso's runner does: a time the oracle's driver converted, a storage figure
// the shared normalizer blanks (page_count and friends).
func tsUnrenderable(res []tsStmtRes) bool {
	for _, r := range res {
		for _, row := range r.Rows {
			for _, cell := range row {
				if strings.HasPrefix(cell, "?:") || cell == "T:<storage>" {
					return true
				}
			}
		}
	}
	return false
}

// tsTursoVerdict compares what C answered for the test's own statements with
// the expect block, as Turso's runner would: the first erroring statement
// makes the whole result an error. It returns "" on agreement.
func tsTursoVerdict(e *tsExpect, res []tsStmtRes) string {
	var rows []string
	errText := ""
	for _, r := range res {
		if r.Kind == "error" {
			errText = r.Err
			rows = nil
			break
		}
		for _, row := range r.Rows {
			cells := make([]string, len(row))
			for i, c := range row {
				cells[i] = tsRenderCell(c)
			}
			rows = append(rows, strings.Join(cells, "|"))
		}
	}
	got := strings.Join(rows, "\n")
	switch e.kind {
	case "error":
		if errText == "" {
			return "expected an error, C answered " + tsTrunc(got, 120)
		}
		if e.text == "" {
			return ""
		}
		norm := func(s string) string { return strings.Join(strings.Fields(strings.ReplaceAll(s, "│", " ")), " ") }
		re, err := regexp.Compile("(?i)" + norm(e.text))
		if err != nil || !re.MatchString(norm(errText)) {
			return fmt.Sprintf("expected error /%s/, C says %q", tsTrunc(e.text, 100), errText)
		}
		return ""
	case "pattern":
		if errText != "" {
			return "expected rows, C errors: " + errText
		}
		re, err := regexp.Compile(e.text)
		if err != nil || !re.MatchString(got) {
			return fmt.Sprintf("pattern /%s/ does not match %q", tsTrunc(e.text, 100), tsTrunc(got, 120))
		}
		return ""
	}
	if errText != "" {
		return "expected rows, C errors: " + errText
	}
	want := make([]string, len(e.lines))
	copy(want, e.lines)
	if e.kind == "unordered" {
		a, b := slices.Clone(want), slices.Clone(rows)
		sort.Strings(a)
		sort.Strings(b)
		if !slices.Equal(a, b) {
			return fmt.Sprintf("expected %q, C answers %q", tsTrunc(strings.Join(want, "\n"), 120), tsTrunc(got, 120))
		}
		return ""
	}
	if strings.Join(want, "\n") != got {
		return fmt.Sprintf("expected %q, C answers %q", tsTrunc(strings.Join(want, "\n"), 120), tsTrunc(got, 120))
	}
	return ""
}

// ---- driving a file ----

func tsRunFile(t *testing.T, rel, path string, fix *tsFixtures, st *tsStats) {
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tf, err := parseTursoSqltest(string(src))
	if err != nil {
		st.mu.Lock()
		st.parseFailed = append(st.parseFailed, rel+": "+err.Error())
		st.mu.Unlock()
		st.skip("file not parsed (unknown syntax)")
		t.Skipf("not parsed: %v", err)
	}
	for _, rq := range tf.requires {
		if tsLacks(rq.capability) {
			n := len(tf.cases) * len(tf.dbs)
			for range n {
				st.skip("needs " + rq.capability + ": neither engine under test has it")
			}
			t.Skipf("needs %s: %s", rq.capability, rq.reason)
		}
	}
	sqliteSkipFile := false
	for _, sk := range tf.skips {
		if sk.cond == "sqlite" {
			sqliteSkipFile = true
		}
	}
	for _, db := range tf.dbs {
		t.Run(db.String(), func(t *testing.T) {
			for _, c := range tf.cases {
				switch c.kind {
				case "snapshot":
					st.skip("EXPLAIN snapshot (Turso's own plan and bytecode)")
				case "matrix":
					names, sqls := tsExpandMatrix(c)
					for i := range names {
						mc := c
						mc.kind, mc.name, mc.sql = "test", names[i], sqls[i]
						mc.expect = nil
						t.Run(mc.name, func(t *testing.T) { tsRunCase(t, rel, tf, db, mc, true, sqliteSkipFile, fix, st) })
					}
				default:
					t.Run(c.name, func(t *testing.T) { tsRunCase(t, rel, tf, db, c, false, sqliteSkipFile, fix, st) })
				}
			}
		})
	}
}

// tsMathFuncs are the functions of SQLITE_ENABLE_MATH_FUNCTIONS (func.c), which
// the oracle's build does not compile in.
var tsMathFuncs = map[string]bool{
	"acos": true, "acosh": true, "asin": true, "asinh": true, "atan": true, "atan2": true, "atanh": true,
	"ceil": true, "ceiling": true, "cos": true, "cosh": true, "degrees": true, "exp": true, "floor": true,
	"ln": true, "log": true, "log10": true, "log2": true, "mod": true, "pi": true, "pow": true, "power": true,
	"radians": true, "sin": true, "sinh": true, "sqrt": true, "tan": true, "tanh": true, "trunc": true,
}

// tsLacks reports whether a capability is one neither musql nor the oracle has.
func tsLacks(capability string) bool {
	return capability == "custom_types" || capability == "materialized_views"
}

func tsRunCase(t *testing.T, rel string, tf *tsFile, db tsDB, c tsCase, matrix, sqliteSkipFile bool, fix *tsFixtures, st *tsStats) {
	for _, rq := range c.requires {
		if tsLacks(rq.capability) {
			st.skip("needs " + rq.capability + ": neither engine under test has it")
			t.Skipf("needs %s: %s", rq.capability, rq.reason)
		}
	}
	dir := t.TempDir()
	cDSN, mDSN := ":memory:", ":memory:"
	crossCheck := c.crossCheck && !db.readonly && (c.expect == nil || c.expect.kind != "error")
	file := func() { cDSN, mDSN = filepath.Join(dir, "c.db"), filepath.Join(dir, "m.musq") }
	switch db.kind {
	case tsDBMemory:
		if crossCheck {
			file()
		}
	case tsDBTemp:
		file()
	case tsDBDefault, tsDBDefaultNoRowid, tsDBPath:
		file()
		var csrc string
		if db.kind == tsDBPath {
			csrc = filepath.Join(fix.root, db.path)
			if _, err := os.Stat(csrc); err != nil {
				st.skip("fixture not vendored: " + filepath.Base(db.path) + " (Turso builds it by patching pages at run time; C-corrupt by design)")
				t.Skipf("fixture %s is generated at run time by Turso's runner", db.path)
			}
		} else {
			if err := fix.init(); err != nil {
				t.Fatalf("default database: %v", err)
			}
			csrc = fix.defaultDB(db.kind)
		}
		msrc, err := fix.musql(csrc)
		if err != nil {
			if db.kind == tsDBPath && strings.Contains(err.Error(), "integrity_check") {
				st.skip("fixture is corrupt by design: musql's importer refuses it (" + filepath.Base(db.path) + ")")
				t.Skipf("importer refuses the corrupt fixture: %v", err)
			}
			t.Fatalf("converting %s: %v", csrc, err)
		}
		if err := tsCopyFile(csrc, cDSN); err != nil {
			t.Fatal(err)
		}
		if err := tsCopyFile(msrc, mDSN); err != nil {
			t.Fatal(err)
		}
	}

	var stmts []string
	for _, name := range c.setups {
		stmts = append(stmts, tsSplitStatements(tf.setups[name])...)
	}
	nSetup := len(stmts)
	stmts = append(stmts, tsSplitStatements(c.sql)...)
	nTest := len(stmts)
	if crossCheck {
		stmts = append(stmts, "PRAGMA integrity_check")
	}

	st.add("ran")
	cr := tsExecWorker("cgo", stmts, cDSN, dir)
	mr := tsExecWorker("musql", stmts, mDSN, dir)
	key := rel + "::" + db.String() + "::" + c.name
	if cr.crash != "" {
		st.skip("the oracle itself failed or timed out")
		t.Skipf("oracle: %s", cr.crash)
	}

	for _, r := range cr.res {
		if strings.HasPrefix(r.Err, "no such table: generate_series") || strings.HasPrefix(r.Err, "no such table-valued function: generate_series") {
			st.skip("the oracle build lacks generate_series (ext/misc/series.c)")
			t.Skip("oracle lacks generate_series")
		}
	}
	// Named as a string, generate_series raises nothing in the oracle:
	// "pragma_table_info('generate_series')" is simply no rows there, while a
	// build with the module (musql's, Turso's) lists its columns.
	for _, s := range stmts {
		if strings.Contains(strings.ToLower(s), "'generate_series'") {
			st.skip("the oracle build lacks generate_series (ext/misc/series.c)")
			t.Skip("oracle lacks generate_series")
		}
	}

	for _, r := range cr.res {
		if strings.HasPrefix(r.Err, "no such table: btree_dump") || strings.HasPrefix(r.Err, "no such function: turso_") {
			st.skip("Turso-only extension (btree_dump, turso_*): neither engine under test has it")
			t.Skip("Turso-only extension")
		}
		if fn, ok := strings.CutPrefix(r.Err, "no such function: "); ok && tsMathFuncs[strings.ToLower(fn)] {
			st.skip("the oracle build lacks the SQL math functions (SQLITE_ENABLE_MATH_FUNCTIONS)")
			t.Skip("oracle lacks math functions")
		}
	}

	loose := c.expect != nil && c.expect.kind == "pattern"
	verdict := ""
	if mr.crash != "" {
		verdict = "crash: musql worker: " + mr.crash
	} else {
		verdict = tsCompare(stmts, cr.res, mr.res, loose)
	}
	if verdict == "" && crossCheck {
		if msg := tsExportCheck(mDSN, dir); msg != "" {
			verdict = "export: " + msg
		}
	}

	// Turso's own expectation against C's answer, where it applies.
	if c.expect != nil && !matrix && db.kind != tsDBDefault && db.kind != tsDBDefaultNoRowid && !sqliteSkipFile && cr.crash == "" && nTest <= len(cr.res) {
		sqliteSkip := false
		for _, sk := range c.skips {
			if sk.cond == "sqlite" {
				sqliteSkip = true
			}
		}
		if !sqliteSkip && c.backend != "cli" && nSetup <= nTest && !tsUnrenderable(cr.res[nSetup:nTest]) {
			st.add("turso-compared")
			if why := tsTursoVerdict(c.expect, cr.res[nSetup:nTest]); why != "" {
				st.add("turso-disagree")
				st.mu.Lock()
				st.disagree = append(st.disagree, key+": "+why)
				st.mu.Unlock()
			}
		}
	}

	if verdict == "" {
		st.add("pass")
		return
	}
	class, detail, _ := strings.Cut(verdict, ": ")
	// Where musql satisfies Turso's recorded expectation and C does not, C
	// 3.53.3 is the odd one out; say so in the report.
	musqlRight := c.expect != nil && !matrix && mr.crash == "" && nTest <= len(mr.res) && nSetup <= nTest &&
		db.kind != tsDBDefault && db.kind != tsDBDefaultNoRowid && tsTursoVerdict(c.expect, mr.res[nSetup:nTest]) == ""
	if musqlRight {
		detail += "\n    (musql satisfies Turso's expect block)"
	}
	if e := tsKnownFor(key); e != nil && e.cBug {
		// A recorded C SQLite bug: musql keeps the right answer, which is
		// Turso's expectation, and that is checked rather than skipped.
		if !musqlRight {
			t.Errorf("recorded C SQLite bug (%s), but musql does not give Turso's expected answer either\n  %s", e.reason, detail)
			return
		}
		st.add("c-bug")
		st.mu.Lock()
		st.details = append(st.details, fmt.Sprintf("[c-bug] %s\n    %s", key, detail))
		st.mu.Unlock()
		return
	}
	st.add(class)
	st.mu.Lock()
	st.details = append(st.details, fmt.Sprintf("[%s] %s\n    %s", class, key, detail))
	st.mu.Unlock()
	if e := tsKnownFor(key); e != nil {
		st.add("known")
		st.add("known-" + class)
		t.Skipf("known divergence (%s): %s", e.reason, verdict)
	}
	t.Errorf("musql diverges from C SQLite [%s]\n  sql: %s\n  %s", class, tsTrunc(c.sql, 400), detail)
}

func tsCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// tsExportCheck is Turso's @cross-check-integrity for musql: the file the test
// left behind is exported to the C format and C's own integrity_check must
// call it ok.
func tsExportCheck(musqPath, dir string) string {
	if _, err := os.Stat(musqPath); err != nil {
		return ""
	}
	out := filepath.Join(dir, "exported.db")
	if err := sqliteconv.Export(musqPath, out, 0); err != nil {
		return "Export: " + err.Error()
	}
	db, err := sql.Open("sqlite3", out)
	if err != nil {
		return err.Error()
	}
	defer db.Close()
	rows, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		return "C integrity_check: " + err.Error()
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err.Error()
		}
		msgs = append(msgs, s)
	}
	if len(msgs) == 1 && msgs[0] == "ok" {
		return ""
	}
	return "C integrity_check of the exported file: " + tsTrunc(strings.Join(msgs, "; "), 400)
}
