// The page's side of embedWorker.js: embed(texts, model, kind) resolves to one
// Float32Array per text. The worker starts on the first call, and each model
// downloads on its first use.
let worker;
const pending = new Map();
let nextId = 0;
let onProgress = () => {};

/**
 * The models the studio embeds with. A column remembers which one filled it
 * (vectors.js), so a search always embeds its text with the same model.
 * prefix: what the model expects before a search's text and a stored text.
 */
export const MODELS = {
	minilm: { id: "Xenova/all-MiniLM-L6-v2", dims: 384, pooling: "mean", name: "all-MiniLM-L6-v2", about: "English, small and fast · ~23 MB" },
	bge: {
		id: "Xenova/bge-small-en-v1.5",
		dims: 384,
		pooling: "cls",
		name: "bge-small-en-v1.5",
		about: "English, better ranking · ~34 MB",
		prefix: { query: "Represent this sentence for searching relevant passages: ", passage: "" },
	},
	e5: {
		id: "Xenova/multilingual-e5-small",
		dims: 384,
		pooling: "mean",
		name: "multilingual-e5-small",
		about: "about 100 languages · ~120 MB",
		prefix: { query: "query: ", passage: "passage: " },
	},
};
export const DEFAULT_MODEL = "minilm";

/** Sets what model-download progress is reported to: fn({model, loaded, total}). */
export function setEmbedProgress(fn) {
	onProgress = fn;
}

function start() {
	worker = new Worker(new URL("./embedWorker.js", import.meta.url), { type: "module" });
	worker.addEventListener("message", ({ data }) => {
		if (data.progress) return onProgress(data.progress);
		const p = pending.get(data.id);
		if (!p) return;
		pending.delete(data.id);
		if (data.error !== undefined) return p.reject(new Error(data.error));
		const rows = [];
		for (let i = 0; i < data.data.length; i += data.dims) rows.push(data.data.subarray(i, i + data.dims));
		p.resolve(rows);
	});
}

/**
 * Embeds texts with a model of MODELS: one Float32Array each. kind is "query"
 * for a search's text and "passage" for text being stored, which some models
 * mark differently.
 */
export function embed(texts, model = DEFAULT_MODEL, kind = "passage") {
	const m = MODELS[model];
	if (!m) return Promise.reject(new Error(`unknown embedding model "${model}"; one of ${Object.keys(MODELS).join(", ")}`));
	if (!worker) start();
	const pre = m.prefix?.[kind] ?? "";
	const id = ++nextId;
	return new Promise((resolve, reject) => {
		pending.set(id, { resolve, reject });
		worker.postMessage({ id, model: m.id, pooling: m.pooling, texts: texts.map((t) => pre + t) });
	});
}

/** A vector as the blob a F32_BLOB column stores: its little-endian float32s. */
export const vectorBlob = (v) => new Uint8Array(v.buffer.slice(v.byteOffset, v.byteOffset + v.byteLength));
