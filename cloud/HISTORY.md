# musql cloud: database history from a physical commit log

Point-in-time restore and branching, for databases with
the engine's full semantics: triggers, virtual tables (FTS included), WITHOUT
ROWID, and rowids exactly as the engine assigned them. The previous design
captured ROW changes through `replication/` and had to refuse all of those;
this one logs the FILE's own bytes, so whatever the engine can store, history
can restore, bit for bit.

## What a `.musq` database is on disk

Read from `engine/segment_delta.go`, `segment_file.go`, `segment_ddl.go`,
`segment_compact.go`, `segment_delta_tail.go`:

- `<db>.musq`, the segment file. It is **never written in place**: every
  writer (`rewriteFile` for DDL, VACUUM, bulk loads, max_size; compaction)
  builds a temporary file beside it, fsyncs it and renames it over the old one.
  Its header names its identity, `(srcChangeCounter, srcPageCount)`.
- `<db>.musq.delta`, an append-only log of one batch per commit, WAL-shaped:
  each batch ends in a trailer (end counter, page count, CUMULATIVE checksum)
  that is the commit point; a torn tail is excluded on replay; zeros end the
  log (the written-ahead tail). The header names the segment file it extends.
  Bytes up to the last committed batch are never rewritten: a later append
  only overwrites a torn tail or zeros past it. A rewrite or compaction
  removes the delta (or replaces it, by rename, with a header + zero tail).
- `<db>.musq.lock`, the OFD byte-range locks and the change word. Not state.

So the database's committed state is exactly **(the segment file, the delta's
bytes up to its last complete batch)**, and a commit changes it in one of two
ways: it appends bytes to the delta, or it replaces the segment file (and
starts a new delta). This is C SQLite's WAL model with the checkpoint made
explicit: a delta batch is a WAL commit's frames, a rewrite is a checkpoint
that resets the WAL.

## What is logged

Per database, a directory `history/<org id>/<db id>/`:

- `log`: append-only records, each `u32 length | body | u32 crc32c(body)`;
  body = `kind u8 | seq u64 | unix nanos i64 | file counter u32 | payload`.
  - **batch** (kind 1): payload = `deltaFrom i64 | bytes`: the delta's bytes
    from the end of what is already logged to the new committed end. One or
    more whole commits, never a part of one: the end always sits on a batch
    trailer.
  - **base** (kind 2): payload = `segment size i64 | sha256 of the segment
    file | delta prefix length i64`, with `base-<seq>.musq` (a HARD LINK to
    the live segment file, which is immutable once named; a copy where a link
    is impossible) and `base-<seq>.delta` (a copy of the delta's committed
    prefix) written and fsynced before the record.
- `seq` is the store's own counter, strictly increasing across the whole
  history (the file's change counter goes back after an in-place restore, so
  it is carried as information only).

A base is logged when the segment file is not the one the latest base names
(a rewrite or a compaction happened; `os.SameFile` against the link, or the
sha256 when it is a copy) or the delta is not the one logged so far (it was
removed or replaced); otherwise a batch. A snapshot (scheduled or
`CreateSnapshot`) is a forced base: it only bounds how much a restore replays.

## The engine hook

