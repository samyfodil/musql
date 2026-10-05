// This file is SQLite's date and time functions -- date(), time(),
// datetime(), julianday(), unixepoch() and strftime() -- ported from date.c
// (3.53.3) onto date.c's own DateTime model, function for function.
//
// The model matters, not only the arithmetic. A DateTime keeps Y/M/D and
// h/m/s as PARSED until something needs the julian day, so
// "2024-01-01 24:00:00" renders back as written, "start of month" over
// "2024-02-30" lands in February, and a negative year survives a modifier that
// brings it back into range. The millisecond-only representation this file
// used to hold normalized all three away, and each was a wrong answer.
//
// "now" is the wall clock at the call; C reads sqlite3StmtCurrentTime, which
// is fixed for one statement, so two calls straddling a millisecond inside one
// statement can differ here where they cannot there.
package engine

import (
	"fmt"
	"strings"
	"time"
)

// sqlDateTime is DateTime, date.c:48.
type sqlDateTime struct {
	iJD                         int64 // julian day number times 86400000
	Y, M, D, h, m, tz           int
	s                           float64
	validJD, validYMD, validHMS bool
	nFloor                      int  // days to implement "floor"
	rawS                        bool // raw numeric value stored in s
	isError                     bool
	useSubsec                   bool
	isUtc, isLocal              bool

	// usedNow records that computing this value CONSULTED the wall clock or
	// the local time zone -- the four points C guards with
	// sqlite3NotPureFunc (date.c:430/437/810/837/1118). It has no effect on
	// the value; it is what pureFuncGuard turns into C SQLite's
	// "non-deterministic use of %s() in %s" (vdbeaux.c:5643) when the call
	// sits inside a CHECK constraint, a generated column or an index.
	usedNow bool
}

// jdUnixEpoch is iJD at 1970-01-01 00:00:00, 21086676*10000*1000
// (date.c:1187).
const jdUnixEpoch = 210866760000000

// dateGetDigits is getDigits, date.c:112: format holds one 4-byte spec per
// integer -- digit count, minimum, maximum letter, separator -- the last one
// 3 bytes. It returns how many conversions succeeded.
func dateGetDigits(z []byte, i int, format string, out ...*int) int {
	maxOf := [...]int{12, 14, 24, 31, 59, 14712}
	cnt := 0
	for f := 0; f+3 <= len(format); f += 4 {
		n := int(format[f] - '0')
		min := int(format[f+1] - '0')
		max := maxOf[format[f+2]-'a']
		var nextC byte
		if f+3 < len(format) {
			nextC = format[f+3]
		}
		val := 0
		for ; n > 0; n-- {
			if !sqlIsDigit(byteAt(z, i)) {
				return cnt
			}
			val = val*10 + int(byteAt(z, i)-'0')
			i++
		}
		if val < min || val > max || (nextC != 0 && nextC != byteAt(z, i)) {
			return cnt
		}
		*out[cnt] = val
		i++
		cnt++
		if nextC == 0 {
			break
		}
	}
	return cnt
}

// parseTimezone is date.c:166. Returns true on a parse error.
func (p *sqlDateTime) parseTimezone(z []byte, i int) bool {
	for sqlIsSpace(byteAt(z, i)) {
		i++
	}
	p.tz = 0
	sgn := 0
	switch c := byteAt(z, i); c {
	case '-':
		sgn = -1
	case '+':
		sgn = 1
	case 'Z', 'z':
		i++
		p.isLocal = false
		p.isUtc = true
		for sqlIsSpace(byteAt(z, i)) {
			i++
		}
		return byteAt(z, i) != 0
	default:
		return c != 0
	}
	i++
	var nHr, nMn int
	if dateGetDigits(z, i, "20b:20e", &nHr, &nMn) != 2 {
		return true
	}
	i += 5
	p.tz = sgn * (nMn + nHr*60)
	if p.tz == 0 {
		p.isLocal = false
		p.isUtc = true
	}
	for sqlIsSpace(byteAt(z, i)) {
		i++
	}
	return byteAt(z, i) != 0
}

