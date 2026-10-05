package compat

import "testing"

// TestDateFunctionsMatchCSQLite compares date(), time(), datetime(),
// julianday(), unixepoch(), strftime() and timediff() with C SQLite over
// the time-value grammar (date.c's parseDateOrTime) and every modifier
// (parseModifier). The engine's date functions are a port of date.c onto its
// DateTime model (engine/scalar_datetime.go); the millisecond model before it
// answered NULL for a negative year, rendered hour 24 as the next day, and
// lost February under "start of month" over 2024-02-30.
func TestDateFunctionsMatchCSQLite(t *testing.T) {
	var q []string
	for _, tv := range []string{
		`'-0001-12-31'`, `'2024-01-01 24:00:00'`, `'12:00:00 +05:00'`, `'12:00:00 Z'`, `'12:00:00Z'`, `'2024-01-01T12:00'`, `'2024-01-01  12:00'`,
		`'2024-01-01 12:00:00.123456'`, `'2451545.5'`, `'1710288000'`, `' 2024-01-01'`, `'2024-01-01 '`, `'2024-01-01t12:00'`, `'2024-02-30'`,
		`'2024-01-01 12:00 +14:00'`, `'2024-01-01 12:00 -15:00'`, `'24:00'`, `'24:59:59'`, `'2024-01-01 12:00:00 z'`, `'1e5'`, `'abc'`, `'-4713-11-24'`, `'-4714-11-24'`,
		`'9999-12-31 23:59:59.999'`, `'2024-01-01T'`, `'2024-01-01 T12:00'`, `'00:00:60'`,
	} {
		q = append(q,
			`SELECT date(`+tv+`), time(`+tv+`), datetime(`+tv+`), julianday(`+tv+`), unixepoch(`+tv+`), strftime('%Y %j %s %f', `+tv+`)`,
			`SELECT datetime(`+tv+`, 'utc'), datetime(`+tv+`, '+1 day'), datetime(`+tv+`, 'unixepoch'), datetime(`+tv+`, 'start of month', 'subsec')`,
		)
	}
	differ(t, "date parse", q)
	var q2 []string
	for _, e := range []string{
		`strftime('%d|%e|%f|%F|%G|%g|%H|%k|%I|%l|%j|%J|%m|%M|%p|%P|%R|%s|%S|%T|%u|%w|%U|%V|%W|%Y|%%', '2024-12-30 13:05:09.5678')`,
		`strftime('%Q', '2024-01-01')`, `strftime('abc%', '2024-01-01')`, `strftime('%s', '2024-01-01', 'subsec')`, `strftime('%J', 2451545)`,
		`datetime('2024-01-31', '+1 month')`, `datetime('2024-01-31', '+1 month', 'floor')`, `datetime('2024-03-31', '-1 month', 'ceiling')`,
		`datetime('2024-01-01', '+1-02-03')`, `datetime('2024-01-01', '-0001-11-30 12:30')`, `datetime('2024-01-01', '+12:30:15.5')`, `datetime('2024-01-01', '-00:00:01')`,
		`datetime('2024-01-01', 'weekday 0')`, `datetime('2024-01-06 10:00', 'weekday 6')`, `datetime('2024-01-01', 'weekday 7')`, `datetime('2024-01-01', 'weekday 1.5')`,
		`datetime(1710288000, 'auto')`, `datetime(2460370.5, 'auto')`, `datetime(1710288000, 'julianday')`, `datetime(2460370.5, 'julianday')`, `datetime('2024-01-01', 'auto')`,
		`datetime('2024-01-01', '+1.5 days')`, `datetime('2024-01-01', '+1.5 years')`, `datetime('2024-02-29', '+1 year')`, `datetime('2024-02-29', '+1 year', 'floor')`,
		`datetime('-4713-11-24 12:00:00', '-1 day')`, `datetime('9999-12-31', '+1 day')`, `datetime('2024-01-01', 'start of week')`, `datetime('2024-01-01', 'START OF YEAR')`,
		`unixepoch('2024-01-01 00:00:00.123', 'subsec')`, `time('12:34:56.789', 'subsecond')`, `julianday('2024-01-01', 'utc', 'utc')`, `date('2024-01-01', '')`, `date('2024-01-01', NULL)`,
		`datetime('2024-01-01', '+10 hours', '+30 minutes', '-15 seconds')`, `datetime('2024-01-01', '+1 fortnight')`, `datetime('2024-01-01', '1 day')`, `datetime('2024-01-01','+1 days ')`,
		`date(x'32303234')`, `date(20240101)`, `time(0.5)`, `datetime('2000-01-01 00:00:00', 'unixepoch')`, `timediff('2024-03-01', '2024-01-31 24:00')`,
	} {
		q2 = append(q2, `SELECT `+e)
	}
	differ(t, "date modifiers", q2)
}

// TestDateLocaltimeUtcMatchesCSQLite covers the "localtime" and "utc"
// modifiers in whatever zone the test process runs in -- both engines read
// the same one. Verified by hand under America/Chicago, Asia/Kolkata,
// Australia/Lord_Howe and Pacific/Chatham as well as UTC.
func TestDateLocaltimeUtcMatchesCSQLite(t *testing.T) {
	var q []string
	for _, e := range []string{
		`datetime('2024-03-10 08:30', 'localtime')`, `datetime('2024-03-10 02:30', 'utc')`, `datetime('2024-11-03 01:30', 'utc')`,
		`datetime('2024-11-03 06:30', 'localtime')`, `datetime('1900-07-01 12:00', 'localtime')`, `datetime('2100-07-01 12:00', 'utc')`,
		`datetime('2024-07-01 12:00', 'localtime', 'localtime')`, `datetime('2024-07-01 12:00', 'utc', 'utc')`, `datetime('2024-07-01 12:00', 'localtime', 'utc')`,
		`datetime('2024-07-01 12:00:00.250', 'localtime', 'subsec')`, `datetime('2024-07-01 12:00 +02:00', 'utc')`, `datetime('2024-07-01 12:00 +02:00', 'localtime')`,
		`strftime('%H:%M %j', '2024-12-31 23:30', 'localtime')`, `date('0001-01-01', 'localtime')`, `datetime(1710288000, 'unixepoch', 'localtime')`,
	} {
		q = append(q, `SELECT `+e)
	}
	differ(t, "date local", q)
}
