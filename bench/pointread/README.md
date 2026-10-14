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
p99 latency, and disk size (`check/`), twice: with every reader going through
one shared `*sql.DB`, and with each reader holding its own `*sql.Conn`
(`-conn`), in both engines. The other percentiles and max are printed but not
gated, since one scheduler hiccup decides them on a shared machine.

With more readers than cores, the shared `*sql.DB` used to measure the pool,
not the engine. Every `QueryRow` takes `database/sql`'s pool lock twice
(`DB.conn` and `putConn`), and `sync.Mutex.Unlock` does not hand the processor
to the waiter it wakes: the waiter sits runnable while the goroutine that
released the lock keeps running, and a musql read never blocks or makes a
system call, so it keeps running until it next parks on that lock. A C read is a
cgo call, and a cgo call that runs long gives its processor to the next
runnable goroutine, which is what kept C's tail short there. The driver's read
gate (`driver/read_gate.go`) answers it: at most GOMAXPROCS pooled reads run at
once, the others wait parked on the gate rather than runnable, and a reader
keeps its slot between statements for up to 8 ms. A reader that finds the pool
lock held then spins instead of parking, and the waiting comes in rare long
turns instead of on a few percent of all reads. On .10 pinned to 4 cores
(`taskset -c 4-7`, `GOMAXPROCS=4`, the short run, 3 runs each):

| readers | mode | engine | reads/s | p50 | p99 | p99.9 |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| 12 | shared `*sql.DB` | musql, no gate | 545-597k | 4.2-4.4 µs | 414-441 µs | 1.8-2.3 ms |
| 12 | shared `*sql.DB` | musql | 533-584k | 4.2-4.4 µs | 23-26 µs | 0.47-0.68 ms |
| 12 | shared `*sql.DB` | C | 181-187k | 17.2-17.5 µs | 44-53 µs | 13.3-14.5 ms |
| 12 | `*sql.Conn` per reader | musql | 677-700k | 3.2 µs | 22-25 µs | 2.6-2.8 ms |
| 12 | `*sql.Conn` per reader | C | 183-186k | 16.2-16.4 µs | 43-46 µs | 12.2-13.5 ms |

Over 8 alternating runs the gate's throughput matched the ungated driver's
(median 578k against 575k reads/s) and its mean was lower (18.4 µs against
19.4 µs). With 48 readers the shared p99 went from 2.6-3.6 ms to 25-28 µs.

This is its own module so that the C SQLite driver's CGo stays out of the main
module. `replace` points it at the working tree, so it measures the code beside
it. The full-size run is heavy (several GB of RAM, about 0.5 GB of disk); run it
on a server.

Profiling: `-cpuprofile file` covers the whole run, `-trace file` the read phase.
`-conn` gives each reader its own `*sql.Conn`, taken and warmed before timing,
so the timed reads never enter `database/sql`'s pool or its lock.
