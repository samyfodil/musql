# Swap readiness: replacing C SQLite with musql under an existing application

This document answers one question — *can I point an existing Go application at
musql instead of `mattn/go-sqlite3` and have its SQL keep working?* — and it
answers it with measurements rather than a feature list. Every claim below names
the gate that produces it, so a reader can re-run it instead of trusting it.

Scope: **SQL**. What is deliberately out of scope is listed at the end.

## The short answer

Yes for SQL, with one driver-level difference, named below. There is no SQL
gap left: every statement in the mined corpus runs, and the last two named
holes -- `PRAGMA locking_mode = EXCLUSIVE` and an `ALTER TABLE ... RENAME`
whose target is spelled as a string literal -- are served.

## What is measured

| Gate | What it does | Result |
| --- | --- | --- |
| `TestTCLCorpus` (`scripts/sweep`) | 67,800 statements (73,855 in the fts5 build) mined from SQLite 3.53.3's own `.test` files — every one of them, no file excluded — replayed against C SQLite as the oracle. `pass` means a FULL output match — column names, row count, every cell. | `unsupported=0 wrong=0 panics=0`, both builds |
| `compat-harness` `-short ./` | The differential suite: ~700 test files comparing rendered results through both drivers. | `0 DIVERGES`, no panics |
| `TestFileInterchangeMatrix` | A database written by one engine, read and verified by the other, both directions, across page sizes / overflow / WITHOUT ROWID / WAL / auto_vacuum. | 22/22 |
| `TestPragmaAppSurface` | Every PRAGMA an application really sets or reads, getter and setter form. | 5 declines, 4 of them by design |
| `TestBoundParameterValues` / `…InDML` / `…Places` / `TestPreparedStatementReuse` | Bound parameters: 17 value types × 21 query shapes, every syntactic POSITION a `?` can occupy, and prepared statements stepped repeatedly with changing argument types. | 0 divergences |
| `TestErrorTextParity` + `…Round5` | ~230 statements whose ERROR MESSAGE is compared, after the driver's parity layer. | 0 divergences |
| `TestGolangMigrateSQL` / `TestGORMGeneratedSQL` / `TestEntGeneratedSQL` / `TestSqlcStyleQueries` | The SQL those four tools actually generate for a SQLite target, replayed statement by statement. | 0 SQL divergences |
| `TestJSONPathMatrix` / `TestDateTimeModifierMatrix` / `TestWindowFrameMatrix` / `TestSelect*CrossProduct` | Cross-product sweeps over the JSON path grammar, the date/time modifiers, window frames × EXCLUDE × collation, and the SELECT surface. | 0 divergences |

`unsupported=0` is the number that speaks most directly to a swap: over 67,800
mined statements there is **no statement C SQLite runs that musql refuses**.
(The bucket is incremented only when musql fails AND the oracle succeeds; a
statement both refuse is `mutualReject`.)

## The gap that closed: `PRAGMA locking_mode = EXCLUSIVE`

It is served. The lock is taken at the first FILE ACCESS after the pragma --
which is where C takes it, "the change does not actually take effect until the
next time the database file is accessed" -- and held across statements on the
ONE descriptor a connection has that outlives a statement,
`engine.Session.HoldLockingMode`.

What made it hard was never the state. A driver connection keeps FOUR
descriptors onto its main file where C's `unixFile` keeps one (`os_unix.c:261`),
and these are OFD locks, owned by the open file *description*, so holding the
mode's lock on any one of them blocked this same connection's next statement on
another exactly as another process would. The answer is the one C already gives:
hold ONE and do not re-lock. While the connection holds it, the per-statement
read lock, `Session.BeginWrite`'s own wrapper and the session's internal ones
(`DB.withSharedLock`) all stand down.

`TestLockingModeExclusiveOracleSpec` measures what the mode owes on the oracle
alone -- the pragma ALONE excludes nobody; after a READ another connection can
still read and can no longer write; after a WRITE it can do neither; and in WAL
mode it loses the read as soon as the exclusive connection has merely read.
`TestLockingModeExclusiveThroughDriver` runs that same table against both
engines, and `TestLockingModeExclusiveLeavesTheModeLikeC` pins the
counter-intuitive half: `= normal` flips what the getter reports at once while
the lock survives until the next access.

Three PRAGMAs are still declined **by design**, because answering would be a lie
about this build rather than a gap: `module_list` (its rows come out of a HASH
whose order this engine does not reproduce -- the same modules, a different
sequence, and a reporter that answers in the wrong order is a wrong answer),
`compile_options` (there are no C compile options here), and
`hard_heap_limit = N` (C enforces it PROCESS-wide, so accepting it would answer
where C raises `SQLITE_NOMEM`). `temp_store_directory = …` is refused for the
neighbouring reason: it is a deprecated global whose value C SQLite validates
against the filesystem. `function_list` used to be on that list and is not any
more -- its rows were already the oracle's own, read out of it rather than
re-derived, and only the statement spelling was missing where the eponymous
`pragma_function_list()` already served them.

## The one driver-level difference, and the flag that closes it

