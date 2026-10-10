// Module libsql checks musql's libSQL extensions (vectors, so far) against
// libSQL itself (github.com/tursodatabase/go-libsql, CGo), and benchmarks the
// two. A module of its own: libSQL carries its own copy of SQLite's C symbols,
// which would collide with the C SQLite oracle in the parent module.
module github.com/samyfodil/musql/compat-harness/libsql

go 1.27.0

replace github.com/samyfodil/musql => ../..

require (
	github.com/samyfodil/musql v0.0.0-00010101000000-000000000000
	github.com/tursodatabase/go-libsql v0.0.0-20260424063416-3051e37e6e04
)

require (
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/libsql/sqlite-antlr4-parser v0.0.0-20240327125255-dbf53b6cbf06 // indirect
	golang.org/x/exp v0.0.0-20230515195305-f3d0a9c9a5cc // indirect
	golang.org/x/sys v0.46.0 // indirect
)
