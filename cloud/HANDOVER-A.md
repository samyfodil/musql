# musql cloud: stage A handover

Stage A is the server core. Stages B (CLI), C (console) and D (history,
regions, usage, AI) build on it; every service they need is already in
`proto/cloud/v1`.

## Done

- `proto/cloud/v1/*.proto`: Auth, User, Org, Group, Database and AI
  services. `buf generate` (repo root) writes `cloud/gen`; `buf.gen.yaml`
  now emits by `go_package` from the repository root (`module=`), so it has
  no `clean` (a removed .proto's output is deleted by hand). `hrana/gen` is
  byte-identical.
- `driver.Conn.Confine(readOnly)` (root module): refuses ATTACH and VACUUM
  INTO, and with readOnly sets `query_only` and refuses turning it off, in
  the driver and in the engine session. Tested in `driver/confine_test.go`.
- `cloud/state`: users (argon2id), orgs, members, invites, groups and
  locations, databases, platform tokens, refresh tokens (rotation with reuse
  detection), signing keys, OIDC clients, device and auth codes; migrations
  at open.
- `cloud/oidc`: discovery, JWKS, authorize (code + PKCE S256 only), token
  (authorization_code, refresh_token, device_code), device authorization and
  its verify page, userinfo, logout, `/login`. EdDSA via go-jose v4. Built-in
  public clients `console` (redirect `public_url/console/callback`) and
  `cli`.
- `cloud/server`: config parsing, admin created/reset from config at every
  start, all RPCs below with role checks, Hrana at `/db/<org>/<name>`, CORS
  for `console.cors_origins`, the console build at `/console/` with SPA
  fallback.
- `cloud/cmd/musql-cloud`: `-config` flag.

## Stubbed (CodeUnimplemented)

- DatabaseService: Usage, ListSnapshots, Restore, Stats; Create with
  `branch_of`, `branch_at` or `schema_only`.
- AIService.GenerateSQL. The `ai:` config block (provider/model/api_key/
  base_url/jitllm) is parsed and carried in `Config.AI`, unused.
- `peers`, `snapshots` are parsed, unused. Databases open with the plain
  driver, not `replication.Open` (stage D).

## Roles

Server admins are owners everywhere. viewer: list/get, read-only database
tokens, Hrana read-only with their own token. member: create/delete/rename/
export/upload databases, full tokens, invalidate tokens, Hrana read-write.
admin: groups and locations, invites, member roles (only an owner grants or
removes owner; the last owner stays). owner: delete a non-personal org.

## Run

```sh
cd cloud
go run ./cmd/musql-cloud -config config.yaml
```

```yaml
listen: ":8080"
public_url: "http://localhost:8080"
data_dir: "./data"
admin:
  email: "admin@example.com"
  password: "change-me-please"
node:
  name: "node-1"
  location: "fra"
console:
  dir: "./console/dist"
  cors_origins: ["http://localhost:5173"]
```

Then sign in at `/login`, or get tokens with the device flow
(`POST /oidc/device` with `client_id=cli`). Hrana clients use
`http://localhost:8080/db/<org>/<name>` with a token from
`DatabaseService.CreateToken`.

## Tests

```sh
cd cloud && ../scripts/cap -- go test -count=1 ./...
cd .. && scripts/cap -- go test -count=1 -run TestConfine ./driver/
```

Both green at handover. The engine suite, harness and sweeps were not run
(the Confine change touches `engine/` in five small places; run the gates on
the servers before merging). `engine/no_sqlite_format_test.go` now skips
`cloud/` (it imports the converter).

## Open issues

- WebSocket Hrana must send `Authorization` on the upgrade; libSQL clients
  that only put the token in the hello get 401. Fix: let `hrana` accept a
  per-request token check (an Option taking a func), then check the hello.
- Signing keys are never rotated; every key is kept because database tokens
  may not expire. Rotation needs a retire policy for non-expiring tokens.
- The login and device pages rely on SameSite=Lax cookies against CSRF; no
  CSRF token, and no rate limit on `/login`.
- Upload is one unary RPC capped at 256 MiB; a streaming or HTTP upload is
  better for big files.
- Export copies with `VACUUM INTO` on an unconfined connection, then
  converts for SQLite; the whole file is held in memory for the response.
- Deleting a user leaves orgs it alone owned.
