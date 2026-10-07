# AGENTS.md

Guidance for coding agents (and humans) working in this repository.

## What this is

musql is a SQLite-compatible database written in pure Go, with no CGo. It
executes on its own format (`.musq` segment + delta) and
converts the C SQLite file format in and out, so its databases stay
interchangeable with C SQLite. That interchange is tested in both directions in
`compat-harness/`.

## Rule 1: the VDBE is the only executor

SQL executes exactly one way: SQL → VDBE program → run the program.

- No AST interpreter, no tree-walker, no fallback executor, not even
  temporarily. If the compiler cannot lower a statement or expression, that is
  an **error**, never a route to somewhere else.
- At most one AST→value function is allowed, with the same contract as C
  SQLite's `sqlite3ValueFromExpr`: literal tokens, unary `+`/`-`, and `CAST` of
  a literal, evaluated at prepare or DDL time only. Every other expression
  compiles to opcodes.
- `engine/no_interpreter_gate_test.go` holds the ratchets at 0. Do not raise
  them, and do not add escape-hatch opcodes.

## Rule 2: no generated Go as an execution path

The engine runs a program two ways only: the VDBE, or the JIT-compiled VDBE.

- No `go:generate` that emits execution code, no generator whose Go output the
  engine runs, no per-operator-combination loops.
- No checked-in `.s` files except the JIT trampoline, one per architecture
  (`internal/jit/trampoline_*.s`).
  On js/wasm the "trampoline" is the `musqljit.call_kernel` import (with
  `compile_kernel`) plus a few lines of JS glue, `examples/wasm/musql.js`:
  kernels are wasm modules emitted by `internal/jit` like any other.
- Specialising a query shape is the JIT's job (`internal/jit`). It is reached by
  a peephole over the compiled program (`engine/segment_peephole.go`), never by
  a branch in the compiler. The peephole leaves anything it does not recognise
  unchanged. `engine.Configure(engine.WithoutJIT())` turns the JIT off.
- Library packages read no environment variables; behavior is set through
  `With...` options. Only commands (`cmd/`, examples) and test tooling read the
  environment (the test harness maps `MUSQL_JIT=0` onto `WithoutJIT`).

## Rule 3: the SQLite file format is import/export only

- The engine has no relationship to the SQLite file format: no page reader,
  b-tree, header, WAL or journal code lives in `engine/`.
- The converter is `convert/sqlite` (`Import`, `Export`). It imports `engine`,
  never the reverse, and only `cmd/musql-convert` and `compat-harness` call it.
  `engine/no_sqlite_format_test.go` enforces this.
- A SQLite file handed to the driver or to ATTACH is an error like any other
  foreign file.
- When the format seems to lack something SQLite has, it is usually a missing
  catalog field, not a missing capability. Add the field.
- The remaining deliberate declines are listed in
  `docs/segment-format-compatibility.md`.

## Layout

| Path | What it is |
| --- | --- |
| `engine/` | The database: format (`segment_*.go`), build/read API for converters (`build.go`, `read_view.go`), parser, VDBE (`vdbe*.go`), write path, triggers, FTS3/4/5, rtree, JSON. |
| `convert/sqlite/` | SQLite-format converter: page and b-tree reader/writer, WAL and hot-journal reading, integrity walk, `Import`/`Export`. |
| `cmd/musql-convert/` | The conversion CLI. |
| `driver/` | The `database/sql` driver (registered as `"sqlite"`). |
| `hrana/` | The Hrana server (HTTP, JSON and Protobuf) and `cmd/musqld`, so libSQL/Turso clients can use musql. Its own module, so the engine and driver depend on nothing but `golang.org/x/sys`. Generated code in `hrana/gen/`. |
| `proto/` | Protobuf definitions. `buf generate` (repo root) writes the Go code into `hrana/gen/` and `examples/libp2p` has its own config. Never edit generated code by hand. |
| `replication/` | Replication: capture, HLC, op log, CRDT and leader modes. The network is supplied by the caller (`WithTransport`). |
| `examples/libp2p/` | An example `Transport` over libp2p, its own module. |
| `internal/jit/` | The JIT: x86-64 and AArch64 emitters, W^X mapping, trampolines. |
| `internal/filelock/` | OFD byte-range locks used by the write path. |
| `compat-harness/` | Differential tests against C SQLite (`mattn/go-sqlite3`). A separate module, and the only one that needs CGo. |

## Invariants

1. **Never wrong.** A wrong answer is worse than an error. If a shape cannot be
   made exact, decline it cleanly (`errVDBEUnsupported`). Features that parse
   and are then silently ignored are the failure mode to fear.
2. **Never panic.** The conformance gates treat a panic as the hardest failure.
   A nil `*ReadOnlyPager` is reachable from the write path; code there must
   tolerate it.
3. **Tests never write into the source tree.** Use `t.TempDir()`. `make test`
   checks for stray database files.

## Read SQLite's source first

Before changing engine code for a divergence, find the C function that owns
the behavior in SQLite's source (the oracle is SQLite 3.53.3):

```sh
git clone --depth 1 --branch version-3.53.3 https://github.com/sqlite/sqlite.git
```

Probes confirm a ported algorithm; they do not invent one. A useful general
rule from the source: SQLite resolves each name once and never re-resolves, so
every site here that derives column metadata from the AST must re-enter the
same scope.

## Commands

```sh
go build ./...
go test ./...                 # unit tests
make test                     # go test + examples/libp2p + stray-db check
make vet
make harness                  # differential tests vs C SQLite (needs CGo)
make build_all_targets        # cross-build every supported GOOS/GOARCH
```

The compat harness:

```sh
cd compat-harness && go test -short -count=1 -timeout 7200s ./
```

- `-timeout` matters: the `-short` set runs well past Go's default 600 s, and a
  timed-out run reports only whatever ran before the cut.
- Run both builds: the default one and `-tags sqlite_fts5`.
- Write the full log to a file and count `--- FAIL` lines; do not trust a
  truncated tail, and check for `panic:`.

The whole-corpus sweep (`TestTCLCorpus`) is the gate that most often catches
what the faster ones miss:

```sh
scripts/sweep                 # every chunk must report wrong=0 panics=0
scripts/sweep '[a-c]' t       # a subset while iterating
```

Use the script rather than a hand-written `-run` pattern: it anchors
`TestTCLCorpus$` so `TestTCLCorpusTxnLockstep` is not replayed once per chunk.

The `-short` harness and the sweep catch different things (error text and
schema text are not compared by the corpus), so a change to the engine needs
both.

## Conformance gates

- `TestCorpus`, `TestSLT`, `TestFuzzDifferential`: 0 divergences, always.
- `TestTCLCorpus`: SQL mined from SQLite's own `.test` files and replayed
  against C SQLite. `pass` is a full output match; `unsupported` is a separate
  bucket.

To prioritise work, classify divergences (statement kind × which engine
errored, and the compiler's decline reason) rather than guess.

## Style

- Smallest correct diff, no speculative abstractions.
- Comments explain why a behavior is what it is; match the surrounding density.
- Do not run `gofmt -w` over whole packages: it rewrites `''` in doc comments
  to a curly quote and corrupts SQL examples. Format the files you touched and
  check the diff.
- Say "C SQLite", not "real SQLite".
