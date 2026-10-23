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
admin:
  email: "admin@example.com"
  password: "…"           # or password_hash: argon2id encoded
node:
  name: "node-1"
  location: "fra"          # this node's location code
peers: []                  # other nodes: {name, location, url, secret}
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

On first start the admin user is created (or its password reset) from the
config; it is the only way in until it adds others.

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
- **Group**: a set of locations a database is placed in; primary location is
  the writer. Every org has a `default` group in the node's location.
- **Database**: `org/name`, group, created_at, size, optional `parent` and
  `parent_at` (a branch), schema-only flag. Served over Hrana at
  `https://<public_url>/db/<org>/<name>` (and `wss://` for v3).
- **Database token**: a JWT signed by the server, claims `db` (or `group`),
  `access` (`full` | `read-only`), `exp` (optional), and `gen`. `db`/`group`
  hold IDs, not names, so a token survives a rename and never reaches a new
  database that reuses a deleted one's name; `gen` must equal the
  database's (group's) token generation, which InvalidateTokens bumps.
  A Hrana connection is confined (`driver.Conn.Confine`): no ATTACH or VACUUM
  INTO for anyone, and a read-only one runs with `query_only` locked on.
  Hrana over WebSocket authenticates with the upgrade request's
  Authorization header (the hello's token is not checked per database yet). Hrana requests must carry
  one (or a user access token with a role that allows it).
- **Platform token** (API token): for CI and the CLI without a browser; acts
  as the user who created it, revocable.

## Storage, history, branching, restore

- Each database is `data/databases/<org id>/<db id>.musq` (IDs, so renames
  move nothing), opened through
  `replication.Open` so writes are captured in the change log
  (`replication/` op log), giving PITR without a new format.
- Snapshots every `snapshots.every`: an export of the database at an HLC
  point, kept for `snapshots.keep`.
- **Restore to time T**: newest snapshot ≤ T, replay the change log up to T,
  into a new database (Turso's model: restore creates a new db) or in place.
- **Branch**: a new database from the parent at now (or at T, via restore).
- **Groups & regions**: a database's group lists locations; the primary
  node writes, others replicate with `replication.Leader` over an HTTP
  transport between peers (`peers` in config, shared secret). A request to a
  replica for a write is forwarded to the primary.

## Usage analytics

Per database, per hour: rows read, rows written, statements, errors, bytes
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
