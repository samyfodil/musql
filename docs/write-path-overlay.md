# The write path: one representation instead of four

`docs/format-design.md` §4 already names the shape — "an append-only row-major
delta per table, which is what INSERT and UPDATE write". This is the concrete
design for it, written after the reads were made to win and the writes were left
as the only losing column on the scoreboard.

## What is wrong now, measured

A single logical mutation is materialised FOUR times in three incompatible
representations:

    VDBE record (recAlloc) -> row store map[uint64][]Value, by reference
                           -> RowChange, 112 B, with a DEEP COPY of the row
                           -> SegDeltaRecord, 48 B, at commit
                           -> rebuilt segment, at compaction

Bulk UPDATE allocation, one statement over ~1/10 of 100,000 rows in autocommit,
after the 17% already cut (total 3,442 MB):

| group | share |
| --- | --- |
| commit / file write | 26% |
| change log (`noteRowChange`, `copyHookRow`) | 17% |
| row store + scan (`materializeRowStore`, `decodeRecordMaskedInto`) | 13% |
| VDBE record (`recAlloc`) | 6% |
| `database/sql` binding | 10% — symmetric, the C side pays it too |

GC drain was 39% of all CPU before those cuts, so **allocation RATE is the cost**,
not instruction count. C SQLite's page cache is simultaneously its mutation buffer
and its eventual storage image; ours is none of those things.

## The design

**One transaction-local, delta-native mutation overlay.** It is at once the
read-your-writes view, the conflict/uniqueness probe source, and the direct input
to the durable delta batch. Per open transaction:

- an append-only **mutation stream**, each entry `rowid + op + changed-column mask
  + values`, serialising to the delta batch without a conversion pass;
- a **rowid index** over that stream giving the latest visible mutation — this is
  what replaces `map[uint64][]Value`, and it holds mutations, not row images;
- **per-index overlays** where a read path can consult an equality index;
- **value arenas** owned by the transaction;
- **savepoint marks**: offsets into every one of the above.

Reads merge `base segment(s) + committed delta(s) + this transaction's overlay`.

An UPDATE records the columns it changed. It does not build a row image, which is
what the current path does and what `copyHookRow` then copies again.

At commit the stream is serialised into the durable batch, the trailer written,
and only then is the overlay published to the committed read view. Compaction
folds base plus a committed-delta PREFIX into new segments, later and off the
write path.

## The correctness surfaces

Most of what a redesign here must not break is already gated — conflict modes,
savepoint rollback, torn-tail replay, compaction identity. Two surfaces are NEW,
have no analogue in the current design, and are where a silent wrong answer would
come from:

1. **An omitted column is not a NULL column.** "This UPDATE did not touch column
   k" and "this UPDATE set column k to SQL NULL" must be distinguishable, and the
   distinction may not rest on a nullable cell. It belongs in the mask.
2. **The index overlay must agree with table visibility, exactly.** If a seek can
   be answered from an equality index, that index must merge base + committed +
   transaction overlay identically to a scan. An index path that sees a row the
   scan path does not is precisely the never-wrong violation this repo fears, and
   it is invisible to any gate that only reads one way.

Two more that are not new but get harder:

3. **Statement rollback is not transaction rollback.** A failing statement rolls
   back while earlier statements in the transaction survive, and savepoints nest.
   Truncating the mutation stream is not enough if an index overlay was mutated;
   every structure needs its own undo boundary.
4. **Value ownership.** Bound `[]byte` and anything derived from VDBE registers
   cannot be referenced after the machine is reused — the machine is POOLED now
   (`vdbe_machine_pool.go`), which is why `recChunk` is dropped rather than
   recycled. The transaction arena must own exactly what the staged mutation
   needs.

And one constraint the current code already imposes: a **full-capture** consumer
(the CRDT layer, the ATTACH trigger delegation) needs OLD images and column names,
which a changed-column mask does not carry. Either the overlay retains old values
under full capture, or it reads the base at capture time. This is a design
decision, not an implementation detail — see `rowhook.go`'s capture levels.

## What is NOT in this

**Column-major staging is not the write path.** "Commit is a memcpy" does not
survive NULL bitmaps, TEXT/BLOB heap ownership and offsets, the exception
side-list, tombstones, rowid ordering, equality-index entries and conflict modes.
Columnar batching earns its place at three sites and no others: **bulk insert**,
**index construction**, and **compaction**. The goal is one owned representation
and ONE encoding pass, which is a different and more achievable claim.

## Staging, each with a gate that can fail

1. **The overlay, reads only.** Build it beside the existing row store, serve
   read-your-writes from it, keep the old path authoritative. Gate: the full
   corpus in both builds, `wrong=0 panics=0`, plus both `-short` builds at their
   known counts. A divergence here is a visibility bug and must be found before
   anything durable depends on it.
2. **The overlay as the delta's input.** Delete `RowChange` and the conversion.
   Gate: as above, plus `TestTCLCorpusTxnLockstep` and the WAL/crash-injection
   harness — this is the step that can lose a commit.
3. **Index overlays.** Gate: a seek and a scan must answer identically for every
   shape the corpus holds; add a differential that runs both and compares, because
   no existing gate does.
4. **The row store's map goes.** Gate: allocation measured, not assumed.
5. **Columnar batching at the three sites.** Gate: measured per site; each stands
   or is reverted on its own number.

## What landed before any of the staging above

Five of the costs above turned out not to need the overlay at all, because the
row store's own contract -- a stored row is never edited in place -- had exactly
ONE violator (a read's fix-ups wrote into live rows, `vdbeCursor.normalizeFrom`),
and once that copied, sharing was sound everywhere else:

- BEGIN/SAVEPOINT share row slices instead of deep-copying every table
  (`rowStore.clone`): a 20k-row insert transaction 131ms -> 73ms.
- the delta-only change log holds the stored row itself, not a copy
  (`noteRowChange`) -- the open question below, answered.
- a write scan normalizes each row as it becomes current rather than copying the
  whole table into an arena first (`lazyNorm`): bulk UPDATE -20%, DELETE -27%.
- UNIQUE checks and foreign-key parent lookups probe an index instead of scanning
  (`row_store_uniqindex.go`, `fkParentCandidates`). These were QUADRATIC: 32,000
  inserts over a UNIQUE column 13.9s -> 69ms, an UPDATE of 3,200 of 32,000 rows
  49s -> 42ms, 32,000 FK child inserts 24s -> 137ms.

TestBenchVsC on .10, same box and minute, `cda0ca01` -> `12c9c226`: batch INSERT
1.82-1.96x -> 1.05-1.30x, bulk UPDATE 1.15-1.19x -> 0.81-1.06x, bulk DELETE
~0.40x -> ~0.37x. What INSERT still pays per row is a hash probe and a hash
insert into a 100k-entry map (a cache miss each) plus the change log and delta
record -- the representation itself, which is what this document's overlay is for.

## One open question -- answered, kept for the reasoning

`copyHookRow`'s deep copy of every written row is ~7% of a bulk UPDATE. It may be
aliasable against the row store's own slice under delta-only capture. **This is
not established** — an earlier claim that reads mutate stored rows turned out to be
false, and the correction does not license the opposite conclusion. It needs its
own call-site audit: what happens to a slice the log holds when a later `put`
replaces the map entry, and what an earlier-materialised cursor still references.
