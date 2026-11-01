module github.com/samyfodil/musql/examples/turso

go 1.27.0

replace (
	github.com/samyfodil/musql => ../..
	github.com/samyfodil/musql/hrana => ../../hrana
)

require (
	github.com/samyfodil/musql v0.1.1
	github.com/samyfodil/musql/hrana v0.0.0
	github.com/tursodatabase/libsql-client-go v0.0.0-20260528064733-9d5d30a29a60
)

require (
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20260702190614-8ae5a48058df // indirect
	golang.org/x/exp v0.0.0-20240325151524-a685a6edb6d8 // indirect
	golang.org/x/sys v0.46.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
