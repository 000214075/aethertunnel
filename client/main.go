// Command aethertunnel-client connects to an AetherTunnel server, publishes the
// tunnels listed in its configuration and forwards each visiting connection to the
// matching local service.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/clientlib"
	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/discovery"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		configPath  = flag.String("config", "", "path to the client configuration file (default client.toml)")
		checkConfig = flag.Bool("check", false, "validate the configuration and exit")
		strictKeys  = flag.Bool("reject-unknown-keys", false, "fail instead of warning when the configuration holds a key this version does not understand")
		showID      = flag.Bool("identity", false, "print this client's public identity key and exit")
		discover    = flag.String("discover", "", "resolve a proxy name through the [dht] network and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] [config-file]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("aethertunnel-client %s (protocol %d, built %s, commit %s)\n",
			clientlib.Version, protocol.ProtocolVersion, clientlib.BuildTime, clientlib.GitCommit)
		return
	}

	path := *configPath
	if path == "" {
		if flag.NArg() > 0 {
			path = flag.Arg(0)
		} else {
			path = "client.toml"
		}
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load(path, config.ValidateOptions{Role: config.RoleClient, RejectUnknownKeys: *strictKeys})
	// Reported even when the configuration is rejected, for the same reason the server
	// does it: the warnings and the validation failures are usually one mistake, and
	// seeing them together saves a second round trip.
	if cfg != nil {
		for _, warning := range cfg.Warnings {
			logger.Printf("warning: %s", warning)
		}
	}
	if err != nil {
		logger.Fatalf("%v", err)
	}

	if *checkConfig {
		fmt.Printf("%s is valid\n", path)
		return
	}

	if *showID {
		identity, err := crypto.LoadIdentity(cfg.Identity.KeyFile)
		if err != nil {
			logger.Fatalf("identity: %v", err)
		}
		fmt.Printf("%s\n", identity.PublicKeyHex())
		return
	}

	if *discover != "" {
		if !cfg.DHT.Enabled {
			logger.Fatalf("dht: -discover needs a [dht] section with enabled = true")
		}
		resolver, err := discovery.Start(cfg.DHTSettings(logger))
		if err != nil {
			logger.Fatalf("dht: %v", err)
		}
		defer func() { _ = resolver.Close() }()

		for _, failure := range resolver.Bootstrap(resolver.Context()) {
			logger.Printf("dht: bootstrap %v", failure)
		}
		record, err := resolver.Resolve(*discover)
		if err != nil {
			logger.Fatalf("dht lookup of %q failed: %v", *discover, err)
		}
		if record.Verified {
			fmt.Printf("%s -> %s (type %s, announced %s, signed by %s)\n",
				record.Name, record.Server, record.Type,
				record.Updated.UTC().Format(time.RFC3339), record.PublicKey)
			return
		}
		fmt.Printf("%s -> %s (type %s, announced %s, unsigned)\n",
			record.Name, record.Server, record.Type, record.Updated.UTC().Format(time.RFC3339))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := clientlib.Run(ctx, cfg, logger); err != nil {
		logger.Fatalf("%v", err)
	}
}
