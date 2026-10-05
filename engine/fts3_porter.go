// This file implements fts3/fts4's "porter" tokenizer, SQLite's variant of
// the Porter stemmer. Key details: token boundaries differ from simple
// (underscore is included); short words and non-ASCII go through copy_stemmer;
// and the stemmer works on reversed words with reversed patterns.
package engine

// porterCType classifies 'a'..'z': 0=vowel, 1=consonant, 2='y' (depends on next).
var porterCType = [26]int8{
	0, 1, 1, 1, 0, 1, 1, 1, 0, 1, 1, 1, 1, 1, 0, 1, 1, 1, 1, 1, 0,
	1, 1, 1, 2, 1,
}

// porterIdChar is the token-character table for 0x30..0x7f; '_' is included.
var porterIdChar = [0x50]bool{
	/*      x0     x1     x2     x3     x4     x5     x6     x7     x8     x9     xA     xB     xC     xD     xE     xF */
	/*3x*/ true, true, true, true, true, true, true, true, true, true, false, false, false, false, false, false,
	/*4x*/ false, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true,
	/*5x*/ true, true, true, true, true, true, true, true, true, true, true, false, false, false, false, true,
	/*6x*/ false, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true,
	/*7x*/ true, true, true, true, true, true, true, true, true, true, true, false, false, false, false, false,
}

// porterIsDelim tests whether c is a delimiter. High-bit bytes are not delimiters.
func porterIsDelim(c byte) bool {
	if c >= 0x80 {
		return false
	}
	return c < 0x30 || !porterIdChar[c-0x30]
}

// porterSpans is porterNext's loop: maximal runs of non-delimiters, each
// STEMMED, with the RAW byte range reported (offsets() and snippet() report
// where the original word was, not where the stem would be).
func porterSpans(text string) []fts3TokenSpan {
	var out []fts3TokenSpan
	i := 0
	for i < len(text) {
		for i < len(text) && porterIsDelim(text[i]) {
			i++
		}
		start := i
		for i < len(text) && !porterIsDelim(text[i]) {
			i++
		}
		if i > start {
			out = append(out, fts3TokenSpan{term: porterStem(text[start:i]), start: start, end: i})
		}
	}
	return out
}

// porterCopyStem is copy_stemmer: the fallback for a word the stemmer will not
// touch. See this file's comment for the truncation rule.
func porterCopyStem(in string) string {
	b := make([]byte, len(in))
	hasDigit := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c - 'A' + 'a'
		default:
			if c >= '0' && c <= '9' {
				hasDigit = true
			}
			b[i] = c
		}
	}
	mx := 10
	if hasDigit {
		mx = 3
	}
	if len(b) > mx*2 {
		copy(b[mx:], b[len(b)-mx:])
		b = b[:mx*2]
	}
	return string(b)
}

