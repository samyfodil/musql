# replication — design

A replicated musql database: `replication.Open(ctx, path, mode, opts...)`
returns an ordinary `*sql.DB`; every table and the schema replicate to the other
nodes that open the same database, in CRDT mode (multi-writer, conflict-free,
eventually consistent) or Leader mode (one writer the caller's consensus picks).
Pure Go, no CGo, no external dependency. README.md is the user view; this is
how it works and which rules are load-bearing.

## Architecture

| Part | Where | What |
|------|-------|------|
| Capture | `driver/capture.go`, `engine/rowhook.go`, `capture.go` | The engine journals each transaction's row changes and DDL; at commit the driver hands them to the capture, which writes ops inside that transaction. |
| Op log, clock, meta | `schema.go`, `store.go` | `_repl_oplog` is the grow-only history, `_repl_clock` the per-cell last-writer-wins truth, `_repl_meta` the site id and installed catalog. User tables are a view materialized from the clock. |
| Schema | `catalog.go`, `store_schema.go` | DDL becomes `OpSchema` ops naming objects by content-addressed ID; the schema is the reduction of those ops on a scratch in-memory musql. |
| Rowid ranges | `wrap.go` (`siteRowidRange`), `engine` `DB.SetRowidRange` | Each site auto-assigns rowids from its own range, so offline inserts never collide. |
| Sync | `node.go`, `transport.go` | Version-vector anti-entropy (pull) plus best-effort push. |
| Entry point | `open.go`, `wrap.go` | `Open` builds the Syncer and a connector whose every connection captures into it. |

## Entry point and wiring

`Open` makes two handles on the file:

- the **apply handle**, a plain single-connection `*sql.DB` the Store uses to
  ingest remote ops and for bookkeeping. It has no capture, so its writes are
  never turned back into ops;
- the **user's `*sql.DB`**, over `syncConnector`, whose `Connect` wraps the
  driver connector's and attaches the capture to each `*driver.Conn` before it
  is used (`Syncer.attach`: `SetCaptureHooks` + `SetRowidRange`).

There is no process-global state on this path: no DSN marker, no registry, no
global connection hook. A process refuses a second `Open` of a site that is
already open (`openSites`), which catches the same file opened twice and a
copied file opened beside its original -- either would be two op-log writers
under one site id. `db.Close` cancels the sync loop, waits for it, closes the
Transport if it is an `io.Closer`, then closes the Syncer and frees the site.

The site id is generated once (16 random bytes, hex) and kept in `_repl_meta`;
`WithSite` may set it on the first open and must match it afterwards, because
the log's sequence numbers belong to it.

## Capture

The engine records one `engine.RowChange` per row mutation of a real table into
the session's log, and a `RowSchema` entry (the statement's SQL) for each DDL
statement on main, in its place among the rows. Recording, not calling back
mid-statement, is what keeps the log honest: a statement-level abort and a
`ROLLBACK` truncate it, so a drained log holds only changes about to commit.
Trigger-body changes are not recorded (and triggers are refused, below). A CTAS
journals its `RowSchema` entry at the statement's START mark, since its rows land
in the log before the CREATE finishes.

At each commit point -- autocommit statement, `Tx.Commit`, SQL `COMMIT` alike --
the driver calls `CaptureHooks.PreCommit` with the session still open and still
routed to by `Conn.Exec`, so SQL the hook runs lands in the same transaction.
`capture.preCommit`:

1. resolves each row change through the installed catalog into an op: one per
   changed row, an insert carrying every non-key cell, an update only the
   changed ones, a delete none; a key change is a delete of the old key and an
   insert of the new. TEMP tables, `_repl_*` and `sqlite_*` are skipped;
2. turns each `RowSchema` entry into schema ops (below) and refuses the commit
   if they would not replay to exactly the local catalog;
3. `flush` stamps the ops -- seqs continue from `max(seq)` as THIS transaction
   reads the log, HLC from `Clock.Now()` -- and writes them to `_repl_oplog`
   and their cells to `_repl_clock`, in the user's transaction.

A PreCommit error aborts the commit and rolls it back (the driver poisons the
held session). The hook's own writes leave `changes()`, `total_changes()` and
`last_insert_rowid()` as the user's statements left them, as C keeps a trigger
program's out of them (`vdbe.c:7569`, `vdbeaux.c:2821`). `PostCommit` advances the local version vector and pushes the
ops; `Rollback` discards them.

