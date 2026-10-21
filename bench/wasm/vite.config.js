import { defineConfig } from "vite";

// Turso's wasm build shares memory with its worker, which needs a
// cross-origin isolated page.
const isolated = {
	"Cross-Origin-Opener-Policy": "same-origin",
	"Cross-Origin-Embedder-Policy": "require-corp",
};

export default defineConfig({
	optimizeDeps: { exclude: ["@samyfodil/musql", "@tursodatabase/database-wasm"] },
	worker: { format: "es" },
	// The package is a link into ../../npm.
	server: { headers: isolated, fs: { allow: ["../.."] } },
	preview: { headers: isolated },
});
