// An app built on the console the way another repository would build one.
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import musqlConsole from "@samyfodil/musql-console/vite";

export default defineConfig({
	root: fileURLToPath(new URL(".", import.meta.url)),
	base: "./",
	plugins: [react(), musqlConsole()],
	build: { chunkSizeWarningLimit: 1000 },
});