Seqs come from the log, never a counter: a seq handed to a commit that then
failed is a hole no peer's version vector can pass, and after a restart a
counter resumes below seqs the log holds. The log insert is a plain `INSERT` so
that two transactions reading the same maximum cannot both commit.

Any other connection to the file is refused writes: the driver gives a session
of a connection with no capture hooks, and not marked `AllowReplicaWrites` (the
apply handle is), a write lock (`engine.DB.SetWriteLock`) whenever main holds
`_repl_meta`. The lock goes through `query_only`'s gate with its own message,
and `PRAGMA query_only = 0` does not lift it.

## Bookkeeping schema

```
_repl_meta(k TEXT PRIMARY KEY, v BLOB)          -- 'site', 'catalog', 'schema_ver'
_repl_oplog(site TEXT, seq INTEGER, hlc INTEGER, tbl TEXT, pk BLOB, op INTEGER,
            cells BLOB, PRIMARY KEY(site,seq)) -- grow-only
_repl_clock(tbl TEXT, pk BLOB, col TEXT, hlc INTEGER, site TEXT, val BLOB,
            PRIMARY KEY(tbl,pk,col))           -- per-cell LWW truth
_repl_ranges(lo INTEGER PRIMARY KEY, site TEXT) -- which site owns each rowid range here
```

`tbl` is a table ID and `col` a column ID, never names. `pk` is the key's value,
type-tagged (`EncodePK`; an integral REAL key encodes as the INTEGER it equals,
since SQLite compares keys numerically across the two, `vdbeaux.c:4579`); for a
key of several columns, a tuple of those encodings in column order (`encodeKey`:
a `0x80` lead byte, then each one length-prefixed); or the rowid for a table
without a declared key. Presence is a synthetic cell
`col='__p'`, `val` `1` alive / `0` tombstone, so ONE LWW rule covers insert,
update, delete and resurrection. A schema op's one cell, `schema`, is a
`schemaOp` in JSON.

## HLC and ordering

64-bit hybrid logical clock: 48-bit physical ms, 16-bit logical counter.
`Now()` = `max(wall, last)`, bumping logical on a tie; `Merge(remote)` advances
past remote. Every Ingest merges the op's HLC first, and `OpenStore` seeds the
clock past `max(hlc)` in the log, so a local write is causally after everything
the database has seen. The total order is `(hlc, site)` lexicographic -- site
breaks ties identically on every replica. `seq` is for dedup and version vectors
only, never for LWW. Schema ops reduce in `(hlc, site, seq)` order.

## `Ingest(op, local)` — decision table

| Op | cells carried | presence `__p` |
|----|---------------|----------------|
| insert | every non-key column | `1` @ op.hlc |
| update | changed columns only | `1` @ op.hlc |
| delete | none | `0` @ op.hlc |
| key change | delete(old) + insert(new, every cell), distinct ticks | as above |
| schema | one `schema` cell | none |

Each Ingest is ONE transaction on the apply handle: the op, the clock cells it
wins, the rows it rewrites and -- for a schema op -- the tables it rebuilds all
land or none do, so a failure leaves the op unrecorded and the next delivery
redoes it.

1. **Dedup**: an op whose `(site, seq)` is already in the log is dropped -- if
   it is the same op. A different op at that `(site, seq)` means two nodes write
   under one site id, and is an error. The version vector advances only over
   contiguous seqs (`advanceVV`), so an op that arrives early is kept and counted
   once its predecessors arrive.
2. **Refuse a rowid-range collision**: an op from a site whose range another
   site already owns here is an error (see Rowid ranges).
3. **Schema op** → `applySchema` (below). Otherwise:
4. **Per-cell merge** of `__p` and each carried cell into `_repl_clock`: absent
   → write; present and `(op.hlc, op.site)` newer → overwrite; else discard
   this cell and keep merging the rest. Cells merge even into a tombstoned row
   (needed for resurrection) and into a table or column not known here yet
   (withheld until its schema op arrives, which materializes them).
5. **Materialize** (`local=false` only), resolving the table and columns by ID:
   - unknown table, or not a table → nothing;
   - the table has a row held out by a UNIQUE conflict → **rebuild** it (below);
   - otherwise `DELETE` the row by key, then, when `__p` is alive and every
     `NOT NULL` column without a default has a cell, ONE `INSERT OR IGNORE` of
     the full row rebuilt from the clock (a generated column has no cell; the
     engine computes it);
   - alive but such a column missing → **withhold**: clock only;
   - the insert ignored → it broke a UNIQUE index → **rebuild** the table.

