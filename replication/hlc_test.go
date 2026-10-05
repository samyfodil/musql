package replication

import (
	"testing"
	"time"
)

func TestNowMonotonicallyIncreasing(t *testing.T) {
	clock := NewClock("site1")
	var prev HLC

	// Generate many HLCs without wall-clock advancement to force logical increments
	for i := 0; i < 1000; i++ {
		curr := clock.Now()
		if curr <= prev {
			t.Fatalf("Now() not strictly monotonic: prev=%v, curr=%v", prev, curr)
		}
		prev = curr
	}
}

func TestNowWallClockAdvance(t *testing.T) {
	clock := NewClock("site1")

	hlc1 := clock.Now()
	time.Sleep(2 * time.Millisecond)
	hlc2 := clock.Now()

	if hlc2 <= hlc1 {
		t.Fatalf("wall clock advance should increase HLC: hlc1=%v, hlc2=%v", hlc1, hlc2)
	}

	phys1 := hlc1.physical()
	phys2 := hlc2.physical()
	if phys2 > phys1 {
		// Physical advanced; logical should reset to 0
		if hlc2.logical() != 0 {
			t.Fatalf("logical should reset when physical advances: phys1=%v, phys2=%v, logical=%v", phys1, phys2, hlc2.logical())
		}
	}
}

func TestMergeAlwaysGreaterThanRemote(t *testing.T) {
	clock := NewClock("site1")

	tests := []struct {
		name   string
		remote HLC
	}{
		{"zero", HLC(0)},
		{"small", HLC(1) << 16},
		{"large", HLC(1000000) << 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := clock.Merge(tt.remote)
			if result <= tt.remote {
				t.Fatalf("Merge(%v) = %v, expected > remote", tt.remote, result)
			}
		})
	}
}

// TestMergeRemoteHighLogicalSamePhysical is the regression case for the
// canonical-merge fix: a remote op in a future millisecond with a high logical
// counter must still come out strictly less than the merged result, even though
// the local clock's logical counter is 0.
func TestMergeRemoteHighLogicalSamePhysical(t *testing.T) {
	clock := NewClock("site1")

	// Remote physical is far in the future so it wins over the wall clock,
	// with a large logical counter the local side has never reached.
	future := uint64(time.Now().UnixMilli()) + 3_600_000 // +1h
	remote := HLC((future << 16) | 500)

	result := clock.Merge(remote)
	if result <= remote {
		t.Fatalf("Merge(%v) = %v, expected strictly > remote (logical carry-forward)", remote, result)
	}
	if result.physical() != future || result.logical() != 501 {
		t.Fatalf("Merge = phys %d log %d, want phys %d log 501", result.physical(), result.logical(), future)
	}
}

func TestMergeGreaterThanPrior(t *testing.T) {
	clock := NewClock("site1")

	prior := clock.Now()
	// Merge with something less than prior
	remote := HLC(0)
	result := clock.Merge(remote)

	if result <= prior {
		t.Fatalf("Merge(%v) = %v, expected > prior=%v", remote, result, prior)
	}
}

func TestMergeMultipleRemotes(t *testing.T) {
	clock := NewClock("site1")

	remote1 := HLC(100) << 16 // physical=100, logical=0
	result1 := clock.Merge(remote1)

	remote2 := HLC(200) << 16 // physical=200, logical=0
	result2 := clock.Merge(remote2)

	if result2 <= result1 {
		t.Fatalf("successive merges should be increasing: result1=%v, result2=%v", result1, result2)
	}
	if result2 <= remote2 {
		t.Fatalf("merge should exceed remote: result2=%v, remote2=%v", result2, remote2)
	}
}

func TestTagLessOrdering(t *testing.T) {
	tests := []struct {
		aHLC  HLC
		aSite string
		bHLC  HLC
		bSite string
		less  bool
	}{
		{HLC(1) << 16, "a", HLC(2) << 16, "b", true},
		{HLC(2) << 16, "a", HLC(1) << 16, "b", false},
		{HLC(1) << 16, "a", HLC(1) << 16, "a", false},
		{HLC(1) << 16, "a", HLC(1) << 16, "b", true},    // same HLC, site breaks tie
		{HLC(1) << 16, "b", HLC(1) << 16, "a", false},   // same HLC, b > a
		{HLC(100) << 16, "z", HLC(1) << 16, "a", false}, // HLC dominates
		{HLC(1) << 16, "z", HLC(100) << 16, "a", true},  // HLC dominates
	}

	for i, tt := range tests {
		result := TagLess(tt.aHLC, tt.aSite, tt.bHLC, tt.bSite)
		if result != tt.less {
			t.Fatalf("test %d: TagLess(%v, %q, %v, %q) = %v, expected %v",
				i, tt.aHLC, tt.aSite, tt.bHLC, tt.bSite, result, tt.less)
		}
	}
}

func TestTagLessTotal(t *testing.T) {
	// Verify that TagLess forms a total order by checking transitivity
	pairs := []struct {
		hlc  HLC
		site string
	}{
		{HLC(1) << 16, "a"},
		{HLC(1) << 16, "b"},
		{HLC(2) << 16, "a"},
		{HLC(100) << 16, "z"},
	}

	for i := 0; i < len(pairs); i++ {
		for j := 0; j < len(pairs); j++ {
			less := TagLess(pairs[i].hlc, pairs[i].site, pairs[j].hlc, pairs[j].site)
			equal := (pairs[i].hlc == pairs[j].hlc && pairs[i].site == pairs[j].site)
			greater := TagLess(pairs[j].hlc, pairs[j].site, pairs[i].hlc, pairs[i].site)

			if equal && (less || greater) {
				t.Fatalf("equal elements should not be less or greater")
			}
			if !equal && less && greater {
				t.Fatalf("cannot be both less and greater")
			}
			if !equal && !less && !greater {
				t.Fatalf("different elements must have an order")
			}
		}
	}
}
