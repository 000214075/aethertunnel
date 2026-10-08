package discovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Rewriting one announcement after another makes a round take one lookup
// timeout per name, so a server with many names pushed the later names past
// their expiry and off the directory whenever the DHT slowed down. A round now
// runs the refreshes side by side: the records are independent and refresh
// already arbitrates against a concurrent Publish.
func TestARepublishRoundRewritesNamesSideBySide(t *testing.T) {
	live := []liveAnnouncement{
		{rec: Record{Name: "a"}},
		{rec: Record{Name: "b"}},
	}

	bothIn := make(chan struct{})
	var arrived atomic.Int32
	problems := make(chan string, 2)
	refresh := func(ctx context.Context, rec Record, epoch uint64) error {
		if arrived.Add(1) == 2 {
			close(bothIn)
		}
		select {
		case <-bothIn:
			return nil
		case <-time.After(2 * time.Second):
			problems <- fmt.Sprintf("refresh of %q ran alone", rec.Name)
			return errors.New("the other refresh never started")
		}
	}

	republishEntries(context.Background(), live, 5*time.Second, refresh, func(format string, args ...any) {
		problems <- fmt.Sprintf(format, args...)
	})
	close(problems)
	for problem := range problems {
		t.Fatalf("the round did not run the two refreshes side by side: %s", problem)
	}
}

// lockedLogs is a log sink the workers can write to together.
type lockedLogs struct {
	mu  sync.Mutex
	buf []string
}

func (l *lockedLogs) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, s)
}

func (l *lockedLogs) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.buf...)
}

// A shutdown during a round is not a refresh failure: the node's context
// cancels the in-flight refresh, and the old loop used to treat that as an
// error line and stop the round. The workers skip the noise instead.
func TestACancelledNodeDoesNotLogItsRepublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var logs lockedLogs
	republishEntries(ctx, []liveAnnouncement{{rec: Record{Name: "a"}}}, 5*time.Second,
		func(ctx context.Context, rec Record, epoch uint64) error {
			return ctx.Err()
		},
		func(format string, args ...any) {
			logs.add(fmt.Sprintf(format, args...))
		})

	for _, line := range logs.all() {
		t.Fatalf("a shutdown produced a refresh-failure log line: %s", line)
	}
}
