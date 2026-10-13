# pointread

Bulk load, then random point reads, through `database/sql`, for musql and for
C SQLite (mattn/go-sqlite3). It is what an application does with a key-value
table: one transaction inserting `-rows` rows through a prepared statement, then
`-workers` goroutines each reading `-reads` random keys with
`SELECT v FROM kv WHERE k = ?`. Every value read is checked against the one
written.

It reports load time, read latency (avg, p50, p90, p95, p99, p99.9, max) and the
bytes on disk of the database and its sidecar files, measured while the
connection is still open.

```sh
bench/pointread/run                     # 1M rows, 12 x 20k reads: a few minutes
POINTREAD_FULL=1 bench/pointread/run    # 10M rows, 12 x 50k reads
make bench-pointread                    # the short run
```

`run` fails unless musql matches or beats C SQLite on load time, avg, p50 and
p99 latency, and disk size (`check/`). The other percentiles and max are
printed but not gated, since one scheduler hiccup decides them on a shared
machine.

This is its own module so that the C SQLite driver's CGo stays out of the main
module. `replace` points it at the working tree, so it measures the code beside
it. The full-size run is heavy (several GB of RAM, about 0.5 GB of disk); run it
on a server.

Profiling: `-cpuprofile file` covers the whole run, `-trace file` the read phase.
`-conn` gives each reader its own `*sql.Conn`, taken and warmed before timing,
so the timed reads never enter `database/sql`'s pool or its lock.
