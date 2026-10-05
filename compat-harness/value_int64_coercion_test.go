package compat

import (
	"fmt"
	"testing"
)

// sqlite3_value_int64 on TEXT/BLOB reads integer prefix only, not exponent.
func TestValueInt64TextIsAtoi64(t *testing.T) {
	// Every context that reaches sqlite3_value_int64 here, and the CAST that
	// lands on the same C function from the other direction.
	tpl := []string{
		"CAST(%s AS INTEGER)",
		"zeroblob(%s)",
		"hex(zeroblob(%s))",
		"substr('abcdefghij', %s)",
		"substr('abcdefghij', 1, %s)",
		"char(%s)",
		"unicode(char(%s))",
		"printf('%%d', %s)",
		"printf('%%x', %s)",
		"round(1.23456789, %s)",
		"json_extract('[10,20,30]', '$[' || CAST(%s AS TEXT) || ']')",
	}
	vals := []string{
		"'1e5'", "'1.9e2'", "'1e400'", "'1e-5'", "'2.9'", "'-2.9'", "'  9 '",
		"'9abc'", "'abc'", "''", "'0x10'", "'+3'", "' -4 '", "'-'", "'.5'",
		"x'3132'", "x''", "NULL", "2.9", "-2.9", "1e19", "-1e19", "9223372036854775807",
	}
	for _, tp := range tpl {
		tp := tp
		t.Run(tp, func(t *testing.T) {
			var stmts []string
			for _, v := range vals {
				e := fmt.Sprintf(tp, v)
				stmts = append(stmts, "SELECT quote("+e+"), typeof("+e+")")
			}
			differ(t, "valueint64/"+tp, stmts)
		})
	}
}
