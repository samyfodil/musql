// The page's side of embedWorker.js: embed(texts) resolves to one Float32Array
// per text. The worker, and the model with it, start on the first call.
let worker;
const pending = new Map();
let nextId = 0;
let onProgress = () => {};

/** Sets what model-download progress is reported to. */
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

/** Embeds texts with all-MiniLM-L6-v2: one 384-component Float32Array each. */
export function embed(texts) {
	if (!worker) start();
	const id = ++nextId;
	return new Promise((resolve, reject) => {
		pending.set(id, { resolve, reject });
		worker.postMessage({ id, texts });
	});
}

/** A vector as the blob a F32_BLOB column stores: its little-endian float32s. */
export const vectorBlob = (v) => new Uint8Array(v.buffer.slice(v.byteOffset, v.byteOffset + v.byteLength));

export const EMBED_DIMS = 384;
