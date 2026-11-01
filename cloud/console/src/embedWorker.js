// Text embeddings in a worker of their own, so a model download or a large
// batch never stalls the database worker. Models are fetched from the Hugging
// Face hub on first use and cached by the browser.
import { pipeline } from "@huggingface/transformers";

const extractors = new Map();

function load(model) {
	if (!extractors.has(model)) {
		extractors.set(
			model,
			pipeline("feature-extraction", model, {
				dtype: "q8",
				progress_callback: (p) => {
					if (p.status === "progress") postMessage({ progress: { model, file: p.file, loaded: p.loaded, total: p.total } });
				},
			}),
		);
	}
	return extractors.get(model);
}

// embed answers {id, model, pooling, texts} with one Float32Array of
// texts.length * dims components, each row pooled and normalized.
onmessage = async ({ data: { id, model, pooling, texts } }) => {
	try {
		const run = await load(model);
		const out = await run(texts, { pooling, normalize: true });
		const data = out.data instanceof Float32Array ? out.data : Float32Array.from(out.data);
		postMessage({ id, data, dims: out.dims.at(-1) }, [data.buffer]);
	} catch (e) {
		postMessage({ id, error: e.message || String(e) });
	}
};
