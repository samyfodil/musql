package compat

import (
	"fmt"
	"strconv"
	"strings"
)

// The .sqltest format is Turso's (testdata/turso-sqltests/README.md names the
// source and the spec). This parser implements what the vendored corpus uses.
// Anything else -- an unknown directive, capability, skip condition, backend or
// modifier -- is a parse error, and the file is then reported as skipped with
// that reason, never run on a guess.

type tsDBKind int

const (
	tsDBMemory tsDBKind = iota
	tsDBTemp
	tsDBDefault
	tsDBDefaultNoRowid
	tsDBPath
)

type tsDB struct {
	kind     tsDBKind
	path     string // relative to the corpus root, for tsDBPath
	readonly bool
}

func (d tsDB) String() string {
	switch d.kind {
	case tsDBMemory:
		return "memory"
	case tsDBTemp:
		return "temp"
	case tsDBDefault:
		return "default"
	case tsDBDefaultNoRowid:
		return "default-no-rowidalias"
	}
	return d.path
}

type tsSkip struct {
	cond   string // "" (always), "mvcc" or "sqlite"
	reason string
}

type tsRequire struct {
	capability string
	reason     string
}

// tsExpect is one expect block. kind is exact, unordered, pattern or error.
type tsExpect struct {
	kind  string
	lines []string // exact, unordered
	text  string   // pattern, error (empty error text: any error)
}

type tsVar struct {
	name   string
	values []string
}

type tsCase struct {
	kind       string // "test", "snapshot" or "matrix"
	name       string
	sql        string
	setups     []string
	skips      []tsSkip
	backend    string
	requires   []tsRequire
	crossCheck bool
	expect     *tsExpect // the default expect block of a test
	vars       []tsVar   // matrix
}

type tsFile struct {
	dbs         []tsDB
	setups      map[string]string
	cases       []tsCase
	skips       []tsSkip
	requires    []tsRequire
	exprDepth   int
	sourceBytes int
}

// tsExprDepthLimit stands in for turso_parser::MAX_EXPR_DEPTH in the
// {{expr_depth_limit}} macros. 1000 is SQLITE_MAX_EXPR_DEPTH's default, the
// limit both engines under test are held to.
const tsExprDepthLimit = 1000

type tsParser struct {
	s   string
	pos int
}

func (p *tsParser) eof() bool { return p.pos >= len(p.s) }

// skipAll skips whitespace, newlines and # comment lines.
func (p *tsParser) skipAll() {
	for p.pos < len(p.s) {
		switch c := p.s[p.pos]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			p.pos++
		case c == '#':
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *tsParser) skipInline() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t' || p.s[p.pos] == '\r') {
		p.pos++
	}
}

// word reads a run of characters up to whitespace or a block's opening brace.
func (p *tsParser) word() string {
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '{' {
			break
		}
		p.pos++
	}
	return p.s[start:p.pos]
}

func (p *tsParser) peekWord() string {
	save := p.pos
	w := p.word()
	p.pos = save
	return w
}

func (p *tsParser) str() (string, error) {
	p.skipInline()
	if p.eof() || p.s[p.pos] != '"' {
		return "", fmt.Errorf("expected a quoted string at offset %d", p.pos)
	}
	i := p.pos + 1
	for i < len(p.s) {
		if p.s[i] == '\\' {
			i += 2
			continue
		}
		if p.s[i] == '"' {
			out := p.s[p.pos+1 : i]
			p.pos = i + 1
			return out, nil
		}
		i++
	}
	return "", fmt.Errorf("unterminated string at offset %d", p.pos)
}

