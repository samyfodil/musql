# musql cloud: stage C handover

Stage C is the console's cloud pages. It builds on stage A (`01c9e13d`).

## Done

- `buf.gen.yaml` has a third plugin, `protoc-gen-es` 2.16.0 (the console's
  pinned dev dependency, run from `cloud/console/node_modules/.bin`), writing
  `cloud.v1` only as JS + `.d.ts` into `cloud/console/src/gen/`. Run
  `npm install` in `cloud/console` before `buf generate`. The Go outputs are
  unchanged.
- The console calls the API with `@connectrpc/connect-web` 2.1.1
  (`src/cloud/api.js`): one transport, an interceptor adding the bearer token
  and retrying once after a refresh on Unauthenticated.
- Sign-in (`src/cloud/auth.js`): authorization code + PKCE S256 as client
  `console`, redirect `/console/callback`, scope
  `openid profile email offline_access`. The access token is in memory, the
  refresh token in sessionStorage (a reload stays signed in, a new tab signs
  in again); a timer refreshes a minute before expiry; refreshes share one
  request, since refresh tokens rotate. Sign out clears both and goes through
  `/oidc/logout` back to `/console/`.
- A "Cloud" rail section (`src/cloud/Cloud.jsx`, `useCloud.js`): org switcher
  (+ new org), Databases (list with group, size, created; create empty,
  upload, branch, restore to a time; open in the editor; copy URL; database
  token with read-only and expiry, shown once; export musq/SQLite; invalidate
  tokens; delete with type-to-confirm), Groups & locations, Members (role
  change with Undo, remove, invite, pending invites, invites for you with
  Accept), Users (admins: create, disable with Undo, reset password), Platform
  tokens (mint shown once, list, revoke), Usage (per org or per database,
  Recharts lines and top queries; a clean empty state while Usage/Stats are
  Unimplemented). ⌘K has a "Cloud: <page>" command per page.
- Unimplemented calls read "not available on this server" (branch, restore,
  usage, stats today).
- Opening a database mints a 1-hour token (read-only for viewers) and opens
  it with the existing remote backend (`connect` in `db.js`), named
  `<org>/<name>`; it is not added to the kept connections.
- Without a server (the console served anywhere but `/console/`), the Cloud
  section asks for a server URL and sends the browser to that server's
  console; local files and plain Hrana URLs work as before.

## Verified

`npx vite build` and `node schema.test.mjs` pass. End to end against the
stage A server (temp config, console dir = `cloud/console/dist`), driven in
a browser: sign in as admin, reload (refresh token), create a database, open
it in the editor, run CREATE/INSERT/SELECT over Hrana, invite a member, mint
a platform token, create a database token, branch (shows not available),
delete with type-to-confirm, groups, users, usage empty state, dark theme,
and the console without a server (sample opens, Cloud asks for a URL).

## Server changes needed

- `Database.url` has no trailing slash, and Hrana clients resolve
  `v2/pipeline` against it, which drops the last segment
  (`/db/<org>/v2/pipeline`, 404). The console appends the slash; the server
  should return `.../db/<org>/<name>/` (or accept both) so other clients work.
- Signing in from a console on another origin cannot work: the `console`
  client has one redirect URI, and `/oidc/token` gets no CORS headers (only
  `/cloud.v1.*` and `/db/`). Allowing `console.cors_origins` on `/oidc/token`
  and as `<origin>/console/callback` redirect URIs would let a dev server
  (`npm run dev`) sign in directly.
- WhoAmI lists only memberships; for an admin the console lists all orgs via
  OrgService.List and treats them as owner.

## Gaps

- A cloud database opened in the editor stops working when its 1-hour token
  expires; reopen it from the list. A reload does not reopen it.
- No rename, InvalidateTokens for a group, group-scoped tokens, or org
  rename/delete in the UI yet; AI SQL is not wired.
- The Usage "per org" view sums per-database series client-side (one Usage
  call per database).
