// sqlean's fuzzy family (src/fuzzy): edit distances, phonetic codes,
// transliteration and script detection. The distances (except
// fuzzy_editdist), fuzzy_translit and fuzzy_script read their arguments as
// UTF-8 with each invalid byte as U+FFFD; fuzzy_editdist and the phonetic
// codes take ASCII only. Input a function rejects (a blob, a non-ASCII
// string, hamming's different lengths) answers NULL, never an error.
package engine

import (
	"strings"
	"unicode/utf8"
)

func init() {
	dist := func(name string, f func(a, b []byte) (int64, bool)) sqleanScalar {
		return sqleanScalar{name: name, lo: 2, hi: 2, fn: func(args []Value) (Value, error) {
			a, b, ok := fuzzyTexts(args)
			if !ok {
				return sqleanNull, nil
			}
			d, ok := f(a, b)
			if !ok {
				return sqleanNull, nil
			}
			return sqleanInt(d), nil
		}}
	}
	code := func(name string, f func(s []byte) (string, bool)) sqleanScalar {
		return sqleanScalar{name: name, lo: 1, hi: 1, fn: func(args []Value) (Value, error) {
			a, _, ok := fuzzyTexts(args)
			if !ok {
				return sqleanNull, nil
			}
			c, ok := f(a)
			if !ok {
				return sqleanNull, nil
			}
			return sqleanText(c), nil
		}}
	}
	damlev := dist("fuzzy_damlev", func(a, b []byte) (int64, bool) {
		return fuzzyEditDistance(a, b, fuzzyDamlev), true
	})
	editdist := dist("fuzzy_editdist", fuzzyEditdist)
	hamming := dist("fuzzy_hamming", fuzzyHamming)
	jarowin := sqleanScalar{name: "fuzzy_jarowin", lo: 2, hi: 2, fn: func(args []Value) (Value, error) {
		a, b, ok := fuzzyTexts(args)
		if !ok {
			return sqleanNull, nil
		}
		return Value{Typ: Float, F: fuzzyJarowin(a, b)}, nil
	}}
	leven := dist("fuzzy_leven", func(a, b []byte) (int64, bool) {
		return fuzzyEditDistance(a, b, fuzzyLeven), true
	})
	osadist := dist("fuzzy_osadist", func(a, b []byte) (int64, bool) {
		return fuzzyEditDistance(a, b, fuzzyOsa), true
	})
	caver := code("fuzzy_caver", fuzzyCaver)
	phonetic := code("fuzzy_phonetic", fuzzyPhonetic)
	soundex := code("fuzzy_soundex", fuzzySoundex)
	rsoundex := code("fuzzy_rsoundex", fuzzyRsoundex)
	translit := code("fuzzy_translit", func(s []byte) (string, bool) { return fuzzyTranslitText(s), true })
	script := sqleanScalar{name: "fuzzy_script", lo: 1, hi: 1, fn: func(args []Value) (Value, error) {
		a, _, ok := fuzzyTexts(args)
		if !ok {
			return sqleanNull, nil
		}
		return sqleanInt(fuzzyScript(a)), nil
	}}
	sqleanFamilies["fuzzy"] = &sqleanFamily{scalars: []sqleanScalar{
		damlev, editdist, hamming, jarowin, leven, osadist,
		caver, phonetic, soundex, rsoundex, translit, script,
		sqleanAlias("dlevenshtein", damlev),
		sqleanAlias("edit_distance", editdist),
		sqleanAlias("hamming", hamming),
		sqleanAlias("jaro_winkler", jarowin),
		sqleanAlias("levenshtein", leven),
		sqleanAlias("osa_distance", osadist),
		sqleanAlias("caverphone", caver),
		sqleanAlias("phonetic_hash", phonetic),
		sqleanAlias("soundex", soundex),
		sqleanAlias("rsoundex", rsoundex),
		sqleanAlias("translit", translit),
		sqleanAlias("script_code", script),
	}}
}

// fuzzyTexts is arg_texts: the arguments as text bytes (a second slot is nil
// for a one-argument function). ok is false when any is NULL or a blob.
func fuzzyTexts(args []Value) (a, b []byte, ok bool) {
	for _, v := range args {
		if v.Typ == Null || v.Typ == Blob {
			return nil, nil, false
		}
	}
	a = sqleanBytes(args[0])
	if len(args) > 1 {
		b = sqleanBytes(args[1])
	}
	return a, b, true
}

