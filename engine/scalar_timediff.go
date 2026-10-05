package engine

import "fmt"

// timediff(A, B) calculates calendar distance from B to A (date.c:1618).

// timediffArg parses a datetime argument (date.c:1107).
func timediffArg(v Value) (sqlDateTime, bool) {
	return isDate([]Value{v})
}

// fnTimediff is timediffFunc, date.c:1618.
func fnTimediff(args []Value) (Value, bool, error) {
	if len(args) != 2 {
		return Value{}, false, fmt.Errorf("engine: wrong number of arguments to function timediff()")
	}
	d1, ok := timediffArg(args[0])
	if !ok {
		return Value{Typ: Null}, d1.usedNow, nil
	}
	d2, ok := timediffArg(args[1])
	if !ok {
		return Value{Typ: Null}, d1.usedNow || d2.usedNow, nil
	}
	// Either side may have read the clock ("timediff('now', x)").
	usedNow := d1.usedNow || d2.usedNow
	d1.computeYMD()
	d1.computeHMS()
	d2.computeYMD()
	d2.computeHMS()
	var sign byte
	var Y, M int
	if d1.iJD >= d2.iJD {
		sign = '+'
		Y = d1.Y - d2.Y
		if Y != 0 {
			d2.Y = d1.Y
			d2.validJD = false
			d2.computeJD()
		}
		M = d1.M - d2.M
		if M < 0 {
			Y--
			M += 12
		}
		if M != 0 {
			d2.M = d1.M
			d2.validJD = false
			d2.computeJD()
		}
		for d1.iJD < d2.iJD {
			M--
			if M < 0 {
				M = 11
				Y--
			}
			d2.M--
			if d2.M < 1 {
				d2.M = 12
				d2.Y--
			}
			d2.validJD = false
			d2.computeJD()
		}
		d1.iJD -= d2.iJD
		d1.iJD += 1486995408 * 100000
	} else {
		sign = '-'
		Y = d2.Y - d1.Y
		if Y != 0 {
			d2.Y = d1.Y
			d2.validJD = false
			d2.computeJD()
		}
		M = d2.M - d1.M
		if M < 0 {
			Y--
			M += 12
		}
		if M != 0 {
			d2.M = d1.M
			d2.validJD = false
			d2.computeJD()
		}
		for d1.iJD > d2.iJD {
			M--
			if M < 0 {
				M = 11
				Y--
			}
			d2.M++
			if d2.M > 12 {
				d2.M = 1
				d2.Y++
			}
			d2.validJD = false
			d2.computeJD()
		}
		d1.iJD = d2.iJD - d1.iJD
		d1.iJD += 1486995408 * 100000
	}
	// clearYMD_HMS_TZ, date.c:518
	d1.validYMD = false
	d1.validHMS = false
	d1.tz = 0
	d1.computeYMD()
	d1.computeHMS()
	// date.c:1704's "%c%04d-%02d-%02d %02d:%02d:%06.3f", the seconds through
	// the printf port: Go's %06.3f rounds the exact binary value half-even,
	// sqlite3FpDecode rounds its decimal digits half-up.
	secs, err := printfFloat('f', d1.s, 6, 3, false, 0, false, false, true, false, 0)
	if err != nil {
		return Value{}, usedNow, err
	}
	return Value{Typ: Text, S: []byte(fmt.Sprintf("%c%04d-%02d-%02d %02d:%02d:",
		sign, Y, M, d1.D-1, d1.h, d1.m) + secs)}, usedNow, nil
}
