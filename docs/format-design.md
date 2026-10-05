# A second on-disk format, and the JIT that reads it

Status: **stages 1-5 done, with stage 4 half-built and the filter not yet
reachable from SQL -- see 6b.** Stage 1's
gate passed; stage 2 round trips, byte-identical where that can hold; stage 3
reads the whole mined corpus through the format with `wrong=0`; stage 4 refuses
a stale segment file but has no delta or compaction, so a persisted file is
invalidated by any write; stage 5 is measured rather than built, and the
measurement says a code generator is worth 1.9x on multi-predicate filters and
nothing on single ones. Every number in it was measured
on an Intel Atom C3558 over the 100,000-row bench table, so it is
comparable with `TestBenchVsC` and with `engine/jit_ceiling_bench_test.go`.

---

## 1. Why the SQLite format is the floor, and whose floor it is

The scan that workload 4 runs -- `SELECT count(*) FROM t WHERE v > ?` over six
columns -- decomposed by LAYOUT, everything in memory, no engine, no cursor, no
allocation:

| layout | ns/row |
| --- | --- |
| columnar, one column of five, contiguous `[]int64` | **2.1** |
| row-major, fixed width, stride to the column | 6.3 |
| SQLite record: serial-type header walked to column 3 | 33.4 |
| + the b-tree page's cell-pointer indirection | 41.6 |
| + materialising each row into a generic `Value` | 114.2 |
| **C SQLite, the whole query** | **128** |
| **musql, the whole query** | **492** |

Read the last three rows together, because they are the whole argument. C SQLite
answers this query in 128 ns/row against a format whose floor is 114. **C SQLite
is already at its own floor** -- roughly 14 ns of engine on top of what the
format costs -- and it cannot go materially faster without changing the format.
musql has ~378 ns of engine above that same floor.

That is why this document exists, and also why it was not written sooner: until
this session, the engine was the binding constraint. It is now much less so, and
the six reverted attempts recorded in `6c69c58` are the evidence that what is
left on the scan is not reachable by tightening Go code.

It also explains a result already in the tree.
`engine/jit_ceiling_bench_test.go` measured a code generator's ceiling at 1.87x
on a simple filter and 3.14x on an expression-heavy one, with a 235 ns/row floor
underneath it that no code generator touches. That floor is rows 3-5 above. A
JIT removes dispatch and `Value` boxing; it does not remove a varint header walk
or a cell-pointer chase. **The JIT and the format are one project, not two, and
the format is the half that sets the ceiling.**

## 2. What the format has to be for generated code to address it

The constraint is not "fast", it is "computable in a few instructions from a
base pointer and a row index", because that is what a generated kernel can do
without calling back into Go.

- **Fixed-width columns, contiguous.** Column *c* of row *i* is at
  `base[c] + i*width[c]`. One shift and one add. No varint, no header, no cell
  pointer, no per-row branch.
- **Variable-length data out of line.** TEXT and BLOB store an
  `(offset uint32, length uint32)` pair in the fixed-width column and their
  bytes in a per-segment heap, so the stride stays constant and a scan that
  never touches the text never touches its pages.
- **NULL as a bitmap**, one bit per row per nullable column, not a per-value
  type tag. A predicate over a NOT NULL column never reads it.
- **Page-aligned column starts, 8-byte aligned payloads**, so a mapping can be
  reinterpreted as `[]int64`/`[]float64` directly. This is a hard requirement on
  `linux/386` and `linux/arm`, where an unaligned 8-byte load is not free.
- **Little-endian, fixed.** The alternative is native-endian, which makes the
  file non-portable between machines and forfeits the interchange property that
  is the whole point of having a converter. `s390x` therefore pays a byte swap
  per value; it is the only big-endian target in `make build_all_targets` and it
  keeps a correct, slower path.
- **Segments, not one array per table.** A segment is a bounded run of rows
  (~64k) with its own column blocks, heap and bitmaps. Segments are what make
  writes and compaction tractable, and they bound how much a mapping must cover
  on 32-bit targets, where the address space cannot hold a large table at once.

## 3. The hard problem is dynamic typing, not the converter

A SQLite column holds an integer in one row and a string in the next; affinity
is a suggestion, not a constraint. A fixed-width column cannot represent that
without a per-value tag -- and a per-value tag is row 5 of the table above, 114
ns/row, which is the thing being escaped.

The proposal is a **declared physical type per column plus an exception list**:
the segment records the physical type its column block is encoded in, and any
row whose value does not fit that type lives in a side table keyed by row index.
A scan reads the fast block; a row in the exception list is patched afterwards.
When exceptions exceed a threshold the column falls back to a tagged encoding
for that segment, which is slow and correct.

