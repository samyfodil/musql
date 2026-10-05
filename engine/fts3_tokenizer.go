// This file implements fts3/fts4's TOKENIZE= specification for simple, unicode61,
// and porter tokenizers. The tokenizer decides which byte strings land in segments,
// so mismatches between write and read paths corrupt indexes.
//
// unicode61 uses the same token-character test and case folding as fts5.
// Its arguments (remove_diacritics, tokenchars, separators) differ from fts5:
// tokenchars=/separators= build an XOR exception set, not an override map.
// There is no categories= argument.
//
// simple uses a delimiter argument (ARGV[1], ignored if only one arg total).
// Every listed byte becomes a delimiter; every other byte is a token character.
// UTF-8 delimiters are not supported.
//
// porter is implemented in fts3_porter.go. icu is declined.
package engine

import (
	"fmt"
	"strings"
)

// fts3TokKind is which tokenizer a table uses.
type fts3TokKind uint8

const (
	fts3TokSimple fts3TokKind = iota // the default, and the zero value
	fts3TokUnicode61
	fts3TokPorter
)

// fts3Tokenizer is one table's resolved tokenize= specification. The NIL
// POINTER is the default "simple" tokenizer, which is what lets every call site
// stay unchanged for a table that never spelled tokenize=.
type fts3Tokenizer struct {
	kind fts3TokKind
	// u is the fts5 tokenizer object unicode61 borrows: same token-character
	// test, same folding, same diacritic rule, same READ_UTF8 (see this file's
	// comment for the 3,514-codepoint probe that established the equivalence).
	// fts3's XOR exception set is converted into fts5's per-codepoint override
	// at parse time, which is exactly equivalent once the set is known.
	u *fts5Tokenizer
	// delim is simple's explicit delimiter table (simple_tokenizer.delim),
	// nil for the default one. Indexed by BYTE, and only over ASCII: a byte
	// >= 0x80 is never a delimiter (simpleDelim's "c<0x80 &&" guard), which is
	// what keeps a UTF-8 sequence inside one token.
	delim *[128]bool
}

// fts3ParseTokenizerSpec resolves the text after "tokenize" + one character
// ("=simple", " unicode61 \"remove_diacritics=0\"") into a tokenizer, or
// declines by name. A nil result is the default simple tokenizer.
func fts3ParseTokenizerSpec(module, spec string) (*fts3Tokenizer, error) {
	toks, ok := fts3SpecTokens(spec)
	if !ok {
		return nil, fmt.Errorf("%s: tokenizer specification %q is not a form this engine has pinned against C fts3", module, strings.TrimSpace(spec))
	}
	name := ""
	if len(toks) > 0 {
		name = toks[0]
	}
	return fts3NewTokenizer(module, name, toks[1:])
}

// fts3NewTokenizer is the tokenizer hash lookup plus the chosen module's
// xCreate: name selects the tokenizer and args are the ALREADY-DEQUOTED
// arguments after it. Shared with fts3tokenize (vtab_fts3tok.go), which
// arrives at the same (name, args) pair from CREATE VIRTUAL TABLE module
// arguments rather than from a tokenize= specification -- C fts3 hands both
// to the same sqlite3_tokenizer_module.xCreate.
func fts3NewTokenizer(module, name string, args []string) (*fts3Tokenizer, error) {
	switch {
	case strings.EqualFold(name, "simple"):
		return fts3NewSimple(module, args)
	case strings.EqualFold(name, "unicode61"):
		return fts3NewUnicode61(module, args)
	case strings.EqualFold(name, "porter"):
		// porter ignores all arguments.
		return &fts3Tokenizer{kind: fts3TokPorter}, nil
	}
	return nil, fmt.Errorf("%s: the %q tokenizer is not implemented by this engine (only \"simple\", \"unicode61\" and \"porter\" are; a different tokenizer changes which rows MATCH selects and the index bytes on disk)", module, name)
}

// fts3NewSimple is simpleCreate: its delimiter table comes from ARGV[1], and
// only when there are at least two arguments (see this file's comment). A nil
// result is the default table, which every call site already treats as simple.
func fts3NewSimple(module string, args []string) (*fts3Tokenizer, error) {
	if len(args) < 2 {
		return nil, nil
	}
	var delim [128]bool
	for i := 0; i < len(args[1]); i++ {
		ch := args[1][i]
		if ch >= 0x80 {
			// simpleCreate returns SQLITE_ERROR, which fts3 reports with its
			// usual catch-all wording for a constructor failure.
			return nil, fmt.Errorf("%s: unknown tokenizer", module)
		}
		delim[ch] = true
	}
	return &fts3Tokenizer{kind: fts3TokSimple, delim: &delim}, nil
}

// fts3NewUnicode61 is unicodeCreate: the argument loop, byte for byte.
func fts3NewUnicode61(module string, args []string) (*fts3Tokenizer, error) {
	u := &fts5Tokenizer{kind: fts5TokUnicode61, removeDia: 1, cats: fts5DefaultCategories}
	// unicodeAddExceptions' set, accumulated across every argument before it is
	// converted: a codepoint joins it only when its DEFAULT alnum-ness differs
	// from the one being requested, so a later argument asking for the default
	// adds nothing and CANNOT undo an earlier flip.
	xor := map[rune]bool{}
	addExceptions := func(chars string, alnum bool) {
		for _, r := range chars {
			if fts5IsAlnum(int32(r)) == alnum {
				continue
			}
			xor[r] = true
		}
	}
	for _, a := range args {
		switch {
		case a == "remove_diacritics=0":
			u.removeDia = 0
		case a == "remove_diacritics=1":
			u.removeDia = 1
		case a == "remove_diacritics=2":
			u.removeDia = 2
		case strings.HasPrefix(a, "tokenchars="):
			addExceptions(a[len("tokenchars="):], true)
		case strings.HasPrefix(a, "separators="):
			addExceptions(a[len("separators="):], false)
		default:
			return nil, fmt.Errorf("%s: unknown tokenizer", module)
		}
	}
	if len(xor) > 0 {
		u.except = make(map[rune]bool, len(xor))
		for r := range xor {
			u.except[r] = !fts5IsAlnum(int32(r))
		}
	}
	return &fts3Tokenizer{kind: fts3TokUnicode61, u: u}, nil
}

