// This file implements json_each, json_tree, jsonb_each and jsonb_tree
// table-valued functions, walking the JSONB structure.
//
// All four declare the SAME ten columns -- key, value, type, atom, id, parent,
// fullkey, path, plus the HIDDEN inputs json and root (json.c:5141) -- and
// differ only in two bits jsonEachConnect reads off the module NAME
// (json.c:5150-5151): a "b" makes an object or array VALUE come back as JSONB
// instead of text, and a "t" makes the walk recursive.
//
// Every observable rule the earlier text-only implementation of this file had
// to discover by probing -- "id" as a JSONB byte offset, fullkey built by
// concatenation onto the root path text as given, the quoting decision made on
// the RAW label bytes, json_tree's root row splitting its path only when the
// selected node is its parent's first child -- is now simply what
// jsonEachFilter, jsonEachNext, jsonAppendPathName and jsonEachPathLength do.
//
// EVERY column is declared with an EMPTY type, so none of them has an affinity:
// "key=0" is true and "key='0'" is false for an array index.
//
// # The two hidden columns, and the WHERE this engine re-applies
//
// jsonEachColumn reports "json" as the input's TEXT rendering, or the JSONB
// blob itself, and "root" as the root path text (json.c:5426-5437) -- so
// "SELECT json FROM json_each(5)" is the TEXT '5'. That is only consistent
// because jsonEachBestIndex sets omit=1 on the equalities it consumes
// (json.c:5507, :5512): nothing re-tests the rendering against the number.
// This engine re-applies the statement's WHERE over a module's rows, so the
// omission has to be real here too. A call argument is never re-applied
// (buildVtabConstraints marks it Omitted), and a literal "WHERE json = ..." is
// dropped from the residual WHERE at compile time (OmittedConjunct,
// vtab_omit.go). Any other consumed equality stays standing, and Filter
// declines the rare input whose rendering is not the input itself, rather
// than let the re-applied conjunct drop the row.
package engine

import (
	"bytes"
	"fmt"
	"strconv"
)

func init() {
	RegisterVtabModule("json_each", jsonEachModule{name: "json_each"})
	RegisterVtabModule("json_tree", jsonEachModule{name: "json_tree"})
	RegisterVtabModule("jsonb_each", jsonEachModule{name: "jsonb_each"})
	RegisterVtabModule("jsonb_tree", jsonEachModule{name: "jsonb_tree"})
}

// Column indices of the declared schema, json.c:5123-5135.
const (
	jsonEachColKey = iota
	jsonEachColValue
	jsonEachColType
	jsonEachColAtom
	jsonEachColID
	jsonEachColParent
	jsonEachColFullkey
	jsonEachColPath
	jsonEachColJSON // hidden input
	jsonEachColRoot // hidden input
	jsonEachNumCols
)

// idxNum bits: which inputs Filter's argv carries, in argv order json, root,
// and which of those two equalities the engine still re-applies (see this
// file's doc comment).
const (
	jsonEachHasJSON = 1 << 0
	jsonEachHasRoot = 1 << 1

	jsonEachJSONReapplied = 1 << 2
	jsonEachRootReapplied = 1 << 3
)

// jsonEachCols is the declared schema's column names, for OmittedConjunct.
var jsonEachCols = []columnInfo{
	{Name: "key"}, {Name: "value"}, {Name: "type"}, {Name: "atom"}, {Name: "id"},
	{Name: "parent"}, {Name: "fullkey"}, {Name: "path"}, {Name: "json"}, {Name: "root"},
}

// OmittedConjunct implements omittingVtabModule: BestIndex consumes the LAST
// usable equality on json ("aIdx[iCol] = i" overwrites, json.c:5482), so this
// names the last such conjunct, and only when its right-hand side is a literal
// (vtabOmitLiteralRHS), which is always usable and so always the one consumed.
func (jsonEachModule) OmittedConjunct(conj []Expr, scopeName string) int {
	last := -1
	var lastRHS Expr
	for i, e := range conj {
		if ci, op, rhs, ok := matchVtabConstraint(e, jsonEachCols, scopeName); ok && ci == jsonEachColJSON && op == VtabEQ {
			last, lastRHS = i, rhs
		}
	}
	if last < 0 || !vtabOmitLiteralRHS(lastRHS) {
		return -1
	}
	return last
}

