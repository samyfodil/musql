//go:build !musql && !cgoengine

package main

// Build with exactly one engine tag: -tags musql | modernc | cgoengine.
const driverName = "NO_ENGINE_TAG_SELECTED"
