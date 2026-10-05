package vdbecc

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseModule reads one module's textual IR. It accepts the subset clang emits
// for C at -O1 and above (LLVM 15+, opaque pointers); anything else is an error
// naming the line, so an unsupported construct fails loudly rather than
// compiling to something wrong.
func ParseModule(src string) (*Module, error) {
	p := &modParser{m: &Module{Types: map[string]Type{}}, lines: strings.Split(src, "\n")}
	for p.ln < len(p.lines) {
		if err := p.topLevel(); err != nil {
			return nil, fmt.Errorf("line %d: %w", p.ln, err)
		}
	}
	return p.m, nil
}

type modParser struct {
	m     *Module
	lines []string
	ln    int // the next line to read, 0-based; also the 1-based number of the last one read
}

// line reads the next line's tokens.
func (p *modParser) line() ([]token, error) {
	l := p.lines[p.ln]
	p.ln++
	return tokenize(l)
}

func (p *modParser) topLevel() error {
	toks, err := p.line()
	if err != nil || len(toks) == 0 {
		return err
	}
	c := &cursor{toks: toks}
	switch first := c.peek(); {
	case first.kind == tkMeta:
		return nil // metadata and attribute groups
	case first.kind == tkWord && set("source_filename", "target", "attributes", "module")[first.text]:
		return nil
	case first.kind == tkWord && first.text == "declare":
		for !c.done() {
			if t := c.next(); t.kind == tkGlobal {
				p.m.External = append(p.m.External, t.text)
				break
			}
		}
		return nil
	case first.kind == tkLocal: // %T = type ...
		c.next()
		if err := c.want("="); err != nil {
			return err
		}
		if err := c.want("type"); err != nil {
			return err
		}
		if c.accept("opaque") {
			return nil
		}
		t, err := parseType(c)
		if err != nil {
			return err
		}
		p.m.Types[first.text] = t
		return nil
	case first.kind == tkGlobal:
		v, err := p.global(c)
		if err != nil {
			return err
		}
		p.m.Vars = append(p.m.Vars, v)
		return nil
	case first.kind == tkWord && first.text == "define":
		f, err := p.function(c)
		if err != nil {
			return err
		}
		p.m.Funcs = append(p.m.Funcs, f)
		return nil
	}
	return fmt.Errorf("unrecognized top-level line")
}

// ---- types and values ----

func parseType(c *cursor) (Type, error) {
	t, err := parseBaseType(c)
	if err != nil {
		return nil, err
	}
	if c.is("(") { // a function type: only ever used opaquely, as a pointer
		if err := c.skipGroup(); err != nil {
			return nil, err
		}
		return PtrType{}, nil
	}
	return t, nil
}

func parseFieldList(c *cursor, close string) ([]Type, error) {
	var fs []Type
	for !c.accept(close) {
		if len(fs) > 0 {
			if err := c.want(","); err != nil {
				return nil, err
			}
		}
		f, err := parseType(c)
		if err != nil {
			return nil, err
		}
		fs = append(fs, f)
	}
	return fs, nil
}

func parseBaseType(c *cursor) (Type, error) {
	t := c.next()
	switch {
	case t.kind == tkLocal:
		return NamedType{t.text}, nil
	case t.kind == tkPunct && t.text == "[":
		n, err := strconv.ParseInt(c.next().text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("array length: %w", err)
		}
		if err := c.want("x"); err != nil {
			return nil, err
		}
		elem, err := parseType(c)
		if err != nil {
			return nil, err
		}
		return ArrayType{n, elem}, c.want("]")
	case t.kind == tkPunct && t.text == "{":
		fs, err := parseFieldList(c, "}")
		return StructType{Fields: fs}, err
	case t.kind == tkPunct && t.text == "<":
		if err := c.want("{"); err != nil {
			return nil, fmt.Errorf("vector types are not supported")
		}
		fs, err := parseFieldList(c, "}")
		if err != nil {
			return nil, err
		}
		return StructType{Fields: fs, Packed: true}, c.want(">")
	case t.kind == tkWord:
		switch t.text {
		case "ptr":
			return PtrType{}, nil
		case "void":
			return VoidType{}, nil
		case "float":
			return FloatType{}, nil
		case "double":
			return FloatType{Double: true}, nil
		}
		if bits, err := strconv.Atoi(strings.TrimPrefix(t.text, "i")); err == nil && strings.HasPrefix(t.text, "i") {
			return IntType{bits}, nil
		}
	}
	return nil, fmt.Errorf("unsupported type %q", t)
}

