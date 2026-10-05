package compat

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Tests float rendering and formatting against C SQLite, including printf,
// round(), and type casting.

// TestFloatRenderBangMatrix tests printf formatting flags and corner cases.
func TestFloatRenderBangMatrix(t *testing.T) {
	stmts := []string{
		`SELECT printf('%!f',42), printf('%!.3f',0.1), printf('%!e',42), printf('%!g',42)`,
		`SELECT printf('%!.20g',0.1), printf('%!.20g',123456789.123456789)`,
		`SELECT printf('%!.0f',42), printf('%!10.4f',-42.125), printf('%.6f',22934673333.950985)`,
		`SELECT printf('%!.17g',49.47), printf('%!.17g',0.3), printf('%!.16g',0.3), printf('%!.18g',0.3)`,
		`SELECT printf('%!#.3g',1), printf('%#!.0f',1), printf('%!.0e',5), printf('%#.0e',5), printf('%!-12.3e|',-1.5)`,
		`SELECT printf('%#.2f',-0.001), printf('%#+.2f',-0.001), printf('%#.2f',-0.009), printf('%#.2e',-0.001)`,
		`SELECT printf('%f',9e999), printf('%+f',9e999), printf('% 8f|',9e999), printf('%-8f|',-9e999)`,
		`SELECT printf('%0f',9e999), printf('%0e',-9e999), printf('%0g',9e999), printf('%!0.17g',-9e999), printf('%#0G',9e999)`,
		`SELECT length(printf('%0,f',9e999)), substr(printf('%0,.2f',-9e999),1,20)`,
		`SELECT quote(9e999), quote(-9e999), json_quote(9e999), json_array(-9e999), CAST(9e999 AS TEXT)`,
		`SELECT printf('%,f',1234567.5), printf('%,015f',1234.5), printf('%,016g',1234.5), printf('%,g',1234567), printf('%,.0f',-999999.5)`,
		`SELECT printf('%+f',-0.0), printf('% e',0.0), printf('%010.3f',-3.25), printf('%-010.3f|',3.25), printf('%+010.3e',3.25)`,
		`SELECT printf('%.*f',-3,1.5), printf('%*.*f',-9,2,1.5), printf('%*f',4294967306,1.5), printf('%.*f',4294967298,1.5)`,
		`SELECT printf('%F',1.5), printf('a%F',1.5), printf('%-'), printf('a%-'), printf('%5'), printf('b%.'), printf('%5%'), printf('%-5%|')`,
		`SELECT printf('%lllf',1.5), printf('x%llf',1.5), printf('%hd',1), printf('y%2$s','a','b')`,
		`SELECT printf('%f','1.5abc'), printf('%.3f','  12'), printf('%e',x'31'), printf('%g',NULL), printf('%f')`,
		`SELECT printf('%.*g',2147483647,0.01), printf('%.100f',1e-100), printf('%.25e',1.0/3)`,
		`SELECT round(0.49999999999999994), round(-0.49999999999999994), round(-0.0,1), round(-0.04,1), round(-0.4)`,
		`SELECT round(12085878556602796.0,11), round(4503599627370495.5), round(2.5), round(-2.5), round(1e300,5)`,
		`SELECT round(22934673333.950985,5), round('2.345abc',2), round(x'312e3235',1), round(1.005,2), round(9e999,3)`,
		`SELECT round(2.345,'2.9'), round(2.345,2.9), round(2.345,-1), round(2.345,1e300), printf('%.*f','3.9',1.23456)`,
		`SELECT CAST(49.47 AS TEXT), CAST(-411606.84739757882 AS TEXT), CAST(1e300 AS TEXT), CAST(-0.0 AS TEXT), 1e-5 || ''`,
		`SELECT json_quote(0.1), json_array(1e300, -1e-300, 49.47), quote(1.0/3), quote(-0.0)`,
		`SELECT timediff('2024-01-01 00:00:01.0625','2024-01-01'), timediff('2024-01-01','2024-01-01 00:00:59.9995')`,
	}
	floatDifferBatch(t, "float-bang-matrix", stmts)
}

