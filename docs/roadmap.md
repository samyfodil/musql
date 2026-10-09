# Roadmap

What is planned, in rough order. Each item says why, what "done" means, and
what it must not break. The standing rules (AGENTS.md) apply to all of it: the
VDBE is the only executor, specialization goes through the JIT, and a shape that
cannot be made exact is declined, never approximated.

## Vector search, compatible with Turso/libSQL

Turso ships vector similarity search in SQL, and its applications use it
through plain SQL over libSQL clients -- which musql already serves through
Hrana (`hrana/`). Today those statements fail here
(docs/local-turso-competitive-research.md lists vector types, functions and
indexes as not portable). The target is that SQL surface, so an application
and its client move over unchanged, rather than a new API of our own.

**1. Types and functions.** Vector columns declared the libSQL way
(`F32_BLOB(n)`, `F64_BLOB(n)`, and the narrower encodings if libSQL keeps
them), stored as BLOBs in libSQL's byte layout so a file converted in or out
(`convert/sqlite`) keeps its vectors. The scalar functions: `vector()` /
`vector32()` / `vector64()` (text or blob to a vector), `vector_extract()`
(back to text), `vector_distance_cos()` and `vector_distance_l2()`. Plain
`OpFunction` builtins first; their results compared against libSQL itself
(a libSQL oracle beside the C SQLite one in `compat-harness/`), including the
error text for a dimension mismatch or a malformed vector.

**2. Fast exact search.** `ORDER BY vector_distance_cos(v, ?) LIMIT k` as a
columnar top-k: the distance computed as a JIT kernel over the segment's
vector column -- SSE/AVX on amd64, NEON on arm64, SIMD128 in wasm, emitted
like the text kernels (internal/jit) -- feeding the existing top-k
(`ORDER BY ... LIMIT` already runs columnar). Exact, so no recall caveat, and
it is what most browser and edge workloads (thousands to a few hundred
thousand vectors) need.

**3. Approximate indexes.** `CREATE INDEX ... (libsql_vector_idx(v))` and the
`vector_top_k(index, query, k)` table-valued function, for tables past what an
exact scan answers in time. libSQL's index is DiskANN; ours needs to be an
index of our own format (a segment-format catalog entry, rebuilt on
compaction) that answers the same SQL. Recall is measured and published, and
the option values libSQL accepts (metric, neighbors, and so on) are honored or
rejected with an error -- never accepted and ignored.

Out of scope until the above is done: embeddings computed in the database,
and vector types outside libSQL's set.

## What SQLite does not do

Features SQLite leaves out, chosen because musql's design -- immutable
segments plus a delta log, a JIT, built-in replication -- makes them cheap
where SQLite's single b-tree file makes them hard. Each stays SQLite-
compatible: new behavior is opt-in, through a PRAGMA, a function or a table,
and a database that uses none of it converts in and out of the C SQLite format
unchanged.

