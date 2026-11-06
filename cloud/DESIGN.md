# musql cloud

A self-hosted counterpart to Turso Cloud: one server that hosts musql
databases over Hrana, manages them through an API, signs people in as its
own OIDC provider, and serves the console; plus a CLI companion.

## Layout

| Path | What it is |
| --- | --- |
| `cloud/` | Go module `github.com/samyfodil/musql/cloud` (its own, like `hrana/`; `replace` the engine and `hrana` with `../`). |
| `cloud/cmd/musql-cloud/` | The server binary: reads `config.yaml`, serves everything on one listener. |
| `cloud/cmd/musql/` | The CLI companion (`musql auth login`, `musql db create`, …), modeled on `turso`. |
| `cloud/server/` | Wiring: HTTP mux, config, the service implementations. |
| `cloud/state/` | The service's own state, kept in a musql database (`state.musq`): users, orgs, members, groups, databases, tokens, OIDC clients/keys/sessions, usage. |
| `cloud/oidc/` | The OIDC provider. |
| `cloud/store/` | Database files, snapshots, change log, branching and restore. |
| `cloud/gen/` | Generated code (`buf generate`); never edited by hand. |
| `cloud/console/` | The web console (React, Vite). |
| `cloud/Dockerfile` | The server's image, built from the repository root; see Releases. |
| `proto/cloud/v1/` | The management API, one `.proto` per service. |

Rules from `AGENTS.md` hold: library packages read no environment variables
(only `cmd/`), no hand-rolled protobuf or Connect wire code, generated code
only via `buf generate` from the repo root.

## Configuration

`config.yaml` (path from `-config`, default `./config.yaml`):

```yaml
listen: ":8080"
public_url: "https://cloud.example.com"   # issuer and links
data_dir: "./data"                        # state.musq, databases/, snapshots/
node:
  name: "node-1"
  location: "fra"          # this node's location code
peers: []                  # a cluster's other nodes: {name, location, addrs, peer_id}
ai:
  provider: "jitllm"       # jitllm | openai | anthropic | openrouter | "" (off)
  model: "slm-sql-1.5b"    # provider's model name; for jitllm, a .jlm path or name in models_dir
  api_key: ""              # openai, anthropic, openrouter
  base_url: ""             # override (an OpenAI-compatible server, jitllmd, a proxy)
  jitllm:
    models_dir: "./models"
    embedded: true         # run the model in-process; false = talk to jitllmd at base_url
snapshots:
  every: "1h"
  keep: "168h"
uploads:
  max_bytes: 17179869184   # the largest file one upload may carry (default 16 GiB)
  ttl: "24h"               # an upload that nothing writes to or uses is removed after this
console:
  dir: "./console/dist"    # the console's build, served at /console/
  cors_origins: []         # other origins allowed to call the API and Hrana
```

The admin is not in the config. `musql-cloud init --admin-email a@b.c
[--admin-password …] [-config config.yaml]` opens the state under `data_dir`,
creates the admin with an argon2id hash (or, if the email exists, resets its
password and makes it an enabled admin), and exits. The password is prompted
for twice without echo on a terminal, or read as one line from stdin
(`echo pw | musql-cloud init --admin-email a@b.c`). It is idempotent and is the
way back in after a lost password. The server refuses to start while the state
has no admin. In a cluster `init` writes locally and the row replicates once
the node runs. An old `admin:` block in the config is an error.

## Identity: an OIDC provider backed by musql

The server is its own issuer at `public_url`:

- `/.well-known/openid-configuration`, `/oidc/jwks`, `/oidc/authorize`
  (authorization code + PKCE, S256 only), `/oidc/token` (authorization_code,
  refresh_token, `urn:ietf:params:oauth:grant-type:device_code`),
  `/oidc/device` (device authorization, for the CLI), `/oidc/userinfo`,
  `/oidc/logout`, and a server-rendered `/login` page (email + password).
- Signing keys: Ed25519 (EdDSA) JWTs; keys in state, rotated, old keys kept in
  JWKS until their tokens expire.
