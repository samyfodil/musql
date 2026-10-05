// This file implements fts5 tokenizers: unicode61 (the default),
// ascii, trigram, and porter. A nil *fts5Tokenizer is the default.
// Every tokenizer is self-contained because this engine re-tokenizes
// %_content for every MATCH query and builds its inverted index the same way.
//
// Tokenizer forms:
//
//	unicode61 [remove_diacritics N] [tokenchars S] [separators S] [categories S]
//	ascii     [tokenchars S] [separators S]
//	trigram   [case_sensitive B] [remove_diacritics N]
//	porter    [<base tokenizer> <its args>...]
//
// Porter is a stemmer wrapper over a base tokenizer. All others decline.
package engine

import (
	"fmt"
	"strings"
)

// fts5TokenizerKind is which tokenizer a table uses.
type fts5TokenizerKind uint8

const (
	fts5TokUnicode61 fts5TokenizerKind = iota // the default, and the zero value
	fts5TokAscii
	fts5TokTrigram
)

// fts5Tokenizer is one fts5 table's tokenizer, the tokenize= argument
// resolved. The NIL POINTER is the default tokenizer, which is what makes every
// call site below tolerate a table that never spelled tokenize= (and what keeps
// a default-tokenizer table's behavior bit-identical to before this file).
type fts5Tokenizer struct {
	kind fts5TokenizerKind

	// removeDia is remove_diacritics: 0 (keep), 1 (SIMPLE) or 2 (COMPLEX).
	// unicode61 defaults to 1; trigram defaults to 0.
	removeDia int32

	// cats is the unicode61 categories= set: bit i means fts5UnicodeCategory code i
	// is a token character. Category code 0 is always a token character.
	cats uint32

	// except maps codepoints to their token-character override (tokenchars=/separators=).
	// It is an override map, not a flip: later assignments take precedence.
	// The override is keyed by the raw codepoint before folding.
	except map[rune]bool

	// asciiTok is the ascii tokenizer's token-character table over 128 ASCII bytes.
	// Every byte >= 0x80 is unconditionally a token character.
	asciiTok [128]bool

	// caseSensitive is trigram's case_sensitive flag: when set, no folding.
	// Mutually exclusive with nonzero remove_diacritics.
	caseSensitive bool

	// stem is set for porter tokenizer: run every token through fts5PorterStem,
	// keeping the base tokenizer's offsets.
	stem bool
}

// fts5DefaultCategories is unicode61's category set: codes 5..9 (Ll Lm Lo Lt Lu),
// 13..15 (Nd Nl No) and 31 (Co).
const fts5DefaultCategories = 1<<5 | 1<<6 | 1<<7 | 1<<8 | 1<<9 |
	1<<13 | 1<<14 | 1<<15 | 1<<31

// fts5CatCode maps categories= names to their fts5UnicodeCategory codes.
var fts5CatCode = map[string]int{
	"Cc": 1, "Cf": 2, "Cn": 3, "Cs": 4, "Co": 31,
	"Ll": 5, "Lm": 6, "Lo": 7, "Lt": 8, "Lu": 9,
	"Mc": 10, "Me": 11, "Mn": 12,
	"Nd": 13, "Nl": 14, "No": 15,
	"Pc": 16, "Pd": 17, "Pe": 18, "Pf": 19, "Pi": 20, "Po": 21, "Ps": 22,
	"Sc": 23, "Sk": 24, "Sm": 25, "So": 26,
	"Zl": 27, "Zp": 28, "Zs": 29,
}

