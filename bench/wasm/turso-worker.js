import { connect } from "@tursodatabase/database-wasm";
import { runEngine } from "./engine.js";

const db = await connect(":memory:");
const adapter = {
	exec: (sql) => db.exec(sql),
	prepare: async (sql) => {
		const s = await db.prepare(sql);
		return { all: async (p) => (await s.raw().all(...p)), run: (...p) => s.run(...p) };
	},
};
postMessage(await runEngine("turso", adapter));