// parseValue reads an operand. hint, when a float type, makes numeric literals
// floats.
func parseValue(c *cursor, hint Type) (Value, error) {
	t := c.next()
	switch t.kind {
	case tkLocal:
		return Local{t.text}, nil
	case tkGlobal:
		return GlobalRef{t.text}, nil
	case tkWord:
	default:
		return nil, fmt.Errorf("expected a value, found %q", t)
	}
	switch t.text {
	case "true":
		return IntConst{1}, nil
	case "false":
		return IntConst{0}, nil
	case "null", "undef", "poison", "zeroinitializer", "none":
		return Zero{}, nil
	case "getelementptr":
		return parseConstGEP(c)
	case "bitcast", "ptrtoint", "inttoptr": // a constant cast: (T v to U)
		if err := c.want("("); err != nil {
			return nil, err
		}
		if _, err := parseType(c); err != nil {
			return nil, err
		}
		v, err := parseValue(c, nil)
		if err != nil {
			return nil, err
		}
		if err := c.want("to"); err != nil {
			return nil, err
		}
		if _, err := parseType(c); err != nil {
			return nil, err
		}
		return v, c.want(")")
	}
	if isFloatType(hint) {
		if strings.HasPrefix(t.text, "0x") {
			bits, err := strconv.ParseUint(t.text[2:], 16, 64)
			if err != nil {
				return nil, fmt.Errorf("float constant %q", t)
			}
			return FloatConst{math.Float64frombits(bits)}, nil
		}
		if f, err := strconv.ParseFloat(t.text, 64); err == nil {
			return FloatConst{f}, nil
		}
	}
	if strings.HasPrefix(t.text, "0x") {
		u, err := strconv.ParseUint(t.text[2:], 16, 64)
		if err != nil {
			return nil, fmt.Errorf("integer constant %q", t)
		}
		return IntConst{int64(u)}, nil
	}
	n, err := strconv.ParseInt(t.text, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("unsupported value %q", t)
	}
	return IntConst{n}, nil
}

var gepFlags = set("inbounds", "nuw", "nusw")

// parseConstGEP reads the rest of "getelementptr (T, ptr @g, i64 K, ...)".
func parseConstGEP(c *cursor) (Value, error) {
	c.acceptAny(gepFlags)
	if err := c.want("("); err != nil {
		return nil, err
	}
	base, err := parseType(c)
	if err != nil {
		return nil, err
	}
	if err := c.want(","); err != nil {
		return nil, err
	}
	if _, err := parseType(c); err != nil {
		return nil, err
	}
	at, err := parseValue(c, nil)
	if err != nil {
		return nil, err
	}
	g, ok := at.(GlobalRef)
	if !ok {
		return nil, fmt.Errorf("constant getelementptr on %v", at)
	}
	out := GlobalPlus{Name: g.Name, Base: base}
	for c.accept(",") {
		if _, err := parseType(c); err != nil {
			return nil, err
		}
		v, err := parseValue(c, nil)
		if err != nil {
			return nil, err
		}
		switch v := v.(type) {
		case IntConst:
			out.Indices = append(out.Indices, v.V)
		case Zero:
			out.Indices = append(out.Indices, 0)
		default:
			return nil, fmt.Errorf("non-constant index in a constant getelementptr")
		}
	}
	return out, c.want(")")
}

// attrWords are parameter and return attributes; those followed by a
// parenthesized payload or a number are handled in skipAttrs.
var attrWords = set("noundef", "signext", "zeroext", "nonnull", "noalias", "nocapture", "readonly",
	"readnone", "writeonly", "inreg", "returned", "dead_on_unwind", "writable", "dereferenceable",
	"dereferenceable_or_null", "immarg", "range", "captures", "initializes", "dead_on_return", "nofpclass", "noext")

func skipAttrs(c *cursor) error {
	for {
		switch {
		case c.accept("align"):
			c.next()
		case c.peek().kind == tkWord && attrWords[c.peek().text]:
			c.next()
			if c.is("(") {
				if err := c.skipGroup(); err != nil {
					return err
				}
			}
		default:
			return nil
		}
	}
}

// ---- globals ----

var varFlags = set("private", "internal", "external", "linkonce", "linkonce_odr", "weak", "weak_odr",
	"common", "appending", "dso_local", "dso_preemptable", "unnamed_addr", "local_unnamed_addr", "hidden",
	"protected", "externally_initialized", "constant", "global", "thread_local")

