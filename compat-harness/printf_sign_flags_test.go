package compat

import "testing"

// TestPrintfSignFlagsLastWins compares with C SQLite a printf spec that
// carries both '+' and ' ': printf.c keeps one flag_prefix, so whichever
// comes later wins (printf.c:269-270).
func TestPrintfSignFlagsLastWins(t *testing.T) {
	var q []string
	for _, f := range []string{"%+ d", "% +d", "%+ 5d", "% +5.2f", "%+ e", "% +g", "%+ i", "% +,d", "%0+ 8d", "% 0+8d"} {
		for _, v := range []string{"42", "-42", "3.5", "0"} {
			q = append(q, `SELECT printf('`+f+`', `+v+`)`)
		}
	}
	differ(t, "printf sign flags", q)
}
