// What the studio knows about a table's embeddings, and everything it does
// with them: set a column up, fill the rows still missing a vector, and search.
//
// The studio records each embedded column in a table of its own,
// musql_studio_embeddings, which travels with the database:
//
//	tbl, vec   the table and its vector column, F32_BLOB(dims)
//	src        the text column the vectors are made from
//	model      a key of MODELS: a search embeds its text with the same model
//	idx        a libsql_vector_idx index on vec, or NULL
//	fts        an fts5 table over src, for keyword + meaning search, or NULL
//
// A trigger sets vec to NULL when src changes, so a stale vector never ranks a
// row: the row waits, NULL, for fillMissing. A row inserted without a vector
// waits the same way.
import { ident, query, rows, run } from "./db.js";
import { DEFAULT_MODEL, MODELS, embed, vectorBlob } from "./embed.js";

export const META = "musql_studio_embeddings";
const PAGE = 100;

const VECTOR_TYPE = /^(F32_BLOB|FLOAT32)\s*\(\s*(\d+)\s*\)$/i;

/** The dimensions of a vector column's declared type, or 0. */
export const vectorDims = (type) => Number(VECTOR_TYPE.exec(type ?? "")?.[2] ?? 0);

/** Whether a table name is one of the studio's own. */
export const isStudioTable = (name) => name === META;

/**
 * The embeddings of a table: one {vec, src, model, idx, fts, dims} per vector
 * column. A vector column the studio did not set up is listed too, with the
 * default model when its width is that model's and src guessed from a
 * "<src>_emb" name: the columns earlier studios made look exactly like that.
 */
export async function embeddingsOf(tbl, cols) {
	let meta = [];
	try {
		meta = await rows(`SELECT vec, src, model, idx, fts FROM ${META} WHERE tbl = ?`, [tbl]);
	} catch {
		// no embeddings recorded in this database yet
	}
	const out = [];
	for (const c of cols) {
		const dims = vectorDims(c.type);
		if (!dims) continue;
		const m = meta.find((e) => e.vec === c.name);
		if (m) {
			out.push({ ...m, dims });
			continue;
		}
		const src = c.name.endsWith("_emb") ? c.name.slice(0, -4) : null;
		const model = MODELS[DEFAULT_MODEL].dims === dims ? DEFAULT_MODEL : null;
		out.push({ vec: c.name, src: cols.some((x) => x.name === src) ? src : null, model, idx: null, fts: null, dims });
	}
	return out;
}

/**
 * Sets up <src>_emb on tbl for model: the column, its record, the trigger that
 * marks a changed row's vector stale, and the indexes asked for. Returns the
 * embedding; fillMissing fills it.
 */
export async function setUpEmbedding(tbl, src, model, { index, keywords }) {
	const m = MODELS[model];
	const vec = `${src}_emb`;
	const cols = await rows(`PRAGMA table_info(${ident(tbl)})`);
	await run("BEGIN");
	try {
		await run(`CREATE TABLE IF NOT EXISTS ${META}(tbl TEXT NOT NULL, vec TEXT NOT NULL, src TEXT, model TEXT, idx TEXT, fts TEXT, PRIMARY KEY(tbl, vec))`);
		const old = (await rows(`SELECT model FROM ${META} WHERE tbl = ? AND vec = ?`, [tbl, vec]))[0];
		if (!cols.some((c) => c.name === vec)) {
			await run(`ALTER TABLE ${ident(tbl)} ADD COLUMN ${ident(vec)} F32_BLOB(${m.dims})`);
		} else if (old?.model !== model) {
			// Vectors from another model are not comparable with this one's.
			await run(`UPDATE ${ident(tbl)} SET ${ident(vec)} = NULL`);
		}
		const stale = `musql_studio_${tbl}_${vec}_stale`;
		await run(`DROP TRIGGER IF EXISTS ${ident(stale)}`);
		await run(
			`CREATE TRIGGER ${ident(stale)} AFTER UPDATE OF ${ident(src)} ON ${ident(tbl)} WHEN NEW.${ident(src)} IS NOT OLD.${ident(src)} ` +
				`BEGIN UPDATE ${ident(tbl)} SET ${ident(vec)} = NULL WHERE rowid = NEW.rowid; END`,
		);
		let idx = null;
		if (index) {
			idx = `${tbl}_${vec}_idx`;
			await run(`CREATE INDEX IF NOT EXISTS ${ident(idx)} ON ${ident(tbl)}(libsql_vector_idx(${ident(vec)}, 'metric=cosine'))`);
		}
		const fts = keywords ? await setUpKeywords(tbl, src) : null;
		await run(`INSERT OR REPLACE INTO ${META}(tbl, vec, src, model, idx, fts) VALUES (?, ?, ?, ?, ?, ?)`, [tbl, vec, src, model, idx, fts]);
		await run("COMMIT");
		return { vec, src, model, idx, fts, dims: m.dims };
	} catch (e) {
		await run("ROLLBACK").catch(() => {});
		throw e;
	}
}

// setUpKeywords makes an fts5 table over tbl's src, kept in step with it by
// triggers, and fills it.
async function setUpKeywords(tbl, src) {
	const fts = `${tbl}_${src}_fts`;
	const s = ident(src);
	await run(`CREATE VIRTUAL TABLE IF NOT EXISTS ${ident(fts)} USING fts5(${s}, content=${ident(tbl)}, content_rowid='rowid')`);
	const f = ident(fts);
	const trig = (what, body) =>
		run(`CREATE TRIGGER IF NOT EXISTS ${ident(`musql_studio_${fts}_${what}`)} AFTER ${what.toUpperCase()}${what === "update" ? ` OF ${s}` : ""} ON ${ident(tbl)} BEGIN ${body} END`);
	await trig("insert", `INSERT INTO ${f}(rowid, ${s}) VALUES (NEW.rowid, NEW.${s});`);
	await trig("delete", `INSERT INTO ${f}(${f}, rowid, ${s}) VALUES ('delete', OLD.rowid, OLD.${s});`);
	await trig("update", `INSERT INTO ${f}(${f}, rowid, ${s}) VALUES ('delete', OLD.rowid, OLD.${s}); INSERT INTO ${f}(rowid, ${s}) VALUES (NEW.rowid, NEW.${s});`);
	await run(`INSERT INTO ${f}(${f}) VALUES ('rebuild')`);
	return fts;
}

