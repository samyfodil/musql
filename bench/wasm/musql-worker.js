import { open } from "@samyfodil/musql";
import { runEngine } from "./engine.js";

const db = await open("bench.musq");
// The package's API is synchronous; the adapter gives it the shape engine.js uses.
const adapter = {
	exec: (sql) => db.exec(sql),
	vacuum: () => db.exec("VACUUM"),
	prepare: (sql) => ({ all: (p) => db.query(sql, ...p).rows, run: (...p) => db.run(sql, ...p) }),
};
postMessage(await runEngine("musql", adapter));
