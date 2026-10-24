# Handover: cluster flake and replication throughput

Branch `cluster-flake`, from `studio-schema-polish` (ab68913e) with 459f1e67
(scripts/cluster-local.sh) cherry-picked. Not pushed. Server trees:
`~/musql-flake` on .10, .11 and .12 (`LAB_DEST=musql-flake scripts/local-lab N ...`).

## Root causes

1. **Apply transactions lost to every local commit.** `Store.Ingest` began a
   deferred transaction. The engine's commit is optimistic (stale-image check,
   engine/segment_write.go "another connection committed while this
   transaction was open"), so any local commit that landed while an apply was
   open failed the apply with SQLITE_BUSY. A pushed op was then logged and
   dropped until the next anti-entropy round, which lost the same way. That is
   where the ~1140 BUSY log lines come from. The reverse also happened: a long
   apply batch held the lock past a local writer's 5s busy budget. That is the
   `CREATE TABLE IF NOT EXISTS ...: SQLITE_BUSY` failing `New` (state open) in
   the cluster tests.

2. **ENGINE: the planner never uses a PRIMARY KEY/UNIQUE autoindex.** This is
   the dominant cost, and it is still open. On a plain database (no
   replication), `CREATE TABLE m(k TEXT PRIMARY KEY, v)` then
   `EXPLAIN QUERY PLAN SELECT v FROM m WHERE k=?` gives `SCAN m`. The same goes
   for `PRIMARY KEY(tbl,pk,col)`. Only INTEGER PRIMARY KEY gets a SEARCH. An
   explicit `CREATE UNIQUE INDEX e_ab ON e(a,b)` is used, but the plan shows
   only its first column (`SEARCH e USING INDEX e_ab (a=?)`). So every
   bookkeeping lookup is a full scan: `_repl_clock` (tbl,pk,col), `_repl_oplog`
   (site,seq), `_repl_meta`, `_repl_floors` and `_repl_withheld`. At ~25k clock
   rows a point read takes 100ms. That makes apply O(rows) per op and catch-up
   quadratic. It also hits the cloud state store, and any user table keyed by
   TEXT. C SQLite uses `sqlite_autoindex_*` like any other index (where.c,
   whereLoopAddBtree / whereLoopAddBtreeIndex). Find where musql drops
   autoindexes, and where it stops after the first equality column.

3. The cluster tests' 10m package timeout came from (1) and (2) under -race.
   Each test took about 100s, and four tests × count=3 run past 600s.
   `must` panicked on a BUSY RPC.

## Done (this branch)

- replication: `Store.begin` is BEGIN IMMEDIATE on its own connection, with
  the driver's busy backoff. An apply can no longer lose at commit.
- replication: `ingestAll` applies ops in one transaction per batch. It cuts the
  batch at `maxHold` (50ms of holding the write lock) so local writers get in.
  When `ops[k]` is refused, `ops[:k]` still apply. Push drains up to 512
  queued ops per batch (`drain`, `applyPushed`). Sync applies each GetOps page
  (`opsPerFetch` 64 -> 512) in one transaction, and so does `snapshotOps`.
- replication: `waitLocal` gives priority to a local commit that lost to an
  apply. The capture sees a commit fail after preCommit while an apply has
  committed since that connection's last outcome. Applies then wait until that
  connection commits or rolls back, at most `engine.BusyTimeout`.
- replication: per op it does an INSERT OR IGNORE into the oplog first, and
  reads `sameOp` only when the row was already there. The cells merge in two
  statements: `mergeOp` does one read of the row's cells and one multi-row
  upsert, where it used to do a read and a write per cell. The row is built
  from those cells without reading them back (`materializeRowFrom`).
  syncCatalog, claimRange and the withheld check run once per transaction.
  `advanceVV` skips the reads for seqs that were just committed. `held()`
  skips the transaction when every op is a duplicate. `rebuildHeld` (on every
  local commit) opens no transaction unless a table has held rows.
- replication: `Synced(db)` is set after the first Sync round that reached at
  least one peer and pulled everything every reached peer had.
