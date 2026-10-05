// This file ports SQLite's fts5 "unicode61" tokenizer to pure Go.
// It provides the three core pieces: unicode category classification,
// case folding with diacritic stripping, and diacritic detection.
// Defaults: remove_diacritics = 1, category set "L* N* Co".
// Other tokenize= spellings are in fts5_tokenizers.go.
package engine

// fts5Utf8Trans1 is SQLite's sqlite3Utf8Trans1[] lead-byte table, used by the
// same UTF-8 decode the tokenizer's main loop performs (including its
// overlong/surrogate/noncharacter -> U+FFFD fallback).
var fts5Utf8Trans1 = [64]uint8{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x00, 0x01, 0x02, 0x03, 0x00, 0x01, 0x00, 0x00,
}

// fts5UnicodeCategory returns the fts5 category code (0..31) for a codepoint,
// a byte-for-byte port of sqlite3Fts5UnicodeCategory.
func fts5UnicodeCategory(iCode uint32) int {
	if iCode >= 1<<20 {
		return 0
	}
	iLo := int(fts5UnicodeBlock[iCode>>16])
	iHi := int(fts5UnicodeBlock[1+(iCode>>16)])
	iKey := int(iCode & 0xFFFF)
	iRes := -1
	for iHi > iLo {
		iTest := (iHi + iLo) / 2
		if iKey >= int(fts5UnicodeMap[iTest]) {
			iRes = iTest
			iLo = iTest + 1
		} else {
			iHi = iTest
		}
	}
	if iRes < 0 {
		return 0
	}
	if iKey >= int(fts5UnicodeMap[iRes])+int(fts5UnicodeData[iRes])>>5 {
		return 0
	}
	ret := int(fts5UnicodeData[iRes]) & 0x1F
	if ret != 30 {
		return ret
	}
	if (iKey-int(fts5UnicodeMap[iRes]))&0x01 != 0 {
		return 5
	}
	return 9
}

// fts5IsAlnum reports whether a codepoint is a token character under the
// default "L* N* Co" (plus implicit category 0) configuration -- i.e.
// fts5UnicodeIsAlnum with an empty exception set (no tokenchars=/separators=).
func fts5IsAlnum(c int32) bool {
	switch fts5UnicodeCategory(uint32(c)) {
	case 0, 5, 6, 7, 8, 9, 13, 14, 15, 30, 31:
		return true
	}
	return false
}

// fts5IsDiacritic ports sqlite3Fts5UnicodeIsdiacritic.
func fts5IsDiacritic(c int32) bool {
	if c < 768 || c > 817 {
		return false
	}
	if c < 768+32 {
		return uint32(0x08029FDF)&(uint32(1)<<(uint(c)-768)) != 0
	}
	return uint32(0x000361F8)&(uint32(1)<<(uint(c)-768-32)) != 0
}

// fts5RemoveDiacritic ports fts5_remove_diacritic. bComplex is true only for
// remove_diacritics=2, which this engine never selects (default is 1).
func fts5RemoveDiacritic(c int32, bComplex bool) int32 {
	key := (uint32(c) << 3) | 0x00000007
	iRes := 0
	iHi := 125
	iLo := 0
	for iHi >= iLo {
		iTest := (iHi + iLo) / 2
		if key >= uint32(fts5ADia[iTest]) {
			iRes = iTest
			iLo = iTest + 1
		} else {
			iHi = iTest - 1
		}
	}
	if !bComplex && (fts5AChar[iRes]&0x80) != 0 {
		return c
	}
	if c > (int32(fts5ADia[iRes])>>3)+(int32(fts5ADia[iRes])&0x07) {
		return c
	}
	return int32(fts5AChar[iRes]) & 0x7F
}

// fts5UnicodeFold ports sqlite3Fts5UnicodeFold: case-fold c to lowercase and,
// when eRemoveDiacritic is nonzero, strip diacritics.
func fts5UnicodeFold(c int32, eRemoveDiacritic int32) int32 {
	ret := c
	switch {
	case c < 128:
		if c >= 'A' && c <= 'Z' {
			ret = c + ('a' - 'A')
		}
	case c < 65536:
		iHi := 162 // 652/4 - 1
		iLo := 0
		iRes := -1
		for iHi >= iLo {
			iTest := (iHi + iLo) / 2
			cmp := c - int32(fts5AEntry[iTest].iCode)
			if cmp >= 0 {
				iRes = iTest
				iLo = iTest + 1
			} else {
				iHi = iTest - 1
			}
		}
		if iRes >= 0 {
			e := fts5AEntry[iRes]
			if c < int32(e.iCode)+int32(e.nRange) && 0 == (0x01&int32(e.flags)&(int32(e.iCode)^c)) {
				ret = (c + int32(fts5AiOff[e.flags>>1])) & 0x0000FFFF
			}
		}
		if eRemoveDiacritic != 0 {
			ret = fts5RemoveDiacritic(ret, eRemoveDiacritic == 2)
		}
	case c >= 66560 && c < 66600:
		ret = c + 40
	}
	return ret
}

// fts5DefaultRemoveDiacritic is unicode61's default (FTS5_REMOVE_DIACRITICS_SIMPLE).
const fts5DefaultRemoveDiacritic = 1

// fts5NextRune decodes one codepoint from b at byte offset i, exactly as the
// tokenizer's main loop does (sqlite3Utf8Trans1 + the U+FFFD fallback for an
// overlong, surrogate, or noncharacter sequence). Returns the codepoint and
// the number of bytes consumed (always >= 1).
func fts5NextRune(b []byte, i int) (uint32, int) {
	c := uint32(b[i])
	if c < 0x80 {
		return c, 1
	}
	if c < 0xc0 {
		// A stray continuation byte: the C code leaves iCode == the byte value
		// (>= 0x80) and advances one byte.
		return c, 1
	}
	code := uint32(fts5Utf8Trans1[c-0xc0])
	j := i + 1
	for j < len(b) && b[j]&0xc0 == 0x80 {
		code = code<<6 + uint32(b[j]&0x3f)
		j++
	}
	if code < 0x80 || code&0xFFFFF800 == 0xD800 || code&0xFFFFFFFE == 0xFFFE {
		code = 0xFFFD
	}
	return code, j - i
}

// The tokenizer MAIN LOOP itself now lives in fts5_tokenizers.go, as
// (*fts5Tok).spans: a nil *fts5Tok is exactly the default configuration this
// file describes, and the other tokenize= spellings (ascii, trigram, and
// unicode61 with arguments) are the same loop with a different token-character
// test and fold.