- Clients: two built-in public clients, `console` (redirect to
  `public_url/console/callback`) and `cli` (device flow). Admins may register
  more.
- Users: email, display name, argon2id password hash, disabled flag,
  created_at. Admin creates, disables, resets passwords. Accounts are the
  admin's to make: there is no self sign-up, and sign-in is by password
  alone (no passkeys, no third-party provider), deliberately.
- Access tokens are JWTs (`sub`, `exp` ≤ 1h, `token_use: access`); refresh
  tokens are opaque, stored hashed, rotated on use (presenting a used one
  revokes its whole family). The token carries no `org`/`role`: a user has a
  role per org and roles change, so every call looks the role up in state.
  A server admin acts as owner of every org.

## Model

- **Organization**: name, slug. Every user has a personal org; admins create
  shared ones. **Members** have a role: `owner`, `admin`, `member`, `viewer`.
  Invites by email create a pending membership the user accepts on login.
- **Group**: a set of locations a database is placed in, and the type its
  databases default to (`plain`, a regular database, unless set). Every org
  has a `default` group in the creating node's location.
- **Database**: `org/name`, type (`plain` | `crdt` | `raft`), group, created_at, size, optional `parent` and
  `parent_at` (a branch), schema-only flag. Served over Hrana at
  `https://<public_url>/db/<org>/<name>` (and `wss://` for v3).
- **Database token**: a JWT signed by the server, claims `db` (or `group`),
  `access` (`full` | `read-only`), `exp` (optional), and `gen`. `db`/`group`
  hold IDs, not names, so a token survives a rename and never reaches a new
  database that reuses a deleted one's name; `gen` must equal the
  database's (group's) token generation, which InvalidateTokens bumps.
  A Hrana connection is confined (`driver.Conn.Confine`): no ATTACH or VACUUM
  INTO for anyone, and a read-only one runs with `query_only` locked on.
  Hrana requests carry a database token (or a user access token with a
  role that allows it), checked by `hrana.WithAuth` on every HTTP request
  and in the WebSocket hello. A bad token, or a record that is not there,
  answers 401; a check the state could not answer (its master out of reach,
  as around an election) answers 503, and an RPC `Unavailable`, so a client
  retries rather than taking a good token for a bad one.
- **Platform token** (API token): for CI and the CLI without a browser; acts
  as the user who created it, revocable.

## Storage, history, branching, restore

- Each database is `data/databases/<org id>/<db id>.musq` (IDs, so renames
  move nothing; an in-place restore points the record at a new file name),
  opened with the plain driver. Its history is a PHYSICAL commit log in
  `data/history/<org id>/<db id>/`: the engine's commit hook hands each
  commit's file state to `cloud/store`, which logs the delta bytes the commit
  appended, or a new base when the segment file was replaced. Full SQLite
  semantics (triggers, virtual tables, WITHOUT ROWID, rowids) are restored
  bit for bit. See `HISTORY.md`.
- Snapshots (bases) every `snapshots.every` and on request; history older
  than `snapshots.keep` is pruned.