func fuzzyRunes(s []byte) []rune {
	r := make([]rune, 0, len(s))
	for len(s) > 0 {
		c, n := utf8.DecodeRune(s)
		r = append(r, c)
		s = s[n:]
	}
	return r
}

func fuzzyRunesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fuzzyEditDistance is edit_distance: the cases every distance shares, then
// f for two different non-empty strings.
func fuzzyEditDistance(a, b []byte, f func(a, b []rune) int) int64 {
	ra, rb := fuzzyRunes(a), fuzzyRunes(b)
	switch {
	case len(ra) == 0:
		return int64(len(rb))
	case len(rb) == 0:
		return int64(len(ra))
	case fuzzyRunesEqual(ra, rb):
		return 0
	}
	return int64(f(ra, rb))
}

func fuzzyDamlev(a, b []rune) int {
	na, nb := len(a), len(b)
	last := map[rune]int{}
	cols := nb + 2
	m := make([]int, (na+2)*cols)
	maxDist := na + nb
	m[0] = maxDist
	for i := 0; i <= na; i++ {
		m[(i+1)*cols] = maxDist
		m[(i+1)*cols+1] = i
	}
	for j := 0; j <= nb; j++ {
		m[j+1] = maxDist
		m[cols+j+1] = j
	}
	for i := 1; i <= na; i++ {
		db := 0
		for j := 1; j <= nb; j++ {
			i1 := last[b[j-1]]
			j1 := db
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
				db = j
			}
			trans := m[i1*cols+j1] + (i - i1 - 1) + 1 + (j - j1 - 1)
			m[(i+1)*cols+j+1] = min(m[(i+1)*cols+j]+1, m[i*cols+j+1]+1, m[i*cols+j]+cost, trans)
		}
		last[a[i-1]] = i
	}
	return m[(na+1)*cols+nb+1]
}

func fuzzyLeven(a, b []rune) int {
	col := make([]int, len(a)+1)
	for y := range col {
		col[y] = y
	}
	for x := 1; x <= len(b); x++ {
		col[0] = x
		lastkey := x - 1
		for y := 1; y <= len(a); y++ {
			old := col[y]
			cost := 0
			if a[y-1] != b[x-1] {
				cost = 1
			}
			col[y] = min(col[y]+1, col[y-1]+1, lastkey+cost)
			lastkey = old
		}
	}
	return col[len(a)]
}

func fuzzyOsa(a, b []rune) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	na, nb := len(a), len(b)
	cols := nb + 1
	m := make([]int, 3*cols)
	at := func(i, j int) *int { return &m[(i%3)*cols+j] }
	for j := 0; j <= nb; j++ {
		*at(0, j) = j
	}
	for i := 1; i <= na; i++ {
		*at(i, 0) = i
		for j := 1; j <= nb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			v := min(*at(i-1, j)+1, *at(i, j-1)+1, *at(i-1, j-1)+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				v = min(v, *at(i-2, j-2)+1)
			}
			*at(i, j) = v
		}
	}
	return *at(na, nb)
}

func fuzzyHamming(a, b []byte) (int64, bool) {
	var count int64
	for len(a) > 0 && len(b) > 0 {
		ca, sa := utf8.DecodeRune(a)
		cb, sb := utf8.DecodeRune(b)
		if ca != cb {
			count++
		}
		a, b = a[sa:], b[sb:]
	}
	if len(a) > 0 || len(b) > 0 {
		return 0, false
	}
	return count, true
}

func fuzzyJarowin(a, b []byte) float64 {
	ra, rb := fuzzyRunes(a), fuzzyRunes(b)
	if fuzzyRunesEqual(ra, rb) {
		return 1
	}
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	sim := fuzzyJaro(ra, rb)
	prefix := 0
	for prefix < min(len(ra), len(rb), 4) && ra[prefix] == rb[prefix] {
		prefix++
	}
	return sim + 0.1*float64(prefix)*(1.0-sim)
}

