package engine

import (
	"fmt"
	"testing"
)

func TestAppendPadIntMatchesPrintf(t *testing.T) {
	for _, f := range []struct {
		format string
		width  int
		fill   byte
	}{{"%02d", 2, '0'}, {"%2d", 2, ' '}, {"%03d", 3, '0'}, {"%04d", 4, '0'}} {
		for _, v := range []int{-4713, -100, -12, -1, 0, 1, 9, 10, 99, 100, 366, 999, 1000, 9999, 12345} {
			want, err := fnPrintf(f.format, []Value{{Typ: Int, I: int64(v)}})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(appendPadInt(nil, v, f.width, f.fill)); got != string(want.S) {
				t.Errorf("%s of %d: %q, printf %q", f.format, v, got, want.S)
			}
		}
	}
}

func TestDateTextMatchesSprintf(t *testing.T) {
	for _, x := range []sqlDateTime{
		{Y: 1994, M: 1, D: 5, h: 7, m: 3, s: 9.25},
		{Y: -4713, M: 11, D: 24, h: 12, m: 0, s: 0, useSubsec: true},
		{Y: 9999, M: 12, D: 31, h: 23, m: 59, s: 59.999, useSubsec: true},
		{Y: 7, M: 2, D: 28, h: 0, m: 0, s: 0.0004, useSubsec: true},
	} {
		Y, sign := x.Y, ""
		if Y < 0 {
			Y, sign = -Y, "-"
		}
		want := fmt.Sprintf("%s%04d-%02d-%02d %02d:%02d:", sign, Y, x.M, x.D, x.h, x.m)
		if x.useSubsec {
			s := int(1000.0*x.s + 0.5)
			want += fmt.Sprintf("%02d.%03d", s/1000, s%1000)
		} else {
			want += fmt.Sprintf("%02d", int(x.s))
		}
		got := string(appendTimeText(append(appendDateText(nil, x), ' '), x))
		if got != want {
			t.Errorf("%+v: %q, want %q", x, got, want)
		}
	}
}
