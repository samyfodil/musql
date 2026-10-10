// Vector functions, compatible with libSQL's: vector(), vector32(),
// vector64(), vector1bit(), vector8(), vector16(), vectorb16(),
// vector_extract(), vector_distance_cos() and vector_distance_l2().
//
// A vector is a BLOB in libSQL's byte layout, so a database written by either
// engine reads the same in the other: little-endian components, then, for
// every type but float32, metadata ending in a type byte:
//
//	float32   [f32 ...]                          even length, no type byte
//	float64   [f64 ...] 2
//	float1bit [bits ...] [pad]? trailing-bits 3   odd length
//	float8    [u8 ...] [pad]* alpha shift 0 trailing-bytes 4
//	float16   [f16 ...] 5
//	floatb16  [bf16 ...] 6
//
// The functions work on those bytes in place: parsing a blob slices it, and a
// distance is one typed loop over two slices, with no allocation. Their
// arithmetic is libSQL's to the bit: float32 sums accumulated in component
// order, each product rounded on its own (Go may fuse a multiply-add, which C
// on the oracle's amd64 does not), and every distance returned through a
// float32. They are also the reference the JIT's distance kernels are checked
// against.
package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"strconv"
)

// vecType is a vector's component type, numbered as libSQL's type byte.
type vecType uint8

const (
	vecF32  vecType = 1
	vecF64  vecType = 2
	vec1Bit vecType = 3
	vecF8   vecType = 4
	vecF16  vecType = 5
	vecFB16 vecType = 6
)

const (
	vecMaxDims    = 65536 // MAX_VECTOR_SZ
	vecMaxFloatSz = 1024  // MAX_FLOAT_CHAR_SZ
)

func (t vecType) String() string {
	switch t {
	case vecF32:
		return "float32"
	case vecF64:
		return "float64"
	case vec1Bit:
		return "float1bit"
	case vecF8:
		return "float8"
	case vecF16:
		return "float16"
	case vecFB16:
		return "floatb16"
	}
	return strconv.Itoa(int(t))
}

// vector is a parsed vector over its data bytes: the blob without its
// metadata, and for float8 just the quantized bytes, its parameters decoded
// alongside.
type vector struct {
	typ          vecType
	dims         int
	data         []byte
	alpha, shift float32
}

func vecErr(format string, a ...any) error {
	return fmt.Errorf("engine: "+format, a...)
}

func align4(n int) int { return (n + 3) &^ 3 }

// dataSize is the size of the data part of a vector of this type.
func (t vecType) dataSize(dims int) int {
	switch t {
	case vecF32:
		return 4 * dims
	case vecF64:
		return 8 * dims
	case vec1Bit:
		return (dims + 7) / 8
	case vecF8:
		return align4(dims) + 8
	}
	return 2 * dims
}

// parseVector reads a vector argument. Text parses as float32, or as float64
// when hint asks for it; anything but text and blobs is an error. hint is 0
// for vector_extract() and the distances, which libSQL parses leniently.
func parseVector(v Value, hint vecType) (vector, error) {
	switch v.Typ {
	case Blob:
		return parseVectorBlob(v.S, hint == 0)
	case Text:
		if hint != vecF64 {
			hint = vecF32
		}
		return parseVectorText(v.S, hint)
	}
	return vector{}, vecErr("vector: unexpected value type: got %s, expected TEXT or BLOB", typeRepr(v))
}

func typeRepr(v Value) string {
	switch v.Typ {
	case Null:
		return "NULL"
	case Int:
		return "INTEGER"
	case Float:
		return "FLOAT"
	case Blob:
		return "BLOB"
	case Text:
		return "TEXT"
	}
	return "UNKNOWN"
}

