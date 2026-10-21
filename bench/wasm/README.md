# musql vs Turso in the browser

The npm package (`../../npm`) against `@tursodatabase/database-wasm`, each in
its own module worker, on the same data and the same seeded parameters. Every
workload's answers are compared between the two before it is timed.

```sh
sh ../../npm/build.sh
npm install
npm run dev     # then open http://localhost:5173/?rows=100000
```

The page prints a table and leaves the raw numbers in `window.benchResult`.
