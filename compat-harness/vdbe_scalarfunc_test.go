// Scalar function conformance against SQLite.
package compat

import (
	"bytes"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/samyfodil/musql/engine"
)

// scalarFuncCases is the deterministic differential gate: every entry is a
// complete "SELECT ..." statement expected to either error identically on
// both engines, or produce an identical (type, value) result.
var scalarFuncCases = []string{
	// ---- abs(): the fnAbs fix (TEXT/BLOB always REAL, never the
	// integer-overflow error even at the exact INT64_MIN magnitude) ----
	`SELECT abs(-5)`,
	`SELECT abs(5)`,
	`SELECT abs(-5.5)`,
	`SELECT abs(NULL)`,
	`SELECT abs('-5')`,
	`SELECT abs('5')`,
	`SELECT abs('5.5')`,
	`SELECT abs('-5abc')`,
	`SELECT abs(x'35')`,
	`SELECT typeof(abs('5'))`,
	`SELECT typeof(abs(5))`,
	`SELECT abs(-9223372036854775808)`,
	`SELECT abs(-9223372036854775808.0)`,
	`SELECT abs('-9223372036854775808')`,
	`SELECT typeof(abs('-9223372036854775808'))`,

	// ---- pre-existing core functions, spot-checked for no regression ----
	`SELECT length('hello')`,
	`SELECT length(x'0102030405')`,
	`SELECT length(NULL)`,
	`SELECT length(1.5)`,
	`SELECT length(1000000)`,
	`SELECT lower('HeLLo')`,
	`SELECT upper('HeLLo')`,
	`SELECT substr('hello world', 1, 5)`,
	`SELECT substr('hello world', -5)`,
	`SELECT substr('hello world', 2, -1)`,
	`SELECT coalesce(NULL, NULL, 3, 4)`,
	`SELECT ifnull(NULL, 5)`,
	`SELECT ifnull(1, 5)`,
	`SELECT nullif(1, 1)`,
	`SELECT nullif(1, 2)`,
	`SELECT typeof(1)`,
	`SELECT typeof(1.5)`,
	`SELECT typeof('x')`,
	`SELECT typeof(x'01')`,
	`SELECT typeof(NULL)`,
	`SELECT hex('AB')`,
	`SELECT hex(x'0a0b0c')`,
	`SELECT hex(NULL)`,

	// ---- round() ----
	`SELECT round(3.14159, 2)`,
	`SELECT round(2.5)`,
	`SELECT round(3.5)`,
	`SELECT round(-2.5)`,
	`SELECT round(-3.5)`,
	`SELECT round(1)`,
	`SELECT typeof(round(1))`,
	`SELECT round(NULL)`,
	`SELECT round(NULL, 2)`,
	`SELECT round(1, NULL)`,
	`SELECT round('abc')`,
	`SELECT round(1, -1)`,
	`SELECT round(123.456, -1)`,
	`SELECT round(1, 2, 3)`,
	`SELECT round(2.5, 0)`,
	`SELECT round(1e308, -1)`,
	`SELECT round(1.5, 0.9)`,
	`SELECT round(1.23456789012345, 20)`,
	`SELECT round(1, 1000000000)`,
	`SELECT round(1, -1000000000)`,
	`SELECT round(9223372036854775807.0)`,
	`SELECT round(-9223372036854775807.0)`,
	`SELECT round(1e300, 2)`,
	`SELECT round(0.1, 20)`,
	`SELECT round(0.1, 30)`,
	`SELECT round(0.1, 40)`,
	`SELECT round(2.005, 2)`,
	`SELECT round(2.345, 2)`,
	`SELECT round(-1.005, 2)`,
	`SELECT round(99999999999994.5)`,
	`SELECT round(9999999999999.55, 1)`,
	`SELECT round(9999999999999.556, 2)`,

	// ---- sign() ----
	`SELECT sign(5)`,
	`SELECT sign(-5)`,
	`SELECT sign(0)`,
	`SELECT sign(NULL)`,
	`SELECT sign('abc')`,
	`SELECT sign(-0.5)`,
	`SELECT sign(1.0/0)`,
	`SELECT sign('  -3.5xyz')`,
	`SELECT sign(x'00')`,
	`SELECT sign(x'31')`,
	`SELECT sign('5')`,
	`SELECT sign('5.5')`,
	`SELECT sign('-5')`,
	`SELECT sign('0')`,
	`SELECT typeof(sign(5))`,

	// ---- replace() ----
	`SELECT replace('abcabc', 'a', 'X')`,
	`SELECT replace('abc', '', 'X')`,
	`SELECT replace('abc', NULL, 'x')`,
	`SELECT replace(NULL, 'a', 'x')`,
	`SELECT replace('abc', 'a', NULL)`,
	`SELECT typeof(replace(1, 2, 3))`,
	`SELECT replace(123, 2, 'X')`,

	// ---- instr() ----
	`SELECT instr('abcabc', 'bc')`,
	`SELECT instr('abc', '')`,
	`SELECT instr('abc', 'x')`,
	`SELECT instr(NULL, 'a')`,
	`SELECT instr('abc', NULL)`,
	`SELECT instr(x'010203', x'02')`,
	`SELECT instr(12345, 34)`,
	`SELECT instr('héllo', 'llo')`,
	`SELECT instr('héllo', 'lo')`,
	`SELECT instr(x'68c3a96c6c6f', 'llo')`,
	`SELECT instr('héllo', x'6c6c6f')`,
	`SELECT instr(x'68c3a96c6c6f', x'6c6c6f')`,
	`SELECT instr(x'010203040203', x'0203')`,
	`SELECT instr('abcabc', x'6263')`,
	`SELECT instr(1.5, '.5')`,
	`SELECT instr('abc', 'abcd')`,
	`SELECT instr('', '')`,
	`SELECT instr('', 'a')`,
	`SELECT instr('a', '')`,
	`SELECT typeof(instr('a', 'a'))`,

	// ---- quote() ----
	`SELECT quote('abc')`,
	`SELECT quote(1)`,
	`SELECT quote(1.5)`,
	`SELECT quote(NULL)`,
	`SELECT quote(x'0102')`,
	`SELECT quote('a''b')`,
	`SELECT quote(3.0)`,
	`SELECT quote(1.0/0)`,
	`SELECT typeof(quote(NULL))`,

	// ---- char() ----
	`SELECT char(65, 66, 67)`,
	`SELECT char(128512)`, // U+1F600; this engine's lexer has no 0x-hex-integer-literal support at all (a general grammar gap unrelated to char() itself), so the decimal codepoint is used instead
	`SELECT char()`,
	`SELECT hex(char(NULL))`,
	`SELECT hex(char(-1))`,
	`SELECT hex(char(1114112))`,
	`SELECT hex(char(1114111))`,
	`SELECT hex(char(0))`,
	`SELECT hex(char(55296))`,
	`SELECT hex(char(57343))`,
	`SELECT hex(char(65536))`,
	`SELECT hex(char(127))`,
	`SELECT hex(char(128))`,
	`SELECT hex(char(65, 55296, 66))`,

	// ---- unicode() ----
	`SELECT unicode('A')`,
	`SELECT unicode('')`,
	`SELECT unicode(NULL)`,

	// ---- unhex() ----
	`SELECT hex(unhex('4142'))`,
	`SELECT unhex('xyz')`,
	`SELECT unhex('414', '2')`,
	`SELECT unhex(NULL)`,
	`SELECT hex(unhex('41', 'x'))`,
	`SELECT unhex('4g')`,
	`SELECT hex(unhex(':41', ':'))`,
	`SELECT unhex('AA', NULL)`,
	`SELECT hex(unhex(4142))`,
	`SELECT hex(unhex('41  42', '  '))`,
	`SELECT unhex('4:1:', '::')`,
	`SELECT unhex('41 42', ' ')`,
	`SELECT unhex('4 1', ' ')`,
	`SELECT hex(unhex(' 41 ', ' '))`,

	// ---- zeroblob()/randomblob() (length/type only for the latter) ----
	`SELECT typeof(zeroblob(3))`,
	`SELECT length(zeroblob(3))`,
	`SELECT hex(zeroblob(3))`,
	`SELECT zeroblob(-1)`,

	// ---- ltrim/rtrim/trim() ----
	`SELECT ltrim('  abc  ')`,
	`SELECT rtrim('  abc  ')`,
	`SELECT trim('  abc  ')`,
	`SELECT trim('xxabcxx', 'x')`,
	`SELECT ltrim(NULL)`,
	`SELECT trim('abc', '')`,
	`SELECT hex(trim('  abc'||char(9)||char(10)))`,
	`SELECT trim(char(9,10,32)||'abc'||char(9,10,32))`,
	`SELECT hex(ltrim(char(9,10,13,32)||'x'))`,
	`SELECT typeof(ltrim(123))`,
	`SELECT hex(trim(x'000102'))`,
	`SELECT typeof(trim(x'000102'))`,
	`SELECT trim('abc', NULL)`,
	`SELECT trim(NULL, 'a')`,

	// ---- likelihood()/likely()/unlikely() (identity pass-through) ----
	`SELECT likelihood(1, 0.5)`,
	`SELECT likely(1)`,
	`SELECT unlikely(1)`,
	`SELECT likelihood(1, 1.5)`,
	`SELECT likelihood(1, -0.1)`,
	`SELECT likelihood(1, 0)`,
	`SELECT likelihood(1, 1)`,
	`SELECT likelihood(123, 0.5+0.3)`,
	`SELECT likely(NULL)`,
	`SELECT typeof(likely('x'))`,

	// ---- iif() ----
	`SELECT iif(1, 2, 3)`,
	`SELECT iif(0, 2, 3)`,
	`SELECT iif(NULL, 2, 3)`,

	// ---- scalar min()/max() (2+ args; the aggregate form is untouched) ----
	`SELECT min(3, 1, 2)`,
	`SELECT max(3, 1, 2)`,
	`SELECT min(1, NULL, 2)`,
	`SELECT max(1, NULL, 2)`,
	`SELECT min('b', 'a')`,
	`SELECT typeof(min(1, 2.0))`,
	`SELECT typeof(min(1.0, 1))`,
	`SELECT min(1, 1.0)`,
	`SELECT min('a', 1)`,
	`SELECT min(1, 'a')`,
	`SELECT max('a', 1)`,
	`SELECT min(1, 2, 3, 'x')`,

	// ---- printf()/format() ----
	`SELECT printf('%d', 42)`,
	`SELECT printf('%5d', 42)`,
	`SELECT printf('%-5d|', 42)`,
	`SELECT printf('%05d', 42)`,
	`SELECT printf('%+d', 42)`,
	`SELECT printf('% d', 42)`,
	`SELECT printf('%x', 255)`,
	`SELECT printf('%X', 255)`,
	`SELECT printf('%#x', 255)`,
	`SELECT printf('%o', 8)`,
	`SELECT printf('%#o', 8)`,
	`SELECT printf('%c', 65)`,
	`SELECT printf('%s', 'hi')`,
	`SELECT printf('%10s|', 'hi')`,
	`SELECT printf('%-10s|', 'hi')`,
	`SELECT printf('%.1s', 'hi')`,
	`SELECT printf('%q', 'it''s')`,
	`SELECT printf('%Q', 'abc')`,
	`SELECT printf('%Q', NULL)`,
	`SELECT printf('%w', 'abc')`,
	`SELECT printf('%e', 12345.6789)`,
	`SELECT printf('%g', 12345.6789)`,
	`SELECT printf('%g', 0.0000123)`,
	`SELECT printf('%%')`,
	`SELECT printf('%d %s', 1)`,
	`SELECT printf('%d', 'abc')`,
	`SELECT printf('%d', 3.9)`,
	`SELECT printf('%u', -1)`,
	`SELECT printf('%d', NULL)`,
	`SELECT printf('%s', NULL)`,
	`SELECT printf('%.2q', 'abcdef')`,
	`SELECT printf('%n%d', 1, 2)`,
	`SELECT printf('[%*d]', 5, 3)`,
	`SELECT printf('[%.*f]', 2, 3.14159)`,
	`SELECT printf('%i', 7)`,
	`SELECT printf('%c', 'hello')`,
	`SELECT printf('%c', 916)`,
	`SELECT printf('%s', 3.14)`,
	`SELECT printf('%s', x'414243')`,
	`SELECT printf('%d', x'0a')`,
	`SELECT printf('%d', 1.5e19)`,
	`SELECT printf('%.3d', 5)`,
	`SELECT printf('%5.2f', 3.14159)`,
	`SELECT printf('%-5.2f|', 3.14159)`,
	`SELECT printf('%x', -1)`,
	`SELECT printf('%d', 9223372036854775807)`,
	`SELECT printf('%d', -9223372036854775808)`,
	`SELECT printf('abc')`,
	`SELECT printf('%')`,
	`SELECT printf('%.2q', 'a''bcdef')`,
	`SELECT printf('%08d', -5)`,
	`SELECT printf('%010.3d', 5)`,
	`SELECT printf('%-010d|', 5)`,
	`SELECT printf('%+.3d', 5)`,
	`SELECT printf('% .3d', 5)`,
	`SELECT printf('%#x', 0)`,
	`SELECT printf('%#o', 0)`,
	`SELECT printf('%.0s', 'abc')`,
	`SELECT printf('%5.0f', 3.7)`,
	`SELECT printf('%-5d|', -3)`,
	`SELECT printf('%x', 3.9)`,
	`SELECT printf('%d', -0.5)`,
	`SELECT printf('% d', -5)`,
	`SELECT printf('%0-10d', 5)`,
	`SELECT printf('%-10s', 5)`,
	`SELECT printf('%-010s', 5)`,
	`SELECT printf('%-10f', 3.5)`,
	`SELECT printf('%-010f', 3.5)`,
	`SELECT printf('%+010d', 5)`,
	`SELECT printf('%010.2f', 3.14159)`,
	`SELECT printf('%-+10d', 5)`,
	`SELECT printf('%c', NULL)`,
	`SELECT printf('%.5c', 'abc')`,
	`SELECT printf('%#010x', 255)`,
	`SELECT printf('%#X', 255)`,
	`SELECT printf('%#010o', 8)`,
	`SELECT printf('%#10x', 255)`,
	`SELECT printf('%#-10x', 255)`,
	`SELECT printf('%Q', 5)`,
	`SELECT printf('%Q', 5.5)`,
	`SELECT printf('%q', 5)`,
	`SELECT printf('%3c', 'abc')`,
	`SELECT printf('%-3c', 'a')`,
	`SELECT printf('%Q', x'4142')`,
	`SELECT printf('%.17f', 1.0/3)`,
	`SELECT printf('%.25f', 1.0/3)`,
	`SELECT printf('%.2f', 2.345)`,
	`SELECT printf('%.2f', 2.005)`,
	`SELECT printf('%.0f', 2.5)`,
	`SELECT printf('%.0f', 3.5)`,
	`SELECT printf('%f', 1.5)`,
	`SELECT printf('%.3f', 100.0)`,
	`SELECT printf('%.2f', 1e20)`,
	`SELECT printf('%.2f', -1.005)`,
	`SELECT printf('%.30f', 1.0/7)`,
	`SELECT printf('%.30f', 123.0/7)`,
	`SELECT printf('%.20f', 12345.6789)`,
	`SELECT printf('%.20f', 0.123456789012345678)`,
	`SELECT printf('%.3g', 123456)`,
	`SELECT printf('%.3g', 0.000123456)`,
	`SELECT printf('%#.3g', 1.5)`,
	`SELECT printf('%e', 0.0)`,
	`SELECT printf('%g', 100000)`,
	`SELECT printf('%g', 1000000)`,
	`SELECT printf('%.10d', -5)`,
	`SELECT printf('%5.3s', 'abcdef')`,
	`SELECT printf('%c%c', 65, 66)`,
	`SELECT format('%d', 5)`,
	`SELECT format('%d-%s', 1, 'a')`,

	// ---- date()/time()/datetime()/julianday()/unixepoch()/strftime() ----
	`SELECT date('2024-03-01')`,
	`SELECT typeof(unixepoch('2024-03-01'))`,
	`SELECT CAST(unixepoch('2024-03-01') AS TEXT)`,
	`SELECT typeof(julianday('2024-03-01'))`,
	`SELECT julianday('2024-03-01')`,
	`SELECT datetime('2024-03-01')`,
	`SELECT time('2024-03-01 12:34:56')`,
	`SELECT date('2024-03-01', '+1 month')`,
	`SELECT date('2024-01-31', '+1 month')`,
	`SELECT date('2024-03-01', 'start of month')`,
	`SELECT date('2024-03-01', 'start of year')`,
	`SELECT date('2024-03-15', 'weekday 0')`,
	`SELECT date('2024-03-15')`,
	`SELECT strftime('%w', '2024-03-15')`,
	`SELECT date(1710288000, 'unixepoch')`,
	`SELECT date('2024-02-29')`,
	`SELECT date('2023-02-29')`,
	`SELECT date(2460359.5)`,
	`SELECT datetime('2024-03-01 12:00:00', 'subsec')`,
	`SELECT strftime('%J', '2024-03-01')`,
	`SELECT strftime('%s', '2024-03-01')`,
	`SELECT strftime('%f', '2024-03-01 12:00:00.123456')`,
	`SELECT date('2024-03-15', 'weekday 5')`,
	`SELECT strftime('%f', '2024-03-01 12:00:01.999999')`,
	`SELECT strftime('%f', '2024-03-01 12:00:01.9999999')`,
	`SELECT date('2024-03-01', ' -1 day')`,
	`SELECT date('2024-03-01', '1 day')`,
	`SELECT date('2024-03-01', '+1.5 days')`,
	`SELECT time('2024-03-01', '+1.5 hours')`,
	`SELECT datetime('2024-03-01', '+90 minutes')`,
	`SELECT date('2024-03-01', '-1 years')`,
	`SELECT date(0)`,
	`SELECT date(-1)`,
	`SELECT julianday('0000-01-01')`,
	`SELECT date('10000-01-01')`,
	`SELECT date('0000-01-01')`,
	`SELECT date('-4713-11-24')`,
	`SELECT date('2024-03-01T12:34')`,
	`SELECT date('2024-03-01 12:34:56.789')`,
	`SELECT time('12:34')`,
	`SELECT time('12:34:56.789')`,
	`SELECT datetime('2024-03-01 12:34:56.789')`,
	`SELECT date('2024-3-1')`,
	`SELECT date('2024-03-01 25:00:00')`,
	`SELECT date('2024-13-01')`,
	`SELECT date('2024-03-32')`,
	`SELECT date('2024-03-01Z')`,
	`SELECT date('2024-03-01 12:00:00+02:00')`,
	`SELECT date('2024-03-01 12:00:00-02:30')`,
	`SELECT datetime('2024-03-01 12:00:00+02:00')`,
	`SELECT datetime('2024-03-01 12:00:00-02:30')`,
	`SELECT date('  2024-03-01  ')`,
	`SELECT date('2024-03-01 ')`,
	`SELECT date('2024-03-01', 'junk modifier')`,
	`SELECT date(NULL)`,
	`SELECT date('2024-03-01', NULL)`,
	`SELECT time('2024-03-01 12:34:56', 'start of day')`,
	`SELECT strftime('%Y') IS NOT NULL`,
	`SELECT date(' 2024-03-01')`,
	`SELECT date('2024-03-01  ')`,
	`SELECT date('2024-03-01', '  +1 day')`,
	`SELECT date('2024-03-01', '+1 day  ')`,
	`SELECT date('2024-03-01', 'START OF MONTH')`,
	`SELECT date('2024-03-01', '+1 DAY')`,
	`SELECT date('2024-03-01', 'weekday 7')`,
	`SELECT date('2024-03-01', 'weekday -1')`,
	`SELECT date('2024-03-01', 'weekday 1.5')`,
	`SELECT datetime('12:34')`,
	`SELECT datetime('12:34:56.789Z')`,
	`SELECT date('NOW') IS NOT NULL`,
	`SELECT date('Now') IS NOT NULL`,
	`SELECT datetime('2024-03-01 12:00:00Z')`,
	`SELECT date('2024-03-01t12:00:00')`,
	`SELECT date('2024-03-01t12:00')`,
	`SELECT strftime('%d %m %Y %H %M %S', '2024-03-05 07:08:09')`,
	`SELECT strftime('%j', '2024-01-01')`,
	`SELECT strftime('%j', '2024-12-31')`,
	`SELECT strftime('%W', '2024-01-01')`,
	`SELECT strftime('%W', '2024-01-08')`,
	// ---- printf: the "," and "!" flags, and the sign flags on floats ----
	//
	// "," groups the integer digits in threes, on the DECIMAL families only.
	// The integer and float verbs order zero-padding against grouping
	// OPPOSITELY -- integers pad then group (and may overshoot the width),
	// floats group then pad. "!" switches %s's width AND precision from
	// bytes to characters. "+"/" " on a float verb were silently dropped
	// before, and %c of an EMPTY string yields a NUL byte just as NULL does.
	`SELECT printf('%,d', 1234567)`,
	`SELECT printf('%,i', -1234567)`,
	`SELECT printf('%,u', 1234567)`,
	`SELECT printf('%,d', 999)`,
	`SELECT printf('%,d', 1000)`,
	`SELECT printf('%,d', 9223372036854775807)`,
	`SELECT printf('%,f', 1234567.5)`,
	`SELECT printf('%,f', -1234567.5)`,
	`SELECT printf('%,.2f', 1234567.891)`,
	`SELECT printf('%,g', 1234)`,    // fixed notation: grouped
	`SELECT printf('%,g', 1234567)`, // scientific: NOT grouped
	`SELECT printf('%,e', 1234.5)`,  // %e can never group
	`SELECT printf('%,o', 1234567)`, // non-decimal: never grouped
	`SELECT printf('%,x', 1234567)`,
	`SELECT printf('%,X', 1234567)`,
	`SELECT printf('%,s', 'abcdefgh')`,
	`SELECT printf('%,08d', 1234)`, // pad THEN group: overshoots to 10
	`SELECT printf('%,012d', 1234)`,
	`SELECT printf('%,12d', 1234567)`,
	`SELECT printf('%,-12d', 1234567)`,
	`SELECT printf('%,012f', 1234.5)`, // group THEN pad
	`SELECT printf('%,015f', 1234.5)`,
	`SELECT printf('%,016g', 1234.5)`,
	`SELECT printf('%,10.1f', 1234.55)`,
	`SELECT printf('%,+d', 1234567)`,
	`SELECT printf('%, d', 1234567)`,
	`SELECT hex(printf('%!.3s', 'abcdefgh'))`,
	`SELECT hex(printf('%!5.3s', 'abcdefgh'))`,
	`SELECT hex(printf('%.3s', char(1492,1504,1492,32,1502,1492)))`,
	`SELECT hex(printf('%!.3s', char(1492,1504,1492,32,1502,1492)))`,
	`SELECT hex(printf('%8.3s', char(1492,1504,1492,32,1502,1492)))`,
	`SELECT hex(printf('%!8.3s', char(1492,1504,1492,32,1502,1492)))`,
	`SELECT hex(printf('%!-8.3s', char(1492,1504,1492,32,1502,1492)))`,
	`SELECT hex(printf('%5s', char(1492,1504,1492)))`,
	`SELECT hex(printf('%!5s', char(1492,1504,1492)))`,
	`SELECT printf('%!d', 42)`,
	// "!" applies to EVERY string verb, not just %s -- with a PRECISION, which
	// is the only place byte-vs-character counting shows. printf2.test caught
	// this as a wrong answer when %q/%Q/%w/%z were left out.
	`SELECT hex(printf('%!.3q', char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%.3q',  char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%!.3Q', char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%.3Q',  char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%!.3w', char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%!.3z', char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%!7.3Q', char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%7.3Q',  char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%!9.3q', char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT hex(printf('%!-9.3q', char(1492,1504,1492,1502,1492,1504)))`,
	`SELECT printf('%!.3Q', 'abcdefgh')`,
	`SELECT printf('%!Q', NULL)`,
	`SELECT printf('%+f', 1.5)`,
	`SELECT printf('%+f', -1.5)`,
	`SELECT printf('%+f', -0.0)`,
	`SELECT printf('% f', 1.5)`,
	`SELECT printf('% f', -1.5)`,
	`SELECT printf('%+g', 1234.5)`,
	`SELECT printf('%+e', 1.5)`,
	`SELECT printf('%+E', 1.5)`,
	`SELECT printf('%+10.2f', 1.5)`,
	`SELECT printf('%+-10.2f', 1.5)`,
	`SELECT printf('%+010.2f', 1.5)`,
	`SELECT hex(printf('%c', ''))`,
	`SELECT hex(printf('%.3c', ''))`,
	`SELECT hex(printf('%5.2c', ''))`,

	`SELECT date() IS NOT NULL`,
	`SELECT julianday() > 0`,
	`SELECT unixepoch() > 0`,

	// ---- embedded NUL in a TEXT value (engine's textBeforeNUL) ----
	//
	// A TEXT value keeps every byte, but any consumer that walks it as a C
	// string stops at the first NUL. Both halves of that rule are pinned
	// here: the cases that must see ALL the bytes come first, then the ones
	// that must stop. x'410042' is 'A' NUL 'B'; x'0041' is NUL 'A'.
	`SELECT hex(cast(x'410042' as text))`,
	`SELECT hex(cast(x'410042' as text) || 'Z')`,
	`SELECT hex(upper(cast(x'610062' as text)))`,
	`SELECT hex(replace(cast(x'410042' as text),'A','Z'))`,
	`SELECT instr(cast(x'410042' as text),'B')`,
	`SELECT hex(rtrim(cast(x'410042' as text)))`,
	`SELECT hex(ltrim(cast(x'004142' as text)))`,
	`SELECT cast(x'410042' as text) = 'A'`,
	`SELECT quote(x'410042')`,
	`SELECT typeof(cast(x'00' as text))`,
	// length(): characters BEFORE the first NUL (0 when it leads).
	`SELECT length(cast(x'00' as text))`,
	`SELECT length(cast(x'0041' as text))`,
	`SELECT length(cast(x'4100' as text))`,
	`SELECT length(cast(x'410042' as text))`,
	`SELECT length(cast(x'C3A90041' as text))`,
	`SELECT length(cast(x'41C3A900' as text))`,
	`SELECT length(cast(zeroblob(2) as text))`,
	`SELECT length(char(65,0,66))`,
	`SELECT length(x'410042')`, // a BLOB still counts all its bytes
	// unicode(): NULL when the C-string form is empty.
	`SELECT unicode(x'00')`,
	`SELECT unicode(cast(x'0041' as text))`,
	`SELECT unicode(cast(x'4100' as text))`,
	// quote() / substr() / printf(): truncated argument.
	`SELECT quote(cast(x'410042' as text))`,
	`SELECT hex(quote(cast(x'004142' as text)))`,
	`SELECT hex(substr(cast(x'410042' as text),1,3))`,
	`SELECT hex(substr(cast(x'410042' as text),-3))`,
	`SELECT hex(substr(cast(x'410042' as text),2))`,
	`SELECT hex(substr(cast(x'004142' as text),1,3))`,
	`SELECT hex(substr(cast(x'41C3A90042' as text),1,3))`,
	`SELECT hex(printf('%s',cast(x'410042' as text)))`,
	`SELECT hex(printf('%.2s',cast(x'410042' as text)))`,
	`SELECT hex(printf('%5s',cast(x'410042' as text)))`,
	`SELECT hex(printf('%q',cast(x'410042' as text)))`,
	`SELECT hex(printf('%Q',cast(x'410042' as text)))`,
	`SELECT hex(printf('%w',cast(x'410042' as text)))`,
	`SELECT hex(printf('%z',cast(x'410042' as text)))`,
	`SELECT hex(printf(cast(x'41002542' as text), 7))`, // the FORMAT string too
	`SELECT hex(printf('%c',cast(x'0041' as text)))`,   // ...but %c takes the NUL byte
	// trim(): the CHARSET is the C-string form (the subject is not).
	`SELECT hex(trim('AAB', cast(x'410042' as text)))`,
	`SELECT hex(ltrim('BBA', cast(x'004142' as text)))`,
	`SELECT hex(trim(cast(x'004142' as text),x'00'))`,
	// replace(): the empty-pattern short-circuit is a C-string test, but a
	// needle whose NUL is not first still matches over full bytes.
	`SELECT hex(replace(cast(x'41004200' as text), cast(x'0042' as text), 'Z'))`,
	`SELECT hex(replace(cast(x'41420043' as text), cast(x'420043' as text), 'Z'))`,
	// LIKE/GLOB: both operands truncated; likewise the ESCAPE character.
	`SELECT cast(x'410042' as text) LIKE 'A'`,
	`SELECT cast(x'410042' as text) GLOB 'A'`,
	`SELECT 'AB' LIKE cast(x'410025' as text)`,
	`SELECT cast(x'410042' as text) LIKE cast(x'410042' as text)`,
	`SELECT 'a%b' LIKE 'a#%b' ESCAPE cast(x'230041' as text)`,
	`SELECT 'a%b' LIKE 'a#%b' ESCAPE cast(x'002341' as text)`, // empty -> error
	// unhex() / json / date-time parse their C-string form.
	`SELECT hex(unhex(cast(x'34310030' as text)))`,
	// ...but unhex()'s IGNORE set does NOT: the 'X' past its leading NUL
	// still counts as ignorable.
	`SELECT hex(unhex('41X42', cast(x'005258' as text)))`,
	`SELECT json_valid(cast(x'7B7D00' as text))`,
	`SELECT json_valid(cast(x'7B7D0058' as text))`,
	`SELECT json_type(cast(x'5B5D00' as text))`,
	`SELECT hex(json_extract(cast(x'7B2261223A317D00' as text),'$.a'))`,
	`SELECT json_extract('{"a":1}', cast(x'242E6100582E62' as text))`,
	`SELECT date(cast(x'323032302D30312D303100' as text))`,
	`SELECT strftime('%Y', cast(x'323032302D30312D303100' as text))`,
	`SELECT julianday(cast(x'323032302D30312D303100' as text))`,
	// ...and so do a MODIFIER and strftime()'s own FORMAT.
	`SELECT date('2020-01-01', cast(x'2B31206461790058585858' as text))`,
	`SELECT datetime('2020-01-01', cast(x'7374617274206F66206D6F6E74680058' as text))`,
	`SELECT date(1710288000, cast(x'756E697865706F636800' as text))`,
	`SELECT strftime(cast(x'25590058' as text), '2020-01-01')`,
	// The zone modifiers, compared as VALUES now that they are implemented.
	`SELECT date('2024-03-01', 'localtime')`,
	`SELECT date('2024-03-01', 'utc')`,
	`SELECT date('2024-03-01', 'auto')`,
	// An unknown conversion character, or a specifier the string ends in,
	// stops printf's output there (printf.c:1033-1036): NULL when nothing
	// was appended yet. These used to be declined.
	`SELECT printf('%y', 1)`,
	`SELECT printf('%2$s %1$s', 'a', 'b')`,
	`SELECT printf('%1')`,
	`SELECT printf('%.')`,
	`SELECT printf('%-')`,
	`SELECT printf('% ')`,
	`SELECT printf('ab%-')`,
}