// TestFloatRenderFuzz renders random doubles, weighted toward the hard cases
// (subnormals, huge magnitudes, negative zero, exact and decimal .5 ties,
// 15-17 significant digits), through every float-rendering path, with
// precisions 0..25 and random widths and flags. Each value is one statement;
// a batch of statements is one worker run per engine, and a divergence is
// reported per statement.
//
// Size: FLOAT_FUZZ_N values (default 20000, 2000 under -short), seed
// FLOAT_FUZZ_SEED (default 1).
func TestFloatRenderFuzz(t *testing.T) {
	n := 20000
	if testing.Short() {
		n = 2000
	}
	if v, err := strconv.Atoi(os.Getenv("FLOAT_FUZZ_N")); err == nil && v > 0 {
		n = v
	}
	seed := int64(1)
	if v, err := strconv.ParseInt(os.Getenv("FLOAT_FUZZ_SEED"), 10, 64); err == nil {
		seed = v
	}
	rng := rand.New(rand.NewSource(seed))

	fixed := []float64{
		22934673333.950985, 0, math.Copysign(0, -1), 5e-324, -5e-324,
		math.SmallestNonzeroFloat64 * 3, 2.2250738585072009e-308, 2.2250738585072014e-308,
		math.MaxFloat64, -math.MaxFloat64, 1e308, 1e-308, 1e-320,
		49.47, 0.1, 0.3, 1.0 / 3, 2.0 / 3, 0.49999999999999994, 0.5, 1.5, 2.5,
		4503599627370495.5, 4503599627370496, 9007199254740993, 9223372036854775807,
		99999999999994.5, 9999999999999.55, 12085878556602796, 123456789.123456789,
		999999999999999.9, 9.999999999999999e22, 1e22, 1e23, 1e15, 1e16, 1e17,
	}
	const batch = 250
	var stmts []string
	flush := func(name string) {
		if len(stmts) > 0 {
			floatDifferBatch(t, name, stmts)
			stmts = stmts[:0]
		}
	}
	for i := 0; i < n; i++ {
		var f float64
		if i < len(fixed) {
			f = fixed[i]
		} else {
			f = randomHardDouble(rng)
		}
		stmts = append(stmts, floatRenderStmt(rng, f))
		if len(stmts) == batch {
			flush(fmt.Sprintf("float-fuzz-%d", i/batch))
		}
	}
	flush("float-fuzz-last")
	t.Logf("float render fuzz: %d values, seed %d", n, seed)
}

