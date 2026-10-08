#!/usr/bin/env bash
# Builds the browser examples' assets: musql.wasm, Go's wasm_exec.js, and
# Turso's browser build in vendor/ (fetched with npm, never committed).
# Then: go run ./examples/wasm/serve
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here/../.."
GOOS=js GOARCH=wasm go build -o "$here/musql.wasm" ./examples/wasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$here/"
turso=0.8.2
if [ ! -f "$here/vendor/turso-$turso.js" ]; then
	tmp="$(mktemp -d)"
	(cd "$tmp" && npm pack --silent "@tursodatabase/database-wasm@$turso" >/dev/null && tar xzf ./*.tgz)
	mkdir -p "$here/vendor"
	cp "$tmp/package/bundle/main.es.js" "$here/vendor/turso-$turso.js"
	rm -rf "$tmp"
fi
echo "ready: go run ./examples/wasm/serve"
