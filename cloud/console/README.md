# musql console

A database editor that runs entirely in the browser, on the npm package
(`../npm`): musql compiled to WebAssembly, in a Web Worker so its JIT is on.
Nothing is uploaded anywhere.

- Open a `.musq` file or a C SQLite file (drop it anywhere, or ⌘O). SQLite
  files are converted on the way in.
- Browse tables and views a page at a time, filter with a WHERE clause, sort by
  any column, and see each table's columns, keys, indexes and DDL.
- Edit a cell with a double-click, add rows, and delete selected rows.
- Write SQL with highlighting and completion of table and column names;
  ⌘/Ctrl+Enter runs the selection or the whole editor.
- "Review changes before saving" (the table toolbar) queues edits, pastes,
  inserts and deletes: Review shows the statements, Save (⌘S) runs them in one
  transaction, Discard drops them.
- Each statement that returns rows gets a result tab, beside a summary of the
  whole run; completion knows the SQL functions and shows a call's signature.
- Connect to a remote database: a Hrana/libSQL URL and a token, kept in this
  browser (`@libsql/client`). A remote one downloads as a SQL dump, and undo
  is for local databases only. The server must allow the console's origin
  (CORS).
- Export as a C SQLite file (`.sqlite`) or a musql file (`.musq`).
- Semantic search: "Embed…" turns a text column into vectors
  (all-MiniLM-L6-v2 via transformers.js, in a worker of its own, downloaded
  once and cached) stored in a `<column>_emb F32_BLOB(384)` column, and a
  table with such a column gets a search box that ranks rows by
  `vector_distance_cos` -- the exact search, run in musql's SIMD kernels. The
  sample has a table of notes to try it on.

## Run it

```sh
sh ../npm/build.sh     # the wasm module the package ships
npm install
npm run dev            # or: npm run build, then serve dist/
npm test               # the *.test.mjs files, under Node 22.15 or later
```

React and CodeMirror, built with Vite.

## As a library

The console is also a package, `@samyfodil/musql-console`, used two ways:

- `createConsole(root, options)`: the stock console plus an app's own pages,
  name and colors, without a fork. `src/main.jsx` is the stock app:
  `createConsole(root, {})`.
- [Components](#components): the pieces the stock console is built from
  (the provider, the database editor, the cloud pages, the building blocks),
  for an app that is its own shell.

`example/` is such a shell: its own header, org switcher and navigation, the
cloud pages, and a database opening in `DatabaseWorkspace`, with no local
studio and no server picker (`npm run example`, or `npm run build:example` for
`example/dist/`; it signs in when a musql cloud server serves it, with
`console.dir` set to `example/dist`).

The package's entry is its source (`src/index.js`), built by the app's own
Vite. Vite's library mode would inline the wasm module and the embedding
model's runtime into the JavaScript as base64; built by the app, they stay
files next to it.

### Depending on it

```sh
npm install @samyfodil/musql-console react react-dom
npm install -D vite @vitejs/plugin-react
```

Every musql release publishes it at the release's version, depending on
`@samyfodil/musql` at the same version. `pack.mjs` writes the published
package from this directory. It ships transformers.js's browser build in
place of `@huggingface/transformers`, so the install does not pull in
onnxruntime-node and its CUDA download. `test-package.sh` installs both
packages into an app outside the repository and builds it, as CI does.

```js
// vite.config.js
import react from "@vitejs/plugin-react";
import musqlConsole from "@samyfodil/musql-console/vite";

export default { base: "./", plugins: [react(), musqlConsole()] };
```

`musqlConsole()` keeps one copy of React for the console and the app's
components, excludes the wasm packages from pre-bundling, and builds workers
as modules. Build with `vite build` and serve the output where musql cloud
serves its console (`console.dir`), under `/console/`.

### createConsole(root, options)

```js
import { createConsole, useConsole } from "@samyfodil/musql-console";

const { unmount } = createConsole(document.getElementById("app"), {
	branding: { name, studioName, logo, favicon, docsURL, supportURL },
	theme: { accent: "#0b7285" },   // or { light: {...}, dark: {...} }
	pages: [{ key, label, icon, component, admin, placement }],
	databaseTabs: [{ key, label, icon, component }],
	banners: [Component],
	api: { baseURL },
});
```

Every option may be left out.

- `branding`: `name` replaces "musql cloud" and `studioName` "musql studio"
  wherever the console shows them. `logo` is an image URL or a React node (the
  default is μ), `favicon` an image URL. `docsURL` and `supportURL`, when set,
  are linked under the cloud list and from the ⌘K palette. The server's own
  sign-in page is not the console's and keeps its look.
- `theme`: CSS variables that override `src/style.css`'s tokens (`bg`,
  `panel`, `side`, `rail`, `line`, `text`, `dim`, `accent`, `accent-soft`,
  `sel`, `err`, `radius`, ...), with or without the leading `--`. One set
  applies to light and dark; `{light, dark}` sets each.