func parseVectorBlob(b []byte, lenient bool) (vector, error) {
	n := len(b)
	t, dims, size := vecF32, n/4, n
	if n%2 == 1 {
		t = vecType(b[n-1])
		n--
		switch t {
		case vecF32, vecF64:
			w := 4
			if t == vecF64 {
				w = 8
			}
			if n%w != 0 {
				opt := ""
				if t == vecF32 {
					opt = "optional "
				}
				return vector{}, vecErr("vector: %s vector blob length must be divisible by %d (excluding %s'type'-byte): length=%d", t, w, opt, n)
			}
			dims, size = n/w, n
		case vec1Bit:
			if n == 0 || n%2 != 0 {
				return vector{}, vecErr("vector: float1bit vector blob length must be divisible by 2 and not be empty (excluding 'type'-byte): length=%d", n)
			}
			dims = 8*n - int(b[n-1])
			size = (dims + 7) / 8
		case vecF8:
			if n < 2 || n%2 != 0 {
				return vector{}, vecErr("vector: float8 vector blob length must be divisible by 2 and has at least 2 bytes (excluding 'type'-byte): length=%d", n)
			}
			dims, size = n-2-8-int(b[n-1]), n-2
		case vecF16, vecFB16:
			if n%2 != 0 {
				return vector{}, vecErr("vector: %s vector blob length must be divisible by 2 (excluding 'type'-byte): length=%d", t, n)
			}
			dims, size = n/2, n
		default:
			return vector{}, vecErr("vector: unexpected binary type: %d", t)
		}
	}
	if dims > vecMaxDims {
		return vector{}, vecErr("vector: max size exceeded: %d > %d", dims, vecMaxDims)
	}
	if dims < 0 {
		// A trailing count larger than the blob. libSQL goes on to allocate
		// with a wrapped dimension count; there is no answer worth copying.
		return vector{}, vecErr("vector: malformed %s vector blob", t)
	}
	// A data part whose size disagrees with the dimensions is an error. libSQL
	// 0.2.3 reports it from vector() and its typed spellings, but its
	// vector_extract() and distances test the parse with "< 0" while the
	// failure is SQLITE_ERROR (1), and go on over uninitialized memory. There
	// is no answer in that to copy except where nothing is read: zero
	// dimensions, which those two (lenient) accept.
	if want := t.dataSize(dims); size != want && !(lenient && dims == 0) {
		return vector{}, vecErr("vector: unexpected data part size: type=%d, dims=%d, %d != %d", t, dims, size, want)
	}
	v := vector{typ: t, dims: dims, data: b[:min(size, t.dataSize(dims))]}
	if t == vecF8 {
		v.data = b[:dims] // the quantized bytes; the parameters are decoded
		p := b[align4(dims):]
		v.alpha = math.Float32frombits(binary.LittleEndian.Uint32(p))
		v.shift = math.Float32frombits(binary.LittleEndian.Uint32(p[4:]))
	}
	return v, nil
}