This is the part to prototype FIRST, before any of the rest, because it is the
only part that can make an answer wrong. The gate is the existing one: the
mined corpus must report `wrong=0` reading through the new format, and
`TestFileBytesEqual` must still hold for the SQLite format alongside it.

## 4. Writes

Columnar layouts are bad at row-at-a-time writes, and this engine's worst
scoreboard numbers are writes. The shape that fits:

- an **append-only row-major delta** per table, which is what INSERT and UPDATE
  write, cheap and unaligned-friendly;
- **columnar segments** built by compaction from the delta;
- reads merge the two, which is one extra branch per row and is why the delta
  must stay small.

This is deliberately the same shape as the existing page store plus
`materializeRowStore`, so the engine's existing write path is the delta and the
new work is compaction plus the columnar read path.

## 5. The converter

Nearly free, and it is the reason this is not a fork of the compatibility
promise. musql already reads and writes the C format byte-exactly in both
directions, verified by `TestFileBytesEqual` and by the whole mined corpus, so
conversion is a table scan into a segment writer and back.

What has to be gated is not the conversion but its FIDELITY:

- `sqlite -> musql -> sqlite` must be **byte-identical** for a database built
  by linear insertion, which is measured and holds. It cannot be asked of a
  database with deletions, whose free space the rebuild compacts -- there the
  claim is every row, every value TYPE and every ROWID, which is also measured;
- every corpus statement must answer identically against both formats, which is
  `scripts/sweep` with the format selected by a flag.

## 6. The JIT, and how little of it is needed

`~/Documents/jitllm` already does this in Go, on both architectures that matter:
`jit/cpu/trampoline_amd64.s` and `trampoline_arm64.s` call generated code with
ONE pointer (RDI on amd64, x0 on arm64) to an `Args` struct of fixed offsets,
with W^X handled by `mmap(RW)` + copy + `mprotect(RX)` in `exec_linux.go`. Its
rules transfer unchanged and are worth restating because each one is a crash
that does not reproduce near its cause:

- the goroutine register (R14 on amd64) is never available to generated code;
- generated code makes no Go call and grows no stack, so the trampoline is
  NOSPLIT and each entry needs a bounded execution budget (no async preemption
  inside a kernel);
- every address in `Args` is a real Go pointer, never a `uintptr`, so the GC
  keeps the backing arrays alive across the call;
- `Args` is append-only ABI with its offsets asserted in a test, because
  generated code addresses it by byte offset.

`warpjs`'s load layer contributes the idea that removes most of the difficulty:
**memory-only codegen, no register allocation.** Every virtual register is a
frame-window slot; an operation the backend cannot lower becomes an exit that
names a resume offset, and Go performs that one step before re-entering. For
musql the frame window already exists -- it is `m.regs []Value` -- and an exit
naming a `pc` means "run that one opcode on the VDBE and re-enter", which
satisfies RULE #1 by construction rather than by promise.

**But the measurement says the machine-code half may not be needed at all.**
Over the same columnar data, two predicates:

| lowering | ns/row |
| --- | --- |
| fused typed loop (what a code generator emits) | 7.8 at 50% selectivity, 1.5 at 1% |
| pre-compiled kernels over batches, branch-free | 5.2, flat at every selectivity |
| closure-threaded, one closure per predicate | 21.6 |
| row-major fixed width, fused | 9.6 |

Both of the top two are ORDINARY COMPILED GO. They cross over -- the fused loop
wins except near 50% selectivity, where branch prediction collapses and the
branch-free kernels win -- and both land in a 2-5 ns/row band that is 20-100x
below where the engine is today. A machine-code backend buys the difference
between those two rows, on nine of seventeen architectures, at the cost of the
whole trampoline/W^X/GC-invisible-frame surface.

So the staging below puts kernels first and the code generator last, not because
the code generator is wrong but because the format is what makes either possible
and kernels prove the format without the ABI risk.

## 6a. What the stages actually measured

