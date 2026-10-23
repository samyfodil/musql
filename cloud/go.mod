module github.com/samyfodil/musql/cloud

go 1.27.0

replace github.com/samyfodil/musql => ../

replace github.com/samyfodil/musql/hrana => ../hrana

require (
	connectrpc.com/connect v1.21.0
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/samyfodil/musql v0.1.1
	github.com/samyfodil/musql/hrana v0.0.0
	github.com/spf13/cobra v1.10.2
	github.com/tursodatabase/libsql-client-go v0.0.0-20260528064733-9d5d30a29a60
	golang.org/x/crypto v0.56.0
	golang.org/x/term v0.45.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/antlr4-go/antlr/v4 v4.13.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/exp v0.0.0-20240325151524-a685a6edb6d8 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
