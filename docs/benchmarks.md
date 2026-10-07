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
| Filtered count, one predicate | 29 µs | 2.69 ms | 2.16 ms | 5.75 ms | **94× faster** |
| Filtered count, two predicates | 28 µs | 2.59 ms | 2.58 ms | 7.02 ms | **92× faster** |
| Rowid lookup | 5 µs | 3.0 µs | 5 µs | 9 µs | **1.7× slower** |
| Secondary-index equality | 5 µs | 2.8 µs | 5 µs | 10 µs | **1.9× slower** |
| Indexed equi-join | 7 µs | 4.2 µs | 8 µs | 15 µs | **1.7× slower** |
| Sum over a filter | 292 µs | 2.40 ms | 2.38 ms | 6.55 ms | **8.2× faster** |
| Grouped aggregate | 6.10 ms | 13.1 ms | 13.2 ms | 26.3 ms | **2.1× faster** |
| `ORDER BY v DESC LIMIT 20` | 894 µs | 2.53 ms | 2.54 ms | 22.4 ms | **2.8× faster** |
| Whole-table count | 5 µs | 7.7 µs | 10 µs | 34 µs | **1.5× faster** |

**Intel Xeon D-2123IT** (amd64, Linux)

| Query | musql | C native | C via Go | Turso | vs C native |
| --- | ---: | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 56 µs | 6.12 ms | 6.62 ms | 20.5 ms | **110× faster** |
| Filtered count, two predicates | 116 µs | 7.11 ms | 7.98 ms | 25.0 ms | **61× faster** |
| Rowid lookup | 35 µs | 11.2 µs | 27 µs | 41 µs | **3.2× slower** |
| Secondary-index equality | 37 µs | 10.4 µs | 26 µs | 49 µs | **3.5× slower** |
| Indexed equi-join | 44 µs | 15.2 µs | 43 µs | 77 µs | **2.9× slower** |
| Sum over a filter | 887 µs | 6.46 ms | 7.31 ms | 24.3 ms | **7.3× faster** |
| Grouped aggregate | 17.9 ms | 39.7 ms | 41.9 ms | 110 ms | **2.2× faster** |
| `ORDER BY v DESC LIMIT 20` | 4.56 ms | 8.70 ms | 9.51 ms | 94.8 ms | **1.9× faster** |
| Whole-table count | 30 µs | 23.8 µs | 40 µs | 242 µs | **1.3× slower** |

## 1,000,000 rows

The scan multiples hold or grow with ten times the rows.

**Apple M4** (arm64, macOS)

| Query | musql | C native | Turso | vs C native |
| --- | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 247 µs | 23.2 ms | 60.2 ms | **94× faster** |
| Filtered count, two predicates | 249 µs | 27.5 ms | 74.0 ms | **111× faster** |
| Rowid lookup | 5 µs | 3.4 µs | 11 µs | **1.5× slower** |
| Secondary-index equality | 5 µs | 3.4 µs | 12 µs | **1.3× slower** |
| Indexed equi-join | 8 µs | 84 µs | 107 µs | 11× faster ¹ |
| Sum over a filter | 3.10 ms | 25.2 ms | 69.7 ms | **8.1× faster** |
| Grouped aggregate | 62.1 ms | 140 ms | 382 ms | **2.3× faster** |
| `ORDER BY v DESC LIMIT 20` | 8.81 ms | 27.1 ms | 287 ms | **3.1× faster** |
| Whole-table count | 3 µs | 1.37 ms | 5.11 ms | **423× faster** |

**Intel Xeon D-2123IT** (amd64, Linux)

| Query | musql | C native | Turso | vs C native |
| --- | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 452 µs | 63.4 ms | 226 ms | **140× faster** |
| Filtered count, two predicates | 1.17 ms | 73.0 ms | 277 ms | **63× faster** |
| Rowid lookup | 14 µs | 12.3 µs | 44 µs | **1.2× slower** |
| Secondary-index equality | 19 µs | 12.0 µs | 53 µs | **1.5× slower** |
| Indexed equi-join | 56 µs | 18 µs | 85 µs | **3.1× slower** |
| Sum over a filter | 7.48 ms | 77.5 ms | 278 ms | **10× faster** |
| Grouped aggregate | 168 ms | 430 ms | 1.37 s | **2.6× faster** |
| `ORDER BY v DESC LIMIT 20` | 34.3 ms | 89.6 ms | 1.17 s | **2.6× faster** |
| Whole-table count | 21 µs | 5.77 ms | 20.4 ms | **280× faster** |

¹ C SQLite took 84 µs for this join on the M4 at 1M rows, against 18 µs on
the amd64 machine; it has not been investigated, so it is left out of any
summary.

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
