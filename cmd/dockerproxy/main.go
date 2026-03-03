package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dockerproxy/internal/app"
)

func main() {
	cfg := app.LoadConfigFromEnv()
	server, err := app.NewServer(cfg)
	if err != nil {
		log.Fatalf("create server failed: %v", err)
	}

	go func() {
		log.Printf("docker proxy listening on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil {
			log.Fatalf("server stopped: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}
