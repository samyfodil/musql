# Segment format compatibility

musql executes against its own format: a `.musq` segment file plus an
append-only delta, never a C SQLite `.db`. C SQLite files are converted in and
out by `sqlite.Import` and `sqlite.Export` (`convert/sqlite`, the
`musql-convert` CLI), and that conversion is tested in both directions.

Almost everything C SQLite records about a database is a field in this format's
catalog: text encoding (a UTF-16 database stores its text in UTF-16), page size,
`auto_vacuum`, `user_version`, `application_id`, the journal mode, and each
catalog row's rowid. The converter carries all of them both ways.

This page lists what still declines and why. Every decline is an error a caller
can act on, never a silently different answer.

## Storage numbers

`PRAGMA page_count`, `PRAGMA freelist_count` and `sqlite_master.rootpage` answer,
computed from this format's own file:

- `page_count` is the segment file plus its delta, in the database's page size.
- `freelist_count` is 0: a rewrite gives back everything churn retained, and the
  delta is append-only.
- `rootpage` is a dense number from 2 upwards for every object with storage, 0 for
  the rest. For a database built by a linear sequence of `CREATE`s this is C's own
  answer.

None of the three is C's number for the same data in general, since C's depend on
its b-tree packing and page reuse. The differential harness compares every other
column and treats these three as implementation detail.

## What declines

| Shape | Why |
| --- | --- |
| `PRAGMA max_page_count` | Its only observable effect is making a write fail once C's pages run out, and this format's pages are not C's. The size cap in this format's own terms is `PRAGMA max_size = <bytes>`: a commit that would leave the database over it fails with "database or disk is full", and nothing of it is written. |
| `UPDATE sqlite_schema SET rootpage = ...` naming no object's storage, or aliasing an index | The UPDATE itself is accepted, as in C, and served at the next schema load when the number is the object's own, another compatible table's (two names for one table), or a sibling index's (which fails the load as C's does). A number that names no object's root reads C's raw pages, which this format does not have, so it declines. |
| `INSERT INTO sqlite_schema` of a table or index with a non-zero rootpage | Same reason. Rows for objects with no storage (views, triggers) are served. |
| `PRAGMA wal_checkpoint` over a non-empty log, except `TRUNCATE` | Its `log`/`checkpointed` columns count C's page frames; the delta holds row records. Over an empty log, and for `TRUNCATE`, it answers exactly. |
| `VACUUM temp INTO 'file'` | C reports success and writes no file. |

## Journal modes

Every `PRAGMA journal_mode` is accepted:

- `delete`, `truncate`, `persist`, `memory` differ in C only in what happens to
  the `-journal` file, which this format does not have.
- `off` disables the undo inside a transaction: `ROLLBACK TO` is a no-op and a
  statement that fails part-way keeps its applied rows, while a full `ROLLBACK`
  still undoes everything.
- `wal` is recorded in the catalog and carried by the converter. The delta is the
  write-ahead log, and a rewrite is the checkpoint.

There is never a `-journal`, `-wal` or `-shm` file beside a segment file.

## The converter

- `sqlite.Import` refuses a source whose `integrity_check` is not "ok"
  (`sqlite.ErrSourceCorrupt`, with the findings); `ImportOptions{Force: true}`
  converts it anyway.
- `sqlite.Export` recreates objects in creation order, so the exported catalog's
  rowids are 1..N with no gaps left by a `DROP`.
- While a rootpage alias exists, whole-database `integrity_check`, `sqlite.Export`
  and a read-only `Open` or `ATTACH` decline.
