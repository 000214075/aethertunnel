package server

import (
	"context"
	"io"
	"log"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
)

// lockedBuffer is an io.Writer the test can read while the server writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A client that has no tunnel address can still emit tunnel packets at line
// rate, and the old code wrote one log line per packet. The lines have to be
// rate-limited: five packets in one second are one line naming all five, not
// five lines.
func TestTunnelPacketsFromASessionWithoutAnAddressAreLoggedOncePerInterval(t *testing.T) {
	var logs lockedBuffer
	cfg := testConfig(t, false)
	rs := startServerLoggingTo(t, cfg, &logs)

	client := dialVPNClient(t, rs, false, false)
	for i := 0; i < 5; i++ {
		if err := client.sendPacket([]byte{0x45, 0x00, 0x00, 0x14}); err != nil {
			t.Fatalf("send packet %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := strings.Count(logs.String(), "holds no tunnel address"); got >= 1 {
			if got != 1 {
				t.Fatalf("the log holds %d per-packet lines, want 1", got)
			}
			// The line names however many packets had arrived when it was
			// written; the rest of the five are counted silently.
			carries := false
			for n := 1; n <= 5; n++ {
				if strings.Contains(logs.String(), "sent "+strconv.Itoa(n)+" tunnel packet(s)") {
					carries = true
				}
			}
			if !carries {
				t.Fatalf("the line does not carry a dropped total; the log reads:\n%s", logs.String())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rate-limited line appeared; the log reads:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Run promises that a failed startup leaves nothing behind, but the ban
// collector used to be launched before p2p and the SSH gateway could fail the
// run: with bans enabled and a p2p port that cannot be bound, the collector
// goroutine outlived Run for as long as the caller's context lasted.
func TestAFailedStartupDoesNotLeaveTheBanCollectorBehind(t *testing.T) {
	occupied, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a p2p port: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	port := occupied.LocalAddr().(*net.UDPAddr).Port

	cfg := testConfig(t, false)
	cfg.Server.BanAfterFailures = 3
	cfg.Server.BanSeconds = 60
	cfg.Server.P2PPort = port
	if err := cfg.Validate(config.RoleServer); err != nil {
		t.Fatalf("config: %v", err)
	}

	srv, err := New(cfg, Options{Version: "test-version", BuildTime: "now", GitCommit: "test", Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(context.Background()) }()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("Run succeeded although the p2p port cannot be bound")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not fail within 10s")
	}

	// The collector ticks at least a minute apart, so a goroutine that is
	// still there shows up in the stack dump for the whole window. The
	// context stays alive on purpose: cancelling it would stop the collector
	// itself and hide the gap.
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		if !strings.Contains(string(buf[:n]), "collectBans") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the ban collector goroutine is still running after the failed Run")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