// parseHhMmSs is date.c:207. Returns true on a parse error.
func (p *sqlDateTime) parseHhMmSs(z []byte, i int) bool {
	var h, m, s int
	if dateGetDigits(z, i, "20c:20e", &h, &m) != 2 {
		return true
	}
	i += 5
	ms := 0.0
	if byteAt(z, i) == ':' {
		i++
		if dateGetDigits(z, i, "20e", &s) != 1 {
			return true
		}
		i += 2
		if byteAt(z, i) == '.' && sqlIsDigit(byteAt(z, i+1)) {
			rScale := 1.0
			i++
			for sqlIsDigit(byteAt(z, i)) {
				ms = ms*10.0 + float64(byteAt(z, i)-'0')
				rScale *= 10.0
				i++
			}
			ms /= rScale
			// Truncate to avoid problems with sub-milliseconds rounding.
			if ms > 0.999 {
				ms = 0.999
			}
		}
	}
	p.validJD = false
	p.rawS = false
	p.validHMS = true
	p.h = h
	p.m = m
	p.s = float64(s) + ms
	return p.parseTimezone(z, i)
}

// errorOut is datetimeError, date.c:249.
func (p *sqlDateTime) errorOut() { *p = sqlDateTime{isError: true} }

// computeJD is date.c:260.
func (p *sqlDateTime) computeJD() {
	if p.validJD {
		return
	}
	Y, M, D := 2000, 1, 1
	if p.validYMD {
		Y, M, D = p.Y, p.M, p.D
	}
	if Y < -4713 || Y > 9999 || p.rawS {
		p.errorOut()
		return
	}
	if M <= 2 {
		Y--
		M += 12
	}
	A := (Y + 4800) / 100
	B := 38 - A + (A / 4)
	X1 := 36525 * (Y + 4716) / 100
	X2 := 306001 * (M + 1) / 10000
	p.iJD = int64((float64(X1+X2+D+B) - 1524.5) * 86400000)
	p.validJD = true
	if p.validHMS {
		p.iJD += int64(p.h*3600000+p.m*60000) + int64(p.s*1000+0.5)
		if p.tz != 0 {
			p.iJD -= int64(p.tz) * 60000
			p.validYMD = false
			p.validHMS = false
			p.tz = 0
			p.isUtc = true
			p.isLocal = false
		}
	}
}

// computeFloor is date.c:306.
func (p *sqlDateTime) computeFloor() {
	switch {
	case p.D <= 28:
		p.nFloor = 0
	case (1<<uint(p.M))&0x15aa != 0:
		p.nFloor = 0
	case p.M != 2:
		p.nFloor = 0
		if p.D == 31 {
			p.nFloor = 1
		}
	case p.Y%4 != 0 || (p.Y%100 == 0 && p.Y%400 != 0):
		p.nFloor = p.D - 28
	default:
		p.nFloor = p.D - 29
	}
}

// parseYyyyMmDd is date.c:335. Returns true on a parse error.
func (p *sqlDateTime) parseYyyyMmDd(z []byte) bool {
	i := 0
	neg := false
	if byteAt(z, 0) == '-' {
		i++
		neg = true
	}
	var Y, M, D int
	if dateGetDigits(z, i, "40f-21a-21d", &Y, &M, &D) != 3 {
		return true
	}
	i += 10
	for sqlIsSpace(byteAt(z, i)) || byteAt(z, i) == 'T' {
		i++
	}
	switch {
	case !p.parseHhMmSs(z, i):
		// We got the time
	case byteAt(z, i) == 0:
		p.validHMS = false
	default:
		return true
	}
	p.validJD = false
	p.validYMD = true
	p.Y = Y
	if neg {
		p.Y = -Y
	}
	p.M = M
	p.D = D
	p.computeFloor()
	if p.tz != 0 {
		p.computeJD()
	}
	return false
}

// setDateTimeToCurrent is date.c:376.
func (p *sqlDateTime) setDateTimeToCurrent() {
	p.iJD = time.Now().UnixMilli() + jdUnixEpoch
	p.validJD = true
	p.isUtc = true
	p.isLocal = false
	p.clearYMDHMSTZ()
	// See pureFuncGuard: reading the wall clock is what makes the call
	// non-deterministic, and C guards exactly this point (date.c:430 for
	// "now", date.c:437 for "subsec", date.c:1118 for the no-argument form).
	p.usedNow = true
}

