// How a value looks in a grid: its text and a class for NULL, numbers and blobs.
export function show(v) {
	if (v === null || v === undefined) return { text: "NULL", cls: "null" };
	if (v instanceof Uint8Array) {
		const hex = Array.from(v.subarray(0, 8), (b) => b.toString(16).padStart(2, "0")).join("");
		return { text: `x'${hex}${v.length > 8 ? "…" : ""}' · ${v.length} B`, cls: "blob" };
	}
	if (typeof v === "number" || typeof v === "bigint") return { text: String(v), cls: "num" };
	const s = String(v);
	return { text: s.length > 300 ? s.slice(0, 300) + "…" : s, cls: "" };
}

export const fmt = (n) => Number(n).toLocaleString();
