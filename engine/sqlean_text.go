// sqlean's text family (src/text): character-based substrings, search, split
// and join, trim and pad, simple Unicode case mapping, and the Postgres-style
// aliases. Strings are walked as sqlean walks them: an ill-formed UTF-8
// subpart is one character, so every function is total over any bytes.
package engine

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"unicode"
	"unicode/utf8"
)

// textLimitLength is SQLITE_LIMIT_LENGTH's default, which alloc_result holds
// results to.
const textLimitLength = 1000000000

var errTextTooBig = errors.New("string or blob too big")

func init() {
	fixed := func(name string, lo, hi int, fn func([]Value) (Value, error)) sqleanScalar {
		return sqleanScalar{name: name, lo: lo, hi: hi, fn: fn}
	}
	substring := fixed("text_substring", 2, 3, textSubstringFn)
	slice := fixed("text_slice", 2, 3, textSliceFn)
	left := fixed("text_left", 2, 2, textPartFn(textLeft))
	right := fixed("text_right", 2, 2, textPartFn(textRight))
	index := fixed("text_index", 2, 2, textLocateFn(textIndex))
	lastIndex := fixed("text_last_index", 2, 2, textLocateFn(textLastIndex))
	contains := fixed("text_contains", 2, 2, textMatchFn(func(s, sub []byte) bool { return bytes.Contains(s, sub) }))
	prefix := fixed("text_has_prefix", 2, 2, textMatchFn(bytes.HasPrefix))
	suffix := fixed("text_has_suffix", 2, 2, textMatchFn(bytes.HasSuffix))
	count := fixed("text_count", 2, 2, textLocateFn(textCount))
	like := fixed("text_like", 2, 2, textMatchFn(textLike))
	split := fixed("text_split", 3, 3, textSplitFn)
	join := fixed("text_join", 0, -1, textJoinFn)
	concat := fixed("text_concat", 0, -1, textConcatFn)
	repeat := fixed("text_repeat", 2, 2, textRepeatFn)
	ltrim := fixed("text_ltrim", 1, 2, textTrimFn(textLtrim))
	rtrim := fixed("text_rtrim", 1, 2, textTrimFn(textRtrim))
	trim := fixed("text_trim", 1, 2, textTrimFn(func(s, chars []byte) []byte {
		return textRtrim(textLtrim(s, chars), chars)
	}))
	lpad := fixed("text_lpad", 2, 3, textPadFn(true))
	rpad := fixed("text_rpad", 2, 3, textPadFn(false))
	upper := fixed("text_upper", 1, 1, textCaseFn(textUpper))
	lower := fixed("text_lower", 1, 1, textCaseFn(textLower))
	title := fixed("text_title", 1, 1, textCaseFn(textTitle))
	casefold := fixed("text_casefold", 1, 1, textCaseFn(textCasefold))
	replace := fixed("text_replace", 3, 4, textReplaceFn)
	translate := fixed("text_translate", 3, 3, textTranslateFn)
	reverse := fixed("text_reverse", 1, 1, textReverseFn)
	length := fixed("text_length", 1, 1, textLengthFn)
	size := fixed("text_size", 1, 1, textSizeFn(1))
	bitsize := fixed("text_bitsize", 1, 1, textSizeFn(8))

	// concat_ws, concat and octet_length are engine built-ins, so sqlean's
	// registration for older SQLite versions has nothing to add.
	sqleanFamilies["text"] = &sqleanFamily{scalars: []sqleanScalar{
		substring, slice, left, right,
		index, lastIndex, contains, prefix, suffix, count, like,
		split, join, concat, repeat,
		ltrim, rtrim, trim, lpad, rpad,
		upper, lower, title, casefold,
		replace, translate, reverse,
		length, size, bitsize,
		sqleanAlias("left", left), sqleanAlias("right", right),
		sqleanAlias("strpos", index), sqleanAlias("starts_with", prefix),
		sqleanAlias("split_part", split), sqleanAlias("repeat", repeat),
		sqleanAlias("btrim", trim), sqleanAlias("lpad", lpad), sqleanAlias("rpad", rpad),
		sqleanAlias("translate", translate), sqleanAlias("reverse", reverse),
		sqleanAlias("char_length", length), sqleanAlias("character_length", length),
		sqleanAlias("bit_length", bitsize),
	}}
}

