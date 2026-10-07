# libp2p transport example

A `replication.Transport` over libp2p, and the end-to-end test that runs real
nodes over it. It is an example of plugging a network into
`replication.WithTransport`, not part of musql: the engine, the driver and the
replication package know nothing of it, and it is its own module so none of
them pull in libp2p.

```go
import (
    "github.com/libp2p/go-libp2p"
    "github.com/samyfodil/musql/replication"
    repllibp2p "github.com/samyfodil/musql/examples/libp2p"
)

host, _ := libp2p.New() // + however your app discovers and connects peers

db, err := replication.Open(ctx, "app.musq", replication.CRDT(), repllibp2p.Sync(host, "my-app"))
```

`Sync` starts a pubsub router of its own; a host that carries several
databases, or already runs pubsub for the application, passes its router
instead: `repllibp2p.SyncWith(host, ps, topic)`, a topic per database.

- Request/response (`ExchangeVV`, `GetOps`) runs on the stream protocol
  `/musql/replication/1.0.0/<topic>`: one per database, so several share a host, each
  answering only for itself, and closing one leaves the others' handlers.
- Push publishes each op to the gossipsub topic.
- `Sync(h, topic)` starts a gossipsub router of its own and is refused on a
  host that already has one -- a second router silently takes over the first's
  gossip. `SyncWith(h, ps, topic)` uses the application's router, which is how
  one host carries several databases. `Close` removes the stream handler and
  leaves the topic; an own router is stopped and its handlers removed.
- Messages are protobuf (`proto/transport.proto`, generated with buf): a
  stream carries one `Request` and one `Response`, each a oneof envelope
  written with `protodelim`. Reads are bounded (4 MiB for a request, 64 MiB
  for a response), streams have a 30s deadline, a GetOps answer stops at 1024
  ops or 64 MiB whatever the caller asked, and an op with an out-of-range kind
  or cell type is refused. A single op over 64 MiB cannot sync, and is logged
  where it is served.

Regenerate `pb/` with `buf generate` after changing the proto.
