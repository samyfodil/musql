//go:build unix

package main

import "syscall"

// minorFaults is the process's minor page faults so far: a read that touches a
// mapped page for the first time takes one.
func minorFaults() int64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return int64(ru.Minflt)
}
