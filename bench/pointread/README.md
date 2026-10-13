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
p99 latency, and disk size (`check/`), with each reader holding its own
`*sql.Conn` (`-conn`) in both engines. The other percentiles and max are
printed but not gated, since one scheduler hiccup decides them on a shared
machine. The same reads through one shared `*sql.DB` are run and printed too,
not gated:

With more readers than cores, the shared `*sql.DB` measures the pool, not
the engine. Every `QueryRow` takes `database/sql`'s pool lock twice (`DB.conn`
and `putConn`), and `sync.Mutex.Unlock` does not hand the processor to the
waiter it wakes: the waiter sits runnable while the goroutine that released the
lock keeps running, and a musql read never blocks or makes a system call, so it
keeps running until it is preempted or next blocks. A C read is a cgo call, and
a cgo call that runs past ~20 us gives its processor to the next runnable
goroutine, which is what keeps C's tail short there. On .10 pinned to 4 cores
(`taskset -c 0-3`, `GOMAXPROCS=4`, the short run, 3 runs each):

| readers | mode | engine | reads/s | p50 | p99 | p99.9 |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| 12 | shared `*sql.DB` | musql | 403-430k | 6.2 µs | 327-407 µs | 3.6-4.0 ms |
| 12 | shared `*sql.DB` | C | 163-164k | 19.1 µs | 62-74 µs | 15.1-15.3 ms |
| 12 | `*sql.Conn` per reader | musql | 385-455k | 5.2 µs | 32-36 µs | 3.9-5.0 ms |
| 12 | `*sql.Conn` per reader | C | 164-169k | 17.1 µs | 48-52 µs | 14.6-15.1 ms |
| 4 | shared `*sql.DB` | musql | 330-348k | 6.2 µs | 30-34 µs | 0.74-0.96 ms |
| 4 | shared `*sql.DB` | C | 117-121k | 23.2 µs | 72-84 µs | 2.8-2.9 ms |

A trace of the shared 12-reader run puts 60% of the reads over 300 µs on a
block in `DB.conn` or `DB.putConn` that lasted about 2 µs, followed by about
580 µs runnable while four other readers held the cores. So the gate times one
retained `*sql.Conn` per reader (`-conn`) for both engines, and the shared run
is still printed, ungated, beside it.


This is its own module so that the C SQLite driver's CGo stays out of the main
module. `replace` points it at the working tree, so it measures the code beside
it. The full-size run is heavy (several GB of RAM, about 0.5 GB of disk); run it
on a server.

Profiling: `-cpuprofile file` covers the whole run, `-trace file` the read phase.
`-conn` gives each reader its own `*sql.Conn`, taken and warmed before timing,
so the timed reads never enter `database/sql`'s pool or its lock.