// jsonEachModule is the VtabModule for all four names.
type jsonEachModule struct{ name string }

// eMode and recursive are jsonEachConnect's reading of the module name,
// json.c:5150-5151.
func jsonEachModeOf(name string) (eMode int, recursive bool) {
	eMode = 1
	if len(name) > 4 && name[4] == 'b' {
		eMode = 2
	}
	return eMode, len(name) > 4+eMode && name[4+eMode] == 't'
}

func (m jsonEachModule) Connect(args []string) ([]VtabColumn, VirtualTable, error) {
	if len(args) != 0 {
		// Eponymous only: "CREATE VIRTUAL TABLE ... USING json_each(...)" is not
		// a thing in C SQLite either.
		return nil, nil, fmt.Errorf("engine: %s takes no module arguments", m.name)
	}
	return []VtabColumn{
		{Name: "key"},
		{Name: "value"},
		{Name: "type"},
		{Name: "atom"},
		{Name: "id"},
		{Name: "parent"},
		{Name: "fullkey"},
		{Name: "path"},
		{Name: "json", Hidden: true},
		{Name: "root", Hidden: true},
	}, jsonEachTable{name: m.name}, nil
}

type jsonEachTable struct{ name string }

// BestIndex is jsonEachBestIndex, json.c:5455: per input, the last usable
// equality, and an unusable constraint on an input it did not get rejects the
// plan. An Omitted constraint is kept over any later one: it is either the
// call's argument, which C codes after every WHERE term (where.c:6975), or the
// literal conjunct OmittedConjunct named as the last.
func (t jsonEachTable) BestIndex(info *VtabIndexInfo) error {
	pick := [2]int{-1, -1}
	unusable := 0
	for i, c := range info.Constraints {
		if c.Column < jsonEachColJSON { // json.c:5474
			continue
		}
		k := c.Column - jsonEachColJSON
		switch {
		case !c.Usable:
			unusable |= 1 << k
		case c.Op == VtabEQ && (pick[k] < 0 || !info.Constraints[pick[k]].Omitted):
			pick[k] = i
		}
	}
	// C SQLite answers the rejection (json.c:5493) by costing another LOOP
	// ORDER, which makes the term usable; this engine materializes a virtual
	// table once per outer row it was arranged for (vtab_correlated.go), so a
	// term still unusable here is final and surfaces as a decline. What it
	// replaces was a wrong answer: without the constraint offered at all,
	// "FROM t, json_each e WHERE e.json = t.j" returned ZERO ROWS.
	for k, col := range []int{jsonEachColJSON, jsonEachColRoot} {
		if unusable&(1<<k) != 0 && pick[k] < 0 {
			return fmt.Errorf("%w: %s(): its %s input depends on another row source, which this engine materializes a virtual table before",
				errVDBEUnsupported, t.name, jsonEachInputName(col))
		}
	}
	info.IdxNum = 0
	if pick[0] < 0 {
		return nil // json.c:5498: no JSON input, plan 0
	}
	for k, bits := range [][2]int{{jsonEachHasJSON, jsonEachJSONReapplied}, {jsonEachHasRoot, jsonEachRootReapplied}} {
		if pick[k] < 0 {
			break
		}
		info.Usage[pick[k]].ArgvIndex = k + 1
		info.Usage[pick[k]].Omit = true
		info.IdxNum |= bits[0]
		if !info.Constraints[pick[k]].Omitted {
			info.IdxNum |= bits[1]
		}
	}
	return nil
}

// jsonEachIsCText reports whether v is what sqlite3_result_text(-1) makes of
// its own text rendering: a TEXT without an embedded NUL.
func jsonEachIsCText(v Value) bool {
	return v.Typ == Text && bytes.IndexByte(v.S, 0) < 0
}

// jsonEachInputName spells the hidden input column col for an error message.
func jsonEachInputName(col int) string {
	if col == jsonEachColRoot {
		return "root"
	}
	return "json"
}