// setRawDateNumber is date.c:395.
func (p *sqlDateTime) setRawDateNumber(r float64) {
	p.s = r
	p.rawS = true
	if r >= 0.0 && r < 5373484.5 {
		p.iJD = int64(r*86400000.0 + 0.5)
		p.validJD = true
	}
}

// parseDateOrTime is date.c:420. Returns true on a parse error.
func (p *sqlDateTime) parseDateOrTime(z []byte) bool {
	// A failed attempt can leave fields set, which is harmless in C only
	// because every later success overwrites what matters; starting each
	// attempt clean is the same outcome.
	if !p.parseYyyyMmDd(z) {
		return false
	}
	*p = sqlDateTime{}
	if !p.parseHhMmSs(z, 0) {
		return false
	}
	*p = sqlDateTime{}
	word := string(z)
	if strings.EqualFold(word, "now") {
		p.setDateTimeToCurrent()
		return false
	}
	if r, rc := sqliteAtoF(z); rc > 0 {
		p.setRawDateNumber(r)
		return false
	}
	if strings.EqualFold(word, "subsec") || strings.EqualFold(word, "subsecond") {
		p.useSubsec = true
		p.setDateTimeToCurrent()
		return false
	}
	return true
}

// validJulianDay is date.c:458: 9999-12-31 23:59:59.999 is the upper bound.
func validJulianDay(iJD int64) bool {
	return iJD >= 0 && iJD <= 464269060799999
}

// computeYMD is date.c:465.
func (p *sqlDateTime) computeYMD() {
	if p.validYMD {
		return
	}
	switch {
	case !p.validJD:
		p.Y, p.M, p.D = 2000, 1, 1
	case !validJulianDay(p.iJD):
		p.errorOut()
		return
	default:
		Z := int((p.iJD + 43200000) / 86400000)
		alpha := int((float64(Z)+32044.75)/36524.25) - 52
		A := Z + 1 + alpha - ((alpha + 100) / 4) + 25
		B := A + 1524
		C := int((float64(B) - 122.1) / 365.25)
		D := (36525 * (C & 32767)) / 100
		E := int(float64(B-D) / 30.6001)
		X1 := int(30.6001 * float64(E))
		p.D = B - D - X1
		if E < 14 {
			p.M = E - 1
		} else {
			p.M = E - 13
		}
		if p.M > 2 {
			p.Y = C - 4716
		} else {
			p.Y = C - 4715
		}
	}
	p.validYMD = true
}

// computeHMS is date.c:494.
func (p *sqlDateTime) computeHMS() {
	if p.validHMS {
		return
	}
	p.computeJD()
	dayMs := int((p.iJD + 43200000) % 86400000)
	p.s = float64(dayMs%60000) / 1000.0
	dayMin := dayMs / 60000
	p.m = dayMin % 60
	p.h = dayMin / 60
	p.rawS = false
	p.validHMS = true
}

// computeYMDHMS is computeYMD_HMS, date.c:510.
func (p *sqlDateTime) computeYMDHMS() {
	p.computeYMD()
	p.computeHMS()
}

// clearYMDHMSTZ is clearYMD_HMS_TZ, date.c:518.
func (p *sqlDateTime) clearYMDHMSTZ() {
	p.validYMD = false
	p.validHMS = false
	p.tz = 0
}

// toLocaltime is date.c:608, over the process's local zone -- the zone
// localtime_r reads in C, which the oracle shares with this engine when both
// run in one process.
func (p *sqlDateTime) toLocaltime() {
	p.computeJD()
	var t int64
	iYearDiff := 0
	if p.iJD < 2108667600*100000 || p.iJD > 2130141456*100000 {
		// localtime_r only works for 1970-2037 on some platforms, so SQLite
		// maps the year into that range, converts, and maps it back.
		x := *p
		x.computeYMDHMS()
		iYearDiff = (2000 + x.Y%4) - x.Y
		x.Y += iYearDiff
		x.validJD = false
		x.computeJD()
		t = x.iJD/1000 - 21086676*10000
	} else {
		t = p.iJD/1000 - 21086676*10000
	}
	lt := time.Unix(t, 0).In(time.Local)
	p.Y = lt.Year() - iYearDiff
	p.M = int(lt.Month())
	p.D = lt.Day()
	p.h = lt.Hour()
	p.m = lt.Minute()
	p.s = float64(lt.Second()) + float64(p.iJD%1000)*0.001
	p.validYMD = true
	p.validHMS = true
	p.validJD = false
	p.rawS = false
	p.tz = 0
	p.isError = false
}