// textArg is arg_text: a number is read as its text, a blob has no text
// (the result is NULL).
func textArg(v Value) ([]byte, bool) {
	if v.Typ == Blob {
		return nil, false
	}
	return sqleanBytes(v), true
}

// textInt is arg_int64: only a value whose numeric type is INTEGER.
func textInt(v Value) (int64, bool) {
	isInt, i, _, ok := mathNumericType(v)
	return i, ok && isInt
}

func textResult(b []byte) Value { return Value{Typ: Text, S: slices.Clone(b)} }

// textAlloc is alloc_result's size check, on a size already saturated.
func textAlloc(size uint64) error {
	if size > textLimitLength {
		return errTextTooBig
	}
	return nil
}

func textMul(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if b > math.MaxUint64/a {
		return math.MaxUint64
	}
	return a * b
}

func textAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// textNext is utf8_next: the character at byte i and the offset after it. A
// valid sequence gives its code point; each maximal ill-formed subpart is one
// character, valued with its bytes packed big-endian above any code point.
func textNext(s []byte, i int) (uint32, int) {
	b0 := s[i]
	if b0 < 0x80 {
		return uint32(b0), i + 1
	}
	var need int
	var c uint32
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case b0 >= 0xC2 && b0 <= 0xDF:
		need, c = 1, uint32(b0&0x1F)
	case b0 >= 0xE0 && b0 <= 0xEF:
		need, c = 2, uint32(b0&0x0F)
		if b0 == 0xE0 {
			lo = 0xA0
		} else if b0 == 0xED {
			hi = 0x9F
		}
	case b0 >= 0xF0 && b0 <= 0xF4:
		need, c = 3, uint32(b0&0x07)
		if b0 == 0xF0 {
			lo = 0x90
		} else if b0 == 0xF4 {
			hi = 0x8F
		}
	default:
		return uint32(b0) << 16, i + 1
	}
	avail := len(s) - i
	k := 1
	for k <= need && k < avail && s[i+k] >= lo && s[i+k] <= hi {
		c = c<<6 | uint32(s[i+k]&0x3F)
		lo, hi = 0x80, 0xBF
		k++
	}
	if k == need+1 {
		return c, i + k
	}
	var packed uint32
	for j := range 3 {
		packed <<= 8
		if j < k {
			packed |= uint32(s[i+j])
		}
	}
	return packed, i + k
}

const textInvalidMin = 0x800000

func textLen(s []byte) int {
	n := 0
	for i := 0; i < len(s); n++ {
		_, i = textNext(s, i)
	}
	return n
}

// textSkip is utf8_skip: the offset count characters after i.
func textSkip(s []byte, i int, count uint64) int {
	for ; count > 0 && i < len(s); count-- {
		_, i = textNext(s, i)
	}
	return i
}

// textFind is find: the offset of sub at or after from, -1 if none.
func textFind(s, sub []byte, from int) int {
	if len(sub) == 0 {
		return from
	}
	if from >= len(s) {
		return -1
	}
	at := bytes.Index(s[from:], sub)
	if at < 0 {
		return -1
	}
	return from + at
}

func textSpan(s []byte, first, last uint64) []byte {
	if first >= last {
		return nil
	}
	start := textSkip(s, 0, first)
	end := textSkip(s, start, last-first)
	return s[start:end]
}

func textSubstring(s []byte, start, count int64) []byte {
	if count <= 0 {
		return nil
	}
	if start < 1 {
		if start+count <= 1 {
			return nil
		}
		count = start + count - 1
		start = 1
	}
	from := textSkip(s, 0, uint64(start-1))
	to := textSkip(s, from, uint64(count))
	return s[from:to]
}

func textSliceIndex(i, n int64) uint64 {
	switch {
	case i > 0:
		return uint64(min(i-1, n))
	case i == 0:
		return 0
	case i < -n:
		return 0
	}
	return uint64(n + i)
}