// scalarFuncDeclinedCases are constructs this package deliberately declines
// (see the doc comment in engine/scalar_printf.go): each must error on the
// ENGINE side (an "unsupported"-style outcome) rather than guess.
//
// The malformed printf() specifiers that used to be listed here follow C's
// etINVALID rule now and are compared in scalarFuncCases.
//
// The three date/time modifiers that used to be listed here -- localtime, utc
// and auto -- are IMPLEMENTED now and moved into scalarFuncCases above, where
// their values are compared against C SQLite rather than merely declined.
// They looked timezone-dependent and are not: both engines run in one process
// off the same system zoneinfo, so the comparison is exact. See
// datetime_timezone_test.go for the full battery, including the DST
// transitions where the two libraries disagree about a wall-clock time that
// does not exist.
var scalarFuncDeclinedCases = []string{
	`SELECT printf('%r', 3)`, // etORDINAL, not implemented
}

func TestScalarFunctionsMatchCSQLite(t *testing.T) {
	for _, pageSize := range []int{512, 4096} {
		t.Run(fmt.Sprintf("pagesize-%d", pageSize), func(t *testing.T) {
			testScalarFuncScenario(t, pageSize)
		})
	}
}

func testScalarFuncScenario(t *testing.T, pageSize int) {
	path := filepath.Join(t.TempDir(), fmt.Sprintf("scalarfunc_%d.sqlite", pageSize))
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()

	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	for _, s := range scalarFuncCases {
		t.Run(s, func(t *testing.T) {
			compareOneScalar(t, db, sdb, s)
		})
	}
	for _, s := range scalarFuncDeclinedCases {
		t.Run("declined: "+s, func(t *testing.T) {
			pager, err := db.SnapshotPager()
			if err != nil {
				t.Fatalf("SnapshotPager: %v", err)
			}
			defer pager.Close()
			_, rows, eerr := pager.Query(s)
			if eerr == nil {
				t.Fatalf("%s: expected engine to decline (error), got rows=%v", s, rows)
			}
		})
	}
}

