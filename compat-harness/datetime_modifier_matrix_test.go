package compat

import (
	"fmt"
	"testing"
)

// Date/time modifiers and their interactions with values and functions.
var dateTimeValues = []string{
	// The ISO-8601 forms, with and without each optional piece.
	`2024-02-29`, `2024-02-29 13:45`, `2024-02-29 13:45:56`,
	`2024-02-29 13:45:56.789`, `2024-02-29T13:45:56`, `2024-02-29T13:45:56Z`,
	`2024-02-29 13:45:56+02:30`, `2024-02-29 13:45:56-05:00`,
	`2024-02-29 13:45:56.789Z`, `2024-02-29 13:45Z`,
	// Time only, which takes 2000-01-01 as its date.
	`13:45`, `13:45:56`, `13:45:56.789`, `13:45:56Z`,
	// A raw julian day number, and a unix epoch as a bare number.
	`2460370.0`, `2460370`, `0`, `1709213156`, `-1`, `0.5`,
	// Month-end and leap-year boundaries, and the year limits date.c enforces.
	`2023-02-28`, `2023-03-31`, `2024-12-31 23:59:59`, `2000-01-01`,
	`1970-01-01`, `0001-01-01`, `9999-12-31`,
	// Rejected by C: out-of-range fields, a bad separator, junk.
	`2024-02-30`, `2024-13-01`, `2024-00-01`, `2024-01-32`, `2024-02-29 24:00`,
	`2024-02-29 13:60`, `2024-02-29 13:45:61`, `2024-2-9`, `2024/02/29`,
	`not a date`, ``, ` `, `2024-02-29 `, ` 2024-02-29`,
}

// dateTimeModifiers tests recognized and unrecognized modifier spellings.
var dateTimeModifiers = []string{
	// The NNN <unit> family, every unit, singular and plural, signed, fractional.
	`+1 day`, `-1 day`, `1 days`, `+13 hours`, `-90 minutes`,
	`+1.5 seconds`, `-0.25 seconds`, `+45 seconds`,
	`+1 month`, `-1 months`, `+13 months`, `+1 year`, `-1 years`, `+400 years`,
	`+0 day`, `1000 days`, `-1000 days`, `+2.5 days`, `+1.5 months`,
	// No sign at all, and whitespace variants.
	`3 days`, ` +1 day`, `+1 day `, `+1  day`, `+1day`,
	// start of ...
	`start of day`, `start of month`, `start of year`,
	`start of  month`, `START OF MONTH`, `start of week`, `start of hour`,
	// weekday N, every N and the two that are out of range.
	`weekday 0`, `weekday 1`, `weekday 6`, `weekday 7`, `weekday -1`,
	`weekday 2.5`, `weekday`,
	// The unit-conversion and rounding modifiers.
	`unixepoch`, `julianday`, `auto`, `utc`, `zulu`, `subsec`, `subsecond`,
	`ceiling`, `floor`,
	// Unrecognized, which must make the whole call NULL.
	`nonsense`, `+1 fortnight`, `days`, ``, ` `, `+`, `-`, `+1`,
}

// TestDateTimeModifierMatrix tests all value/modifier/function combinations.
func TestDateTimeModifierMatrix(t *testing.T) {
	c, m := boundPair(t, nil)
	forms := []string{
		`SELECT quote(date(%s, %s))`,
		`SELECT quote(time(%s, %s))`,
		`SELECT quote(datetime(%s, %s))`,
		`SELECT quote(julianday(%s, %s))`,
		`SELECT quote(unixepoch(%s, %s))`,
		`SELECT quote(strftime('%%Y-%%m-%%dT%%H:%%M:%%f', %s, %s))`,
	}
	n, total := 0, 0
	for _, v := range dateTimeValues {
		for _, mod := range dateTimeModifiers {
			for _, f := range forms {
				q := fmt.Sprintf(f, sqlQuote(v), sqlQuote(mod))
				total++
				cv, mv := renderQuery(c, q), renderQuery(m, q)
				if cv != mv {
					n++
					if n <= 40 {
						t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
					}
				}
			}
		}
	}
	if n > 0 {
		t.Errorf("%d of %d value x modifier x function cells diverged", n, total)
	}
}