// parseVectorText reads '[1, 2, 3]' into a float32 or float64 vector.
// Whitespace is dropped everywhere, inside a number too, as libSQL drops it.
func parseVectorText(s []byte, t vecType) (vector, error) {
	i := 0
	skip := func() {
		for i < len(s) && sqlIsSpace(s[i]) {
			i++
		}
	}
	skip()
	if i == len(s) || s[i] != '[' {
		return vector{}, vecErr("vector: must start with '['")
	}
	i++
	w := 4
	if t == vecF64 {
		w = 8
	}
	var data []byte
	n := 0
	// One number per pass: its span runs to the next ',' or ']' (or a NUL,
	// where C's string ends), found in one scan and parsed in place. Only a
	// number with whitespace inside it -- which libSQL drops, so "1 2" is 12
	// -- is copied to strip it.
	for {
		// e is the terminator; [lo, hi) the span from the first non-space to
		// the last; inner whether a space falls between them.
		e, lo, hi, inner, gap := i, -1, -1, false, false
		for ; e < len(s); e++ {
			c := s[e]
			if c == ',' || c == ']' || c == 0 {
				break
			}
			if sqlIsSpace(c) {
				gap = lo >= 0
				continue
			}
			if lo < 0 {
				lo = e
			}
			inner = inner || gap
			gap = false
			hi = e + 1
		}
		var num []byte
		if lo >= 0 {
			num = s[lo:hi]
		}
		if inner {
			num = bytes.Map(func(r rune) rune {
				if r < 0x80 && sqlIsSpace(byte(r)) {
					return -1
				}
				return r
			}, num)
		}
		// libSQL buffers 1,025 characters and refuses the next.
		if len(num) > vecMaxFloatSz+1 {
			return vector{}, vecErr("vector: float string length exceeded %d characters: '%s'", vecMaxFloatSz, num[:vecMaxFloatSz+1])
		}
		i = e
		if e == len(s) || s[e] == 0 || (s[e] == ']' && n == 0 && len(num) == 0) {
			break // unterminated (reported below), or '[]'
		}
		x, rc := sqliteAtoF(num)
		if rc <= 0 {
			return vector{}, vecErr("vector: invalid float at position %d: '%s'", n, num)
		}
		if n >= vecMaxDims {
			return vector{}, vecErr("vector: max size exceeded %d", vecMaxDims)
		}
		if w == 4 {
			data = binary.LittleEndian.AppendUint32(data, math.Float32bits(float32(x)))
		} else {
			data = binary.LittleEndian.AppendUint64(data, math.Float64bits(x))
		}
		n++
		if s[e] == ']' {
			break
		}
		i = e + 1
	}
	skip()
	if i == len(s) || s[i] != ']' {
		return vector{}, vecErr("vector: must end with ']'")
	}
	i++
	skip()
	if i < len(s) && s[i] != 0 {
		return vector{}, vecErr("vector: non-space symbols after closing ']' are forbidden")
	}
	return vector{typ: t, dims: n, data: data}, nil
}

// ---- components --------------------------------------------------------------

func f32At(b []byte, i int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])) }
func f64At(b []byte, i int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b[8*i:])) }
func u16At(b []byte, i int) uint16  { return binary.LittleEndian.Uint16(b[2*i:]) }

// floats decodes the components as libSQL's converters read them: float32
// for every type but float64. A float64 vector goes through float64s instead.
func (v vector) floats() []float32 {
	out := make([]float32, v.dims)
	switch v.typ {
	case vecF32:
		for i := range out {
			out[i] = f32At(v.data, i)
		}
	case vecF64:
		for i := range out {
			out[i] = float32(f64At(v.data, i))
		}
	case vec1Bit:
		for i := range out {
			out[i] = float32(int(v.data[i/8]>>(i&7)&1)*2 - 1)
		}
	case vecF8:
		for i, q := range v.data[:v.dims] {
			out[i] = float32(v.alpha*float32(q)) + v.shift
		}
	case vecF16:
		for i := range out {
			out[i] = f16ToFloat(u16At(v.data, i))
		}
	case vecFB16:
		for i := range out {
			out[i] = bf16ToFloat(u16At(v.data, i))
		}
	}
	return out
}

// convert re-encodes v as type t.
func (v vector) convert(t vecType) vector {
	if v.typ == t {
		return v
	}
	out := vector{typ: t, dims: v.dims}
	if v.typ == vecF64 && (t == vec1Bit || t == vecF8) {
		// These two read a float64 source at full precision.
		x := make([]float64, v.dims)
		for i := range x {
			x[i] = f64At(v.data, i)
		}
		if t == vec1Bit {
			out.data = signBits(x)
		} else {
			out.data, out.alpha, out.shift = quantize(x)
		}
		return out
	}
	f := v.floats()
	switch t {
	case vecF32:
		out.data = make([]byte, 0, 4*len(f))
		for _, x := range f {
			out.data = binary.LittleEndian.AppendUint32(out.data, math.Float32bits(x))
		}
	case vecF64:
		out.data = make([]byte, 0, 8*len(f))
		for _, x := range f {
			out.data = binary.LittleEndian.AppendUint64(out.data, math.Float64bits(float64(x)))
		}
	case vec1Bit:
		out.data = signBits(f)
	case vecF8:
		out.data, out.alpha, out.shift = quantize(f)
	case vecF16, vecFB16:
		enc := f16FromFloat
		if t == vecFB16 {
			enc = bf16FromFloat
		}
		out.data = make([]byte, 0, 2*len(f))
		for _, x := range f {
			out.data = binary.LittleEndian.AppendUint16(out.data, enc(x))
		}
	}
	return out
}