/** How many rows have text and no vector yet. */
export async function missingCount(tbl, e) {
	if (!e.src) return 0;
	return Number((await query(`SELECT count(*) FROM ${ident(tbl)} WHERE ${ident(e.vec)} IS NULL AND ${ident(e.src)} IS NOT NULL`)).rows[0][0]);
}

/**
 * Embeds every row whose vector is missing, 64 to a batch and a transaction.
 * progress(done, total) after each batch. Returns the number embedded.
 */
export async function fillMissing(tbl, e, progress = () => {}) {
	const total = await missingCount(tbl, e);
	const v = ident(e.vec);
	const s = ident(e.src);
	let done = 0;
	let last = null;
	while (done < total) {
		const batch = await query(
			`SELECT rowid, ${s} FROM ${ident(tbl)} WHERE ${v} IS NULL AND ${s} IS NOT NULL${last === null ? "" : " AND rowid > ?"} ORDER BY rowid LIMIT 64`,
			last === null ? [] : [last],
		);
		if (!batch.rows.length) break;
		const vecs = await embed(batch.rows.map((r) => String(r[1])), e.model, "passage");
		await run("BEGIN");
		try {
			for (let i = 0; i < vecs.length; i++) {
				await run(`UPDATE ${ident(tbl)} SET ${v} = ? WHERE rowid = ?`, [vectorBlob(vecs[i]), batch.rows[i][0]]);
			}
			await run("COMMIT");
		} catch (err) {
			await run("ROLLBACK").catch(() => {});
			throw err;
		}
		last = batch.rows.at(-1)[0];
		done += batch.rows.length;
		progress(done, total);
	}
	return done;
}

/** An fts5 query matching any word of s, or "" when s has none. */
// STOP is the English words too common to rank by: matching "and" or "the"
// would lift nearly every row.
const STOP = new Set("a an and are as at be but by for from has have in into is it its of on or that the their this to was were will with what which who how".split(" "));

function anyWord(s) {
	const words = (s.match(/[\p{L}\p{N}_]+/gu) ?? []).filter((w) => !STOP.has(w.toLowerCase()));
	return words.map((w) => `"${w}"`).join(" OR ");
}

/**
 * The rows of tbl closest in meaning to text: {columns, rows, how}. With
 * keywords and an fts table, ranks by meaning and by the words together
 * (reciprocal rank fusion of the two top 50s); otherwise by cosine distance,
 * through the vector index when there is one.
 */
export async function search(tbl, e, text, keywords) {
	const [q] = await embed([text], e.model, "query");
	const blob = vectorBlob(q);
	const t = ident(tbl);
	const v = ident(e.vec);
	const words = keywords && e.fts ? anyWord(text) : "";
	if (words) {
		const f = ident(e.fts);
		const m = await query(`SELECT rowid FROM ${t} WHERE ${v} IS NOT NULL ORDER BY vector_distance_cos(${v}, ?) LIMIT 50`, [blob]);
		const k = await query(`SELECT rowid FROM ${f} WHERE ${f} MATCH ? ORDER BY rank LIMIT 50`, [words]);
		// Reciprocal rank fusion: each list adds 1/(60 + rank) to a row's score.
		const score = new Map();
		for (const list of [m.rows, k.rows]) list.forEach(([id], i) => score.set(id, (score.get(id) ?? 0) + 1 / (61 + i)));
		const ids = [...score.keys()].sort((a, b) => score.get(b) - score.get(a)).slice(0, PAGE);
		if (!ids.length) return { columns: [], rows: [], how: "meaning and keywords" };
		const r = await query(`SELECT rowid AS __rid, * FROM ${t} WHERE rowid IN (${ids.map(() => "?").join(", ")})`, ids);
		const byId = new Map(r.rows.map((row) => [row[0], row.slice(1)]));
		return {
			columns: [...r.columns.slice(1), "score"],
			types: [...(r.types ?? []).slice(1), ""],
			rows: ids.filter((id) => byId.has(id)).map((id) => [...byId.get(id), Number(score.get(id).toFixed(5))]),
			how: "meaning and keywords",
		};
	}
	if (e.idx) {
		const r = await query(
			`SELECT ${t}.*, vector_distance_cos(${t}.${v}, ?) AS distance FROM vector_top_k(?, ?, ${PAGE}) AS k JOIN ${t} ON ${t}.rowid = k.id ORDER BY distance`,
			[blob, e.idx, blob],
		);
		return { ...r, how: `meaning, through ${e.idx}` };
	}
	const r = await query(`SELECT *, vector_distance_cos(${v}, ?) AS distance FROM ${t} WHERE ${v} IS NOT NULL ORDER BY distance LIMIT ${PAGE}`, [blob]);
	return { ...r, how: "meaning" };
}

/** The rows of a vec1 table nearest to text, embedded with model. */
export async function searchVec1(tbl, model, text) {
	const [q] = await embed([text], model, "query");
	const r = await query(`SELECT rowid, distance, * FROM ${ident(tbl)}(?, '{k:${PAGE}}')`, [vectorBlob(q)]);
	return { ...r, how: "meaning, through vec1" };
}