- **Restore to time T**: newest base ≤ T plus the logged commits up to T,
  into a new database (Turso's model) or in place.
- **Branch**: a restore into a new database, at now or at T; schema-only
  recreates the parent's objects empty.
- **Standalone and cluster**: see below; a cluster database's history is
  its node's.

## Uploads and exports

A database file moves in pieces, so neither side holds it in memory.

- **Upload**, three unary calls (connect-web cannot stream a request).
  `StartUpload` (`org`, `size_bytes` if known, `resume` to continue an
  unfinished one) returns the upload's id, the offset the server holds and the
  largest piece, 4 MiB. `UploadChunk` appends a piece at an offset, which must
  be exactly the bytes the server holds: a piece sent twice or out of order is
  refused (`FailedPrecondition`, naming the offset to continue from).
  `FinishUpload` takes the SHA-256 of the whole file, reads the file back to
  check it, and makes it usable; a mismatch removes the upload. `Create` then
  moves the file into place, or converts it if it is a C SQLite file.
- **Where it lives**: `data_dir/uploads/<org id>-<id>.part` while pieces
  arrive, without `.part` once finished. The size of the file is the whole
  state of the upload, so it survives a restart, and a client that gets no
  answer to a piece asks `StartUpload` with `resume` where the upload stands
  (a finished one holds the whole file, and finishing it again answers the
  same). Both clients do; the CLI also takes `--resume <id>` after an
  interrupted run. An upload belongs to its org and to the node that started
  it: in a cluster, every call of one upload, and the `Create` that uses it,
  must reach that node.
- **Limits**: `uploads.max_bytes` is checked against `size_bytes` at
  `StartUpload` and on every piece; `uploads.ttl` removes an upload, finished
  or not, that nothing has written to for that long. Console requests are JSON,
  where a piece is base64: a reverse proxy in front of the server must pass
  request bodies of about 6 MiB.
- **Export** is a server-streaming call. One function (`exportFile`) writes a
  consistent copy (`VACUUM INTO`, then the converter for a C SQLite file) to a
  file under `data_dir/export-*`; the call reads it out in 1 MiB messages, the
  first carrying the size, and removes it when it ends. A stopped server's
  leftovers are removed at the next start. The CLI writes `<file>.part` and
  renames it when whole; the console writes through the File System Access API
  where the browser has one, and folds the pieces into a Blob where not.
- **What is bounded**: memory, whatever the file's size. The transfer holds a
  piece or a message, and the musql-format export (`VACUUM INTO`) stays flat.
  `Create` from an upload runs the engine's `integrity_check`, which reads the
  rows once and sorts the index keys it compares in runs on disk
  (`internal/extsort`). A C SQLite upload is converted first
  (`convert/sqlite`), which checks its source the same way, and a C SQLite
  export is built through a fixed page cache straight into its file. Measured
  heap growth is the same from an 80 MiB file to a 1.7 GiB one of 10M rows
  with two indexes: about 40 MiB for `integrity_check`, under 100 MiB for an
  export, about 115 MiB for an import (64 MiB of it the source's page
  cache). What grows with the file is disk and time: an upload is kept whole
  while it is used, converting a C SQLite one needs about as much room again,
  and the sorts write runs the size of the index keys to the temporary
  directory (keep `TMPDIR` on disk, not tmpfs). That 1.7 GiB file took 3
  minutes to import and 10 to export on a 4-core server. Size
  `uploads.max_bytes` for those.

## Deployments: standalone and cluster

A deployment is one node (**standalone**) or several (**cluster**). The config
alone decides: no `peers` is standalone.

### Standalone (the default)

With no `peers`, a node is the single server described above: no libp2p host
is started and no node key is made, the state and every database are opened
with the plain driver (no capture, so triggers, virtual tables and FTS,
WITHOUT ROWID and rowids all work), and history is the physical log of
`HISTORY.md`. Its databases are regular ones (type `plain`, `raft` an alias),
the only kind it accepts (`crdt` needs peers to replicate to and is
declined).

A standalone node joins a cluster in place: give it `peers` (and the other
nodes it in theirs) and restart it. At that first start it checks every
database it holds against what a cluster refuses (below) without changing
any; one that holds a virtual table stops it, named, so it serves all of its databases or none, and starting it again
without peers lets the operator fix or export it. Then its state is set aside
(`state.musq.import`) and its records join the cluster's state by import, as
a state kept in CRDT mode does (below): the cluster may already have its own,
and the two merge, a name another record holds coming in renamed. A group
placed at no cluster node's location (the node's location may have been
another while it was alone) is placed at its location. Its databases stay
where they are, regular ones, each opened through replication at its next
placement, its rows becoming the node's first ops; placing their groups at
more locations copies them to those nodes. Their history from before stays
readable on this node.

### Cluster