// dateXforms is aXformType, date.c:667.
var dateXforms = [...]struct {
	name  string
	limit float64
	xform float64
}{
	{"second", 4.6427e+14, 1.0},
	{"minute", 7.7379e+12, 60.0},
	{"hour", 1.2897e+11, 3600.0},
	{"day", 5373485.0, 86400.0},
	{"month", 176546.0, 2592000.0},
	{"year", 14713.0, 31536000.0},
}

// autoAdjustDate is date.c:686.
func (p *sqlDateTime) autoAdjustDate() {
	if !p.rawS || p.validJD {
		p.rawS = false
		return
	}
	if p.s >= -21086676*10000 && p.s <= 25340230*10000+799 {
		r := p.s*1000.0 + 210866760000000.0
		p.clearYMDHMSTZ()
		p.iJD = int64(r + 0.5)
		p.validJD = true
		p.rawS = false
	}
}

// parseModifier is date.c:730: false on any error, which every caller turns
// into a NULL result. idx is the modifier's argument index (the time value is
// 0).
func (p *sqlDateTime) parseModifier(z []byte, idx int) bool {
	n := len(z)
	word := string(z)
	switch toLowerASCII(byteAt(z, 0)) {
	case 'a':
		if strings.EqualFold(word, "auto") {
			if idx > 1 {
				return false
			}
			p.autoAdjustDate()
			return true
		}
	case 'c':
		if strings.EqualFold(word, "ceiling") {
			p.computeJD()
			p.clearYMDHMSTZ()
			p.nFloor = 0
			return true
		}
	case 'f':
		if strings.EqualFold(word, "floor") {
			p.computeJD()
			p.iJD -= int64(p.nFloor) * 86400000
			p.clearYMDHMSTZ()
			return true
		}
	case 'j':
		if strings.EqualFold(word, "julianday") {
			if idx > 1 {
				return false
			}
			if p.validJD && p.rawS {
				p.rawS = false
				return true
			}
		}
	case 'l':
		if strings.EqualFold(word, "localtime") {
			if !p.isLocal {
				p.toLocaltime()
			}
			p.isUtc = false
			p.isLocal = true
			// date.c:810's own sqlite3NotPureFunc point: the local zone is
			// process/environment state, not a property of the value.
			p.usedNow = true
			return true
		}
	case 'u':
		if strings.EqualFold(word, "unixepoch") && p.rawS {
			if idx > 1 {
				return false
			}
			r := p.s*1000.0 + 210866760000000.0
			if r >= 0.0 && r < 464269060800000.0 {
				p.clearYMDHMSTZ()
				p.iJD = int64(r + 0.5)
				p.validJD = true
				p.rawS = false
				return true
			}
		} else if strings.EqualFold(word, "utc") {
			// date.c:837's own sqlite3NotPureFunc point. Recorded around the
			// wholesale "*p = sqlDateTime{...}" below, which would drop it.
			usedNow := p.usedNow || !p.isUtc
			if !p.isUtc {
				p.computeJD()
				iOrigJD := p.iJD
				iGuess := iOrigJD
				var iErr int64
				for cnt := 0; ; cnt++ {
					iGuess -= iErr
					nx := sqlDateTime{iJD: iGuess, validJD: true}
					nx.toLocaltime()
					nx.computeJD()
					iErr = nx.iJD - iOrigJD
					if iErr == 0 || cnt >= 3 {
						break
					}
				}
				*p = sqlDateTime{iJD: iGuess, validJD: true, isUtc: true}
			}
			p.usedNow = usedNow
			return true
		}
	case 'w':
		if n > 8 && strings.EqualFold(word[:8], "weekday ") {
			r, rc := sqliteAtoF(z[8:])
			if rc > 0 && r >= 0.0 && r < 7.0 && float64(int(r)) == r {
				wd := int64(r)
				p.computeYMDHMS()
				p.tz = 0
				p.validJD = false
				p.computeJD()
				Z := ((p.iJD + 129600000) / 86400000) % 7
				if Z > wd {
					Z -= 7
				}
				p.iJD += (wd - Z) * 86400000
				p.clearYMDHMSTZ()
				return true
			}
		}
	case 's':
		if n < 9 || !strings.EqualFold(word[:9], "start of ") {
			if strings.EqualFold(word, "subsec") || strings.EqualFold(word, "subsecond") {
				p.useSubsec = true
				return true
			}
			return false
		}
		if !p.validJD && !p.validYMD && !p.validHMS {
			return false
		}
		p.computeYMD()
		p.validHMS = true
		p.h, p.m = 0, 0
		p.s = 0.0
		p.rawS = false
		p.tz = 0
		p.validJD = false
		switch unit := word[9:]; {
		case strings.EqualFold(unit, "month"):
			p.D = 1
			return true
		case strings.EqualFold(unit, "year"):
			p.M = 1
			p.D = 1
			return true
		case strings.EqualFold(unit, "day"):
			return true
		}
	case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.parseNumericModifier(z)
	}
	return false
}