// block reads a {...} block the way the DSL's lexer does: nested braces count,
// \{ \} \\ are literal, the newline right after { and the one right before the
// closing } are structure.
func (p *tsParser) block() (string, error) {
	p.skipAll()
	if p.eof() || p.s[p.pos] != '{' {
		return "", fmt.Errorf("expected '{' at offset %d", p.pos)
	}
	i := p.pos + 1
	depth := 1
	var b strings.Builder
	first := true
	for i < len(p.s) {
		c := p.s[i]
		if first {
			first = false
			if c == '\n' {
				i++
				continue
			}
		}
		switch c {
		case '\\':
			if i+1 < len(p.s) {
				if n := p.s[i+1]; n == '}' || n == '{' || n == '\\' {
					b.WriteByte(n)
					i += 2
					continue
				}
			}
			b.WriteByte(c)
		case '{':
			depth++
			b.WriteByte(c)
		case '}':
			depth--
			if depth == 0 {
				p.pos = i + 1
				out := b.String()
				return strings.TrimSuffix(out, "\n"), nil
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
		i++
	}
	return "", fmt.Errorf("unterminated block at offset %d", p.pos)
}

func tsSkipCond(w string) (string, error) {
	switch w {
	case "mvcc", "sqlite":
		return w, nil
	}
	return "", fmt.Errorf("unknown skip condition %q", w)
}

func tsCapability(w string) (string, error) {
	switch w {
	case "trigger", "strict", "materialized_views", "custom_types":
		return w, nil
	}
	return "", fmt.Errorf("unknown capability %q", w)
}

// tsMacros expands {{repeat:<count>:<text>}} and {{expr_depth_limit}}.
func tsMacros(s string) (string, error) {
	count := func(expr string) (int, error) {
		expr = strings.Join(strings.Fields(expr), "")
		if expr == "expr_depth_limit" {
			return tsExprDepthLimit, nil
		}
		for _, sign := range []string{"+", "-"} {
			if rest, ok := strings.CutPrefix(expr, "expr_depth_limit"+sign); ok {
				n, err := strconv.Atoi(rest)
				if err != nil || n < 0 {
					return 0, fmt.Errorf("bad repeat count %q", expr)
				}
				if sign == "-" {
					n = -n
				}
				if tsExprDepthLimit+n < 0 {
					return 0, fmt.Errorf("repeat count underflow in %q", expr)
				}
				return tsExprDepthLimit + n, nil
			}
		}
		n, err := strconv.Atoi(expr)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad repeat count %q", expr)
		}
		return n, nil
	}
	var out strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			out.WriteString(s)
			return out.String(), nil
		}
		out.WriteString(s[:i])
		rest := s[i+2:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			return "", fmt.Errorf("unterminated block macro")
		}
		body := strings.TrimSpace(rest[:j])
		if args, ok := strings.CutPrefix(body, "repeat:"); ok {
			cnt, text, ok := strings.Cut(args, ":")
			if !ok {
				return "", fmt.Errorf("repeat macro is missing text to repeat")
			}
			n, err := count(cnt)
			if err != nil {
				return "", err
			}
			out.WriteString(strings.Repeat(text, n))
		} else {
			n, err := count(body)
			if err != nil {
				return "", err
			}
			out.WriteString(strconv.Itoa(n))
		}
		s = rest[j+2:]
	}
}

func (p *tsParser) expectBlock() (*tsExpect, string, error) {
	// optional backend qualifier: expect @js { ... }
	p.skipInline()
	qual := ""
	if !p.eof() && p.s[p.pos] == '@' {
		qual = p.word()
	}
	p.skipAll()
	mod := ""
	if !p.eof() && p.s[p.pos] != '{' {
		mod = p.word()
	}
	body, err := p.block()
	if err != nil {
		return nil, "", err
	}
	body, err = tsMacros(body)
	if err != nil {
		return nil, "", err
	}
	e := &tsExpect{}
	switch mod {
	case "":
		e.kind = "exact"
		for l := range strings.SplitSeq(body, "\n") {
			e.lines = append(e.lines, strings.TrimSpace(l))
		}
	case "raw":
		e.kind = "exact"
		e.lines = strings.Split(body, "\n")
	case "error":
		e.kind = "error"
		e.text = strings.TrimSpace(body)
	case "pattern":
		e.kind = "pattern"
		e.text = strings.TrimSpace(body)
	case "unordered":
		e.kind = "unordered"
		for l := range strings.SplitSeq(strings.TrimSpace(body), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				e.lines = append(e.lines, l)
			}
		}
	default:
		return nil, "", fmt.Errorf("unknown expect modifier %q", mod)
	}
	return e, qual, nil
}

