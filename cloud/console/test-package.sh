#!/bin/sh
# Installs the two packages as an app gets them, from tarballs packed here,
# into an app outside the repository, and builds it with Vite: the console's
# published package.json must resolve, onnxruntime-node must stay out of the
# install, and the build must emit the wasm module as a file.
#
# Run after sh npm/build.sh and npm ci here.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
version=0.0.0-test

cp -r "$root/npm" "$work/musql"
(cd "$work/musql" && npm version "$version" --no-git-tag-version >/dev/null && npm pack --silent --pack-destination "$work" >/dev/null)
node "$here/pack.mjs" "$version" "$work/console"
(cd "$work/console" && npm pack --silent --pack-destination "$work" >/dev/null)

app="$work/app"
mkdir -p "$app/src"
cd "$app"
cat >package.json <<'EOF'
{ "name": "console-app", "private": true, "type": "module" }
EOF
cat >index.html <<'EOF'
<!doctype html>
<html><body><div id="root"></div><script type="module" src="/src/main.jsx"></script></body></html>
EOF
cat >src/main.jsx <<'EOF'
import { createConsole } from "@samyfodil/musql-console";
createConsole(document.getElementById("root"), {});
EOF
cat >vite.config.js <<'EOF'
import react from "@vitejs/plugin-react";
import musqlConsole from "@samyfodil/musql-console/vite";
export default { base: "./", plugins: [react(), musqlConsole()] };
EOF
npm install --no-audit --no-fund "$work/samyfodil-musql-$version.tgz" "$work/samyfodil-musql-console-$version.tgz" \
	react@^19 react-dom@^19 vite@^6 @vitejs/plugin-react@^4
if [ -e node_modules/onnxruntime-node ]; then
	echo "onnxruntime-node was installed" >&2
	exit 1
fi
npx vite build
ls dist/assets/*.wasm >/dev/null
echo "the packages install and build"