`mattn/go-sqlite3` inspects each column's DECLARED type and parses `timestamp`,
`datetime` and `date` into a Go `time.Time` before handing it to `database/sql`.
driver returns the value as STORED — which is what `modernc.org/sqlite` does
too, and what SQLite stores, having no date type at all.

No statement answers different CELLS because of it, and every SQL-level
comparison in the ORM sweep agrees. But it is visible to an application:
`database/sql` will not scan a string into a `time.Time`, so a model field of
that type fails against the default.

**`?_time_decltype=1` reproduces mattn's rule exactly**, and `?_loc=<name>` (or
`auto` for `time.Local`) places the result, as mattn's own `_loc` does:

```go
db, _ := sql.Open("sqlite", "app.musq?_time_decltype=1&_loc=auto")
```

"Exactly" is the claim the gate makes, including the parts that surprise —
a 13-digit integer is milliseconds while a smaller one is seconds; TEXT that no
format parses becomes the **zero time** rather than an error; a REAL is not
converted at all; and the three names match **exactly**, so `DATETIME(3)` and
`TIMESTAMPTZ` get nothing. `decltype_time_test.go` compares 24 stored values
across seven declared types, both engines, flag on; asserts the flag OFF changes
nothing; scans into a real `time.Time` field; crosses four `_loc` zones; and
checks the 19 expression shapes where a declared type does or does not exist at
all.

It is **off by default**, for three reasons in order of weight: it is that
driver's invention rather than SQLite's, so defaulting to it would diverge from
the other pure-Go driver rather than converge on C; it would break code that
reads those columns as strings today; and mattn's zero-time-on-parse-failure
rule silently turns a `datetime` column holding `'not a date'` into
`0001-01-01`, which is not a trade to make on a user's behalf.

The declared types themselves come from `engine.ResultDeclTypes`, a port of
`columnType` (`select.c:1919`) — only a column REFERENCE has one, recursed
through a derived table, a view and a scalar subquery; a CAST, arithmetic, a
function call and a literal have none.

## Declines an application can still meet

The corpus reports `unsupported=0`, so every remaining decline was found by
GENERATING shapes the corpus never mined. Each is listed with what it protects.

| Shape | Kind | Why |
| --- | --- | --- |
| `module_list`, `compile_options`, `hard_heap_limit = N`, `temp_store_directory = …` | by design | answering would misreport this build, or answer where C errors |
| `group_concat(x)` / `string_agg(x, y)` over a join whose FROM holds a DERIVED TABLE | protective | the result is built FROM the arrival order, and the order is only served where it is PROVEN to match C's. Over-serving this family measured 67 wrong cells. SQLite itself documents the concatenation order as undefined without an `ORDER BY` inside the call. `TestSelectGroupConcatJoinDecline` pins the boundary — the identical FROM with `count`/`sum`/`max` is served. |
| an aggregate's own `ORDER BY` whose one term and one argument are the same column position of two different tables | declined, an oracle bug | 3.53.3 compares the two equal when the `ORDER BY`'s table is cursor 0 and then aggregates the KEY in the argument's place (`group_concat(u.x ORDER BY t.x) FROM t, u` is `1,1,2,2`). Reproducing it needs C's cursor numbering. Every other aggregate `ORDER BY` is served, including over the derived-table join above: C sorts the contributions first, so arrival order reaches the result only through ties, and the tie test runs over the sorted order. `TestAggregateOrderByCursorZeroDeclines` pins it. |

Four defects this project had RECORDED as open were re-probed and are CLOSED —
a CTAS declared type, a RIGHT-JOIN group's coalesced column, a vtab subtype
through a derived table, and a panic on `ALTER TABLE … RENAME COLUMN` against a
string-literal index column reference — along with `min()`/`max()` written only
inside a select-list subquery, aggregates and GROUP BY over a parenthesized join
group, and SQLite's `:N` duplicate-column naming inside such a group.
`TestRecordedDefectsReprobe` runs their reproducers, so the list stays
self-maintaining rather than becoming folklore.

## Deliberately out of scope

None of these are SQL, and none are measured here:

- **The C API surface**: loading extensions, incremental BLOB I/O, registering
  application functions or collations, the backup API, `sqlite3_*` entry points
  generally. musql is reached through `database/sql`.
- **Performance**: tracked separately (`TestBenchVsC`), not part of this claim.
- **Multi-process concurrency against a LIVE C writer.** Two musql connections
  in one process are covered (`TestN4ConcurrentReadersAndWriters`), and the file
  format is interchangeable, but a C process and a musql process writing the
  same file concurrently is not a claim this document makes.
- **`EXPLAIN` output format.** `EXPLAIN` and `EXPLAIN QUERY PLAN` both run and
  describe the compiled program; the bytecode is not C's, and matching its
  listing is explicitly not a target.

## How to re-run all of it

```sh
scripts/sweep                                              # the corpus; every chunk must report wrong=0 panics=0
cd compat-harness && go test -short -count=1 -timeout 3600s ./   # 0 DIVERGES
go test -count=1 ./engine/ ./driver/ ./crdt/...
```

`-timeout` is load-bearing on the harness command: the `-short` set runs past
`go test`'s default 600s package timeout, and a truncated run reports a clean
tally of whatever happened to run first.
