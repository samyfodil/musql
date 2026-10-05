package engine

import "testing"

// TestLogEstArithmetic verifies LogEst functions match C SQLite's implementation.

// fnvAcc is FNV-1a hash accumulation.
type fnvAcc uint64

func newFNV() *fnvAcc { a := fnvAcc(1469598103934665603); return &a }

func (a *fnvAcc) mix(v int64) {
	for i := 0; i < 8; i++ {
		*a ^= fnvAcc(uint64(v>>(8*i)) & 0xff)
		*a *= 1099511628211
	}
}

func TestLogEstMatchesC(t *testing.T) {
	// sqlite3LogEst over every small input, every power of two, and the
	// neighbourhood of each power of two.
	h := newFNV()
	for x := uint64(0); x <= 4096; x++ {
		h.mix(int64(logEstFromInt(x)))
	}
	for n := 0; n < 64; n++ {
		p := uint64(1) << n
		h.mix(int64(logEstFromInt(p)))
		h.mix(int64(logEstFromInt(p - 1)))
		h.mix(int64(logEstFromInt(p + 1)))
		h.mix(int64(logEstFromInt(p + p/3)))
	}
	h.mix(int64(logEstFromInt(0xffffffffffffffff)))
	if uint64(*h) != 7102433279417648657 {
		t.Errorf("logEstFromInt sweep = %d, C says 7102433279417648657", uint64(*h))
	}

	h = newFNV()
	for a := -200; a <= 1000; a++ {
		for b := -200; b <= 1000; b += 7 {
			h.mix(int64(logEstAdd(logEst(a), logEst(b))))
		}
	}
	if uint64(*h) != 145509932025537868 {
		t.Errorf("logEstAdd sweep = %d, C says 145509932025537868", uint64(*h))
	}

	h = newFNV()
	for a := 0; a <= 2000; a++ {
		h.mix(int64(logEstToInt(logEst(a))))
	}
	if uint64(*h) != 16904685422053150839 {
		t.Errorf("logEstToInt sweep = %d, C says 16904685422053150839", uint64(*h))
	}

	h = newFNV()
	for a := -200; a <= 2000; a++ {
		h.mix(int64(estLog(logEst(a))))
	}
	if uint64(*h) != 4259660511560763561 {
		t.Errorf("estLog sweep = %d, C says 4259660511560763561", uint64(*h))
	}

	// The anchors SQLite asserts inline, so a hash mismatch above always has
	// at least one readable companion failure.
	for _, c := range []struct {
		name string
		got  logEst
		want logEst
	}{
		{"LogEst(1048576)", logEstFromInt(1048576), 200}, // build.c's default nRowLogEst
		{"LogEst(20)", logEstFromInt(20), 43},            // whereLoopAddBtree's auto-index nOut
		{"LogEst(28)", logEstFromInt(28), 48},            // wherePathSolver's nQueryLoop cap
		{"LogEst(2)", logEstFromInt(2), 10},              // the auto-index "runs <1.25 times" cut
		{"LogEst(200)", logEstFromInt(200), 76},
		{"estLog(200)", estLog(200), 43},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if got := logEstToInt(200); got != 1048576 {
		t.Errorf("logEstToInt(200) = %d, want 1048576", got)
	}
}