// parseTursoSqltest parses one .sqltest file.
func parseTursoSqltest(src string) (*tsFile, error) {
	p := &tsParser{s: src}
	f := &tsFile{setups: map[string]string{}, sourceBytes: len(src)}
	for {
		p.skipAll()
		if p.eof() {
			break
		}
		w := p.word()
		switch w {
		case "@database":
			p.skipInline()
			spec := p.word()
			for _, known := range []string{":memory:", ":temp:", ":default-no-rowidalias:", ":default:"} {
				// ":memory:@database :memory:" on one line is two declarations.
				if strings.HasPrefix(spec, known) && len(spec) > len(known) {
					p.pos -= len(spec) - len(known)
					spec = known
				}
			}
			switch spec {
			case ":memory:":
				f.dbs = append(f.dbs, tsDB{kind: tsDBMemory})
			case ":temp:":
				f.dbs = append(f.dbs, tsDB{kind: tsDBTemp})
			case ":default:":
				f.dbs = append(f.dbs, tsDB{kind: tsDBDefault, readonly: true})
			case ":default-no-rowidalias:":
				f.dbs = append(f.dbs, tsDB{kind: tsDBDefaultNoRowid, readonly: true})
			case "":
				return nil, fmt.Errorf("@database without a specifier")
			default:
				if strings.HasPrefix(spec, ":") || strings.HasPrefix(spec, "@") {
					return nil, fmt.Errorf("unknown database specifier %q", spec)
				}
				d := tsDB{kind: tsDBPath, path: spec}
				p.skipInline()
				if p.peekWord() == "readonly" {
					p.word()
					d.readonly = true
				}
				f.dbs = append(f.dbs, d)
			}
		case "@skip-file":
			r, err := p.str()
			if err != nil {
				return nil, err
			}
			f.skips = append(f.skips, tsSkip{reason: r})
		case "@skip-file-if":
			p.skipInline()
			c, err := tsSkipCond(p.word())
			if err != nil {
				return nil, err
			}
			r, err := p.str()
			if err != nil {
				return nil, err
			}
			f.skips = append(f.skips, tsSkip{cond: c, reason: r})
		case "@requires-file":
			p.skipInline()
			c, err := tsCapability(p.word())
			if err != nil {
				return nil, err
			}
			r, err := p.str()
			if err != nil {
				return nil, err
			}
			f.requires = append(f.requires, tsRequire{c, r})
		case "setup":
			p.skipInline()
			name := p.word()
			body, err := p.block()
			if err != nil {
				return nil, err
			}
			if body, err = tsMacros(body); err != nil {
				return nil, err
			}
			if _, dup := f.setups[name]; dup {
				return nil, fmt.Errorf("duplicate setup %q", name)
			}
			f.setups[name] = body
		case "@setup", "@skip", "@skip-if", "@requires", "@backend", "@cross-check-integrity",
			"@sqlite-reference", "@var", "test", "snapshot", "snapshot-eqp", "matrix":
			p.pos -= len(w)
			c, err := p.parseCase()
			if err != nil {
				return nil, err
			}
			f.cases = append(f.cases, *c)
		default:
			return nil, fmt.Errorf("unknown directive or token %q at offset %d", w, p.pos-len(w))
		}
	}
	if len(f.dbs) == 0 {
		return nil, fmt.Errorf("no @database declaration")
	}
	return f, nil
}

