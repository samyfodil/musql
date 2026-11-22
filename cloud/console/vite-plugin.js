// The Vite settings an app built on the console needs: one copy of React for
// the console and the app's own components, the npm package's wasm found next
// to it, and module workers.
import { fileURLToPath } from "node:url";
import { searchForWorkspaceRoot } from "vite";

export default function musqlConsole() {
	return {
		name: "musql-console",
		config: () => ({
			optimizeDeps: { exclude: ["@samyfodil/musql", "@samyfodil/musql-console"] },
			resolve: { dedupe: ["react", "react-dom"] },
			worker: { format: "es" },
			server: {
				fs: {
					allow: [searchForWorkspaceRoot(process.cwd()), fileURLToPath(new URL(".", import.meta.url)), fileURLToPath(new URL("../../npm", import.meta.url))],
				},
			},
		}),
	};
}
