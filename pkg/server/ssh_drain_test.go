package server

import (
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// The ssh tunnel gateway used to be stopped in one step: Shutdown closed its
// listener and every established SSH connection and waited for the handlers. The
// connections' teardown removes the sessions they own from the SessionManager, so
// by the time Shutdown reached the graceful drain there was nothing left to wait
// for and an ssh tunnel got none of the [server] graceful_shutdown_seconds window
// every other tunnel gets. The listener still has to close before the drain — a
// connection admitted during the window would build a session that is not
// draining — so the gateway now stops accepting first and is disconnected after
// the drain, inside Shutdown and before Run closes the stores.
func TestAnSSHTunnelIsDrainedBeforeItsConnectionIsClosed(t *testing.T) {
	echoAddr := startEcho(t)
	echoPort := portOf(t, echoAddr)
	keyPath := filepath.Join(t.TempDir(), "gateway_host_key")

	cfg, gwPort := sshGatewayConfig(t, keyPath)
	cfg.Server.GracefulShutdownSecs = 3
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}
	rs := startServer(t, cfg)

	client := dialSSHGateway(t, gwPort)
	defer client.Close()

	forwarded, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("request a remote forwarding: %v", err)
	}
	defer forwarded.Close()
	go serveForwardedToEcho(forwarded, echoAddr)

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	publicPort := freePort(t)
	command := fmt.Sprintf("tcp --proxy_name web --remote_port %d --local_ip 127.0.0.1 --local_port %d --token %s",
		publicPort, echoPort, testToken)
	if err := session.Start(command); err != nil {
		t.Fatalf("exec %q: %v", command, err)
	}
	readSSHUntil(t, stdout, "RemoteAddress:", 5*time.Second)

	visitor := holdStream(t, publicPort, "")
	defer visitor.Close()

	done := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		rs.server.Shutdown("ssh gateway drain test")
		done <- time.Since(started)
	}()

	// Half a second into a three-second window the ssh tunnel has to be as open
	// as a client's: the gateway may refuse new connections, but the stream the
	// drain is waiting for is the visitor's.
	time.Sleep(500 * time.Millisecond)
	_ = visitor.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := visitor.Write([]byte("still-open")); err != nil {
		t.Fatalf("the visitor could not write during the grace period: %v", err)
	}
	answer := make([]byte, len("still-open"))
	if _, err := io.ReadFull(visitor, answer); err != nil {
		t.Fatalf("the ssh tunnel stopped carrying the stream during the grace period: %v", err)
	}
	if string(answer) != "still-open" {
		t.Fatalf("the stream answered %q during the grace period", answer)
	}

	// Ending the stream releases the drain, so the shutdown does not sit out the
	// rest of the grace period and the gateway's own wait returns with it.
	_ = visitor.Close()
	select {
	case took := <-done:
		if took > 2*time.Second {
			t.Errorf("the shutdown took %s after the ssh tunnel's stream ended, want it to stop waiting", took)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the shutdown did not return after the ssh tunnel's stream ended")
	}
}

// sshLedgerConfig is a validated configuration with the SSH tunnel gateway and
// the bandwidth ledger both on, plus the audit log: the shape of a deployment
// that publishes SSH tunnels, bills them for the bytes they carry and records who
// published what.
func sshLedgerConfig(t *testing.T, keyPath string) (*config.Config, int) {
	t.Helper()
	cfg := ledgerConfig(t, false)
	cfg.Server.GracefulShutdownSecs = 1
	port := freePort(t)
	cfg.Server.SSHTunnelGateway = &config.SSHTunnelGatewayConfig{
		BindAddr: "127.0.0.1",
		BindPort: port,
		KeyFile:  keyPath,
	}
	cfg.Audit.Enabled = true
	cfg.Audit.Path = filepath.Join(t.TempDir(), "audit.jsonl")
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("ssh gateway + ledger test config invalid: %v", err)
	}
	return cfg, port
}

// The SSH gateway's handlers are not in the WaitGroup Run's accept loop waits
// on: Shutdown waits for them itself, as its last step. Closing the stores was
// therefore free to happen first, and what it takes with it is the gateway
// session's last ledger entry — the billing record for the bytes the SSH tunnel
// carried — and its audit records; a real deployment exits the process on that
// same ordering. The window is a few instructions wide, so the whole deployment
// runs several rounds and every round has to end with the entry on disk.
func TestAnSSHTunnelSessionsLedgerEntryIsWrittenBeforeTheStoresClose(t *testing.T) {
	const rounds = 5
	for round := 0; round < rounds; round++ {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			echoAddr := startEcho(t)
			echoPort := portOf(t, echoAddr)
			keyPath := filepath.Join(t.TempDir(), "gateway_host_key")

			cfg, gwPort := sshLedgerConfig(t, keyPath)
			rs := startServer(t, cfg)

			// The ordinary authenticated control connection a real deployment
			// has, which is also the handler the accept loop's WaitGroup does
			// wait for.
			control, err := newTestClient(t, rs.addr, false)
			if err != nil {
				t.Fatalf("control connect: %v", err)
			}
			defer control.close()
			if _, err := control.authenticate("plain", testToken); err != nil {
				t.Fatalf("control authenticate: %v", err)
			}

			sshClient := dialSSHGateway(t, gwPort)
			defer sshClient.Close()
			forwarded, err := sshClient.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("request a remote forwarding: %v", err)
			}
			defer forwarded.Close()
			go serveForwardedToEcho(forwarded, echoAddr)

			session, err := sshClient.NewSession()
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			stdout, err := session.StdoutPipe()
			if err != nil {
				t.Fatalf("stdout pipe: %v", err)
			}
			publicPort := freePort(t)
			command := fmt.Sprintf("tcp --proxy_name web --remote_port %d --local_ip 127.0.0.1 --local_port %d --token %s",
				publicPort, echoPort, testToken)
			if err := session.Start(command); err != nil {
				t.Fatalf("exec %q: %v", command, err)
			}
			readSSHUntil(t, stdout, "RemoteAddress:", 5*time.Second)

			// Bytes both ways, so the session's entry has something to bill.
			visitor := holdStream(t, publicPort, "")
			_ = visitor.Close()

			rs.server.Shutdown("ssh gateway ledger ordering test")

			// The entry is written by the gateway handler on its way out. The
			// poll allows for the file write; it does not hide a missing entry,
			// because a lost one is never written at all.
			entries := waitForLedgerEntries(t, cfg.Ledger.Path, 1)
			found := false
			for _, entry := range entries {
				if entry.Proxy == "web" {
					found = true
				}
			}
			if !found {
				t.Fatalf("the ledger holds %d entries and none of them is the ssh tunnel's %q: %+v", len(entries), "web", entries)
			}

			// The same ordering decides whether the gateway session's audit
			// records reach the file or are counted as lost against a log that
			// was closed first.
			deadline := time.Now().Add(5 * time.Second)
			removals := 0
			for {
				removals = countAuditEvents(t, cfg.Audit.Path)[EventProxyRemoved]
				if removals > 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if removals == 0 {
				t.Fatalf("the audit log has no %s record for the ssh tunnel's proxy; it holds %v",
					EventProxyRemoved, countAuditEvents(t, cfg.Audit.Path))
			}
		})
	}
}
