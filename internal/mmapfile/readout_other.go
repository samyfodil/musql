//go:build !darwin

package mmapfile

// readNotFault is false where a mapped page faults in cheaply: see the
// darwin twin.
const readNotFault = false
