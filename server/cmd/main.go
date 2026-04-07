// Command mpfpv2-server is the mpfpv2 relay server entry point.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	log "github.com/sirupsen/logrus"

	"github.com/cloud/mpfpv2/config"
	"github.com/cloud/mpfpv2/server"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "mpfpv.yml", "path to config file")
	verbose := flag.Bool("v", false, "enable debug logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("mpfpv2-server", version)
		os.Exit(0)
	}

	if *verbose {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Mode != "server" {
		log.Fatalf("config: mode must be 'server', got %q", cfg.Mode)
	}

	s, err := server.New(cfg)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	s.SetConfigPath(*configPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Infof("received signal %s, shutting down", sig)
		cancel()
	}()

	// Start Web UI if configured (non-blocking; failure is non-fatal).
	if cfg.Server.WebUI != "" {
		go func() {
			if err := server.StartWebUI(ctx, cfg.Server.WebUI, s, cfg.TeamKey, cfg.Server.ListenAddr); err != nil {
				log.Warnf("web UI: %v", err)
			}
		}()
	}

	if err := s.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("server: %v", err)
	}
}
