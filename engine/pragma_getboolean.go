package engine

// pragmaGetBoolean parses a boolean PRAGMA value: digits, keywords (on/off/
// yes/no/true/false), or the default. Non-keywords and digits outside u8 range
// defer to the default.
func pragmaGetBoolean(text string, dflt bool) bool {
	var d int64
	if dflt {
		d = 1
	}
	return pragmaSafetyLevelDflt(text, true, d) != 0
}
