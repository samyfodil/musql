// Tests for the unistr() function.
package compat

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// bs is a single backslash -- unistr's escape introducer.
const bs = string(rune(92))

func TestUnistr(t *testing.T) {
	for _, q := range []string{
		"SELECT unistr('G" + bs + "u00e4ste')",
		"SELECT hex(unistr('" + bs + "UFFFFFFFF'))",
		"SELECT unistr('" + bs + "" + bs + "')",
		"SELECT hex(unistr('" + bs + "0041'))",
		"SELECT hex(unistr('" + bs + "u0041'))",
		"SELECT hex(unistr('" + bs + "+000041'))",
		"SELECT hex(unistr('" + bs + "U00000041'))",
		"SELECT hex(unistr('" + bs + "u0000'))",
		"SELECT hex(unistr('" + bs + "u007f'))",
		"SELECT hex(unistr('" + bs + "u0080'))",
		"SELECT hex(unistr('" + bs + "u07ff'))",
		"SELECT hex(unistr('" + bs + "u0800'))",
		"SELECT hex(unistr('" + bs + "uffff'))",
		"SELECT hex(unistr('" + bs + "U00010000'))",
		"SELECT hex(unistr('" + bs + "U0010ffff'))",
		"SELECT hex(unistr('" + bs + "U00110000'))",
		"SELECT hex(unistr('" + bs + "ud800'))",
		"SELECT hex(unistr('" + bs + "udfff'))",
		"SELECT hex(unistr('" + bs + "U7fffffff'))",
		"SELECT hex(unistr('" + bs + "Uffffffff'))",
		"SELECT unistr('abc" + bs + "u0041def')",
		"SELECT unistr('')",
		"SELECT unistr('" + bs + "u0041" + bs + "u0042abc')",
		"SELECT unistr('abc" + bs + "u0041" + bs + "u0042')",
		"SELECT unistr('" + bs + "" + bs + "u0041')",
		"SELECT unistr('x" + bs + "" + bs + "y')",
		"SELECT hex(unistr('" + bs + "uFFFF'))",
		"SELECT hex(unistr('" + bs + "ufffF'))",
		"SELECT unistr('" + bs + "u041')",
		"SELECT unistr('" + bs + "q0041')",
		"SELECT unistr('" + bs + "')",
		"SELECT unistr('" + bs + "u')",
		"SELECT unistr('" + bs + "+00041')",
		"SELECT unistr('" + bs + "U0000041')",
		"SELECT unistr('abc" + bs + "')",
		"SELECT unistr(NULL)",
		"SELECT typeof(unistr(NULL))",
		"SELECT unistr(123)",
		"SELECT typeof(unistr(123))",
		"SELECT unistr(1.5)",
		"SELECT unistr(x'41')",
		"SELECT unistr()",
		"SELECT unistr('a','b')",
		"SELECT length(unistr('G" + bs + "u00e4ste'))",
		"SELECT unicode(unistr('" + bs + "u00e4'))",
		"SELECT quote(unistr('G" + bs + "u00e4ste'))",
		"SELECT upper(unistr('" + bs + "u00e4'))",
		"SELECT unistr('" + bs + "u00e4') = char(228)",
	} {
		differ(t, q, []string{q})
	}
}

// TestUnistrFuzz randomizes the escape forms and the code points, because the
// writer branches on the VALUE and the parser branches on the introducer and
// the digit count -- a fixed list covers the boundaries it was written for and
// nothing between them. Roughly one draw in seven is a deliberately MALFORMED
// escape, so the error path is compared about as often as the success one.
func TestUnistrFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20260803))
	for i := 0; i < 120; i++ {
		i := i
		t.Run(fmt.Sprintf("u%03d", i), func(t *testing.T) {
			var b strings.Builder
			for k := 0; k < 1+rng.Intn(4); k++ {
				switch rng.Intn(7) {
				case 0:
					b.WriteString(bs + bs)
				case 1:
					b.WriteString(string(rune('a' + rng.Intn(26))))
				case 2:
					fmt.Fprintf(&b, bs+"%04x", rng.Intn(0x10000))
				case 3:
					fmt.Fprintf(&b, bs+"u%04X", rng.Intn(0x10000))
				case 4:
					fmt.Fprintf(&b, bs+"+%06x", rng.Intn(0x120000))
				case 5:
					fmt.Fprintf(&b, bs+"U%08x", rng.Uint32())
				default:
					fmt.Fprintf(&b, bs+"%c%03x", "qzZ+u"[rng.Intn(5)], rng.Intn(0x1000))
				}
			}
			lit := strings.ReplaceAll(b.String(), "'", "''")
			differ(t, lit, []string{
				fmt.Sprintf("SELECT hex(unistr('%s'))", lit),
				fmt.Sprintf("SELECT length(unistr('%s'))", lit),
				fmt.Sprintf("SELECT quote(unistr('%s'))", lit),
			})
		})
	}
}

