package engine

// vec1's SQL functions (vec1.c's aFunc[]): vec1_info, vec1_to_json (and its
// _f and _i spellings), vec1_from_json, vec1_l2_distance, vec1_cos_distance and
// vec1_config. vec1_train, the aggregate, is vec1_train.go.
//
// vec1 registers every one without SQLITE_DETERMINISTIC, so none is a
// constant to the planner, and none may appear in an index, a CHECK or a
// generated column.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

func init() {
	for _, name := range []string{"vec1_info", "vec1_to_json", "vec1_to_json_f", "vec1_to_json_i", "vec1_from_json",
		"vec1_l2_distance", "vec1_cos_distance", "vec1_config", "vec1_train"} {
		supportedFuncs[name] = true
		nonConstantFuncs[name] = true
	}
}

// vec1Arity is funcArity for vec1's scalar functions.
func vec1Arity(name string) (lo, hi int, ok bool) {
	switch name {
	case "vec1_info":
		return 0, 0, true
	case "vec1_to_json", "vec1_to_json_f", "vec1_to_json_i", "vec1_from_json":
		return 1, 1, true
	case "vec1_l2_distance", "vec1_cos_distance":
		return 2, 2, true
	case "vec1_config":
		return 1, 2, true
	}
	return 0, 0, false
}

// callVec1Func evaluates a vec1 scalar function other than vec1_config; ok is
// false for any other name.
func callVec1Func(name string, args []Value) (Value, bool, error) {
	switch name {
	case "vec1_info":
		// The arithmetic is vec1's portable C, which is how vec1 describes a
		// build without AVX2 or NEON.
		return Value{Typ: Text, S: []byte("version " + vec1Version + " (Scalar, multi-threaded)")}, true, nil
	case "vec1_to_json", "vec1_to_json_f":
		v, err := vec1ToJSON(args[0], false)
		return v, true, err
	case "vec1_to_json_i":
		v, err := vec1ToJSON(args[0], true)
		return v, true, err
	case "vec1_from_json":
		v, err := vec1FromJSON(args[0])
		return v, true, err
	case "vec1_l2_distance", "vec1_cos_distance":
		a, b := args[0], args[1]
		if len(a.S) != len(b.S) || len(a.S)%4 != 0 || a.Typ != Blob || b.Typ != Blob {
			return Value{}, true, fmt.Errorf("%s: bad arguments", name)
		}
		x, y := vec1Floats(a.S), vec1Floats(b.S)
		if name == "vec1_l2_distance" {
			return Value{Typ: Float, F: vec1L2(x, y)}, true, nil
		}
		return Value{Typ: Float, F: vec1Cos(x, y)}, true, nil
	}
	return Value{}, false, nil
}

// vec1ToJSON is vec1ToJsonGeneric: "%.9g" per float32, or "%d" per int32.
func vec1ToJSON(v Value, ints bool) (Value, error) {
	b := vec1BlobOf(v)
	if len(b) == 0 || len(b)%4 != 0 {
		return Value{}, fmt.Errorf("vec1: invalid vector argument")
	}
	var out strings.Builder
	for i := 0; i < len(b); i += 4 {
		if i == 0 {
			out.WriteByte('[')
		} else {
			out.WriteByte(',')
		}
		if ints {
			out.WriteString(strconv.FormatInt(int64(int32(vec1Native.Uint32(b[i:]))), 10))
			continue
		}
		f := math.Float32frombits(vec1Native.Uint32(b[i:]))
		s, err := fnPrintf("%.9g", []Value{{Typ: Float, F: float64(f)}})
		if err != nil {
			return Value{}, err
		}
		out.Write(s.S)
	}
	out.WriteByte(']')
	return Value{Typ: Text, S: []byte(out.String())}, nil
}

// vec1FromJSON is vec1FromJsonFunc: a JSON array of numbers, each read by
// strtod and rounded to float32. Only a space is whitespace around the
// brackets and commas (vec1_isspace), though strtod skips any before a
// number.
func vec1FromJSON(v Value) (Value, error) {
	var z []byte
	switch v.Typ {
	case Null:
		return Value{Typ: Null}, nil
	case Text, Blob:
		z = v.S
	default:
		z = []byte(valueToText(v))
	}
	// vec1 allocates a float per two bytes of the argument, and
	// sqlite3_malloc(0) is NULL: a one-byte argument runs out of memory.
	if len(z)/2 == 0 {
		return Value{}, fmt.Errorf("out of memory")
	}
	if i := strings.IndexByte(string(z), 0); i >= 0 {
		z = z[:i]
	}
	fail := fmt.Errorf("error parsing json vector")
	p := 0
	at := func(i int) byte {
		if i < len(z) {
			return z[i]
		}
		return 0
	}
	for at(p) == ' ' {
		p++
	}
	if at(p) != '[' {
		return Value{}, fail
	}
	p++
	var out []float32
	for {
		for at(p) == ' ' {
			p++
		}
		f, n := vec1Strtod(z[p:])
		if n == 0 {
			return Value{}, fail
		}
		p += n
		out = append(out, float32(f))
		for at(p) == ' ' {
			p++
		}
		if at(p) == ']' {
			p++
			break
		}
		if at(p) != ',' {
			return Value{}, fail
		}
		p++
	}
	for at(p) == ' ' {
		p++
	}
	if p < len(z) {
		return Value{}, fail
	}
	return Value{Typ: Blob, S: vec1FloatBytes(out)}, nil
}