**Rebuild** (`rebuildTable`) empties the table and writes every alive, complete
row from the clock, newest first by the latest `(hlc, site)` of any of its
cells, each with `INSERT OR IGNORE`. A row skipped there lost a UNIQUE value to
a newer row; it goes in `_repl_withheld`, stays in the clock, and comes back at
the next rebuild that finds the value free. A local commit to a table with held
rows rebuilds it too (`rebuildHeld`), since no peer op will. The table is then a
function of the clock alone, so nodes with the same ops hold the same rows.

The apply handle runs with `ignore_check_constraints` on and foreign keys off:
each write was checked where it was made, and a row merged from two of them
may break a CHECK or leave an orphan neither did -- applied, every node holds
it; refused, the op would fail on every node forever. That is the eventual
consistency the caller chose.

The invariants: a full row in one statement is never torn and is always a
per-cell-LWW snapshot; the completeness gate means an update that races ahead
of its insert never writes NULL into a NOT NULL column.

A local capture writes its cells with an unconditional upsert: its HLC is from
`Now()`, newer than anything in the clock.

## Identity

Tables and columns replicate by ID, not by name. IDs are CONTENT-ADDRESSED
(`contentID`: sha256 over a versioned, length-prefixed tuple, ASCII-folded
names) and carried in the op:

| ID | hashes |
|----|--------|
| object | type, folded name, catalog SQL text, generation of the name |
| a table's initial column | table ID, folded name (the definition is in the table's text) |
| an added column | table ID, folded name, definition text, generation |

The generation counts how often the name was retired (dropped or renamed away).
So: two nodes that run the same migration while apart (`CREATE TABLE IF NOT
EXISTS` at startup) create the SAME table and their rows merge; two different
definitions under one name get different IDs and the first in canonical order
wins the name, the other's ops being deterministic no-ops everywhere; a rename
keeps the ID, so an update written under the old name lands under the new one; a
drop retires the ID for good (`Retired`), so a late identical CREATE cannot
resurrect it and a late write to the dropped table stays out of a new table that
reuses the name.

## Schema: reducer and classifier

The schema is a pure function of the set of schema ops: replay them in canonical
order onto an empty scratch in-memory musql (`replaySchema`), and its catalog
text is the schema on every node, byte for byte -- the scratch is this engine, so
the text is what the engine writes.

`catalog.apply` reduces one op, building the SQL from the target's CURRENT name
there (an op names IDs, never names, so it binds to the incarnation its origin
meant). An op whose target is gone, whose name is taken, or that the scratch
refuses is a no-op -- the same on every node. Afterwards `refresh` re-reads every
live object's text, since a rename rewrites dependent indexes and views and a
`DROP TABLE` takes its indexes.

| Action | carries | runs |
|--------|---------|------|
| `create` | ID, type, name, SQL, (index) table ID | the SQL; an index that lands on a different table than its ID names is dropped again |
| `drop` | ID | `DROP <type>` |
| `rename` | table ID, new name | `ALTER TABLE .. RENAME TO "new"` (always quoted, `alter.c:1770`) |
| `addcol` | table ID, column ID, definition | `ALTER TABLE .. ADD COLUMN def` |
| `renamecol` | table ID, column ID, new name, quote | `RENAME COLUMN`, quoted as the origin did (`alter.c:656`) |
| `dropcol` | table ID, column ID | `DROP COLUMN` |

Locally, `capture.preCommit` builds two scratches from the durable log INSIDE
the transaction (race-free): `w` runs each DDL statement and `classify` reads the
change off its catalog's before/after difference (one object appeared, objects
vanished, a table renamed, a column added/dropped/renamed); `r` applies the
resulting ops. `r` must equal `w`, and finally main's real catalog must equal
`r` -- any disagreement refuses the commit, because a schema change that does
not replay is one peers would not get. A statement that does not run on the
scratch (it names a TEMP object) is checked by that last comparison alone.

## Installing the schema (`applySchema`)

The installed catalog is kept in `_repl_meta` with its version, a hash of its
encoding (never a counter, so a cache keyed by it can only hold what it names),
written in the same transaction as the schema ops and the tables. A capture
reads the version with one point read and reuses its cached catalog when it
matches (`Syncer.catalogIn`).

- **Fast path**: the op is the newest schema op and the installed schema is
  exactly the reduction before it (nearly always). Run the statement the
  reducer ran, then materialize only what it unlocks: a new table's rows, an
  added column's cells.
