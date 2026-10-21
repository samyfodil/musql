# Benchmarks

Read workloads compared with C SQLite and Turso. Every workload is checked
against C SQLite on up to 40 bind values before timing, then timed for about
200 ms rather than a fixed iteration count. All engines read disk-backed files
with a warm cache.

How each engine is called:

- **musql**: its direct engine interface from Go, with the JIT.
- **C SQLite (native)**: a C program (`compat-harness/testdata/nativebench/cbench.c`)
  calling the SQLite C API directly, built from the same SQLite 3.53.3 source
  and options as the Go driver. No Go, cgo or `database/sql` in the path.
- **C SQLite via Go**: mattn/go-sqlite3 through `database/sql`, for reference.
  The difference between the two C columns is what the Go bridge costs.
- **Turso**: its Go driver, which reaches the Rust library through purego. It
  pays a foreign-call cost per statement as the Go path to C does.

The × columns compare musql with native C SQLite.

## 100,000 rows

**Apple M4** (arm64, macOS)

| Query | musql | C native | C via Go | Turso | vs C native |
| --- | ---: | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 25 µs | 2.77 ms | 2.19 ms | 5.91 ms | **111× faster** |
| Filtered count, two predicates | 25 µs | 2.61 ms | 2.61 ms | 7.34 ms | **104× faster** |
| Rowid lookup | <1 µs | 3.1 µs | 5 µs | 9 µs | **9× faster** |
| Secondary-index equality | <1 µs | 2.9 µs | 6 µs | 11 µs | **6× faster** |
| Indexed equi-join | 2 µs | 4.4 µs | 8 µs | 16 µs | **2.2× faster** |
| Sum over a filter | 25 µs | 2.44 ms | 2.43 ms | 6.69 ms | **98× faster** |
| Grouped aggregate | 169 µs | 14.0 ms | 13.8 ms | 26.4 ms | **83× faster** |
| `ORDER BY v DESC LIMIT 20` | 100 µs | 2.58 ms | 2.56 ms | 22.4 ms | **26× faster** |
| Whole-table count | <1 µs | 8.2 µs | 10 µs | 33 µs | **25× faster** |

**Intel Xeon D-2123IT** (amd64, Linux)

| Query | musql | C native | C via Go | Turso | vs C native |
| --- | ---: | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 40 µs | 5.63 ms | 6.29 ms | 18.9 ms | **142× faster** |
| Filtered count, two predicates | 76 µs | 6.52 ms | 7.25 ms | 23.4 ms | **86× faster** |
| Rowid lookup | 1 µs | 10.3 µs | 20 µs | 37 µs | **9× faster** |
| Secondary-index equality | 2 µs | 9.7 µs | 22 µs | 51 µs | **4× faster** |
| Indexed equi-join | 7 µs | 14.5 µs | 30 µs | 79 µs | **2× faster** |
| Sum over a filter | 32 µs | 6.08 ms | 6.80 ms | 22.4 ms | **190× faster** |
| Grouped aggregate | 543 µs | 38.3 ms | 40.5 ms | 102 ms | **71× faster** |
| `ORDER BY v DESC LIMIT 20` | 330 µs | 8.07 ms | 8.88 ms | 87.0 ms | **24× faster** |
| Whole-table count | 1 µs | 22.3 µs | 35 µs | 149 µs | **16× faster** |

## 1,000,000 rows

The scan multiples hold or grow with ten times the rows.

**Apple M4** (arm64, macOS)

| Query | musql | C native | Turso | vs C native |
| --- | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 248 µs | 23.0 ms | 60.7 ms | **93× faster** |
| Filtered count, two predicates | 253 µs | 27.5 ms | 74.8 ms | **109× faster** |
| Rowid lookup | 1 µs | 3.5 µs | 11 µs | **3.5× faster** |
| Secondary-index equality | 3 µs | 3.5 µs | 12 µs | **1.2× faster** |
| Indexed equi-join | 4 µs | 5.5 µs | 19 µs | **1.4× faster** |
| Sum over a filter | 252 µs | 25.2 ms | 70.4 ms | **100× faster** |
| Grouped aggregate | 1.60 ms | 157 ms | 414 ms | **98× faster** |
| `ORDER BY v DESC LIMIT 20` | 209 µs | 27.1 ms | 293 ms | **130× faster** |
| Whole-table count | <1 µs | 1.36 ms | 5.82 ms | **4,200× faster** |

