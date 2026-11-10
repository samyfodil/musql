# Architecture

A map of the code: where a statement goes from the moment `database/sql` hands
it over to the rows coming back, and where the data lives. It names the file
that owns each step, so you can open the right one. The rules that hold
throughout are in [AGENTS.md](../AGENTS.md); the storage format's design is in
[format-design.md](format-design.md).

## One read, end to end

Follow `SELECT sum(x) FROM t WHERE y > ?` from the driver to its answer.

```
database/sql
  driver/stmt.go        Stmt.QueryContext: bind the arguments
  driver/conn.go        Conn.queryArgs: pragmas and connection state, then the engine
engine
  query.go              ReadOnlyPager.QueryArgs: the plan cache, else parse
  sql_lexer.go
  sql_parser.go         ParseSelect -> SelectStmt (sql_ast.go)
  query.go              execSelect: dispatch by shape (compound, FROM-less, scan)
  vdbe_run.go           tryVDBEScan -> compileSelectScan (vdbe_scan.go)
  where_plan*.go        the loop order and index choice (ported from where.c)
  vdbe_*codegen.go      the AST lowered to a Program (vdbe_op.go)
  segment_peephole.go   peepholes over the finished Program
  vdbe.go               vdbe.run: interpret the Program ...
  vdbe_jit.go           ... entering its native code where it has some
```

### 1. The driver

`driver/` is the `database/sql` driver, registered as `"sqlite"`. A `Conn`
holds one engine session (`driver/session.go`). `Stmt.QueryContext` binds the
arguments and calls `Conn.queryArgs` (`driver/conn.go`). That function handles
what belongs to the connection rather than to one statement, such as pragmas,
transactions and attached databases. Then it calls `ReadOnlyPager.QueryArgs`
on the session's current snapshot.

`boost/` is a faster read API with its own connection pool. It reaches the
driver through `internal/boostcore`.

### 2. Parse

`engine/query.go`'s `QueryArgs` first asks the plan cache (`cachedPlan`, keyed
on the statement's text). A hit is a finished Program, so it goes straight to
step 6.

Otherwise `ParseSelect` (`sql_parser.go`, over `sql_lexer.go`) builds a
`SelectStmt` (`sql_ast.go`). Writes and DDL have parsers of their own beside
their execution: `parseInsertStmt` in `insert_write.go`, `parseCreateIndexStmt`
in `index_write.go`, and so on.

### 3. Rewrite and plan

`execSelect` (`query.go`) picks the path by the statement's shape:
- a compound (`UNION` and the rest) goes to `tryVDBECompound`;
- a statement with no `FROM` goes to `tryVDBENoFrom`;
- everything else goes to `tryVDBEScan` (`vdbe_run.go`).

On the way, derived tables, views and CTEs are flattened into the query that
reads them, as C SQLite's `flattenSubquery` does (`flatten_*.go`, `cte.go`,
`view.go`).

The planner is a port of `where.c`. `where_plan.go` turns the WHERE into
terms. `where_plan_index.go` and `where_cost.go` cost each loop order and index;
`where_plan_stat1.go` uses `ANALYZE`'s statistics. The order loops run in is
visible in some answers, so the port follows C's choices exactly.

### 4. Compile to a Program

The compiler (`compiler`, `vdbe_codegen.go`) lowers the AST to a `Program`: a
list of `Instruction`s whose opcodes are C SQLite's, with musql's own added
(`vdbe_op.go`). Names are resolved here, scope by scope, as C resolves them
(`column_scope.go`).