// isAlnum reports whether c is a TOKEN CHARACTER for this tokenizer -- the
// fts5UnicodeIsAlnum(p, iCode) the tokenizer's main loop tests.
func (tk *fts5Tokenizer) isAlnum(c int32) bool {
	// Codepoint 0 is NEVER a token character, whatever the configuration says.
	// It falls out of the default category set anyway (NUL is Cc), but
	// "categories 'L* N* Co Cc'" would otherwise make it one -- and there the
	// oracle still breaks the token: 'abc'||char(0)||'def' indexes as the two
	// terms 'abc' and 'def', while 'abc'||char(1)||'def' (also Cc) indexes as
	// the SINGLE term x'61626301646566'. Same for ascii, whose tokenchars=
	// cannot reach a NUL but whose default set must exclude it for the same
	// reason. (A token character whose FOLD is zero is a different case and
	// does NOT break the token: under "categories 'L* N* Co Mn'",
	// 'cafe'||char(769)||'x' is the one term 'cafex'.)
	if c == 0 {
		return false
	}
	if tk == nil {
		return fts5IsAlnum(c)
	}
	if tk.kind == fts5TokAscii {
		if c >= 128 {
			return true
		}
		return c >= 0 && tk.asciiTok[c]
	}
	if v, ok := tk.except[rune(c)]; ok {
		return v
	}
	cat := fts5UnicodeCategory(uint32(c))
	return cat == 0 || tk.cats&(1<<uint(cat)) != 0
}

// fold applies this tokenizer's case/diacritic folding to one token character.
func (tk *fts5Tokenizer) fold(c int32) int32 {
	if tk == nil {
		return fts5UnicodeFold(c, fts5DefaultRemoveDiacritic)
	}
	switch tk.kind {
	case fts5TokAscii:
		// ascii folds uppercase ASCII only; bytes >= 0x80 pass through untouched.
		if c >= 'A' && c <= 'Z' {
			return c + ('a' - 'A')
		}
		return c
	case fts5TokTrigram:
		if tk.caseSensitive {
			return c
		}
	}
	return fts5UnicodeFold(c, tk.removeDia)
}

