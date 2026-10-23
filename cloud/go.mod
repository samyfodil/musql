module github.com/samyfodil/musql/cloud

go 1.27.0

replace github.com/samyfodil/musql => ../

replace github.com/samyfodil/musql/hrana => ../hrana

require (
	connectrpc.com/connect v1.21.0
	github.com/samyfodil/musql v0.1.1
	golang.org/x/crypto v0.56.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