func textSlice(s []byte, start, end int64) []byte {
	n := int64(textLen(s))
	return textSpan(s, textSliceIndex(start, n), textSliceIndex(end, n))
}

func textLeft(s []byte, count int64) []byte {
	if count >= 0 {
		return textSpan(s, 0, uint64(count))
	}
	n := int64(textLen(s))
	if count < -n {
		return nil
	}
	return textSpan(s, 0, uint64(n+count))
}

func textRight(s []byte, count int64) []byte {
	n := int64(textLen(s))
	var keep int64
	switch {
	case count >= 0:
		keep = min(count, n)
	case count >= -n:
		keep = n + count
	}
	return textSpan(s, uint64(n-keep), uint64(n))
}

func textSubstringFn(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := textArg(args[0])
	start, ok2 := textInt(args[1])
	length := int64(math.MaxInt64)
	ok3 := true
	if len(args) == 3 {
		length, ok3 = textInt(args[2])
	}
	if !ok || !ok2 || !ok3 {
		return sqleanNull, nil
	}
	return textResult(textSubstring(s, start, length)), nil
}

func textSliceFn(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := textArg(args[0])
	start, ok2 := textInt(args[1])
	end := int64(math.MaxInt64)
	ok3 := true
	if len(args) == 3 {
		end, ok3 = textInt(args[2])
	}
	if !ok || !ok2 || !ok3 {
		return sqleanNull, nil
	}
	return textResult(textSlice(s, start, end)), nil
}

func textPartFn(part func([]byte, int64) []byte) func([]Value) (Value, error) {
	return func(args []Value) (Value, error) {
		if sqleanAnyNull(args) {
			return sqleanNull, nil
		}
		s, ok := textArg(args[0])
		n, ok2 := textInt(args[1])
		if !ok || !ok2 {
			return sqleanNull, nil
		}
		return textResult(part(s, n)), nil
	}
}

// textTwo reads the first two arguments as text.
func textTwo(args []Value) (a, b []byte, ok bool) {
	a, ok = textArg(args[0])
	if !ok {
		return nil, nil, false
	}
	b, ok = textArg(args[1])
	return a, b, ok
}

func textLocateFn(locate func(s, sub []byte) int64) func([]Value) (Value, error) {
	return func(args []Value) (Value, error) {
		if sqleanAnyNull(args) {
			return sqleanNull, nil
		}
		s, sub, ok := textTwo(args)
		if !ok {
			return sqleanNull, nil
		}
		return sqleanInt(locate(s, sub)), nil
	}
}

func textMatchFn(match func(s, sub []byte) bool) func([]Value) (Value, error) {
	return func(args []Value) (Value, error) {
		if sqleanAnyNull(args) {
			return sqleanNull, nil
		}
		s, sub, ok := textTwo(args)
		if !ok {
			return sqleanNull, nil
		}
		if match(s, sub) {
			return sqleanInt(1), nil
		}
		return sqleanInt(0), nil
	}
}

// textPosition is position: the 1-based character position of byte offset off.
func textPosition(s []byte, off int) int64 {
	pos := int64(1)
	for i := 0; i < off; {
		_, i = textNext(s, i)
		if i <= off {
			pos++
		}
	}
	return pos
}

func textIndex(s, sub []byte) int64 {
	at := textFind(s, sub, 0)
	if at < 0 {
		return 0
	}
	return textPosition(s, at)
}

func textLastIndex(s, sub []byte) int64 {
	if len(sub) > len(s) {
		return 0
	}
	for at := len(s) - len(sub); at >= 0; at-- {
		if bytes.Equal(s[at:at+len(sub)], sub) {
			return textPosition(s, at)
		}
	}
	return 0
}

func textCount(s, sub []byte) int64 {
	if len(sub) == 0 {
		return int64(textLen(s)) + 1
	}
	var n int64
	for at := textFind(s, sub, 0); at >= 0; at = textFind(s, sub, at+len(sub)) {
		n++
	}
	return n
}