// signBits packs each component's sign: 1 for positive, else 0.
func signBits[F float32 | float64](x []F) []byte {
	b := make([]byte, (len(x)+7)/8)
	for i, v := range x {
		if v > 0 {
			b[i/8] |= 1 << (i & 7)
		}
	}
	return b
}

// quantize maps components linearly onto 0..255 (vectorConvertToF8). For a
// float64 source the division runs in float64, as libSQL's does; min and max
// are rounded to float32 either way.
func quantize[F float32 | float64](x []F) (q []byte, alpha, shift float32) {
	// C's MIN(a,b) is a<b?a:b and MAX(a,b) is a>b?a:b: on a tie (0 and
	// -0) each keeps the newer value, which decides the sign of a zero shift.
	var lo, hi F
	for i, v := range x {
		if i == 0 || !(lo < v) {
			lo = v
		}
		if i == 0 || !(hi > v) {
			hi = v
		}
	}
	shift = float32(lo)
	alpha = (float32(hi) - shift) / 255
	q = make([]byte, len(x))
	for i, v := range x {
		q[i] = clip255(float32((v - F(shift)) / F(alpha)))
	}
	return q, alpha, shift
}

// clip255 is libSQL's clip(f, 0, 255). A NaN (all components equal, so alpha
// is 0) becomes 0, as C's conversion does on the oracle's amd64.
func clip255(f float32) byte {
	switch {
	case f < 0, f != f:
		return 0
	case f > 255:
		return 255
	}
	return byte(float64(f) + 0.5)
}

// ---- half precision ------------------------------------------------------------

func f16ToFloat(h uint16) float32 {
	sgn := uint32(h&0x8000) << 16
	exp := int(h>>10&0x1f) - 15
	mnt := uint32(h & 0x3ff)
	switch {
	case exp == 16: // Inf, NaN
		exp = 128
		if mnt != 0 {
			mnt = 1 << 22
		}
	case exp == -15 && mnt == 0:
		exp = -127
	case exp == -15: // subnormal: normalize
		exp++
		for mnt&0x400 == 0 {
			mnt <<= 1
			exp--
		}
		mnt = (mnt & 0x3ff) << 13
	default:
		mnt <<= 13
	}
	return math.Float32frombits(sgn | uint32(exp+127)<<23 | mnt)
}

func f16FromFloat(f float32) uint16 {
	i := math.Float32bits(f)
	sgn := i >> 16 & 0x8000
	exp := int(i>>23&0xff) - 127
	m := i & 0x7fffff
	switch {
	case exp == 128: // Inf, NaN
		exp = 16
		if m != 0 {
			m = 1 << 22
		}
	case exp > 15:
		exp, m = 16, 0
	case exp >= -25 && exp < -14:
		m = (m | 0x800000) >> uint(-exp-14)
		exp = -15
	case exp < -24:
		exp, m = -15, 0
	}
	if m&0x1fff > 0x1000-(m>>13&1) { // round to nearest, ties to even
		m += 0x2000
	}
	mnt := m >> 13
	return uint16(sgn | uint32(exp+15+int(mnt>>10))<<10 | mnt&0x3ff)
}

func bf16ToFloat(h uint16) float32   { return math.Float32frombits(uint32(h) << 16) }
func bf16FromFloat(f float32) uint16 { return uint16(math.Float32bits(f) >> 16) }

// ---- encoding ------------------------------------------------------------------

// blob is the stored form: the data and its metadata.
func (v vector) blob() []byte {
	if v.typ == vecF8 {
		b := make([]byte, align4(v.dims), align4(v.dims)+11) // zero padding
		copy(b, v.data)
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(v.alpha))
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(v.shift))
		return append(b, 0, byte(align4(v.dims)-v.dims), byte(vecF8))
	}
	b := append([]byte(nil), v.data...)
	switch v.typ {
	case vecF32:
		return b
	case vec1Bit:
		if len(b)%2 == 0 {
			b = append(b, 0) // pad, so the length stays odd
		}
		return append(b, byte(8*(len(b)+1)-v.dims), byte(vec1Bit))
	}
	return append(b, byte(v.typ))
}