// compareOneScalar runs sqlText (a single-column, single-row "SELECT ...")
// against both the pure-Go engine (via a fresh SnapshotPager over db,
// exactly like tcl_test.go's own read-your-writes pattern) and a live real
// SQLite connection, requiring either both to error or both to succeed with
// an identical (storage-class, value) result.
func compareOneScalar(t *testing.T, db *engine.Session, sdb *sql.DB, sqlText string) {
	t.Helper()
	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pager.Close()

	_, rows, eerr := pager.Query(sqlText)
	var rv any
	rerr := sdb.QueryRow(sqlText).Scan(&rv)

	if (eerr == nil) != (rerr == nil) {
		t.Fatalf("%s: engine err=%v (rows=%v), C SQLite err=%v", sqlText, eerr, rows, rerr)
	}
	if eerr != nil {
		return // both declined/errored -- fine, never counted wrong
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: engine returned unexpected shape: %v", sqlText, rows)
	}
	got := rows[0][0]
	if !scalarValueMatchesReal(got, rv) {
		t.Errorf("%s:\n  engine: %s\n  real:   %#v", sqlText, describeEngineValue(got), rv)
	}
}

// scalarValueMatchesReal reports whether got (an engine.Value, exposed via
// its exported Typ/I/F/S fields) exactly matches real (whatever
// database/sql's mattn driver scanned a real-SQLite column into: nil,
// int64, float64, string, or []byte -- the driver's own standard mapping
// of SQLite's five storage classes).
func scalarValueMatchesReal(got engine.Value, real any) bool {
	switch real := real.(type) {
	case nil:
		return got.Typ == engine.Null
	case int64:
		return got.Typ == engine.Int && got.I == real
	case float64:
		return got.Typ == engine.Float && got.F == real
	case string:
		return got.Typ == engine.Text && string(got.S) == real
	case []byte:
		return got.Typ == engine.Blob && bytes.Equal(got.S, real)
	}
	return false
}

