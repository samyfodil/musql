package compat

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file runs the sqllogictest (SLT) corpus against the differential
// harness. It discards SLT's expected-result blocks and instead runs each
// file's SQL sequence through differ() to check musql and modernc.org/sqlite
// against C SQLite on real-world queries SLT encodes.

// sltTestdataDir is where the curated SLT subset lives.
const sltTestdataDir = "testdata/slt"

// parseSLTFile reads one .test file and returns SQL texts that survive the
// "sqlite" conditional filter, with rowsort/valuesort queries wrapped in a
// deterministic ORDER BY to ensure reproducible row ordering.
func parseSLTFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // some SLT lines/result blocks are long
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	var out []string
	var pending []condition // skipif/onlyif lines accumulated ahead of the next record
	i := 0
	n := len(lines)

	for i < n {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			i++

		case strings.HasPrefix(trimmed, "#"):
			i++

		case strings.HasPrefix(trimmed, "skipif ") || trimmed == "skipif":
			pending = append(pending, condition{skip: true, db: conditionDB(trimmed)})
			i++

		case strings.HasPrefix(trimmed, "onlyif ") || trimmed == "onlyif":
			pending = append(pending, condition{skip: false, db: conditionDB(trimmed)})
			i++

		case strings.HasPrefix(trimmed, "hash-threshold"):
			pending = nil
			i++

		case trimmed == "halt" || strings.HasPrefix(trimmed, "halt "):
			include := conditionsAllowSQLite(pending)
			pending = nil
			i++
			if include {
				return out, nil
			}

		case strings.HasPrefix(trimmed, "statement "):
			include := conditionsAllowSQLite(pending)
			pending = nil
			i++
			var body []string
			for i < n && strings.TrimSpace(lines[i]) != "" {
				body = append(body, lines[i])
				i++
			}
			if include {
				if sql := strings.TrimSpace(strings.Join(body, "\n")); sql != "" {
					out = append(out, splitTopLevelStatements(sql)...)
				}
			}

		case strings.HasPrefix(trimmed, "query "):
			include := conditionsAllowSQLite(pending)
			pending = nil
			fields := strings.Fields(trimmed)
			typestring := ""
			sortmode := "nosort"
			if len(fields) > 1 {
				typestring = fields[1]
			}
			if len(fields) > 2 {
				sortmode = fields[2]
			}
			i++
			var body []string
			for i < n && strings.TrimSpace(lines[i]) != "----" {
				body = append(body, lines[i])
				i++
			}
			if i < n { // skip the "----" line
				i++
			}
			// Discard the expected-result block: consume through the next
			// blank line (or EOF). We never compare against it -- differ()
			// compares the three engines directly.
			for i < n && strings.TrimSpace(lines[i]) != "" {
				i++
			}
			if include {
				sql := strings.TrimSpace(strings.Join(body, "\n"))
				if sql != "" {
					out = append(out, wrapForDeterministicOrder(sql, typestring, sortmode))
				}
			}

		default:
			// Unrecognized line outside of any record (e.g. stray prose).
			// Don't let it wedge the parser; drop any half-formed
			// conditional state and move on.
			pending = nil
			i++
		}
	}
	return out, nil
}

type condition struct {
	skip bool // true => skipif, false => onlyif
	db   string
}

// conditionDB extracts the db token from a "skipif <db>" / "onlyif <db>"
// line, ignoring any trailing "# comment".
func conditionDB(trimmed string) string {
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return ""
	}
	db := fields[1]
	return strings.ToLower(db)
}

// conditionsAllowSQLite applies the "sqlite" filter described above.
func conditionsAllowSQLite(pending []condition) bool {
	hasOnlyif := false
	onlyifSQLite := false
	for _, c := range pending {
		if c.skip && c.db == "sqlite" {
			return false
		}
		if !c.skip {
			hasOnlyif = true
			if c.db == "sqlite" {
				onlyifSQLite = true
			}
		}
	}
	if hasOnlyif && !onlyifSQLite {
		return false
	}
	return true
}

// wrapForDeterministicOrder wraps unordered rowsort/valuesort queries in a
// derived table ordered by every result column so all engines return rows in
// the same order. Queries with existing ORDER BY clauses are left unchanged.
func wrapForDeterministicOrder(sql, typestring, sortmode string) string {
	if sortmode != "rowsort" && sortmode != "valuesort" {
		return sql
	}
	if strings.Contains(strings.ToLower(sql), "order by") {
		return sql
	}
	n := len(typestring)
	if n == 0 {
		return sql
	}
	ordinals := make([]string, n)
	for k := 0; k < n; k++ {
		ordinals[k] = strconv.Itoa(k + 1)
	}
	return "SELECT * FROM (\n" + sql + "\n) AS _slt_sort ORDER BY " + strings.Join(ordinals, ",")
}

