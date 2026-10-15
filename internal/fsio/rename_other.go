//go:build !windows

package fsio

// platformRenameRetryable is never true off Windows, where a rename replaces a file
// whatever has it open.
func platformRenameRetryable(error) bool { return false }