// parseNumericModifier is parseModifier's '+', '-' and digit arm,
// date.c:936-1100.
func (p *sqlDateTime) parseNumericModifier(z []byte) bool {
	z0 := z[0]
	var n int
	var Y, M, D, h, m int
	for n = 1; n < len(z); n++ {
		if z[n] == ':' || sqlIsSpace(z[n]) {
			break
		}
		if z[n] == '-' {
			if n == 5 && dateGetDigits(z, 1, "40f", &Y) == 1 {
				break
			}
			if n == 6 && dateGetDigits(z, 1, "50f", &Y) == 1 {
				break
			}
		}
	}
	r, rc := sqliteAtoF(z[:n])
	if rc <= 0 {
		return false
	}
	z2 := z
	if byteAt(z, n) == '-' {
		// (+|-)YYYY-MM-DD adds or subtracts years, months and days; MM is
		// limited to 0-11 and DD to 0-30.
		if z0 != '+' && z0 != '-' {
			return false
		}
		if n == 5 {
			if dateGetDigits(z, 1, "40f-20a-20d", &Y, &M, &D) != 3 {
				return false
			}
		} else {
			if dateGetDigits(z, 1, "50f-20a-20d", &Y, &M, &D) != 3 {
				return false
			}
			z = z[1:]
		}
		if M >= 12 || D >= 31 {
			return false
		}
		p.computeYMDHMS()
		p.validJD = false
		if z0 == '-' {
			p.Y -= Y
			p.M -= M
			D = -D
		} else {
			p.Y += Y
			p.M += M
		}
		x := (p.M - 12) / 12
		if p.M > 0 {
			x = (p.M - 1) / 12
		}
		p.Y += x
		p.M -= x * 12
		p.computeFloor()
		p.computeJD()
		p.validHMS = false
		p.validYMD = false
		p.iJD += int64(D) * 86400000
		if byteAt(z, 11) == 0 {
			return true
		}
		if sqlIsSpace(byteAt(z, 11)) && dateGetDigits(z, 12, "20c:20e", &h, &m) == 2 {
			z2 = z[12:]
			n = 2
		} else {
			return false
		}
	}
	if byteAt(z2, n) == ':' {
		// (+|-)HH:MM:SS.FFF adds or subtracts hours, minutes, seconds and
		// fractional seconds; ".FFF" and ":SS.FFF" may be omitted.
		if !sqlIsDigit(byteAt(z2, 0)) {
			z2 = z2[1:]
		}
		var tx sqlDateTime
		if tx.parseHhMmSs(z2, 0) {
			return false
		}
		tx.computeJD()
		tx.iJD -= 43200000
		day := tx.iJD / 86400000
		tx.iJD -= day * 86400000
		if z0 == '-' {
			tx.iJD = -tx.iJD
		}
		p.computeJD()
		p.clearYMDHMSTZ()
		p.iJD += tx.iJD
		return true
	}

	// "+NNN days" and the like.
	rest := z[n:]
	for len(rest) > 0 && sqlIsSpace(rest[0]) {
		rest = rest[1:]
	}
	unit := string(rest)
	if len(unit) < 3 || len(unit) > 10 {
		return false
	}
	if toLowerASCII(unit[len(unit)-1]) == 's' {
		unit = unit[:len(unit)-1]
	}
	p.computeJD()
	rRounder := 0.5
	if r < 0 {
		rRounder = -0.5
	}
	p.nFloor = 0
	ok := false
	for i, xf := range dateXforms {
		if !strings.EqualFold(xf.name, unit) || r <= -xf.limit || r >= xf.limit {
			continue
		}
		switch i {
		case 4: // months
			p.computeYMDHMS()
			p.M += int(r)
			x := (p.M - 12) / 12
			if p.M > 0 {
				x = (p.M - 1) / 12
			}
			p.Y += x
			p.M -= x * 12
			p.computeFloor()
			p.validJD = false
			r -= float64(int(r))
		case 5: // years
			p.computeYMDHMS()
			p.Y += int(r)
			p.computeFloor()
			p.validJD = false
			r -= float64(int(r))
		}
		p.computeJD()
		p.iJD += int64(r*1000.0*xf.xform + rRounder)
		ok = true
		break
	}
	p.clearYMDHMSTZ()
	return ok
}

