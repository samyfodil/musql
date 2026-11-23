// The Vite settings an app built on the console needs: one copy of React for
// the console and the app's own components, the npm package's wasm found next
// to it, and module workers.
import { createRequire } from "node:module";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { searchForWorkspaceRoot } from "vite";

// Where @samyfodil/musql is: in this repository a link to ../../npm, which
// resolves to that directory; installed, a package of its own.
const musqlDir = dirname(createRequire(import.meta.url).resolve("@samyfodil/musql"));

export default function musqlConsole() {
	return {
		name: "musql-console",
		config: () => ({
			optimizeDeps: { exclude: ["@samyfodil/musql", "@samyfodil/musql-console"] },
			resolve: { dedupe: ["react", "react-dom"] },
			worker: { format: "es" },
			server: {
				fs: {
					allow: [searchForWorkspaceRoot(process.cwd()), fileURLToPath(new URL(".", import.meta.url)), musqlDir],
				},
			},
		}),
	};
}
