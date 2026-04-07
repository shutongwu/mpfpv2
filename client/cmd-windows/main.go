//go:build windows

// mpfpv2 Windows client — simple single-NIC mode.
// Double-click to run. Reads mpfpv.yml from same directory.
// Shows virtual IP prominently in console.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cloud/mpfpv2/client"
	"github.com/cloud/mpfpv2/config"
	log "github.com/sirupsen/logrus"

	"golang.org/x/sys/windows"
)

var version = "dev"

func main() {
	// Keep console window open on crash.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "\nFATAL: %v\n", r)
			fmt.Println("\nPress Enter to exit...")
			fmt.Scanln()
		}
	}()

	// Auto-elevate to admin if needed (wintun requires it).
	if !isAdmin() {
		relaunchAsAdmin()
		return
	}

	showVersion := flag.Bool("version", false, "show version")
	configPath := flag.String("config", "", "config file path")
	verbose := flag.Bool("v", false, "debug logging")
	flag.Parse()

	if *showVersion {
		fmt.Println("mpfpv2-windows", version)
		return
	}
	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	fmt.Println("========================================")
	fmt.Println("  mpfpv2 Windows Client", version)
	fmt.Println("========================================")
	fmt.Println()

	var cfg *config.Config
	var err error

	// Priority: args > -config > exe-dir/mpfpv.yml
	args := flag.Args()
	if len(args) > 0 {
		cfg, err = config.FromConnString(args[0])
		if err != nil {
			fatal("Invalid connection string: %v", err)
		}
		fmt.Printf("  Server: %s\n", cfg.Client.ServerAddr)
	} else {
		cfgFile := *configPath
		if cfgFile == "" {
			exePath, _ := os.Executable()
			cfgFile = filepath.Join(filepath.Dir(exePath), "mpfpv.yml")
		}
		cfg, err = config.LoadFile(cfgFile)
		if err != nil {
			fatal("Cannot load config %s: %v\n\nUsage: mpfpv2.exe mpfpv://teamkey@server:port\n  or place mpfpv.yml next to exe", cfgFile, err)
		}
		fmt.Printf("  Config: %s\n", cfgFile)
		fmt.Printf("  Server: %s\n", cfg.Client.ServerAddr)
	}

	cfg.Mode = "client"
	// Windows: single socket mode, no multipath. OS routing picks the right NIC.
	cfg.Client.BindInterface = "auto"
	fmt.Println("  Connecting...")
	fmt.Println()

	c, err := client.New(cfg)
	if err != nil {
		fatal("Client init failed: %v", err)
	}

	// Hook into registration to print the virtual IP prominently.
	go func() {
		c.WaitRegistered()
		vip := c.VirtualIP()
		fmt.Println("========================================")
		fmt.Printf("  Virtual IP: %s\n", vip)
		fmt.Println("  Status: CONNECTED")
		fmt.Println("========================================")
		fmt.Println()
		fmt.Println("Keep this window open. Press Ctrl+C to disconnect.")
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := c.Run(ctx); err != nil {
		fatal("Client error: %v", err)
	}
	fmt.Println("\nDisconnected.")
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "\nERROR: "+format+"\n", args...)
	fmt.Println("\nPress Enter to exit...")
	fmt.Scanln()
	os.Exit(1)
}

func isAdmin() bool {
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)
	member, err := windows.Token(0).IsMember(sid)
	return err == nil && member
}

func relaunchAsAdmin() {
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	verb, _ := syscall.UTF16PtrFromString("runas")
	exeW, _ := syscall.UTF16PtrFromString(exe)
	args := ""
	for _, a := range os.Args[1:] {
		args += " " + a
	}
	argsW, _ := syscall.UTF16PtrFromString(args)
	cwdW, _ := syscall.UTF16PtrFromString(cwd)
	windows.ShellExecute(0, verb, exeW, argsW, cwdW, windows.SW_SHOWNORMAL)
}