// toLowerASCII is sqlite3UpperToLower for one byte.
func toLowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// dateArgText is sqlite3_value_text for a date function argument: nil for
// NULL, and a C string, so an embedded NUL ends it.
func dateArgText(v Value) []byte {
	if v.Typ == Null {
		return nil
	}
	return []byte(textBeforeNUL(valueToText(v)))
}

// isDate is date.c:1107: args[0] is the time value ("now" when there are no
// arguments), the rest modifiers. ok is false for the NULL result.
func isDate(args []Value) (p sqlDateTime, ok bool) {
	if len(args) == 0 {
		p.setDateTimeToCurrent()
		return p, true
	}
	switch args[0].Typ {
	case Int:
		p.setRawDateNumber(float64(args[0].I))
	case Float:
		p.setRawDateNumber(args[0].F)
	default:
		z := dateArgText(args[0])
		if z == nil || p.parseDateOrTime(z) {
			return p, false
		}
	}
	for i := 1; i < len(args); i++ {
		z := dateArgText(args[i])
		if z == nil || !p.parseModifier(z, i) {
			return p, false
		}
	}
	p.computeJD()
	if p.isError || !validJulianDay(p.iJD) {
		return p, false
	}
	if len(args) == 1 && p.validYMD && p.D > 28 {
		// Make sure a YYYY-MM-DD is normalized: 2023-02-31 -> 2023-03-03.
		p.validYMD = false
	}
	return p, true
}

// fnDateTimeFamily is juliandayFunc, unixepochFunc, datetimeFunc, timeFunc
// and dateFunc (date.c:1157-1336), selected by kind.
// usedNow reports whether computing the answer consulted the wall clock or
// the local zone, which is what pureFuncGuard turns into C's OP_PureFunc
// error inside a CHECK constraint, a generated column or an index.
func fnDateTimeFamily(kind string, args []Value) (v Value, usedNow bool, err error) {
	x, ok := isDate(args)
	if !ok {
		return Value{Typ: Null}, x.usedNow, nil
	}
	switch kind {
	case "julianday":
		x.computeJD()
		return Value{Typ: Float, F: float64(x.iJD) / 86400000.0}, x.usedNow, nil
	case "unixepoch":
		x.computeJD()
		if x.useSubsec {
			return Value{Typ: Float, F: float64(x.iJD-jdUnixEpoch) / 1000.0}, x.usedNow, nil
		}
		return Value{Typ: Int, I: x.iJD/1000 - jdUnixEpoch/1000}, x.usedNow, nil
	case "datetime":
		x.computeYMDHMS()
		return Value{Typ: Text, S: []byte(dateText(x) + " " + timeText(x))}, x.usedNow, nil
	case "time":
		x.computeHMS()
		return Value{Typ: Text, S: []byte(timeText(x))}, x.usedNow, nil
	case "date":
		x.computeYMD()
		return Value{Typ: Text, S: []byte(dateText(x))}, x.usedNow, nil
	}
	return Value{}, false, fmt.Errorf("engine: internal: unknown date/time function kind %q", kind)
}

