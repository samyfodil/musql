# replication/rpc

Replication's wire format, and a `replication.Transport` over it.

- `proto/replication/v1/replication.proto` defines the messages nodes send
  each other (ops and their cells, version vectors, acks, snapshot pages,
  image manifests) and `ReplicationService`, whose calls are `Transport`'s.
  Every request names its database. `buf generate` at the repository root
  writes the Go code into `gen/`.
- `OpsToProto`, `SnapshotPageFromProto` and the other conversions turn the
  messages into `replication`'s types and back, for a program with a
  transport of its own. `MarshalQuorumState` encodes a `Quorum`'s frontier for a
  Leader-mode node's raft snapshot.
- `Node` serves every database a process replicates behind one ConnectRPC
  handler, and syncs each with the database of the same name on its peers. The
  program supplies the peers and their URLs:

```go
n := rpc.NewNode(self, peers, http.DefaultClient) // peers: rpc.Peers
mux.Handle(n.Handler())
db, err := replication.Open(ctx, "app.musq", replication.CRDT(), n.Sync("app"))
```

It is its own module, so `replication` itself depends on no RPC library.
