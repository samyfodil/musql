# musql cloud: stage B handover

Stage B is the `musql` CLI, modeled on `turso`. Logic is in `cloud/cli`
(cobra); `cloud/cmd/musql` only fills `cli.Env` from the process.

## Done

- `auth login` (device flow against `/oidc/device` + `/oidc/token`, client
  `cli`; prints the link and code, opens a browser unless `--headless`),
  `logout`, `whoami`, `token`, `api-tokens list|mint|revoke`.
- `config set url|org`, `config show`. `--url`, `--org`, `--json` are global.
- `org list|create|switch`, `org members list|invite|rm|role` (members by
  email).
- `group list|create|destroy`, `group locations list|add|rm`.
- `db list|create [--group] [--from-file] [--from-db] [--timestamp]|show
  [--url]|destroy [--yes]|rename|tokens create [--read-only] [--expiration
  never|7d|12h] [--group]|tokens invalidate|import|export [--sqlite] [-o]|
  restore --timestamp [--new-name]|branch|inspect|shell`.
- `db shell <name> [sql]`: mints a one-hour database token (full, or
  read-only when the role allows only that) and connects with
  `libsql-client-go` over Hrana HTTP. Runs the SQL argument, else a piped
  script, else a REPL on a terminal (x/term line editing, history in
  `<dir>/history`): statements end at a `;` the engine's own
  `SplitStatements` agrees on, `.tables`, `.schema [t]`, `.mode
  table|csv|json`, `.help`, `.quit`.
- Credentials in `<dir>/config.json`, mode 0600, written atomically. The
  access token is refreshed 30 s before expiry and the rotated refresh
  token saved. An RPC answering Unimplemented prints "this server does not
  support that yet" (restore, branch, usage today).

The command reads `MUSQL_CONFIG_DIR` (default `os.UserConfigDir()/musql`)
and `MUSQL_API_TOKEN` (a platform token, for CI). No server code changed.

## Tests

```sh
cd cloud && ../scripts/cap -- go test -count=1 ./...
```

Green. `cli/e2e_test.go` starts the stage A server on httptest, drives the
device flow (approving the printed code with `Store().DecideDevice`), checks
token refresh and rotation, creates an org and databases, mints tokens, runs
SQL through `db shell`, exports .musq and C SQLite, imports the export, and
uses an API token. `TestREPL` drives the REPL with scripted lines.

## Gaps

- `restore`, `branch`, `--from-db`, `inspect`'s usage wait on stage D.
- `org members` has no `invites list|accept`; invites are accepted on the
  console or via the API.
- `db shell` uses Hrana over HTTP only; WebSocket waits on the hello-token
  fix noted in HANDOVER-A.
- `auth logout` forgets local tokens but does not revoke the refresh token
  on the server (no revocation endpoint yet).
- Uploads and exports are whole files in memory (the API is unary).