// TestFloatParseFuzz reads long decimal strings -- past the ~19 significant
// digits sqlite3AtoF keeps -- as a REAL literal, through CAST, through REAL
// affinity, and through numeric arithmetic, and compares the doubles. Size:
// FLOAT_FUZZ_N/4 strings.
func TestFloatParseFuzz(t *testing.T) {
	n := 5000
	if testing.Short() {
		n = 500
	}
	if v, err := strconv.Atoi(os.Getenv("FLOAT_FUZZ_N")); err == nil && v > 0 {
		n = max(v/4, 1)
	}
	rng := rand.New(rand.NewSource(7))
	stmts := []string{
		`CREATE TABLE r(x REAL)`,
		`SELECT 3500000000000000.2500001 AS a, CAST('3500000000000000.2500001' AS REAL) AS b, '3500000000000000.2500001' + 0 AS c`,
		`SELECT 1e500 AS a, -1e500 AS b, 1e-500 AS c, CAST('1e99999' AS REAL) AS d, 123456789012345678901234567890 AS e`,
	}
	for i := 0; i < n; i++ {
		var x string
		if rng.Intn(2) == 0 {
			// Random digits, 18 to 31 of them.
			digits := strconv.Itoa(1 + rng.Intn(9))
			for k := 17 + rng.Intn(14); k > 0; k-- {
				digits += strconv.Itoa(rng.Intn(10))
			}
			dot := rng.Intn(len(digits) + 1)
			x = digits[:dot] + "." + digits[dot:]
			if rng.Intn(3) == 0 {
				x += "e" + strconv.Itoa(rng.Intn(700)-350)
			}
		} else {
			// Within a few units in the 30th digit of the exact midpoint
			// between two adjacent doubles, where digits past the 19th
			// decide the rounding.
			f := math.Abs(randomHardDouble(rng))
			if f == 0 || f == math.MaxFloat64 {
				f = 1
			}
			mid := new(big.Float).SetPrec(2000).SetFloat64(f)
			mid.Add(mid, new(big.Float).SetPrec(2000).SetFloat64(math.Nextafter(f, math.Inf(1))))
			mid.Quo(mid, big.NewFloat(2))
			m, e, _ := strings.Cut(mid.Text('e', 29), "e")
			digits := []byte(strings.Replace(m, ".", "", 1))
			last := int(digits[len(digits)-1]-'0') + rng.Intn(7) - 3
			digits[len(digits)-1] = byte(min(max(last, 0), 9)) + '0'
			x = string(digits[:1]) + "." + string(digits[1:]) + "e" + e
		}
		if rng.Intn(2) == 0 {
			x = "-" + x
		}
		stmts = append(stmts,
			fmt.Sprintf(`SELECT %[1]s AS a, CAST('%[1]s' AS REAL) AS b, '%[1]s' + 0 AS c, CAST('%[1]s' AS NUMERIC) AS d`, x),
			fmt.Sprintf(`INSERT INTO r VALUES('%s')`, x),
		)
		if len(stmts) >= 500 {
			stmts = append(stmts, `SELECT x, typeof(x) FROM r ORDER BY rowid`)
			floatDifferBatch(t, fmt.Sprintf("float-parse-%d", i), stmts)
			stmts = []string{`CREATE TABLE r(x REAL)`} // each batch is a fresh database
		}
	}
	stmts = append(stmts, `SELECT x, typeof(x) FROM r ORDER BY rowid`)
	floatDifferBatch(t, "float-parse-last", stmts)
}

// randomHardDouble draws a finite double from a mix of distributions that
// stress different parts of sqlite3FpDecode.
func randomHardDouble(rng *rand.Rand) float64 {
	sign := 1.0
	if rng.Intn(2) == 0 {
		sign = -1
	}
	switch rng.Intn(10) {
	case 0: // any finite bit pattern: huge, tiny, subnormal
		for {
			if f := math.Float64frombits(rng.Uint64()); !math.IsInf(f, 0) && !math.IsNaN(f) {
				return f
			}
		}
	case 1: // subnormal
		return sign * math.Float64frombits(rng.Uint64()&(1<<52-1))
	case 2: // an exact binary .5 tie at some decimal place
		return sign * (float64(rng.Int63n(1<<20)) + 0.5) / math.Pow(2, float64(rng.Intn(12)))
	case 3: // a decimal .5 tie, which binary cannot hold exactly
		return sign * (float64(rng.Int63n(1_000_000)) + 0.5) * math.Pow10(-rng.Intn(18))
	case 4, 5: // 15-17 significant digits at any magnitude
		digits := 15 + rng.Intn(3)
		m := rng.Int63n(int64(math.Pow10(digits)))
		f, _ := strconv.ParseFloat(fmt.Sprintf("%de%d", m, rng.Intn(80)-40-digits), 64)
		return sign * f
	case 6: // a neighbor of a power of ten
		p := math.Pow10(rng.Intn(60) - 30)
		if rng.Intn(2) == 0 {
			return sign * math.Nextafter(p, 0)
		}
		return sign * math.Nextafter(p, math.Inf(1))
	case 7: // runs of 9s and 0s, which trip the precision-17 shortening
		s := strconv.Itoa(1+rng.Intn(9)) + strings.Repeat("9", rng.Intn(16)) + strconv.Itoa(rng.Intn(10))
		if rng.Intn(2) == 0 {
			s = strconv.Itoa(1+rng.Intn(9)) + strings.Repeat("0", rng.Intn(16)) + strconv.Itoa(rng.Intn(10))
		}
		f, _ := strconv.ParseFloat(s+"e"+strconv.Itoa(rng.Intn(40)-20), 64)
		return sign * f
	case 8: // an integer near 2^52..2^64
		return sign * float64(rng.Uint64()>>uint(rng.Intn(13)))
	default: // ordinary magnitudes
		return sign * rng.Float64() * math.Pow10(rng.Intn(30)-15)
	}
}

