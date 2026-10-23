# musql cloud: stage D-a' handover (history from a physical commit log)

Branch: this worktree's, from `91340ce8` (`studio-schema-polish`). Not pushed.
Design: `cloud/HISTORY.md`. Databases are NOT hosted through `replication/`:
they open with the plain driver and keep every engine feature.

## Done

- `engine.WithCommitHook(fn)` (engine/options.go, engine/commit_hook.go): after
  each commit that wrote the main file (and the compaction it may trigger),
  still under the file's exclusive write lock, `fn(CommitInfo{Path, Counter,
  DeltaEnd})`. One nil check per commit when unset. `engine.CommitState(path,
  fn)` reads the same state under the shared lock (delta replayed to its last
  complete batch). Tests: engine/commit_hook_test.go, driver/commit_hook_test.go.
- `cloud/store`: `History` per database (`data/history/<org>/<db>/`): `log`
  (CRC'd records: batch = the delta bytes a commit appended; base = a hard
  link of the immutable segment file + its sha256 + a copy of the delta's
  committed prefix), `OnCommit` (the hook, dispatching by absolute path),
  `Open` (repair: catch up a file ahead of its log, roll a file behind its
  log forward, truncate a torn record), `Sync`, `Snapshot`,
  `SnapshotIfChanged`, `Restore(at, dst)` (declines `ErrFuture`,
  `ErrBeforeHistory`), `RestoreSnapshot`, `Retarget` (in-place restore),
  `Prune(cutoff)`, `SchemaOnly(ctx, src, dst)`.
- `cloud/server`: histories opened before any full-access connection;
  `ListSnapshots`, `CreateSnapshot`, `Restore` (new db, default name
  `<name>-<time>`; or `in_place`: pool blocked, new file, `Retarget`, then
  `SetDatabaseFile` as the commit point), `Create` with `branch_of`,
  `branch_at`, `schema_only` (from a branch or an upload), scheduled
  snapshots + pruning (`snapshots.every`/`keep`), the usage meter, `Usage`
  and `Stats` (from the earlier attempt: hrana.WithObserver, state usage
  tables, the `file` column; its replication hosting and snapshot table
  dropped).
- `cmd/musql-cloud` calls `engine.Configure(engine.WithCommitHook(store.OnCommit))`
  and `engine.RegisterFTS5()`. Any other process embedding `server.New` must
  do the same (a library may not reset the process's engine options).

## Tests

`cd cloud && ../scripts/cap -- go test ./store ./server ./state` green (also
`-race`). store: restore exactness at 26 points (rows, rowids incl. one after
a delete, CREATE INDEX, ALTER ADD COLUMN, AUTOINCREMENT, a trigger, FTS5 with
a MATCH, WITHOUT ROWID, enough writes to compact, VACUUM), file-ahead and
torn-record repair, log-ahead roll-forward (with and without a rewrite),
4 concurrent writers (every restored point is a prefix per writer), branch,
schema-only, prune, in-place. server: the earlier attempt's end-to-end
restore/branch/in-place/prune tests over Hrana, now with a trigger, FTS5 and
WITHOUT ROWID.

Gates on the servers, tree at `0d810ade` (`~/musql-dap`, logs in `~/dap-logs/`):
`go build ./... && go vet ./engine/` and engine 366s / driver 37s /
replication 51s all ok (.11); compat-harness `-short` default ok 5320.8s (.11;
a first run on an overloaded .10 hit the 7200s limit mid-run, no failure),
`-tags sqlite_fts5` ok 6222.0s (.12), 0 `--- FAIL`, 0 `panic:` in both;
`scripts/sweep` 20 chunks all wrong=0 panics=0, 67,800 statements, the 16
`max_page_count` declines only, TxnLockstep PASS (.11).

## Open

- `cloud/cli` e2e (`TestEndToEnd`) is RED: it asserts restore says "does not
  support" and inspect says "does not record usage" (both now implemented).
  Left alone per the brief (no edits to cloud/cli); its owner should update it.
- A commit the hook missed (no hook configured, a failed log write) is logged
  at the next sync with the sync's time, so a restore between the commit and
  the sync gives the state before it.
- Every base costs a sha256 pass over the segment file (it already rewrote the
  whole file); a restore copies the base and verifies its sha.
- Read replicas (shipping records) are designed, not built.
- `Export` still uses `VACUUM INTO` on an unconfined connection.
