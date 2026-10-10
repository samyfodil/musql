# @samyfodil/musql

[musql](https://github.com/samyfodil/musql), a SQLite-compatible database
written in Go, compiled to WebAssembly with its query JIT, for Node and
browsers. Databases live in memory; `export()` and `open(name, { data })` move
them in and out as the bytes of one file.

## Install

The package is on GitHub Packages. Point the `@samyfodil` scope at it, with a
GitHub token that can read packages (GitHub requires one even for public
packages):

```sh
echo "@samyfodil:registry=https://npm.pkg.github.com" >> .npmrc
echo "//npm.pkg.github.com/:_authToken=${GITHUB_TOKEN}" >> .npmrc
npm install @samyfodil/musql
```

## Use

```js
import { open } from "@samyfodil/musql";

const db = await open("app.db");
db.exec("CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, data BLOB)");
db.run("INSERT INTO t(name, data) VALUES (?, ?)", "ada", new Uint8Array([1, 2]));
db.all("SELECT * FROM t");      // [{ id: 1, name: "ada", data: Uint8Array [1, 2] }]
db.get("SELECT count(*) AS n FROM t"); // { n: 1 }
db.query("SELECT id, name FROM t");    // { columns: ["id", "name"], rows: [[1, "ada"]] }

const bytes = db.export();      // the whole database, compacted, as one file
const copy = await open("copy.db", { data: bytes });
db.close();
```

- `run(sql, ...params)` returns `{ changes, lastInsertRowid }`; `exec(sql)` runs
  SQL that returns no rows.
- Parameters: `null`, booleans, numbers (integers bind as INTEGER), `BigInt`,
  strings, `Uint8Array`/`ArrayBuffer` (BLOB).
- Results: an INTEGER outside JavaScript's safe range comes back as a `BigInt`,
  a BLOB as a `Uint8Array`.

## The JIT and Web Workers

musql compiles query kernels to WebAssembly while it runs. Browsers allow that
only in a Web Worker, so on a page's main thread the package runs without the
JIT: the same answers, slower. Run it in a worker for full speed; in Node it is
always on. `open(name, { jit: false })` turns it off.

## Size

`musql.wasm` is about 20 MB raw, 5.1 MB gzipped and 3.7 MB with brotli. Serve
it compressed (`Content-Encoding: br` or `gzip`); most hosts and CDNs do this
for `.wasm` once it is enabled.
