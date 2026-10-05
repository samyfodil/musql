package vdbecc

import (
	"fmt"
	"strings"
)

// funcParser parses one function body's statements.
type funcParser struct{ env typeEnv }

// instrParser parses what follows an opcode. dst is "" for an unnamed result.
type instrParser func(fp *funcParser, c *cursor, op, dst string) (Instr, Term, error)

var arithOps = map[string]Arith{
	"add": ArAdd, "sub": ArSub, "mul": ArMul, "sdiv": ArSDiv, "udiv": ArUDiv, "srem": ArSRem,
	"urem": ArURem, "and": ArAnd, "or": ArOr, "xor": ArXor, "shl": ArShl, "ashr": ArAShr, "lshr": ArLShr,
	"fadd": ArAdd, "fsub": ArSub, "fmul": ArMul, "fdiv": ArFDiv,
}

var intPreds = map[string]Pred{
	"eq": PredEQ, "ne": PredNE, "slt": PredLT, "sle": PredLE, "sgt": PredGT, "sge": PredGE,
	"ult": PredULT, "ule": PredULE, "ugt": PredUGT, "uge": PredUGE,
}

// Float predicates drop the ordered/unordered distinction: nothing compiled
// here produces a NaN.
var floatPreds = map[string]Pred{"eq": PredEQ, "ne": PredNE, "lt": PredLT, "le": PredLE, "gt": PredGT, "ge": PredGE}

var convOps = map[string]Conv{
	"sext": ConvSExt, "zext": ConvZExt, "trunc": ConvTrunc,
	"bitcast": ConvSame, "ptrtoint": ConvSame, "inttoptr": ConvSame, "freeze": ConvSame,
	"sitofp": ConvIntFloat, "uitofp": ConvIntFloat, "fptosi": ConvFloatInt, "fptoui": ConvFloatInt,
	"fpext": ConvFloatSize, "fptrunc": ConvFloatSize,
}

var opFlags = set("nsw", "nuw", "exact", "disjoint", "nneg", "samesign", "volatile", "inbounds", "nusw",
	"fast", "nnan", "ninf", "nsz", "arcp", "contract", "afn", "reassoc", "tail", "musttail", "notail",
	"fastcc", "coldcc", "tailcc", "swiftcc")

var instrTable map[string]instrParser

func init() {
	instrTable = map[string]instrParser{
		"fneg": parseNeg, "icmp": parseCmp, "fcmp": parseCmp, "select": parseSelect, "phi": parsePhi,
		"alloca": parseAlloca, "load": parseLoad, "store": parseStore, "getelementptr": parseGEP,
		"call": parseCall, "va_arg": parseVaArg, "br": parseBr, "switch": parseSwitch, "ret": parseRet,
		"unreachable": func(*funcParser, *cursor, string, string) (Instr, Term, error) { return nil, &Trap{}, nil },
	}
	for op := range arithOps {
		instrTable[op] = parseBin
	}
	for op := range convOps {
		instrTable[op] = parseConv
	}
}

func (fp *funcParser) statement(c *cursor) (Instr, Term, error) {
	dst := ""
	if c.peek().kind == tkLocal {
		dst = c.next().text
		if err := c.want("="); err != nil {
			return nil, nil, err
		}
	}
	c.acceptAny(set("tail", "musttail", "notail"))
	op := c.next().text
	parse, ok := instrTable[op]
	if !ok {
		return nil, nil, fmt.Errorf("unsupported instruction %q", op)
	}
	c.acceptAny(opFlags)
	instr, term, err := parse(fp, c, op, dst)
	if err == nil && instr != nil && dst == "" {
		if _, isCall := instr.(*CallInstr); !isCall {
			if _, isStore := instr.(*StoreInstr); !isStore {
				err = fmt.Errorf("%s needs a result name", op)
			}
		}
	}
	return instr, term, err
}

// typed reads "T v", returning both.
func typed(c *cursor) (Type, Value, error) {
	t, err := parseType(c)
	if err != nil {
		return nil, nil, err
	}
	if err := skipAttrs(c); err != nil {
		return nil, nil, err
	}
	v, err := parseValue(c, t)
	return t, v, err
}