**Stage 1 passed, and it is the one that could have killed the document.** The
mined corpus replayed into engine databases, every column's stored values
counted (`compat-harness`'s `TestColumnTypeCensus`):

	columns=10171  cleanly-typed=8405 (82.6%)  all-NULL=1526 (15.0%)  mixed=240 (2.4%)
	rows: typed=2057499  exceptions=893  (0.043%)

On the most adversarial population that exists -- SQLite's own test suite, which
mixes types on purpose where an application's schema does not -- 2.4% of columns
are mixed and 0.043% of stored values need the side list. `int+text` is 129 of
the 240. The fixed-width premise holds.

**Stage 3's numbers, over 100,000 real rows of the bench table, workload 4's
filter:**

| path | ns/row |
| --- | --- |
| b-tree scan, as a read costs today | 551.9 |
| segment, general accessor (NULL bitmap and exception list honoured) | 36.5 |
| segment, zero-copy `[]int64` | **2.07** |
| C SQLite, the whole query | 128 |

The safe accessor is already 3.5x faster than C SQLite; the slice is 61x, and
matches the 2.1 ns/row this document predicted from a synthetic layout before
any of it was built.

**What stage 3 cost to get right, which is the part worth carrying forward.**
The differential test -- compare the columnar answer against the answer this
engine gives for the same SQL, the engine itself being gated against C by the
corpus -- found a wrong answer on its first run. A stray TEXT in a
mostly-integer column: SQLite orders ACROSS storage classes, so it is greater
than any integer bound, and a hand-rolled `v.Typ == Int && v.I > bound` dropped
it. Two things about that: the bug was unreachable from the fast path by
construction, so it lived only where the exception list does, which is the
least-examined corner of the format; and the TEST's own expectation had the same
wrong rule, so an oracle computed by hand would have agreed with the bug.
**Every further stage compares against the engine, never against arithmetic
written next to the thing being tested.**

## 6b. What is NOT wired, stated plainly

This section used to say the filter was unreachable from SQL. It no longer is:
`segment_peephole.go` recognises a filter-and-count over a columnar table in the
compiled program and `OpSegFilterCount` runs it, which is the 250x in 6a. What
is still true is narrower, and worth keeping honest.

ONE SHAPE. The recogniser fires on `SELECT count(*) FROM t WHERE <col> <op>
<int> AND <col> <op> <int>` and nothing else. Both columns must be fixed-width
int64 blocks with no NULL and no exception, because the kernel reads the block
directly and has no way to consult a bitmap or a side list. Every other
statement -- a different aggregate, a projection, three predicates, a TEXT
column, a NULL anywhere in the column -- declines and runs the VDBE's own path.
That is a guard that jumps on success and falls through on decline, so a
decline costs one comparison.

WRITES INVALIDATE. A persisted segment file records its source's change counter
and page count and REFUSES to attach once the database has moved on. There is
no delta and no compaction, so any write invalidates a persisted file entirely
(stage 4). The in-memory path rebuilds at `Open` and cannot go stale.

So the remaining work is breadth, not plumbing: more shapes through the same
recogniser, and stage 4's delta so a file survives a write.

## 7. Staging, each with a gate that can fail

1. ~~**Exception encoding prototype.**~~ DONE, gate passed -- see 6a. Physical types plus the exception list, in
   memory, no file format. Gate: the corpus's value distribution, measured --
   how many columns are cleanly typed and what the exception rate is on the ones
   that are not. If that rate is high, this document is wrong and the rest does
   not get built.
2. **Segment format and writer**, plus the converter both ways. BUILT, and the
   gate this document originally asked for was wrong: "byte-identical" cannot
   hold in general, because rebuilding a table inserts its rows into fresh
   pages and therefore COMPACTS the free space a DELETE left behind. The bytes
   differ because the rebuild is tidier, not because anything was lost.

   What holds, and is pinned: a database built by LINEAR INSERTION round trips
   byte-identical, which is the case where a difference means a converter bug
   rather than compaction. A database with deletions round trips with every
   row, value type and rowid intact and a smaller file. Rowid tables only;
   indexes are not yet recreated in schema order, which is what a byte-identical
   round trip of an INDEXED table would additionally need.
3. ~~**Columnar read path behind a flag**~~ DONE, gate met.
   `MUSQL_COLUMNAR_READS=1` swaps the source at `ScanTable`, which is where
   every read in this engine asks for a table's rows -- so one swap makes the
   VDBE's cursors, the schema loader and `integrity_check` columnar at once,
   with no second executor. The whole mined corpus reports
   `statements=67800 unsupported=0 wrong=0 panics=0`, identical buckets to the
   b-tree run.
4. **Delta plus compaction**, so writes work. HALF DONE, and the half that is
   missing is the useful one. A segment file now records its source's change
   counter and page count and REFUSES to attach when the database has moved on
   -- derived data that has silently fallen behind is a wrong answer, not a
   slow one. But there is no delta and no compaction, so every write
   invalidates a persisted file entirely. The in-memory path needs none of
   this: it rebuilds at `Open` and cannot go stale, which is why stage 3 could
   be gated before stage 4 existed. Section 4 above is still the design.
5. **JITted VDBE** -- BUILT and wired, on amd64 and arm64. `internal/jit`
   emits machine code at run time, maps it W^X and enters through one
   trampoline per architecture; `segment_peephole.go`
   recognises a filter-and-count over a columnar table in the COMPILED PROGRAM
   and adds a guarded fast path. End to end, through parse, compile, peephole,
   opcode, segments and kernel:

   | `SELECT count(*) FROM t WHERE v > 500000 AND k <> 7`, 100k rows | ns/op |
   | --- | --- |
   | VDBE over the b-tree | 11,984,181 |
   | JITted VDBE over segments | **47,786** |

   250x, on a real statement. Re-measured on three machines --
   206x on the same i9 (11,499,370 -> 55,762), 253x on an M4
   (7,604,065 -> 30,077), and 195x on an Atom (61,327,566 -> 314,935), the last
   of those through the SCALAR kernel because the Atoms have no AVX2.

   These are ratios against musql's OWN VDBE. The number that matters is
   against C, and it is in 8b below -- it needed the recogniser to accept
   `WHERE v > ?` first, which it did not: it required exactly TWO predicates and
   required each bound to be a literal `OpInteger`, where a parameter compiles
   to `OpVariable`. Both restrictions are gone.

   Measured rather than reasoned, and note this is the WRONG configuration --
   it is kept because the result is instructive. `TestBenchVsC` with
   `MUSQL_COLUMNAR_READS=1` gives every table an in-memory columnar view, which
   routes general row reads through segments WITHOUT the kernel:

   | workload | b-tree | columnar | C |
   | --- | --- | --- | --- |
   | 4. full-scan count filter | 12.14 ms | **22.90 ms** | 3.22 ms |
   | 5. aggregate GROUP BY | 21.99 ms | 33.44 ms | 22.58 ms |
   | 6. ORDER BY v DESC LIMIT 20 | 7.73 ms | 23.39 ms | 3.83 ms |
   | 7b. bulk UPDATE | 134.08 ms | 300.81 ms | 27.52 ms |

   Turning the columnar path on made every scan and every write WORSE, by two to
   three times. That is not a contradiction of the 195-253x above, it is the
   other side of it: a segment is fast when a column can be handed over as a
   slice, and `ScanTable`'s contract is one `[]Value` per row, so the general
   read path pays to materialise rows it then throws away. The kernel is the
   only consumer that takes the slice, and the recogniser admits one statement
   shape to it.

   So the honest position: the format and the JIT are fast on the shape they
   cover, that shape is narrow, and switching the general read path to segments
   today is a regression. Three things stand between here and a number that
   means anything against C -- the recogniser's breadth (one predicate,
   parameters), a columnar read path that does not materialise rows, and stage
   4's delta so a write does not invalidate a segment file. None is a codegen
   problem.

   One trap, because it cost a wrong conclusion here: do NOT set
   MUSQL_COLUMNAR_READS=1 for this benchmark. It gives the b-tree arm segments
   too, so the peephole fires for BOTH and they report the same number -- which
   reads as "the JIT bought nothing" rather than as a broken measurement. The
   comparison arm attaches its segments explicitly and needs no flag.

   Verified on amd64 two ways, which is the point of having both: this
   machine has AVX2 and runs the JIT, the Atom machines have SSE4.2 and no
   AVX at all and run the VDBE. Green on both, in all four configurations (JIT
   on, JIT off, columnar reads on, off), across all seventeen cross-build
   targets, and over the whole mined corpus on the servers
   (statements=67800 unsupported=0 wrong=0 panics=0, TxnLockstep PASS).

   The recogniser runs on EVERY compiled scan, so its refusal path is what the
   corpus actually exercises -- and the servers cannot exercise it, because
   with no AVX2 the peephole returns before looking at anything. That coverage
   was taken here instead, a chunk at a time with the JIT live: i-k, c-e, l-n,
   o-q, 0-9, a and g, all clean. A whole-corpus local run is not available for
   it -- this box's /tmp is on the root filesystem and the attempt took free
   space from 25G to 19G before being stopped, which is the ENOSPC failure
   AGENTS.md documents. The x-z chunk is not in that list because it is KILLED
   under scripts/cap's 2G ceiling -- at 610s, by the cgroup rather than by the
   test timeout. It was checked AT BASELINE, with the JIT and columnar reads
   both off, and it does not survive there either, so its absence says nothing
   about this work.

   Below, the measurements that decided the shape.

   There are two execution paths here and only two: the VDBE, and a JITted
   VDBE (AGENTS.md Rule 2). Pre-generating Go source for the compiler is a
   third and is forbidden -- it was tried during this work, measured at 1.9x,
   and deleted.

   What the ceiling says, over real segments, two predicates, 100,000 rows:

   | lowering | ns/row |
   | --- | --- |
   | kernels, branch-free and batched | 7.25 |
   | scalar loop with `&&` | 11.31 |
   | scalar loop, branchless | 3.75 |
   | **AVX2, four lanes** | **0.205** (i9-12900HK, where the scalar loop is 0.754) |

   Two separate findings, and the second is the one that matters.

   The first is that `&&` is a BRANCH and costs more than the flag array it was
   meant to replace, and that a switch reached per row does not inline (8.28
   ns/row, worse than the kernels). Both are loop SHAPE, and both are things a
   JIT fixes by construction.

   The second is INSTRUCTION COUNT. AVX2 compares four int64 per instruction and
   beat the scalar Go loop by 3.68x on identical data -- almost exactly the four
   lanes. That figure was taken with a throwaway hand-written kernel which was
   then DELETED: checked-in assembly is forbidden here except the JIT's
   trampoline (AGENTS.md Rule 2), because a hand-written kernel is a build-time
   guess at a shape the JIT should be choosing at plan time. Go's compiler will not vectorise these loops, cannot fold the
   comparison bound into an immediate, and cannot always prove away the bounds
   check. That gap does not close by rearranging Go; it closes by emitting
   different instructions, which is what a JIT is for.

   So the backend case is no longer architectural taste. It is 3.68x on a filter
   kernel, on top of the 241x the format already bought, and it is what the
   remaining work should go into.

