// Tests printf() length modifiers "l" and "ll", which SQLite parses but
// ignores. Also tests invalid modifiers like "h" and formats ending in a modifier.
package compat

import "testing"

func TestPrintfLengthModifier(t *testing.T) {
	differ(t, "printf l/ll length modifier", []string{
		`SELECT printf('%lld',314159.2653) AS a`,
		`SELECT printf('%lld',12345678901234) AS b`,
		`SELECT printf('%ld',5) AS c`,
		`SELECT printf('%llx',255) AS d`,
		`SELECT printf('%llX',255) AS e`,
		`SELECT printf('%llo',255) AS f`,
		`SELECT printf('%lu',5) AS g`,
		`SELECT printf('%llu',-1) AS h`,
		`SELECT printf('%lf',1.5) AS i`,
		`SELECT printf('%lle',1.5) AS j`,
		`SELECT printf('%llg',1234567.0) AS k`,
		`SELECT printf('%lls','x') AS l`,
		`SELECT printf('%llc','xyz') AS m`,
		`SELECT printf('%llq','it''s') AS n`,
		// The modifier must not disturb flags, width or precision, which are
		// parsed BEFORE it.
		`SELECT printf('%-8lld|',42) AS o`,
		`SELECT printf('%08lld',42) AS p`,
		`SELECT printf('%+lld',42) AS q`,
		`SELECT printf('%,lld',1234567) AS r`,
		`SELECT printf('%12.4llf',3.14159) AS s`,
		`SELECT printf('%.3lls','abcdef') AS u`,
		`SELECT printf('%*lld',6,42) AS v`,
		// ...and the unmodified forms must be unchanged.
		`SELECT printf('%d %x %u %f %s',5,255,5,1.5,'x') AS w`,
		`SELECT printf('%%lld') AS y`,
		`SELECT printf('a%lldb%llsc',1,'z') AS z`,
	})
	// "z" is a C SQLite conversion (a string), so "%zd" is "%z" followed by
	// a literal "d" on BOTH engines -- not a length modifier, and not a gap.
	differ(t, "printf %z is a conversion, not a modifier", []string{
		`SELECT printf('%zd',5) AS a`,
		`SELECT printf('%z','x') AS b`,
	})
	// "h", and a format ending in the modifier itself, are etINVALID.
	differ(t, "printf invalid after the modifier", []string{
		`SELECT printf('%hd',5) AS a`,
		`SELECT printf('%hhd',5) AS b`,
		`SELECT printf('x%hd',5) AS c`,
		`SELECT printf('%l',5) AS d`,
		`SELECT printf('%ll',5) AS e`,
		`SELECT printf('%lll',5) AS f`,
		`SELECT printf('y%llld',5) AS g`,
	})
}
