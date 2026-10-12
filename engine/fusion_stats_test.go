package engine

import (
	"fmt"
	"os"
	"testing"

	"github.com/samyfodil/musql/internal/jit"
)

// TestMain reports, under MUSQL_FUSION_STATS=1, how many programs the suite
// emitted, how many went through the fusion pass, and the time the pass took;
// MUSQL_FUSION_ALWAYS=1 runs the pass on every program, as before the
// Fusable gate, for the comparison. Test tooling only: the engine reads no
// environment.
func TestMain(m *testing.M) {
	if os.Getenv("MUSQL_FUSION_ALWAYS") == "1" {
		jit.SetFuseAllForTest(true)
	}
	code := m.Run()
	if os.Getenv("MUSQL_FUSION_STATS") == "1" {
		emitted, fused, missed, spent := jit.FusionStatsForTest()
		fmt.Printf("fusion: %d programs emitted, %d fused (%.1f%% skipped), %d unflagged yet changed, %v in the pass\n",
			emitted, fused, 100*float64(emitted-fused)/float64(max(emitted, 1)), missed, spent)
	}
	os.Exit(code)
}