// splitTopLevelStatements splits a body containing multiple semicolon-terminated
// statements into individual statements by tracking quote/comment state and
// BEGIN/CASE...END nesting depth, splitting only at depth 0.
func splitTopLevelStatements(body string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	runes := []rune(body)
	n := len(runes)

	isWordChar := func(r rune) bool {
		return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}
	matchKeyword := func(pos int, kw string) bool {
		if pos+len(kw) > n {
			return false
		}
		for k, want := range kw {
			c := runes[pos+k]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != want {
				return false
			}
		}
		if pos > 0 && isWordChar(runes[pos-1]) {
			return false
		}
		if pos+len(kw) < n && isWordChar(runes[pos+len(kw)]) {
			return false
		}
		return true
	}
	flush := func() {
		s := strings.TrimSpace(cur.String())
		s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
		if s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}

	i := 0
	for i < n {
		r := runes[i]
		switch r {
		case '\'', '"', '`':
			quote := r
			cur.WriteRune(r)
			i++
			for i < n {
				cur.WriteRune(runes[i])
				if runes[i] == quote {
					if i+1 < n && runes[i+1] == quote { // doubled-quote escape
						i++
						cur.WriteRune(runes[i])
						i++
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		case '[':
			cur.WriteRune(r)
			i++
			for i < n {
				cur.WriteRune(runes[i])
				done := runes[i] == ']'
				i++
				if done {
					break
				}
			}
			continue
		case '-':
			if i+1 < n && runes[i+1] == '-' {
				for i < n && runes[i] != '\n' {
					cur.WriteRune(runes[i])
					i++
				}
				continue
			}
		case '/':
			if i+1 < n && runes[i+1] == '*' {
				cur.WriteRune(runes[i])
				cur.WriteRune(runes[i+1])
				i += 2
				for i < n && !(runes[i] == '*' && i+1 < n && runes[i+1] == '/') {
					cur.WriteRune(runes[i])
					i++
				}
				if i < n {
					cur.WriteRune(runes[i])
					cur.WriteRune(runes[i+1])
					i += 2
				}
				continue
			}
		}
		switch {
		case matchKeyword(i, "begin"):
			// A "begin" only opens a nested (trigger-body) scope when it
			// appears mid-statement -- e.g. "CREATE TRIGGER foo ... BEGIN"
			// -- so the trigger body's internal semicolons don't cause a
			// false split. A bare transaction "BEGIN;" is the FIRST token
			// of a fresh top-level statement (nothing accumulated in cur
			// since the last flush) and has no matching "END": it is
			// closed later by COMMIT/ROLLBACK, not END. Treating it as a
			// depth-opener would swallow everything up to the next literal
			// "end" (or the whole rest of the block) into one opaque
			// statement. So: only increment depth when cur already has
			// non-whitespace content.
			if !(depth == 0 && strings.TrimSpace(cur.String()) == "") {
				depth++
			}
		case matchKeyword(i, "case"):
			depth++
		case matchKeyword(i, "end"):
			if depth > 0 {
				depth--
			}
		case r == ';' && depth == 0:
			cur.WriteRune(r)
			flush()
			i++
			continue
		}
		cur.WriteRune(r)
		i++
	}
	flush()
	return out
}

// sltIncludedInShort returns whether the file should run under `go test -short`.
// Only the classic core (select1-3) and evidence/ files run in short mode.
func sltIncludedInShort(relpath string) bool {
	if strings.HasPrefix(relpath, "evidence"+string(filepath.Separator)) {
		return true
	}
	base := filepath.Base(relpath)
	return base == "select1.test" || base == "select2.test" || base == "select3.test"
}

func TestSLT(t *testing.T) {
	root := sltTestdataDir
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".test") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("no .test files found under %s -- corpus not vendored?", root)
	}

	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		if testing.Short() && !sltIncludedInShort(rel) {
			continue
		}
		path, rel := path, rel
		t.Run(rel, func(t *testing.T) {
			stmts, err := parseSLTFile(path)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			if len(stmts) == 0 {
				t.Skipf("no records survived the sqlite filter in %s", rel)
			}
			differ(t, rel, stmts)
		})
	}
}