## 8. arm64

Same design, second architecture, and the reason it is a separate file rather
than a flag is that the two share no encoding. `asm_arm64.go` is half the length
of its x86 counterpart -- fixed 32-bit instructions, no REX, no ModRM, no
displacement whose width depends on the distance -- and `trampoline_arm64.s`
saves nothing, because the kernels stay inside X0-X10 and V0-V7, which AAPCS
makes caller-saved outright. What it does need is a non-zero frame: at zero the
assembler treats it as a leaf and does not save LR, and BL overwrites LR.

Three registers are not offered on any terms: Go pins the goroutine in X28,
keeps the assembler's temporary in X27, and darwin reserves X18.

NEON is TWO int64 lanes to AVX2's four, so the per-instruction win is half.

## 8a. The scalar kernel, and the gate it invalidated

The first scalar kernel was a straight transcription of the branchy loop: load,
compare, jump past the increment. On amd64 that is the SAME SHAPE Go already
emits, and it measured 2.3% SLOWER -- the trampoline call is not free and
nothing else had changed. arm64's was ten times faster at the same instruction
count, purely because CSET has no jump in it.

So the gate said: emit only where a VECTOR kernel is possible. That was reading
the symptom. The mispredict was the cost, and a scalar kernel can remove it on
BOTH architectures; it just has to stop being a transcription.