**Intel Xeon D-2123IT** (amd64, Linux)

| Query | musql | C native | Turso | vs C native |
| --- | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 438 µs | 60.0 ms | 207 ms | **137× faster** |
| Filtered count, two predicates | 1.17 ms | 68.8 ms | 252 ms | **59× faster** |
| Rowid lookup | 1 µs | 11.4 µs | 50 µs | **9× faster** |
| Secondary-index equality | 7 µs | 11.2 µs | 51 µs | **1.6× faster** |
| Indexed equi-join | 12 µs | 16.6 µs | 82 µs | **1.4× faster** |
| Sum over a filter | 507 µs | 64.5 ms | 253 ms | **127× faster** |
| Grouped aggregate | 3.98 ms | 396 ms | 1.32 s | **99× faster** |
| `ORDER BY v DESC LIMIT 20` | 678 µs | 83.8 ms | 1.07 s | **124× faster** |
| Whole-table count | 3 µs | 5.67 ms | 18.6 ms | **2,200× faster** |

## Every shape the benchmark times

**1,000,000 rows, Intel Xeon D-2123IT** (amd64, Linux), one thread

The tables above show the core shapes. The benchmark also times shapes that no
columnar filter models: `HAVING`, `OR`, `LIKE`, TEXT sorting, `DISTINCT`,
subqueries, compounds and pagination. Every one is JIT-compiled, and
`TestBenchColumnarVsC` fails if the JIT declines one. musql is faster than C
SQLite on all 26 shapes, and on the 4 single-row writes (7–8×, through
`database/sql` against mattn/go-sqlite3).

| Query | musql | C native | Turso | vs C native |
| --- | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 457 µs | 59.3 ms | 214 ms | **130× faster** |
| Filtered count, two predicates | 1.18 ms | 68.1 ms | 254 ms | **58× faster** |
| Rowid lookup | 2 µs | 11.2 µs | 45 µs | **5.6× faster** |
| Secondary-index equality | 7 µs | 11.1 µs | 52 µs | **1.6× faster** |
| Indexed equi-join | 12 µs | 16.6 µs | 82 µs | **1.4× faster** |
| Sum over a filter | 500 µs | 63.8 ms | 257 ms | **128× faster** |
| Grouped aggregate | 3.98 ms | 393 ms | 1.32 s | **99× faster** |
| `ORDER BY v DESC LIMIT 20` | 676 µs | 83.2 ms | 1.06 s | **123× faster** |
| Whole-table count | 9 µs | 5.53 ms | 19.1 ms | **614× faster** |
| `min`/`max` whole table | 25 µs | 86.2 ms | 300 ms | **3,448× faster** |
| Grouped `min`/`max` | 9.25 ms | 423 ms | 1.38 s | **46× faster** |
| `GROUP BY ... HAVING` | 2.61 ms | 349 ms | 1.07 s | **134× faster** |
| `OR` predicate | 5.07 ms | 67.0 ms | 247 ms | **13× faster** |
| `BETWEEN` range | 600 µs | 64.1 ms | 274 ms | **107× faster** |
| `IN` list | 11.0 ms | 109 ms | 283 ms | **9.9× faster** |
| `IS NULL` scan | 1.52 ms | 57.0 ms | 178 ms | **38× faster** |
| `LIKE` scan | 19.8 ms | 107 ms | 285 ms | **5.4× faster** |
| `ORDER BY` text `LIMIT 20` | 57.9 ms | 93.7 ms | 489 ms | **1.6× faster** |
| Grouped count, few groups | 9.77 ms | 132 ms | 407 ms | **14× faster** |
| `DISTINCT` | 3.37 ms | 99.1 ms | 292 ms | **29× faster** |
| Scalar subquery | 31.5 ms | 168 ms | 472 ms | **5.3× faster** |
| `IN` subquery | 11.5 ms | 86.8 ms | 154 s | **7.5× faster** |
| Correlated `EXISTS` | 11.2 ms | 2.15 s | 3.31 s | **192× faster** |
| `UNION ALL` compound | 875 µs | 130 ms | 448 ms | **149× faster** |
| `LIMIT 20 OFFSET 1000` | 6.55 ms | 92.3 ms | 1.07 s | **14× faster** |
| Join projecting rows | 8 µs | 18.1 µs | 81 µs | **2.3× faster** |

