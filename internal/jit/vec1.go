package jit

// Vec1Op picks the sum EmitVec1T8 computes for each row.
type Vec1Op int

const (
	// Vec1L2F: sum of (q-x)^2 in float32 (vec1ModelTDist).
	Vec1L2F Vec1Op = iota
	// Vec1L2D: each difference in float32, squared and summed in float64
	// (vec1L2Dist).
	Vec1L2D
	// Vec1DotF: sum of x*q in float32 (vec1DotProduct).
	Vec1DotF
)
