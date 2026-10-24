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
  created_at. Admin creates, disables, resets passwords. (Self sign-up,
  passkeys and federation are later.)
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
  databases default to (`plain` standalone, `crdt` in a cluster). Every org
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
  and in the WebSocket hello.
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
- **Standalone and cluster**: see below; a `crdt` database's history is its
  node's.

## Deployments: standalone and cluster

A deployment is one node (**standalone**) or several (**cluster**). The config
alone decides: no `peers` is standalone.

### Standalone (the default)

With no `peers`, a node is the single server described above: no libp2p host
is started and no node key is made, the state and every database are opened
with the plain driver (no capture, so triggers, virtual tables and FTS,
WITHOUT ROWID and rowids all work), and history is the physical log of
`HISTORY.md`. Its databases are of type `plain`, the only type it accepts
(`crdt` needs peers to replicate to and is declined).

A standalone node does not join a cluster in place: declined for now. Its
state and `plain` databases carry no replication bookkeeping and may hold what
CRDT capture refuses, so turning them into replicated files is a migration of
its own (export each database, check it against the limits below, upload it
as a `crdt` database in the cluster). A cluster starts from nodes with a fresh
`data_dir`.

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
- **Every node serves everything it holds.** There is no primary, no control
  node and no proxy: each node serves the management API, `/oidc/`, `/login`,
  the console, and Hrana for every database placed at its location, from its
  own files. `public_url` is the deployment's (behind geo-routing or a load
  balancer); every node issues tokens for it.
- **The state** (`state.musq`) is replicated with `replication` in CRDT mode
  on every node: a write is local, and reaches the others in the background
  (pushed at commit, anti-entropy every 2s).