// textLike is text_like with the pattern first: % any run, _ one character,
// anything else by simple case folding.
func textLike(p, s []byte) bool {
	pi, si := 0, 0
	starPi, starSi := -1, 0
	for si < len(s) {
		if pi < len(p) {
			pc, pnext := textNext(p, pi)
			if pc == '%' {
				pi, starPi, starSi = pnext, pnext, si
				continue
			}
			sc, snext := textNext(s, si)
			if pc == '_' || textFold(pc) == textFold(sc) {
				pi, si = pnext, snext
				continue
			}
		}
		if starPi < 0 {
			return false
		}
		_, starSi = textNext(s, starSi)
		pi, si = starPi, starSi
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}

func textSplit(s, sep []byte, part int64) ([]byte, bool) {
	if part == 0 {
		return nil, false
	}
	if len(sep) == 0 {
		if part == 1 || part == -1 {
			return s, true
		}
		return nil, true
	}
	var index uint64
	if part > 0 {
		index = uint64(part - 1)
	} else {
		nparts := uint64(1)
		for at := textFind(s, sep, 0); at >= 0; at = textFind(s, sep, at+len(sep)) {
			nparts++
		}
		back := -uint64(part)
		if back > nparts {
			return nil, true
		}
		index = nparts - back
	}
	start := 0
	for i := uint64(0); ; i++ {
		at := textFind(s, sep, start)
		if i == index {
			end := at
			if at < 0 {
				end = len(s)
			}
			return s[start:end], true
		}
		if at < 0 {
			return nil, true
		}
		start = at + len(sep)
	}
}

func textSplitFn(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, sep, ok := textTwo(args)
	part, ok2 := textInt(args[2])
	if !ok || !ok2 {
		return sqleanNull, nil
	}
	out, ok := textSplit(s, sep, part)
	if !ok {
		return sqleanNull, nil
	}
	return textResult(out), nil
}

// textJoin joins the non-NULL args with sep; a blob makes the result NULL.
func textJoin(sep []byte, args []Value) (Value, error) {
	var parts [][]byte
	var size uint64
	for _, a := range args {
		if a.Typ == Null {
			continue
		}
		p, ok := textArg(a)
		if !ok {
			return sqleanNull, nil
		}
		parts = append(parts, p)
		size = textAdd(size, uint64(len(p)))
	}
	if len(parts) > 1 {
		size = textAdd(size, textMul(uint64(len(sep)), uint64(len(parts)-1)))
	}
	if err := textAlloc(size); err != nil {
		return sqleanNull, err
	}
	return Value{Typ: Text, S: bytes.Join(parts, sep)}, nil
}

func textJoinFn(args []Value) (Value, error) {
	if len(args) < 2 {
		return sqleanNull, errors.New("wrong number of arguments to function text_join()")
	}
	if args[0].Typ == Null {
		return sqleanNull, nil
	}
	sep, ok := textArg(args[0])
	if !ok {
		return sqleanNull, nil
	}
	return textJoin(sep, args[1:])
}

func textConcatFn(args []Value) (Value, error) {
	if len(args) < 1 {
		return sqleanNull, errors.New("wrong number of arguments to function text_concat()")
	}
	return textJoin(nil, args)
}

func textRepeatFn(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := textArg(args[0])
	count, ok2 := textInt(args[1])
	if !ok || !ok2 {
		return sqleanNull, nil
	}
	if count <= 0 || len(s) == 0 {
		return sqleanText(""), nil
	}
	if err := textAlloc(textMul(uint64(len(s)), uint64(count))); err != nil {
		return sqleanNull, err
	}
	return Value{Typ: Text, S: bytes.Repeat(s, int(count))}, nil
}

// textInSet is in_set: c matches a character of set by its bytes.
func textInSet(set, c []byte) bool {
	for i := 0; i < len(set); {
		start := i
		_, i = textNext(set, i)
		if bytes.Equal(set[start:i], c) {
			return true
		}
	}
	return false
}

func textLtrim(s, chars []byte) []byte {
	i := 0
	for i < len(s) {
		_, next := textNext(s, i)
		if !textInSet(chars, s[i:next]) {
			break
		}
		i = next
	}
	return s[i:]
}

func textRtrim(s, chars []byte) []byte {
	end := 0
	for i := 0; i < len(s); {
		start := i
		_, i = textNext(s, i)
		if !textInSet(chars, s[start:i]) {
			end = i
		}
	}
	return s[:end]
}

func textTrimFn(trim func(s, chars []byte) []byte) func([]Value) (Value, error) {
	return func(args []Value) (Value, error) {
		if sqleanAnyNull(args) {
			return sqleanNull, nil
		}
		s, ok := textArg(args[0])
		chars := []byte(" ")
		ok2 := true
		if len(args) == 2 {
			chars, ok2 = textArg(args[1])
		}
		if !ok || !ok2 {
			return sqleanNull, nil
		}
		return textResult(trim(s, chars)), nil
	}
}

func textPadFn(left bool) func([]Value) (Value, error) {
	return func(args []Value) (Value, error) {
		if sqleanAnyNull(args) {
			return sqleanNull, nil
		}
		s, ok := textArg(args[0])
		length, ok2 := textInt(args[1])
		fill := []byte(" ")
		ok3 := true
		if len(args) == 3 {
			fill, ok3 = textArg(args[2])
		}
		if !ok || !ok2 || !ok3 {
			return sqleanNull, nil
		}
		var want uint64
		if length > 0 {
			want = uint64(length)
		}
		n := uint64(textLen(s))
		if want <= n {
			return textResult(s[:textSkip(s, 0, want)]), nil
		}
		if len(fill) == 0 {
			return textResult(s), nil
		}
		npad := want - n
		fillLen := uint64(textLen(fill))
		full := npad / fillLen
		rest := textSkip(fill, 0, npad%fillLen)
		size := textAdd(textAdd(textMul(uint64(len(fill)), full), uint64(rest)), uint64(len(s)))
		if err := textAlloc(size); err != nil {
			return sqleanNull, err
		}
		pad := append(bytes.Repeat(fill, int(full)), fill[:rest]...)
		if left {
			return Value{Typ: Text, S: append(pad, s...)}, nil
		}
		return Value{Typ: Text, S: append(slices.Clone(s), pad...)}, nil
	}
}

func textCaseFn(change func([]byte) []byte) func([]Value) (Value, error) {
	return func(args []Value) (Value, error) {
		if sqleanAnyNull(args) {
			return sqleanNull, nil
		}
		s, ok := textArg(args[0])
		if !ok {
			return sqleanNull, nil
		}
		out := change(s)
		if err := textAlloc(uint64(len(out))); err != nil {
			return sqleanNull, err
		}
		return Value{Typ: Text, S: out}, nil
	}
}

func textMap(s []byte, f func(c uint32) uint32, onInvalid func()) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		start := i
		var c uint32
		c, i = textNext(s, i)
		if c >= textInvalidMin {
			out = append(out, s[start:i]...)
			if onInvalid != nil {
				onInvalid()
			}
			continue
		}
		out = utf8.AppendRune(out, rune(f(c)))
	}
	return out
}