// pair reads "T x, y".
func pair(c *cursor) (Type, Value, Value, error) {
	t, x, err := typed(c)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := c.want(","); err != nil {
		return nil, nil, nil, err
	}
	y, err := parseValue(c, t)
	return t, x, y, err
}

// target reads "label %name".
func target(c *cursor) (string, error) {
	if err := c.want("label"); err != nil {
		return "", err
	}
	t := c.next()
	if t.kind != tkLocal {
		return "", fmt.Errorf("label, found %q", t)
	}
	return t.text, nil
}

func parseBin(_ *funcParser, c *cursor, op, dst string) (Instr, Term, error) {
	t, x, y, err := pair(c)
	return &BinInstr{Dst: dst, Op: arithOps[op], Type: t, X: x, Y: y}, nil, err
}

func parseNeg(_ *funcParser, c *cursor, _, dst string) (Instr, Term, error) {
	_, x, err := typed(c)
	return &NegInstr{Dst: dst, X: x}, nil, err
}

func parseCmp(_ *funcParser, c *cursor, op, dst string) (Instr, Term, error) {
	word := c.next().text
	float := op == "fcmp"
	pred, ok := intPreds[word]
	if float {
		pred, ok = floatPreds[strings.TrimLeft(word, "ou")]
	}
	if !ok {
		return nil, nil, fmt.Errorf("%s predicate %q", op, word)
	}
	t, x, y, err := pair(c)
	return &CmpInstr{Dst: dst, Pred: pred, Type: t, X: x, Y: y, Float: float}, nil, err
}

func parseConv(_ *funcParser, c *cursor, op, dst string) (Instr, Term, error) {
	from, x, err := typed(c)
	if err != nil {
		return nil, nil, err
	}
	to := from
	if op != "freeze" {
		if err := c.want("to"); err != nil {
			return nil, nil, err
		}
		if to, err = parseType(c); err != nil {
			return nil, nil, err
		}
	}
	return &ConvInstr{Dst: dst, Kind: convOps[op], From: from, To: to, X: x}, nil, nil
}

func parseSelect(_ *funcParser, c *cursor, _, dst string) (Instr, Term, error) {
	var vs [3]Value
	for i := range vs {
		if i > 0 {
			if err := c.want(","); err != nil {
				return nil, nil, err
			}
		}
		_, v, err := typed(c)
		if err != nil {
			return nil, nil, err
		}
		vs[i] = v
	}
	return &SelectInstr{Dst: dst, Cond: vs[0], T: vs[1], F: vs[2]}, nil, nil
}

func parsePhi(_ *funcParser, c *cursor, _, dst string) (Instr, Term, error) {
	t, err := parseType(c)
	if err != nil {
		return nil, nil, err
	}
	phi := &PhiInstr{Dst: dst}
	for {
		if err := c.want("["); err != nil {
			return nil, nil, err
		}
		v, err := parseValue(c, t)
		if err != nil {
			return nil, nil, err
		}
		if err := c.want(","); err != nil {
			return nil, nil, err
		}
		from := c.next()
		if from.kind != tkLocal {
			return nil, nil, fmt.Errorf("phi predecessor %q", from)
		}
		if err := c.want("]"); err != nil {
			return nil, nil, err
		}
		phi.In = append(phi.In, PhiEdge{v, from.text})
		if !c.accept(",") {
			return phi, nil, nil
		}
	}
}

func parseAlloca(_ *funcParser, c *cursor, _, dst string) (Instr, Term, error) {
	t, err := parseType(c)
	if err != nil {
		return nil, nil, err
	}
	a := &AllocaInstr{Dst: dst, Type: t, Count: 1}
	for c.accept(",") {
		if c.accept("align") {
			c.next()
			continue
		}
		_, n, err := typed(c)
		if err != nil {
			return nil, nil, err
		}
		k, ok := n.(IntConst)
		if !ok || k.V <= 0 {
			return nil, nil, fmt.Errorf("alloca of a dynamic count")
		}
		a.Count = k.V
	}
	return a, nil, nil
}

// pointer reads ", ptr v".
func pointer(c *cursor) (Value, error) {
	if err := c.want(","); err != nil {
		return nil, err
	}
	if err := c.want("ptr"); err != nil {
		return nil, err
	}
	return parseValue(c, nil)
}

