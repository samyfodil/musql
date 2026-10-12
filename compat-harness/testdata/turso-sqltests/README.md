# Turso SQLite-compatibility corpus

Vendored from https://github.com/tursodatabase/turso at commit
`809c89b1c49b639e248de58623c61db5ff42c8b9` (MIT, see `LICENSE.md`):

- `sqlite-sqltests/` is `sqlite/conformance/sqlite-sqltests/`, the `*.sqltest`
  files only. The `.snap` snapshot files are not vendored: the snapshot cases
  record Turso's own EXPLAIN output and are not run.
- `database/` is `sqlite/conformance/database/`, the C SQLite files the corpus
  opens with `@database <path> readonly`. Turso builds its `integrity_*.db`
  fixtures at run time by patching pages; they are not in its repository and
  are not here.

The format is described by `testing/sqltest/docs/dsl-spec.md` in that
repository. `turso_sqltests_test.go` parses it and runs every test against
musql and C SQLite (the oracle); Turso's recorded `expect` blocks are only
compared against C SQLite to report where they disagree.