func textUpper(s []byte) []byte    { return textMap(s, textToUpper, nil) }
func textLower(s []byte) []byte    { return textMap(s, textToLower, nil) }
func textCasefold(s []byte) []byte { return textMap(s, textFold, nil) }

func textTitle(s []byte) []byte {
	inWord := false
	return textMap(s, func(c uint32) uint32 {
		res := textToTitle(c)
		if inWord {
			res = textToLower(c)
		}
		inWord = textIsWord(c)
		return res
	}, func() { inWord = false })
}

// sqlean's tables pair ß with ẞ (U+1E9E), which Unicode's simple mappings do
// not.
func textToUpper(c uint32) uint32 {
	if c == 0xDF {
		return 0x1E9E
	}
	return uint32(unicode.ToUpper(rune(c)))
}

func textToLower(c uint32) uint32 { return uint32(unicode.ToLower(rune(c))) }

func textToTitle(c uint32) uint32 {
	if c >= 0x10D0 && c <= 0x10FF {
		return c
	}
	if c == 0xDF {
		return 0x1E9E
	}
	return uint32(unicode.ToTitle(rune(c)))
}

// textFold is rune_casefold: the simple case folding, which for every
// character with one is the lowercase of its uppercase.
func textFold(c uint32) uint32 {
	if c >= textInvalidMin {
		return c
	}
	return uint32(unicode.ToLower(unicode.ToUpper(rune(c))))
}

