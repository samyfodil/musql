//go:build !unix

package engine

import "os"

// segFileKeyOf answers false where files are read rather than mapped
// (mmapfile): a copy shares nothing worth sharing.
func segFileKeyOf(*os.File) (segFileKey, bool) { return segFileKey{}, false }