Rewritten, two predicates, ~50% selectivity, 100,000 rows:

| | Go loop | scalar, first try | scalar, rewritten | vector |
| --- | --- | --- | --- | --- |
| amd64, i9-12900HK | 302 us | 328 us (0.92x) | **42.7 us (7.1x)** | 19.8 us (15.2x) |
| arm64, Apple M4 | 328 us | 34 us (9.7x) | **25.4 us (12.9x)** | 22.7 us (11.6x) |
| amd64, Atom C3558 | 781 us | -- | **274 us (2.85x)** | none (no AVX2) |

Ten instructions became seven on both, and the measurement tracked the count at
every step. On amd64, medians of ten runs at 500x, same binary and same data:

| | instructions | median ns/op |
| --- | --- | --- |
| original, branchy | 10 | 328,000 |
| branch-free, indexed | 8 | 49,254 |
| + CMOV for the conjunction | 7 | **42,710** |

Single runs are not enough to see the last step: this box drifts about 8% run to
run, which is wider than the 8 -> 7 difference. The first attempt at that
comparison read 52 us against 45 us from one run each, which was the right
direction for the wrong reason. Ten runs put it at 49.3 -> 42.7, a real 13%.

	amd64, 7                        arm64, 7
	  CMP    [rsi+rcx*8], rax         LDR  x7, [x1], #8
	  SETcc  r10b                     LDR  x8, [x2], #8
	  CMP    [rdx+rcx*8], rbx         CMP  x7, x4
	  CMOVcc r10, r9   ; r9 == 0      CCMP x8, x5, #nzcv, condA
	  ADD    r8, r10                  CINC x6, x6, condC
	  INC    rcx                      SUBS x3, x3, #1
	  JNZ    loop                     B.NE loop