func (t jsonEachTable) Open() (VtabCursor, error) {
	eMode, recursive := jsonEachModeOf(t.name)
	return &jsonEachCursor{eMode: eMode, bRecursive: recursive}, nil
}

// vtabEncodingAware is a cursor that reads a TEXT or BLOB argument the way
// sqlite3_value_text does and so needs the database's text encoding: a BLOB
// read as text is decoded from it (materializeVtab, vtab.go, supplies it
// before Filter).
type vtabEncodingAware interface {
	setTextEncoding(TextEncoding)
}

// jsonParent is JsonParent, json.c:5077.
type jsonParent struct {
	iHead  uint32 // start of object or array
	iValue uint32 // start of the value
	iEnd   uint32 // first byte past the end
	nPath  uint32 // length of path
	iKey   int64  // key for JSONB_ARRAY
}

// jsonEachCursor is JsonEachCursor, json.c:5086.
type jsonEachCursor struct {
	iRowid     uint32
	i          uint32
	iEnd       uint32
	nRoot      uint32
	eType      byte
	bRecursive bool
	eMode      int
	aParent    []jsonParent // len is nParent
	path       jsonString
	sParse     jsonParse
	// enc is the database encoding a non-JSONB BLOB document or root is read
	// in (vtabEncodingAware).
	enc TextEncoding
}

func (p *jsonEachCursor) setTextEncoding(enc TextEncoding) { p.enc = enc }

// reset is jsonEachCursorReset, json.c:5181.
func (p *jsonEachCursor) reset() {
	p.sParse = jsonParse{}
	p.path = jsonString{}
	p.iRowid = 0
	p.i = 0
	p.aParent = nil
	p.iEnd = 0
	p.eType = 0
}

// Filter is jsonEachFilter, json.c:5521.
func (p *jsonEachCursor) Filter(idxNum int, _ string, argv []Value) error {
	p.reset()
	if idxNum&jsonEachHasJSON == 0 {
		return nil
	}
	ctx := &jsonCtx{enc: p.enc}
	if !jsonArgIsJsonb(argv[0], &p.sParse) {
		z := ctx.valueText(argv[0])
		if z == nil {
			p.i, p.iEnd = 0, 0
			return nil
		}
		p.sParse.zJson = z
		p.sParse.isText = true
		if p.sParse.convertTextToBlob(nil) {
			p.reset()
			return fmt.Errorf("engine: malformed JSON")
		}
	}
	var i uint32
	if idxNum&jsonEachHasRoot != 0 {
		zRoot := ctx.valueText(argv[1])
		if zRoot == nil {
			p.i, p.iEnd = 0, 0
			return nil
		}
		zRoot = jsonCString(zRoot)
		if byteAt(zRoot, 0) != '$' {
			p.reset()
			return fmt.Errorf("engine: %s", jsonBadPathError(zRoot, 0))
		}
		p.nRoot = uint32(len(zRoot))
		if byteAt(zRoot, 1) == 0 {
			i = 0
			p.i = 0
			p.eType = 0
		} else {
			i = p.sParse.lookupStep(0, zRoot, 1, 0)
			if jsonLookupIsError(i) {
				if i == jsonLookupNotFound {
					p.i = 0
					p.eType = 0
					p.iEnd = 0
					return nil
				}
				p.reset()
				return fmt.Errorf("engine: %s", jsonBadPathError(zRoot, 0))
			}
			if p.sParse.iLabel != 0 {
				p.i = p.sParse.iLabel
				p.eType = jsonbObject
			} else {
				p.i = i
				p.eType = jsonbArray
			}
		}
		p.path.appendRaw(zRoot)
	} else {
		i = 0
		p.i = 0
		p.eType = 0
		p.nRoot = 1
		p.path.appendChar('$')
	}
	p.aParent = nil
	n, sz := p.sParse.payloadSize(i)
	p.iEnd = i + n + sz
	if p.sParse.at(i)&0x0f >= jsonbArray && !p.bRecursive {
		p.i = i + n
		p.eType = p.sParse.at(i) & 0x0f
		p.aParent = []jsonParent{{iKey: 0, iEnd: p.iEnd, iHead: p.i, iValue: i}}
	}
	if p.i < p.iEnd && (idxNum&jsonEachJSONReapplied != 0 && p.sParse.isText && !jsonEachIsCText(argv[0]) ||
		idxNum&jsonEachRootReapplied != 0 && !jsonEachIsCText(argv[1])) {
		p.reset()
		return fmt.Errorf("%w: a re-applied equality on a JSON table-valued function's hidden input would not hold for the text it reports",
			errVDBEUnsupported)
	}
	return nil
}

