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

## TinyGo build of the wasm module

The npm package's module compiles with TinyGo (one `os.SameFile` stub
needed). Its garbage collectors and size against the Go toolchain's build are
being measured; adopt it only if it is faster on bench_node.js with every
answer the same.