// porterStem is porter_stemmer. The word is reversed into a fixed buffer with
// five NUL bytes after it (so a rule may read up to four bytes past the end and
// see zero, exactly as the C does) and headroom in front of it for the rules
// that write a longer ending than they consumed.
func porterStem(in string) string {
	// sizeof(zReverse) is 28 and the guard is `nIn<3 || nIn>=sizeof-7`.
	const bufLen = 32
	if len(in) < 3 || len(in) >= 21 {
		return porterCopyStem(in)
	}
	var buf [bufLen]byte
	j := 22 // sizeof(zReverse)-6
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c >= 'A' && c <= 'Z':
			buf[j] = c + 'a' - 'A'
		case c >= 'a' && c <= 'z':
			buf[j] = c
		default:
			// Any character outside [A-Za-z] falls back to the copy stemmer.
			return porterCopyStem(in)
		}
		j--
	}
	b := buf[:]
	z := j + 1

	// Step 1a
	if b[z] == 's' {
		if !porterRule(b, &z, "sess", "ss", nil) &&
			!porterRule(b, &z, "sei", "i", nil) &&
			!porterRule(b, &z, "ss", "ss", nil) {
			z++
		}
	}

	// Step 1b
	z2 := z
	if porterRule(b, &z, "dee", "ee", porterMGt0) {
		// Do nothing. The work was all in the test.
	} else if (porterRule(b, &z, "gni", "", porterHasVowel) ||
		porterRule(b, &z, "de", "", porterHasVowel)) && z != z2 {
		if porterRule(b, &z, "ta", "ate", nil) ||
			porterRule(b, &z, "lb", "ble", nil) ||
			porterRule(b, &z, "zi", "ize", nil) {
			// Do nothing.
		} else if porterDoubleConsonant(b, z) && b[z] != 'l' && b[z] != 's' && b[z] != 'z' {
			z++
		} else if porterMEq1(b, z) && porterStarOh(b, z) {
			z--
			b[z] = 'e'
		}
	}

	// Step 1c
	if b[z] == 'y' && porterHasVowel(b, z+1) {
		b[z] = 'i'
	}

	// Step 2
	switch b[z+1] {
	case 'a':
		if !porterRule(b, &z, "lanoita", "ate", porterMGt0) {
			porterRule(b, &z, "lanoit", "tion", porterMGt0)
		}
	case 'c':
		if !porterRule(b, &z, "icne", "ence", porterMGt0) {
			porterRule(b, &z, "icna", "ance", porterMGt0)
		}
	case 'e':
		porterRule(b, &z, "rezi", "ize", porterMGt0)
	case 'g':
		porterRule(b, &z, "igol", "log", porterMGt0)
	case 'l':
		if !porterRule(b, &z, "ilb", "ble", porterMGt0) &&
			!porterRule(b, &z, "illa", "al", porterMGt0) &&
			!porterRule(b, &z, "iltne", "ent", porterMGt0) &&
			!porterRule(b, &z, "ile", "e", porterMGt0) {
			porterRule(b, &z, "ilsuo", "ous", porterMGt0)
		}
	case 'o':
		if !porterRule(b, &z, "noitazi", "ize", porterMGt0) &&
			!porterRule(b, &z, "noita", "ate", porterMGt0) {
			porterRule(b, &z, "rota", "ate", porterMGt0)
		}
	case 's':
		if !porterRule(b, &z, "msila", "al", porterMGt0) &&
			!porterRule(b, &z, "ssenevi", "ive", porterMGt0) &&
			!porterRule(b, &z, "ssenluf", "ful", porterMGt0) {
			porterRule(b, &z, "ssensuo", "ous", porterMGt0)
		}
	case 't':
		if !porterRule(b, &z, "itila", "al", porterMGt0) &&
			!porterRule(b, &z, "itivi", "ive", porterMGt0) {
			porterRule(b, &z, "itilib", "ble", porterMGt0)
		}
	}

	// Step 3
	switch b[z] {
	case 'e':
		if !porterRule(b, &z, "etaci", "ic", porterMGt0) &&
			!porterRule(b, &z, "evita", "", porterMGt0) {
			porterRule(b, &z, "ezila", "al", porterMGt0)
		}
	case 'i':
		porterRule(b, &z, "itici", "ic", porterMGt0)
	case 'l':
		if !porterRule(b, &z, "laci", "ic", porterMGt0) {
			porterRule(b, &z, "luf", "", porterMGt0)
		}
	case 's':
		porterRule(b, &z, "ssen", "", porterMGt0)
	}

	// Step 4
	switch b[z+1] {
	case 'a':
		if b[z] == 'l' && porterMGt1(b, z+2) {
			z += 2
		}
	case 'c':
		if b[z] == 'e' && b[z+2] == 'n' && (b[z+3] == 'a' || b[z+3] == 'e') && porterMGt1(b, z+4) {
			z += 4
		}
	case 'e':
		if b[z] == 'r' && porterMGt1(b, z+2) {
			z += 2
		}
	case 'i':
		if b[z] == 'c' && porterMGt1(b, z+2) {
			z += 2
		}
	case 'l':
		if b[z] == 'e' && b[z+2] == 'b' && (b[z+3] == 'a' || b[z+3] == 'i') && porterMGt1(b, z+4) {
			z += 4
		}
	case 'n':
		if b[z] == 't' {
			if b[z+2] == 'a' {
				if porterMGt1(b, z+3) {
					z += 3
				}
			} else if b[z+2] == 'e' {
				if !porterRule(b, &z, "tneme", "", porterMGt1) &&
					!porterRule(b, &z, "tnem", "", porterMGt1) {
					porterRule(b, &z, "tne", "", porterMGt1)
				}
			}
		}
	case 'o':
		if b[z] == 'u' {
			if porterMGt1(b, z+2) {
				z += 2
			}
		} else if b[z+3] == 's' || b[z+3] == 't' {
			porterRule(b, &z, "noi", "", porterMGt1)
		}
	case 's':
		if b[z] == 'm' && b[z+2] == 'i' && porterMGt1(b, z+3) {
			z += 3
		}
	case 't':
		if !porterRule(b, &z, "eta", "", porterMGt1) {
			porterRule(b, &z, "iti", "", porterMGt1)
		}
	case 'u':
		if b[z] == 's' && b[z+2] == 'o' && porterMGt1(b, z+3) {
			z += 3
		}
	case 'v', 'z':
		if b[z] == 'e' && b[z+2] == 'i' && porterMGt1(b, z+3) {
			z += 3
		}
	}

	// Step 5a
	if b[z] == 'e' {
		if porterMGt1(b, z+1) {
			z++
		} else if porterMEq1(b, z+1) && !porterStarOh(b, z+1) {
			z++
		}
	}

	// Step 5b
	if porterMGt1(b, z) && b[z] == 'l' && b[z+1] == 'l' {
		z++
	}

	// Flip the stem back into forward order.
	end := z
	for end < len(b) && b[end] != 0 {
		end++
	}
	out := make([]byte, end-z)
	for i := range out {
		out[i] = b[end-1-i]
	}
	return string(out)
}