func fuzzyJaro(a, b []rune) float64 {
	na, nb := len(a), len(b)
	amatch := make([]bool, na)
	bmatch := make([]bool, nb)
	maxDist := 0
	if longest := max(na, nb); longest/2 > 0 {
		maxDist = longest/2 - 1
	}
	match := 0
	for i := 0; i < na; i++ {
		for j := max(i-maxDist, 0); j < min(nb, i+maxDist+1); j++ {
			if a[i] == b[j] && !bmatch[j] {
				amatch[i], bmatch[j] = true, true
				match++
				break
			}
		}
	}
	if match == 0 {
		return 0
	}
	unaligned, p := 0, 0
	for i := 0; i < na; i++ {
		if amatch[i] {
			for !bmatch[p] {
				p++
			}
			if a[i] != b[p] {
				unaligned++
			}
			p++
		}
	}
	m := float64(match)
	t := float64(unaligned) / 2
	return (m/float64(na) + m/float64(nb) + (m-t)/m) / 3.0
}

// Character classes of the spellfix functions; fuzzyInitClass is the class
// of a word's first character, fuzzyMidClass of the rest.
const (
	fzSilent = iota
	fzVowel
	fzB
	fzC
	fzD
	fzH
	fzL
	fzR
	fzM
	fzY
	fzDigit
	fzSpace
	fzOther
)

const fuzzyClassName = ".ABCDHLRMY9 ?"

