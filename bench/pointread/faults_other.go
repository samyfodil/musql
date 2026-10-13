//go:build !unix

package main

func minorFaults() int64 { return 0 }