## Vector search

Against libSQL through its Go driver, 384-dimension float32 vectors, top 10,
one thread unless noted, on a 4-core amd64 server
([benchmarks](../compat-harness/libsql/bench_test.go)):

| Workload | musql | libSQL |
| --- | ---: | ---: |
| `vector_top_k`, 100k clustered vectors | **4.1 ms, exact** | 27.5 ms, recall 10/10 |
| `vector_top_k`, 100k random vectors | **15.2 ms, exact** | 33.3 ms, recall 6/10 |
| Brute-force KNN, 100k rows, cosine | 209 ms (60 ms on 4 cores) | 264 ms |
| Brute-force KNN, 100k rows, L2 | 215 ms (61 ms on 4 cores) | 216 ms |
| Insert 2,000 vectors as text | **125–134 ms** | 173–185 ms |

libSQL's index answers from a DiskANN graph and can miss neighbours;
musql's default index returns the exact top 10 and is still faster here.

`vec1` against C SQLite with vec1 built for speed (AVX2/FMA), 100,000
clustered vectors of 384 dimensions, one thread, mean of 200 queries, on an
amd64 Xeon D ([benchmark](../compat-harness/vec1/bench_test.go)):

| Index | musql query | C SQLite query | recall@10 (musql / C) | musql train + build | C train + build |
| --- | ---: | ---: | ---: | ---: | ---: |
| flat | **25.0 ms** | 72.5 ms | 1.000 / 1.000 | 0.8 s | 0.9 s |
| ivf | **8.5 ms** | 19.3 ms | 1.000 / 1.000 | 2.8 s | 3.8 s |
| ivfpq | **1.30 ms** | 1.86 ms | 0.999 / 0.999 | 12.1 s | 16.5 s |
| ivfopq | **0.99 ms** | 1.30 ms | 0.990 / 0.993 | 60.1 s | 71.5 s |
| ivfbq | **0.98 ms** | 1.33 ms | 0.994 / 0.996 | 2.7 s | 3.5 s |

Inserting the 100,000 rows takes about the same time in both (0.59–0.63 s
for musql, 0.52–0.64 s for C).

## Against DuckDB

DuckDB is an analytical engine: columnar, vectorized, and multithreaded by
default. It is not SQLite-compatible, so it is a yardstick for musql's columnar
paths, not a replacement. The harness times it on the same data and bind values,
once on one thread and once with its default thread count, and checks its
answers against C SQLite by value.

musql runs single-threaded by default, as SQLite does. `engine.WithWorkers(n)`
lets its columnar kernels, GROUP BY and compiled predicates split a table's
segments (64k rows each) across n goroutines.

**1,000,000 rows, Intel Xeon D-2123IT** (amd64, 8 cores)

