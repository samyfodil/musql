// Module compat-harness is a differential-testing harness that runs identical
// SQL through two engines and asserts identical results:
//   - musql        (github.com/samyfodil/musql, the engine under test)
//   - real C SQLite (github.com/mattn/go-sqlite3, CGo — the ground-truth oracle)
//
// It is a SEPARATE nested module with its own go.mod so its CGo dependency never
// touches musql's pure-Go, CGo-free promise. Both register the database/sql
// driver name "sqlite", so the worker is built once per engine via build tags
// (musql / cgoengine).
module github.com/samyfodil/musql/compat-harness

go 1.27.0

require (
	github.com/mattn/go-sqlite3 v1.14.48
	github.com/samyfodil/musql v0.0.0
	turso.tech/database/tursogo v0.7.2
)

require (
	github.com/ebitengine/purego v0.9.1 // indirect
	github.com/tursodatabase/turso-go-platform-libs v0.7.2 // indirect
	golang.org/x/sys v0.46.0 // indirect
)

replace github.com/samyfodil/musql => ../
