#!/bin/sh
# Builds the package's files from the repository: the wasm module, Go's own
# wasm_exec.js (it must match the Go that built the module), and the glue and
# in-memory filesystem the browser example uses, copied rather than forked.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
cd "$root"
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o "$here/musql.wasm" ./npm/wasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$here/wasm_exec.js"
cp examples/wasm/memfs.js "$here/memfs.js"
cp examples/wasm/musql.js "$here/glue.js"
echo "built $here/musql.wasm"