// porterRule is fts3_porter.c's stem(): if the (reversed) word starts with
// from, replace it with to (written backwards into the bytes before the
// cursor) provided cond holds for what remains. It reports whether FROM
// matched, even when cond then refused the rewrite.
func porterRule(b []byte, z *int, from, to string, cond func([]byte, int) bool) bool {
	i := *z
	k := 0
	for k < len(from) && from[k] == b[i] {
		i++
		k++
	}
	if k < len(from) {
		return false
	}
	if cond != nil && !cond(b, i) {
		return true
	}
	for t := 0; t < len(to); t++ {
		i--
		b[i] = to[t]
	}
	*z = i
	return true
}

func porterIsConsonant(b []byte, z int) bool {
	x := b[z]
	if x == 0 {
		return false
	}
	j := porterCType[x-'a']
	if j < 2 {
		return j == 1
	}
	return b[z+1] == 0 || porterIsVowel(b, z+1)
}

func porterIsVowel(b []byte, z int) bool {
	x := b[z]
	if x == 0 {
		return false
	}
	j := porterCType[x-'a']
	if j < 2 {
		return j == 0
	}
	return porterIsConsonant(b, z+1)
}

// porterMGt0 is m_gt_0: the reversed stem holds a consonant followed by a
// vowel, i.e. m >= 1.
func porterMGt0(b []byte, z int) bool {
	for porterIsVowel(b, z) {
		z++
	}
	if b[z] == 0 {
		return false
	}
	for porterIsConsonant(b, z) {
		z++
	}
	return b[z] != 0
}

func porterMEq1(b []byte, z int) bool {
	for porterIsVowel(b, z) {
		z++
	}
	if b[z] == 0 {
		return false
	}
	for porterIsConsonant(b, z) {
		z++
	}
	if b[z] == 0 {
		return false
	}
	for porterIsVowel(b, z) {
		z++
	}
	if b[z] == 0 {
		return true
	}
	for porterIsConsonant(b, z) {
		z++
	}
	return b[z] == 0
}

func porterMGt1(b []byte, z int) bool {
	for porterIsVowel(b, z) {
		z++
	}
	if b[z] == 0 {
		return false
	}
	for porterIsConsonant(b, z) {
		z++
	}
	if b[z] == 0 {
		return false
	}
	for porterIsVowel(b, z) {
		z++
	}
	if b[z] == 0 {
		return false
	}
	for porterIsConsonant(b, z) {
		z++
	}
	return b[z] != 0
}

func porterHasVowel(b []byte, z int) bool {
	for porterIsConsonant(b, z) {
		z++
	}
	return b[z] != 0
}

func porterDoubleConsonant(b []byte, z int) bool {
	return porterIsConsonant(b, z) && b[z] == b[z+1]
}

// porterStarOh is star_oh: the (reversed) word starts consonant-vowel-consonant
// with the first consonant not in [wxy].
func porterStarOh(b []byte, z int) bool {
	return porterIsConsonant(b, z) &&
		b[z] != 'w' && b[z] != 'x' && b[z] != 'y' &&
		porterIsVowel(b, z+1) &&
		porterIsConsonant(b, z+2)
}
