package compat

import (
	"fmt"
	"testing"
)

// %q, %Q and %w render a NULL argument as a WORD, not as the empty string:
// "isnull = escarg==0; if( isnull ) escarg = (xtype==etSQLESCAPE2 ? \"NULL\" :
// \"(NULL)\");" (printf.c). %s is the one that renders NULL as empty, and this
// engine treated all four alike -- so printf('%q', NULL) was '' where real
// SQLite says '(NULL)'. The word is a string field like any other, so width,
// precision, '-' and '!' all still apply to it.
func TestPrintfEscapeVerbsOnNull(t *testing.T) {
	var stmts []string
	for _, verb := range []string{"%q", "%Q", "%w", "%s", "%#q", "%#Q",
		"%.2q", "%.2Q", "%.2w", "%10q", "%-10Q|", "%!3q", "%.0q", "%.0Q"} {
		for _, arg := range []string{"NULL", "'a''b'", `'a"b'`, "''", "1", "1.5", "x'00'"} {
			stmts = append(stmts, fmt.Sprintf("SELECT quote(printf('[%s]',%s))", verb, arg))
		}
	}
	// Several arguments at once, so a mis-stepped argument cursor shows.
	stmts = append(stmts,
		`SELECT printf('%s|%q|%w|%Q', NULL, NULL, NULL, NULL)`,
		`SELECT printf('%q%q%q', NULL, 'x', NULL)`,
		`SELECT printf('%Q,%Q', NULL, 'it''s')`,
	)
	differ(t, "printfescapenull", stmts)
}

// The REAL operands of "%" go through doubleToInt64, which SATURATES: Go's
// own int64(f) wraps a hugely positive float to MinInt64 on amd64, and
// OP_Remainder's fp arm reads that straight back out.
func TestRemainderSaturatesRealOperands(t *testing.T) {
	vals := []string{"1e308", "-1e308", "2.5", "-2.5", "0.0", "1e19", "-1e19",
		"9223372036854775807", "-9223372036854775808", "3", "-3", "-1", "1",
		"'1e308'", "'2.5'", "9.9e300"}
	var stmts []string
	for _, a := range vals {
		for _, b := range vals {
			e := a + " % " + b
			stmts = append(stmts, "SELECT quote("+e+"), typeof("+e+")")
			for _, op := range []string{" / ", " * ", " + ", " - "} {
				e := a + op + b
				stmts = append(stmts, "SELECT quote("+e+"), typeof("+e+")")
			}
		}
	}
	differ(t, "remaindersaturate", stmts)
}