func parseLoad(_ *funcParser, c *cursor, _, dst string) (Instr, Term, error) {
	t, err := parseType(c)
	if err != nil {
		return nil, nil, err
	}
	addr, err := pointer(c)
	return &LoadInstr{Dst: dst, Type: t, Addr: addr}, nil, err
}

func parseStore(_ *funcParser, c *cursor, _, _ string) (Instr, Term, error) {
	t, v, err := typed(c)
	if err != nil {
		return nil, nil, err
	}
	addr, err := pointer(c)
	return &StoreInstr{Type: t, Val: v, Addr: addr}, nil, err
}

func parseGEP(_ *funcParser, c *cursor, _, dst string) (Instr, Term, error) {
	base, err := parseType(c)
	if err != nil {
		return nil, nil, err
	}
	addr, err := pointer(c)
	if err != nil {
		return nil, nil, err
	}
	g := &GEPInstr{Dst: dst, Base: base, Addr: addr}
	for c.accept(",") {
		c.acceptAny(set("inrange"))
		t, v, err := typed(c)
		if err != nil {
			return nil, nil, err
		}
		g.Indices = append(g.Indices, TypedValue{t, v})
	}
	return g, nil, nil
}

func parseCall(_ *funcParser, c *cursor, _, dst string) (Instr, Term, error) {
	if err := skipAttrs(c); err != nil {
		return nil, nil, err
	}
	if _, err := parseType(c); err != nil {
		return nil, nil, err
	}
	if err := skipAttrs(c); err != nil {
		return nil, nil, err
	}
	call := &CallInstr{Dst: dst}
	switch t := c.next(); t.kind {
	case tkGlobal:
		call.Callee = t.text
	case tkLocal:
		call.Target = Local{t.text}
	default:
		return nil, nil, fmt.Errorf("callee %q", t)
	}
	if err := c.want("("); err != nil {
		return nil, nil, err
	}
	for !c.accept(")") {
		if len(call.Args) > 0 {
			if err := c.want(","); err != nil {
				return nil, nil, err
			}
		}
		t, v, err := typed(c)
		if err != nil {
			return nil, nil, err
		}
		call.Args = append(call.Args, TypedValue{t, v})
	}
	return call, nil, nil
}

func parseVaArg(_ *funcParser, c *cursor, _, dst string) (Instr, Term, error) {
	_, list, err := typed(c)
	if err != nil {
		return nil, nil, err
	}
	if err := c.want(","); err != nil {
		return nil, nil, err
	}
	t, err := parseType(c)
	return &VaArgInstr{Dst: dst, Type: t, List: list}, nil, err
}

func parseBr(_ *funcParser, c *cursor, _, _ string) (Instr, Term, error) {
	if c.is("label") {
		to, err := target(c)
		return nil, &Jump{to}, err
	}
	_, cond, err := typed(c)
	if err != nil {
		return nil, nil, err
	}
	b := &Branch{Cond: cond}
	for _, dst := range []*string{&b.Then, &b.Els} {
		if err := c.want(","); err != nil {
			return nil, nil, err
		}
		if *dst, err = target(c); err != nil {
			return nil, nil, err
		}
	}
	return nil, b, nil
}

func parseSwitch(_ *funcParser, c *cursor, _, _ string) (Instr, Term, error) {
	_, x, err := typed(c)
	if err != nil {
		return nil, nil, err
	}
	if err := c.want(","); err != nil {
		return nil, nil, err
	}
	sw := &Switch{X: x}
	if sw.Default, err = target(c); err != nil {
		return nil, nil, err
	}
	if err := c.want("["); err != nil {
		return nil, nil, err
	}
	for !c.accept("]") {
		_, v, err := typed(c)
		if err != nil {
			return nil, nil, err
		}
		k, ok := v.(IntConst)
		if !ok {
			return nil, nil, fmt.Errorf("switch case %v", v)
		}
		if err := c.want(","); err != nil {
			return nil, nil, err
		}
		to, err := target(c)
		if err != nil {
			return nil, nil, err
		}
		sw.Arms = append(sw.Arms, SwitchArm{k.V, to})
	}
	return nil, sw, nil
}

func parseRet(_ *funcParser, c *cursor, _, _ string) (Instr, Term, error) {
	if c.accept("void") {
		return nil, &Return{}, nil
	}
	_, v, err := typed(c)
	return nil, &Return{Val: v}, err
}