What did it:

- **branch-free.** `SETcc` on amd64 and `CINC` on arm64 materialise a
  comparison as 0 or 1 and ADD it, so no jump depends on the data. This is the
  one that mattered most -- it is the whole 332 -> 52.
- **the loop counter IS the cursor.** Both blocks are addressed as
  `[end + i*8]` with `i` running from `-n` to 0, so one `INC` advances both
  reads and tests for the end. That is two ADDs and a DEC replaced by one INC.
  On arm64 the post-indexed load does the same job inside the load.
- **compare against memory directly**, rather than loading into a scratch
  register first.
- **combine the two predicates in one instruction.** Each architecture has its
  own way and neither has the other's. amd64 sets the first predicate into a
  register and then CMOVs a zero over it when the second fails, replacing a
  second `SETcc` and an `AND`. arm64 uses `CCMP`, which performs the second
  comparison only if a condition on the first holds and writes a chosen NZCV
  otherwise -- a short-circuit AND expressed in the flags, with no intermediate
  register at all.

The vector kernel is still better where it exists, so it is still preferred --
but it is no longer the price of admission, and `segment_jit.go`'s gate dropped
the `HasVector` requirement accordingly.

The practical effect is coverage, not just speed. With the old gate, an amd64
without AVX2 ran no generated code at all, so `segPeephole` returned before
looking at anything and the Atom machines that run the whole mined corpus
could never exercise the recogniser. They do now.

**Verification.** On the M4 (darwin/arm64): both kernels agree with Go across
all thirty-six operator pairs at lengths straddling both lane counts, and the
engine suite is green -- which is the arm64 gate AGENTS.md prescribes, the
`-short` harness being red there at main for two darwin-only reasons unrelated
to this. W^X is the part that could have failed and did not: plain
`mmap(READ|WRITE)` then `mprotect(READ|EXEC)` works on Apple Silicon for an
ad-hoc signed binary, with no MAP_JIT and no entitlement, and the I-cache is
coherent across the protection change without explicit maintenance.

**What was NOT tested, and what stands on reasoning instead.** The architecture
is exercised on darwin/arm64 only, because that is the only arm64 machine here.
linux/arm64 is ENABLED (`exec_jit.go` covers `(amd64 || arm64) && (linux ||
darwin)`) and builds, but has never run. The machine code is identical; what
differs is the OS half, and only in two places. `mmap`/`mprotect` are POSIX.
I-cache coherency across the protection change is the one that could bite, and
Linux does it: `change_pte_range` reaches `set_pte_at`, which calls
`__sync_icache_dcache()` for an executable PTE whose page is not
`PG_dcache_clean` -- and a page just written by the CPU is not. That is a
citation, not a guess, but it is not a test either.

Also not done: arm64 has had no corpus run. The recogniser's refusal path is
exercised by the engine suite there, not by the mined corpus.

## 8b. musql on its own format, against C on theirs

`bench_vs_c_test.go` times musql against C SQLite with BOTH engines reading the
SQLite file format. That is the right test of "is our engine good at their
layout", and musql loses it -- 3.7x slower on a full-scan count. It is not the
bet the format makes. `bench_columnar_vs_c_test.go` is: musql on segments with
the JIT live, C on the format it was designed around.

100,000 rows, disk-backed, warm, all three arms asserted to agree on the answer
before anything is timed:

| workload | musql b-tree | musql+JIT | mattn-C | JIT/C |
| --- | --- | --- | --- | --- |
| `count(*) WHERE v > ?` | 9.643 ms | **44 us** | 2.803 ms | **0.02x** |
| `count(*) WHERE v > ? AND k <> ?` | 11.352 ms | **35 us** | 3.502 ms | **0.01x** |