| What | File |
| --- | --- |
| expressions, functions, subqueries | `vdbe_codegen.go` |
| one table's scan loop | `vdbe_scan.go` |
| joins | `vdbe_join_codegen.go`, `vdbe_join_seek.go` |
| aggregates and GROUP BY | `vdbe_agg_codegen.go`, `vdbe_agg_item_codegen.go`, `vdbe_agg_hoist.go`, `vdbe_hashagg.go` |
| ORDER BY and the sorter | `vdbe_sort_codegen.go`, `vdbe_sorter.go` |
| window functions | `vdbe_window_codegen.go`, `vdbe_window*.go` |
| compound SELECTs | `vdbe_compound_codegen.go` |
| recursive CTEs | `vdbe_recursive_cte.go` |
| INSERT, UPDATE, DELETE, upserts | `vdbe_write.go`, `insert_write.go`, `write_update_delete.go` |
| triggers and foreign keys | `vdbe_trigger.go`, `trigger.go`, `fk.go` |

A shape the compiler cannot lower is an error (`errVDBEUnsupported`), never a
route to some other executor.

`EXPLAIN` prints a Program (`vdbe_disasm.go`, `explain.go`). It is the quickest
way to see what a statement compiled to.

### 5. Peepholes

After compiling, `compileScanAttempt` (`vdbe_scan.go`) runs the peepholes over
the finished Program. Each one recognises an exact opcode sequence, which its
doc comment shows. It either rewrites the Program or leaves it untouched. Most
add a guard opcode in front of the loop. At run time the guard answers the
statement with a kernel when the data allows it, or declines, and the original
loop runs as compiled.

| Peephole | Shape | File |
| --- | --- | --- |
| `segPeephole` | count or sum under a filter | `segment_peephole.go` |
| `segVectorPeephole` | vector distance top-k | `segment_vector.go` |
| `segOrderPeephole` | ORDER BY ... LIMIT | `segment_order_peephole.go` |
| `segGroupPeephole` | GROUP BY over a hash | `segment_group_peephole.go` |
| `segDistinctPeephole` | DISTINCT over one column | `segment_distinct.go` |
| `segJoin*Peephole` | joins: aggregate, GROUP BY, hash, top-k | `segment_join_peephole.go` |
| `segProgPeephole` | any aggregate loop whose body lowers | `segment_prog_peephole.go` |
| `segSeriesPairPeephole` | a loop over two series | `segment_series.go` |
| `segRowFilterPeephole` | a WHERE that selects rows | `segment_row_filter.go` |
| `existsInlinePeephole` | correlated EXISTS as a subroutine | `exists_inline.go` |
| `recInlinePeephole` | a recursive CTE's step, inline | `rec_inline.go` |

`segProgPeephole` and `segRowFilterPeephole` lower the loop body to the program
JIT's IR (`jit.ProgInsn`) in `segment_lower.go` (REALs in
`segment_lower_real.go`). `internal/jit` compiles that IR to a columnar kernel,
which runs over a segment's column blocks.

### 6. Run

`vdbe.run` (`vdbe.go`) is the interpreter: one `switch`, one case per opcode,
as C's `sqlite3VdbeExec`. Cursors are in `vdbe_cursor.go`. They read a table's
rows from its segments (`segment_cursor.go`, `segment_colreader.go`) with the
session's changes on top.

The whole-program JIT (`vdbe_jit.go`) compiles a Program the first time it
runs. The native code is cached on the Program, so a prepared statement
compiles once. `run` enters native code at a pc and the code runs as far as it
can: every instruction it has a native form for, over the types it handles. At
anything else it exits back to `run` with the pc to resume at, and the
interpreter carries on. REAL arms are in `vdbe_jit_real.go`.
`WithoutJIT()` turns both JITs off.

## Writes

`Conn.execArgs` (`driver/conn.go`) calls `DB.ExecArgs` (`insert_write.go`),
which compiles the statement to a write Program like any other. `DB`
(`writer.go`) is the engine state of one connection: its catalog, its row
stores and its pragmas. `Session` (`segment_write.go`) embeds a `DB` and owns
the file.

- Statements change rows in the row store (`row_store.go`): the table's
  segments with the session's changes layered on top (`row_store_seg.go`). The
  store also keeps an undo log (`row_store_undo.go`) and the indexes that
  UNIQUE and seeks need (`row_store_uniqindex.go`, `row_store_eqindex.go`).