// TestDateTimeModifierChains tests modifier ordering and interactions.
func TestDateTimeModifierChains(t *testing.T) {
	c, m := boundPair(t, nil)
	n := 0
	for _, q := range []string{
		// Day-of-month overflow with floor/ceiling.
		`SELECT quote(date('2024-01-31','+1 month'))`,
		`SELECT quote(date('2024-01-31','+1 month','ceiling'))`,
		`SELECT quote(date('2024-01-31','+1 month','floor'))`,
		`SELECT quote(date('2024-01-31','floor','+1 month'))`,
		`SELECT quote(date('2024-03-31','-1 month','floor'))`,
		`SELECT quote(date('2020-02-29','+1 year','floor'))`,
		`SELECT quote(date('2020-02-29','+1 year','ceiling'))`,
		// start of ... before and after shifts.
		`SELECT quote(datetime('2024-02-29 13:45:56','start of month','+1 month','-1 day'))`,
		`SELECT quote(datetime('2024-02-29 13:45:56','+1 month','start of month'))`,
		`SELECT quote(datetime('2024-02-29 13:45:56','start of year','+45 days'))`,
		`SELECT quote(datetime('2024-02-29 13:45:56','start of day','+1 hour'))`,
		// weekday N.
		`SELECT quote(date('2024-02-29','weekday 0'))`,
		`SELECT quote(date('2024-02-29','weekday 4'))`,
		`SELECT quote(date('2024-02-29','weekday 4','weekday 4'))`,
		`SELECT quote(date('2024-02-29','weekday 1','+7 days'))`,
		// auto (first only).
		`SELECT quote(datetime(2460370,'auto'))`,
		`SELECT quote(datetime(1709213156,'auto'))`,
		`SELECT quote(datetime(1709213156,'auto','+1 day'))`,
		`SELECT quote(datetime(1709213156,'+1 day','auto'))`,
		`SELECT quote(datetime(2460370,'subsec','auto'))`,
		// unixepoch / julianday as modifiers.
		`SELECT quote(datetime(1709213156,'unixepoch'))`,
		`SELECT quote(datetime(1709213156,'unixepoch','+1 day'))`,
		`SELECT quote(datetime('1709213156','unixepoch'))`,
		`SELECT quote(datetime(2460370,'julianday'))`,
		`SELECT quote(datetime(1709213156,'julianday'))`,
		`SELECT quote(datetime(1709213156,'unixepoch','julianday'))`,
		// subsec changes output format.
		`SELECT quote(datetime('2024-02-29 13:45:56.789','subsec'))`,
		`SELECT quote(datetime('2024-02-29 13:45:56','subsec'))`,
		`SELECT quote(time('2024-02-29 13:45:56.789','subsec'))`,
		`SELECT quote(date('2024-02-29 13:45:56.789','subsec'))`,
		`SELECT quote(unixepoch('2024-02-29 13:45:56.789','subsec'))`,
		`SELECT quote(unixepoch('2024-02-29 13:45:56.789','subsecond'))`,
		`SELECT quote(julianday('2024-02-29 13:45:56.789','subsec'))`,
		// utc / zulu with and without timezone offset.
		`SELECT quote(datetime('2024-02-29 13:45:56','utc'))`,
		`SELECT quote(datetime('2024-02-29 13:45:56+02:00','utc'))`,
		`SELECT quote(datetime('2024-02-29 13:45:56Z','utc'))`,
		`SELECT quote(datetime('2024-02-29 13:45:56','zulu'))`,
		// Long chains and unrecognized modifiers.
		`SELECT quote(datetime('2024-02-29','+1 year','+1 month','+1 day','+1 hour','+1 minute','+1 second'))`,
		`SELECT quote(datetime('2024-02-29','+1 year','nonsense','+1 day'))`,
		`SELECT quote(datetime('2024-02-29','nonsense'))`,
		// Extreme boundaries.
		`SELECT quote(date('9999-12-31','+1 day'))`,
		`SELECT quote(date('0001-01-01','-1 day'))`,
		`SELECT quote(datetime('9999-12-31 23:59:59','+1 second'))`,
		`SELECT quote(julianday('9999-12-31','+1000 years'))`,
	} {
		cv, mv := renderQuery(c, q), renderQuery(m, q)
		if cv != mv {
			n++
			t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
		}
	}
	if n > 0 {
		t.Errorf("%d modifier chains diverged", n)
	}
}

// TestStrftimeAndTimediff tests strftime format characters and timediff.
func TestStrftimeAndTimediff(t *testing.T) {
	c, m := boundPair(t, nil)
	n := 0
	var qs []string
	// Format characters: recognized and unrecognized.
	for _, f := range []string{
		"%d", "%e", "%f", "%F", "%G", "%g", "%H", "%I", "%j", "%J", "%k", "%l",
		"%m", "%M", "%p", "%P", "%R", "%s", "%S", "%T", "%u", "%U", "%V", "%w",
		"%W", "%Y", "%%", "%", "%z", "%Z", "%q", "%1", "%_", "%n", "%t",
		"%Y-%m-%d", "%H:%M:%S", "[%Y]", "%Y%Y", "a%db",
	} {
		for _, v := range []string{
			`2024-02-29 13:45:56.789`, `2024-01-01 00:00:00`, `2024-12-31 23:59:59`,
			`2023-01-01`, `2024-06-05 07:08:09`, `1970-01-01 00:00:00`,
		} {
			qs = append(qs, fmt.Sprintf(`SELECT quote(strftime(%s, %s))`, sqlQuote(f), sqlQuote(v)))
		}
	}
	for _, a := range []string{`2024-02-29`, `2024-02-29 13:45:56`, `2020-01-01`, `2024-03-01`} {
		for _, b := range []string{`2024-02-29`, `2023-02-28`, `2024-03-31 01:02:03`, `2030-12-31`} {
			qs = append(qs, fmt.Sprintf(`SELECT quote(timediff(%s, %s))`, sqlQuote(a), sqlQuote(b)))
		}
	}
	qs = append(qs,
		`SELECT quote(timediff('2024-02-29','not a date'))`,
		`SELECT quote(timediff('2024-02-29'))`,
		`SELECT quote(timediff(2460370, 2460000))`,
		`SELECT quote(strftime('%Y'))`,
		`SELECT quote(strftime('%Y','2024-02-29','+1 day','start of month'))`,
	)
	for _, q := range qs {
		cv, mv := renderQuery(c, q), renderQuery(m, q)
		if cv != mv {
			n++
			if n <= 40 {
				t.Errorf("%s\n  cgo: %s\n  mus: %s", q, cv, mv)
			}
		}
	}
	if n > 0 {
		t.Errorf("%d of %d strftime/timediff cells diverged", n, len(qs))
	}
}