// vec1Strtod is C's strtod over s: the value of the longest prefix that is a
// number, and its length (0 for none). Leading whitespace is skipped; the
// forms are decimal, hexadecimal ("0x"), INF/INFINITY and NAN, each signed.
func vec1Strtod(s []byte) (float64, int) {
	i := 0
	for i < len(s) && (s[i] == ' ' || (s[i] >= '\t' && s[i] <= '\r')) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	sign := func(f float64) float64 {
		if neg {
			return -f
		}
		return f
	}
	word := func(w string) bool {
		return len(s)-i >= len(w) && strings.EqualFold(string(s[i:i+len(w)]), w)
	}
	switch {
	case word("infinity"):
		return sign(math.Inf(1)), i + 8
	case word("inf"):
		return sign(math.Inf(1)), i + 3
	case word("nan"):
		n := i + 3
		if n < len(s) && s[n] == '(' {
			j := n + 1
			for j < len(s) && (s[j] == '_' || s[j] >= '0' && s[j] <= '9' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
				j++
			}
			if j < len(s) && s[j] == ')' {
				n = j + 1
			}
		}
		bits := uint64(0x7ff8000000000000)
		if neg {
			bits |= 1 << 63
		}
		return math.Float64frombits(bits), n
	}
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	isHex := func(c byte) bool { return isDigit(c) || (c|0x20) >= 'a' && (c|0x20) <= 'f' }
	if i+1 < len(s) && s[i] == '0' && (s[i+1]|0x20) == 'x' {
		j := i + 2
		mant := j
		for j < len(s) && isHex(s[j]) {
			j++
		}
		digits := j - mant
		if j < len(s) && s[j] == '.' {
			j++
			f0 := j
			for j < len(s) && isHex(s[j]) {
				j++
			}
			digits += j - f0
		}
		if digits > 0 {
			end := j
			if j < len(s) && (s[j]|0x20) == 'p' {
				k := j + 1
				if k < len(s) && (s[k] == '+' || s[k] == '-') {
					k++
				}
				if k < len(s) && isDigit(s[k]) {
					for k < len(s) && isDigit(s[k]) {
						k++
					}
					end = k
				}
			}
			text := string(s[i:end])
			if !strings.ContainsAny(text, "pP") {
				text += "p0"
			}
			f, _ := strconv.ParseFloat(text, 64)
			return sign(f), end
		}
		// "0x" with no digit is the number 0, ending before the x.
		return sign(0), i + 1
	}
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	digits := j - i
	if j < len(s) && s[j] == '.' {
		j++
		f0 := j
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		digits += j - f0
	}
	if digits == 0 {
		return 0, 0
	}
	end := j
	if j < len(s) && (s[j]|0x20) == 'e' {
		k := j + 1
		if k < len(s) && (s[k] == '+' || s[k] == '-') {
			k++
		}
		if k < len(s) && isDigit(s[k]) {
			for k < len(s) && isDigit(s[k]) {
				k++
			}
			end = k
		}
	}
	text := string(s[i:end])
	if strings.HasSuffix(text, ".") {
		text += "0"
	}
	if strings.HasPrefix(text, ".") {
		text = "0" + text
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !strings.Contains(err.Error(), "range") {
		return 0, 0
	}
	return sign(f), end
}

// vec1ConfigFunc is vec1ConfigFunc: nthread and nprobe, per connection.
func vec1ConfigFunc(c *Vec1Connection, args []Value) (Value, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	param := ""
	if args[0].Typ != Null {
		param = valueToText(args[0])
	}
	switch strings.ToLower(param) {
	case "nthread":
		if len(args) == 2 {
			n := int(int32(valueToInt64Trunc(args[1])))
			if n < 1 || n > vec1MaxThread {
				return Value{}, fmt.Errorf("vec1: nthread requires an integer value between 1 and %d", vec1MaxThread)
			}
			c.nThread = n
		}
		return Value{Typ: Int, I: int64(c.nThread)}, nil
	case "nprobe":
		if len(args) == 2 {
			f := 0.0
			if n := vec1NumericType(args[1]); n.Typ == Int || n.Typ == Float {
				f = sqliteValueDouble(args[1])
			}
			if f <= 0 {
				return Value{}, fmt.Errorf("vec1: nprobe must be a numeric value greater than 0.0")
			}
			c.nProbe = f
		}
		return Value{Typ: Float, F: c.nProbe}, nil
	}
	return Value{}, fmt.Errorf("vec1: no such parameter: %s", param)
}

// vec1Config is vec1_config() as the machine runs it, with its connection.
func (m *vdbe) vec1Config(args []Value) (Value, error) {
	if m.wctx != nil && m.wctx.db != nil {
		return vec1ConfigFunc(m.wctx.db.vec1Conn(), args)
	}
	if m.pager != nil {
		return vec1ConfigFunc(m.pager.vec1Conn(), args)
	}
	return Value{}, fmt.Errorf("%w: vec1_config() with no connection", errVDBEUnsupported)
}
