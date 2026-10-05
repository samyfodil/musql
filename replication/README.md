# replication

Replication for musql: open a database on several nodes and every node holds
the same tables, in the mode you pick -- **CRDT** (every node writes; nodes are
eventually consistent and partition-tolerant) or **Leader** (one writer, chosen
by your consensus). Pure Go, no CGo, no external dependency in the base package.

Conflicts resolve per column by last-writer-wins on a hybrid logical clock. See
`DESIGN.md` for the architecture and the invariant-critical decision tables.

## Usage

```go
import "github.com/samyfodil/musql/replication"

// newTransport builds your network's replication.Transport (examples/libp2p has one).
db, err := replication.Open(ctx, "app.db", replication.CRDT(), replication.WithTransport(newTransport))
if err != nil { ... }
defer db.Close()

// db is an ordinary *sql.DB. Use it for every read and write.
db.Exec(`INSERT INTO notes(id, body) VALUES(?, ?)`, "n1", "hello")
```

That is the whole setup:

- **A mode is required**, and every node of one database runs the same one:
  the first a database is opened in is recorded and replicates, and `Open` in
  another fails with `replication.ErrMode` (see "Two modes" below).
- **Every table syncs, and so does the schema.** No list to maintain, no schema
  to create on each node first: a node opened on an empty database gets the
  others' tables and rows, and `CREATE TABLE`/`INDEX`/`VIEW`, `DROP`, and
  `ALTER TABLE` (add, rename or drop a column, rename a table) replicate like
  rows do. The schema and rows a database had before its first `Open` become
  that node's first ops.
- **Any key works.** A table may have a primary key of one column or several,
  an `INTEGER PRIMARY KEY`, or none: each node hands out auto-assigned rowids from a
  range of its own, so two nodes inserting while apart never pick the same id,
  and an explicit id is the same row everywhere. A range is a hash of the node's
  site id unless you assign it: `replication.WithRowidRange(n)`, a distinct `n`
  per node, rules out the rare collision of two hashes (which is otherwise
  contained: each node keeps the first of the two sites it sees).
- **The node's identity is automatic.** A site id is generated on the first open
  and kept in the database; `replication.WithSite` sets one explicitly.
- **Sync runs in the background** — pushed writes are applied as they arrive and a
  full anti-entropy round runs every 2s (`replication.WithSyncInterval`) — and
  `db.Close()` stops it.
- **Offline is fine.** Writes made while a node is partitioned are captured and
  reach the others when it reconnects.
- **The log is compacted.** Once every node has acknowledged an op, it is pruned
  (the tables hold its effect); a node that joins after takes a snapshot from a
  peer. A node gone for good would hold that back forever:
  `replication.Retire(ctx, db, site)` stops waiting for it.

The network is yours: `replication.WithTransport` takes any `Transport`, and
this package pulls in none. `examples/libp2p` is a working one over libp2p.

`Open` is the whole entry point. `Syncer.Attach` and `Node` remain for a
caller that wires its own transport loop (`WithTransport` hands it the
`Syncer`).

## How it works

At commit, the driver hands the transaction's row changes to the capture
(`driver.CaptureHooks`); they become ops — one per changed row, carrying only the
changed columns — written to an op log (`_repl_oplog`) **inside the same
transaction**, so data and log commit atomically. Each op has a dense per-site
sequence number and an HLC timestamp. Nodes converge by anti-entropy (exchange
version vectors, pull what is missing) plus best-effort push. Applying an op
merges per cell into `_repl_clock` (the last-writer-wins truth) and rewrites the
affected row, with a completeness gate that never writes a torn row or a NULL into
a NOT NULL column.

## Schema changes while apart

Tables and columns are replicated by identity, not by name, and an identity is
derived from the definition: two nodes that run the same migration while apart
(the usual `CREATE TABLE IF NOT EXISTS` at startup) create the same table, and
their rows merge. A rename keeps the identity, so an update written under the
old name lands under the new one. A drop wins over a concurrent write to the
dropped table, and a table later created under the same name is a new one.
Two different definitions under one name: the earlier (by hybrid clock) wins
everywhere, and the other's rows stay out.

## Two modes

**CRDT** (`replication.CRDT()`) -- every node writes, and nodes are eventually
consistent. Constraints are checked on a node's own writes; a peer's rows are
applied without them, so every node converges even where the merge breaks a
rule each write kept: a two-column `CHECK` may not hold, a child may be
orphaned, and when two nodes write the same `UNIQUE` value the newer row keeps
it and the other comes back once the value is free -- until then it is out of
the table on every node, and `replication.Withheld(ctx, db)` lists it (table,
key and columns). Generated columns are
recomputed from the merged row.

**Leader** (`replication.Leader(isWriter)`) -- only the node for which
`isWriter()` returns true commits; any other gets `replication.ErrNotWriter`.
Which node that is -- raft, a lease, configuration -- is yours to decide, and
`isWriter` is asked at every commit, so the role can move. Followers hold what
the writer holds, constraints included. On its own this is primary/replica: a
commit is durable on the writer when it returns and reaches the followers
after, so a writer that dies first takes its last commits with it, and a new
writer can start before it has them.

**Leader with a Quorum** (`replication.Leader(isWriter, q)`) is the raft model.
`q := replication.NewQuorum(propose)` hands each commit to your consensus log:
the writer runs the transaction, turns its changes into one entry, discards the
transaction, and calls `propose(ctx, entry)`; your log applies every committed
entry on every node -- the writer too -- with `q.Apply(ctx, entry)`. A commit
returns once a quorum has it and this node applied it, so nothing acknowledged
is lost with the writer. Open the database before your log replays into
`Apply`, and have a new writer apply everything committed before it writes (a
raft `Barrier`). A transaction that began before another commit landed fails
with SQLITE_BUSY and is retried, as on any database; one that also writes TEMP
tables is refused.

```go
q := replication.NewQuorum(func(ctx context.Context, entry []byte) error {
    return r.Apply(entry, 5*time.Second).Error() // hashicorp/raft: committed and applied here
})
db, err := replication.Open(ctx, "app.db", replication.Leader(isLeader, q))
// in the raft FSM: func (f *fsm) Apply(l *raft.Log) any { return q.Apply(ctx, l.Data) }
```

Networking and consensus are the caller's in both.

## Not replicated

Refused with an error, at `Open` for a database that has them and at commit for
a statement that makes them: triggers, virtual tables, `WITHOUT ROWID`, a key
column collated other than `BINARY`, and a `NULL` in a key column. `TEMP`
objects are the connection's own and never replicate.

Every write -- rows and schema alike -- goes through the `*sql.DB` that `Open`
returned. Any other connection to a replicated file can read it, but its writes
are refused ("this database is replicated; write it through the *sql.DB
replication.Open returned"): they would never reach a peer.

And by design: no triggers on synced tables; edge and collaboration scale rather
than a high-throughput write path.