func describeEngineValue(v engine.Value) string {
	switch v.Typ {
	case engine.Null:
		return "NULL"
	case engine.Int:
		return fmt.Sprintf("INT:%d", v.I)
	case engine.Float:
		return fmt.Sprintf("REAL:%v", v.F)
	case engine.Text:
		return fmt.Sprintf("TEXT:%q", string(v.S))
	case engine.Blob:
		return fmt.Sprintf("BLOB:%x", v.S)
	}
	return "?"
}

// TestScalarFuncsRandomShape checks random()/randomblob()'s TYPE and
// LENGTH contract only -- their actual content is nondeterministic by
// design and must never be gated against the oracle (per this package's
// own conformance-pass instructions).
func TestScalarFuncsRandomShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scalarfunc_random.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()
	pager, err := db.SnapshotPager()
	if err != nil {
		t.Fatalf("SnapshotPager: %v", err)
	}
	defer pager.Close()

	_, rows, err := pager.Query(`SELECT typeof(random()), random(), typeof(randomblob(11)), length(randomblob(11)), length(randomblob(0)), length(randomblob(-5))`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	r := rows[0]
	if r[0].Typ != engine.Text || string(r[0].S) != "integer" {
		t.Errorf("typeof(random()) = %v, want integer", r[0])
	}
	if r[1].Typ != engine.Int {
		t.Errorf("random() typ = %v, want Int", r[1].Typ)
	}
	if r[2].Typ != engine.Text || string(r[2].S) != "blob" {
		t.Errorf("typeof(randomblob(11)) = %v, want blob", r[2])
	}
	if r[3].Typ != engine.Int || r[3].I != 11 {
		t.Errorf("length(randomblob(11)) = %v, want 11", r[3])
	}
	if r[4].Typ != engine.Int || r[4].I != 1 {
		t.Errorf("length(randomblob(0)) = %v, want 1 (C SQLite's own minimum)", r[4])
	}
	if r[5].Typ != engine.Int || r[5].I != 1 {
		t.Errorf("length(randomblob(-5)) = %v, want 1", r[5])
	}
}