| Query | musql, 1 worker | musql, 4 workers | DuckDB, 1 thread | DuckDB, default | C native |
| --- | ---: | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 458 µs | **166 µs** | 3.36 ms | 2.00 ms | 61.9 ms |
| Filtered count, two predicates | 1.14 ms | **501 µs** | 4.89 ms | 2.52 ms | 71.3 ms |
| Sum over a filter | 652 µs | **232 µs** | 4.34 ms | 2.27 ms | 67.6 ms |
| Grouped aggregate | 4.57 ms | **3.13 ms** | 8.43 ms | 3.59 ms | 424 ms |
| `ORDER BY v DESC LIMIT 20` | 597 µs | **573 µs** | 6.06 ms | 3.08 ms | 87.2 ms |
| `BETWEEN` range | 779 µs | **278 µs** | 4.51 ms | 2.26 ms | 66.6 ms |
| `min`/`max` whole table | **28 µs** | 60 µs | 4.64 ms | 2.11 ms | 90.5 ms |
| Grouped `min`/`max` | 10.6 ms | 4.83 ms | 8.91 ms | **3.78 ms** | 448 ms |
| `OR` predicate | 7.43 ms | **2.56 ms** | 19.2 ms | 7.11 ms | 69.8 ms |
| `IN` list | 16.3 ms | **5.58 ms** | 24.0 ms | 7.45 ms | 114 ms |
| Rowid lookup | 15 µs | 15 µs | 353 µs | 510 µs | **14.4 µs** |

On one thread each, musql is ahead of DuckDB on every row except grouped
`min`/`max` (1.2× behind). With four workers it is ahead of DuckDB's default
multithreaded mode on every row but that one, where DuckDB leads by 1.3×.
Point lookups are DuckDB's weak spot and C SQLite's strength.

Run with `MUSQL_WORKERS=4` for the four-worker column.

## Bulk load and point reads

`bench/pointread` measures what an application does with a key-value table,
through `database/sql` for both engines: one transaction inserting 10M rows
(`k INTEGER PRIMARY KEY, v TEXT`, values like `value-1234567`) through a
prepared statement, then 12 goroutines each reading 50,000 random keys with
`SELECT v FROM kv WHERE k = ?`, every value checked. C SQLite is mattn/go-sqlite3.
Disk is the database and its sidecar files, measured with the connection open.

**Apple M4** (arm64, macOS)

| Metric | musql | C SQLite | musql / C |
| --- | ---: | ---: | ---: |
| Load, 10M rows | 4.52 s | 4.99 s | 0.90 |
| Read avg | 10.5 µs | 28.9 µs | 0.36 |
| Read p50 | 3.5 µs | 17.2 µs | 0.21 |
| Read p99 | 101 µs | 273 µs | 0.37 |
| Read max | 5.9 ms | 32.1 ms | 0.18 |
| Disk | 190.9 MiB | 218.2 MiB | 0.87 |

**Intel Xeon D-2123IT** (amd64, Linux, a shared server)

| Metric | musql | C SQLite | musql / C |
| --- | ---: | ---: | ---: |
| Load, 10M rows | 19.2 s | 22.3 s | 0.86 |
| Read avg | 18.7 µs | 55.4 µs | 0.34 |
| Read p50 | 8.8 µs | 32.5 µs | 0.27 |
| Read p99 | 265 µs | 227 µs | 1.17 |
| Read max | 8.6 ms | 21.0 ms | 0.41 |
| Disk | 190.9 MiB | 218.2 MiB | 0.87 |

The x86 machine is shared and was at a load of 7-9 on its 8 cores; there
musql's p99 came out at 0.98x, 1.16x and 1.17x C's over three runs. That tail
is not the engine's read: twelve readers on eight cores contend on
`database/sql`'s own pool lock (`DB.conn` and `putConn`, 12% of the read
phase's CPU, much of it spinning), which a read 3.5x faster than C's takes
3.5x as often. With the GC off it is about even; the M4, idle, has it at 0.37x.

Before the work that brought these, musql took 2.2x C's time to load, lost on
p99 and max, and wrote 2.1x C's bytes. What changed:

- **Load.** A transaction's inserts into an empty table build segments
  directly (engine/insert_bulk_direct.go) instead of going through the row
  store, the change log and the spill file and being rebuilt at COMMIT.
- **Disk** (47 to 20 bytes a row). Segment file version 6 keeps consecutive
  rowids as one, drops the width block of full rows, stores a TEXT column as
  4-byte heap bounds instead of 8-byte cells, and keeps 3-byte row positions
  in the TEXT equality index instead of a value, a prefix sum and a position.