- `pages`: added, in order, after the cloud list's own pages, and to the
  palette. `admin: true` shows a page to server admins only. `placement:
  "database"` makes it a database tab instead. A key must be unique and not
  one of the console's own (`databases`, `groups`, `members`, `usage`,
  `settings`, `tokens`, `users`).
- `databaseTabs`: listed under "Database" in the table list when the database
  open in the editor is one of this server's, and opened as tabs beside its
  tables.
- `banners`: rendered above the tabs in every section. One that has nothing
  to say returns `null`; `className="banner"` gives the console's bar.
- `api.baseURL`: where the ConnectRPC calls go; the console's own origin by
  default.

`icon` is an element, a component such as a `lucide-react` icon, or text.
Pages and tabs render inside the console's page container, so the console's
classes (`cloudhead`, `cloudtable`, `primary`, `dim`, `small`, `mono`) look as
the stock pages do.

### The context

Each page, tab and banner gets `ctx` as a prop, and `useConsole()` returns the
same object anywhere under one:

| Field | What it is |
| --- | --- |
| `api` | The console's ConnectRPC clients: `ai`, `auth`, `db`, `groups`, `orgs`, `users` (`proto/cloud/v1`). |
| `transport` | The transport under them: each call carries the signed-in user's token and retries once after a refresh. `createClient(BillingService, ctx.transport)` calls an app's own service as that user. |
| `token()` | Resolves to a live access token, for a call that is not ConnectRPC. |
| `server` | The cloud server's origin, or `null` when this console is not served by one. |
| `user`, `admin` | The signed-in user (`cloud.v1.User`) or `null`, and whether they are a server admin. |
| `org`, `role`, `orgs` | The organization in view (`cloud.v1.Org`) or `null`, the user's role in it (`"viewer"`, `"member"`, `"admin"`, `"owner"`, `"none"`), and every org they can switch to. |
| `database` | `{org, database}` when the editor has one of this server's databases open, else `null`. |
| `toast(text, isError)` | The console's toast. |
| `navigate(key)` | Opens a cloud page, the console's or an extension's. |
| `openTab(key)` | Opens a database tab on the open database. |
| `branding` | The branding in use, defaults filled in. |

The object changes when any of these do, so an effect depends on the fields
it reads (`ctx.org?.slug`), not on `ctx`. Everything else under `src/` is
private.

## Components

```js
import "@samyfodil/musql-console/style.css"; // optional: ConsoleProvider imports it too
import { ConsoleProvider, DatabaseWorkspace, Databases, useCloud } from "@samyfodil/musql-console";

function Shell() {
	const cloud = useCloud();
	const [open, setOpen] = useState(null);
	if (cloud.auth.status !== "in") return <button onClick={cloud.signIn}>Sign in</button>;
	return open
		? <DatabaseWorkspace key={`${open.org}/${open.database}`} {...open} />
		: <Databases org={cloud.org} onOpenDatabase={({ org, database }) => setOpen({ org, database })} />;
}

createRoot(el).render(<ConsoleProvider><Shell /></ConsoleProvider>);
```

`createConsole` is assembled from the same pieces, so the stock console and a
shell built from them look and behave alike. Everything works under one
`ConsoleProvider`, bound to one server.

### ConsoleProvider

`<ConsoleProvider cloud branding theme>{children}</ConsoleProvider>`

- `cloud`: the server the API calls and sign-in go to, read once when it
  mounts. `{}` (the default) is the page's own origin.
  - `baseURL`: the server's origin.
  - `getToken`: `() => Promise<string>` when the app signs users in itself;
    every call carries its token, and `signIn` / `signOut` (also options)
    are the app's. Without it the console signs in with the server's OIDC
    provider as its `console` client, returning at `callbackPath` on this
    origin (`/console/callback` for a page under `/console/`, else
    `/callback`; the server accepts the second for origins in
    `console.cors_origins`), and keeps the session as the stock console does.
  - `transport`: a ready ConnectRPC transport to call over instead, which
    signs its own calls.
  - `null`: no server.
- `branding`: as `createConsole`'s (`name`, `logo`, `favicon`, `docsURL`,
  `supportURL`).
