module github.com/samyfodil/musql/bench/pointread

go 1.27.0

replace github.com/samyfodil/musql => ../..

require (
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/samyfodil/musql v0.0.0-00010101000000-000000000000
)

require golang.org/x/sys v0.46.0 // indirect