// Eof is jsonEachEof, json.c:5205.
func (p *jsonEachCursor) Eof() bool { return p.i >= p.iEnd }

// skipLabel is jsonSkipLabel, json.c:5215.
func (p *jsonEachCursor) skipLabel() uint32 {
	if p.eType == jsonbObject {
		n, sz := p.sParse.payloadSize(p.i)
		return p.i + n + sz
	}
	return p.i
}

// appendPathName is jsonAppendPathName, json.c:5228.
func (p *jsonEachCursor) appendPathName() {
	if len(p.aParent) == 0 {
		return
	}
	if p.eType == jsonbArray {
		p.path.appendChar('[')
		p.path.buf = strconv.AppendInt(p.path.buf, p.aParent[len(p.aParent)-1].iKey, 10)
		p.path.appendChar(']')
		return
	}
	n, sz := p.sParse.payloadSize(p.i)
	z := p.sParse.span(p.i+n, p.i+n+sz)
	needQuote := false
	if sz == 0 || !sqlIsAlpha(byteAt(z, 0)) {
		needQuote = true
	} else {
		for k := uint32(0); k < sz; k++ {
			if !sqlIsAlnum(byteAt(z, int(k))) {
				needQuote = true
				break
			}
		}
	}
	// "%.*s" stops at a NUL inside the label.
	label := jsonCString(z)
	if needQuote {
		p.path.appendRawStr(".\"")
		p.path.appendRaw(label)
		p.path.appendChar('"')
	} else {
		p.path.appendChar('.')
		p.path.appendRaw(label)
	}
}

// Next is jsonEachNext, json.c:5259.
func (p *jsonEachCursor) Next() error {
	if p.bRecursive {
		levelChange := false
		i := p.skipLabel()
		x := p.sParse.at(i) & 0x0f
		n, sz := p.sParse.payloadSize(i)
		if x == jsonbObject || x == jsonbArray {
			levelChange = true
			parent := jsonParent{
				iHead:  p.i,
				iValue: i,
				iEnd:   i + n + sz,
				iKey:   -1,
				nPath:  uint32(len(p.path.buf)),
			}
			if p.eType != 0 && len(p.aParent) > 0 {
				p.appendPathName()
			}
			p.aParent = append(p.aParent, parent)
			p.i = i + n
		} else {
			p.i = i + n + sz
		}
		for len(p.aParent) > 0 && p.i >= p.aParent[len(p.aParent)-1].iEnd {
			last := p.aParent[len(p.aParent)-1]
			p.aParent = p.aParent[:len(p.aParent)-1]
			if int(last.nPath) <= len(p.path.buf) {
				p.path.buf = p.path.buf[:last.nPath]
			}
			levelChange = true
		}
		if levelChange {
			if len(p.aParent) > 0 {
				p.eType = p.sParse.at(p.aParent[len(p.aParent)-1].iValue) & 0x0f
			} else {
				p.eType = 0
			}
		}
	} else {
		i := p.skipLabel()
		n, sz := p.sParse.payloadSize(i)
		p.i = i + n + sz
	}
	if p.eType == jsonbArray && len(p.aParent) > 0 {
		p.aParent[len(p.aParent)-1].iKey++
	}
	p.iRowid++
	return nil
}