func (p *modParser) global(c *cursor) (*Var, error) {
	name := c.next().text
	if err := c.want("="); err != nil {
		return nil, err
	}
	v := &Var{Name: name}
	external := false
	for c.peek().kind == tkWord && varFlags[c.peek().text] {
		switch c.next().text {
		case "external":
			external = true
		case "internal", "private":
			v.Local = true
		}
	}
	t, err := parseType(c)
	if err != nil {
		return nil, err
	}
	env := typeEnv(p.m.Types)
	v.Align = max(env.align(t), 8)
	size := env.size(t)
	if external || c.done() || c.is(",") {
		v.Init = make([]byte, size)
		return v, nil
	}
	ib := &initBuilder{env: env}
	if err := ib.value(c, t); err != nil {
		return nil, fmt.Errorf("@%s: %w", name, err)
	}
	if int64(len(ib.bytes)) != size {
		return nil, fmt.Errorf("@%s: initializer is %d bytes, its type %d", name, len(ib.bytes), size)
	}
	v.Init, v.Fixups = ib.bytes, ib.fixups
	return v, nil // ", align N" and the like are irrelevant: Align is computed
}

// initBuilder flattens a constant initializer.
type initBuilder struct {
	env    typeEnv
	bytes  []byte
	fixups []Fixup
}

func (b *initBuilder) pad(n int64) { b.bytes = append(b.bytes, make([]byte, n)...) }

func (b *initBuilder) le(v uint64, n int) {
	for i := range n {
		b.bytes = append(b.bytes, byte(v>>(8*i)))
	}
}

func (b *initBuilder) value(c *cursor, t Type) error {
	if c.accept("zeroinitializer") || c.accept("undef") || c.accept("poison") {
		b.pad(b.env.size(t))
		return nil
	}
	switch r := b.env.resolve(t).(type) {
	case IntType:
		v, err := parseValue(c, nil)
		if err != nil {
			return err
		}
		var n int64
		switch v := v.(type) {
		case IntConst:
			n = v.V
		case Zero:
		default:
			return fmt.Errorf("integer initializer %v", v)
		}
		b.le(uint64(n), int(b.env.size(r)))
	case FloatType:
		v, err := parseValue(c, r)
		if err != nil {
			return err
		}
		var f float64
		switch v := v.(type) {
		case FloatConst:
			f = v.V
		case IntConst:
			f = float64(v.V)
		case Zero:
		default:
			return fmt.Errorf("float initializer %v", v)
		}
		if r.Double {
			b.le(math.Float64bits(f), 8)
		} else {
			b.le(uint64(math.Float32bits(float32(f))), 4)
		}
	case PtrType:
		v, err := parseValue(c, nil)
		if err != nil {
			return err
		}
		switch v.(type) {
		case Zero:
		case GlobalRef, GlobalPlus:
			b.fixups = append(b.fixups, Fixup{At: int64(len(b.bytes)), Ref: v})
		default:
			return fmt.Errorf("pointer initializer %v", v)
		}
		b.pad(8)
	case ArrayType:
		if c.peek().kind == tkStr {
			s, err := unescapeIR(c.next().text)
			if err != nil {
				return err
			}
			if int64(len(s)) != r.Len {
				return fmt.Errorf("string of %d bytes for [%d x i8]", len(s), r.Len)
			}
			b.bytes = append(b.bytes, s...)
			return nil
		}
		if err := c.want("["); err != nil {
			return err
		}
		for i := int64(0); i < r.Len; i++ {
			if i > 0 {
				if err := c.want(","); err != nil {
					return err
				}
			}
			if _, err := parseType(c); err != nil {
				return err
			}
			if err := b.value(c, r.Elem); err != nil {
				return err
			}
		}
		return c.want("]")
	case StructType:
		close := "}"
		if r.Packed {
			if err := c.want("<"); err != nil {
				return err
			}
			close = ">"
		}
		if err := c.want("{"); err != nil {
			return err
		}
		start := int64(len(b.bytes))
		end, offs := b.env.fieldOffsets(r)
		for i, f := range r.Fields {
			if i > 0 {
				if err := c.want(","); err != nil {
					return err
				}
			}
			b.pad(start + offs[i] - int64(len(b.bytes)))
			if _, err := parseType(c); err != nil {
				return err
			}
			if err := b.value(c, f); err != nil {
				return err
			}
		}
		if err := c.want("}"); err != nil {
			return err
		}
		if r.Packed {
			if err := c.want(close); err != nil {
				return err
			}
		}
		b.pad(start + roundUp(end, b.env.align(r)) - int64(len(b.bytes)))
	default:
		return fmt.Errorf("initializer for %s", t)
	}
	return nil
}

