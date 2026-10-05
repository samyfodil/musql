// Join query planner: decides which FROM item drives which nested loop.
// This decision is observable (group_concat, aggregates, LIMIT, subqueries
// report loop order), so it must exactly match SQLite's. The arithmetic uses
// SQLite's LogEst (signed 16-bit fixed-point approximation of 10*log2(x)).
package engine

// logEst is SQLite's LogEst (signed 16-bit LogEstimate). The width is load-bearing
// for rounding in the planner's arithmetic.
type logEst = int16

// logEstAddTab is sqlite3LogEstAdd's static x[] table (src/util.c).
var logEstAddTab = [32]uint8{
	10, 10, // 0,1
	9, 9, // 2,3
	8, 8, // 4,5
	7, 7, 7, // 6,7,8
	6, 6, 6, // 9,10,11
	5, 5, 5, // 12-14
	4, 4, 4, 4, // 15-18
	3, 3, 3, 3, 3, 3, // 19-24
	2, 2, 2, 2, 2, 2, 2, // 25-31
}

// logEstAdd is sqlite3LogEstAdd (src/util.c): an approximate sum of two
// LogEst values, which is not "+" because the representation is logarithmic.
func logEstAdd(a, b logEst) logEst {
	if a >= b {
		if a > b+49 {
			return a
		}
		if a > b+31 {
			return a + 1
		}
		return a + logEst(logEstAddTab[a-b])
	}
	if b > a+49 {
		return b
	}
	if b > a+31 {
		return b + 1
	}
	return b + logEst(logEstAddTab[b-a])
}

// logEstTab is sqlite3LogEst's static a[] table (src/util.c).
var logEstTab = [8]logEst{0, 2, 3, 5, 6, 7, 8, 9}

// logEstFromInt is sqlite3LogEst (src/util.c): an approximation of
// 10*log2(x). The C body has two spellings of the same loop (a __builtin_clzll
// fast path and a shift loop); they agree for every input, so only the shift
// loop is transcribed.
func logEstFromInt(x uint64) logEst {
	y := logEst(40)
	if x < 8 {
		if x < 2 {
			return 0
		}
		for x < 8 {
			y -= 10
			x <<= 1
		}
	} else {
		for x > 255 {
			y += 40
			x >>= 4
		}
		for x > 15 {
			y += 10
			x >>= 1
		}
	}
	return logEstTab[x&7] + y - 10
}

// logEstToInt is sqlite3LogEstToInt (src/util.c). Only ever called on a
// non-negative LogEst: for a negative one the C shift count exceeds the word
// width, which is undefined there and therefore has no answer to reproduce.
func logEstToInt(x logEst) uint64 {
	n := uint64(x % 10)
	x /= 10
	if n >= 5 {
		n -= 2
	} else if n >= 1 {
		n--
	}
	if x > 60 {
		return 0x7fffffffffffffff
	}
	if x >= 3 {
		return (n + 8) << uint(x-3)
	}
	return (n + 8) >> uint(3-x)
}

// estLog is where.c's estLog: log2 of a LogEst, in LogEst units. The 33 is
// 10*log2(10) rounded, undoing the input's own scale.
func estLog(n logEst) logEst {
	if n <= 10 {
		return 0
	}
	return logEstFromInt(uint64(n)) - 33
}