- Every changed row is recorded as a `RowChange` (`rowhook.go`). A commit
  appends those changes to the delta log as one batch and fsyncs
  (`segment_delta_write.go`). It doesn't rewrite the segments.
- Compaction folds the delta into new segments (`segment_compact.go`). A
  catalog change that leaves the tables as they are (a view, a trigger,
  `user_version`) is a catalog record in the delta
  (`segment_delta_catalog.go`); any other DDL rewrites the file
  (`segment_ddl.go`).
- Locking across processes uses OFD byte-range locks (`lock.go`,
  `internal/filelock`).

## Storage

A database is a segment file (`.musq`) and its delta log.

- `segment_file.go`: the file, its catalog and its format version
  (`segFileVersion`). Any change a reader would see takes a new version, and
  every old version still opens (rule 4 in AGENTS.md).
- `segment.go`: a segment, each column in a contiguous fixed-width block that
  kernels read in place. `segment_zone.go` keeps each block's minimum and
  maximum per run of rows.
- `segment_delta*.go`: the delta log, and how its rows merge over the
  segments.
- `segment_open.go`: `Open` maps the file and returns a `ReadOnlyPager`, the
  snapshot a read runs on.

The engine knows nothing of C SQLite's file format. `convert/sqlite` imports
and exports it, and calls only the engine's build and read API (`build.go`,
`read_view.go`).

## The JIT, `internal/jit`

There are two compilers, plus the kernels behind them.

| What | Files |
| --- | --- |
| the whole-program JIT: VDBE instructions as native code, with the IR in `vm.go` | `vm_amd64.go`, `vm_arm64.go`, `vm_wasm.go`; `vm_real_*` and `vm_text_*` for REAL and TEXT |
| the program JIT: a loop body over column blocks, with the IR in `jit.go` | `program_amd64.go`, `program_arm64.go`, `program_wasm.go`; `fuse.go` and `regalloc.go` |
| fixed kernels: filtered counts, vector distance | `filter_*.go`, `vector_*.go`, `vec1*.go` |
| math kernels: `internal/fir` is their IR and `internal/libm` writes them, bit for bit as glibc | `fir_amd64.go`, `fir_arm64.go`, `fir_wasm.go` |
| encoders | `asm_amd64.go`, `asm_arm64.go`, `asm_wasm.go`, `asm_*` |
| mapping code W^X, and the trampoline into it | `exec_*.go`, `trampoline_*.s` |
| CPU features | `cpu_*.go` |

On js/wasm a kernel is a wasm module the JS host compiles and calls
(`exec_js.go`, `examples/wasm/musql.js`).

## Everything else

The rest of `engine/` is features: FTS3/4 (`fts3_*.go`) and FTS5 (`fts5_*.go`),
R-Tree (`rtree_*.go`), JSON (`json*.go`), virtual tables (`vtab*.go`), vectors
(`vector*.go`, `vec1_*.go`), pragmas (`pragma*.go`), ATTACH (`attach*.go`), the
temp database (`temp_*.go`), and scalar functions (`scalar_*.go`).

Outside it:

| Path | What it is |
| --- | --- |
| `replication/` | replication: capture, op log, CRDT and leader modes |
| `hrana/` | the Hrana server, so libSQL and Turso clients can connect |
| `npm/` | the npm package, with `cloud/console` built on it |
| `cloud/` | musql cloud |

## Testing against C SQLite

A change is checked by running both engines and comparing their answers.

- `compat-harness/` holds the differential tests against C SQLite through
  `mattn/go-sqlite3`, in two builds: the default one and `-tags sqlite_fts5`.
  Both need `sqlite_math_functions` too.
- `scripts/sweep` replays SQL mined from SQLite's own test suite through both
  engines.

AGENTS.md has the commands and the gates a change must pass.