// spans returns text's tokens with their byte ranges -- what offsets(),
// snippet() and the query parser all walk. A nil receiver is the simple
// tokenizer (fts3_index.go).
func (tk *fts3Tokenizer) spans(text string) []fts3TokenSpan {
	return tk.spansOf(fts3TokenizerInput(text))
}

// spansOf is spans over text EXACTLY AS GIVEN -- no NUL truncation. Only
// fts3tokenize reaches it directly: that module opens the tokenizer with an
// explicit byte count (fts3tokFilterMethod's sqlite3_value_bytes) rather than
// the -1 every indexing call site passes, so a NUL there does not end the
// input. See fts3TokenizerInput.
func (tk *fts3Tokenizer) spansOf(text string) []fts3TokenSpan {
	if tk == nil || tk.kind == fts3TokSimple {
		if tk != nil && tk.delim != nil {
			return fts3TokenizeSpansDelim(text, tk.delim)
		}
		return fts3TokenizeSpans(text)
	}
	if tk.kind == fts3TokPorter {
		return porterSpans(text)
	}
	f5 := tk.u.spans(text)
	out := make([]fts3TokenSpan, len(f5))
	for i, sp := range f5 {
		out[i] = fts3TokenSpan{term: sp.tok, start: sp.start, end: sp.end}
	}
	return out
}

// fts3TokenizerInput truncates text at the first NUL byte, where every
// fts3 tokenizer stops. Only tokens stop there; content values are stored fully.
// and this applies to the query string and to offsets()/snippet() too (the
// spans it returns are all inside the prefix, so their offsets stay exact).
// The same truncation happens for unicode61 and porter -- 'running'||char(0)||
// 'gardens' indexes as the single stem 'run'.
func fts3TokenizerInput(text string) string {
	if i := strings.IndexByte(text, 0); i >= 0 {
		return text[:i]
	}
	return text
}

// tokenize is spans reduced to the terms, whose slice index IS each term's
// position.
func (tk *fts3Tokenizer) tokenize(text string) []string {
	spans := tk.spans(text)
	if len(spans) == 0 {
		return nil
	}
	out := make([]string, len(spans))
	for i, sp := range spans {
		out[i] = sp.term
	}
	return out
}

// fts3SpecTokens splits a tokenizer specification into its DEQUOTED tokens,
// which is sqlite3Fts3NextToken applied repeatedly (see this file's comment).
// It reports false for a token this engine has not pinned -- an unterminated
// quote -- rather than guessing.
func fts3SpecTokens(spec string) ([]string, bool) {
	var out []string
	i := 0
	for i < len(spec) {
		tok, n, ok := fts3NextSpecToken(spec[i:])
		if !ok {
			return nil, false
		}
		if n == 0 {
			break
		}
		out = append(out, fts3DequoteToken(tok))
		i += n
		// sqlite3Fts3InitTokenizer's "z = &z[n+1]": the character that ended
		// the token is consumed with it.
		i++
	}
	return out, true
}

// fts3NextSpecToken is sqlite3Fts3NextToken: skip anything that could not start
// a token, then take a quoted run (respecting doubling), a bracketed run, or a
// maximal run of identifier characters. It returns the token and how far into z
// it ends.
func fts3NextSpecToken(z string) (tok string, end int, ok bool) {
	i := 0
	for i < len(z) {
		c := z[i]
		switch c {
		case '\'', '"', '`':
			j := i + 1
			for {
				if j >= len(z) {
					return "", 0, false
				}
				if z[j] != c {
					j++
					continue
				}
				j++
				if j < len(z) && z[j] == c {
					j++
					continue
				}
				return z[i:j], j, true
			}
		case '[':
			j := i + 1
			for j < len(z) && z[j] != ']' {
				j++
			}
			if j < len(z) {
				j++
			}
			return z[i:j], j, true
		default:
			if fts3IsIdentChar(c) {
				j := i + 1
				for j < len(z) && fts3IsIdentChar(z[j]) {
					j++
				}
				return z[i:j], j, true
			}
			i++
		}
	}
	return "", 0, true
}

// fts3DequoteToken is sqlite3Fts3Dequote: strip one layer of quoting, turning a
// DOUBLED quote inside into a single one.
func fts3DequoteToken(z string) string {
	if z == "" {
		return z
	}
	q := z[0]
	switch q {
	case '\'', '"', '`':
	case '[':
		q = ']'
	default:
		return z
	}
	var b strings.Builder
	for i := 1; i < len(z); {
		if z[i] == q {
			if i+1 < len(z) && z[i+1] == q {
				b.WriteByte(q)
				i += 2
				continue
			}
			break
		}
		b.WriteByte(z[i])
		i++
	}
	return b.String()
}
