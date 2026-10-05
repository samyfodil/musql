package engine

import (
	"math"
	"testing"
)

// TestPrintfFloatOracle pins printfFloat (and through it sqliteFpDecode)
// against renderings taken from SQLite 3.53.3 (mattn/go-sqlite3). The
// compat-harness's float_render_diff_test.go is the differential gate; this
// is the fast in-package check.
func TestPrintfFloatOracle(t *testing.T) {
	cases := []struct {
		format string
		f      float64
		want   string
	}{
		{"%.0f", 2.5, "3"},
		{"%.0f", 3.5, "4"},
		{"%.0f", -2.5, "-3"},
		{"%.20f", 0.1, "0.10000000000000000000"},
		{"%.17f", 1.0 / 3.0, "0.33333333333333330"},
		{"%.2f", 2.005, "2.00"},
		{"%.2f", 2.345, "2.35"},
		{"%.2f", -1.005, "-1.00"},
		{"%.20f", 12345.6789, "12345.67890000000000000000"},
		{"%.3f", 100.0, "100.000"},
		{"%.2f", 1e20, "100000000000000000000.00"},
		{"%.30f", 1.0 / 7.0, "0.142857142857142800000000000000"},
		{"%.30f", 123.0 / 7.0, "17.571428571428570000000000000000"},
		{"%.0f", 99999999999994.5, "99999999999995"},
		{"%.1f", 9999999999999.55, "9999999999999.6"},
		{"%.2f", 9999999999999.556, "9999999999999.56"},
		{"%.6f", 22934673333.950985, "22934673333.950990"},
		{"%.6e", 12345.6789, "1.234568e+04"},
		{"%.6e", 0.0, "0.000000e+00"},
		{"%.6g", 12345.6789, "12345.7"},
		{"%.6g", 0.0000123, "1.23e-05"},
		{"%.3g", 123456, "1.23e+05"},
		{"%.3g", 0.000123456, "0.000123"},
		{"%#.3g", 1.5, "1.50"},
		{"%.6g", 100000, "100000"},
		{"%.6g", 1000000, "1e+06"},
		{"%!f", 42, "42.0"},
		{"%!.3f", 0.1, "0.1"},
		{"%!e", 42, "4.2e+01"},
		{"%!g", 42, "42.0"},
		{"%!.20g", 0.1, "0.1000000000000000056"},
		{"%!.20g", 123456789.123456789, "123456789.123456791"},
		{"%!.0f", 42, "42.0"},
		{"%!10.4f", -42.125, "   -42.125"},
		{"%,015f", 1234.5, "0001,234.500000"},
		{"%,016g", 1234.5, "0000000001,234.5"},
		{"%,g", 1234567, "1.23457e+06"},
		{"%+f", math.Copysign(0, -1), "+0.000000"},
		{"%f", math.Inf(1), "Inf"},
		{"%+f", math.Inf(1), "+Inf"},
		{"%5f", math.Inf(-1), " -Inf"},
		{"%!0.17g", math.Inf(-1), "-9.0e+999"},
	}
	for _, c := range cases {
		got, err := fnPrintf(c.format, []Value{{Typ: Float, F: c.f}})
		if err != nil || string(got.S) != c.want {
			t.Errorf("printf(%q, %v) = %q (err %v), want %q", c.format, c.f, got.S, err, c.want)
		}
	}
}

// TestFormatFloatTextOracle pins the "%!.17g" REAL->TEXT rendering.
func TestFormatFloatTextOracle(t *testing.T) {
	cases := []struct {
		f    float64
		want string
	}{
		{49.47, "49.47"},
		{-411606.84739757882, "-411606.84739757882"},
		{3.0, "3.0"},
		{1e300, "1.0e+300"},
		{math.Copysign(0, -1), "0.0"},
		{0.1, "0.1"},
		{math.Inf(-1), "-Inf"},
	}
	for _, c := range cases {
		if got := formatFloatText(c.f); got != c.want {
			t.Errorf("formatFloatText(%v) = %q, want %q", c.f, got, c.want)
		}
	}
}

// TestFnRoundOracle pins roundFunc's three arms.
func TestFnRoundOracle(t *testing.T) {
	cases := []struct {
		x    float64
		n    int64
		want float64
	}{
		{0.49999999999999994, 0, 1},
		{2.5, 0, 3},
		{-2.5, 0, -3},
		{2.345, 2, 2.35},
		{99999999999994.5, 0, 99999999999995},
		{12085878556602796.0, 11, 12085878556602796.0},
	}
	for _, c := range cases {
		got, err := fnRound([]Value{{Typ: Float, F: c.x}, {Typ: Int, I: c.n}})
		if err != nil || got.F != c.want {
			t.Errorf("round(%v,%d) = %v (err %v), want %v", c.x, c.n, got.F, err, c.want)
		}
	}
}
