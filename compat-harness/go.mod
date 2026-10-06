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
	github.com/duckdb/duckdb-go/v2 v2.10506.0
	github.com/mattn/go-sqlite3 v1.14.48
	github.com/samyfodil/musql v0.0.0
	turso.tech/database/tursogo v0.7.2
)

require (
	github.com/apache/arrow-go/v18 v18.5.1 // indirect
	github.com/duckdb/duckdb-go-bindings v0.10506.0 // indirect
	github.com/duckdb/duckdb-go-bindings/lib/darwin-amd64 v0.10506.0 // indirect
	github.com/duckdb/duckdb-go-bindings/lib/darwin-arm64 v0.10506.0 // indirect
	github.com/duckdb/duckdb-go-bindings/lib/linux-amd64 v0.10506.0 // indirect
	github.com/duckdb/duckdb-go-bindings/lib/linux-arm64 v0.10506.0 // indirect
	github.com/duckdb/duckdb-go-bindings/lib/windows-amd64 v0.10506.0 // indirect
	github.com/ebitengine/purego v0.9.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/google/flatbuffers v25.12.19+incompatible // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.3 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.25 // indirect
	github.com/tursodatabase/turso-go-platform-libs v0.7.2 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	golang.org/x/exp v0.0.0-20260112195511-716be5621a96 // indirect
	golang.org/x/mod v0.32.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/telemetry v0.0.0-20260116145544-c6413dc483f5 // indirect
	golang.org/x/tools v0.41.0 // indirect
	golang.org/x/xerrors v0.0.0-20240903120638-7835f813f4da // indirect
)

replace github.com/samyfodil/musql => ../