// dateText is dateFunc's YYYY-MM-DD (date.c:1308-1333): four digits of |Y|,
// with a '-' in front of a negative year.
func dateText(x sqlDateTime) string {
	Y := x.Y
	sign := ""
	if Y < 0 {
		Y, sign = -Y, "-"
	}
	return fmt.Sprintf("%s%d%d%d%d-%d%d-%d%d", sign, (Y/1000)%10, (Y/100)%10, (Y/10)%10, Y%10,
		(x.M/10)%10, x.M%10, (x.D/10)%10, x.D%10)
}

// timeText is timeFunc's HH:MM:SS[.SSS] (date.c:1266-1293).
func timeText(x sqlDateTime) string {
	hm := fmt.Sprintf("%d%d:%d%d:", (x.h/10)%10, x.h%10, (x.m/10)%10, x.m%10)
	if x.useSubsec {
		s := int(1000.0*x.s + 0.5)
		return hm + fmt.Sprintf("%d%d.%d%d%d", (s/10000)%10, (s/1000)%10, (s/100)%10, (s/10)%10, s%10)
	}
	s := int(x.s)
	return hm + fmt.Sprintf("%d%d", (s/10)%10, s%10)
}

// daysAfterJan01 is date.c:1339.
func daysAfterJan01(x sqlDateTime) int {
	jan01 := x
	jan01.validJD = false
	jan01.M = 1
	jan01.D = 1
	jan01.computeJD()
	return int((x.iJD - jan01.iJD + 43200000) / 86400000)
}

// daysAfterMonday is date.c:1359.
func daysAfterMonday(x sqlDateTime) int { return int(((x.iJD + 43200000) / 86400000) % 7) }

// daysAfterSunday is date.c:1372.
func daysAfterSunday(x sqlDateTime) int { return int(((x.iJD + 129600000) / 86400000) % 7) }

// fnStrftime is strftimeFunc, date.c:1410. Every numeric conversion goes
// through the printf port, as sqlite3_str_appendf does.
func fnStrftime(args []Value) (Value, bool, error) {
	if len(args) == 0 {
		return Value{Typ: Null}, false, nil
	}
	zFmt := dateArgText(args[0])
	if zFmt == nil {
		return Value{Typ: Null}, false, nil
	}
	x, ok := isDate(args[1:])
	if !ok {
		return Value{Typ: Null}, x.usedNow, nil
	}
	x.computeJD()
	x.computeYMDHMS()
	var b strings.Builder
	appendf := func(format string, vals ...Value) error {
		v, err := fnPrintf(format, vals)
		if err != nil {
			return err
		}
		b.Write(v.S)
		return nil
	}
	intv := func(i int) Value { return Value{Typ: Int, I: int64(i)} }
	j := 0
	i := 0
	for ; i < len(zFmt); i++ {
		if zFmt[i] != '%' {
			continue
		}
		if j < i {
			b.Write(zFmt[j:i])
		}
		i++
		j = i + 1
		cf := byteAt(zFmt, i)
		var err error
		switch cf {
		case 'd', 'e':
			f := "%02d"
			if cf == 'e' {
				f = "%2d"
			}
			err = appendf(f, intv(x.D))
		case 'f':
			s := x.s
			if s > 59.999 {
				s = 59.999
			}
			err = appendf("%06.3f", Value{Typ: Float, F: s})
		case 'F':
			err = appendf("%04d-%02d-%02d", intv(x.Y), intv(x.M), intv(x.D))
		case 'G', 'g':
			y := x
			y.iJD += int64(3-daysAfterMonday(x)) * 86400000
			y.validYMD = false
			y.computeYMD()
			if cf == 'g' {
				err = appendf("%02d", intv(y.Y%100))
			} else {
				err = appendf("%04d", intv(y.Y))
			}
		case 'H', 'k':
			f := "%02d"
			if cf == 'k' {
				f = "%2d"
			}
			err = appendf(f, intv(x.h))
		case 'I', 'l':
			h := x.h
			if h > 12 {
				h -= 12
			}
			if h == 0 {
				h = 12
			}
			f := "%02d"
			if cf == 'l' {
				f = "%2d"
			}
			err = appendf(f, intv(h))
		case 'j':
			err = appendf("%03d", intv(daysAfterJan01(x)+1))
		case 'J':
			err = appendf("%.16g", Value{Typ: Float, F: float64(x.iJD) / 86400000.0})
		case 'm':
			err = appendf("%02d", intv(x.M))
		case 'M':
			err = appendf("%02d", intv(x.m))
		case 'p', 'P':
			switch {
			case x.h >= 12 && cf == 'p':
				b.WriteString("PM")
			case x.h >= 12:
				b.WriteString("pm")
			case cf == 'p':
				b.WriteString("AM")
			default:
				b.WriteString("am")
			}
		case 'R':
			err = appendf("%02d:%02d", intv(x.h), intv(x.m))
		case 's':
			if x.useSubsec {
				err = appendf("%.3f", Value{Typ: Float, F: float64(x.iJD-jdUnixEpoch) / 1000.0})
			} else {
				err = appendf("%lld", Value{Typ: Int, I: x.iJD/1000 - jdUnixEpoch/1000})
			}
		case 'S':
			err = appendf("%02d", intv(int(x.s)))
		case 'T':
			err = appendf("%02d:%02d:%02d", intv(x.h), intv(x.m), intv(int(x.s)))
		case 'u', 'w':
			c := byte(daysAfterSunday(x)) + '0'
			if c == '0' && cf == 'u' {
				c = '7'
			}
			b.WriteByte(c)
		case 'U':
			err = appendf("%02d", intv((daysAfterJan01(x)-daysAfterSunday(x)+7)/7))
		case 'V':
			y := x
			y.iJD += int64(3-daysAfterMonday(x)) * 86400000
			y.validYMD = false
			y.computeYMD()
			err = appendf("%02d", intv(daysAfterJan01(y)/7+1))
		case 'W':
			err = appendf("%02d", intv((daysAfterJan01(x)-daysAfterMonday(x)+7)/7))
		case 'Y':
			err = appendf("%04d", intv(x.Y))
		case '%':
			b.WriteByte('%')
		default:
			// An unknown conversion -- or a '%' ending the format -- resets
			// the result, which is NULL.
			return Value{Typ: Null}, x.usedNow, nil
		}
		if err != nil {
			return Value{}, x.usedNow, err
		}
	}
	if j < i {
		b.Write(zFmt[j:])
	}
	return Value{Typ: Text, S: []byte(b.String())}, x.usedNow, nil
}

