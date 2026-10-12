//go:build !jitscore

package engine

// jitScore is off outside a jitscore build: every "if jitScore" in the VDBE
// is constant false and compiles to nothing.
const jitScore = false

func jitScoreStep(OpCode)      {}
func jitScoreEnter()           {}
func jitScoreProgram(int, int) {}

func jitScoreTake(int64) JitScoreStat { return JitScoreStat{} }
