import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
	base: "./",
	plugins: [react()],
	// The package finds musql.wasm next to itself (new URL(..., import.meta.url)),
	// which pre-bundling would break.
	optimizeDeps: { exclude: ["@samyfodil/musql"] },
	worker: { format: "es" },
	// React and CodeMirror; the wasm module is its own asset.
	build: { chunkSizeWarningLimit: 1000 },
});
