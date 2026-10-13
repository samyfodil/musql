// Module vec1 checks musql's vec1 (https://sqlite.org/vec1) against C SQLite
// with the real vec1 extension compiled in (CGo), and benchmarks the two. A
// module of its own so the extension's C code stays out of the parent
// harness, whose oracle is a plain C SQLite.
module github.com/samyfodil/musql/compat-harness/vec1

go 1.27.0

replace github.com/samyfodil/musql => ../..

require (
	github.com/mattn/go-sqlite3 v1.14.48
	github.com/samyfodil/musql v0.0.0-00010101000000-000000000000
)

require golang.org/x/sys v0.46.0
