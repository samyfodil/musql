// Gate for printfField's %p case (scalar_printf.go): printf.c's fmtinfo
// table (row 20, "{'p', 16, 0, etPOINTER, 0, 1, 0}") gives %p its own
// unique combination of hex-digit case and alternate-form prefix case --
// UPPERCASE digits (charset offset 0, same half of aDigits %X uses) but a
// LOWERCASE "0x" prefix (prefix offset 1, the SAME aPrefix entry %x uses,
// not %X's own "0X"). Verified directly against mattn/go-sqlite3 3.53.3.
package compat

import "testing"

func TestPrintfPConversion(t *testing.T) {
	for _, q := range []string{
		`SELECT printf('%p')`,
		`SELECT printf('%p', 255)`,
		`SELECT printf('%#p', 255)`,
		`SELECT printf('%8p', 255)`,
		`SELECT printf('%-8p|', 255)`,
		`SELECT printf('%08p', 255)`,
		`SELECT printf('%#08p', 255)`,
		`SELECT printf('%.4p', 255)`,
		`SELECT printf('%p', 0)`,
		`SELECT printf('%#p', 0)`,
		`SELECT printf('%p', -1)`,
		`SELECT printf('%p', NULL)`,
	} {
		differ(t, "printf_p_conversion", []string{q})
	}
}
