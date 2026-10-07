// musqld serves musql databases over Hrana, the protocol libSQL and Turso
// clients speak, so they can use musql as a drop-in server.
//
//	musqld -db app.musq -listen :8080 [-auth-token TOKEN]
//	musqld -dir ./dbs   -listen :8080 [-create]
//
// With -db, every request reaches that one database. With -dir, the first label
// of the request's host picks the database, as Turso names them:
// http://app.example.com:8080 serves ./dbs/app.musq.
//
// Clients use http://, ws:// or libsql:// URLs, e.g. createClient({url:
// "http://localhost:8080"}) with @libsql/client.
package main

import (
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/samyfodil/musql/driver"
	"github.com/samyfodil/musql/hrana"
)

func main() {
	dbPath := flag.String("db", "", "serve this one database file (created if missing)")
	dir := flag.String("dir", "", "serve every <name>.musq in this directory, by host name")
	create := flag.Bool("create", false, "with -dir: create a database on first use of a new name")
	listen := flag.String("listen", ":8080", "address to listen on")
	token := flag.String("auth-token", os.Getenv("MUSQLD_AUTH_TOKEN"), "require this bearer token (default $MUSQLD_AUTH_TOKEN; empty accepts anyone)")
	idle := flag.Duration("idle-timeout", 10*time.Second, "close an HTTP stream after this long without a request")
	flag.Parse()
	opts := []hrana.Option{hrana.WithAuthToken(*token), hrana.WithIdleTimeout(*idle)}

	var h http.Handler
	switch {
	case *dbPath != "" && *dir != "":
		log.Fatal("musqld: -db and -dir are exclusive")
	case *dir != "":
		m := hrana.NewMulti(*dir, *create, opts...)
		defer m.Close()
		h = m
		log.Printf("musqld: serving %s/<name>.musq on %s", *dir, *listen)
	default:
		if *dbPath == "" {
			*dbPath = "musql.musq"
		}
		db, err := sql.Open("sqlite", *dbPath)
		if err != nil {
			log.Fatal(err)
		}
		defer db.Close()
		if err := db.Ping(); err != nil {
			log.Fatal(err)
		}
		srv := hrana.NewServer(db, opts...)
		defer srv.Close()
		h = srv
		log.Printf("musqld: serving %s on %s", *dbPath, *listen)
	}
	log.Fatal(http.ListenAndServe(*listen, h))
}
