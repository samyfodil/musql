# Rows read and rows written

musql counts the rows each statement reads and writes the way libSQL counts
`LIBSQL_STMTSTATUS_ROWS_READ` and `LIBSQL_STMTSTATUS_ROWS_WRITTEN`, which is
what Turso bills by. The driver keeps the counts per connection
(`driver.Conn.RowCounts`), and Hrana returns each statement's own in
`rows_read`, `rows_written` and `query_duration_ms`, in both the protobuf and
the JSON encodings. `hrana.Stmt.RowsRead` and `RowsWritten`, which the cloud's
usage meter bills from, are the same numbers.

## The rule

libSQL adds one read each time a cursor lands on a row: `OP_Rewind`, `OP_Next`
and `OP_Prev`, a seek (`OP_SeekGE` and kin, `OP_NotExists`), `OP_Last` and
`OP_VNext`. `OP_Count` adds the b-tree pages it visits. It adds one write for
each `OP_Insert`, `OP_IdxInsert` and `OP_Delete` on a real table, and
`OP_Clear` adds the rows it removed.

musql counts the same events:

- A read for every row a table cursor (`OpOpenRead`, `OpOpenWrite`) or a
  virtual table's lands on, at `OpRewind` and `OpNext`, a rowid seek that finds
  its row, and each row an index seek finds. Rows a statement holds for itself
  (a sorter's, a subquery's or a CTE's result) are not read again: they were
  counted when they came out of their table.
- Native code counts the same rows. Each JIT cursor has a step counter
  (`jit.VCursor.Steps`) that the native `Rewind`, `Next` and seeks bump, and
  the VDBE adds it in when it takes the cursor back.
- A columnar kernel that answers a loop instead of walking it counts the rows
  the loop would have landed on: the whole table for a scan, the rows of the
  seek it skipped, the outer rows of a join plus each row its lookups find in
  the levels inside, and for a GROUP BY that C streams in index order, the rows
  of the groups up to the last one sent plus the first of the next.
- A write for each row inserted, updated or deleted, plus one for each index
  entry an insert or update writes (every index for an insert; for an update,
  the indexes keyed on a column it assigns). DDL writes its catalog entry, and
  `CREATE INDEX` reads the table and writes an entry per row.

## The reference table

A 10,000-row table `big(id INTEGER PRIMARY KEY, k INTEGER, v TEXT)` with
`k = id % 100`. `driver/row_counts_test.go` reproduces it with and without the
JIT.

| statement | Turso | musql |
| --- | --- | --- |
| `CREATE TABLE big(...)` | 1 / 2 | 0 / 1 |
| `INSERT INTO big SELECT ... FROM generate_series(1, 10000)` | 10,000 / 10,000 | 10,000 / 10,000 |
| `SELECT * FROM big WHERE id = 5000` | 1 / 0 | 1 / 0 |
| `SELECT count(*) FROM big WHERE k = 7` (no index) | 10,000 / 0 | 10,000 / 0 |
| `SELECT count(*) FROM big` | 104 / 0 | 1 / 0 |
| `CREATE INDEX big_k ON big(k)` | 20,043 / 10,001 | 10,000 / 10,001 |
| `SELECT count(*) FROM big WHERE k = 7` (indexed) | 101 / 0 | 100 / 0 |
| `SELECT k, count(*) FROM big GROUP BY k LIMIT 3` | 301 / 0 | 301 / 0 |
| `UPDATE big SET v = 'x' WHERE k = 7` | 101 / 100 | 100 / 100 |

## Where musql counts less

musql never counts more than libSQL for the same statement. Where the two
engines do different work, musql counts what libSQL's plan would have read, or
less:

- **An index range.** libSQL's index walk also lands on the first entry past
  the range before it stops (the 101 above). musql's seeks return only the
  matching rows.
- **`count(*)` with no WHERE.** libSQL counts b-tree pages. musql adds the row
  counts its segments record, and counts one read per segment, plus one for
  the delta.
- **`CREATE TABLE` and other DDL.** libSQL inserts a placeholder schema row,
  reads it back and rewrites it. musql writes its catalog entry once.
- **`CREATE INDEX`.** libSQL also reads every entry back out of the sorter it
  builds the index with. musql counts the table scan and the entries.
- **A wide index seek.** Where a declared index matches a large share of the
  table, musql walks the table instead of seeking (`seekIsWide`) and counts no
  more rows than the seek would have found.
- **Rowid bounds musql does not seek.** Where libSQL would take `rowid <
  ...`, `BETWEEN` and kin as a rowid range but musql walks, musql counts no
  more rows than lie in the range. The same goes for a one-table `ORDER BY` on
  the rowid or a declared index's leading column with a `LIMIT` and no WHERE:
  libSQL walks that order and stops after `LIMIT + OFFSET` rows.
- **A kernel's skipped seek.** A columnar kernel that answers a loop whose
  seek it does not take counts the rows the seek would have found, when it
  knows them, else none.
- **Ephemeral rows.** libSQL counts steps through automatic indexes and other
  ephemeral tables. musql builds no such tables and counts none.
- **Partial and vector indexes** are not counted in an insert's or update's
  index writes.
- **A WITHOUT ROWID primary key seek** and a lone `min()` or `max()` over the
  rowid or an indexed column read one row in libSQL. musql walks there and
  counts one row.

## Where musql can still count more

musql counts what its own plan reads, capped where libSQL's plan is known to
read less. Two shapes are left where musql's plan reads more and nothing caps
it, and both are the planner's to fix, which fixes the count and the scan
together:

- **An aggregate over a declared index range**, such as `SELECT count(*) FROM
  big WHERE k BETWEEN 3 AND 5`. libSQL walks the index range (301 rows);
  musql's aggregate loops take no index range, so the columnar kernel scans
  the table and counts all 10,000.
- **A join musql orders differently.** Where libSQL puts a rowid-bounded table
  outside and probes the other through an index, and musql puts the unbounded
  table outside, musql counts its own outer scan and probes.