// tokenize returns this tokenizer's tokens for text, in order (a token's
// position is its index).
func (tk *fts5Tokenizer) tokenize(text string) []string {
	spans := tk.spans(text)
	out := make([]string, len(spans))
	for i, sp := range spans {
		out[i] = sp.tok
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// spans returns this tokenizer's tokens with their [start,end) byte offsets.
func (tk *fts5Tokenizer) spans(text string) []fts5TokenSpan {
	out := tk.baseSpans(text)
	if tk != nil && tk.stem {
		// Stemming replaces token text but keeps offsets for snippet()/highlight().
		for i := range out {
			out[i].tok = fts5PorterStem(out[i].tok)
		}
	}
	return out
}

// baseSpans is spans() without the porter wrapper: the tokenizer proper.
func (tk *fts5Tokenizer) baseSpans(text string) []fts5TokenSpan {
	if tk != nil && tk.kind == fts5TokTrigram {
		return tk.trigramSpans(text)
	}
	if tk != nil && tk.kind == fts5TokAscii {
		return tk.asciiSpans(text)
	}
	b := []byte(text)
	n := len(b)
	var out []fts5TokenSpan
	i := 0
	for i < n {
		// Skip separators: a token may only START on a token character
		// (diacritic-only starts are not token starts).
		for i < n {
			cp, sz := fts5NextRune(b, i)
			if tk.isAlnum(int32(cp)) {
				break
			}
			i += sz
		}
		if i >= n {
			break
		}
		start := i
		// Consume token characters and combining diacritics, folding each.
		var sb strings.Builder
		for i < n {
			cp, sz := fts5NextRune(b, i)
			ci := int32(cp)
			if !(tk.isAlnum(ci) || fts5IsDiacritic(ci)) {
				break
			}
			i += sz
			if folded := tk.fold(ci); folded != 0 {
				fts5AppendRune(&sb, folded)
			}
		}
		out = append(out, fts5TokenSpan{tok: sb.String(), start: start, end: i})
	}
	return out
}

// asciiSpans tokenizes on bytes: works on bytes, not codepoints.
// Bytes >= 0x80 pass through unchanged, never decoded, folded or validated.
func (tk *fts5Tokenizer) asciiSpans(text string) []fts5TokenSpan {
	n := len(text)
	var out []fts5TokenSpan
	i := 0
	for i < n {
		for i < n && !tk.isAlnum(int32(text[i])) {
			i++
		}
		if i >= n {
			break
		}
		start := i
		var sb strings.Builder
		for i < n && tk.isAlnum(int32(text[i])) {
			c := text[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			sb.WriteByte(c)
			i++
		}
		out = append(out, fts5TokenSpan{tok: sb.String(), start: start, end: i})
	}
	return out
}

// trigramSpans is the trigram tokenizer: one token per starting codepoint,
// each the three folded codepoints beginning there. Fewer than three codepoints
// yields none. MATCH becomes a substring search: 'abcd' tokenizes to "abc" "bcd".
func (tk *fts5Tokenizer) trigramSpans(text string) []fts5TokenSpan {
	b := []byte(text)
	n := len(b)
	// Decode codepoints once: cps[k] is the k'th folded codepoint, off[k] its byte offset.
	// Codepoints that fold to nothing (combining marks under remove_diacritics) are dropped.
	var cps []int32
	var off []int
	for i := 0; i < n; {
		cp, sz := fts5NextRune(b, i)
		if folded := tk.fold(int32(cp)); folded != 0 {
			cps = append(cps, folded)
			off = append(off, i)
		}
		i += sz
	}
	off = append(off, n)
	if len(cps) < 3 {
		return nil
	}
	out := make([]fts5TokenSpan, 0, len(cps)-2)
	for k := 0; k+2 < len(cps); k++ {
		var sb strings.Builder
		fts5AppendRune(&sb, cps[k])
		fts5AppendRune(&sb, cps[k+1])
		fts5AppendRune(&sb, cps[k+2])
		out = append(out, fts5TokenSpan{tok: sb.String(), start: off[k], end: off[k+3]})
	}
	return out
}

// fts5AppendRune appends c's UTF-8 encoding, matching fts5's WRITE_UTF8 macro.
// Unlike strings.Builder.WriteRune, it preserves codepoints above U+10FFFF.
func fts5AppendRune(sb *strings.Builder, c int32) {
	u := uint32(c)
	switch {
	case u < 0x80:
		sb.WriteByte(byte(u))
	case u < 0x800:
		sb.WriteByte(byte(0xC0 | (u >> 6)))
		sb.WriteByte(byte(0x80 | (u & 0x3F)))
	case u < 0x10000:
		sb.WriteByte(byte(0xE0 | (u >> 12)))
		sb.WriteByte(byte(0x80 | ((u >> 6) & 0x3F)))
		sb.WriteByte(byte(0x80 | (u & 0x3F)))
	default:
		sb.WriteByte(byte(0xF0 | ((u >> 18) & 0x07)))
		sb.WriteByte(byte(0x80 | ((u >> 12) & 0x3F)))
		sb.WriteByte(byte(0x80 | ((u >> 6) & 0x3F)))
		sb.WriteByte(byte(0x80 | (u & 0x3F)))
	}
}

// fts5ParseTokenizer resolves a tokenize= VALUE into a tokenizer.
// An empty value returns the default tokenizer (nil, nil).
// Arguments are space-separated; barewords or single-quoted strings.
// Names are case-insensitive. Unknown names or values are errors.
func fts5ParseTokenizer(val string) (*fts5Tokenizer, error) {
	if val == "" {
		return nil, nil
	}
	args, err := fts5SplitTokenizeArgs(val)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("parse error in tokenize directive")
	}
	return fts5BuildTokenizer(args[0], args[1:])
}

// fts5BuildTokenizer creates a tokenizer from a name and arguments.
func fts5BuildTokenizer(name string, args []string) (*fts5Tokenizer, error) {
	tk := &fts5Tokenizer{}
	switch {
	case strings.EqualFold(name, "unicode61"):
		tk.kind = fts5TokUnicode61
		tk.removeDia = fts5DefaultRemoveDiacritic
		tk.cats = fts5DefaultCategories
	case strings.EqualFold(name, "ascii"):
		tk.kind = fts5TokAscii
		for c := 0; c < 128; c++ {
			tk.asciiTok[c] = c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
		}
	case strings.EqualFold(name, "trigram"):
		tk.kind = fts5TokTrigram
		tk.removeDia = 0
	case strings.EqualFold(name, "porter"):
		return fts5PorterCreate(args)
	default:
		return nil, fmt.Errorf("fts5: no such tokenizer: %s", name)
	}
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return nil, fmt.Errorf("fts5: error in tokenizer constructor: %s takes a value", args[i])
		}
		if err := tk.applyArg(args[i], args[i+1]); err != nil {
			return nil, err
		}
	}
	if tk.kind == fts5TokTrigram && tk.caseSensitive && tk.removeDia != 0 {
		return nil, fmt.Errorf("fts5: error in tokenizer constructor: trigram case_sensitive and remove_diacritics are mutually exclusive")
	}
	return tk, nil
}

