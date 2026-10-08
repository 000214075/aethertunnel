// Command aethertunnel-client connects to an AetherTunnel server, publishes the
// tunnels listed in its configuration and forwards each visiting connection to the
// matching local service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/clientlib"
	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/crypto"
	"github.com/aethertunnel/aethertunnel/pkg/discovery"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

func main() {
	os.Exit(run(os.Args[0], os.Args[1:], os.Stdout, os.Stderr))
}

// notifyShutdown returns a context that the first SIGINT or SIGTERM cancels, and
// drops the signal registration as soon as it is cancelled. The registration has
// to go with that first signal: signal.NotifyContext stops relaying once it has
// cancelled the context but leaves the handler installed, so every later signal
// was delivered to a channel nobody reads and the default action — terminating
// the process — never happened. A shutdown stuck in the drain then had nothing
// left to end it but SIGKILL.
func notifyShutdown() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// run is main's body with its streams passed in, so the command line — the first
// thing anyone meets — can be exercised without starting a process: --version,
// --check, --identity and --discover all return here, and the tunnel itself runs
// through the last branch. It returns the exit status: 0 for what succeeded, 1
// for what was refused (the reason goes to stderr first), 2 for a command line
// that does not parse.
func run(program string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(program, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		showVersion = flags.Bool("version", false, "print the version and exit")
		configPath  = flags.String("config", "", "path to the client configuration file (default client.toml)")
		checkConfig = flags.Bool("check", false, "validate the configuration and exit")
		strictKeys  = flags.Bool("reject-unknown-keys", false, "fail instead of warning when the configuration holds a key this version does not understand")
		showID      = flags.Bool("identity", false, "print this client's public identity key and exit")
		discover    = flags.String("discover", "", "resolve a proxy name through the [dht] network and exit")
	)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: %s [flags] [config-file]\n\n", program)
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		// Asking for the usage is not a mistake: the flag set has written it
		// already, and the command reports success the way the standard flag
		// handling does. A command line that does not parse is a usage error
		// with its own status, so a script can tell the two apart.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Every query flag is an action of its own. Without this, a command line
	// naming two of them runs the first in the source and silently drops the
	// rest, so a script gets one action's output and a success status.
	actions := 0
	for _, wanted := range []bool{*checkConfig, *showID, *discover != "", *showVersion} {
		if wanted {
			actions++
		}
	}
	if actions > 1 {
		// -version is one of them: it prints and exits before every other action.
		fmt.Fprintf(stderr, "%s: name one action at a time: -check, -identity, -discover and -version are mutually exclusive\n", program)
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "aethertunnel-client %s (protocol %d, built %s, commit %s)\n",
			clientlib.Version, protocol.ProtocolVersion, clientlib.BuildTime, clientlib.GitCommit)
		return 0
	}

	path := *configPath
	if path == "" {
		if flags.NArg() > 0 {
			path = flags.Arg(0)
		} else {
			path = "client.toml"
		}
	}

	// Everything up to the run reports through standard error: --check,
	// --identity and --discover answer to the terminal that ran them, and a
	// check run must not create or append to the log file it is checking. The
	// run itself writes where [log] sends it.
	bootLogger := log.New(stderr, "", log.LstdFlags)

	cfg, err := config.Load(path, config.ValidateOptions{Role: config.RoleClient, RejectUnknownKeys: *strictKeys})
	// Reported even when the configuration is rejected, for the same reason the server
	// does it: the warnings and the validation failures are usually one mistake, and
	// seeing them together saves a second round trip.
	if cfg != nil {
		for _, warning := range cfg.Warnings {
			bootLogger.Printf("warning: %s", warning)
		}
	}
	if err != nil {
		bootLogger.Printf("%v", err)
		return 1
	}

	if *checkConfig {
		fmt.Fprintf(stdout, "%s is valid\n", path)
		return 0
	}

	if *showID {
		identity, err := crypto.LoadIdentity(cfg.Identity.KeyFile)
		if err != nil {
			bootLogger.Printf("identity: %v", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n", identity.PublicKeyHex())
		return 0
	}

	if *discover != "" {
		if !cfg.DHT.Enabled {
			bootLogger.Printf("dht: -discover needs a [dht] section with enabled = true")
			return 1
		}
		resolver, err := discovery.Start(cfg.DHTSettings(bootLogger))
		if err != nil {
			bootLogger.Printf("dht: %v", err)
			return 1
		}
		defer func() { _ = resolver.Close() }()

		for _, failure := range resolver.Bootstrap(resolver.Context()) {
			bootLogger.Printf("dht: bootstrap %v", failure)
		}
		record, err := resolver.Resolve(*discover)
		if err != nil {
			logging.Errorf(bootLogger, "dht lookup of %q failed: %v", *discover, err)
			return 1
		}
		if record.Verified {
			fmt.Fprintf(stdout, "%s -> %s (type %s, announced %s, signed by %s)\n",
				record.Name, record.Server, record.Type,
				record.Updated.UTC().Format(time.RFC3339), record.PublicKey)
			return 0
		}
		fmt.Fprintf(stdout, "%s -> %s (type %s, announced %s, unsigned)\n",
			record.Name, record.Server, record.Type, record.Updated.UTC().Format(time.RFC3339))
		return 0
	}

	logger, closeLog, err := logging.Open(cfg.Log.Options(), stderr)
	if err != nil {
		bootLogger.Printf("%v", err)
		return 1
	}
	defer func() { _ = closeLog() }()

	ctx, stop := notifyShutdown()
	defer stop()

	if err := clientlib.Run(ctx, cfg, logger); err != nil {
		logger.Printf("%v", err)
		return 1
	}
	return 0
}