```yaml
node:
  name: "fra-1"
  location: "fra"
  p2p_listen: ["/ip4/0.0.0.0/tcp/4001"]   # the default
peers:
  - name: "iad-1"
    location: "iad"
    addrs: ["/dns4/iad-1.example.com/tcp/4001"]
    peer_id: "12D3KooW…"
```

- **Identity and transport**: each node has a libp2p key, made on its first
  start as `data_dir/node.key` (`musql-cloud -peer-id` prints the ID to put
  in the other nodes' `peers`). Nodes talk libp2p only, dialling the
  configured multiaddrs and redialling a lost peer. A connection gater admits
  only the configured peer IDs, in and out: the transport's handshake
  authenticates the ID, so there is no shared secret. Replication runs over
  `examples/libp2p` (`SyncWith`: one gossipsub router per node, one topic per
  replicated file, `musql-cloud/state` and `musql-cloud/db/<id>`), wired in
  `cloud/cluster`; the engine, driver and `replication/` never see libp2p.
- **Masters, elected by raft.** The state, and each group with a regular
  database, has one node that writes it: the master, elected by a raft group
  (`hashicorp/raft` over a libp2p stream per group, `/musql-cloud/raft/<name>/1`;
  `cloud/cluster.Election`). The state's election runs on every node; a
  group's on the nodes at its locations, its master adding and removing them
  as locations change. The data moves by replication in Leader mode with the
  master as the writer, and a regular database's durability, chosen when it
  is created, decides how a commit is kept:
  - **durable** (the default): the commit goes into its group's raft log
    (replication's Leader with a Quorum) and returns once a majority of the
    group's nodes hold it, so the master stopping loses none. Each node
    applies the log in order (`server.groupLog`); one that has yet to open a
    database waits for it, since the proposer may stop before any other copy
    has the commit. A new master writes once it has applied every entry from
    before its term (a raft Barrier). One log orders every commit, so rowids
    are the table's, 1, 2, 3, as on a single node. A commit costs one round
    trip to a majority, and a database's commits run one at a time.
  - **fast**: the commit returns once the master has it, and replication's
    anti-entropy brings it to the others right after. A commit the master
    made and nobody pulled before it stopped is lost. Rowids come from
    per-node ranges, so two masters overlapping around an election never
    give one rowid to two rows.

  Either way a newly elected master writes only after two sync intervals,
  so a fast database's last commits reach it first. The state's own raft log
  carries only its election. An election's raft state (term, vote, the log)
  is `data_dir/raft/<name>/raft.musq`, a musql database, synced at every
  commit; a snapshot of a group's log records what each durable database
  held, and a node too far behind for the log waits until anti-entropy
  brings it that far. An upload, a branch and a restore into a new database
  start as their first node's rows and reach the others by anti-entropy.
- **Every node serves everything it holds.** Each node serves the management
  API, `/oidc/`, `/login`, the console, and Hrana for every database placed
  at its location. `public_url` is the deployment's (behind geo-routing or a
  load balancer); every node issues tokens for it. What must be written by a
  master goes to it over libp2p (`/musql-cloud/http/1`, which only the
  configured peers can open): a follower's state calls, and Hrana requests and
  history requests for a regular database.
- **The state** (`state.musq`) is replicated with `replication` in Leader mode
  on every node, the state's master writing it. A follower's `state.Store`
  runs every call on the master's, over Hrana on that stream, so a request
  reads back what it wrote; with no master to reach (an election under way),
  reads answer from the node's own copy and a write waits for a master, up to
  15s. A node writes its own copy alone while it starts: its schema
  statements, its signing key, the built-in clients, each the same on every
  node or that node's alone, so the writes merge. A state kept in CRDT mode
  before is not converted in place: replication names a schema object by its
  text, and a file's own tables (made by `CREATE` then `ALTER`, recorded by
  their final text) would not be the cluster's (recorded statement by
  statement), so their rows would not merge. The node starts from a state
  built as every node's is, on a topic of its own
  (`musql-cloud/state/leader`), and imports the old file's records into it
  through the master (`state.Store.Import`): a record already there by its
  key stays, one whose name is taken comes in renamed as below. The old file
  stays as `state.musq.crdt`, the copy imported as `state.musq.imported`.