// fts5PorterCreate creates a porter tokenizer, which wraps a base tokenizer.
// Leading "porter" arguments are skipped. The first non-porter argument names the base.
// With no arguments, the base is "unicode61".
func fts5PorterCreate(args []string) (*fts5Tokenizer, error) {
	base := "unicode61"
	for len(args) > 0 {
		if strings.EqualFold(args[0], "porter") {
			args = args[1:]
			continue
		}
		base = args[0]
		break
	}
	var baseArgs []string
	if len(args) > 0 {
		baseArgs = args[1:]
	}
	tk, err := fts5BuildTokenizer(base, baseArgs)
	if err != nil {
		return nil, err
	}
	if tk == nil {
		return nil, fmt.Errorf("fts5: error in tokenizer constructor: %s", base)
	}
	tk.stem = true
	return tk, nil
}

// applyArg records one "<name> <value>" tokenizer argument.
// ascii refuses remove_diacritics and categories; trigram refuses tokenchars/separators/categories.
func (tk *fts5Tokenizer) applyArg(name, val string) error {
	bad := fmt.Errorf("fts5: error in tokenizer constructor: unknown or out-of-range argument %q", name)
	switch {
	case strings.EqualFold(name, "remove_diacritics") && (tk.kind == fts5TokUnicode61 || tk.kind == fts5TokTrigram):
		// Values matched literally: "0", "1", or "2" only.
		switch val {
		case "0", "1", "2":
			tk.removeDia = int32(val[0] - '0')
			return nil
		}
		return bad
	case strings.EqualFold(name, "case_sensitive") && tk.kind == fts5TokTrigram:
		switch val {
		case "0":
			tk.caseSensitive = false
			return nil
		case "1":
			tk.caseSensitive = true
			return nil
		}
		return bad
	case strings.EqualFold(name, "tokenchars") && tk.kind != fts5TokTrigram:
		tk.addExceptions(val, true)
		return nil
	case strings.EqualFold(name, "separators") && tk.kind != fts5TokTrigram:
		tk.addExceptions(val, false)
		return nil
	case strings.EqualFold(name, "categories") && tk.kind == fts5TokUnicode61:
		return tk.setCategories(val)
	}
	return bad
}

// addExceptions records codepoints as token characters or separators.
func (tk *fts5Tokenizer) addExceptions(s string, tokenChar bool) {
	if tk.kind == fts5TokAscii {
		// ascii's table only covers the 128 ASCII bytes; non-ASCII is ignored.
		for _, r := range s {
			if r < 128 {
				tk.asciiTok[r] = tokenChar
			}
		}
		return
	}
	if tk.except == nil {
		tk.except = map[rune]bool{}
	}
	for _, r := range s {
		tk.except[r] = tokenChar
	}
}