- **Databases** are typed (`type` on a database, defaulting to its group's):

  | type | where | |
  | --- | --- | --- |
  | `plain` | standalone only | the full engine, physical history, one writer |
  | `crdt` | cluster only | opened with `replication.CRDT()` on every node of its group's locations; every node accepts writes locally |
  | `raft` | — | a known value, declined: "raft databases are not yet supported" |

  Placement follows the state: every node runs a reconcile (at start, after
  each state change, and every few seconds) that opens each `crdt` database
  whose group lists its location and is not open yet (creating an empty file,
  which syncs schema and rows from the peers), and closes and removes the
  file and history of one no longer placed there, or deleted.
  `AddLocation`/`RemoveLocation` are state writes like any other. A `crdt`
  database's file stays open while placed (its sync runs in the background);
  Hrana writes through the replicated `*sql.DB`. A database whose group does
  not list this node's location answers 404 here, naming the locations it
  lives in; nothing is forwarded.
- **History is per node.** Each node logs its own file's physical history
  (`cloud/store`), as standalone does: what that node's copy held, with its
  replication bookkeeping. Restore and branch of a `crdt` database are
  declined for now: a restored copy carries the node's site and op log, and
  bringing it back into a replicated group is a replication operation of its
  own (an op-log rewind), not a file copy. Export reads the local copy.

#### What a `crdt` database refuses

The limits are `replication/`'s, enforced by it when a statement commits, so a
node never stores what it cannot replicate: the statement fails with an error
and nothing diverges.

- triggers;
- virtual tables (FTS3/4/5, rtree, …);
- `WITHOUT ROWID` tables;
- a key column collated other than `BINARY`, and a `NULL` in a key column;
- writes through any connection but the replicated one (Hrana's is it).

And what CRDT means for the rest: constraints hold for each node's own
writes; a merge may break a multi-column `CHECK` or orphan a child, and a
`UNIQUE` value written on two nodes at once is kept by the newer row (the
other is held out until the value frees). An upload into a `crdt` database
is opened through replication too, so a file holding any of the above is
refused.

#### The state under CRDT

Conflicting writes merge per column, last writer (hybrid clock) wins. Creates
that claim a name are made deterministic or visible, never two rows with one
name:

- **Users** have an ID derived from their normalized email, as do their
  personal org, its default group and the membership: the same email created
  on two nodes at once is one user, its columns merged (the later password or
  name wins). A user deleted and created again has the same ID: an access
  token from before the delete works for the new account until it expires
  (≤ 1h); refresh and platform tokens are deleted with the user.
- **Org slugs, group names, database names and user emails** are `UNIQUE`;
  when two nodes write the same one, replication keeps the row with the newer
  hybrid clock and holds the other out. Every node then runs the same
  resolution: a held-out row is written back under its name suffixed with the
  first 6 characters of its ID (`shop` → `shop-1a2b3c`; an email gets
  `+<id6>` before the `@`), so both rows exist under distinct names, and the
  node logs the rename. Every node writes the same values, so the merge is
  the same everywhere.
- **Invites** (`org`, `email`): the newer one stands, the other stays held
  out (same intent).
- **Usage** is an increment, so it is kept per node (`node` in the key) and
  summed when read. A `token_gen` bump needs no sum: any bump changes the
  generation, which is all invalidation needs.
- **Signing keys**: every node signs. A node starts a key of its own when it
  has none and rotates on schedule; JWKS publishes every key in the
  replicated set, and verification reloads the set on an unknown `kid` (and
  at each 2s sync pass), so a token signed anywhere verifies everywhere once its key
  has synced: one sync interval after a node's new key.
- **Refresh tokens** rotate on use, and a reused one revokes its family. A
  use reaches the other nodes only at sync, so within one sync interval
  (≈2s plus the network) the same refresh token presented to two nodes
  succeeds on both, giving two children of one parent; once the uses meet,
  any later use of the parent is a reuse and revokes the whole family on
  every node. Revocation (logout, `/oidc/revoke`, disabling a user, deleting
  a platform token) reaches the other nodes within the same window.
  Authorization and device codes are single-use per node: one redeemed on a
  node that has not synced it yet is "not found" there, and the client
  retries.

Declined, deliberately: `raft` databases (and with them linearizable
multi-node writes); `plain` databases in a cluster; restore and branch of
`crdt` databases; a standalone node joining a cluster in place; a usage view
that counts more than the nodes have synced.

## Usage analytics

Per database, per hour (per node, summed when read): rows read, rows written, statements, errors, bytes
stored, p50/p95 latency; per org totals. Counted at the Hrana layer and kept
in state. The API returns series; the console charts them. Top queries:
normalized statement text with counts and total time.

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
  Built behind a `jitllm` build tag so a server without it stays small; the
  release build includes it.
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
- `DatabaseService`: List, Get, Create (empty, from upload, as branch, from restore point), Delete, Rename, Usage, ListSnapshots, Restore (to time, new or in place), CreateToken, InvalidateTokens, Upload, Export (musq or C SQLite), Stats (top queries).
- `AIService`: GenerateSQL.

## Console

The current editor becomes the per-database view. Around it:
sign-in through OIDC (code + PKCE), an org switcher, database list
(create, branch, restore, delete, copy URL, create token), group and
location settings, members and roles, users (admins), platform tokens,
usage charts per database and org, and the editor's new pieces:
connections (local files kept in the browser, and remote databases on this
server or any Hrana/libSQL URL with a token), staged edits (changes queue up,
reviewed as SQL, applied in one transaction or discarded), several result
tabs per run with a summary tab, function hints in completion, and AI SQL.

## CLI (`musql`)

Same verbs as `turso` where they apply: `auth login|logout|whoami|token`,
`org list|create|switch|members …`, `group list|create|destroy|locations …`,
`db list|create|show|destroy|shell|tokens create|tokens invalidate|import|export|restore|branch`,
`db inspect` (size, usage), `db shell` (a REPL over Hrana with `.tables`,
`.schema`, `.mode`). Config in `~/.config/musql/` (only the CLI reads it).