// TestScalarFuncsRoundTripFuzz throws a large number of random floats and
// precisions at round()/printf's %f, %e, and %g conversions and compares
// against a live real-SQLite connection -- a much broader net than any
// hand-picked case list for numeric-formatting edge cases (see
// engine/scalar_numfmt.go's own doc comment for the double-rounding and
// round-half-to-even-vs-away-from-zero traps this is designed to catch).
func TestScalarFuncsRoundTripFuzz(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scalarfunc_fuzz.sqlite")
	db, err := engine.Create(path)
	if err != nil {
		t.Fatalf("engine.Create: %v", err)
	}
	defer db.Close()
	sdb, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3): %v", err)
	}
	defer sdb.Close()

	rng := rand.New(rand.NewSource(1))
	randomFloat := func() float64 {
		switch rng.Intn(5) {
		case 0:
			return rng.Float64() * math.Pow(10, float64(rng.Intn(30)-15))
		case 1:
			return float64(rng.Int63()) / float64(rng.Intn(1000)+1)
		case 2:
			return float64(rng.Intn(1000)) + 0.5 // exact .5 ties
		case 3:
			return float64(rng.Intn(20000)-10000) / 8.0 // exact binary fractions
		default:
			return rng.NormFloat64() * math.Pow(10, float64(rng.Intn(20)-10))
		}
	}

	for i := 0; i < 400; i++ {
		f := randomFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		prec := rng.Intn(12)
		sqlText := fmt.Sprintf("SELECT round(%s, %d)", sqliteFloatLiteral(f), prec)
		compareOneScalar(t, db, sdb, sqlText)

		sqlText = fmt.Sprintf("SELECT printf('%%.%df', %s)", prec, sqliteFloatLiteral(f))
		compareOneScalar(t, db, sdb, sqlText)

		sqlText = fmt.Sprintf("SELECT printf('%%.%de', %s)", prec, sqliteFloatLiteral(f))
		compareOneScalar(t, db, sdb, sqlText)

		gprec := rng.Intn(12) + 1
		sqlText = fmt.Sprintf("SELECT printf('%%.%dg', %s)", gprec, sqliteFloatLiteral(f))
		compareOneScalar(t, db, sdb, sqlText)
	}
}

// sqliteFloatLiteral renders f as a SQL REAL literal precise enough to
// round-trip exactly (Go's %.17g is always sufficient for a float64).
func sqliteFloatLiteral(f float64) string {
	return fmt.Sprintf("%.17g", f)
}
