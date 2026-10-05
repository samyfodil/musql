package compat

import (
	"fmt"
	"testing"
)

// Boolean-valued pragmas accept all value spellings that SQLite accepts,
// including numeric prefixes and various word forms.
func TestPragmaBooleanValueSpellings(t *testing.T) {
	// One flag pragma per behaviour class this engine implements differently
	// underneath, so a regression in any of them shows here.
	for _, name := range []string{
		"foreign_keys", "recursive_triggers", "reverse_unordered_selects",
		"automatic_index", "count_changes", "legacy_alter_table",
		"ignore_check_constraints", "full_column_names", "short_column_names",
		"cell_size_check", "checkpoint_fullfsync", "query_only",
		"trusted_schema", "writable_schema", "case_sensitive_like",
	} {
		name := name
		t.Run(name, func(t *testing.T) {
			var stmts []string
			for _, v := range []string{
				"1", "0", "on", "off", "yes", "no", "true", "false",
				"2", "-1", "0x10", "256", "bogus", "'on'", "''", "2.7",
				"full", "extra", "1e3", "007",
			} {
				stmts = append(stmts, fmt.Sprintf("PRAGMA %s=%s", name, v))
				stmts = append(stmts, "PRAGMA "+name)
			}
			differ(t, "pragmabool/"+name, stmts)
		})
	}
}

// Header-scalar pragmas use sqlite3Atoi parsing: leading integer prefix,
// hex notation, and 32-bit limit are all supported.
func TestPragmaHeaderScalarValueSpellings(t *testing.T) {
	for _, name := range []string{"user_version", "application_id"} {
		name := name
		t.Run(name, func(t *testing.T) {
			var stmts []string
			for _, v := range []string{
				"7", "-7", "0", "2.7", "1e3", "0x10", "0xffffffff",
				"2147483647", "-2147483648", "2147483648", "4294967295",
				"-2147483649", "bogus", "'12'", "12abc", "007", "+5",
			} {
				stmts = append(stmts, fmt.Sprintf("PRAGMA %s=%s", name, v))
				stmts = append(stmts, "PRAGMA "+name)
			}
			differ(t, "pragmaint/"+name, stmts)
		})
	}
}

// secure_delete has one spelling of its own ("fast" == 2); every other value
// is sqlite3GetBoolean's, and its GETTER reports the resulting 0/1/2.
func TestPragmaSecureDeleteValueSpellings(t *testing.T) {
	var stmts []string
	for _, v := range []string{"fast", "1", "0", "on", "off", "2", "-1", "0x10", "bogus", "full"} {
		stmts = append(stmts, "PRAGMA secure_delete="+v, "PRAGMA secure_delete")
	}
	differ(t, "pragmasecdel", stmts)
}

// synchronous keeps getSafetyLevel's OTHER two arguments -- FULL and EXTRA are
// live names there, and an unrecognized one defaults to 1, not 0
// (pragma.c:1140's "getSafetyLevel(zRight,0,1)").
func TestPragmaSynchronousValueSpellings(t *testing.T) {
	var stmts []string
	for _, v := range []string{"0", "1", "2", "3", "off", "normal", "full", "extra", "bogus", "-1", "256"} {
		stmts = append(stmts, "PRAGMA synchronous="+v, "PRAGMA synchronous")
	}
	differ(t, "pragmasync", stmts)
}