// unescapeIR decodes a c"..." body: \\ and \XX hex escapes.
func unescapeIR(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		if strings.HasPrefix(s[i:], `\\`) {
			out = append(out, '\\')
			i++
			continue
		}
		if i+2 >= len(s) {
			return nil, fmt.Errorf("truncated escape")
		}
		v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("escape %q", s[i:i+3])
		}
		out = append(out, byte(v))
		i += 2
	}
	return out, nil
}

// ---- functions ----

var funcFlags = set("internal", "private", "dso_local", "hidden", "protected", "weak", "linkonce",
	"linkonce_odr", "weak_odr", "fastcc", "coldcc", "tailcc", "noundef", "signext", "zeroext",
	"local_unnamed_addr", "unnamed_addr")

func (p *modParser) function(c *cursor) (*Func, error) {
	c.next() // define
	f := &Func{}
	for {
		if err := skipAttrs(c); err != nil {
			return nil, err
		}
		if c.peek().kind != tkWord || !funcFlags[c.peek().text] {
			break
		}
		if w := c.next().text; w == "internal" || w == "private" {
			f.Local = true
		}
	}
	res, err := parseType(c)
	if err != nil {
		return nil, err
	}
	f.Result = res
	if err := skipAttrs(c); err != nil {
		return nil, err
	}
	n := c.next()
	if n.kind != tkGlobal {
		return nil, fmt.Errorf("function name, found %q", n)
	}
	f.Name = n.text
	if err := c.want("("); err != nil {
		return nil, err
	}
	next := 0 // LLVM numbers unnamed values from 0, parameters first
	for !c.accept(")") {
		if len(f.Params) > 0 || f.Variadic {
			if err := c.want(","); err != nil {
				return nil, err
			}
		}
		if c.accept("...") {
			f.Variadic = true
			continue
		}
		t, err := parseType(c)
		if err != nil {
			return nil, err
		}
		if err := skipAttrs(c); err != nil {
			return nil, err
		}
		name := strconv.Itoa(next)
		if c.peek().kind == tkLocal {
			name = c.next().text
		}
		if k, err := strconv.Atoi(name); err == nil {
			next = k + 1
		}
		f.Params = append(f.Params, Param{name, t})
	}

	var cur *Block
	fp := &funcParser{env: typeEnv(p.m.Types)}
	for p.ln < len(p.lines) {
		raw := strings.TrimSpace(p.lines[p.ln])
		toks, err := p.line()
		if err != nil {
			return nil, err
		}
		if len(toks) == 0 {
			continue
		}
		if toks[0].kind == tkPunct && toks[0].text == "}" {
			if cur != nil && cur.End == nil {
				return nil, fmt.Errorf("block %%%s has no terminator", cur.Name)
			}
			return f, nil
		}
		if len(toks) == 2 && toks[1].kind == tkPunct && toks[1].text == ":" {
			cur = &Block{Name: toks[0].text}
			f.Blocks = append(f.Blocks, cur)
			continue
		}
		if cur == nil { // the unlabeled entry block is the next unnamed value
			cur = &Block{Name: strconv.Itoa(next)}
			f.Blocks = append(f.Blocks, cur)
		}
		// A switch's case list spans lines up to its closing bracket.
		if toks[len(toks)-1].text == "[" {
			for p.ln < len(p.lines) {
				more, err := p.line()
				if err != nil {
					return nil, err
				}
				toks = append(toks, more...)
				if len(more) > 0 && more[len(more)-1].text == "]" {
					break
				}
			}
		}
		if cur.End != nil {
			return nil, fmt.Errorf("instruction after the terminator of %%%s: %s", cur.Name, raw)
		}
		instr, term, err := fp.statement(&cursor{toks: toks})
		if err != nil {
			return nil, fmt.Errorf("%w: %s", err, raw)
		}
		if term != nil {
			cur.End = term
		} else {
			cur.Body = append(cur.Body, instr)
		}
	}
	return nil, fmt.Errorf("@%s: missing closing brace", f.Name)
}