// text is '[x,y,...]', each component printed as printf's %g prints it.
func (v vector) text() []byte {
	out := []byte{'['}
	for i := range v.dims {
		if i > 0 {
			out = append(out, ',')
		}
		var x float64
		if v.typ == vecF64 {
			x = f64At(v.data, i)
		} else {
			x = float64(f32At(v.data, i))
		}
		s, _ := printfFloat('g', x, 0, -1, false, 0, false, false, false, false, 0)
		out = append(out, s...)
	}
	return append(out, ']')
}

// ---- distances -----------------------------------------------------------------

// cosine finishes a cosine distance from its float32 sums as libSQL does:
// the norms' product in float32, the rest in float64, the result a float32.
func cosine(dot, n1, n2 float32) float32 {
	return float32(1 - float64(dot)/math.Sqrt(float64(n1*n2)))
}

// distanceCos is vector_distance_cos for two vectors of one type and length.
// float1bit vectors answer their Hamming distance, as in libSQL.
func distanceCos(a, b vector) float32 {
	switch a.typ {
	case vecF32:
		return cosF32(a.data, b.data, a.dims)
	case vecF64:
		var dot, n1, n2 float64
		for i := range a.dims {
			x, y := f64At(a.data, i), f64At(b.data, i)
			dot += float64(x * y)
			n1 += float64(x * x)
			n2 += float64(y * y)
		}
		return float32(1 - dot/math.Sqrt(n1*n2))
	case vec1Bit:
		d := 0
		for i := range a.data {
			d += bits.OnesCount8(a.data[i] ^ b.data[i])
		}
		return float32(d)
	case vecF8:
		return cosF8(a, b)
	}
	dec := f16ToFloat
	if a.typ == vecFB16 {
		dec = bf16ToFloat
	}
	var dot, n1, n2 float32
	for i := range a.dims {
		x, y := dec(u16At(a.data, i)), dec(u16At(b.data, i))
		dot += float32(x * y)
		n1 += float32(x * x)
		n2 += float32(y * y)
	}
	return cosine(dot, n1, n2)
}

// cosF32 is the float32 cosine distance, the case search runs most.
func cosF32(a, b []byte, dims int) float32 {
	a, b = a[:4*dims], b[:4*dims]
	var dot, n1, n2 float32
	for i := 0; i < len(a); i += 4 {
		x := math.Float32frombits(binary.LittleEndian.Uint32(a[i:]))
		y := math.Float32frombits(binary.LittleEndian.Uint32(b[i:]))
		dot += float32(x * y)
		n1 += float32(x * x)
		n2 += float32(y * y)
	}
	return cosine(dot, n1, n2)
}

// cosF8 expands (alpha q + shift) algebraically over integer sums of the
// quantized bytes, as vectorF8DistanceCos does.
func cosF8(a, b vector) float32 {
	var s1, s2, q1, q2, d uint32
	for i := range a.dims {
		x, y := uint32(a.data[i]), uint32(b.data[i])
		s1 += x
		s2 += y
		q1 += x * x
		q2 += y * y
		d += x * y
	}
	// Each product rounded on its own, left to right, as C evaluates them.
	mul := func(x, y float32) float32 { return float32(x * y) }
	a1, h1, a2, h2 := a.alpha, a.shift, b.alpha, b.shift
	n := float32(a.dims)
	dot := mul(mul(a1, a2), float32(d)) + mul(mul(a1, h2), float32(s1)) + mul(mul(a2, h1), float32(s2)) + mul(mul(h1, h2), n)
	n1 := mul(mul(a1, a1), float32(q1)) + mul(mul(mul(2, a1), h1), float32(s1)) + mul(mul(h1, h1), n)
	n2 := mul(mul(a2, a2), float32(q2)) + mul(mul(mul(2, a2), h2), float32(s2)) + mul(mul(h2, h2), n)
	return cosine(dot, n1, n2)
}