// setCategories replaces the token-character category set from two-letter names
// ("Nd") or one-letter wildcards ("N*"). Names are space or tab separated.
func (tk *fts5Tokenizer) setCategories(s string) error {
	var cats uint32
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' }) {
		if len(f) < 2 {
			return fmt.Errorf("fts5: error in tokenizer constructor: malformed categories name %q", f)
		}
		if f[1] == '*' {
			n := 0
			for name, code := range fts5CatCode {
				if name[0] == f[0] {
					cats |= 1 << uint(code)
					n++
				}
			}
			if n == 0 {
				return fmt.Errorf("fts5: error in tokenizer constructor: malformed categories name %q", f)
			}
			continue
		}
		code, ok := fts5CatCode[f[:2]]
		if !ok {
			return fmt.Errorf("fts5: error in tokenizer constructor: malformed categories name %q", f)
		}
		cats |= 1 << uint(code)
	}
	tk.cats = cats
	return nil
}

// fts5SplitTokenizeArgs splits a tokenize= value into its arguments.
func fts5SplitTokenizeArgs(s string) ([]string, error) {
	parse := fmt.Errorf("parse error in tokenize directive")
	var out []string
	i := 0
	for {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			return out, nil
		}
		if s[i] == '\'' {
			i++
			var sb strings.Builder
			closed := false
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						sb.WriteByte('\'')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				sb.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, parse
			}
			out = append(out, sb.String())
		} else {
			start := i
			for i < len(s) && fts5IsBarewordByte(s[i]) {
				i++
			}
			if i == start {
				return nil, parse
			}
			out = append(out, s[start:i])
		}
		if i < len(s) && s[i] != ' ' {
			return nil, parse
		}
	}
}

