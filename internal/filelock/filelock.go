// Package filelock implements SQLite-compatible byte-range file locking at
// the reserved byte offsets near 2^30, using POSIX/OFD locks to implement
// the NONE/SHARED/RESERVED/PENDING/EXCLUSIVE lock ladder.
package filelock

// Byte offsets for SQLite's file-locking protocol. These must match exactly
// for cross-engine lock coordination.
const (
	// PendingByte sits just past the 1GiB mark, taken transiently by readers
	// and held during writer escalation.
	PendingByte int64 = 0x40000000

	// ReservedByte signals a writer's intent to modify the database.
	ReservedByte int64 = PendingByte + 1

	// SharedFirst/SharedSize bound the byte range every reader read-locks
	// (SHARED) and a writer escalating to EXCLUSIVE write-locks in full
	// (which only succeeds once every reader has released its SHARED
	// lock on this range).
	SharedFirst int64 = PendingByte + 2
	SharedSize  int64 = 510
)