// pathLength is jsonEachPathLength, json.c:5325: the length of json_tree's
// root-row path, which is the root path cut at its last accessor only when the
// lookup of that prefix lands immediately before the current element.
func (p *jsonEachCursor) pathLength() uint32 {
	n := uint32(len(p.path.buf))
	z := p.path.buf
	if p.iRowid == 0 && p.bRecursive && n >= 2 {
		for n > 1 {
			n--
			if z[n] == '[' || z[n] == '.' {
				x := p.sParse.lookupStep(0, z[:n], 1, 0)
				if jsonLookupIsError(x) {
					continue
				}
				if hdr, _ := p.sParse.payloadSize(x); x+hdr == p.i {
					break
				}
			}
		}
	}
	return n
}

// Column is jsonEachColumn, json.c:5347.
func (p *jsonEachCursor) Column(iColumn int) (Value, error) {
	ctx := &jsonCtx{}
	switch iColumn {
	case jsonEachColKey:
		if len(p.aParent) == 0 {
			if p.nRoot == 1 {
				break
			}
			j := p.pathLength()
			n := int64(p.nRoot) - int64(j)
			switch {
			case n <= 0:
			case byteAt(p.path.buf, int(j)) == '[':
				x, _ := sqliteAtoi64(p.path.buf[min(int(j)+1, len(p.path.buf)):], int(n-1))
				ctx.resultInt(x)
			case byteAt(p.path.buf, int(j)+1) == '"':
				ctx.resultText(append([]byte(nil), jsonSubBytes(p.path.buf, int(j)+2, int(n)-3)...))
			default:
				ctx.resultText(append([]byte(nil), jsonSubBytes(p.path.buf, int(j)+1, int(n)-1)...))
			}
			break
		}
		if p.eType == jsonbObject {
			jsonReturnFromBlob(&p.sParse, p.i, ctx, 1)
		} else {
			ctx.resultInt(p.aParent[len(p.aParent)-1].iKey)
		}
	case jsonEachColValue:
		i := p.skipLabel()
		jsonReturnFromBlob(&p.sParse, i, ctx, p.eMode)
		if p.sParse.at(i)&0x0f >= jsonbArray {
			ctx.resultSubtype()
		}
	case jsonEachColType:
		i := p.skipLabel()
		ctx.resultStatic(jsonbTypeName[p.sParse.at(i)&0x0f])
	case jsonEachColAtom:
		i := p.skipLabel()
		if p.sParse.at(i)&0x0f < jsonbArray {
			jsonReturnFromBlob(&p.sParse, i, ctx, 1)
		}
	case jsonEachColID:
		ctx.resultInt(int64(p.i))
	case jsonEachColParent:
		if len(p.aParent) > 0 && p.bRecursive {
			ctx.resultInt(int64(p.aParent[len(p.aParent)-1].iHead))
		}
	case jsonEachColFullkey:
		nBase := len(p.path.buf)
		if len(p.aParent) > 0 {
			p.appendPathName()
		}
		ctx.resultText(append([]byte(nil), p.path.buf...))
		p.path.buf = p.path.buf[:nBase]
	case jsonEachColPath:
		n := p.pathLength()
		ctx.resultText(append([]byte(nil), p.path.buf[:n]...))
	case jsonEachColJSON:
		if p.sParse.isText {
			ctx.resultText(append([]byte(nil), jsonCString(p.sParse.zJson)...))
		} else {
			ctx.resultBlob(append([]byte(nil), p.sParse.aBlob...))
		}
	case jsonEachColRoot:
		ctx.resultText(append([]byte(nil), jsonSubBytes(p.path.buf, 0, int(p.nRoot))...))
	default:
		return Value{}, fmt.Errorf("engine: json_each: no column %d", iColumn)
	}
	return ctx.finish()
}

// jsonSubBytes is z[from:from+n], clamped, for the root-key slices C takes
// by pointer and length.
func jsonSubBytes(z []byte, from, n int) []byte {
	if n <= 0 || from >= len(z) {
		return nil
	}
	return z[from:min(from+n, len(z))]
}

// Rowid is jsonEachRowid, json.c:5444: the 0-based position of the row.
func (p *jsonEachCursor) Rowid() (int64, error) { return int64(p.iRowid), nil }
func (p *jsonEachCursor) Close() error          { return nil }
