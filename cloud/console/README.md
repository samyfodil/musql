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

The console is also a package, `@samyfodil/musql-console`, so another app can
be this console plus its own pages, name and colors without a fork.
`src/main.jsx` is the stock app: `createConsole(root, {})`. `example/` is an
app that adds a page, a database tab, a banner, a theme and a name
(`npm run example`, or `npm run build:example` for `example/dist/`; its cloud
parts show when a musql cloud server serves it, with `console.dir` set to
`example/dist`).

The package's entry is its source (`src/index.js`), built by the app's own
Vite. Vite's library mode would inline the wasm module and the embedding
model's runtime into the JavaScript as base64; built by the app, they stay
files next to it.

### Depending on it

```sh
git clone https://github.com/samyfodil/musql
sh musql/npm/build.sh
(cd musql/cloud/console && npm ci)
```

```json
"dependencies": { "@samyfodil/musql-console": "file:../musql/cloud/console", "react": "^19.0.0", "react-dom": "^19.0.0" }
```

Or straight from git, without a clone: the repository's root `package.json`
is this package, and its `prepare` script builds the wasm module, so the
installing machine needs Go. Pin a commit:

```json
"dependencies": { "@samyfodil/musql-console": "github:samyfodil/musql#<commit>", "react": "^19.0.0", "react-dom": "^19.0.0" }
```

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
