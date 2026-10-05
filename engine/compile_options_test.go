package engine

import "testing"

// TestPinnedCompileOptionValuesAreIdCharsOnly verifies that pinned compile options
// contain only id-char-safe values so full-text equality comparison matches oracle behavior.
func TestPinnedCompileOptionValuesAreIdCharsOnly(t *testing.T) {
	for name, full := range sqliteCompileOptionsPinned {
		value := ""
		if len(full) > len(name) {
			if full[:len(name)] != name || full[len(name)] != '=' {
				t.Errorf("pinned option %q has text %q, which is neither the bare name nor name=value", name, full)
				continue
			}
			value = full[len(name)+1:]
		} else if full != name {
			t.Errorf("pinned option %q has text %q, which is neither the bare name nor name=value", name, full)
			continue
		}
		for i := 0; i < len(value); i++ {
			if !sqliteIsIdCharASCII(value[i]) {
				t.Errorf("pinned option %q value %q contains %q, which sqlite3IsIdChar rejects: compileOptionUsed's whole-text equality is no longer equivalent to main.c:5220's prefix match for it", name, value, value[i])
			}
		}
	}
}

// sqliteIsIdCharASCII is tokenize.c:167's IdChar for the ASCII build:
// alphanumerics, '_', '$', and every byte with the high bit set.
func sqliteIsIdCharASCII(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c == '_' || c == '$' || c >= 0x80:
		return true
	}
	return false
}

// TestCompileOptionCatalogueBoundsGet pins the bound evalCompileOptionGet
// answers NULL past: sqlite3azCompileOpt[] cannot hold more entries than it
// has distinct names, so nOpt <= len(sqliteCompileOptionCatalogue) on every
// 3.53.3 build and any index at or beyond it is out of range everywhere.
// Re-extracted mechanically from the oracle module's own amalgamation, which
// is where the map's doc comment says it came from:
//
//	awk '/static const char \* const sqlite3azCompileOpt/,/^\} ;/' \
//	  sqlite3-binding.c | grep -oE '"[A-Z0-9_]+(=[^"]*)?"' |
//	  tr -d '"' | sed 's/=.*//' | sort -u | wc -l    -> 242
func TestCompileOptionCatalogueBoundsGet(t *testing.T) {
	if got := len(sqliteCompileOptionCatalogue); got != 242 {
		t.Fatalf("catalogue holds %d names, want 242 -- re-derive evalCompileOptionGet's out-of-range bound against the oracle's amalgamation before changing this", got)
	}
	for _, v := range []Value{{Typ: Int, I: -1}, {Typ: Int, I: 242}, {Typ: Int, I: 9999}} {
		got, err := evalCompileOptionGet(v)
		if err != nil || got.Typ != Null {
			t.Errorf("sqlite_compileoption_get(%d) = (%v, %v), want SQL NULL", v.I, got, err)
		}
	}
	for _, v := range []Value{{Typ: Int, I: 0}, {Typ: Int, I: 241}, {Typ: Null}, {Typ: Int, I: 4294967296}} {
		if _, err := evalCompileOptionGet(v); err == nil {
			t.Errorf("sqlite_compileoption_get(%v) answered where it must decline -- that index is inside the oracle build's own list", v)
		}
	}
}