- **Slow path**: an earlier op arrived after later ones. `reconcile` drops every
  object whose identity, name, text or columns differ between the installed and
  target catalogs (views and indexes first, a rebuilt table's indexes with it),
  creates what the target has, and rebuilds each recreated table's rows from the
  clock -- never from the rows it had, which may belong to an incarnation that
  just lost its name.

Either way the real catalog is then checked against the reducer's scratch.
`resync` runs the same reconcile at `Open`, for a process that stopped between a
schema op's durability and anything that depends on it.

## Genesis

The first `Open` of a database with no op log turns what it already has into
this site's first ops, in one transaction: a create op per table, index and
view (generation 0, so two nodes whose schemas were made alike agree on every
ID), then an insert per row. A table without a declared key has its rows
renumbered into this site's rowid range first -- its rowids are its identity,
and two nodes' rowid-1 rows are different rows (C SQLite promises no stable
rowid for such a table either: VACUUM may renumber it). An `INTEGER PRIMARY KEY`
keeps its ids, which are the application's. An op log written by the older
name-keyed predecessor of this package is refused.

## Rowid ranges

A site's auto-assigned rowids are `h<<32 | counter` (`RowidRange`), `h` in
`[1, 2^30)`: the `n` of a site id ending `#r<n>` (`WithRowidRange`, which writes
it there on the first `Open` -- the id is how every other node learns it), or
else a 30-bit hash of the site id. Every id stays below 2^62. Range 0 is left to explicit ids and rows that
predate sync. The engine draws from the range on every insert path
(`DB.autoRowid`, including CTAS) for main tables only; an exhausted range is
`SQLITE_FULL`. An explicit id is the same row everywhere. A deleted row's id is
never handed out again: the engine allocates above a per-table floor
(`DB.SetRowidFloor`) that this package keeps at the largest id the range ever had,
seeded at `Open` from the clock (which keeps a deleted row's key) -- to a peer
the id IS the row, and a late update to the deleted row would land on the new
one.

A caller that assigns every node a distinct range has no collisions. Two sites
whose ranges do collide -- hashed ones, with odds about sites^2/2^31 -- would
merge different rows as one -- on either
of them, and on any node that takes both. So every node keeps `_repl_ranges`:
the first site it admits to a range owns it there, and the other's ops are
refused, whatever key they touch. Its own site is claimed at `Open`, and so is
every site already in the log; a log that already holds two sites of one range
may hold their rows merged, and `Open` refuses it. This is containment, not
convergence: two nodes that met the two sites in different orders keep
different ones. A sync round skips a site whose op is refused and goes on with
the rest.

## Sync

Each site's ops have dense seqs; the version vector is `{site → max contiguous
seq}`. `Node.Run` applies pushed ops as they arrive and runs `Node.Sync` every
interval: `ExchangeVV` with each peer, then `GetOps` pulls (64 per round trip)
for every site the peer is ahead on. Push is best-effort; anti-entropy alone
converges.

The transport is the caller's (`WithTransport`): this package defines the
`Transport` interface and ships only an in-memory one for tests
(`MemNetwork`). `examples/libp2p` is a working one over libp2p.

## Modes

Networking (`Transport`) and consensus are the caller's in both.

| | CRDT (`CRDT()`) | Leader (`Leader(isWriter[, quorum])`) |
|--|--|--|
| who commits | every node | the node `isWriter` reports, asked at every commit; any other gets `ErrNotWriter` and rolls back, TEMP tables excepted |
| consistency | eventual: a merged row may break a CHECK, a UNIQUE value goes to the newer row, a child may be orphaned | the writer checks every constraint and followers apply its rows, so they hold what it holds |
| durability | a commit is on the node that made it, and reaches the others by push and anti-entropy | without a Quorum, the same -- a writer that dies before its followers pulled a commit takes it along, and a new writer can start behind. With one, a commit is the caller's log's: durable once a quorum has it |
| underneath | the same op log, clock and transport | the same: two nodes that briefly both think they write still converge |

### Leader with a Quorum

The writer's commit runs the transaction as usual up to PreCommit, which builds
and stamps its ops and returns `driver.ErrHandOff`: the driver discards the
transaction as it would a refused one and makes the commit's result
`CaptureHooks.HandOff`'s. `handOff` encodes the ops as one entry and calls the
caller's `propose`; the caller's log applies committed entries on every node
with `Quorum.Apply` (an `Ingest` per op), and the commit succeeds when the ops
are in this node's log. So a node's tables change only through the log.

The writer's handed-off commits are serialized (`quorumMu`, held from stamping
to apply). Before stamping, `fresh` compares the op log as the transaction sees
it with the log as it is: any difference means a commit landed after the
transaction began, so its statements ran on old data and its seqs would repeat
-- it fails with `engine.ErrBusy`, exactly as an ordinary commit fails when the
file moved under it, and the driver's busy retry re-runs an autocommit statement.

`Open` requires a mode. The mode is recorded in the replicated catalog as a
schema op (`actMode`): the first a database is opened in, in canonical order,
is its mode, and later ones are no-ops. `Open` in another mode fails with
`ErrMode`; a node first opened in another mode while apart learns it when the
earlier op arrives, and its commits fail with `ErrMode` from then on.

## Constraints

| | On a local write | On a peer's row |
|--|--|--|
| `CHECK` | enforced | not checked |
| `REFERENCES` | enforced if the connection turns `foreign_keys` on; a cascade's changes are captured and replicate | foreign keys off |
| `UNIQUE` | enforced | the newer row keeps the value, the other is held out (`_repl_withheld`) until it frees up |
| generated columns | computed | never a cell; computed from the merged row (C requires the expression be deterministic, `resolve.c:1219-1228`) |
| `AUTOINCREMENT` | allocated inside this site's range, above its floor and its in-range sequence; `sqlite_sequence` is per node | -- |

## Compaction (`prune.go`, `snapshot.go`)

An op only has to stay in the log until every node has it; the clock holds its
effect. Each node's **ack** is its version vector, stamped by its clock when the
vector last changed. Acks are state, not ops: every sync round exchanges all the
acks a node knows (`ExchangeAcks`) and the newer stamp per site wins, so they
reach every node by any path, take no seq (nothing can collide with a write's),
and write nothing to the file.

The **members** are the sites a node admitted (`_repl_ranges`) less those
`Retire` took out -- and always the node itself, retired or not: it may not
prune past its own version vector. Per site, the **frontier** is the least seq
every member has acked; a member whose ack has not arrived holds everything
back. Each sync tick, `prune` deletes the row ops at or below the frontier and
raises the site's **floor** (`_repl_floors`) to it.

Floors are part of the version vector: a row op at or below its site's floor is
applied already and dropped on arrival, the vector counts from the floor, a
site's next seq is above both its log and its floor (`maxSeqQuery`), and an op
at or below the floor counts as applied (`holds`). The HLC is seeded from the
clock as well as the log, since a pruned op's stamp survives only in its cells.

**Schema ops are never pruned.** The schema is the reduction of all of them in
(hlc, site, seq) order, and an op some member has not acked may sort before one
already acked, so no per-site frontier is a safe prefix to fold. For the same
reason a schema op below a floor is never dropped as already applied: if it is
not in the log, this node has not seen it. **Tombstones are never collected**,
so a node back after any absence merges cleanly, and `Retire` needs no fence: it
only stops the frontier waiting for a node. In Leader mode with a Quorum, a
retirement is an op like any write: the writer proposes it.

`OpsSince` below a floor answers `ErrPruned`, and the asker **bootstraps** from
that peer (`Node.bootstrap`): its retained ops (applied with `Ingest`), its
floors and range owners, its retained ops again, then its clock in pages
(merged cell by cell, newest writer winning), and last, every table rebuilt and
the floors raised. The order is what makes the peer's pruning and writes during
the transfer harmless: an op at or below the floors was applied on the peer
before they were read, so its effect is in every clock page read after, and a
schema op among them is in the second op pass; one above them was in an op page
or comes later by anti-entropy. Pages stop at 512 records or 16 MiB.

## Refused

At `Open` for a database that has them, at commit for a statement that makes
them (`supportedObject`), because they cannot be replicated at all:

| Refused | Why |
|---------|-----|
| triggers | their effects are not captured, and they would fire again on every peer |
| virtual tables | their storage is a module's, not rows the capture sees |
| `WITHOUT ROWID`, a key column collated other than `BINARY` | the key is the row's identity, and these make it not one |
| a `NULL` in a key column | a rowid table's PRIMARY KEY allows any number of them |

TEMP objects are the connection's own and never replicate; ATTACHed databases'
changes do not reach the capture.

## Limits

In CRDT mode, eventually consistent only: no global foreign-key, uniqueness or
cross-row transaction guarantee; a coordinated write is single-writer mode, with
the caller's consensus choosing the writer. The op log is compacted (below), but tombstones are never collected. Rowid-range collisions are contained rather than resolved:
resolving them needs an immutable creation identity carried by every op, which
the engine's change log does not provide. Edge
and collaboration scale: every row change is several bookkeeping writes.
