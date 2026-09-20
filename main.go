// Command aethertunnel-server runs the AetherTunnel server: it accepts client
// control sessions, publishes the tunnels they register and serves the dashboard.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/server"
)

// These are stamped at build time with
//
//	-ldflags "-X main.version=v3.1.0 -X main.buildTime=... -X main.gitCommit=..."
//
// and fall back to the values below when the binary is built with plain
// "go build".
var (
	version   = "dev"
	buildTime = "unknown"
	gitCommit = "unknown"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		configPath  = flag.String("config", "", "path to the server configuration file (default server.toml)")
		checkConfig = flag.Bool("check", false, "validate the configuration and exit")
		verifyPath  = flag.String("verify-ledger", "", "verify a bandwidth ledger file against a public key and exit")
		verifyKey   = flag.String("ledger-key", "", "verification key for -verify-ledger: a hex Ed25519 public key or a signing key file")
		dhtLookup   = flag.String("dht-lookup", "", "resolve a proxy name through the [dht] network and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] [config-file]\n\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "\nThe positional config-file form is kept for compatibility with older releases.\n")
		fmt.Fprintf(flag.CommandLine.Output(), "\nExamples:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  %s -config server.toml\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "  %s -verify-ledger aethertunnel-ledger.jsonl -ledger-key 3b1f...\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "  %s -dht-lookup ssh -config server.toml\n", os.Args[0])
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("aethertunnel-server %s (protocol %d, built %s, commit %s)\n",
			version, protocol.ProtocolVersion, buildTime, gitCommit)
		return
	}

	if *verifyPath != "" {
		if err := verifyLedger(*verifyPath, *verifyKey); err != nil {
			fmt.Fprintf(os.Stderr, "ledger verification failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	path := *configPath
	if path == "" {
		if flag.NArg() > 0 {
			path = flag.Arg(0)
		} else {
			path = "server.toml"
		}
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	// A DHT lookup only needs the [dht] section, so it does not require the rest of
	// a server configuration to be complete.
	role := config.RoleServer
	if *dhtLookup != "" {
		role = ""
	}
	cfg, err := config.Load(path, config.ValidateOptions{Role: role})
	if err != nil {
		logger.Fatalf("%v", err)
	}

	for _, warning := range cfg.Warnings {
		logger.Printf("warning: %s", warning)
	}

	if *checkConfig {
		fmt.Printf("%s is valid\n", path)
		return
	}

	if *dhtLookup != "" {
		record, err := server.LookupProxy(cfg, logger, *dhtLookup)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dht lookup of %q failed: %v\n", *dhtLookup, err)
			os.Exit(1)
		}
		fmt.Printf("%s -> %s (type %s", record.Name, record.Server, record.Type)
		if len(record.Domains) > 0 {
			fmt.Printf(", domains %s", strings.Join(record.Domains, ","))
		}
		fmt.Printf(", announced %s)\n", record.Updated.UTC().Format(time.RFC3339))
		return
	}

	srv, err := server.New(cfg, server.Options{
		Version:   version,
		BuildTime: buildTime,
		GitCommit: gitCommit,
		Logger:    logger,
	})
	if err != nil {
		logger.Fatalf("cannot start server: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var dashboard *server.Dashboard
	if cfg.Dashboard.Enabled {
		dashboard, err = server.NewDashboard(srv, logger)
		if err != nil {
			logger.Fatalf("cannot prepare dashboard: %v", err)
		}
		if err := dashboard.Start(); err != nil {
			// A busy dashboard port should not stop the tunnel service.
			logger.Printf("dashboard disabled: %v", err)
			dashboard = nil
		}
	}

	if err := srv.Run(ctx); err != nil {
		logger.Fatalf("server stopped: %v", err)
	}
	if dashboard != nil {
		dashboard.Stop()
	}
	logger.Printf("AetherTunnel server %s stopped", version)
}

// verifyLedger checks a ledger file end to end and prints the per-client totals.
//
// It needs only the public key, so it can be run by anyone the operator gives the
// public key and the file to, on a machine that has neither the signing key nor a
// running server.
func verifyLedger(path, keyText string) error {
	entries, totals, head, err := server.VerifyLedgerFile(path, keyText)
	if err != nil {
		return err
	}

	fmt.Printf("%s: %d entries verified\n", path, entries)
	if head == "" {
		fmt.Printf("chain head: (empty)\n")
	} else {
		fmt.Printf("chain head: %s\n", head)
	}

	if len(totals) == 0 {
		fmt.Printf("no usage recorded\n")
		return nil
	}

	ids := make([]string, 0, len(totals))
	for id := range totals {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	fmt.Printf("%-18s %14s %14s %8s\n", "CLIENT", "BYTES_IN", "BYTES_OUT", "ENTRIES")
	for _, id := range ids {
		sum := totals[id]
		fmt.Printf("%-18s %14d %14d %8d\n", id, sum.BytesIn, sum.BytesOut, sum.Entries)
	}
	return nil
}