- **Reads and their tail.** Each statement's freshness check stamped two
  files, a stat and an fstat that twelve readers queued on in the kernel; it
  now reads a word the writers keep in the lock file. All connections share
  one mapping of the file, so a page faults once and not once per connection.
  A new connection's first read no longer maps the file twice, and a closing
  connection that had only read no longer takes the exclusive commit lock,
  which made every connection opening at that moment sleep a lock-poll
  interval.

`bench/pointread/run` runs a short version (1M rows) and fails if musql
loses on load, avg, p50, p99 or disk; CI runs it, and
`POINTREAD_FULL=1 bench/pointread/run` is the full size. It gates two ways of
reading, in both engines: every reader through one shared `*sql.DB` (the tables
above, and the upstream shape), and each reader with its own `*sql.Conn`.

The shared `*sql.DB` was ungated until the driver's read gate
(`driver/read_gate.go`). With more readers than cores it measured the pool, not
the engine: every `QueryRow` takes `database/sql`'s pool lock twice (`DB.conn`
and `putConn`), and `sync.Mutex.Unlock` does not hand the processor to the
waiter it wakes. The waiter sat runnable while the goroutine that released the
lock kept running, and a musql read never blocks or makes a system call, so it
kept running until it next parked on that lock; a trace put 60% of the reads
over 300 µs on such a wait. A C read is a cgo call, which gives its processor
away when it runs long, and that kept C's tail short. The gate lets at most
GOMAXPROCS pooled reads run at once and parks the rest on itself instead of
leaving them runnable, so a reader that finds the pool lock held spins instead
of parking and the waiting comes in rare, long turns. On .10 pinned to 4 cores
(`taskset -c 4-7`, `GOMAXPROCS=4`, the short run, 3 runs each):

| readers | mode | engine | reads/s | p50 | p99 | p99.9 |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| 12 | shared `*sql.DB` | musql, no gate | 545-597k | 4.2-4.4 µs | 414-441 µs | 1.8-2.3 ms |
| 12 | shared `*sql.DB` | musql | 533-584k | 4.2-4.4 µs | 23-26 µs | 0.47-0.68 ms |
| 12 | shared `*sql.DB` | C | 181-187k | 17.2-17.5 µs | 44-53 µs | 13.3-14.5 ms |
| 12 | `*sql.Conn` per reader | musql | 677-700k | 3.2 µs | 22-25 µs | 2.6-2.8 ms |
| 12 | `*sql.Conn` per reader | C | 183-186k | 16.2-16.4 µs | 43-46 µs | 12.2-13.5 ms |

## Notes

- The Go bridge costs C SQLite microseconds per statement: point lookups take
  roughly twice as long through mattn and `database/sql` as natively. On scans
  it is lost in the noise.
- Without the JIT, scans run on the ordinary VDBE and musql loses to C SQLite
  by a large multiple.
- Point lookups and joins do not use the JIT.
- Microsecond-scale results vary between runs, more so on the amd64 machine.

## Reproducing

From the repository root (requires CGo and a C compiler):

```sh
cd compat-harness
BENCH_ROWS=100000 \
BENCH_SKIP='min/max,GROUP BY with HAVING,OR predicate,BETWEEN,IN list,IS NULL,LIKE,ORDER BY text,group_concat,DISTINCT,subquery,EXISTS,UNION,OFFSET,join projecting,UPDATE,INSERT,DELETE' \
  go test -run '^TestBenchColumnarVsC$' -count=1 -v -timeout 2h .
```

`BENCH_ROWS` sets the table size (default 100,000), and `MUSQL_WORKERS` the
number of musql workers (default 1). Without `BENCH_SKIP` the
harness measures every workload, which takes hours. `MUSQL_JIT=0` runs musql
without the JIT; that run reports FAIL because the harness asserts the JIT
served each JIT workload, but its timings are still valid.
