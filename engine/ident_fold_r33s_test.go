package engine

import "testing"

// r33sUpperToLower is sqlite3UpperToLower's SQLITE_ASCII branch, transcribed
// byte for byte from C SQLite. It specifies identifier folding for equality.
// Transcribed rather than generated so the test states the C's own contents.
var r33sUpperToLower = [256]byte{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17,
	18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35,
	36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53,
	54, 55, 56, 57, 58, 59, 60, 61, 62, 63, 64, 97, 98, 99, 100, 101, 102, 103,
	104, 105, 106, 107, 108, 109, 110, 111, 112, 113, 114, 115, 116, 117, 118, 119, 120, 121,
	122, 91, 92, 93, 94, 95, 96, 97, 98, 99, 100, 101, 102, 103, 104, 105, 106, 107,
	108, 109, 110, 111, 112, 113, 114, 115, 116, 117, 118, 119, 120, 121, 122, 123, 124, 125,
	126, 127, 128, 129, 130, 131, 132, 133, 134, 135, 136, 137, 138, 139, 140, 141, 142, 143,
	144, 145, 146, 147, 148, 149, 150, 151, 152, 153, 154, 155, 156, 157, 158, 159, 160, 161,
	162, 163, 164, 165, 166, 167, 168, 169, 170, 171, 172, 173, 174, 175, 176, 177, 178, 179,
	180, 181, 182, 183, 184, 185, 186, 187, 188, 189, 190, 191, 192, 193, 194, 195, 196, 197,
	198, 199, 200, 201, 202, 203, 204, 205, 206, 207, 208, 209, 210, 211, 212, 213, 214, 215,
	216, 217, 218, 219, 220, 221, 222, 223, 224, 225, 226, 227, 228, 229, 230, 231, 232, 233,
	234, 235, 236, 237, 238, 239, 240, 241, 242, 243, 244, 245, 246, 247, 248, 249, 250, 251,
	252, 253, 254, 255,
}

// TestR33SFoldIdentIsTheCTable verifies r33sFoldIdent and asciiLowerByte match
// the table exactly and that only 'A'-'Z' are affected.
func TestR33SFoldIdentIsTheCTable(t *testing.T) {
	moved := 0
	for c := 0; c < 256; c++ {
		want := r33sUpperToLower[c]
		if got := asciiLowerByte(byte(c)); got != want {
			t.Errorf("asciiLowerByte(%d) = %d, sqlite3UpperToLower[%d] = %d", c, got, c, want)
		}
		if got := r33sFoldIdent(string([]byte{byte(c)})); got != string([]byte{want}) {
			t.Errorf("r33sFoldIdent(%q) = %q, want %q", string([]byte{byte(c)}), got, string([]byte{want}))
		}
		if want != byte(c) {
			moved++
			if c < 'A' || c > 'Z' {
				t.Errorf("sqlite3UpperToLower moves byte %d, which is outside 'A'-'Z'", c)
			}
		}
	}
	if moved != 26 {
		t.Errorf("sqlite3UpperToLower moves %d bytes, want exactly the 26 of 'A'-'Z'", moved)
	}
}

// TestR33SFoldIdentLeavesNonASCIIAlone verifies that non-ASCII identifiers are
// left alone, unlike strings.ToLower, matching C SQLite's behavior.
func TestR33SFoldIdentLeavesNonASCIIAlone(t *testing.T) {
	distinct := [][2]string{
		{"Ä", "ä"},              // A-umlaut / a-umlaut
		{"xÖy", "xöy"},          // O-umlaut, embedded
		{"İ", "i"},              // I with dot above -> "i" (2 bytes -> 1)
		{"K", "k"},              // KELVIN SIGN -> "k" (3 bytes -> 1)
		{"Σ", "σ"},              // Sigma / sigma
		{"ς", "σ"},              // final sigma / sigma (EqualFold-only)
		{"ſ", "s"},              // long s / s (EqualFold-only)
		{"ẞ", "ß"},              // capital sharp s / sharp s
		{"ı", "i"},              // dotless i / i (ToUpper-only)
		{"ﬁ", "fi"},             // fi ligature / "fi"
		{"MIİN", "MIIN"},        // the fold cannot shorten a name
		{"ROWİD", "rowid"},      // ... so this is not the rowid alias
		{"SQLİTE_x", "sqlite_"}, // ... and this is not a reserved name
	}
	for _, p := range distinct {
		if a, b := r33sFoldIdent(p[0]), r33sFoldIdent(p[1]); a == b {
			t.Errorf("r33sFoldIdent(%q) == r33sFoldIdent(%q) == %q; sqlite3StrICmp keeps them distinct", p[0], p[1], a)
		}
		if equalFoldName(p[0], p[1]) {
			t.Errorf("equalFoldName(%q, %q) is true; sqlite3StrICmp keeps them distinct", p[0], p[1])
		}
	}
	same := [][2]string{
		{"A", "a"},
		{"ROWID", "rowid"},
		{"SqLiTe_MaStEr", "sqlite_master"},
		{"ÄB", "Äb"},   // ASCII folds even beside a non-ASCII rune
		{"xKY", "xKy"}, // ... and after one
	}
	for _, p := range same {
		if a, b := r33sFoldIdent(p[0]), r33sFoldIdent(p[1]); a != b {
			t.Errorf("r33sFoldIdent(%q) = %q, r33sFoldIdent(%q) = %q; sqlite3StrICmp calls them equal", p[0], a, p[1], b)
		}
		if !equalFoldName(p[0], p[1]) {
			t.Errorf("equalFoldName(%q, %q) is false; sqlite3StrICmp calls them equal", p[0], p[1])
		}
	}
}
