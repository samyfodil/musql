// This file gates the date/time modifiers ("utc", "localtime", "auto")
// against C SQLite. Both engines read the same zoneinfo, so comparisons are exact.
package compat

import "testing"

// dtqCases are compared as VALUES: a modifier that silently did nothing would
// still "succeed", so agreeing on the answer is the whole test.
var dtqCases = []string{
	// "now" tests use equality comparisons to pin exact behavior across engines.
	`SELECT datetime('now','utc') = datetime('now')`,
	`SELECT datetime('now','utc','utc') = datetime('now')`,
	`SELECT datetime('now','localtime','utc') = datetime('now')`,
	`SELECT datetime('now','utc','localtime') = datetime('now','localtime')`,
	`SELECT datetime('now','+0 days','utc') = datetime('now')`,
	`SELECT date('now','utc') = date('now')`,
	`SELECT time('now','utc') = time('now')`,
	`SELECT datetime('now','localtime','localtime') = datetime('now','localtime')`,
	// ...and "localtime" really does SHIFT by the zone offset, which an
	// equality alone would not catch.
	`SELECT CAST(round((julianday('now','localtime') - julianday('now'))*86400) AS INT)`,
	`SELECT CAST(round((julianday('now','utc') - julianday('now'))*86400) AS INT)`,
	// repeated modifiers: each is a no-op only when the value is ALREADY in
	// its target zone, which is why one boolean cannot model this
	`SELECT datetime('2020-01-02 12:00:00','localtime','localtime')`,
	`SELECT datetime('2020-01-02 12:00:00','utc','utc')`,
	`SELECT datetime('2020-01-02 12:00:00','utc','localtime')`,
	`SELECT datetime('2020-01-02 12:00:00','localtime','utc')`,
	`SELECT datetime('2020-01-02 12:00:00','localtime','+1 day','localtime')`,
	`SELECT datetime('now','localtime','localtime','localtime') = datetime('now','localtime')`,
	// ...while a NUMBER does not carry a zone, so "utc" converts
	`SELECT datetime(2451545.0,'utc')`,
	`SELECT datetime(2451545.0,'localtime')`,
	// the plain conversions, in both directions and both seasons
	`SELECT datetime('2020-01-02 03:04:05','utc')`,
	`SELECT datetime('2020-01-02 03:04:05','localtime')`,
	`SELECT datetime('2020-07-01 12:00:00','utc')`,
	`SELECT datetime('2020-07-01 12:00:00','localtime')`,
	`SELECT datetime('2020-01-01 12:00:00','utc')`,
	`SELECT datetime('2020-01-01 12:00:00','localtime')`,
	`SELECT date('2020-01-02','utc')`,
	`SELECT date('2020-01-02','localtime')`,
	// a round trip must land back where it started
	`SELECT datetime('2020-01-02','utc','localtime')`,
	`SELECT datetime('2020-01-02','localtime','utc')`,
	`SELECT datetime('2020-07-15 08:00:00','localtime','utc')`,
	// SPRING FORWARD: 02:00-02:59 does not exist on this day
	`SELECT datetime('2020-03-08 00:30:00','utc')`,
	`SELECT datetime('2020-03-08 01:30:00','utc')`,
	`SELECT datetime('2020-03-08 01:59:59','utc')`,
	`SELECT datetime('2020-03-08 02:00:00','utc')`,
	`SELECT datetime('2020-03-08 02:30:00','utc')`,
	`SELECT datetime('2020-03-08 02:59:59','utc')`,
	`SELECT datetime('2020-03-08 03:00:00','utc')`,
	`SELECT datetime('2020-03-08 03:30:00','utc')`,
	`SELECT datetime('2020-03-08 07:30:00','localtime')`,
	`SELECT datetime('2020-03-08 08:30:00','localtime')`,
	// FALL BACK: 01:00-01:59 happens twice
	`SELECT datetime('2020-11-01 00:30:00','utc')`,
	`SELECT datetime('2020-11-01 01:30:00','utc')`,
	`SELECT datetime('2020-11-01 02:30:00','utc')`,
	`SELECT datetime('2020-11-01 05:30:00','localtime')`,
	`SELECT datetime('2020-11-01 06:30:00','localtime')`,
	// the other functions in the family take the same modifiers
	`SELECT julianday('2020-01-02','utc')`,
	`SELECT julianday('2020-01-02','localtime')`,
	`SELECT unixepoch('2020-01-02','utc')`,
	`SELECT unixepoch('2020-01-02','localtime')`,
	`SELECT strftime('%Y-%m-%d %H:%M','2020-06-15 12:00','utc')`,
	`SELECT strftime('%s','2020-01-02','utc')`,
	// combined with an arithmetic modifier, in both orders
	`SELECT datetime('2020-01-02','+1 day','utc')`,
	`SELECT datetime('2020-01-02','utc','+1 day')`,
	`SELECT datetime('2020-03-07','+1 day','utc')`,
	// "auto": a numeric argument is a julian day inside the range and unix
	// seconds outside it; the boundary is compared directly
	`SELECT datetime(2451545.0,'auto')`,
	`SELECT datetime(5373484,'auto')`,
	`SELECT datetime(5373485,'auto')`,
	`SELECT datetime(1577944800,'auto')`,
	`SELECT datetime(0,'auto')`,
	`SELECT datetime(-1000,'auto')`,
	`SELECT date(1710288000,'auto')`,
	// ...and a NON-numeric argument, which "auto" leaves alone
	`SELECT datetime('2020-01-02','auto')`,
	`SELECT datetime('2020-01-02 03:04:05','auto')`,
	// epoch and 32-bit boundaries
	`SELECT datetime('1970-01-01','localtime')`,
	`SELECT datetime('1970-01-01','utc')`,
	`SELECT datetime('2038-01-19 03:14:07','utc')`,
	`SELECT datetime('2038-01-19 03:14:08','localtime')`,
	// NULL and nonsense arguments must still behave
	`SELECT datetime(NULL,'utc')`,
	`SELECT datetime('2020-01-02',NULL)`,
	`SELECT datetime('nonsense','utc')`,
	`SELECT typeof(datetime('2020-01-02','utc'))`,
}

func TestDateTimeZoneModifiers(t *testing.T) {
	for _, q := range dtqCases {
		differ(t, q, []string{q})
	}
}

// TestDateTimeZoneModifiersInTable runs them over stored values too, so the
// modifiers are exercised through the VDBE's expression path rather than only
// as constant folding on a bare SELECT.
func TestDateTimeZoneModifiersInTable(t *testing.T) {
	setup := []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT, n REAL)`,
		`INSERT INTO t VALUES
		   (1,'2020-01-02 03:04:05', 2451545.0),
		   (2,'2020-07-01 12:00:00', 1577944800),
		   (3,'2020-03-08 02:30:00', 0),
		   (4,'2020-11-01 01:30:00', -1000),
		   (5,NULL, NULL)`,
	}
	for _, q := range []string{
		`SELECT id, datetime(s,'utc') FROM t ORDER BY id`,
		`SELECT id, datetime(s,'localtime') FROM t ORDER BY id`,
		`SELECT id, datetime(s,'utc','localtime') FROM t ORDER BY id`,
		`SELECT id, datetime(n,'auto') FROM t ORDER BY id`,
		`SELECT id, julianday(s,'utc') FROM t ORDER BY id`,
		`SELECT id, unixepoch(s,'localtime') FROM t ORDER BY id`,
		`SELECT count(*) FROM t WHERE datetime(s,'utc') > '2020-01-01'`,
	} {
		differ(t, q, append(append([]string{}, setup...), q))
	}
}