// TestUnistrQuote gates unistr_quote(X): quote() with printf's ALTERNATE
// FORM ("%#Q" in sqlite3QuoteValue). The "#" does nothing unless the string
// holds a CONTROL byte (<= 0x1f) -- SQLite clears the flag when it finds none
// -- so a merely non-ASCII string quotes plainly. That is the rule
// func9.test pins with unistr_quote(unistr('G\u00e4ste')) -> 'Gaste' with an
// umlaut, and the reason this is NOT "escape anything non-ASCII". quote() is
// paired with every case so the two are compared side by side.
func TestUnistrQuote(t *testing.T) {
	for _, q := range []string{
		"SELECT unistr_quote(unistr(" + "'G" + bs + "u00e4ste'" + "))", // non-ASCII alone does NOT trigger the wrapper
		"SELECT quote(unistr(" + "'G" + bs + "u00e4ste'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'G" + bs + "u00e4ste'" + ")))",
		"SELECT unistr_quote(unistr(" + "'abc'" + "))", // plain
		"SELECT quote(unistr(" + "'abc'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'abc'" + ")))",
		"SELECT unistr_quote(unistr(" + "''" + "))", // empty
		"SELECT quote(unistr(" + "''" + "))",
		"SELECT hex(unistr_quote(unistr(" + "''" + ")))",
		"SELECT unistr_quote(unistr(" + "'it''s'" + "))", // an apostrophe still doubles
		"SELECT quote(unistr(" + "'it''s'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'it''s'" + ")))",
		"SELECT unistr_quote(unistr(" + "'a" + bs + "u0009b'" + "))", // a TAB is a control byte -> unistr() wrapper
		"SELECT quote(unistr(" + "'a" + bs + "u0009b'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'a" + bs + "u0009b'" + ")))",
		"SELECT unistr_quote(unistr(" + "'" + bs + "u0000'" + "))", // NUL
		"SELECT quote(unistr(" + "'" + bs + "u0000'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'" + bs + "u0000'" + ")))",
		"SELECT unistr_quote(unistr(" + "'" + bs + "u001f'" + "))", // the last control byte
		"SELECT quote(unistr(" + "'" + bs + "u001f'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'" + bs + "u001f'" + ")))",
		"SELECT unistr_quote(unistr(" + "'" + bs + "u0020'" + "))", // the first NON-control byte
		"SELECT quote(unistr(" + "'" + bs + "u0020'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'" + bs + "u0020'" + ")))",
		"SELECT unistr_quote(unistr(" + "'a" + bs + "" + bs + "b'" + "))", // a backslash, no control byte -> plain quote
		"SELECT quote(unistr(" + "'a" + bs + "" + bs + "b'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'a" + bs + "" + bs + "b'" + ")))",
		"SELECT unistr_quote(unistr(" + "'a" + bs + "u0009" + bs + "" + bs + "b'" + "))", // a backslash INSIDE the wrapper doubles
		"SELECT quote(unistr(" + "'a" + bs + "u0009" + bs + "" + bs + "b'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'a" + bs + "u0009" + bs + "" + bs + "b'" + ")))",
		"SELECT unistr_quote(unistr(" + "'" + bs + "u000a" + bs + "u000d'" + "))", // newline and carriage return
		"SELECT quote(unistr(" + "'" + bs + "u000a" + bs + "u000d'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'" + bs + "u000a" + bs + "u000d'" + ")))",
		"SELECT unistr_quote(unistr(" + "'" + bs + "u0009" + bs + "u00e4'" + "))", // control AND non-ASCII together
		"SELECT quote(unistr(" + "'" + bs + "u0009" + bs + "u00e4'" + "))",
		"SELECT hex(unistr_quote(unistr(" + "'" + bs + "u0009" + bs + "u00e4'" + ")))",
		"SELECT unistr_quote(NULL)",
		"SELECT unistr_quote(123)",
		"SELECT unistr_quote(1.5)",
		"SELECT unistr_quote(x'00ff')",
		"SELECT unistr_quote()",
		"SELECT unistr_quote('a','b')",
		"SELECT format('%#Q', unistr(" + "'G" + bs + "u00e4ste'" + "))",
		"SELECT format('%Q', unistr(" + "'G" + bs + "u00e4ste'" + "))",
		"SELECT format('%#Q', unistr(" + "'a" + bs + "u0009b'" + "))",
		"SELECT format('%q', unistr(" + "'a" + bs + "u0009b'" + "))",
	} {
		differ(t, q, []string{q})
	}
}