// pureFuncError is the sentinel a date/time function's own result carries
// when it consulted the wall clock or the local zone: sqlDateTime.usedNow.
// It is threaded out of the value rather than raised inside the date code so
// the five entry points share ONE rule and the date functions themselves stay
// exactly the ports they are.
type pureFuncError struct {
	fn  string
	ctx string
}

func (e pureFuncError) Error() string {
	return "engine: non-deterministic use of " + e.fn + "() in " + e.ctx
}

// pureFuncGuard is sqlite3NotPureFunc (vdbeaux.c:5627). ctx is the schema
// expression this call sits inside, empty for an ordinary statement; when it
// is not empty and the call read the clock, the result is C's error instead
// of the value.
//
// Verified against 3.53.3: over "CREATE TABLE t(a, d AS (date()))",
// "INSERT INTO t VALUES(1)" is "non-deterministic use of date() in a
// generated column"; "CHECK(julianday('now')>0)" and
// "CREATE INDEX i ON t(julianday('now'))" are the same error naming "a CHECK
// constraint" and "an index"; and "date(a)" -- the same function with a
// column argument -- is fine in all three.
func pureFuncGuard(fn string, ctx uint16, usedNow bool, v Value, err error) (Value, error) {
	if err != nil || ctx == pureCtxNone || !usedNow {
		return v, err
	}
	return Value{}, pureFuncError{fn: fn, ctx: pureFuncContextName(ctx)}
}

// The three contexts sqlite3NotPureFunc names (vdbeaux.c:5634-5641). They
// travel as an OpFunction P5 code, exactly as C's own p5 carries
// NC_IsCheck/NC_GenCol, and pureFuncContextName spells each the way C spells
// it so the error text is C's.
const (
	pureCtxNone uint16 = iota
	pureCtxCheck
	pureCtxGenerated
	pureCtxIndex
)

func pureFuncContextName(code uint16) string {
	switch code {
	case pureCtxCheck:
		return "a CHECK constraint"
	case pureCtxGenerated:
		return "a generated column"
	case pureCtxIndex:
		return "an index"
	}
	return ""
}
