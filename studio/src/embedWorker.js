// Text embeddings for semantic search, in a worker of their own so a model
// download or a large batch never stalls the database worker. The model is
// all-MiniLM-L6-v2 (384 dimensions, quantized): fetched from the Hugging Face
// hub on first use and cached by the browser after that.
import { pipeline } from "@huggingface/transformers";

const MODEL = "Xenova/all-MiniLM-L6-v2";
let extractor;

function load() {
	extractor ??= pipeline("feature-extraction", MODEL, {
		dtype: "q8",
		progress_callback: (p) => {
			if (p.status === "progress") postMessage({ progress: { file: p.file, loaded: p.loaded, total: p.total } });
		},
	});
	return extractor;
}

// embed answers {id, texts} with one Float32Array of texts.length * 384
// components, each row mean-pooled and normalized.
onmessage = async ({ data: { id, texts } }) => {
	try {
		const run = await load();
		const out = await run(texts, { pooling: "mean", normalize: true });
		const data = out.data instanceof Float32Array ? out.data : Float32Array.from(out.data);
		postMessage({ id, data, dims: out.dims.at(-1) }, [data.buffer]);
	} catch (e) {
		postMessage({ id, error: e.message || String(e) });
	}
};