func fuzzyIsASCII(s []byte) bool {
	for _, c := range s {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

func fuzzyIsAlpha(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func fuzzyLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func fuzzyUpper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}

func fuzzyCharClass(prev, c byte) byte {
	if prev == 0 {
		return fuzzyInitClass[c&0x7f]
	}
	return fuzzyMidClass[c&0x7f]
}

func fuzzyAt(s []byte, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func fuzzyInsertOrDeleteCost(prev, c, next byte) int {
	classC := fuzzyCharClass(prev, c)
	if classC == fzSilent {
		return 1
	}
	if prev == c {
		return 10
	}
	if classC == fzVowel && (prev == 'r' || next == 'r') {
		return 20
	}
	if classC == fuzzyCharClass(prev, prev) {
		if classC == fzVowel {
			return 15
		}
		return 50
	}
	return 100
}

func fuzzySubstituteCost(prev, from, to byte) int {
	if from == to {
		return 0
	}
	if from == to^0x20 && fuzzyIsAlpha(to) {
		return 0
	}
	classFrom, classTo := fuzzyCharClass(prev, from), fuzzyCharClass(prev, to)
	if classFrom == classTo {
		return 40
	}
	if classFrom >= fzB && classFrom <= fzY && classTo >= fzB && classTo <= fzY {
		return 75
	}
	return 100
}

// fuzzyEditdist is fuzzy_editdist, spellfix's editdist1 with its trailing
// '*' prefix match.
func fuzzyEditdist(a, b []byte) (int64, bool) {
	if !fuzzyIsASCII(a) || !fuzzyIsASCII(b) {
		return 0, false
	}
	var dc byte
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		dc = a[0]
		a, b = a[1:], b[1:]
	}
	na, nb := len(a), len(b)
	if na == 0 && nb == 0 {
		return 0, true
	}
	var res int64
	if na == 0 {
		prev := dc
		for i, cb := range b {
			res += int64(fuzzyInsertOrDeleteCost(prev, cb, fuzzyAt(b, i+1)) / 4)
			prev = cb
		}
		return res, true
	}
	if nb == 0 {
		prev := dc
		for i, ca := range a {
			res += int64(fuzzyInsertOrDeleteCost(prev, ca, fuzzyAt(a, i+1)))
			prev = ca
		}
		return res, true
	}
	if na == 1 && a[0] == '*' {
		return 0, true
	}

	m := make([]int64, nb+1)
	cx := make([]byte, nb+1)
	cx[0] = dc
	bprev := dc
	for xb := 1; xb <= nb; xb++ {
		cb := b[xb-1]
		cx[xb] = cb
		m[xb] = m[xb-1] + int64(fuzzyInsertOrDeleteCost(bprev, cb, fuzzyAt(b, xb)))
		bprev = cb
	}
	var ca byte
	aprev := dc
	for xa := 1; xa <= na; xa++ {
		lastA := xa == na
		ca = a[xa-1]
		anext := fuzzyAt(a, xa)
		if ca == '*' && lastA {
			break
		}
		d := m[0]
		m[0] = d + int64(fuzzyInsertOrDeleteCost(aprev, ca, anext))
		for xb := 1; xb <= nb; xb++ {
			cb := b[xb-1]
			bnext := fuzzyAt(b, xb)
			ins := fuzzyInsertOrDeleteCost(cx[xb-1], cb, bnext)
			if lastA {
				ins /= 4
			}
			del := fuzzyInsertOrDeleteCost(cx[xb], ca, bnext)
			sub := fuzzySubstituteCost(cx[xb-1], ca, cb)
			total := int64(ins) + m[xb-1]
			ncx := cb
			if int64(del)+m[xb] < total {
				total = int64(del) + m[xb]
				ncx = ca
			}
			if int64(sub)+d < total {
				total = int64(sub) + d
			}
			d = m[xb]
			m[xb] = total
			cx[xb] = ncx
		}
		aprev = ca
	}
	if ca == '*' {
		res = m[1]
		for xb := 1; xb <= nb; xb++ {
			res = min(res, m[xb])
		}
		return res, true
	}
	return m[nb], true
}

// fuzzyPhonetic is fuzzy_phonetic, spellfix's phonetic hash.
func fuzzyPhonetic(s []byte) (string, bool) {
	if !fuzzyIsASCII(s) {
		return "", false
	}
	if len(s) > 2 && (fuzzyLower(s[0]) == 'g' || fuzzyLower(s[0]) == 'k') && fuzzyLower(s[1]) == 'n' {
		s = s[1:]
	}
	const noClass = 0x77
	var buf []byte
	prev, prevx := byte(noClass), byte(noClass)
	classes := &fuzzyInitClass
	for i := range s {
		c := fuzzyLower(s[i])
		var next, next2 byte
		if i+1 < len(s) {
			next = fuzzyLower(s[i+1])
		}
		if i+2 < len(s) {
			next2 = fuzzyLower(s[i+2])
		}
		if c == 'w' && next == 'r' || c == 'd' && (next == 'j' || next == 'g') || c == 't' && next == 'c' && next2 == 'h' {
			continue
		}
		cls := classes[c]
		if cls == fzSpace {
			continue
		}
		if cls == fzOther && prev != fzDigit {
			continue
		}
		classes = &fuzzyMidClass
		if cls == fzVowel && (prevx == fzR || prevx == fzL) {
			continue
		}
		if (cls == fzR || cls == fzL) && prevx == fzVowel && len(buf) > 0 {
			buf = buf[:len(buf)-1]
		}
		prev = cls
		if cls == fzSilent {
			continue
		}
		prevx = cls
		code := fuzzyClassName[cls]
		if len(buf) == 0 || code != buf[len(buf)-1] {
			buf = append(buf, code)
		}
	}
	return string(buf), true
}

func fuzzySoundexDigit(c byte) byte {
	switch fuzzyLower(c) {
	case 'b', 'f', 'p', 'v':
		return '1'
	case 'c', 'g', 'j', 'k', 'q', 's', 'x', 'z':
		return '2'
	case 'd', 't':
		return '3'
	case 'l':
		return '4'
	case 'm', 'n':
		return '5'
	case 'r':
		return '6'
	}
	return '0'
}

func fuzzyRsoundexDigit(c byte) byte {
	switch fuzzyLower(c) {
	case 'b', 'p':
		return '1'
	case 'f', 'v':
		return '2'
	case 'c', 'k', 's':
		return '3'
	case 'g', 'j':
		return '4'
	case 'q', 'x', 'z':
		return '5'
	case 'd', 't':
		return '6'
	case 'l':
		return '7'
	case 'm', 'n':
		return '8'
	case 'r':
		return '9'
	}
	return '0'
}

// fuzzyFirstLetter is skip_non_letters: the index of the first letter, or
// len(s) if there is none.
func fuzzyFirstLetter(s []byte) int {
	i := 0
	for i < len(s) && !fuzzyIsAlpha(s[i]) {
		i++
	}
	return i
}

func fuzzySoundex(s []byte) (string, bool) {
	if !fuzzyIsASCII(s) {
		return "", false
	}
	start := fuzzyFirstLetter(s)
	if start == len(s) {
		return "", true
	}
	buf := []byte{fuzzyUpper(s[start])}
	for i := start + 1; i < len(s) && len(buf) < 4; i++ {
		code := fuzzySoundexDigit(s[i])
		if code == '0' || code == fuzzySoundexDigit(s[i-1]) {
			continue
		}
		sep := fuzzyLower(s[i-1])
		if i > start+1 && code == fuzzySoundexDigit(s[i-2]) && (sep == 'h' || sep == 'w') {
			continue
		}
		buf = append(buf, code)
	}
	for len(buf) < 4 {
		buf = append(buf, '0')
	}
	return string(buf), true
}

func fuzzyRsoundex(s []byte) (string, bool) {
	if !fuzzyIsASCII(s) {
		return "", false
	}
	start := fuzzyFirstLetter(s)
	if start == len(s) {
		return "", true
	}
	buf := []byte{fuzzyUpper(s[start])}
	var prev byte
	for _, c := range s[start:] {
		if code := fuzzyRsoundexDigit(c); code != prev {
			buf = append(buf, code)
			prev = code
		}
	}
	return string(buf), true
}

// fuzzyReplaceSeq is replace_seq: every run of old becomes one new.
func fuzzyReplaceSeq(w string, old, new byte) string {
	var b []byte
	for i := 0; i < len(w); {
		if w[i] != old {
			b = append(b, w[i])
			i++
			continue
		}
		for i < len(w) && w[i] == old {
			i++
		}
		b = append(b, new)
	}
	return string(b)
}

func fuzzyReplaceStart(w, old, new string) string {
	if strings.HasPrefix(w, old) {
		return new + w[len(old):]
	}
	return w
}

func fuzzyReplaceEnd(w, old, new string) string {
	if strings.HasSuffix(w, old) {
		return w[:len(w)-len(old)] + new
	}
	return w
}

// fuzzyCaver is Caverphone 2.0 with sqlean's rule order.
func fuzzyCaver(s []byte) (string, bool) {
	if !fuzzyIsASCII(s) {
		return "", false
	}
	var l []byte
	for _, c := range s {
		if c = fuzzyLower(c); c >= 'a' && c <= 'z' {
			l = append(l, c)
		}
	}
	if len(l) == 0 {
		return "", true
	}
	w := string(l)
	rep := func(old, new string) { w = strings.ReplaceAll(w, old, new) }
	start := func(old, new string) { w = fuzzyReplaceStart(w, old, new) }
	end := func(old, new string) { w = fuzzyReplaceEnd(w, old, new) }
	seq := func(old, new byte) { w = fuzzyReplaceSeq(w, old, new) }

	end("e", "")
	start("cough", "cou2f")
	start("rough", "rou2f")
	start("tough", "tou2f")
	start("enough", "enou2f")
	start("trough", "trou2f")
	start("gn", "2n")
	end("mb", "m2")
	rep("cq", "2q")
	rep("ci", "si")
	rep("ce", "se")
	rep("cy", "sy")
	rep("tch", "2ch")
	rep("c", "k")
	rep("q", "k")
	rep("x", "k")
	rep("v", "f")
	rep("dg", "2g")
	rep("tio", "sio")
	rep("tia", "sia")
	rep("d", "t")
	rep("ph", "fh")
	rep("b", "p")
	rep("sh", "s2")
	rep("z", "s")
	start("a", "A")
	start("e", "A")
	start("i", "A")
	start("o", "A")
	start("u", "A")
	rep("a", "3")
	rep("e", "3")
	rep("i", "3")
	rep("o", "3")
	rep("u", "3")
	rep("j", "y")
	start("y3", "Y3")
	start("y", "A")
	rep("y", "3")
	rep("3gh3", "3kh3")
	rep("gh", "22")
	rep("g", "k")
	seq('s', 'S')
	seq('t', 'T')
	seq('p', 'P')
	seq('k', 'K')
	seq('f', 'F')
	seq('m', 'M')
	seq('n', 'N')
	rep("w3", "W3")
	rep("wh3", "Wh3")
	end("w", "3")
	rep("w", "2")
	start("h", "A")
	rep("h", "2")
	rep("r3", "R3")
	end("r", "3")
	rep("r", "2")
	rep("l3", "L3")
	end("l", "3")
	rep("l", "2")
	rep("2", "")
	end("3", "A")
	rep("3", "")
	if len(w) > 10 {
		w = w[:10]
	}
	return w + strings.Repeat("1", 10-len(w)), true
}

func fuzzyTranslitText(s []byte) string {
	var b []byte
	for len(s) > 0 {
		c, n := utf8.DecodeRune(s)
		s = s[n:]
		if c < 0x80 {
			b = append(b, byte(c))
		} else if t, ok := fuzzyTranslit[c]; ok {
			b = append(b, t...)
		} else {
			b = append(b, '?')
		}
	}
	return string(b)
}

// Script codes (ISO 15924 numeric), as fuzzy_script returns them.
const (
	fuzzyScriptHebrew   = 125
	fuzzyScriptArabic   = 160
	fuzzyScriptGreek    = 200
	fuzzyScriptLatin    = 215
	fuzzyScriptCyrillic = 220
	fuzzyScriptMixed    = 998
	fuzzyScriptUnknown  = 999
)

func fuzzyScript(s []byte) int64 {
	const (
		latin = 1 << iota
		cyrillic
		greek
		hebrew
		arabic
	)
	mask := 0
	seenDigit := false
	for len(s) > 0 {
		c, n := utf8.DecodeRune(s)
		s = s[n:]
		switch {
		case c < 0x80 && fuzzyIsAlpha(byte(c)) || c >= 0xC0 && c <= 0x2AF && c != 0xD7 && c != 0xF7:
			mask |= latin
		case c >= '0' && c <= '9':
			seenDigit = true
		case c >= 0x400 && c <= 0x4FF:
			mask |= cyrillic
		case c >= 0x386 && c <= 0x3CE:
			mask |= greek
		case c >= 0x590 && c <= 0x5FF:
			mask |= hebrew
		case c >= 0x600 && c <= 0x6FF:
			mask |= arabic
		}
	}
	if mask == 0 && seenDigit {
		mask = latin
	}
	switch mask {
	case 0:
		return fuzzyScriptUnknown
	case latin:
		return fuzzyScriptLatin
	case cyrillic:
		return fuzzyScriptCyrillic
	case greek:
		return fuzzyScriptGreek
	case hebrew:
		return fuzzyScriptHebrew
	case arabic:
		return fuzzyScriptArabic
	}
	return fuzzyScriptMixed
}

var fuzzyMidClass = [128]byte{
	fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzSpace, fzOther, fzOther, fzSpace, fzSpace, fzOther, fzOther,
	fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzSpace, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzSilent,
	fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzDigit, fzDigit, fzDigit, fzDigit, fzDigit, fzDigit, fzDigit, fzDigit,
	fzDigit, fzDigit, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzVowel, fzB, fzC, fzD, fzVowel, fzB, fzC,
	fzSilent, fzVowel, fzC, fzC, fzL, fzM, fzM, fzVowel,
	fzB, fzC, fzR, fzC, fzD, fzVowel, fzB, fzB,
	fzC, fzVowel, fzC, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzVowel, fzB, fzC, fzD, fzVowel, fzB, fzC,
	fzSilent, fzVowel, fzC, fzC, fzL, fzM, fzM, fzVowel,
	fzB, fzC, fzR, fzC, fzD, fzVowel, fzB, fzB,
	fzC, fzVowel, fzC, fzOther, fzOther, fzOther, fzOther, fzOther,
}

var fuzzyInitClass = [128]byte{
	fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzSpace, fzOther, fzOther, fzSpace, fzSpace, fzOther, fzOther,
	fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzSpace, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzDigit, fzDigit, fzDigit, fzDigit, fzDigit, fzDigit, fzDigit, fzDigit,
	fzDigit, fzDigit, fzOther, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzVowel, fzB, fzC, fzD, fzVowel, fzB, fzC,
	fzSilent, fzVowel, fzC, fzC, fzL, fzM, fzM, fzVowel,
	fzB, fzC, fzR, fzC, fzD, fzVowel, fzB, fzB,
	fzC, fzY, fzC, fzOther, fzOther, fzOther, fzOther, fzOther,
	fzOther, fzVowel, fzB, fzC, fzD, fzVowel, fzB, fzC,
	fzSilent, fzVowel, fzC, fzC, fzL, fzM, fzM, fzVowel,
	fzB, fzC, fzR, fzC, fzD, fzVowel, fzB, fzB,
	fzC, fzY, fzC, fzOther, fzOther, fzOther, fzOther, fzOther,
}
