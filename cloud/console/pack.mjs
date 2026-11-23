// Writes the published @samyfodil/musql-console into a directory of its own:
//
//	node pack.mjs <version> <dir>
//
// The package is this directory's src/ and Vite plugin, at <version>, and
// depends on @samyfodil/musql at the same version rather than on ../../npm.
//
// It ships transformers.js's browser build in place of @huggingface/transformers,
// which depends on onnxruntime-node. That package's install script downloads
// CUDA binaries on a machine with an NVIDIA GPU, and the download 404s and
// fails the install. The console runs embeddings in the browser, where the
// browser build needs only onnxruntime-web, so the package depends on that.
// Run it after npm ci, which installs the build it copies.
import { cpSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const [version, out] = process.argv.slice(2);
if (!version || !out) {
	console.error("usage: node pack.mjs <version> <dir>");
	process.exit(2);
}
const here = fileURLToPath(new URL(".", import.meta.url));
const readJSON = (p) => JSON.parse(readFileSync(p, "utf8"));

rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });
for (const f of ["src", "vite-plugin.js", "README.md"]) {
	cpSync(join(here, f), join(out, f), { recursive: true });
}

const tf = join(here, "node_modules/@huggingface/transformers");
const tfPkg = readJSON(join(tf, "package.json"));
const ortPkg = readJSON(join(here, "node_modules/onnxruntime-web/package.json"));
const vendor = join(out, "src/vendor");
mkdirSync(vendor);
const web = readFileSync(join(tf, "dist/transformers.web.js"), "utf8").replace(/\n\/\/# sourceMappingURL=\S+\s*$/, "\n");
writeFileSync(join(vendor, "transformers.web.js"), `// transformers.js ${tfPkg.version}, its browser build (dist/transformers.web.js).\n${web}`);
cpSync(join(tf, "LICENSE"), join(vendor, "transformers.LICENSE"));

const worker = join(out, "src/embedWorker.js");
const src = readFileSync(worker, "utf8");
const patched = src.replace(`from "@huggingface/transformers"`, `from "./vendor/transformers.web.js"`);
if (patched === src) {
	throw new Error("src/embedWorker.js no longer imports @huggingface/transformers");
}
writeFileSync(worker, patched);

const pkg = readJSON(join(here, "package.json"));
delete pkg.private;
delete pkg.scripts;
delete pkg.devDependencies;
pkg.version = version;
const deps = { ...pkg.dependencies };
delete deps["@huggingface/transformers"];
deps["@samyfodil/musql"] = version;
deps["onnxruntime-web"] = tfPkg.dependencies["onnxruntime-web"];
deps["onnxruntime-common"] = ortPkg.dependencies["onnxruntime-common"];
pkg.dependencies = Object.fromEntries(Object.entries(deps).sort(([a], [b]) => a.localeCompare(b)));
writeFileSync(join(out, "package.json"), JSON.stringify(pkg, null, 2) + "\n");
console.log(`wrote ${pkg.name}@${version} to ${out}`);