func (p *tsParser) parseCase() (*tsCase, error) {
	c := &tsCase{}
	for {
		p.skipAll()
		w := p.peekWord()
		switch w {
		case "@var":
			p.word()
			p.skipInline()
			name := p.word()
			body, err := p.block()
			if err != nil {
				return nil, err
			}
			v := tsVar{name: name}
			for part := range strings.SplitSeq(body, "|") {
				v.values = append(v.values, strings.TrimSpace(part))
			}
			c.vars = append(c.vars, v)
			continue
		case "@setup":
			p.word()
			p.skipInline()
			c.setups = append(c.setups, p.word())
			continue
		case "@skip":
			p.word()
			r, err := p.str()
			if err != nil {
				return nil, err
			}
			c.skips = append(c.skips, tsSkip{reason: r})
			continue
		case "@skip-if":
			p.word()
			p.skipInline()
			cond, err := tsSkipCond(p.word())
			if err != nil {
				return nil, err
			}
			r, err := p.str()
			if err != nil {
				return nil, err
			}
			c.skips = append(c.skips, tsSkip{cond: cond, reason: r})
			continue
		case "@requires":
			p.word()
			p.skipInline()
			cp, err := tsCapability(p.word())
			if err != nil {
				return nil, err
			}
			r, err := p.str()
			if err != nil {
				return nil, err
			}
			c.requires = append(c.requires, tsRequire{cp, r})
			continue
		case "@backend":
			p.word()
			p.skipInline()
			b := p.word()
			switch b {
			case "rust", "cli", "js":
			default:
				return nil, fmt.Errorf("unknown backend %q", b)
			}
			c.backend = b
			continue
		case "@cross-check-integrity":
			p.word()
			c.crossCheck = true
			continue
		case "@sqlite-reference":
			p.word()
			continue
		}
		break
	}
	kw := p.word()
	switch kw {
	case "test", "snapshot", "snapshot-eqp", "matrix":
	default:
		return nil, fmt.Errorf("expected test, snapshot or matrix after decorators, got %q", kw)
	}
	c.kind = kw
	if kw == "snapshot-eqp" {
		c.kind = "snapshot"
	}
	p.skipInline()
	c.name = p.word()
	body, err := p.block()
	if err != nil {
		return nil, err
	}
	if c.sql, err = tsMacros(strings.TrimSpace(body)); err != nil {
		return nil, err
	}
	if len(c.vars) > 0 && c.kind != "matrix" {
		return nil, fmt.Errorf("@var is only valid before a matrix")
	}
	if c.kind != "test" {
		return c, nil
	}
	for {
		p.skipAll()
		if p.peekWord() != "expect" {
			break
		}
		p.word()
		e, qual, err := p.expectBlock()
		if err != nil {
			return nil, err
		}
		switch qual {
		case "":
			if c.expect != nil {
				return nil, fmt.Errorf("test %s: multiple default expect blocks", c.name)
			}
			c.expect = e
		case "@js", "@cli", "@rust":
			// A backend-specific override of the recorded expectation; the
			// default block is the one compared with C.
		default:
			return nil, fmt.Errorf("test %s: unknown expect backend %q", c.name, qual)
		}
	}
	if c.expect == nil {
		return nil, fmt.Errorf("test %s: no default expect block", c.name)
	}
	return c, nil
}