- **Databases** are typed (`type` on a database, defaulting to its group's,
  `plain` unless set):

  | type | where | |
  | --- | --- | --- |
  | `plain` (regular) | anywhere | standalone: the full engine, physical history, one writer. In a cluster: opened with `replication.Leader` on every node of its group's locations, its group's master the writer; a write on any other node is forwarded to it |
  | `crdt` | cluster only | opened with `replication.CRDT()` on every node of its group's locations; every node accepts writes locally |
  | `raft` | anywhere | a regular database: in a cluster raft elects the master that writes it |

  Placement follows the state: every node runs a reconcile (at start, after
  each state change, and every few seconds) that opens each database whose
  group lists its location and is not open yet (creating an empty file,
  which syncs schema and rows from the peers), and closes and removes the
  file and history of one no longer placed there, or deleted. A regular
  database is served by its group's master, reads included (a stream's
  statements run where its transaction is), and so are its history, export,
  branches and restores: the master's copy is the one every write went
  through. A request waits up to 10s for a master while one is elected.
  `AddLocation`/`RemoveLocation` are state writes like any other. A
  database's file stays open while placed (its sync runs in the background);
  Hrana writes through the replicated `*sql.DB`. A database whose group does
  not list this node's location answers 404 here, naming the locations it
  lives in; it is not forwarded.
