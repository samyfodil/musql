package jit

// KernelCallsForTest reads and clears how many kernel and program calls ran;
// zero outside a jitscore build, which keeps no count.
func KernelCallsForTest() int64 {
	if !scoreCalls {
		return 0
	}
	return swapKernelCalls()
}