`engine.Configure(engine.WithCommitHook(fn))`, process-wide like the other
options, unset by default (one nil check per commit; no behaviour change).
`fn(CommitInfo{Path, Counter, DeltaEnd})` is called from `Session.Commit`
after a commit that wrote the main file has landed and been synced, and after
the compaction that commit may trigger, **still under the file's exclusive
write lock**: the files cannot move while the sink reads them, and calls for
one file arrive in commit order. `DeltaEnd` is where the delta's committed
batches end (the session's own append point). Hooks never fail a commit: the
commit is already durable and acknowledging it is correct.

`engine.CommitState(path, fn)` gives a sink the same view outside a commit:
it holds the shared lock (no writer can commit) and replays the delta to find
its committed end. The store uses it at open (repair), for a snapshot, and
before a restore, so a commit the hook missed (no hook configured, a failed
log write) is caught up rather than lost.

The sink is `cloud/store`: it is SYNC-TO-FILE rather than event-driven. On each
call it compares the file's state to what the log holds and appends what is
missing (a batch, or a base). Missing a call loses only granularity (two
commits logged as one entry), never correctness.

## Durability and crashes

Order per commit: the engine fsyncs the delta (or the renamed file), then the
hook appends and fdatasyncs the log record, then the commit returns to the
client. A crash leaves one of:

- **file ahead of log** (crash after the commit, before the record): open()
  replays the delta's committed end, sees the logged prefix matches, and
  appends the rest as one batch (or a base, if the segment file changed). The
  entry's time is the repair's, later than the commit's: a restore to a time
  in that window gives the state before the commit, which was never
  acknowledged, so both are legitimate answers.
- **log ahead of file**: cannot happen while the file keeps what it fsynced;
  if it does (a lost write, an operator restoring an old copy), open() finds
  the file's state at an earlier point of the log and rolls the file FORWARD to
  the log's end, by materializing that state and renaming it in.
- **file at no logged point**: the file is authoritative; a new base is taken.
- **torn log record**: replay stops at the first bad length/CRC and truncates
  there; the file then reads as ahead and is caught up as above.

## Restore to time T

Declined with a clear error when T is after now or before the oldest base. Else
pick the last record with time ≤ T, the newest base at or before it, and write
a new file pair: the base segment file (copied; its sha256 is verified first,
so a base that was ever mutated is an error, never a wrong answer), and a
delta of the base's delta prefix followed by every batch's bytes in order.
Records always end on a commit trailer, so no partial commit is ever restored,
and the cumulative checksums stay valid because the bytes are the original
bytes in the original order. The result is the database exactly as it was,
rowids, triggers, vtab shadow tables and all.

- **new database** (default): the restore is a fresh database with its own
  history starting from a base of the restored state.
- **in place**: the server closes the database's connections and refuses new
  ones, writes the restored pair under a new file name, logs it as a base of
  the SAME history (the timeline stays linear in wall time: restoring to T and
  then to T' < T is still possible), then points the state record at the new
  file (one row update, the commit point), then deletes the old files.
- **branch**: a restore into a new database, at now or at T.
- **schema-only**: a branch whose objects are recreated empty: the parent's
  `sqlite_schema` SQL in rowid order on a fresh database, skipping internal
  (`sqlite_%`) and shadow tables (`PRAGMA table_list` type `shadow`, which the
  virtual table recreates itself).

## Per node, in a cluster

In a cluster a database, regular or `crdt`, is a file on each node of its
group, written by replication; each node logs its own file's history as
above. The logs are
not shipped between nodes and are not each other's: a node's log holds what
its copy held, bookkeeping included (replication's `_repl_*` tables and the
file's capture guard). See `DESIGN.md`, Deployments.

So no restored file goes back into a replicated group as it is: it carries
this node's site id and op log, and a second file under one site is two
writers of one log. A cluster database's export, branch and restore go
through a **plain copy** instead. A regular database's run on its group's
master, whose copy every write went through: another node forwards them. None
of them runs on a node still in its first sync of the database, which holds
only part of it: they answer Unavailable, as Hrana does.

### Plain copy

`replication.PlainCopy(ctx, src, dst)` writes `dst`, an ordinary database
holding `src`'s schema and rows: `VACUUM INTO` (one consistent read, so `src`
may be live; it is never written), then on the copy the bookkeeping tables are
dropped, exactly replication's own names, so a user table that merely starts
with `_repl_` stays, and the capture guard is lifted. Rowids, `user_version`
and `sqlite_sequence` come along as `VACUUM INTO` keeps them. A row withheld by
a UNIQUE conflict (`replication.Withheld`) is in no table, so it is in no
plain copy.

- **Export** is the plain copy of this node's file.
- **Branch** (now or at T) and **restore into a new database**: this node's
  history at T is restored to a scratch file and plain-copied into the new
  database's file (a schema-only branch then keeps only its schema), which is
  opened through replication here: what a database holds before its first
  `Open` becomes that node's first ops (a keyless table's rowids are
  renumbered into the new site's range). The new database is of the parent's
  type, in the parent's group unless one is named; it is made on this node, so both the
  parent (its history is here) and the new database's group must be placed
  here, as for an upload. The group's other nodes sync it like any database
  newly placed there. A point from before this node's first sync completed
  holds what the node held then: part of the database.

### Restore in place

The file is not replaced: it is the replicated state every peer's version
vector points at. A restore to T is ordinary writes through the database's
replicated `*sql.DB`, in one transaction, so it is never half done:

1. this node's history at T is restored to a scratch file and plain-copied;
2. in the transaction, `sqlite_schema` less the engine's `sqlite_` objects
   must equal T's (the bookkeeping is on both sides), else it is declined:
   "the schema changed since <T>: restore into a new database";
3. it is declined as well when a row is withheld, at T or now: a withheld row
   is alive in the clock and in no table, so a table diff can neither bring it
   back as it was nor delete it;
4. each table's rows are matched by replication's identity, the declared
   primary key or else the rowid, and compared value for value, types
   included (generated columns follow the others). A row only now is deleted,
   a row only at T is inserted (a keyless table's with its rowid, which is its
   identity), and a row in both has the columns that differ updated;
5. commit. `ErrBusy` (a peer's op or a local write landed meanwhile) runs the
   diff again on the new state, a few times before giving up.

The writes are captured like any others, with fresh HLCs, and win
last-writer-wins on every node: every node converges to the state at T plus
the writes made concurrently with the restore, and a cell the restore did not
change keeps any concurrent write to it (but for a row it deletes and inserts
again, below). The restore is a commit like others,
so the history stays linear and a restore to before it undoes it.

- **UNIQUE**: deletes run first and inserts last. An update that fails on a
  UNIQUE value another changed row still holds (two rows trading a value) is
  undone and that row is deleted, then inserted with its values at T. From
  then on the table is a subset of T's, which held every UNIQUE constraint, so
  no insert can conflict.
- Foreign keys are off on the restore's connection (a cascade would delete
  rows it then brings back) and CHECK constraints are not checked (a merge may
  have broken a multi-column CHECK at T, as replication's apply allows). The
  connection is closed afterwards, so no client inherits either setting.

## Concurrency

All connections to one file, in this process or another, commit under the
file's exclusive OFD lock, and the hook runs inside it, so log order is commit
order without a lock of the store's own across files. The store keeps a mutex
per database for its in-memory index (the hook, snapshots, pruning and
restores all take it). A snapshot or repair outside a commit takes the shared
lock through `engine.CommitState`, which waits for a commit in progress.

## Retention

`snapshots.keep`: the newest base older than `now - keep` is kept (a restore at
exactly the edge needs it); every earlier base file is deleted and the log is
rewritten from that base's record on (written beside, fsynced, renamed).
Restorable range: from the oldest kept base to now.

## Declined, deliberately

- TEMP and ATTACHed databases are not logged: a Hrana connection is confined
  (no ATTACH), and TEMP dies with its connection, as in C.
- `NoSyncOnCommit` databases: the hook still logs, but a power loss can lose
  the file's last commits and the log's equally; nothing is promised beyond
  what the file promises.