// floatRenderStmt builds one SELECT rendering f every way SQLite renders a
// REAL, with a random precision, width and flag set.
func floatRenderStmt(rng *rand.Rand, f float64) string {
	// %.17g round-trips, and 17 digits is within the 19 that sqlite3AtoF
	// reads exactly, so both engines parse the same double; column c0 shows
	// it.
	x := strconv.FormatFloat(f, 'g', 17, 64)
	if !strings.ContainsAny(x, ".e") {
		x += ".0"
	}
	p := rng.Intn(26)
	w := rng.Intn(30)
	flags := ""
	for _, fl := range []string{"-", "+", " ", "0", "#", "!", ","} {
		if rng.Intn(4) == 0 {
			flags += fl
		}
	}
	verbs := []string{"f", "e", "E", "g", "G"}
	v := verbs[rng.Intn(len(verbs))]
	cols := []string{
		x,
		fmt.Sprintf("printf('%%f|%%e|%%g|%%G', %[1]s, %[1]s, %[1]s, %[1]s)", x),
		fmt.Sprintf("printf('%%.%[2]df|%%.%[2]de|%%.%[2]dg', %[1]s, %[1]s, %[1]s)", x, p),
		fmt.Sprintf("printf('%%!.%[2]dg|%%!.%[2]df|%%!.%[2]de|%%#.%[2]dg', %[1]s, %[1]s, %[1]s, %[1]s)", x, p),
		fmt.Sprintf("printf('%%,f|%%,.%[2]df|%%,g|%%!0.17g|%%!.15g', %[1]s, %[1]s, %[1]s, %[1]s, %[1]s)", x, p),
		fmt.Sprintf("printf('%%%[3]s%[4]d.%[2]d%[5]s|', %[1]s)", x, p, flags, w, v),
		fmt.Sprintf("round(%[1]s, %[2]d)", x, p),
		fmt.Sprintf("round(%s)", x),
		fmt.Sprintf("CAST(%s AS TEXT)", x),
		fmt.Sprintf("json_quote(%s)", x),
		fmt.Sprintf("quote(%s)", x),
	}
	for i := range cols {
		cols[i] = fmt.Sprintf("%s AS c%d", cols[i], i)
	}
	return "SELECT " + strings.Join(cols, ", ")
}

// floatDifferBatch is differ() reporting each diverging statement on its own,
// so one bad rendering in a batch of 250 names itself: one worker run per
// engine over the whole batch, compared statement by statement.
func floatDifferBatch(t *testing.T, name string, stmts []string) {
	t.Helper()
	oracle := run(t, "cgo", stmts)
	got := run(t, "musql", stmts)
	if len(oracle) != len(got) {
		t.Fatalf("[%s] result count: cgo %d, musql %d", name, len(oracle), len(got))
	}
	for i := range stmts {
		// No statement here should fail on the oracle; an error on both
		// sides would compare equal and test nothing.
		if oracle[i]["kind"] == "error" {
			t.Errorf("[%s] C SQLite errored on: %s", name, stmts[i])
		}
		ob, _ := json.Marshal(oracle[i])
		gb, _ := json.Marshal(got[i])
		if string(ob) != string(gb) {
			t.Errorf("[%s] musql DIVERGES from C SQLite\n  sql:     %s\n  cgo:     %s\n  musql:  %s", name, stmts[i], ob, gb)
		}
	}
}