- `theme`: as `createConsole`'s, CSS variables over the style sheet's tokens.

It holds the style sheet and the fonts, the theme switch (`useTheme()`:
`[theme, setTheme, dark]`), the toasts, and the signed-in state. The wasm
engine runs in a worker started with the package.

### useCloud()

The server the provider is bound to:

| Field | What it is |
| --- | --- |
| `auth` | `{status, error}`: `"starting"`, `"out"`, `"in"`, or `"none"` without a server. |
| `user`, `admin` | The signed-in user (`cloud.v1.User`) or `null`, and whether a server admin. |
| `orgs` | The orgs the user can switch to (`cloud.v1.Org`; a server admin sees every one). |
| `org`, `setOrg(slug)` | The org in view (a slug, kept in this browser) and its setter. |
| `role`, `roleIn(slug)` | The user's role (`Role` from `proto/cloud/v1`) in the org in view, or in another. |
| `signIn()`, `signOut()`, `refresh()` | Sign-in redirect, sign out, and refreshing the session now. |
| `reload()` | Reads the user and orgs again, after creating or leaving one. |
| `server` | The server's origin. |

### The database editor

- `DatabaseWorkspace({ org, database })`: what opening a cloud database
  shows in the stock console: the table list, the table editor and grid, the
  SQL editor with AI SQL, the schema diagram, the row and column panels, over
  Hrana with a token it mints (read-only for a viewer) and renews. It fills
  its container; one database is open at a time on a page, so key it by
  `org/database`. `controls`, an element of the host's such as a slot in
  its header, takes the section and search buttons instead of the rail on
  the left; `title: false` drops the database's name and menu above the
  table list, for a host that already shows which database is open:
  `DatabaseWorkspace({ org, database, controls, title })`.
- `DatabaseProvider({ org, database, children })`: opens the database for
  the parts below, laid out as the app likes; `useDatabase()` under it gives
  `{org, database, phase, error, readOnly, schema, counts, reload,
  openTable, openQuery, openDiagram, section, setSection}`.
  - `TableView({ name, mode })`: a table's rows (`mode` `"data"`) or
    structure (`"structure"`).
  - `QueryView({ initial, title, onTitle })`: the SQL editor, loaded with
    the first one shown.
  - `Diagram({ onOpen })`: the schema diagram; `onOpen(table)` on opening a
    table from it.
- `Grid`: the virtualized grid on its own, over props (`columns`, `count`,
  `rowAt(i)`, `need(from, to)`, `types`, `sort`, `onSort`, `onPick`,
  `onEdit`, ...; see `src/components/Grid.jsx`).

### The cloud pages

Each takes `{ org, onOpenDatabase, onNavigate }` (`org` defaults to the org in
view) and renders inside the console's page container:

- `Databases`: list, create (empty, upload, branch, restore to a time),
  tokens, rename, delete and restore, protection, blocking, export.
  `onOpenDatabase({ org, database }, renamedFrom)` opens one; `current`
  (`{org, database}`) is the one the app has open, reopened when renamed.
- `Groups`: groups and locations.
- `Members`: members, roles and invites, and the user's own invites.
- `Usage`: rows, statements, errors and latency, and top queries.
- `OrgSettings`: the org, and deleting it (then `onNavigate("databases")`).
- `PlatformTokens`: the user's API tokens.
- `Users`: the server's users, for a server admin.

`CLOUD_PAGES` lists them as the stock console does:
`[{key, label, icon, component, admin}]`.

### Building blocks

- `Toasts` / `useToast()`: `push(text, isError, action?)`, `action` being
  `{label, run}` (Undo). `ConsoleProvider` holds a `Toasts`.
- `Drawer({ title, subtitle, footer, onClose, wide, children })`: the panel
  that slides in from the right; `Field({ label, help, children })` is its
  form row.
- `MenuButton({ label, items, className, title })`, `MenuList({ items, x, y,
  onClose })` and `useContextMenu()`: menus of `{label, hint, danger,
  disabled, onClick}` and `"-"`.
- `TableDialog` and `ImportDialog`: creating or editing a table, importing
  CSV or JSON, on the open database.
- `Palette({ commands, onClose })`: the ⌘K palette over `{label, group,
  hint, icon, run}`.
- `Spinner({ label })`.
- `@samyfodil/musql-console/style.css`: the style sheet and its tokens
  (`--bg`, `--panel`, `--side`, `--line`, `--text`, `--dim`, `--accent`,
  ...), light and dark.

A shell that imports none of `createConsole` leaves the local studio (the
sample, opening files, remote connections) out of its build.

