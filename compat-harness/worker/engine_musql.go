//go:build musql

package main

import (
	"os"

	_ "github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/engine"
)

const driverName = "sqlite"

// The worker is a command, so it may read the environment: MUSQL_JIT=0 turns
// the JIT off for an A/B run.
func init() {
	if os.Getenv("MUSQL_JIT") == "0" {
		engine.Configure(engine.WithoutJIT())
	}
}
