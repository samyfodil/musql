// The json1 SQL functions. Each body is value-level code called by OpFunction
// or an aggregate step, exactly as C bodies are called in the original.
package engine

import (
	"fmt"
	"math"
	"strconv"
)

// jsonCtx is the part of sqlite3_context the JSON bodies use: the function's
// user-data flags, the database encoding (for sqlite3_value_text of a BLOB),
// and the result. An error is sticky, as pCtx->isError is.
type jsonCtx struct {
	flags  int
	enc    TextEncoding
	res    Value
	isErr  bool
	errMsg string
}

func (c *jsonCtx) resultError(msg string) {
	if !c.isErr {
		c.isErr = true
		c.errMsg = msg
	}
}

func (c *jsonCtx) resultText(b []byte)   { c.res = Value{Typ: Text, S: b} }
func (c *jsonCtx) resultBlob(b []byte)   { c.res = Value{Typ: Blob, S: b} }
func (c *jsonCtx) resultInt(i int64)     { c.res = Value{Typ: Int, I: i} }
func (c *jsonCtx) resultSubtype()        { c.res.Subtype = jsonSubtype }
func (c *jsonCtx) resultStatic(s string) { c.res = Value{Typ: Text, S: []byte(s)} }
func (c *jsonCtx) resultDouble(f float64) {
	// sqlite3VdbeMemSetDouble stores a NaN as NULL.
	if math.IsNaN(f) {
		c.res = Value{}
		return
	}
	c.res = Value{Typ: Float, F: f}
}

func (c *jsonCtx) finish() (Value, error) {
	if c.isErr {
		return Value{}, fmt.Errorf("engine: %s", c.errMsg)
	}
	return c.res, nil
}

// valueText is sqlite3_value_text/bytes: nil for NULL, the rendering of a
// number, a TEXT's bytes, and a BLOB's bytes read as text in the database
// encoding.
func (c *jsonCtx) valueText(v Value) []byte {
	switch v.Typ {
	case Null:
		return nil
	case Int:
		return strconv.AppendInt(nil, v.I, 10)
	case Float:
		return []byte(formatFloatText(v.F))
	case Blob:
		b := decodeTextBytes(c.enc, v.S)
		if b == nil {
			b = []byte{}
		}
		return b
	default:
		if v.S == nil {
			return []byte{}
		}
		return v.S
	}
}