**Concurrent writers (MVCC).** SQLite allows one writer at a time; a busy app
spends its time in SQLITE_BUSY retries. Segments never change in place, so
snapshot reads are already free; the step is letting transactions that touch
disjoint rows commit together, validating at commit time against the delta
(what replication's conflict detection already does across nodes). Turso is
building the same thing.

**Branching and time travel.** A segment file never changes once written, so a
read-only view as of an earlier commit, or a writable branch, costs a pointer
to the old segments plus its own delta -- not a copy. `ATTACH ... AS OF` (or a
`musql_branch()` function) for previews, tests against production data, and
undo. SQLite would have to copy the file.

**Change feeds.** Subscribing to committed row changes (table, rowid, old and
new values) in commit order, from SQL or from the Go and JS APIs. The delta
log already holds exactly that, and replication's capture already reads it;
this exposes it. Lets an app keep caches, search indexes or a UI in sync
without triggers and polling.

**Encryption at rest.** Per-segment authenticated encryption with a key the
application supplies. SQLite's own (SEE) is closed and paid; the open
alternatives are forks. Segments are written whole and read through one
mapping, which is the easy place for it.

**Incremental materialized views.** A view whose result is stored and kept up
to date from the delta on each commit, rather than recomputed on read --
counts, sums and GROUP BYs over large tables answered in microseconds. The
columnar aggregates already merge a delta into a stored answer
(segment_delta_merge.go); a materialized view is that merge, kept.

**Row expiry (TTL).** `CREATE TABLE ... WITH (ttl = ...)`-style expiry handled
by compaction: expired rows are dropped when their segment is rewritten,
invisible to reads before that. Common for caches, sessions and logs; in
SQLite it is a cron job of DELETEs.

Not planned: stored procedures and a server-side language (out of SQLite's
model), and types SQLite has no affinity for -- except vectors, above.

## SQL that SQLite rejects

SQLite keeps its dialect small, and some of what it leaves out costs
applications real work. The rule for adding any of it: **only syntax C SQLite
rejects with an error.** A statement C SQLite accepts must mean exactly what it
means there, so the differential gates stay true, and a statement that uses an
extension fails loudly on C SQLite instead of quietly answering something else.
The harness gets an "extension" bucket for them: musql answers, C errors, and
the answer is checked against PostgreSQL's where PostgreSQL has the feature.

**Schema changes SQLite cannot make in place.** `ALTER TABLE ... ALTER COLUMN`
(type, NOT NULL, DEFAULT), `ADD CONSTRAINT` / `DROP CONSTRAINT` for CHECK,
UNIQUE and foreign keys, and `ADD COLUMN` with a non-constant default. In
SQLite each of these is the twelve-step create-copy-drop-rename recipe, done
by hand. Here it is a catalog change plus, where the data must be checked or
rewritten, one pass that compaction already knows how to do.

**Grouping and filtering.** `GROUPING SETS`, `ROLLUP` and `CUBE` (one query
instead of a UNION ALL per level), `DISTINCT ON (...)`, and `QUALIFY` (filter
on a window function's result without a subquery). All lower to plans the
VDBE already runs.

**Joins and upserts.** `LATERAL` joins (a subquery in FROM that reads the row
to its left -- SQLite only allows it through a correlated scalar subquery),
and `MERGE` (insert, update or delete by match in one statement, beyond what
`INSERT ... ON CONFLICT` covers).

**Foreign keys that hold.** musql enforces foreign keys as C SQLite does,
DEFERRABLE ones checked at COMMIT included. What SQLite leaves sharp:

- *Off unless every connection asks.* Enforcement is a per-connection PRAGMA,
  so one connection that forgets it can break the database's integrity. A
  database-level setting, stored in the catalog, makes every connection
  enforce: `PRAGMA enforce_foreign_keys = ON` (a new name, so no existing
  PRAGMA changes meaning).
- *Errors that name the problem.* "FOREIGN KEY constraint failed" names no
  constraint, table, column or value. Behind an opt-in PRAGMA (C SQLite's
  error text stays the default, since applications match on it), the error
  says which constraint, which child row and which missing parent value.
- *Adding one checks what is there.* `ALTER TABLE ... ADD FOREIGN KEY` (with
  the schema changes above) validates the existing rows and refuses with the
  first violation, instead of SQLite's choice of accepting a constraint the
  data already breaks.
- *The missing index.* With no index on the child columns, every parent
  DELETE or UPDATE scans the whole child table, silently. Options: index the
  child columns automatically (as an automatic index, visible and
  droppable), or report the unindexed foreign keys from a PRAGMA, as
  `foreign_key_check` reports violations.
- *ON DELETE / ON UPDATE actions already work*; what needs adding is
  `foreign_key_check` over attached databases and a clear refusal, rather
  than nothing, for a foreign key that names a table in another database.

**Smaller ones.** `TRUNCATE`; `UPDATE` / `DELETE` with `ORDER BY` and `LIMIT`
always available (a compile-time option in C SQLite, so builds disagree);
foreign keys on by default per connection through a PRAGMA a server can set
once.

**Access control, later.** Read-only and per-table permissions for a
connection, and row-level security for the Hrana server, where many clients
share one database. SQLite has none, since it trusts whoever opens the file;
a server cannot.

## PostgreSQL and MySQL connections

Servers beside Hrana, so applications use the driver they already have. Each
is its own module, like `hrana/`, so the engine and driver still depend on
nothing but `golang.org/x/sys`; the engine does not change. PostgreSQL first:
its protocol is well specified, Go has a message codec for it (pgproto3, from
pgx), and it is where demand -- and Turso -- is heading. MySQL is the same
four layers after it, at somewhat more cost (a less tidy protocol, fewer Go
building blocks).

1. **Wire protocol, simple queries.** Startup and authentication (cleartext,
   MD5, SCRAM), the simple query flow, text-format rows, errors with SQLSTATE
   codes, transaction status. `psql` and simple-query clients work -- with
   SQLite's SQL.
2. **Extended protocol and types.** Parse/Bind/Describe/Execute, which pgx,
   node-postgres, psycopg and JDBC all use; `$1` parameters; binary formats.
   PostgreSQL reports a column's type before running the statement, where
   SQLite types each value: types come from the declared type, `text`
   otherwise. Most application drivers work.
3. **Catalog emulation.** `pg_catalog` and `information_schema` as read-only
   views over musql's catalog, and the `SET` / `SHOW` / `version()` traffic
   drivers send at connect. ORMs (Prisma, Django, SQLAlchemy) and tools
   (`psql`'s `\d`, DBeaver) work. Long tail: each tool's own catalog queries.
4. **PostgreSQL's dialect** (`::` casts, `ILIKE`, `SERIAL`, schemas, real
   booleans, `now()`): translated to the same AST before compiling. A
   separate decision, taken only after 1-3: a dialect mismatch is a silent
   wrong answer, so translation must refuse whatever it cannot map exactly.

Stopping at 2 or 3 is a complete product -- PostgreSQL's protocol over
SQLite's SQL, as Hrana is libSQL's protocol over it. Tested the way the Turso
client job tests Hrana: real `psql`, pgx, node-postgres and psycopg against
`musqld` in CI.

## Write path in the browser

Single-row writes in wasm run at 20-25 us under Node; in the browser race,
UPDATE and DELETE are at or under Turso's times and INSERT is about 1.5-2x
behind. Bulk loading is about 2x behind Turso: the per-row insert loop is
interpreted. Next: JIT-compile the bulk-insert loop, and profile INSERT inside
a browser (its cost there is several times what Node measures).

## Aggregates with scalar subqueries in the select list

`SELECT count(*), sum(v), (SELECT ... ) FROM t` never takes the compiled
aggregate path, even on a clean table (15 ms at 100k rows, against 0.1 ms
without the subquery). An uncorrelated scalar subquery there is a constant;
the recognizer should let it through.
