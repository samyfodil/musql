// Command serve serves examples/wasm locally with the headers a
// cross-origin-isolated page needs. Turso's browser build runs wasm threads,
// which need SharedArrayBuffer, which browsers only allow on such a page.
//
//	go run ./examples/wasm/serve            # http://localhost:8080
package main

import (
	"flag"
	"log"
	"net/http"
	"path/filepath"
	"runtime"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()
	_, self, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filepath.Dir(self))
	files := http.FileServer(http.Dir(dir))
	log.Printf("serving %s on http://%s", dir, *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})))
}
