# Benchmarks

Read workloads over 100,000 rows, compared with C SQLite (mattn/go-sqlite3) and
Turso (its Go driver). Every workload is checked against C SQLite on 40 bind
values before timing, then timed for about 200 ms rather than a fixed iteration
count. All engines read disk-backed files with a warm cache; musql is measured
through its direct engine interface, once with the JIT and once with
`MUSQL_JIT=0`. The × columns compare the JIT run.

**Apple M4** (arm64, macOS)

| Query | musql (JIT) | musql (no JIT) | C SQLite | Turso | JIT vs C | JIT vs Turso |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 28 µs | 9.83 ms | 2.17 ms | 5.74 ms | **77×** | **205×** |
| Filtered count, two predicates | 28 µs | 11.0 ms | 2.58 ms | 7.05 ms | **92×** | **252×** |
| Rowid lookup | 5 µs | 5 µs | 5 µs | 9 µs | **1.0×** | **1.8×** |
| Secondary-index equality | 6 µs | 6 µs | 5 µs | 10 µs | **1.2× slower** | **1.7×** |
| Indexed equi-join | 7 µs | 7 µs | 8 µs | 16 µs | **1.1×** | **2.3×** |
| Sum over a filter | 287 µs | 10.6 ms | 2.43 ms | 6.52 ms | **8.5×** | **23×** |
| Grouped aggregate | 6.04 ms | 15.3 ms | 12.9 ms | 26.1 ms | **2.1×** | **4.3×** |
| `ORDER BY v DESC LIMIT 20` | 916 µs | 4.76 ms | 2.50 ms | 22.6 ms | **2.7×** | **25×** |
| Whole-table count | 5 µs | 74.5 ms | 9 µs | 34 µs | **1.8×** | **6.8×** |

**Intel Xeon D-2123IT** (amd64, Linux)

| Query | musql (JIT) | musql (no JIT) | C SQLite | Turso | JIT vs C | JIT vs Turso |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Filtered count, one predicate | 89 µs | 51.6 ms | 6.19 ms | 19.5 ms | **70×** | **219×** |
| Filtered count, two predicates | 115 µs | 55.3 ms | 7.48 ms | 23.2 ms | **65×** | **202×** |
| Rowid lookup | 37 µs | 31 µs | 21 µs | 36 µs | **1.8× slower** | **1.0×** |
| Secondary-index equality | 60 µs | 74 µs | 21 µs | 66 µs | **2.9× slower** | **1.1×** |
| Indexed equi-join | 49 µs | 47 µs | 39 µs | 77 µs | **1.3× slower** | **1.6×** |
| Sum over a filter | 893 µs | 53.7 ms | 6.79 ms | 24.6 ms | **7.6×** | **28×** |
| Grouped aggregate | 16.3 ms | 69.8 ms | 39.6 ms | 101.7 ms | **2.4×** | **6.2×** |
| `ORDER BY v DESC LIMIT 20` | 4.74 ms | 12.1 ms | 9.65 ms | 85.9 ms | **2.0×** | **18×** |
| Whole-table count | 29 µs | 276.8 ms | 47 µs | 144 µs | **1.6×** | **5.0×** |

## Notes

- Without the JIT, scans run on the ordinary VDBE. A whole-table `count(*)`
  decodes every row there, where C SQLite only counts b-tree cells.
- Point lookups and joins do not use the JIT.
- Microsecond-scale results vary between runs, more so on the amd64 machine.

## Reproducing

From the repository root (requires CGo):

```sh
cd compat-harness
BENCH_SKIP='min/max,GROUP BY with HAVING,OR predicate,BETWEEN,IN list,IS NULL,LIKE,ORDER BY text,group_concat,DISTINCT,subquery,EXISTS,UNION,OFFSET,join projecting,UPDATE,INSERT,DELETE' \
  go test -run '^TestBenchColumnarVsC$' -count=1 -v -timeout 2h .
```

Run it again with `MUSQL_JIT=0` for the no-JIT column (the harness maps it
to `engine.WithoutJIT()`). Without `BENCH_SKIP`
the harness measures every workload, which takes hours. The no-JIT run reports
FAIL because the harness asserts that the JIT served each JIT workload; its
timings are still valid.