// textWordScripts are the scripts whose uncased letters count as word
// characters; sqlean's tables stop at the Basic Multilingual Plane.
var textWordScripts = []*unicode.RangeTable{
	unicode.Latin, unicode.Greek, unicode.Cyrillic, unicode.Han, unicode.Arabic, unicode.Devanagari,
}

func textIsWord(c uint32) bool {
	if c < 0x80 {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
	}
	if textToLower(c) != c || textToUpper(c) != c {
		return true
	}
	if c > 0xFFFF {
		return false
	}
	r := rune(c)
	return unicode.Is(unicode.Lt, r) || unicode.Is(unicode.Nd, r) || unicode.Is(unicode.Nl, r) ||
		unicode.Is(unicode.Pc, r) || unicode.IsOneOf(textWordScripts, r)
}

func textReplaceFn(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := textArg(args[0])
	old, ok2 := textArg(args[1])
	repl, ok3 := textArg(args[2])
	count := int64(math.MaxInt64)
	ok4 := true
	if len(args) == 4 {
		count, ok4 = textInt(args[3])
	}
	if !ok || !ok2 || !ok3 || !ok4 {
		return sqleanNull, nil
	}
	if len(old) == 0 || count <= 0 {
		return textResult(s), nil
	}
	var out []byte
	from := 0
	for i := int64(0); i < count; i++ {
		at := textFind(s, old, from)
		if at < 0 {
			break
		}
		out = append(out, s[from:at]...)
		out = append(out, repl...)
		from = at + len(old)
		if err := textAlloc(uint64(len(out))); err != nil {
			return sqleanNull, err
		}
	}
	out = append(out, s[from:]...)
	if err := textAlloc(uint64(len(out))); err != nil {
		return sqleanNull, err
	}
	return Value{Typ: Text, S: out}, nil
}

func textTranslateFn(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := textArg(args[0])
	from, ok2 := textArg(args[1])
	to, ok3 := textArg(args[2])
	if !ok || !ok2 || !ok3 {
		return sqleanNull, nil
	}
	var out []byte
	for i := 0; i < len(s); {
		start := i
		_, i = textNext(s, i)
		k := -1
		for j, f := 0, 0; f < len(from); j++ {
			fs := f
			_, f = textNext(from, f)
			if bytes.Equal(from[fs:f], s[start:i]) {
				k = j
				break
			}
		}
		if k < 0 {
			out = append(out, s[start:i]...)
			continue
		}
		ts := textSkip(to, 0, uint64(k))
		te := ts
		if te < len(to) {
			_, te = textNext(to, te)
		}
		out = append(out, to[ts:te]...)
	}
	if err := textAlloc(uint64(len(out))); err != nil {
		return sqleanNull, err
	}
	return Value{Typ: Text, S: out}, nil
}

func textReverseFn(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := textArg(args[0])
	if !ok {
		return sqleanNull, nil
	}
	if err := textAlloc(uint64(len(s))); err != nil {
		return sqleanNull, err
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); {
		start := i
		_, i = textNext(s, i)
		copy(out[len(s)-i:], s[start:i])
	}
	return Value{Typ: Text, S: out}, nil
}

func textLengthFn(args []Value) (Value, error) {
	if sqleanAnyNull(args) {
		return sqleanNull, nil
	}
	s, ok := textArg(args[0])
	if !ok {
		return sqleanNull, nil
	}
	return sqleanInt(int64(textLen(s))), nil
}

func textSizeFn(unit int64) func([]Value) (Value, error) {
	return func(args []Value) (Value, error) {
		if sqleanAnyNull(args) {
			return sqleanNull, nil
		}
		s, ok := textArg(args[0])
		if !ok {
			return sqleanNull, nil
		}
		return sqleanInt(int64(len(s)) * unit), nil
	}
}
