module github.com/samyfodil/musql/replication/rpc

go 1.27.0

replace github.com/samyfodil/musql => ../..

require (
	connectrpc.com/connect v1.21.0
	github.com/samyfodil/musql v0.1.1
	google.golang.org/protobuf v1.36.12
)

require golang.org/x/sys v0.46.0 // indirect