// distanceL2 is vector_distance_l2; the caller has refused float1bit.
func distanceL2(a, b vector) float32 {
	if a.typ == vecF64 {
		var sum float64
		for i := range a.dims {
			d := f64At(a.data, i) - f64At(b.data, i)
			sum += float64(d * d)
		}
		return float32(math.Sqrt(sum))
	}
	var sum float32
	if a.typ == vecF32 {
		for i := range a.dims {
			d := f32At(a.data, i) - f32At(b.data, i)
			sum += float32(d * d)
		}
	} else {
		x, y := a.floats(), b.floats()
		for i := range x {
			d := x[i] - y[i]
			sum += float32(d * d)
		}
	}
	return float32(math.Sqrt(float64(sum)))
}

// distanceValue is a distance as SQL sees it: a NaN (the cosine distance of a
// zero vector) is NULL, as sqlite3_result_double makes it.
func distanceValue(d float32) Value {
	if d != d {
		return Value{Typ: Null}
	}
	return Value{Typ: Float, F: float64(d)}
}

// ---- the SQL functions -----------------------------------------------------------

// vectorEncoders maps vector() and its typed spellings to the type each makes.
var vectorEncoders = map[string]vecType{
	"vector": vecF32, "vector32": vecF32, "vector64": vecF64, "vector1bit": vec1Bit,
	"vector8": vecF8, "vector16": vecF16, "vectorb16": vecFB16,
}

func init() {
	for name := range vectorEncoders {
		supportedFuncs[name] = true
	}
	for _, name := range []string{"vector_extract", "vector_distance_cos", "vector_distance_l2", "libsql_vector_idx"} {
		supportedFuncs[name] = true
	}
}

// vectorArity is funcArity for the vector functions.
func vectorArity(name string) (lo, hi int, ok bool) {
	switch name {
	case "vector_distance_cos", "vector_distance_l2":
		return 2, 2, true
	case "vector_extract":
		return 1, 1, true
	case "libsql_vector_idx":
		return 1, -1, true
	}
	_, ok = vectorEncoders[name]
	return 1, 1, ok
}

// callVectorFunc evaluates a vector function; ok is false for any other name.
// funcArity has checked the argument count at prepare time.
func callVectorFunc(name string, args []Value) (res Value, ok bool, err error) {
	if t, isEnc := vectorEncoders[name]; isEnc {
		v, err := parseVector(args[0], t)
		if err != nil {
			return Value{}, true, err
		}
		return Value{Typ: Blob, S: v.convert(t).blob()}, true, nil
	}
	switch name {
	case "libsql_vector_idx":
		// Only a marker for CREATE INDEX (vector_index.go); called, it is its
		// first argument, as in libSQL.
		return args[0], true, nil
	case "vector_extract":
		v, err := parseVector(args[0], 0)
		if err != nil {
			return Value{}, true, err
		}
		if v.typ != vecF64 {
			v = v.convert(vecF32)
		}
		return Value{Typ: Text, S: v.text()}, true, nil
	case "vector_distance_cos", "vector_distance_l2":
		a, err := parseVector(args[0], 0)
		if err != nil {
			return Value{}, true, err
		}
		b, err := parseVector(args[1], 0)
		if err != nil {
			return Value{}, true, err
		}
		switch {
		case a.typ != b.typ:
			return Value{}, true, vecErr("vector_distance: vectors must have the same type: %d != %d", a.typ, b.typ)
		case a.dims != b.dims:
			return Value{}, true, vecErr("vector_distance: vectors must have the same length: %d != %d", a.dims, b.dims)
		case name == "vector_distance_cos":
			return distanceValue(distanceCos(a, b)), true, nil
		case a.typ == vec1Bit:
			return Value{}, true, vecErr("vector_distance: l2 distance is not supported for float1bit vectors")
		}
		return distanceValue(distanceL2(a, b)), true, nil
	}
	return Value{}, false, nil
}
