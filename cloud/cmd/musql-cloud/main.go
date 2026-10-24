// musql-cloud is the self-hosted musql cloud server: the management API, the
// OIDC provider, Hrana for every hosted database and the console, on one
// listener.
//
//	musql-cloud init --admin-email a@b.c [--admin-password ...] [-config config.yaml]
//	musql-cloud -config config.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/samyfodil/musql/engine"

	"github.com/samyfodil/musql/cloud/cluster"
	"github.com/samyfodil/musql/cloud/server"
	"github.com/samyfodil/musql/cloud/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if err := runInit(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	path := flag.String("config", "./config.yaml", "path to config.yaml")
	peerID := flag.Bool("peer-id", false, "print this node's libp2p peer ID (making its key in data_dir if needed) and exit")
	flag.Parse()
	b, err := os.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := server.ParseConfig(b)
	if err != nil {
		log.Fatal(err)
	}
	if *peerID {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			log.Fatal(err)
		}
		key, err := cluster.LoadKey(filepath.Join(cfg.DataDir, "node.key"))
		if err != nil {
			log.Fatal(err)
		}
		id, err := cluster.PeerID(key)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(id)
		return
	}
	// Every commit to a hosted database is logged for its history, and hosted
	// databases may use FTS5, which the engine leaves opt-in.
	engine.Configure(engine.WithCommitHook(store.OnCommit))
	engine.RegisterFTS5()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := log.New(os.Stderr, "musql-cloud: ", log.LstdFlags)
	srv, err := server.New(ctx, cfg, logger)
	if err != nil {
		logger.Fatal(err)
	}
	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shut)
	}()
	if id := srv.PeerID(); id != "" {
		logger.Printf("peer %s on %v", id, srv.P2PAddrs())
	}
	logger.Printf("serving %s on %s", cfg.PublicURL, cfg.Listen)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Print(err)
	}
	srv.Close()
}
