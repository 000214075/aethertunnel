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
	"syscall"

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
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] [config-file]\n\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "\nThe positional config-file form is kept for compatibility with older releases.\n")
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("aethertunnel-server %s (protocol %d, built %s, commit %s)\n",
			version, protocol.ProtocolVersion, buildTime, gitCommit)
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

	cfg, err := config.Load(path, config.ValidateOptions{Role: config.RoleServer})
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
