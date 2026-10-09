// Package fsio is the positioned read, positioned write and fsync the commit
// path issues on every commit. Natively they are the os.File methods. On
// js/wasm, where the os package sends each call through syscall/js -- a
// round trip that builds a JS value, with a finalizer, per argument and per
// result -- they are direct imports the JS glue answers (examples/wasm/musql.js,
// as it answers fsstamp's).
package fsio