// jsonCString is a text value walked as a C string: everything before its
// first NUL.
func jsonCString(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// jsonFuncFlags maps each scalar JSON function to its JFUNCTION iArg/bJsonB
// user data, json.c:5662-5693.
var jsonFuncFlags = map[string]int{
	"json":                0,
	"jsonb":               jsonFlagBlob,
	"json_array":          0,
	"jsonb_array":         jsonFlagBlob,
	"json_array_insert":   jsonFlagAIns,
	"jsonb_array_insert":  jsonFlagAIns | jsonFlagBlob,
	"json_array_length":   0,
	"json_error_position": 0,
	"json_extract":        0,
	"jsonb_extract":       jsonFlagBlob,
	"->":                  jsonFlagJSON,
	"->>":                 jsonFlagSQL,
	"json_insert":         0,
	"jsonb_insert":        jsonFlagBlob,
	"json_object":         0,
	"jsonb_object":        jsonFlagBlob,
	"json_patch":          0,
	"jsonb_patch":         jsonFlagBlob,
	"json_pretty":         0,
	"json_quote":          0,
	"json_remove":         0,
	"jsonb_remove":        jsonFlagBlob,
	"json_replace":        0,
	"jsonb_replace":       jsonFlagBlob,
	"json_set":            jsonFlagIsSet,
	"jsonb_set":           jsonFlagIsSet | jsonFlagBlob,
	"json_type":           0,
	"json_valid":          0,
}

// jsonFuncArity is each name's accepted argument counts, from the nArg of its
// JFUNCTION rows (json.c:5662-5693): -1 is any count, and a name registered
// twice (json_array_length, json_pretty, json_type, json_valid) takes either.
func jsonFuncArity(name string) (lo, hi int, ok bool) {
	switch name {
	case "json", "jsonb", "json_error_position", "json_quote":
		return 1, 1, true
	case "json_array_length", "json_pretty", "json_type", "json_valid":
		return 1, 2, true
	case "json_patch", "jsonb_patch", "->", "->>":
		return 2, 2, true
	}
	if _, ok := jsonFuncFlags[name]; ok {
		return 0, -1, true
	}
	return 0, 0, false
}

// callJSONFunc dispatches one scalar JSON function call.
func callJSONFunc(name string, args []Value, enc TextEncoding) (Value, error) {
	ctx := &jsonCtx{flags: jsonFuncFlags[name], enc: enc}
	switch name {
	case "json", "jsonb", "json_remove", "jsonb_remove":
		jsonRemoveFunc(ctx, args)
	case "json_array", "jsonb_array":
		jsonArrayFunc(ctx, args)
	case "json_array_insert", "jsonb_array_insert", "json_insert", "jsonb_insert", "json_set", "jsonb_set":
		jsonSetFunc(ctx, args)
	case "json_replace", "jsonb_replace":
		jsonReplaceFunc(ctx, args)
	case "json_array_length":
		jsonArrayLengthFunc(ctx, args)
	case "json_error_position":
		jsonErrorFunc(ctx, args)
	case "json_extract", "jsonb_extract", "->", "->>":
		jsonExtractFunc(ctx, args)
	case "json_object", "jsonb_object":
		jsonObjectFunc(ctx, args)
	case "json_patch", "jsonb_patch":
		jsonPatchFunc(ctx, args)
	case "json_pretty":
		jsonPrettyFunc(ctx, args)
	case "json_quote":
		jsonQuoteFunc(ctx, args)
	case "json_type":
		jsonTypeFunc(ctx, args)
	case "json_valid":
		jsonValidFunc(ctx, args)
	default:
		return Value{}, fmt.Errorf("engine: unsupported function %s()", name)
	}
	return ctx.finish()
}

// jsonParseFuncArg is json.c:3659 (without the cache): the JSONB for a
// function argument, or nil when the argument is NULL or an error was
// reported.
const (
	jsonEditable  = 0x01
	jsonKeepError = 0x02
)

func jsonParseFuncArg(ctx *jsonCtx, arg Value, flgs uint32) *jsonParse {
	if arg.Typ == Null {
		return nil
	}
	p := &jsonParse{}
	if arg.Typ == Blob {
		if jsonArgIsJsonb(arg, p) {
			if flgs&jsonEditable != 0 && !p.blobMakeEditable(0) {
				ctx.resultError("out of memory")
				return nil
			}
			return p
		}
		// If the blob is not valid JSONB, fall through into trying to cast
		// the blob into text which is then interpreted as JSON
		// (tag-20240123-a, json.c:3705).
	}
	p.zJson = ctx.valueText(arg)
	p.isText = true
	if len(p.zJson) == 0 {
		if flgs&jsonKeepError != 0 {
			p.nErr = 1
			return p
		}
		ctx.resultError("malformed JSON")
		return nil
	}
	var errCtx *jsonCtx
	if flgs&jsonKeepError == 0 {
		errCtx = ctx
	}
	if p.convertTextToBlob(errCtx) {
		if flgs&jsonKeepError != 0 {
			p.nErr = 1
			return p
		}
		return nil
	}
	if flgs&jsonEditable != 0 {
		// rebuild_from_cache (json.c:3688-3696): a text argument's editable
		// parse is always a copy whose allocation is exactly its size.
		q := &jsonParse{
			aBlob:      append([]byte(nil), p.aBlob[:p.nBlob]...),
			nBlob:      p.nBlob,
			nBlobAlloc: p.nBlob,
			hasNonstd:  p.hasNonstd,
		}
		if q.nBlobAlloc == 0 {
			q.aBlob = []byte{}
		}
		return q
	}
	return p
}

// jsonReturnParse is json.c:3776.
func jsonReturnParse(ctx *jsonCtx, p *jsonParse) {
	if p.oom {
		ctx.resultError("malformed JSON")
		return
	}
	if ctx.flags&jsonFlagBlob != 0 {
		ctx.resultBlob(append([]byte(nil), p.aBlob[:p.nBlob]...))
		return
	}
	s := jsonString{ctx: ctx}
	p.delta = 0
	p.translateBlobToText(0, &s)
	s.returnString()
	ctx.resultSubtype()
}

// jsonFunctionArgToBlob is json.c:3412: the JSONB encoding of a VALUE
// argument (json_set's new value, say). true means an error was reported.
var jsonNullBlob = []byte{jsonbNull}

func jsonFunctionArgToBlob(ctx *jsonCtx, arg Value, p *jsonParse) bool {
	*p = jsonParse{}
	switch arg.Typ {
	case Blob:
		if !jsonArgIsJsonb(arg, p) {
			ctx.resultError("JSON cannot hold BLOB values")
			return true
		}
	case Text:
		z := arg.S
		if arg.Subtype == jsonSubtype {
			p.zJson = z
			if p.convertTextToBlob(ctx) {
				ctx.resultError("malformed JSON")
				*p = jsonParse{}
				return true
			}
		} else {
			p.blobAppendNode(jsonbTextRaw, uint64(len(z)), z, true)
		}
	case Float:
		z := []byte(formatFloatText(arg.F))
		switch {
		case byteAt(z, 0) == 'I':
			p.blobAppendNode(jsonbFloat, 5, []byte("9e999"), true)
		case byteAt(z, 0) == '-' && byteAt(z, 1) == 'I':
			p.blobAppendNode(jsonbFloat, 6, []byte("-9e999"), true)
		default:
			p.blobAppendNode(jsonbFloat, uint64(len(z)), z, true)
		}
	case Int:
		z := strconv.AppendInt(nil, arg.I, 10)
		p.blobAppendNode(jsonbInt, uint64(len(z)), z, true)
	default:
		p.aBlob = jsonNullBlob
		p.nBlob = 1
		return false
	}
	if p.oom {
		ctx.resultError("out of memory")
		return true
	}
	return false
}

// jsonBadPathError is json.c:3501.
func jsonBadPathError(zPath []byte, rc uint32) string {
	switch rc {
	case jsonLookupNotArray:
		return "not an array element: " + sqlQuoteQ(zPath)
	case jsonLookupError:
		return "malformed JSON"
	case jsonLookupTooDeep:
		return "JSON path too deep"
	}
	return "bad JSON path: " + sqlQuoteQ(zPath)
}

// sqlQuoteQ is printf's %Q of a C string: single-quoted, quotes doubled.
func sqlQuoteQ(z []byte) string {
	out := []byte{'\''}
	for _, c := range z {
		if c == '\'' {
			out = append(out, '\'')
		}
		out = append(out, c)
	}
	return string(append(out, '\''))
}

// jsonInsertIntoBlob is json.c:3534.
func jsonInsertIntoBlob(ctx *jsonCtx, args []Value, eEdit uint8) {
	var rc uint32
	var zPath []byte
	flgs := uint32(jsonEditable)
	if len(args) == 1 {
		flgs = 0
	}
	p := jsonParseFuncArg(ctx, args[0], flgs)
	if p == nil {
		return
	}
	for i := 1; i < len(args)-1; i += 2 {
		if args[i].Typ == Null {
			continue
		}
		zPath = jsonCString(ctx.valueText(args[i]))
		if byteAt(zPath, 0) != '$' {
			ctx.resultError(jsonBadPathError(zPath, rc))
			return
		}
		var ax jsonParse
		if jsonFunctionArgToBlob(ctx, args[i+1], &ax) {
			return
		}
		if byteAt(zPath, 1) == 0 {
			if eEdit == jeditRepl || eEdit == jeditSet {
				p.blobEdit(0, p.nBlob, ax.aBlob[:ax.nBlob], ax.nBlob)
			}
			rc = 0
		} else {
			p.eEdit = eEdit
			p.nIns = ax.nBlob
			p.aIns = ax.aBlob[:ax.nBlob]
			p.delta = 0
			p.iDepth = 0
			rc = p.lookupStep(0, zPath, 1, 0)
		}
		if rc == jsonLookupNotFound {
			continue
		}
		if jsonLookupIsError(rc) {
			ctx.resultError(jsonBadPathError(zPath, rc))
			return
		}
	}
	jsonReturnParse(ctx, p)
}

// jsonQuoteFunc is json.c:3961.
func jsonQuoteFunc(ctx *jsonCtx, args []Value) {
	jx := jsonString{ctx: ctx}
	jx.appendSQLValue(args[0])
	jx.returnString()
	ctx.resultSubtype()
}

// jsonArrayFunc is json.c:3980.
func jsonArrayFunc(ctx *jsonCtx, args []Value) {
	jx := jsonString{ctx: ctx}
	jx.appendChar('[')
	for _, a := range args {
		jx.appendSeparator()
		jx.appendSQLValue(a)
	}
	jx.appendChar(']')
	jx.returnString()
	ctx.resultSubtype()
}

// jsonArrayLengthFunc is json.c:4006.
func jsonArrayLengthFunc(ctx *jsonCtx, args []Value) {
	var cnt int64
	eErr := false
	p := jsonParseFuncArg(ctx, args[0], 0)
	if p == nil {
		return
	}
	var i uint32
	if len(args) == 2 {
		zPath := ctx.valueText(args[1])
		if zPath == nil {
			return
		}
		zPath = jsonCString(zPath)
		if byteAt(zPath, 0) == '$' {
			i = p.lookupStep(0, zPath, 1, 0)
		} else {
			i = p.lookupStep(0, []byte("@"), 0, 0)
		}
		if jsonLookupIsError(i) {
			if i != jsonLookupNotFound {
				ctx.resultError(jsonBadPathError(zPath, i))
			}
			eErr = true
			i = 0
		}
	}
	if p.at(i)&0x0f == jsonbArray {
		cnt = int64(p.arrayCount(i))
	}
	if !eErr {
		ctx.resultInt(cnt)
	}
}

// jsonAllAlphanum is json.c:4045.
func jsonAllAlphanum(z []byte) bool {
	for _, c := range z {
		if !sqlIsAlnum(c) && c != '_' {
			return false
		}
	}
	return true
}

// jsonExtractFunc is json.c:4071: json_extract, jsonb_extract, -> and ->>.
func jsonExtractFunc(ctx *jsonCtx, args []Value) {
	if len(args) < 2 {
		return
	}
	p := jsonParseFuncArg(ctx, args[0], 0)
	if p == nil {
		return
	}
	flags := ctx.flags
	jx := jsonString{ctx: ctx}
	if len(args) > 2 {
		jx.appendChar('[')
	}
	for i := 1; i < len(args); i++ {
		zPath := ctx.valueText(args[i])
		if zPath == nil {
			return
		}
		zPath = jsonCString(zPath)
		var j uint32
		switch {
		case byteAt(zPath, 0) == '$':
			j = p.lookupStep(0, zPath, 1, 0)
		case flags&jsonFlagABPath != 0:
			// The -> and ->> operators accept abbreviated PATH arguments
			// (json.c:4099-4111).
			var abs []byte
			switch {
			case args[i].Typ == Int:
				abs = append(abs, '[')
				if zPath[0] == '-' {
					abs = append(abs, '#')
				}
				abs = append(abs, zPath...)
				abs = append(abs, ']')
			case jsonAllAlphanum(zPath):
				abs = append(abs, '.')
				abs = append(abs, zPath...)
			case zPath[0] == '[' && len(zPath) >= 3 && zPath[len(zPath)-1] == ']':
				abs = append(abs, zPath...)
			default:
				abs = append(abs, '.', '"')
				abs = append(abs, zPath...)
				abs = append(abs, '"')
			}
			jx = jsonString{ctx: ctx}
			j = p.lookupStep(0, jsonCString(abs), 0, 0)
		default:
			ctx.resultError(jsonBadPathError(zPath, 0))
			return
		}
		switch {
		case j < p.nBlob:
			if len(args) == 2 {
				if flags&jsonFlagJSON != 0 {
					jx = jsonString{ctx: ctx}
					p.translateBlobToText(j, &jx)
					jx.returnString()
					ctx.resultSubtype()
				} else {
					jsonReturnFromBlob(p, j, ctx, 0)
					if flags&(jsonFlagSQL|jsonFlagBlob) == 0 && p.at(j)&0x0f >= jsonbArray {
						ctx.resultSubtype()
					}
				}
			} else {
				jx.appendSeparator()
				p.translateBlobToText(j, &jx)
			}
		case j == jsonLookupNotFound:
			if len(args) == 2 {
				return // Return NULL if not found
			}
			jx.appendSeparator()
			jx.appendRawStr("null")
		default:
			ctx.resultError(jsonBadPathError(zPath, j))
			return
		}
	}
	if len(args) > 2 {
		jx.appendChar(']')
		jx.returnString()
		if flags&jsonFlagBlob == 0 {
			ctx.resultSubtype()
		}
	}
}

// jsonPatchFunc is json.c:4389.
func jsonPatchFunc(ctx *jsonCtx, args []Value) {
	pTarget := jsonParseFuncArg(ctx, args[0], jsonEditable)
	if pTarget == nil {
		return
	}
	pPatch := jsonParseFuncArg(ctx, args[1], 0)
	if pPatch == nil {
		return
	}
	switch rc := pTarget.mergePatch(0, pPatch, 0, 0); rc {
	case jsonMergeOK:
		jsonReturnParse(ctx, pTarget)
	case jsonMergeTooDeep:
		ctx.resultError("JSON nested too deep")
	default:
		ctx.resultError("malformed JSON")
	}
}

// jsonObjectFunc is json.c:4425.
func jsonObjectFunc(ctx *jsonCtx, args []Value) {
	if len(args)&1 != 0 {
		ctx.resultError("json_object() requires an even number of arguments")
		return
	}
	jx := jsonString{ctx: ctx}
	jx.appendChar('{')
	for i := 0; i < len(args); i += 2 {
		if args[i].Typ != Text {
			ctx.resultError("json_object() labels must be TEXT")
			return
		}
		jx.appendSeparator()
		jx.appendString(args[i].S)
		jx.appendChar(':')
		jx.appendSQLValue(args[i+1])
	}
	jx.appendChar('}')
	jx.returnString()
	ctx.resultSubtype()
}

// jsonRemoveFunc is json.c:4467, which is also json() and jsonb() with their
// single argument (json.c:5662-5663).
func jsonRemoveFunc(ctx *jsonCtx, args []Value) {
	if len(args) < 1 {
		return
	}
	flgs := uint32(0)
	if len(args) > 1 {
		flgs = jsonEditable
	}
	p := jsonParseFuncArg(ctx, args[0], flgs)
	if p == nil {
		return
	}
	for i := 1; i < len(args); i++ {
		zPath := ctx.valueText(args[i])
		if zPath == nil {
			return
		}
		zPath = jsonCString(zPath)
		if byteAt(zPath, 0) != '$' {
			ctx.resultError(jsonBadPathError(zPath, 0))
			return
		}
		if byteAt(zPath, 1) == 0 {
			// json_remove(j,'$') returns NULL
			return
		}
		p.eEdit = jeditDel
		p.delta = 0
		rc := p.lookupStep(0, zPath, 1, 0)
		if jsonLookupIsError(rc) {
			if rc == jsonLookupNotFound {
				continue // No-op
			}
			ctx.resultError(jsonBadPathError(zPath, rc))
			return
		}
	}
	jsonReturnParse(ctx, p)
}

// jsonReplaceFunc is json.c:4522.
func jsonReplaceFunc(ctx *jsonCtx, args []Value) {
	if len(args) < 1 {
		return
	}
	if len(args)&1 == 0 {
		ctx.resultError("json_replace() needs an odd number of arguments")
		return
	}
	jsonInsertIntoBlob(ctx, args, jeditRepl)
}

// jsonSetFunc is json.c:4548: json_set, json_insert and json_array_insert.
func jsonSetFunc(ctx *jsonCtx, args []Value) {
	eInsType := (ctx.flags & 0xC) >> 2
	azInsType := [3]string{"insert", "set", "array_insert"}
	aEditType := [3]uint8{jeditIns, jeditSet, jeditAIns}
	if len(args) < 1 {
		return
	}
	if len(args)&1 == 0 {
		ctx.resultError("json_" + azInsType[eInsType] + "() needs an odd number of arguments")
		return
	}
	jsonInsertIntoBlob(ctx, args, aEditType[eInsType])
}

// jsonTypeFunc is json.c:4574.
func jsonTypeFunc(ctx *jsonCtx, args []Value) {
	p := jsonParseFuncArg(ctx, args[0], 0)
	if p == nil {
		return
	}
	var i uint32
	if len(args) == 2 {
		zPath := ctx.valueText(args[1])
		if zPath == nil {
			return
		}
		zPath = jsonCString(zPath)
		if byteAt(zPath, 0) != '$' {
			ctx.resultError(jsonBadPathError(zPath, 0))
			return
		}
		i = p.lookupStep(0, zPath, 1, 0)
		if jsonLookupIsError(i) {
			if i != jsonLookupNotFound {
				ctx.resultError(jsonBadPathError(zPath, i))
			}
			return
		}
	}
	ctx.resultStatic(jsonbTypeName[p.at(i)&0x0f])
}

// jsonPrettyFunc is json.c:4619.
func jsonPrettyFunc(ctx *jsonCtx, args []Value) {
	x := jsonPretty{}
	x.pParse = jsonParseFuncArg(ctx, args[0], 0)
	if x.pParse == nil {
		return
	}
	s := jsonString{ctx: ctx}
	x.pOut = &s
	var zIndent []byte
	if len(args) == 2 {
		zIndent = ctx.valueText(args[1])
	}
	if zIndent == nil {
		x.zIndent = []byte("    ")
	} else {
		x.zIndent = jsonCString(zIndent)
	}
	x.translate(0)
	s.returnString()
}

// jsonValidFunc is json.c:4700.
func jsonValidFunc(ctx *jsonCtx, args []Value) {
	flags := uint8(1)
	res := false
	if len(args) == 2 {
		f := valueToInt64Trunc(args[1])
		if f < 1 || f > 15 {
			ctx.resultError("FLAGS parameter to json_valid() must be between 1 and 15")
			return
		}
		flags = uint8(f & 0x0f)
	}
	switch args[0].Typ {
	case Null:
		return
	case Blob:
		var py jsonParse
		if jsonArgIsJsonb(args[0], &py) {
			if flags&0x04 != 0 {
				// Superficial checking only - accomplished by the
				// jsonArgIsJsonb() call above.
				res = true
			} else if flags&0x08 != 0 {
				// Strict checking.
				res = py.validityCheck(0, py.nBlob, 1) == 0
			}
			break
		}
		// Fall through into interpreting the input as text (json.c:4740).
		res = jsonValidText(ctx, args[0], flags)
	default:
		res = jsonValidText(ctx, args[0], flags)
	}
	ctx.resultInt(boolInt(res))
}

// jsonValidText is jsonValidFunc's default arm, json.c:4744-4763.
func jsonValidText(ctx *jsonCtx, arg Value, flags uint8) bool {
	if flags&0x3 == 0 {
		return false
	}
	p := jsonParseFuncArg(ctx, arg, jsonKeepError)
	if p == nil {
		return false
	}
	if p.oom || p.nErr != 0 {
		return false
	}
	return flags&0x02 != 0 || !p.hasNonstd
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// jsonErrorFunc is json_error_position, json.c:4782.
func jsonErrorFunc(ctx *jsonCtx, args []Value) {
	var iErrPos int64
	var s jsonParse
	if jsonArgIsJsonb(args[0], &s) {
		iErrPos = int64(s.validityCheck(0, s.nBlob, 1))
	} else {
		z := ctx.valueText(args[0])
		if z == nil {
			return // NULL input
		}
		s.zJson = z
		if s.convertTextToBlob(nil) {
			// Convert byte-offset s.iErr into a character offset.
			for k := uint32(0); k < s.iErr && s.z(k) != 0; k++ {
				if s.z(k)&0xc0 != 0x80 {
					iErrPos++
				}
			}
			iErrPos++
		}
	}
	ctx.resultInt(iErrPos)
}

// jsonReturnFromBlob is json.c:3225: the SQL value of the node at i. An array
// or object comes back as text JSON (eMode 1), as JSONB (eMode 2), or per the
// function's JSON_BLOB flag (eMode 0).
func jsonReturnFromBlob(p *jsonParse, i uint32, ctx *jsonCtx, eMode int) {
	n, sz := p.payloadSize(i)
	if n == 0 {
		ctx.resultError("malformed JSON")
		return
	}
	switch p.at(i) & 0x0f {
	case jsonbNull:
		if sz != 0 {
			ctx.resultError("malformed JSON")
			return
		}
		ctx.res = Value{}
	case jsonbTrue:
		if sz != 0 {
			ctx.resultError("malformed JSON")
			return
		}
		ctx.resultInt(1)
	case jsonbFalse:
		if sz != 0 {
			ctx.resultError("malformed JSON")
			return
		}
		ctx.resultInt(0)
	case jsonbInt5, jsonbInt:
		if sz == 0 {
			ctx.resultError("malformed JSON")
			return
		}
		bNeg := false
		if p.at(i+n) == '-' {
			if sz < 2 {
				ctx.resultError("malformed JSON")
				return
			}
			n++
			sz--
			bNeg = true
		}
		iRes, rc := sqliteDecOrHexToI64(p.span(i+n, i+n+sz))
		switch {
		case rc == 0:
			if iRes < 0 {
				// A hexadecimal literal with 16 significant digits and with the
				// high-order bit set is a negative integer in SQLite but should
				// be interpreted as a positive value within JSON.
				r := float64(uint64(iRes))
				if bNeg {
					r = -r
				}
				ctx.resultDouble(r)
			} else if bNeg {
				ctx.resultInt(-iRes)
			} else {
				ctx.resultInt(iRes)
			}
		case rc == 3 && bNeg:
			ctx.resultInt(math.MinInt64)
		case rc == 1:
			ctx.resultError("malformed JSON")
		default:
			if bNeg {
				n--
				sz++
			}
			jsonReturnDouble(p, i+n, sz, ctx)
		}
	case jsonbFloat5, jsonbFloat:
		if sz == 0 {
			ctx.resultError("malformed JSON")
			return
		}
		jsonReturnDouble(p, i+n, sz, ctx)
	case jsonbTextRaw, jsonbText:
		ctx.resultText(append([]byte(nil), p.span(i+n, i+n+sz)...))
	case jsonbText5, jsonbTextJ:
		// Translate JSON formatted string into raw text.
		z := p.span(i+n, i+n+sz)
		zOut := make([]byte, 0, len(z))
		for iIn := uint32(0); iIn < uint32(len(z)); iIn++ {
			c := z[iIn]
			if c != '\\' {
				zOut = append(zOut, c)
				continue
			}
			var v uint32
			szEscape := jsonUnescapeOneChar(z[iIn:], uint32(len(z))-iIn, &v)
			switch {
			case v <= 0x7f:
				zOut = append(zOut, byte(v))
			case v <= 0x7ff:
				zOut = append(zOut, byte(0xc0|(v>>6)), byte(0x80|(v&0x3f)))
			case v < 0x10000:
				zOut = append(zOut, byte(0xe0|(v>>12)), byte(0x80|((v>>6)&0x3f)), byte(0x80|(v&0x3f)))
			case v == jsonInvalidChar:
				// Silently ignore illegal unicode
			default:
				zOut = append(zOut, byte(0xf0|(v>>18)), byte(0x80|((v>>12)&0x3f)),
					byte(0x80|((v>>6)&0x3f)), byte(0x80|(v&0x3f)))
			}
			iIn += szEscape - 1
		}
		ctx.resultText(zOut)
	case jsonbArray, jsonbObject:
		if eMode == 0 {
			if ctx.flags&jsonFlagBlob != 0 {
				eMode = 2
			} else {
				eMode = 1
			}
		}
		if eMode == 2 {
			ctx.resultBlob(append([]byte(nil), p.span(i, i+sz+n)...))
		} else {
			// jsonReturnTextJsonFromBlob, json.c:3192.
			x := jsonParse{aBlob: p.span(i, i+sz+n)}
			x.nBlob = uint32(len(x.aBlob))
			s := jsonString{ctx: ctx}
			x.translateBlobToText(0, &s)
			flags := ctx.flags
			ctx.flags &^= jsonFlagBlob // jsonReturnString reads user data; text here
			s.returnString()
			ctx.flags = flags
		}
	default:
		ctx.resultError("malformed JSON")
	}
}

// jsonReturnDouble is jsonReturnFromBlob's to_double arm, json.c:3304.
func jsonReturnDouble(p *jsonParse, start, sz uint32, ctx *jsonCtx) {
	r, rc := sqliteAtoF(p.span(start, start+sz))
	if rc <= 0 {
		ctx.resultError("malformed JSON")
		return
	}
	ctx.resultDouble(r)
}

// ---- util.c numeric parsers -------------------------------------------------

// sqliteAtoF is sqlite3AtoF, util.c:871, over a NUL-terminated copy of z
// (byteAt supplies the terminator; an embedded NUL ends the text as in C). It
// returns the value and C's result code: positive only for a complete number
// with nothing but trailing whitespace.
func sqliteAtoF(z []byte) (float64, int32) {
	const largest = uint64(math.MaxUint64)
	i := 0
	neg := false
	var s uint64
	d := 0
	mState := int32(0)
	for sqlIsSpace(byteAt(z, i)) {
		i++
	}
	switch byteAt(z, i) {
	case '-':
		neg = true
		i++
	case '+':
		i++
	}
	if sqlIsDigit(byteAt(z, i)) {
		mState = 1
		s = uint64(byteAt(z, i) - '0')
		i++
		for sqlIsDigit(byteAt(z, i)) {
			s = s*10 + uint64(byteAt(z, i)-'0')
			i++
			if s >= (largest-9)/10 {
				mState = 9
				for sqlIsDigit(byteAt(z, i)) {
					i++
					d++
				}
				break
			}
		}
	}
	if byteAt(z, i) == '.' {
		i++
		if sqlIsDigit(byteAt(z, i)) {
			mState |= 1
			for {
				if s < (largest-9)/10 {
					s = s*10 + uint64(byteAt(z, i)-'0')
					d--
				} else {
					mState = 11
				}
				i++
				if !sqlIsDigit(byteAt(z, i)) {
					break
				}
			}
		} else if mState == 0 {
			return 0, 0
		}
		mState |= 2
	} else if mState == 0 {
		return 0, 0
	}
	if c := byteAt(z, i); c == 'e' || c == 'E' {
		i++
		esign := 1
		if byteAt(z, i) == '-' {
			esign = -1
			i++
		} else if byteAt(z, i) == '+' {
			i++
		}
		if sqlIsDigit(byteAt(z, i)) {
			exp := int(byteAt(z, i) - '0')
			i++
			mState |= 2
			for sqlIsDigit(byteAt(z, i)) {
				if exp < 10000 {
					exp = exp*10 + int(byteAt(z, i)-'0')
				} else {
					exp = 10000
				}
				i++
			}
			d += esign * exp
		} else {
			i-- // leave z[0] at 'e' or '+' or '-', so that the return is <=0
		}
	}
	var r float64
	if s == 0 {
		mState |= 4
	} else {
		r = sqliteFp10Convert2(s, d)
	}
	if neg {
		r = -r
	}
	if byteAt(z, i) == 0 {
		return r, mState
	}
	if sqlIsSpace(byteAt(z, i)) {
		for sqlIsSpace(byteAt(z, i)) {
			i++
		}
		if byteAt(z, i) == 0 {
			return r, mState
		}
	}
	return r, int32(uint32(0xfffffff0) | uint32(mState))
}

// sqliteDecOrHexToI64 is sqlite3DecOrHexToI64, util.c:1269.
func sqliteDecOrHexToI64(z []byte) (int64, int) {
	if byteAt(z, 0) == '0' && (byteAt(z, 1) == 'x' || byteAt(z, 1) == 'X') {
		var u uint64
		i := 2
		for byteAt(z, i) == '0' {
			i++
		}
		k := i
		for ; sqlIsXdigit(byteAt(z, k)); k++ {
			u = u*16 + uint64(jsonHexToInt(byteAt(z, k)))
		}
		out := int64(u)
		if k-i > 16 {
			return out, 2
		}
		if byteAt(z, k) != 0 {
			return out, 1
		}
		return out, 0
	}
	n := 0
	for {
		c := byteAt(z, n)
		if c == 0 || !(c == '+' || c == '-' || c == ' ' || c == '\n' || c == '\t' || sqlIsDigit(c)) {
			break
		}
		n++
	}
	if byteAt(z, n) != 0 {
		n++
	}
	return sqliteAtoi64(z, n)
}

// sqliteAtoi64 is sqlite3Atoi64, util.c:1166, for UTF-8 text of length bytes.
func sqliteAtoi64(z []byte, length int) (int64, int) {
	zEnd := length
	pos := 0
	for pos < zEnd && sqlIsSpace(byteAt(z, pos)) {
		pos++
	}
	neg := false
	if pos < zEnd {
		if byteAt(z, pos) == '-' {
			neg = true
			pos++
		} else if byteAt(z, pos) == '+' {
			pos++
		}
	}
	zStart := pos
	for pos < zEnd && byteAt(z, pos) == '0' {
		pos++
	}
	var u uint64
	i := 0
	for ; pos+i < zEnd; i++ {
		c := byteAt(z, pos+i)
		if c < '0' || c > '9' {
			break
		}
		u = u*10 + uint64(c-'0')
	}
	var num int64
	switch {
	case u > math.MaxInt64:
		if neg {
			num = math.MinInt64
		} else {
			num = math.MaxInt64
		}
	case neg:
		num = -int64(u)
	default:
		num = int64(u)
	}
	rc := 0
	if i == 0 && zStart == pos {
		rc = -1 // No digits
	} else if pos+i < zEnd {
		for jj := i; pos+jj < zEnd; jj++ {
			if !sqlIsSpace(byteAt(z, pos+jj)) {
				rc = 1 // Extra non-space text after the integer
				break
			}
		}
	}
	if i < 19 {
		return num, rc
	}
	j := 1
	if i == 19 {
		j = compare2pow63(z, pos)
	}
	if j < 0 {
		return num, rc
	}
	if neg {
		num = math.MinInt64
	} else {
		num = math.MaxInt64
	}
	if j > 0 {
		return num, 2
	}
	if neg {
		return num, rc
	}
	return num, 3
}

// compare2pow63 is util.c's compare2pow63 for UTF-8.
func compare2pow63(z []byte, pos int) int {
	const pow63 = "922337203685477580"
	c := 0
	for i := 0; c == 0 && i < 18; i++ {
		c = (int(byteAt(z, pos+i)) - int(pow63[i])) * 10
	}
	if c == 0 {
		c = int(byteAt(z, pos+18)) - '8'
	}
	return c
}

// ---- json_group_array / json_group_object ---------------------------------

// jsonGroupValueText renders one aggregate argument the way jsonArrayStep and
// jsonObjectStep append it (jsonAppendSqlValue, json.c:803), reporting the
// error C raises in the step's own context.
func jsonGroupValueText(v Value, enc TextEncoding) ([]byte, error) {
	ctx := &jsonCtx{enc: enc}
	s := jsonString{ctx: ctx}
	s.appendSQLValue(v)
	if ctx.isErr {
		return nil, fmt.Errorf("engine: %s", ctx.errMsg)
	}
	if s.eErr != 0 {
		// A malformed JSONB argument poisons the accumulator; C reports it
		// when the result is next computed (jsonArrayCompute, json.c:4857),
		// which every aggregate and window frame reaches.
		return nil, fmt.Errorf("engine: malformed JSON")
	}
	return s.buf, nil
}

// jsonGroupKeyText is jsonObjectStep's label, json.c:4963-4973: the key's
// text up to its first NUL, quoted; nil for a NULL key, which contributes no
// entry.
func jsonGroupKeyText(k Value, enc TextEncoding) []byte {
	ctx := &jsonCtx{enc: enc}
	z := ctx.valueText(k)
	if z == nil {
		return nil
	}
	s := jsonString{ctx: ctx}
	s.appendString(jsonCString(z))
	return s.buf
}

// jsonGroupResult is jsonArrayCompute/jsonObjectCompute's result, json.c:4849
// and :4982: the accumulated text, or its JSONB translation for the jsonb_
// spellings. subtype says whether the JSON subtype is set, which is not
// always: jsonArrayCompute's JSON_BLOB arm RETURNS before its closing
// sqlite3_result_subtype (json.c:4860-4867) once any row was stepped, while
// jsonObjectCompute falls through to it on every path (json.c:5038-5061).
func jsonGroupResult(text []byte, blob, subtype bool) Value {
	if blob {
		ctx := &jsonCtx{flags: jsonFlagBlob}
		s := jsonString{ctx: ctx, buf: text}
		s.returnStringAsBlob()
		v := ctx.res
		if subtype {
			v.Subtype = jsonSubtype
		}
		return v
	}
	v := Value{Typ: Text, S: text}
	if subtype {
		v.Subtype = jsonSubtype
	}
	return v
}

// stepJSONGroupArray feeds one row into a json_group_array()/jsonb_group_array()
// accumulator: jsonArrayStep, json.c:4830. Unlike every other aggregate a NULL
// row contributes (as null), so sql_agg.go's step routes here ahead of its
// generic NULL skip. gcBuf holds the elements between the brackets.
//
// DISTINCT's dedup (it.seen) is applied here for the same reason, and dedupes
// NULL contributions too -- two NULL rows contribute one null element.
func (it *aggItem) stepJSONGroupArray(ctx *evalCtx, regs []Value) error {
	v, err := it.rowValue(aggExprArg, ctx, regs)
	if err != nil {
		return err
	}
	if it.distinct {
		for _, s := range it.seen {
			if compareValues(v, s) == 0 {
				return nil
			}
		}
		it.seen = append(it.seen, v)
	}
	text, err := jsonGroupValueText(v, ctx.encoding())
	if err != nil {
		return err
	}
	if it.gcHave {
		it.gcBuf.WriteByte(',')
	}
	it.gcBuf.Write(text)
	it.gcHave = true
	return nil
}

// stepJSONGroupObject feeds one row's (key, value) pair into a
// json_group_object()/jsonb_group_object() accumulator: jsonObjectStep,
// json.c:4952. it.expr is the KEY and it.sepExpr the VALUE (see
// planAggregateCall).
//
// A NULL key contributes nothing. C does append an "@" placeholder there and
// strips it again in jsonObjectCompute (json.c:4996-5036), which exists only so
// jsonGroupInverse can count entries; the text it leaves is the text of
// skipping the row. The key is the value's text up to its first NUL
// (sqlite3Strlen30, json.c:4964), quoted even when it carries the JSON subtype.
func (it *aggItem) stepJSONGroupObject(ctx *evalCtx, regs []Value) error {
	k, err := it.rowValue(aggExprArg, ctx, regs)
	if err != nil {
		return err
	}
	// BOTH arguments are evaluated before the step function runs, so the VALUE
	// is evaluated even on a row whose KEY is NULL and which contributes
	// nothing: updateAccumulator codes the whole argument list with one
	// sqlite3ExprCodeExprList (select.c:6902-6906) and OP_AggStep is handed
	// that register range (select.c:6942). Reading it ahead of the NULL-key
	// skip is what makes that true, and it was a LIVE WRONG ANSWER: over
	// t(x,y) = (NULL, -9223372036854775808),
	//
	//	SELECT json_group_object(x, abs(y)) FROM t
	//	    3.53.3: "integer overflow"     musql: {}
	//
	// (verified directly against the linked oracle; the group_concat twin is in
	// aggItem.step, sql_agg.go).
	v, err := it.rowValue(aggExprSep, ctx, regs)
	if err != nil {
		return err
	}
	if k.Typ == Null {
		return nil
	}
	if it.distinct {
		for i := 0; i+1 < len(it.seen); i += 2 {
			if compareValues(k, it.seen[i]) == 0 && compareValues(v, it.seen[i+1]) == 0 {
				return nil
			}
		}
		it.seen = append(it.seen, k, v)
	}
	enc := ctx.encoding()
	label := jsonGroupKeyText(k, enc)
	valText, err := jsonGroupValueText(v, enc)
	if err != nil {
		return err
	}
	if it.gcHave {
		it.gcBuf.WriteByte(',')
	}
	it.gcBuf.Write(label)
	it.gcBuf.WriteByte(':')
	it.gcBuf.Write(valText)
	it.gcHave = true
	return nil
}

// finalizeJSONGroupArray/finalizeJSONGroupObject are jsonArrayCompute and
// jsonObjectCompute (json.c:4849, :4982). An empty group is "[]"/"{}" (or the
// one-byte JSONB 0x0b/0x0c), never NULL, and the result carries the JSON
// subtype in both spellings.
func (it *aggItem) finalizeJSONGroupArray() Value {
	// A stepped jsonb_group_array has no subtype; an empty one (no
	// aggregate context, json.c:4877-4879) and the text spelling do.
	return jsonGroupResult([]byte("["+it.gcBuf.String()+"]"), it.jsonb, !(it.jsonb && it.gcHave))
}

func (it *aggItem) finalizeJSONGroupObject() Value {
	return jsonGroupResult([]byte("{"+it.gcBuf.String()+"}"), it.jsonb, true)
}
