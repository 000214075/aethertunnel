// Command aethertunnel-server runs the AetherTunnel server: it accepts client
// control sessions, publishes the tunnels they register and serves the dashboard.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/ledger"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
	"github.com/aethertunnel/aethertunnel/pkg/server"
)

// These are stamped at build time with
//
//	-ldflags "-X main.version=v1.0.0 -X main.buildTime=... -X main.gitCommit=..."
//
// and fall back to the values below when the binary is built with plain
// "go build".
var (
	version   = "dev"
	buildTime = "unknown"
	gitCommit = "unknown"
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
// thing anyone meets, and the whole of what a script can rely on — can be
// exercised without starting a process. The last branch is the server itself;
// everything before it answers one question and exits. It returns the exit
// status: 0 for what succeeded, 1 for what was refused (the reason goes to
// stderr first), 2 for a command line that does not parse.
func run(program string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(program, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		showVersion = flags.Bool("version", false, "print the version and exit")
		configPath  = flags.String("config", "", "path to the server configuration file (default server.toml)")
		checkConfig = flags.Bool("check", false, "validate the configuration and exit")
		strictKeys  = flags.Bool("reject-unknown-keys", false, "fail instead of warning when the configuration holds a key this version does not understand")
		verifyPath  = flags.String("verify-ledger", "", "verify a bandwidth ledger file against a public key and exit")
		verifyKey   = flags.String("ledger-key", "", "verification key for -verify-ledger: a hex Ed25519 public key or a signing key file")
		proofPath   = flags.String("ledger-proof", "", "write the ledger entries up to -proof-index to stdout and exit")
		proofIndex  = flags.Int("proof-index", -1, "entry index for -ledger-proof: the proof covers entries 0 through this index")
		dhtLookup   = flags.String("dht-lookup", "", "resolve a proxy name through the [dht] network and exit")
		dhtKey      = flags.Bool("dht-key", false, "print the [dht] announcement signing key and exit")
	)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: %s [flags] [config-file]\n\n", program)
		fmt.Fprintf(flags.Output(), "Flags:\n")
		flags.PrintDefaults()
		fmt.Fprintf(flags.Output(), "\nThe positional config-file form is kept for compatibility with older releases.\n")
		fmt.Fprintf(flags.Output(), "\nExamples:\n")
		fmt.Fprintf(flags.Output(), "  %s -config server.toml\n", program)
		fmt.Fprintf(flags.Output(), "  %s -verify-ledger aethertunnel-ledger.jsonl -ledger-key 3b1f...\n", program)
		fmt.Fprintf(flags.Output(), "  %s -ledger-proof aethertunnel-ledger.jsonl -proof-index 41 > proof.jsonl\n", program)
		fmt.Fprintf(flags.Output(), "  %s -dht-lookup ssh -config server.toml\n", program)
		fmt.Fprintf(flags.Output(), "  %s -dht-key -config server.toml\n", program)
	}

	if err := flags.Parse(args); err != nil {
		// Asking for the usage is not a mistake (the flag set has written it
		// already); a command line that does not parse is a usage error with its
		// own status, so a script can tell the two apart.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Every query flag is an action of its own. Without this, a command line
	// naming two of them runs the first in the source and silently drops the
	// rest, so a script gets one action's output and a success status.
	actions := 0
	for _, wanted := range []bool{*verifyPath != "", *proofPath != "", *checkConfig, *dhtKey, *dhtLookup != "", *showVersion} {
		if wanted {
			actions++
		}
	}
	if actions > 1 {
		// -version is one of them: it prints and exits before every other action,
		// so a command line that named it and something else ran the version and
		// silently dropped the action.
		fmt.Fprintf(stderr, "%s: name one action at a time: -verify-ledger, -ledger-proof, -check, -dht-key, -dht-lookup and -version are mutually exclusive\n", program)
		return 2
	}
	// Two flags only modify an action. Naming one without its action used to be
	// dropped in silence while the server started or another action ran, so a
	// script got a success status for a request nothing honoured.
	if *verifyKey != "" && *verifyPath == "" {
		fmt.Fprintf(stderr, "%s: -ledger-key sets the verification key for -verify-ledger, which was not named\n", program)
		return 2
	}
	if *proofIndex != -1 && *proofPath == "" {
		fmt.Fprintf(stderr, "%s: -proof-index selects how much of the chain -ledger-proof covers, which was not named\n", program)
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "aethertunnel-server %s (protocol %d, built %s, commit %s)\n",
			version, protocol.ProtocolVersion, buildTime, gitCommit)
		return 0
	}

	if *verifyPath != "" {
		if err := verifyLedger(stdout, *verifyPath, *verifyKey); err != nil {
			fmt.Fprintf(stderr, "ledger verification failed: %v\n", err)
			return 1
		}
		return 0
	}

	if *proofPath != "" {
		if err := writeLedgerProof(stdout, stderr, *proofPath, *proofIndex); err != nil {
			fmt.Fprintf(stderr, "ledger proof failed: %v\n", err)
			return 1
		}
		return 0
	}

	path := *configPath
	if path == "" {
		if flags.NArg() > 0 {
			path = flags.Arg(0)
		} else {
			path = "server.toml"
		}
	}

	// The configuration is read before the logger can be built from its [log]
	// section, so the lines that report a broken configuration go to standard
	// error, and everything after this point goes where [log] sends it. That
	// order is deliberate: a configuration that cannot be parsed is exactly the
	// case where the path in it cannot be trusted.
	bootLogger := log.New(stderr, "", log.LstdFlags)

	// A DHT lookup only needs the [dht] section, so it does not require the rest of
	// a server configuration to be complete.
	role := config.RoleServer
	if *dhtLookup != "" {
		role = ""
	}
	cfg, err := config.Load(path, config.ValidateOptions{Role: role, RejectUnknownKeys: *strictKeys})
	// Reported before the error is acted on, and even when there is one: the warnings
	// are what Load could not put in the error, and a run of --check that names the
	// unknown keys and the validation failures together saves a second round trip.
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

	if *dhtKey {
		key, err := server.AnnouncementKey(cfg)
		if err != nil {
			bootLogger.Printf("dht announcement key: %v", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n", key)
		return 0
	}

	if *dhtLookup != "" {
		record, err := server.LookupProxy(cfg, bootLogger, *dhtLookup)
		if err != nil {
			fmt.Fprintf(stderr, "dht lookup of %q failed: %v\n", *dhtLookup, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s -> %s (type %s", record.Name, record.Server, record.Type)
		if len(record.Domains) > 0 {
			fmt.Fprintf(stdout, ", domains %s", strings.Join(record.Domains, ","))
		}
		fmt.Fprintf(stdout, ", announced %s", record.Updated.UTC().Format(time.RFC3339))
		if record.Verified {
			fmt.Fprintf(stdout, ", signed by %s)\n", record.PublicKey)
		} else {
			fmt.Fprintf(stdout, ", unsigned)\n")
		}
		return 0
	}

	// Everything above reported through standard error on purpose: --check and
	// the two DHT helpers answer to the terminal that ran them, and a check run
	// must not create or append to the log file it is checking. From here the
	// server runs, so [log] takes over.
	logger, closeLog, err := logging.Open(cfg.Log.Options(), stderr)
	if err != nil {
		bootLogger.Printf("%v", err)
		return 1
	}
	defer func() { _ = closeLog() }()

	srv, err := server.New(cfg, server.Options{
		Version:   version,
		BuildTime: buildTime,
		GitCommit: gitCommit,
		Logger:    logger,
	})
	if err != nil {
		logging.Errorf(logger, "cannot start server: %v", err)
		return 1
	}

	ctx, stop := notifyShutdown()
	defer stop()

	// [metrics].port puts the Prometheus endpoint on its own address, so a scrape
	// does not have to reach the dashboard. A port that cannot be bound is reported
	// and the tunnels keep serving, the same treatment a busy dashboard port gets.
	var metricsListener *server.MetricsListener
	if cfg.Metrics.Enabled && cfg.Metrics.Port > 0 {
		metricsListener = server.NewMetricsListener(srv, logger)
		if err := metricsListener.Start(); err != nil {
			logger.Printf("metrics listener disabled: %v", err)
			metricsListener = nil
		} else {
			// Stopped by defer rather than after Run below: a Run that cannot
			// start returns there, and the listener would keep answering on a
			// server whose stores Run has already closed. run() is called
			// in-process by main_test.go, where that leak is real.
			defer metricsListener.Stop()
		}
	}

	var dashboard *server.Dashboard
	if cfg.Dashboard.Enabled {
		dashboard, err = server.NewDashboard(srv, logger)
		if err != nil {
			logging.Errorf(logger, "cannot prepare dashboard: %v", err)
			return 1
		}
		if err := dashboard.Start(); err != nil {
			// A busy dashboard port should not stop the tunnel service.
			logger.Printf("dashboard disabled: %v", err)
			dashboard = nil
		} else {
			// Registered here for the same reason as the metrics listener: the
			// failed-Run return below used to skip this and leave the panel
			// serving.
			defer dashboard.Stop()
		}
	}

	if err := srv.Run(ctx); err != nil {
		logger.Printf("server stopped: %v", err)
		return 1
	}
	logger.Printf("AetherTunnel server %s stopped", version)
	return 0
}

// writeLedgerProof writes the ledger prefix that proves one entry's inclusion.
//
// The whole file has to be read to reach an entry, but what comes out is only entries
// 0 through index. A holder of the public key can verify that prefix on its own, so an
// operator can show one period's usage, or prove that a published head belongs to a
// chain, without handing over the rest of the ledger.
func writeLedgerProof(stdout, stderr io.Writer, path string, index int) error {
	if index < 0 {
		return errors.New("-ledger-proof needs -proof-index (0 is the first entry)")
	}
	chain, err := ledger.ReadFile(path)
	if err != nil {
		return err
	}
	if len(chain) == 0 {
		return fmt.Errorf("%s has no entries", path)
	}
	if index >= len(chain) {
		return fmt.Errorf("%s has %d entries, so index %d does not exist", path, len(chain), index)
	}
	// The proof is written one JSON object per line, exactly as the ledger file is, so
	// the result verifies with -verify-ledger as it stands.
	var out bytes.Buffer
	for _, entry := range chain[:index+1] {
		line, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode entry %d: %w", entry.Index, err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	if _, err := stdout.Write(out.Bytes()); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "proof of %d entries, head %s\n", index+1, chain[index].Hash)
	return nil
}

// verifyLedger checks a ledger file end to end and prints the per-client totals.
//
// It needs only the public key, so it can be run by anyone the operator gives the
// public key and the file to, on a machine that has neither the signing key nor a
// running server.
func verifyLedger(stdout io.Writer, path, keyText string) error {
	entries, totals, head, err := server.VerifyLedgerFile(path, keyText)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s: %d entries verified\n", path, entries)
	if head == "" {
		fmt.Fprintf(stdout, "chain head: (empty)\n")
	} else {
		fmt.Fprintf(stdout, "chain head: %s\n", head)
	}

	if len(totals) == 0 {
		fmt.Fprintf(stdout, "no usage recorded\n")
		return nil
	}

	ids := make([]string, 0, len(totals))
	for id := range totals {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	fmt.Fprintf(stdout, "%-18s %14s %14s %8s\n", "CLIENT", "BYTES_IN", "BYTES_OUT", "ENTRIES")
	for _, id := range ids {
		sum := totals[id]
		fmt.Fprintf(stdout, "%-18s %14d %14d %8d\n", id, sum.BytesIn, sum.BytesOut, sum.Entries)
	}
	return nil
}