// fts5IsBarewordByte reports whether c may appear in an unquoted tokenizer argument.
// Letters, digits, '_', and non-ASCII bytes are allowed.
func fts5IsBarewordByte(c byte) bool {
	return c >= 0x80 || c == '_' ||
		c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// ---------------------------------------------------------------------------
// The porter stemmer
// ---------------------------------------------------------------------------

// fts5PorterMaxToken is the maximum token length (in bytes) for stemming.
// Longer tokens or tokens shorter than three bytes are passed through unstemmed.
const fts5PorterMaxToken = 64

// fts5PorterRule is one stemming rule: suffix to match, condition on the stem,
// replacement text, and whether the rule reports a match to its caller.
type fts5PorterRule struct {
	suffix string
	cond   func(stem []byte) bool
	output string
	ret    bool
}

// fts5PorterApply applies stemming rules to a buffer.
// Rules are checked in order; the first matching rule applies (even if its condition fails).
func fts5PorterApply(buf []byte, rules []fts5PorterRule) ([]byte, bool) {
	for _, r := range rules {
		n := len(r.suffix)
		// Buffer must be longer than the suffix.
		if len(buf) <= n || string(buf[len(buf)-n:]) != r.suffix {
			continue
		}
		stem := buf[:len(buf)-n]
		if r.cond != nil && !r.cond(stem) {
			return buf, false
		}
		return append(stem, r.output...), r.ret
	}
	return buf, false
}

// fts5PorterIsVowel reports whether c is a vowel (or 'y' if yIsVowel is set).
func fts5PorterIsVowel(c byte, yIsVowel bool) bool {
	return c == 'a' || c == 'e' || c == 'i' || c == 'o' || c == 'u' ||
		(yIsVowel && c == 'y')
}

// fts5PorterGobbleVC scans past one vowel-then-consonant run.
// Returns the offset just after it, or 0 if there is no such run.
func fts5PorterGobbleVC(z []byte, prevCons bool) int {
	cons := prevCons
	i := 0
	for ; i < len(z); i++ {
		cons = !fts5PorterIsVowel(z[i], cons)
		if !cons {
			break
		}
	}
	for i++; i < len(z); i++ {
		cons = !fts5PorterIsVowel(z[i], cons)
		if cons {
			return i + 1
		}
	}
	return 0
}

// fts5PorterMGt0 is the (m > 0) rule condition.
func fts5PorterMGt0(stem []byte) bool { return fts5PorterGobbleVC(stem, false) != 0 }

// fts5PorterMGt1 is the (m > 1) rule condition.
func fts5PorterMGt1(stem []byte) bool {
	n := fts5PorterGobbleVC(stem, false)
	return n != 0 && fts5PorterGobbleVC(stem[n:], true) != 0
}

// fts5PorterMEq1 is the (m = 1) rule condition.
func fts5PorterMEq1(stem []byte) bool {
	n := fts5PorterGobbleVC(stem, false)
	return n != 0 && fts5PorterGobbleVC(stem[n:], true) == 0
}

// fts5PorterOstar is the (*o) rule condition: the stem ends consonant-vowel-consonant
// with the last consonant not w, x or y. An empty stem returns false.
func fts5PorterOstar(stem []byte) bool {
	if len(stem) == 0 {
		return false
	}
	if c := stem[len(stem)-1]; c == 'w' || c == 'x' || c == 'y' {
		return false
	}
	mask := 0
	cons := false
	for i := 0; i < len(stem); i++ {
		cons = !fts5PorterIsVowel(stem[i], cons)
		mask <<= 1
		if cons {
			mask++
		}
	}
	return mask&0x0007 == 0x0005
}

// fts5PorterMGt1AndSorT is the (m > 1 and (*S or *T)) rule condition.
func fts5PorterMGt1AndSorT(stem []byte) bool {
	if len(stem) == 0 {
		return false
	}
	c := stem[len(stem)-1]
	return (c == 's' || c == 't') && fts5PorterMGt1(stem)
}

// fts5PorterVowel is the (*v*) rule condition. 'y' counts as a vowel
// everywhere but in the first position.
func fts5PorterVowel(stem []byte) bool {
	for i := 0; i < len(stem); i++ {
		if fts5PorterIsVowel(stem[i], i > 0) {
			return true
		}
	}
	return false
}

// The five rule tables for the porter stemmer.
var (
	fts5PorterStep1BRules = []fts5PorterRule{
		{"eed", fts5PorterMGt0, "ee", false},
		{"ed", fts5PorterVowel, "", true},
		{"ing", fts5PorterVowel, "", true},
	}
	fts5PorterStep1B2Rules = []fts5PorterRule{
		{"at", nil, "ate", true},
		{"bl", nil, "ble", true},
		{"iz", nil, "ize", true},
	}
	fts5PorterStep2Rules = []fts5PorterRule{
		{"ational", fts5PorterMGt0, "ate", false},
		{"tional", fts5PorterMGt0, "tion", false},
		{"enci", fts5PorterMGt0, "ence", false},
		{"anci", fts5PorterMGt0, "ance", false},
		{"izer", fts5PorterMGt0, "ize", false},
		{"logi", fts5PorterMGt0, "log", false},
		{"bli", fts5PorterMGt0, "ble", false},
		{"alli", fts5PorterMGt0, "al", false},
		{"entli", fts5PorterMGt0, "ent", false},
		{"eli", fts5PorterMGt0, "e", false},
		{"ousli", fts5PorterMGt0, "ous", false},
		{"ization", fts5PorterMGt0, "ize", false},
		{"ation", fts5PorterMGt0, "ate", false},
		{"ator", fts5PorterMGt0, "ate", false},
		{"alism", fts5PorterMGt0, "al", false},
		{"iveness", fts5PorterMGt0, "ive", false},
		{"fulness", fts5PorterMGt0, "ful", false},
		{"ousness", fts5PorterMGt0, "ous", false},
		{"aliti", fts5PorterMGt0, "al", false},
		{"iviti", fts5PorterMGt0, "ive", false},
		{"biliti", fts5PorterMGt0, "ble", false},
	}
	fts5PorterStep3Rules = []fts5PorterRule{
		{"icate", fts5PorterMGt0, "ic", false},
		{"ative", fts5PorterMGt0, "", false},
		{"alize", fts5PorterMGt0, "al", false},
		{"iciti", fts5PorterMGt0, "ic", false},
		{"ical", fts5PorterMGt0, "ic", false},
		{"ful", fts5PorterMGt0, "", false},
		{"ness", fts5PorterMGt0, "", false},
	}
	fts5PorterStep4Rules = []fts5PorterRule{
		{"al", fts5PorterMGt1, "", false},
		{"ance", fts5PorterMGt1, "", false},
		{"ence", fts5PorterMGt1, "", false},
		{"er", fts5PorterMGt1, "", false},
		{"ic", fts5PorterMGt1, "", false},
		{"able", fts5PorterMGt1, "", false},
		{"ible", fts5PorterMGt1, "", false},
		{"ant", fts5PorterMGt1, "", false},
		{"ement", fts5PorterMGt1, "", false},
		{"ment", fts5PorterMGt1, "", false},
		{"ent", fts5PorterMGt1, "", false},
		{"ion", fts5PorterMGt1AndSorT, "", false},
		{"ou", fts5PorterMGt1, "", false},
		{"ism", fts5PorterMGt1, "", false},
		{"ate", fts5PorterMGt1, "", false},
		{"iti", fts5PorterMGt1, "", false},
		{"ous", fts5PorterMGt1, "", false},
		{"ive", fts5PorterMGt1, "", false},
		{"ize", fts5PorterMGt1, "", false},
	}
)

// fts5PorterStep1A is step 1A of the porter stemmer.
// Its caller guarantees at least three bytes.
func fts5PorterStep1A(buf []byte) []byte {
	n := len(buf)
	if buf[n-1] != 's' {
		return buf
	}
	if buf[n-2] == 'e' {
		if (n > 4 && buf[n-4] == 's' && buf[n-3] == 's') || (n > 3 && buf[n-3] == 'i') {
			return buf[:n-2]
		}
		return buf[:n-1]
	}
	if buf[n-2] != 's' {
		return buf[:n-1]
	}
	return buf
}

// fts5PorterByteAt safely reads buf[i], returning 0 if out of bounds.
func fts5PorterByteAt(buf []byte, i int) byte {
	if i < 0 || i >= len(buf) {
		return 0
	}
	return buf[i]
}

// fts5PorterStem applies the porter stemming algorithm to one token.
func fts5PorterStem(token string) string {
	if len(token) > fts5PorterMaxToken || len(token) < 3 {
		return token
	}
	// Buffer only needs one extra byte for step 1B's appended 'e'.
	buf := make([]byte, len(token), len(token)+1)
	copy(buf, token)

	// Step 1.
	buf = fts5PorterStep1A(buf)
	buf, matched := fts5PorterApply(buf, fts5PorterStep1BRules)
	if matched {
		var matched2 bool
		buf, matched2 = fts5PorterApply(buf, fts5PorterStep1B2Rules)
		if !matched2 {
			c := buf[len(buf)-1]
			if !fts5PorterIsVowel(c, false) && c != 'l' && c != 's' && c != 'z' &&
				c == fts5PorterByteAt(buf, len(buf)-2) {
				buf = buf[:len(buf)-1]
			} else if fts5PorterMEq1(buf) && fts5PorterOstar(buf) {
				buf = append(buf, 'e')
			}
		}
	}

	// Step 1C.
	if buf[len(buf)-1] == 'y' && fts5PorterVowel(buf[:len(buf)-1]) {
		buf[len(buf)-1] = 'i'
	}

	// Steps 2 through 4.
	buf, _ = fts5PorterApply(buf, fts5PorterStep2Rules)
	buf, _ = fts5PorterApply(buf, fts5PorterStep3Rules)
	buf, _ = fts5PorterApply(buf, fts5PorterStep4Rules)

	// Step 5a.
	if buf[len(buf)-1] == 'e' {
		stem := buf[:len(buf)-1]
		if fts5PorterMGt1(stem) || (fts5PorterMEq1(stem) && !fts5PorterOstar(stem)) {
			buf = stem
		}
	}

	// Step 5b.
	if len(buf) > 1 && buf[len(buf)-1] == 'l' && buf[len(buf)-2] == 'l' &&
		fts5PorterMGt1(buf[:len(buf)-1]) {
		buf = buf[:len(buf)-1]
	}
	return string(buf)
}
