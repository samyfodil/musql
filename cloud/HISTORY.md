# musql cloud: database history from a physical commit log

Point-in-time restore, branching and (later) read replicas, for databases with
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

## Read replicas (later)

A replica follows by shipping records: it fetches `log` from its last seq
(and any `base-*` file a base record names), applies batches to its own copy
of the delta and renames bases in. Its file is then byte-identical to the
primary's at that seq, so the engine reads it with no replica-specific code;
it must open the file read-only and re-check freshness (the change word moves
on every applied record).

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
