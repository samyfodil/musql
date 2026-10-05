// mattn/go-sqlite3's one type coercion, reproduced behind a DSN flag.
//
// That driver inspects each result column's DECLARED type and, when it is
// exactly "timestamp", "datetime" or "date", hands database/sql a Go time.Time
// instead of the stored value (sqlite3.go for an INTEGER value, :2679 for a
// TEXT one, v1.14.48). This driver returns the value as STORED -- which is what
// modernc.org/sqlite does too, and what SQLite itself stores, having no date
// type at all -- so an application written against mattn that scans such a
// column straight into a time.Time meets the difference immediately:
// database/sql will not convert a string into one.
//
// # Why it is a flag and not the default
//
// Three reasons, in order of weight:
//
//   - it is that driver's INVENTION, not SQLite's, so making it the default
//     would diverge from the other pure-Go driver rather than converge on C;
//   - it is a BREAKING change for code that reads those columns as strings
//     today;
//   - mattn's TEXT arm returns the ZERO TIME when no format parses
//     (sqlite3.go), so a "datetime" column holding 'not a date' comes
//     back as 0001-01-01 rather than as an error. Inheriting that silently, for
//     everyone, is not a trade this driver should make on a user's behalf.
//
// With "?_time_decltype=1" the whole table below is reproduced EXACTLY,
// including the zero-time rule -- a half-compatible coercion would be worse than
// none. "?_loc=<name>" (or "auto" for time.Local) places the result, exactly as
// mattn's own _loc does.
package driver

import (
	"strings"
	"time"

	"github.com/samyfodil/musql/engine"
)

// declTypeTimeNames is mattn's set, matched EXACTLY after lower-casing --
// sqlite3.go's three constants, compared with ==. "DATETIME(3)",
// "TIMESTAMPTZ" and "TEXT" get nothing, which is why this is a set of three
// strings rather than a prefix test.
var declTypeTimeNames = map[string]bool{
	"date":      true,
	"datetime":  true,
	"timestamp": true,
}

// sqliteTimestampFormats is mattn's SQLiteTimestampFormats (sqlite3.go),
// in order -- the first one that parses wins.
var sqliteTimestampFormats = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
}

// declTypeTimeValue converts one value of a time-declared column, or reports
// ok=false to leave it exactly as stored.
//
// Only INTEGER and TEXT convert, which is mattn's own split: its SQLITE_FLOAT
// arm has no decltype switch at all (sqlite3.go), so a Julian-day REAL in a
// "datetime" column stays a float64 there and here, and BLOB and NULL are
// likewise untouched.
func declTypeTimeValue(v engine.Value, loc *time.Location) (time.Time, bool) {
	switch v.Typ {
	case engine.Int:
		// sqlite3.go: "Assume a millisecond unix timestamp if it's 13
		// digits -- too large to be a reasonable timestamp in seconds." The test
		// is on the MAGNITUDE, so a large negative is milliseconds too.
		var t time.Time
		if v.I > 1e12 || v.I < -1e12 {
			t = time.Unix(0, v.I*int64(time.Millisecond))
		} else {
			t = time.Unix(v.I, 0)
		}
		return placeTime(t.UTC(), loc), true
	case engine.Text:
		// sqlite3.go: strip a trailing "Z", try each format in order,
		// and fall back to the ZERO time -- not an error -- when none parses.
		s := strings.TrimSuffix(string(v.S), "Z")
		for _, f := range sqliteTimestampFormats {
			if tv, err := time.ParseInLocation(f, s, time.UTC); err == nil {
				return placeTime(tv, loc), true
			}
		}
		return placeTime(time.Time{}, loc), true
	}
	return time.Time{}, false
}

// placeTime applies the connection's _loc, which mattn does on both arms after
// the value is built (sqlite3.go, :2693-2695).
func placeTime(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		return t
	}
	return t.In(loc)
}

// timeDeclTypeCols asks the engine for the statement's result-column declared
// types and marks the ones this coercion applies to. It returns nil -- meaning
// "convert nothing" -- whenever the connection did not ask for the feature, or
// the types could not be derived, or they do not line up with the columns
// actually returned.
//
// The engine call happens ONLY when the flag is on, so a connection that does
// not use the feature pays nothing: ResultDeclTypes parses and resolves the
// statement a second time, which is a real cost and is the reason it is not
// computed for everyone.
func (c *Conn) timeDeclTypeCols(pager *engine.ReadOnlyPager, sqlText string, cols []string) []bool {
	if !c.timeDeclType || pager == nil || len(cols) == 0 {
		return nil
	}
	types, ok := pager.ResultDeclTypes(sqlText)
	if !ok || len(types) != len(cols) {
		return nil
	}
	var marks []bool
	for i, t := range types {
		if declTypeTimeNames[strings.ToLower(strings.TrimSpace(t))] {
			if marks == nil {
				marks = make([]bool, len(cols))
			}
			marks[i] = true
		}
	}
	return marks
}

// rowsWithDeclTypes is the *Rows every read path returns, with the time coercion
// wired in when this connection asked for it.
func (c *Conn) rowsWithDeclTypes(pager *engine.ReadOnlyPager, sqlText string, cols []string, rows [][]engine.Value) *Rows {
	r := &Rows{cols: cols, rows: rows, declTypes: c.reportedDeclTypes(pager, sqlText, cols)}
	if marks := c.timeDeclTypeCols(pager, sqlText, cols); marks != nil {
		r.timeCols, r.loc = marks, c.loc
	}
	return r
}