- cloud: a newly placed crdt database is not served until `Synced`. It answers
  503 meanwhile. The state survives a restart through a `<db>.syncing` marker
  file. When clustered, a node without an admin starts anyway and logs
  `no admin yet: ...` every minute; standalone still refuses. The
  cluster-local.sh retry loop is gone. An fts5 table on a crdt database now
  says "virtual tables are not replicated".
- tests: cluster waits share a 4-minute per-test budget, kept below the package
  -timeout, and fail with what they were waiting for. `need(f())(t)` replaces
  `must` in cluster tests. New tests: TestClusterNewPlacementServesAllOrNothing,
  a SQLITE_BUSY log-line counter in TestClusterConcurrentWritesConverge, and
  replication TestIngestDoesNotLoseToLocalWriters. Benchmarks:
  replication/bench_test.go (B1 BenchmarkIngest, B2
  BenchmarkIngestWithLocalWriters, B4 BenchmarkCatchUp in memory),
  replication/bench_batch_test.go (B1/B2 through the paged path), and
  examples/libp2p/bench_test.go (B3 BenchmarkLag, B4 BenchmarkCatchUp).

## Numbers so far (.10, before this branch)

- B3 (libp2p, 3 nodes, 500 rows/s target each, 30s): 18 writes/s/node
  achieved, p50 lag 32.7s, p99 76.1s, converged 76.3s after the writes stopped.
- B1 (per-op Ingest, 20k ops): it did not finish in 53 minutes (the SCAN in
  root cause 2).
- After this branch: not measured. Every number is bound by the SCAN. Measure
  after the planner fix:
  `go test -run XXX -bench 'Ingest|CatchUp' -benchtime=1x -timeout 3600s ./replication/`
  and `cd examples/libp2p && go test -run XXX -bench 'Lag|CatchUp' -benchtime=1x -timeout 3600s .`
  (`BENCH_SECONDS` shortens B3). go test prints nothing until the package
  ends.

## Test state at the checkpoint

- replication -race: ok (241s, .12), the new test included. Without -race it
  is ok too. Earlier, with two local writers, the new test failed once: a
  local write ran out of its 5s while it lost to the OTHER local writer. That
  is local-local optimistic contention, not replication, so the test now uses
  one writer.
- examples/libp2p -race: ok (45s, .10).
- cloud Cluster -race, count=1, .11, log `~/flake4.log`:
  ConcurrentWritesConverge PASS (225s, 100 rows a node, zero SQLITE_BUSY
  lines), DeclinesWhatCRDTCannotReplicate PASS, EveryNodeServesIdentity PASS.
  NewPlacementServesAllOrNothing and Placement FAIL: they run out of the
  4-minute budget while the re-placed node catches up. Each Hrana insert and
  each apply scans (root cause 2). Placement passed before this branch because
  sea served whatever part it had; it now waits for a whole sync. Check that
  `Synced` does flip on a re-placed node: sea's old site id is in the peers'
  version vectors, and a pruned site makes a round take a snapshot and return
  early. If it still fails after the planner fix, look there first. Re-run:
  `cd cloud && go test -race -count=10 -timeout 3600s -run Cluster ./server/`,
  then `go test -race -count=2 ./...`.

## Next steps

1. Engine: autoindex use, and multi-column equality seeks. Read C first. Gates:
   engine suite, driver, both compat-harness builds (-timeout 7200s, count
   `--- FAIL` and `panic:` in the full logs), scripts/sweep.
2. Re-measure B1-B4. If B1 is still under 20k ops/s, profile it. The candidates
   are the per-statement driver overhead and the commit hook (the cloud's
   history `store.OnCommit` runs on every commit).
3. Transport: libp2p PushOp publishes one gossip message per op. Batch them
   (repeated ops in PushMessage; proto change plus `buf generate` in
   examples/libp2p).
4. prune commits on most rounds once the acks advance, and each prune commit
   invalidates any local explicit transaction that is open. Consider pruning
   less often.
5. The driver's `BEGIN IMMEDIATE` returns BUSY at once, while C waits up to
   busy_timeout (btree.c sqlite3BtreeBeginTrans busy loop). replication works
   around it in `Store.begin`. A driver fix would need the full gates.
