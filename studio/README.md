# musql studio

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
- Export as a C SQLite file (`.sqlite`) or a musql file (`.musq`).

## Run it

```sh
sh ../npm/build.sh     # the wasm module the package ships
npm install
npm run dev            # or: npm run build, then serve dist/
```

React and CodeMirror, built with Vite.
