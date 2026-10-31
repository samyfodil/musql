module github.com/samyfodil/musql/hrana

go 1.27.0

replace github.com/samyfodil/musql => ../

require (
	github.com/coder/websocket v1.8.15
	github.com/planetscale/vtprotobuf v0.6.1-0.20260702190614-8ae5a48058df
	github.com/samyfodil/musql v0.1.1
	google.golang.org/protobuf v1.36.12
)

require golang.org/x/sys v0.46.0 // indirect