- **History is per node.** Each node logs its own file's physical history
  (`cloud/store`), as standalone does: what that node's copy held, with its
  replication bookkeeping. A restored file carries the node's site and op
  log, so none goes back into a replicated group as a file: export, branch
  and restore take a **plain copy** (`replication.PlainCopy`: the schema and
  rows, without the bookkeeping or the capture guard) of this node's file or
  of its history at T. Export is the plain copy of the local file. A branch
  or a restore into a new database is a new database of its parent's type
  made on this node from the plain copy, whose rows become its first ops (a
  regular database's are made on its master). A restore in
  place writes the difference between now and T through the replicated
  handle, in one transaction, so every node converges to T plus the writes
  made concurrently, its schema made T's too and a row withheld now ended.
  See `HISTORY.md`, "Per node, in a cluster".

#### What a cluster database refuses

The limits are `replication/`'s, enforced by it when a statement commits, so
a node never stores what it cannot replicate: the statement fails with an
error and nothing diverges. Either type refuses:

- virtual tables (FTS3/4/5, rtree, …);
- a key column collated other than `BINARY`, and a `NULL` in a key column;
- writes through any connection but the replicated one (Hrana's is it).

A crdt database also refuses triggers and `WITHOUT ROWID` tables. A regular
one takes them: its master runs a trigger, what the trigger wrote reaches the
others as rows, and they apply them with their own triggers off. A restore in
place runs with triggers off too, so the rows it writes are the point's.

And what CRDT means for the rest: constraints hold for each node's own
writes; a merge may break a multi-column `CHECK` or orphan a child, and a
`UNIQUE` value written on two nodes at once is kept by the newer row (the
other is held out until the value frees). A regular database's master checks
every constraint, and the others hold what it holds. An upload into a
cluster database is opened through replication too, so a file holding any of
the above is refused.

#### The state's writes

Every write reaches the state's master, which runs them one at a time: a
name is claimed once, a refresh token's use and an authorization or device
code's redemption are seen by the next call anywhere, and revocation holds at
once. Two masters can overlap for a moment around an election (one that has
not yet seen it lost its term); what both wrote then merges per column, last
writer (hybrid clock) wins, and creates that claim a name are made
deterministic or visible, never two rows with one name:

- **Users** have an ID derived from their normalized email, as do their
  personal org, its default group and the membership: the same email created
  on two masters at once is one user, its columns merged (the later password
  or name wins). A user deleted and created again has the same ID: an access
  token from before the delete works for the new account until it expires
  (≤ 1h); refresh and platform tokens are deleted with the user.
- **Org slugs, group names, database names and user emails** are `UNIQUE`;
  when two masters write the same one, replication keeps the row with the
  newer hybrid clock and holds the other out. The master then writes the held
  row back under its name suffixed with the first 6 characters of its ID
  (`shop` → `shop-1a2b3c`; an email gets `+<id6>` before the `@`), so both
  rows exist under distinct names, and logs the rename.
- **Invites** (`org`, `email`): the newer one stands, the other stays held
  out (same intent).
- **Usage** is kept per node (`node` in the key) and summed when read: each
  node counts what it served.
- **Signing keys**: every node signs. A node starts a key of its own when it
  has none and rotates on schedule; JWKS publishes every key in the
  replicated set, and verification reloads the set on an unknown `kid` (and
  at each 2s sync pass), so a token signed anywhere verifies everywhere once
  its key has synced: one sync interval after a node's new key.

Declined, deliberately: linearizable writes to the state (its master's commit
is durable on the master and reaches the others after it); a standalone
database holding what a cluster refuses joining one.

## Usage analytics

Per database, per hour (per node, summed when read): rows read, rows written, statements, errors, bytes
stored, p50/p95 latency; per org totals. Counted at the Hrana layer and kept
in state. The API returns series; the console charts them. Top queries:
normalized statement text with counts and total time.

Each node counts what it serves in memory and writes it to the state every
minute. A usage or stats read first writes this node's counts and has every
other node it reaches write its own (`/usage/flush` over libp2p, waiting up
to 2s), so it counts every statement those nodes have served; a node that
does not answer in time is counted at its next write.

## AI SQL

`AI.GenerateSQL(database, prompt)` sends the schema (CREATE statements, no
data) and the prompt to the configured provider and returns SQL and a short
explanation; the console never runs it without the user pressing Run. The
SQL is checked before it is returned: it must parse (`EXPLAIN` it on a
read-only connection), and a statement that would write is labelled so.

Providers, behind one `cloud/ai` interface (`Generate(ctx, system, user)`):

- **jitllm, embedded** (the default): `github.com/jitllm/jitllm/engine/model`
  in-process, a `.jlm` from `models_dir`. The default model is SLM-SQL 1.5B
  (Qwen2.5-Coder based, 67.1% on BIRD), small enough to answer fast on a CPU.
  The model is opened once, lazily, sessions are short-lived, decoding is
  greedy with a token budget, and generation is cut at the end of the SQL.
  Built behind a `jitllm` build tag, so a server without it carries none of it
  (the tag adds 5 MiB to the stripped linux/amd64 binary: 61 MiB without,
  66 MiB with). The release archives and the image include it (see Releases);
  the model is in neither: point `jitllm.models_dir` at a directory with the
  `.jlm`.
- **jitllm server**: `jitllmd`'s OpenAI-compatible API at `base_url`.
- **OpenAI**: Chat Completions, `api_key`, `model`.
- **Anthropic**: Messages API, `api_key`, `model`.
- **OpenRouter**: its OpenAI-compatible API (`https://openrouter.ai/api/v1`),
  any model it routes to.

Calls go through the official Go SDKs where they exist
(`github.com/openai/openai-go`, `github.com/anthropics/anthropic-sdk-go`;
OpenRouter through the OpenAI SDK with its base URL).

## API (ConnectRPC, `proto/cloud/v1`)

Services, each method checks the caller's role:

- `AuthService`: WhoAmI, ListPlatformTokens, CreatePlatformToken, RevokePlatformToken.
- `UserService` (admin): List, Create, Update (name, disabled), ResetPassword, Delete.
- `OrgService`: List, Create, Update, Delete, ListMembers, Invite, UpdateMember, RemoveMember, ListInvites, AcceptInvite.
- `GroupService`: List, Create, Delete, AddLocation, RemoveLocation, ListLocations.
- `DatabaseService`: List, Get, Create (empty, from upload, as branch, from restore point), Delete, Rename, Usage, ListSnapshots, Restore (to time, new or in place), CreateToken, InvalidateTokens, StartUpload / UploadChunk / FinishUpload (a database file in pieces), Export (musq or C SQLite, streamed), Stats (top queries).
- `AIService`: GenerateSQL.

## Console

The current editor becomes the per-database view. Around it:
sign-in through OIDC (code + PKCE), an org switcher, database list
(create, branch, restore, rename, delete, copy URL, create token), group and
location settings, members and roles, org settings (deleting the org, for
owners, confirmed by typing its slug), users (admins), platform tokens,
usage charts per database and org, and the editor's new pieces:
connections (local files kept in the browser, and remote databases on this
server or any Hrana/libSQL URL with a token), staged edits (changes queue up,
reviewed as SQL, applied in one transaction or discarded), several result
tabs per run with a summary tab, function hints in completion, and AI SQL.

A database of this server open in the editor runs on a one-hour token that
the console renews halfway through its life, and again when Hrana answers
401; the console's own session is refreshed the same way, and ends only when
the token endpoint refuses the refresh token. Then the editor shows a "signed
out" bar with a sign-in button, and the database stops answering once its
current token expires, with that reason.

## CLI (`musql`)

Same verbs as `turso` where they apply: `auth login|logout|whoami|token`,
`org list|create|switch|members …`, `group list|create|destroy|locations …`,
`db list|create|show|destroy|shell|tokens create|tokens invalidate|import|export|restore|branch`,
`db inspect` (size, usage), `db shell` (a REPL over Hrana with `.tables`,
`.schema`, `.mode`). `db import`, `db create --from-file` and `db export`
move the file in pieces with progress on stderr; an interrupted upload
continues with `--resume <id>`. Config in `~/.config/musql/` (only the CLI
reads it).

## Releases

A tag `v*` runs `.github/workflows/release.yml`. It attaches to the GitHub
release, beside the `musql-<os>-<arch>` archives of `musqld`, `musql-convert`
and the Doom example, a `musql-cloud-<os>-<arch>` archive (`.tar.gz`, `.zip`
on Windows) for linux, darwin and windows on amd64 and arm64. It also pushes
the image `ghcr.io/samyfodil/musql-cloud` for linux/amd64 and linux/arm64,
tagged with the version and `latest`.

An archive holds `musql-cloud` (built with `-tags jitllm`, so the embedded AI
is in), `musql`, `console/` (the console's build, made once for all six),
`config.example.yaml` with its paths set for this layout (`console/` and `data/`
beside the binary), `LICENSE`, `README.md` and this file. All six build with
`CGO_ENABLED=0` and the tag. `musql-cloud` is 61 to 68 MiB stripped.

```sh
tar xzf musql-cloud-linux-amd64.tar.gz && cd musql-cloud-linux-amd64
cp config.example.yaml config.yaml        # set public_url, the OIDC issuer
./musql-cloud init --admin-email you@example.com
./musql-cloud                             # -config config.yaml
```

The image is `scratch` and the static binary (`cloud/Dockerfile`, built from
the repository root; it builds the console itself). It runs as 65532 and
keeps its state in the `/data` volume. Its `/config.yaml` sets only
`listen: ":8080"`, `data_dir: "/data"` and `console.dir: "/console"`; mount your
own over it to set `public_url`, `peers` or `ai`, and a directory of models for
`ai.jitllm.models_dir`. `init` is the same command:

```sh
docker run --rm -it -v musql-cloud:/data ghcr.io/samyfodil/musql-cloud init --admin-email you@example.com
docker run -p 8080:8080 -v musql-cloud:/data ghcr.io/samyfodil/musql-cloud
```

`make cloud TAGS=jitllm` builds the same server and CLI into `bin/`. CI builds
the server with the tag, runs this module's tests with it, and cross-compiles
the other release targets.
