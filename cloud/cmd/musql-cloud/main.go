// musql-cloud is the self-hosted musql cloud server: the management API, the
// OIDC provider, Hrana for every hosted database and the console, on one
// listener.
//
//	musql-cloud -config config.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/samyfodil/musql/cloud/server"
)

func main() {
	path := flag.String("config", "./config.yaml", "path to config.yaml")
	flag.Parse()
	b, err := os.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := server.ParseConfig(b)
	if err != nil {
		log.Fatal(err)
	}
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
	logger.Printf("serving %s on %s", cfg.PublicURL, cfg.Listen)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Print(err)
	}
	srv.Close()
}
