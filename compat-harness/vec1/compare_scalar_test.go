//go:build !vec1simd

package vec1

// The scalar oracle computes every distance the way musql does, so the two
// must agree to the bit.

func equalResults(got, want result) bool { return got.String() == want.String() }

func batchQuirk(model string) bool { return false }

func equalIdx(got, want result, model string) bool { return equalResults(got, want) }

func equalMeta(got, want result, model string) bool { return equalResults(got, want) }
