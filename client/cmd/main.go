// mpfpv2 client entry point.
//
// Usage:
//
//	mpfpv2 mpfpv://teamKey@host:port     # zero-config via connection string
//	mpfpv2 -config mpfpv.yml             # config file
//	mpfpv2 -v                             # verbose logging
//	mpfpv2 -version                       # print version
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloud/mpfpv2/client"
	"github.com/cloud/mpfpv2/config"
	log "github.com/sirupsen/logrus"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to config file (optional)")
	verbose := flag.Bool("v", false, "enable debug logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("mpfpv2", version)
		os.Exit(0)
	}

	if *verbose {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "15:04:05",
	})

	var cfg *config.Config
	var err error

	// Priority: positional arg > -config > ./mpfpv.yml
	args := flag.Args()
	if len(args) > 0 {
		cfg, err = config.FromConnString(args[0])
		if err != nil {
			log.Fatalf("invalid connection string: %v", err)
		}
		log.Infof("using connection string: %s", args[0])
	} else {
		path := *configPath
		if path == "" {
			// Try current directory default.
			if _, statErr := os.Stat("mpfpv.yml"); statErr == nil {
				path = "mpfpv.yml"
			}
		}
		if path == "" {
			fmt.Fprintln(os.Stderr, "usage: mpfpv2 [flags] mpfpv://teamKey@host:port")
			fmt.Fprintln(os.Stderr, "   or: mpfpv2 -config mpfpv.yml")
			os.Exit(1)
		}
		cfg, err = config.LoadFile(path)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		log.Infof("loaded config from %s", path)
	}

	// Force client mode.
	cfg.Mode = "client"

	c, err := client.New(cfg)
	if err != nil {
		log.Fatalf("create client: %v", err)
	}

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

	if err := c.Run(ctx); err != nil {
		log.Fatalf("client: %v", err)
	}
}