// tsExpandMatrix expands a matrix over the cross-product of its variables,
// substituting longer variable names first, as the DSL specifies.
func tsExpandMatrix(c tsCase) (names, sqls []string) {
	order := make([]int, len(c.vars))
	for i := range order {
		order[i] = i
	}
	for i := range order {
		for j := i + 1; j < len(order); j++ {
			if len(c.vars[order[j]].name) > len(c.vars[order[i]].name) {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	idx := make([]int, len(c.vars))
	for {
		sql := c.sql
		for _, vi := range order {
			sql = strings.ReplaceAll(sql, "$"+c.vars[vi].name, c.vars[vi].values[idx[vi]])
		}
		name := c.name
		for vi, v := range c.vars {
			name += "[" + tsSlug(v.values[idx[vi]]) + "]"
		}
		names = append(names, name)
		sqls = append(sqls, sql)
		pos := len(c.vars)
		for {
			if pos == 0 {
				return names, sqls
			}
			pos--
			idx[pos]++
			if idx[pos] < len(c.vars[pos].values) {
				break
			}
			idx[pos] = 0
		}
	}
}

func tsSlug(v string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range v {
		switch {
		case r < 128 && (r >= 'a' && r <= 'z' || r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case r < 128 && r >= 'A' && r <= 'Z':
			b.WriteRune(r + 'a' - 'A')
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "none"
	}
	return out
}

// tsSplitStatements splits SQL into statements the way sqlite3_complete()
// finds their ends (complete.c): a semicolon ends a statement, except inside a
// CREATE [TEMP] TRIGGER, which ends at the semicolon after its END. Statements
// holding nothing but comments and semicolons are dropped.
func tsSplitStatements(sql string) []string {
	const (
		tkSemi = iota
		tkWS
		tkOther
		tkExplain
		tkCreate
		tkTemp
		tkTrigger
		tkEnd
	)
	trans := [8][8]uint8{
		{1, 0, 2, 3, 4, 2, 2, 2},
		{1, 1, 2, 3, 4, 2, 2, 2},
		{1, 2, 2, 2, 2, 2, 2, 2},
		{1, 3, 3, 2, 4, 2, 2, 2},
		{1, 4, 2, 2, 2, 4, 5, 2},
		{6, 5, 5, 5, 5, 5, 5, 5},
		{6, 6, 5, 5, 5, 5, 5, 7},
		{1, 7, 5, 5, 5, 5, 5, 5},
	}
	isID := func(c byte) bool {
		return c == '_' || c == '$' || c >= 0x80 || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	var out []string
	var state uint8
	start := 0
	content := false
	flush := func(end int) {
		if content {
			if s := strings.TrimSpace(sql[start:end]); s != "" {
				out = append(out, s)
			}
		}
		start = end
		content = false
	}
	i := 0
	n := len(sql)
	for i < n {
		var tok int
		switch c := sql[i]; c {
		case ';':
			tok = tkSemi
		case ' ', '\r', '\t', '\n', '\f':
			tok = tkWS
		case '/':
			if i+1 >= n || sql[i+1] != '*' {
				tok = tkOther
				break
			}
			i += 2
			for i < n && !(sql[i] == '*' && i+1 < n && sql[i+1] == '/') {
				i++
			}
			if i >= n {
				i = n
			} else {
				i++
			}
			tok = tkWS
		case '-':
			if i+1 >= n || sql[i+1] != '-' {
				tok = tkOther
				break
			}
			for i < n && sql[i] != '\n' {
				i++
			}
			if i >= n {
				i = n
			}
			tok = tkWS
		case '[':
			i++
			for i < n && sql[i] != ']' {
				i++
			}
			tok = tkOther
		case '`', '"', '\'':
			i++
			for i < n && sql[i] != c {
				i++
			}
			tok = tkOther
		default:
			tok = tkOther
			if isID(c) {
				j := i + 1
				for j < n && isID(sql[j]) {
					j++
				}
				switch w := strings.ToLower(sql[i:j]); w {
				case "create":
					tok = tkCreate
				case "trigger":
					tok = tkTrigger
				case "temp", "temporary":
					tok = tkTemp
				case "end":
					tok = tkEnd
				case "explain":
					tok = tkExplain
				}
				i = j - 1
			}
		}
		if tok != tkSemi && tok != tkWS {
			content = true
		}
		state = trans[state][tok]
		i++
		if tok == tkSemi && state == 1 {
			flush(min(i, n))
		}
	}
	if content {
		flush(n)
	}
	return out
}