64x and 100x FASTER than C SQLite. Both musql arms are engine-direct over
the same data through the same API, so what separates them is the storage format
and the kernel, and nothing else. A musql arm through `database/sql` is timed
alongside so the driver's per-statement cost stays visible rather than folded in
(it is ~0.2ms, and it dwarfs the kernel -- a caller reaching this through the
driver gets the b-tree's time, not 44 us).

The single-predicate case being SLOWER than the two-predicate one is not noise:
the vector kernels emit a two-column compare and have no one-column form, so one
predicate takes the scalar kernel on every machine. A one-column vector kernel
would close it.

**What this is not.** The segment file is built offline, once (85 ms), from a
database nobody is writing to, and any write invalidates it entirely -- there is
no delta and no compaction. This is the ceiling the format offers a read-mostly
table, not a number a live database reaches today. Stage 4 is what stands
between the two.

### The bug that a "faster" number hid

The first run of this benchmark reported the columnar arm at 20.9 ms -- SLOWER
than the b-tree's 9.4 ms -- while its assertion said the peephole had fired. The
assertion was checking that `OpSegFilterCount` was PRESENT in the program. That
is not the same claim as the fast path having ANSWERED, because the opcode is a
guard that can decline at run time and fall through to the loop the peephole
deliberately left behind. The loop then produces the right answer, slowly, and
nothing anywhere says so.

It was declining every statement. A parameter's bound was recorded as the
REGISTER it lands in, and `OpSegFilterCount` sits BEFORE the loop while the
`OpVariable` that fills that register sits INSIDE it -- so the guard read an
empty register and refused. The differential test written for that path passed
throughout, comparing the loop against itself.

`SegFilterCountersForTest` now reports served and declined counts, and the
benchmark and the test both assert on them; a mutation that makes the guard
decline fails the test rather than passing it. The general rule, which this repo
has now paid for three times in one session: **a performance claim needs the
fast path to prove it ran, not that it was compiled.**

## 8c. What widening the benchmark found, which is not about the JIT

The table in 8b had two workloads, both the shape the recogniser answers. Widened
to joins, aggregates and ordering -- 100,000 rows, every arm checked against C
SQLite as oracle on all 40 bind values, and they all agree:

| workload | musql b-tree | musql+JIT | mattn-C | turso-rust | vs C |
| --- | --- | --- | --- | --- | --- |
| `count(*) WHERE v > ?` | 9.9 ms | **17 us** | 2.79 ms | 8.16 ms | 165x faster |
| `count(*) WHERE v > ? AND k <> ?` | 12.1 ms | **36 us** | 3.43 ms | 11.1 ms | 95x faster |
| rowid point lookup | 3 us | 4 us | 7 us | 16 us | 2x faster |
| secondary-index eq | 172 ms | 138 ms | 8 us | 19 us | **18,293x SLOWER** |
| indexed equi-join | 129 ms | 130 ms | 11 us | 32 us | **11,635x SLOWER** |
| sum over a filter | 11.1 ms | 21.9 ms | 3.06 ms | 9.47 ms | 7.2x slower |
| aggregate GROUP BY | 98 ms | 110 ms | 18.0 ms | 43.7 ms | 6.1x slower |
| ORDER BY v DESC LIMIT 20 | 7.6 ms | 21.5 ms | 3.64 ms | 36.5 ms | 5.9x slower |
| `count(*)` whole table | 139 ms | 143 ms | 12 us | 68 us | **11,984x SLOWER** |

Three readings, in order of how much they should change what happens next.

**1. `count(*)` ABANDONS INDEXES.** This is the finding. Isolated
engine-direct at 50k rows with one index on `sec`:

| query | musql |
| --- | --- |
| `SELECT id,sec,v FROM t WHERE sec = ?` | 4 us (index seek) |
| `SELECT count(*) FROM t WHERE sec = ?` -- SAME WHERE | 74 ms (full scan) |
| `SELECT count(*) FROM t` | 81 ms |
| `SELECT count(*) FROM t WHERE id = ?` (ROWID equality) | 3.4 ms |

Changing only the PROJECTION costs 18,500x. C answers a bare `count(*)` in 12 us
because it counts b-tree entries without decoding rows; musql walks and decodes
every one. These are four orders of magnitude on completely ordinary queries, and
they dwarf everything else in this document. The JIT is 165x faster than C on one
statement shape while this sits in the same engine.

**2. Against Turso, musql is level on its own terms.** On the SQLite format,
where all three compete evenly: musql's b-tree is 1.1-1.2x slower than Turso on
scans, 6x FASTER on a rowid lookup, 5x faster on ORDER BY ... LIMIT, and 2.25x
slower on GROUP BY. Turso is the other from-scratch SQLite-compatible rewrite --
Rust where this is Go -- so that is the closest thing to a peer comparison
available, and it says the engine is roughly where that project is, with both
trailing C by ~3x on scans.

**3. The columnar arm is SLOWER than the b-tree wherever the kernel does not
fire.** `sum over a filter` 21.9 ms against 11.1, ORDER BY 21.5 against 7.6.
Same cause as 8's earlier note: segments are fast when a column can be handed
over as a slice, and `ScanTable`'s contract is one `[]Value` per row, so the
general read path pays to materialise rows it then discards. It is left visible
in the table rather than hidden by only benchmarking the shapes that win.

## 9. Against C SQLite and Turso, all nine workloads

Current numbers, measured with segments plus a writable delta, are in
[benchmarks.md](benchmarks.md).

The rest of this section is from the stage with segments built offline, on an
Intel Atom C3558; its table is what the root causes below were measured against.

| workload | musql | mattn-C | turso | vs C | vs turso |
| --- | --- | --- | --- | --- | --- |
| `count(*)` whole table | 11 us | 32 us | 162 us | **3x** | 14x |
| `count(*) WHERE v > ?` | 28 us | 7.03 ms | 20.2 ms | **255x** | 734x |
| `count(*) WHERE v > ? AND k <> ?` | 82 us | 7.41 ms | 23.7 ms | **91x** | 289x |
| rowid point lookup | 9 us | 24 us | 50 us | **3x** | 5x |
| secondary-index eq | 3 us | 22 us | 52 us | **7x** | 15x |
| indexed equi-join | 29 us | 32 us | 82 us | **1.1x** | 3x |
| `sum()` over a filter | 1.23 ms | 6.78 ms | 22.6 ms | **5x** | 18x |
| ORDER BY v DESC LIMIT 20 | 3.65 ms | 8.75 ms | 86.3 ms | **2x** | 24x |
| aggregate GROUP BY | 14.4 ms | 39.1 ms | 102 ms | **3x** | 7x |

At that stage, nine of nine were faster than C and Turso. Today the point lookups
and the join run on the VDBE and are at or below C, most visibly on the Xeon.

GROUP BY was the last holdout, at parity, until the scan behind it was driven
from the columnar blocks (segment_group.go): 41.6ms -> 14.4ms. The design choice
there is the one worth copying. The obvious columnar GROUP BY accumulates into
its own map and emits its own rows, which means classifying every output as the
group key, count(*), sum(col) and so on -- and `sum(v) + 1` carries one
aggregate template exactly as `sum(v)` does, so reading it as a plain sum is a
silent wrong answer. Replacing only the ROW SOURCE, and calling the same
hashAggStepRow the ordinary loop calls, reimplements no aggregate at all: that
is why count(DISTINCT) and avg() work without a line of code for either. The
test expected DISTINCT to decline and was wrong.

That driver also produced the clearest wrong answer of the whole exercise. It
first handed each step a correctly SHAPED but empty row, on the reasoning that a
plan reading only lowered values never looks at one. A GROUP BY key in the
select list is finalized from the GATHERED ROW, not from the bucket's key tuple,
so every group came back with a NULL key -- with the right row count and the
right aggregates. Nothing but a value-by-value comparison finds that.

**Five root causes did nearly all of it, and only one was the JIT.**

1. **`count(*)` disabled index selection entirely.** compileScanAggregate never
   called detectIndexSeekKey -- twelve call sites in the scan compiler, zero in
   the aggregate one -- so every aggregate full-scanned even when the WHERE
   pinned an indexed column to a constant. `count(*) WHERE sec = ?` was 23.7ms
   against 5us for the same WHERE with columns selected. The join was never the
   problem; the PROJECTION was, and fixing it took the count-over-join from
   10,618x slower than C to parity.
2. **Any index on the table disabled the recogniser.** OpAutoIndexOrder is
   compiled between the OpenRead and the Rewind whenever a table carries an
   index, and the matcher demanded Rewind at a fixed slot -- so one index, on
   any column, cost the fast path for every query on that table.
3. **A GROUP BY ordered by its own key built a sorter it did not need.** The
   hash drain already emits group-key ascending order; the gate only checked
   for the absence of an ORDER BY. 96ms -> 15.8ms.
4. **ORDER BY ... LIMIT decoded every row to read one key.** Now a pass over an
   int64 block with a bounded heap, and the multi-row opcodes
   (OpSegOrderLimit/OpSegEmitRow) that made it possible are reusable.
5. **GROUP BY scanned the b-tree to fill a hash it could have been handed.**
   The driver writes the key and lowered arguments into the registers the scan
   body would have written and calls the same step: 41.6ms -> 14.4ms.

**And two that were measurement, not engine.** Attaching a segment file used to
make every query WITHOUT a fast path slower, because row reads went through the
segments and the streaming b-tree cursor was declined for any table that had
them -- 100,654 allocations a query against 596. Attaching is purely additive
now. Separately, a fixed 40 iterations timed a 37us join over 1.5ms, and its
ratio against C flipped sign between runs of the same binary; twice I started
investigating a gap that was the benchmark.

**What this is not.** Segments are built offline and any write invalidates them
entirely (stage 4). The numbers above are a read-mostly ceiling, and the
recogniser covers one statement shape per fast path -- everything else runs the
ordinary VDBE, which is the `musql btree` column and still trails C by ~3.4x on
a full scan.
