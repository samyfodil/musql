package compat

import (
	"fmt"
	"testing"
)

// TestSubstrIndexArithmetic verifies SUBSTR index arithmetic matches C SQLite.
func TestSubstrIndexArithmetic(t *testing.T) {
	subjects := []string{
		`'abcdefghij'`,
		`''`,
		`x'00010203040506'`,
		`'αβγδε'`,            // multi-byte, so the character/byte split shows
		`CAST(x'41ff42' AS TEXT)`, // not valid UTF-8; the raw bytes must survive
		`NULL`,
		`12345`,
		`1.5`,
	}
	ys := []string{
		"1", "2", "0", "-1", "-3", "-10", "-11", "-100",
		"1000000000", "1000000001", "-1000000000", "-1000000008",
		"-1000000009", "-1000000010", "-1000000011", "-1000000100",
		"-1e19", "1e19", "9223372036854775807", "-9223372036854775808",
		"'2'", "'-2'", "NULL", "2.7", "-2.7",
	}
	zs := []string{"", "0", "1", "3", "100", "-1", "-3", "-100",
		"1000000000", "-1000000000", "NULL", "9223372036854775807",
		"-9223372036854775808", "1e19", "-1e19"}

	for _, subj := range subjects {
		subj := subj
		t.Run(subj, func(t *testing.T) {
			var stmts []string
			for _, y := range ys {
				for _, z := range zs {
					var e string
					if z == "" {
						e = fmt.Sprintf("substr(%s,%s)", subj, y)
					} else {
						e = fmt.Sprintf("substr(%s,%s,%s)", subj, y, z)
					}
					stmts = append(stmts, "SELECT quote("+e+"), typeof("+e+")")
				}
			}
			differ(t, "substrrange/"+subj, stmts)
		})
	}
}
