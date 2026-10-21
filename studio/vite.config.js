import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
	base: "./",
	plugins: [react()],
	// The package finds musql.wasm next to itself (new URL(..., import.meta.url)),
	// which pre-bundling would break.
	optimizeDeps: { exclude: ["@samyfodil/musql"] },
	worker: { format: "es" },
	// The package is a link to ../npm, outside this root, and the dev server
	// serves its musql.wasm from there.
	server: { fs: { allow: [".", "../npm"] } },
	// React and CodeMirror; the wasm module is its own asset.
	build: { chunkSizeWarningLimit: 1000 },
});
